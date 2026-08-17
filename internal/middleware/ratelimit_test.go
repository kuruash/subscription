// Unit tests for the Redis-backed rate limiter.
//
// Same testing conventions as Phase 6 services tests:
//   - External test package (middleware_test) so we can only touch
//     exported API.
//   - miniredis stands in for Redis so tests are fast and hermetic.
//   - No real network, no real time.Sleep — miniredis.FastForward moves
//     the fake clock, which is how the window-reset test proves the
//     TTL-based reset behavior without a real 60s wait.
//
// Middleware is normally in the "don't test glue" bucket per CLAUDE.md,
// but this middleware carries real logic (counter math, Retry-After
// computation, per-window reset), which is exactly the "if any of them
// start carrying real logic, add tests then" case.
package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/middleware"
)

func init() {
	// Silence gin's request logging in tests.
	gin.SetMode(gin.TestMode)
}

// setup returns a fresh miniredis + go-redis client pair per test.
// miniredis.RunT auto-closes when the test ends.
//
// Short DialTimeout/ReadTimeout + MaxRetries=-1 (disabled) keep the
// fail-open test snappy — with go-redis defaults it takes ~9s to
// exhaust retries against a closed miniredis. Doesn't affect the
// happy-path tests since miniredis is in-memory.
func setup(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	return mr, rdb
}

// newRouter builds a minimal gin router with the given middlewares
// and a trivial handler that returns 200. Extracted so each test
// stays focused on the behavior it's asserting.
func newRouter(mws ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	handlers := append([]gin.HandlerFunc{}, mws...)
	handlers = append(handlers, func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/x", handlers...)
	return r
}

// call fires a single request with the given IP and returns the
// recorded status + Retry-After header. Kept tiny so tests read like
// a script of "call, assert, call, assert".
func call(r *gin.Engine, ip string) (int, string) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = ip + ":1234"
	r.ServeHTTP(w, req)
	return w.Code, w.Header().Get("Retry-After")
}

// -----------------------------------------------------------------------------
// Under the limit: every request passes through.
// -----------------------------------------------------------------------------

func TestLimiter_UnderLimitPassesThrough(t *testing.T) {
	_, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 3, time.Minute, middleware.ByIP)
	r := newRouter(l.Handler())

	for i := 1; i <= 3; i++ {
		code, ra := call(r, "1.2.3.4")
		if code != http.StatusOK {
			t.Fatalf("call %d: want 200, got %d", i, code)
		}
		if ra != "" {
			t.Errorf("call %d: unexpected Retry-After=%q on allowed request", i, ra)
		}
	}
}

// -----------------------------------------------------------------------------
// Over the limit: 429 + Retry-After reflects real remaining TTL.
// -----------------------------------------------------------------------------

func TestLimiter_OverLimitReturns429WithRetryAfter(t *testing.T) {
	_, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 3, time.Minute, middleware.ByIP)
	r := newRouter(l.Handler())

	// Exhaust the limit.
	for i := 1; i <= 3; i++ {
		if code, _ := call(r, "9.9.9.9"); code != http.StatusOK {
			t.Fatalf("setup call %d: want 200, got %d", i, code)
		}
	}

	// Fourth request must be rejected.
	code, ra := call(r, "9.9.9.9")
	if code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", code)
	}
	if ra == "" {
		t.Fatalf("Retry-After header missing on 429 response")
	}
	sec, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After not an integer: %q (%v)", ra, err)
	}
	// Window is 60s and we just tripped it, so remaining TTL should be
	// within a hair of 60s. Give a small tolerance for test execution
	// time and the ExpireNX being called on a fresh key.
	if sec < 55 || sec > 60 {
		t.Errorf("Retry-After = %d, want ~55-60 for a fresh 1-min window", sec)
	}
}

// -----------------------------------------------------------------------------
// The window resets: after the TTL expires, the client is allowed again.
// Uses miniredis.FastForward to advance the fake clock instead of
// actually sleeping for a minute.
// -----------------------------------------------------------------------------

func TestLimiter_ResetsAfterWindow(t *testing.T) {
	mr, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 2, time.Minute, middleware.ByIP)
	r := newRouter(l.Handler())

	// Exhaust the limit: 2 pass, 3rd blocks.
	if code, _ := call(r, "8.8.8.8"); code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", code)
	}
	if code, _ := call(r, "8.8.8.8"); code != http.StatusOK {
		t.Fatalf("second: want 200, got %d", code)
	}
	if code, _ := call(r, "8.8.8.8"); code != http.StatusTooManyRequests {
		t.Fatalf("third: want 429, got %d", code)
	}

	// Advance miniredis's clock past the 1-minute window. TTL'd keys
	// disappear at this point exactly like they would in real Redis.
	mr.FastForward(time.Minute + time.Second)

	if code, _ := call(r, "8.8.8.8"); code != http.StatusOK {
		t.Fatalf("after window reset: want 200, got %d", code)
	}
}

// -----------------------------------------------------------------------------
// Separate clients have separate buckets: one IP hitting the limit does
// not affect another IP.
// -----------------------------------------------------------------------------

func TestLimiter_SeparateClientsHaveSeparateBuckets(t *testing.T) {
	_, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 1, time.Minute, middleware.ByIP)
	r := newRouter(l.Handler())

	if code, _ := call(r, "1.1.1.1"); code != http.StatusOK {
		t.Fatalf("IP1 first call: want 200, got %d", code)
	}
	if code, _ := call(r, "1.1.1.1"); code != http.StatusTooManyRequests {
		t.Fatalf("IP1 second call: want 429, got %d", code)
	}
	// A different IP must still be allowed — bucket keyed by IP, not global.
	if code, _ := call(r, "2.2.2.2"); code != http.StatusOK {
		t.Fatalf("IP2 first call: want 200 (separate bucket), got %d", code)
	}
}

// -----------------------------------------------------------------------------
// ByAuthUser fails OPEN when there's no authenticated user on context.
//
// This is the "middleware misconfigured" safety net: if someone wires
// ByAuthUser without RequireAuth in front of it, we let requests pass
// through rather than blocking every anonymous request. The
// unauthenticated request will still get 401 downstream from whatever
// requires auth — the limiter isn't the right layer to be the auth
// gate too.
// -----------------------------------------------------------------------------

func TestLimiter_ByAuthUser_FailsOpenWithoutAuthContext(t *testing.T) {
	_, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 1, time.Minute, middleware.ByAuthUser)
	r := newRouter(l.Handler())

	// No RequireAuth in this chain, so nothing puts a user_id on ctx.
	// The limit is 1/min per user, but no user identity = no bucket =
	// no rate limiting. Fire five requests; every one should pass.
	for i := 1; i <= 5; i++ {
		if code, _ := call(r, "1.1.1.1"); code != http.StatusOK {
			t.Fatalf("call %d without auth context: want 200 (limiter no-op), got %d", i, code)
		}
	}
}

// -----------------------------------------------------------------------------
// ByAuthUser DOES limit per user when there IS a user on context.
// Simulates what happens with RequireAuth in the chain in front of it.
// -----------------------------------------------------------------------------

func TestLimiter_ByAuthUser_LimitsPerUser(t *testing.T) {
	_, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 2, time.Minute, middleware.ByAuthUser)

	// Fake "RequireAuth" middleware that unconditionally stashes a
	// user_id on the request context. Two variants swap the id so we
	// can prove per-user bucketing.
	stashUser := func(uid int) gin.HandlerFunc {
		return func(c *gin.Context) {
			// The real RequireAuth calls c.Request = c.Request.WithContext(...).
			// We do the same shape here so ByAuthUser reads it via
			// c.Request.Context() → middleware.UserIDFrom(ctx).
			ctx := middleware.WithUserID(c.Request.Context(), uid)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		}
	}

	// Router for user 7.
	r7 := newRouter(stashUser(7), l.Handler())
	if code, _ := call(r7, "1.1.1.1"); code != http.StatusOK {
		t.Fatalf("u7 call 1: want 200, got %d", code)
	}
	if code, _ := call(r7, "1.1.1.1"); code != http.StatusOK {
		t.Fatalf("u7 call 2: want 200, got %d", code)
	}
	if code, _ := call(r7, "1.1.1.1"); code != http.StatusTooManyRequests {
		t.Fatalf("u7 call 3: want 429, got %d", code)
	}

	// Different user, same IP — separate bucket, first call passes.
	r8 := newRouter(stashUser(8), l.Handler())
	if code, _ := call(r8, "1.1.1.1"); code != http.StatusOK {
		t.Fatalf("u8 call 1 (different user, same IP): want 200, got %d", code)
	}
}

// -----------------------------------------------------------------------------
// Redis unreachable → fail OPEN (don't block legitimate traffic).
// Simulated by closing miniredis before making requests.
// -----------------------------------------------------------------------------

func TestLimiter_FailsOpenWhenRedisUnreachable(t *testing.T) {
	mr, rdb := setup(t)
	l := middleware.NewLimiter(rdb, "test", 1, time.Minute, middleware.ByIP)
	r := newRouter(l.Handler())

	mr.Close() // simulate Redis outage; INCR will now error

	// Even with limit=1, five requests should still pass because the
	// limiter can't reach Redis to enforce.
	for i := 1; i <= 5; i++ {
		if code, _ := call(r, "3.3.3.3"); code != http.StatusOK {
			t.Fatalf("call %d with Redis down: want 200 (fail open), got %d", i, code)
		}
	}
}

// Compile-time assertion that context.Context is the type ByAuthUser
// reads from — protects against a signature drift on middleware.UserIDFrom.
var _ = func() bool {
	var ctx context.Context = context.Background()
	_, _ = middleware.UserIDFrom(ctx)
	return true
}()
