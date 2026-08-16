// Command api is the HTTP server entry point.
//
// Go convention: anything under cmd/<name>/main.go compiles to a binary
// named <name>. Building this from the repo root:
//     go run ./cmd/api
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
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/handlers"
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
	// Public routes — no auth required.
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	authH.Register(r)

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
	protected := r.Group("", middleware.RequireAuth(jwtSecret))
	subH.Register(protected)

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
