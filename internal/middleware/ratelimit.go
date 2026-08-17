// ratelimit.go — Redis-backed fixed-window rate limiter.
//
// WHY REDIS AND NOT AN IN-PROCESS MAP:
// An in-memory counter is per-process. As soon as you run two API
// instances behind a load balancer, each has its own view of "how many
// requests has this client sent" and an attacker sending N/instance
// gets 2N through. Redis is the shared source of truth every instance
// consults, so the limit holds across horizontal scale-out without
// coordination between the API instances.
//
// WHY FIXED-WINDOW INCR+EXPIRE AND NOT TOKEN BUCKET:
//   - INCR is one atomic Redis op. EXPIRE-if-first (via EXPIRE NX) adds
//     one more. That's it — 2 commands, no Lua, no state on our side.
//   - Token bucket needs refill math (lastRefill, currentTokens,
//     capacity, refillRate), which is either a Lua script for atomicity
//     or several roundtrips with CAS. More code, more Redis calls, more
//     ways to be subtly wrong.
//   - The classic fixed-window edge case is burst-across-boundary: a
//     client can send N at second 59 and N more at second 61 for an
//     effective 2N in ~2s. Real cost here: at limit=60/min for
//     subscriptions, worst case burst is 120 requests in 2s — still
//     ~one order of magnitude below anything that hurts our Postgres.
//     For /login we're limiting IPs to 10/min, so worst case is 20 in
//     ~2s, which is trivial. When we start caring about smoother
//     shaping (e.g. protecting a paid downstream API from bursts) we
//     upgrade to a token bucket or a sliding-window log. Not now.
//
// FAIL OPEN, NOT CLOSED:
// If Redis is unreachable, we let the request through and log. Rate
// limiting protects against abuse; blocking legitimate traffic because
// our cache is down would be a worse outcome than briefly missing the
// abuse guard. Same "Redis is optional degradation" philosophy as
// Phase 2's cache-aside layer.
package middleware

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/metrics"
)

// Limiter enforces `limit` requests per `window` per client, where
// "client" is defined by the keyer function (IP, user_id, etc).
type Limiter struct {
	rdb    *redis.Client
	limit  int64
	window time.Duration
	// keyer returns the per-client identifier to bucket by, plus an
	// "apply the limit at all?" flag. Returning ok=false makes this a
	// no-op for that request — see ByAuthUser for why that's the right
	// fail-open behavior.
	keyer func(*gin.Context) (string, bool)
	// name is a namespace prefix in Redis so two Limiters (e.g. "login"
	// vs "sub") can't share counters even if they happen to have the
	// same window.
	name string
}

// NewLimiter constructs a Limiter. Usually consumed via one of the
// helpers below (LoginByIP, SubscriptionsByUser) rather than raw.
func NewLimiter(rdb *redis.Client, name string, limit int64, window time.Duration, keyer func(*gin.Context) (string, bool)) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window, keyer: keyer, name: name}
}

// Handler returns the Gin middleware. Attach with r.Use(l.Handler())
// or on a specific route: r.POST("/login", l.Handler(), authH.login).
func (l *Limiter) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		clientKey, ok := l.keyer(c)
		if !ok {
			// No identifier available — skip. This covers the
			// "middleware misconfigured" case (e.g. ByAuthUser used
			// without RequireAuth in front of it). Fail open.
			c.Next()
			return
		}
		redisKey := fmt.Sprintf("ratelimit:%s:%s", l.name, clientKey)
		ctx := c.Request.Context()

		// INCR is atomic. On first hit within the window Redis creates
		// the key at value 1; subsequent hits increment. No CAS needed.
		n, err := l.rdb.Incr(ctx, redisKey).Result()
		if err != nil {
			log.Printf("ratelimit: INCR %s failed: %v (allowing request)", redisKey, err)
			c.Next()
			return
		}

		// ExpireNX sets the TTL only if the key doesn't already have
		// one. Safe to call every request — after the first-in-window
		// call it's a no-op. This is more robust than "only call
		// EXPIRE when INCR returns 1": if a crash landed between INCR
		// and EXPIRE last window, the key could linger without a TTL
		// (Redis default) and the counter would live forever. ExpireNX
		// on every request self-heals that case on the next request.
		l.rdb.ExpireNX(ctx, redisKey, l.window)

		if n > l.limit {
			// Retry-After MUST reflect the actual remaining seconds
			// until reset — hardcoding the window length would tell a
			// well-behaved client to back off longer than necessary.
			ttl, err := l.rdb.TTL(ctx, redisKey).Result()
			retryAfterSec := int64(l.window.Seconds())
			if err == nil && ttl > 0 {
				retryAfterSec = int64(ttl.Seconds())
				// TTL of 0.4s would floor to 0 which is meaningless as
				// a Retry-After. Bump to 1s so the client backs off at
				// least a full second.
				if retryAfterSec < 1 {
					retryAfterSec = 1
				}
			}
			// Metrics: record the rejection labeled by which limiter
			// fired ("login" or "sub"). Alerting on
			// rate(rate_limit_rejections_total{limiter="login"}[5m])
			// spiking is a good "someone's credential-stuffing us" signal.
			metrics.RateLimitRejectionsTotal.WithLabelValues(l.name).Inc()
			c.Header("Retry-After", strconv.FormatInt(retryAfterSec, 10))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": fmt.Sprintf("rate limit exceeded (%d per %s)", l.limit, l.window),
			})
			return
		}
		c.Next()
	}
}

// -----------------------------------------------------------------------------
// Common keyers + constructors
// -----------------------------------------------------------------------------

// ByIP buckets by client IP address. Uses gin.Context.ClientIP which
// respects X-Forwarded-For / X-Real-IP if trusted-proxy config allows.
// In dev/no-proxy setups this returns the raw remote address.
func ByIP(c *gin.Context) (string, bool) {
	ip := c.ClientIP()
	if ip == "" {
		return "", false
	}
	return "ip:" + ip, true
}

// ByAuthUser buckets by the authenticated user_id stashed on context
// by RequireAuth. If no user_id is present the limiter is a no-op —
// the request will fall through to whatever comes next in the chain.
// This is the fail-open case: on protected routes RequireAuth would
// have already rejected the request with 401, so we'd never reach
// here without a user_id. If we ever do reach here without one it
// means the middleware chain is misconfigured, and blocking anonymous
// traffic at the rate limiter would be an obscure way to surface that
// bug — better to just let it flow to RequireAuth which will 401 loudly.
func ByAuthUser(c *gin.Context) (string, bool) {
	uid, ok := UserIDFrom(c.Request.Context())
	if !ok {
		return "", false
	}
	return fmt.Sprintf("u:%d", uid), true
}

// LoginByIP is the recommended limiter for POST /login: 10/min per IP.
//
// WHY 10/min DESPITE /login HAVING NO PASSWORD CHECK YET (Phase 5 gap):
//   - /login still mints signed JWTs, which is a CPU-nontrivial HMAC op
//     an attacker could spam to burn CPU or fill logs.
//   - user_ids are sequential integers, so an unrate-limited /login is
//     a free enumeration oracle — no auth needed to probe which ids
//     are minted.
//   - Once password checks land, this same limit becomes the
//     brute-force ceiling: 10 guesses/min = 600/hr per IP, well below
//     what any real user needs (typically 1–2 attempts) and slow
//     enough to make credential stuffing impractical without a
//     botnet-scale IP pool.
//   - Adding the limit now means we don't have to re-plumb it (or
//     forget to plumb it) when passwords arrive.
func LoginByIP(rdb *redis.Client) *Limiter {
	return NewLimiter(rdb, "login", 10, time.Minute, ByIP)
}

// SubscriptionsByUser is the recommended limiter for the protected
// subscription routes: 60/min per authenticated user.
//
// WHY 60/min PER USER:
//   - A legitimate interactive session (viewing a list, refreshing,
//     subscribing/cancelling a few times) rarely exceeds 5–10/min.
//     60 leaves ~10x headroom for chatty UIs (auto-refresh polling
//     every second is 60/min exactly — allowed but tight).
//   - Protects against a compromised token being used to spam
//     Subscribe or hammer Renew.
//   - Per-user bucketing means one user can't affect another — total
//     capacity scales linearly with user count, so we're not creating
//     a shared choke point.
//   - Distinct from LoginByIP so an attacker that got a token still
//     has to spread across users to exceed a per-user limit — an IP
//     limit on protected routes would penalize NAT'd corporate
//     networks where many legitimate users share one egress IP.
func SubscriptionsByUser(rdb *redis.Client) *Limiter {
	return NewLimiter(rdb, "sub", 60, time.Minute, ByAuthUser)
}
