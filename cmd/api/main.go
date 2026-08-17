// Command api is the HTTP server entry point.
//
// Go convention: anything under cmd/<name>/main.go compiles to a binary
// named <name>. Building this from the repo root:
//
//	go run ./cmd/api
//
// The `package main` + `func main()` combo is what makes it an executable
// rather than a library.
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq" // blank import: registers the "postgres" driver
	// with database/sql. We never call pq directly here,
	// but without this line sql.Open("postgres", ...) fails.
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/handlers"
	"subscription-service/internal/metrics"
	"subscription-service/internal/middleware"
	"subscription-service/internal/notifications"
	"subscription-service/internal/payments"
	"subscription-service/internal/repository"
	"subscription-service/internal/services"
)

// head returns the first n runes of s, or all of s if shorter. Used
// only for logging secret fingerprints — see the stripe env-wiring
// block in main().
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// Sensible default matching docker-compose.yml so `go run` works
		// out of the box for local dev.
		dsn = "postgres://subs:subs@localhost:5433/subscriptions?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err) // log.Fatal calls os.Exit(1)
	}
	defer db.Close()

	// sql.Open is lazy — it doesn't actually connect. Ping forces a real
	// connection so we fail fast at startup instead of on the first request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	// --- Redis ---
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379" // matches docker-compose.yml
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// Non-fatal: the service is designed to degrade to DB-only if Redis
		// is unreachable. We log loudly and keep going.
		log.Printf("WARN: redis ping failed (%v) — will run without cache", err)
	}

	// --- Notification queue (Phase 4) ---
	// In-process channel; the consumer lives in THIS process because a
	// Go channel can't be read from another process. When we swap to SQS,
	// the consumer can move to its own binary (cmd/notifier) — the
	// service side stays exactly the same because it only knows about
	// notifications.Publisher.
	notifQueue := notifications.NewQueue(128)
	// Consumer goroutine — one is plenty at this scale.
	notifCtx, notifCancel := context.WithCancel(context.Background())
	defer notifCancel()
	go notifQueue.Consume(notifCtx)

	// --- JWT secret (Phase 5) ---
	// Read from env, never hardcoded. In dev we fall back to a placeholder
	// so `go run` still works, but LOG A LOUD WARNING because that
	// secret is public knowledge (anyone reading this source can mint a
	// token). Real deployments MUST set JWT_SECRET.
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		log.Printf("WARN: JWT_SECRET not set — using an insecure development default. DO NOT DEPLOY LIKE THIS.")
		jwtSecret = []byte("dev-only-insecure-secret-change-me")
	}

	// --- Stripe (Phase 7) ---
	// Two secrets from env: the secret API key (sk_test_... / sk_live_...)
	// for creating PaymentIntents, and the webhook signing secret
	// (whsec_...) for verifying inbound webhooks. Both are printed by
	// `stripe login` and `stripe listen` for local dev.
	//
	// If either is missing we log a WARN and pass a nil payments client.
	// That's deliberate: it lets `go run` come up for developers who
	// aren't working on the Stripe flow, but any request that reaches
	// service.Subscribe will crash with a nil-pointer panic — a loud
	// failure that surfaces the misconfiguration instantly, rather than
	// silently accepting Subscribes that quietly do nothing.
	stripeKey := os.Getenv("STRIPE_SECRET_KEY")
	stripeWebhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	var payClient payments.Client
	if stripeKey == "" || stripeWebhookSecret == "" {
		log.Printf("WARN: STRIPE_SECRET_KEY and/or STRIPE_WEBHOOK_SECRET not set — /subscriptions and /webhooks/stripe will not work until both are provided.")
	} else {
		// Log a fingerprint (first 12 chars) of each secret so mismatches
		// with `stripe listen --print-secret` and the Stripe dashboard are
		// eyeball-verifiable at startup. Not the full secret — that would
		// defeat the point of a secret.
		log.Printf("stripe: SECRET_KEY=%s... WEBHOOK_SECRET=%s...", head(stripeKey, 12), head(stripeWebhookSecret, 12))
		payClient = payments.NewClient(stripeKey, stripeWebhookSecret)
	}

	// Dependency injection, done by hand. Each layer is constructed with
	// the one below it. No framework, no container — just function calls.
	repo := repository.NewPostgresRepo(db)
	svc := services.NewSubscriptionService(repo, rdb, notifQueue, payClient)
	subH := handlers.NewSubscriptionHandler(svc)
	authH := handlers.NewAuthHandler(jwtSecret)

	r := gin.Default()

	// --- Metrics middleware (Phase 9) ---
	//
	// Attached BEFORE any other middleware so the duration it records
	// covers the full handler chain (auth parse, rate-limit lookup,
	// DB calls, JSON marshal). If we put it after RequireAuth we'd
	// under-count latency by the auth cost — a bad trade because auth
	// cost is one of the things we want to see if it ever spikes.
	//
	// The middleware itself is <300 ns/request steady state — well
	// below the noise floor of any real handler work. See the
	// PERFORMANCE note at the top of internal/middleware/metrics.go
	// for the actual numbers.
	r.Use(middleware.Metrics())

	// Public routes — no auth required.
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	// GET /metrics — Prometheus scrape endpoint.
	//
	// PUBLIC (no JWT). Same reasoning as POST /webhooks/stripe: the
	// caller is a metrics scraper (Prometheus, a sidecar, a Grafana
	// Agent), not a logged-in user. It doesn't have — and shouldn't
	// need — one of our tokens; requiring one would mean sharing a
	// JWT secret with the scraping infrastructure, which is a much
	// worse security posture than leaving /metrics readable.
	//
	// In production the /metrics endpoint is typically network-
	// isolated (a private port, a mesh-only path, or an IP allowlist
	// at the LB) so exposure is limited to the scraper. We don't
	// implement that here — trivial to add later with a middleware
	// that checks c.ClientIP() against an allowlist, or by binding
	// /metrics to a second http.Server on a different port.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// --- Rate limiters (Phase 8) ---
	//
	// Two limiters, different keyer strategies:
	//   - LoginByIP fronts POST /login. IP-based because there's no
	//     authenticated identity yet on that route by definition —
	//     /login is where identity gets minted. 10/min per IP.
	//   - SubscriptionsByUser guards the protected subscription routes.
	//     User-based because on the protected group we DO have an
	//     authenticated user_id (RequireAuth put it on the context),
	//     and per-user bucketing avoids penalizing NAT'd corporate
	//     networks where many legit users share one egress IP.
	//     60/min per authenticated user.
	//
	// See internal/middleware/ratelimit.go for the numbers' justification.
	loginLimiter := middleware.LoginByIP(rdb)
	subLimiter := middleware.SubscriptionsByUser(rdb)

	// /login goes into a group whose only middleware is the IP limiter.
	// The route itself is otherwise unauthenticated (Phase 5's password
	// gap is deliberate). Registering via a group with the middleware
	// attached is cleaner than mutating the handler to take a middleware
	// argument — auth handler stays ignorant of rate limiting.
	loginGroup := r.Group("", loginLimiter.Handler())
	authH.Register(loginGroup)

	// --- Stripe webhook, PUBLIC (no JWT) ---
	//
	// Stripe is the caller here, not a user, so JWT would make no sense
	// — Stripe doesn't have one of our tokens. The signature check
	// inside the handler (payments.VerifyWebhookSignature) is what
	// actually authenticates the request. This is why the route is
	// registered on the root router, not the `protected` group below.
	//
	// RAW BODY GOTCHA — Stripe computes the HMAC signature over the
	// EXACT bytes it sent. Anything that reads or mutates
	// c.Request.Body before the handler gets it will invalidate the
	// signature and every webhook will 400.
	//
	// Concretely, these Gin patterns would break signature verification
	// if we ever added them without excluding this route:
	//   - c.ShouldBindJSON / c.BindJSON in a middleware — reads and
	//     parses the body; body is drained by the time our handler
	//     calls io.ReadAll and we'd verify against an empty payload.
	//   - Body-logging middleware that does io.ReadAll then wraps the
	//     body back with io.NopCloser(bytes.NewReader(...)) — even if
	//     it restores byte-for-byte, ANY re-encoding (json.Marshal
	//     round-trip, whitespace normalization) invalidates the signature.
	//   - http.MaxBytesReader / body-size middleware that truncates
	//     large payloads — signature computed over the truncated bytes
	//     won't match Stripe's signature over the full bytes.
	//   - Compression/decompression middleware that changes byte-for-
	//     byte content.
	//
	// Current status: gin.Default() only wires Logger + Recovery, neither
	// of which reads the body, so nothing is between io.ReadAll and the
	// raw wire bytes today. But this is a landmine for any future
	// middleware — the mitigation is either (a) always register this
	// route BEFORE any body-reading middleware, or (b) exclude the path
	// inside such middleware. (a) is what we do here.
	stripeWebhookH := handlers.NewStripeWebhookHandler(svc, payClient)
	stripeWebhookH.Register(r)

	// Protected routes — every /subscriptions and /users/:id/subscriptions
	// path requires a valid JWT. Grouping means adding a new route to the
	// group inherits the middleware automatically.
	//
	// MIDDLEWARE ORDER MATTERS: RequireAuth MUST run before
	// subLimiter.Handler(). subLimiter uses middleware.ByAuthUser which
	// reads user_id from the context — the id RequireAuth put there.
	// If we reversed the order, ByAuthUser would find no user_id, fall
	// through as a no-op (fail open), and the per-user limit would
	// silently do nothing. Gin runs middlewares left-to-right, so
	// (RequireAuth, subLimiter) is the correct order.
	protected := r.Group("", middleware.RequireAuth(jwtSecret), subLimiter.Handler())
	subH.Register(protected)

	// --- Active-subscriptions gauge updater (Phase 9) ---
	//
	// A goroutine that runs a `SELECT COUNT(*) WHERE status='active'`
	// every 15s and sets the gauge. Design tradeoff (periodic query
	// vs increment/decrement on state transitions) is documented in
	// full at internal/metrics/metrics.go — TL;DR: periodic query
	// cannot drift, works trivially across horizontally-scaled
	// instances (they all query the same DB), and 4 indexed COUNTs
	// per minute is cost noise.
	//
	// Runs in the API process only, not the worker, because the API
	// serves /metrics — worker doesn't need to update this gauge
	// (they'd both write and either read the same value).
	gaugeCtx, gaugeCancel := context.WithCancel(context.Background())
	defer gaugeCancel()
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		refresh := func() {
			var n int
			qctx, qcancel := context.WithTimeout(gaugeCtx, 2*time.Second)
			defer qcancel()
			if err := db.QueryRowContext(qctx, `SELECT COUNT(*) FROM subscriptions WHERE status = 'active'`).Scan(&n); err != nil {
				log.Printf("metrics: active_subscriptions refresh failed: %v", err)
				return
			}
			metrics.ActiveSubscriptions.Set(float64(n))
		}
		refresh() // don't wait 15s for the first value
		for {
			select {
			case <-gaugeCtx.Done():
				return
			case <-tick.C:
				refresh()
			}
		}
	}()

	// Graceful shutdown: run the server in a goroutine, then wait for
	// SIGINT/SIGTERM in the main goroutine. On signal, tell the server
	// to stop accepting new connections and let in-flight ones finish.
	srv := &http.Server{Addr: ":8080", Handler: r}
	go func() {
		log.Println("listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// A channel is Go's built-in typed pipe. signal.Notify pushes onto it
	// when the OS sends the listed signals; the <-quit blocks until one
	// arrives. This is the canonical shutdown pattern.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}
