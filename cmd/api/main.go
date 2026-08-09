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
	"subscription-service/internal/notifications"
	"subscription-service/internal/repository"
	"subscription-service/internal/services"
)

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

	// Dependency injection, done by hand. Each layer is constructed with
	// the one below it. No framework, no container — just function calls.
	repo := repository.NewPostgresRepo(db)
	svc := services.NewSubscriptionService(repo, rdb, notifQueue)
	h := handlers.NewSubscriptionHandler(svc)

	r := gin.Default()
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	h.Register(r)

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
