// Command worker runs the background expiration sweep.
//
// This is a SEPARATE binary from cmd/api on purpose:
//   - It serves no HTTP. Expiration must run whether or not any user is
//     hitting the API, which is why it can't live inside a request handler.
//   - It can be scaled, restarted, or crashed independently of the API.
//   - Building it as `cmd/worker/main.go` means `go run ./cmd/worker`
//     produces a distinct process — you'll run the API in one terminal
//     and the worker in another.
//
// It shares internal/repository, internal/services, and internal/cache
// with the API — same code, same behavior — so the invalidations the
// worker performs are byte-identical to the ones the API performs.
package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/notifications"
	"subscription-service/internal/repository"
	"subscription-service/internal/services"
)

const (
	// 30s is short enough to see the worker tick during manual testing.
	// PRODUCTION would use 1–5 minutes: expiration timing doesn't need to
	// be second-precise (a subscription being "expired" 90s late is
	// invisible to users), and a longer interval means fewer wasted
	// no-op sweeps against Postgres. Never zero this out — polling too
	// often just burns CPU and DB connections.
	sweepInterval = 30 * time.Second
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://subs:subs@localhost:5433/subscriptions?sslmode=disable"
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		// Match the API's behavior: Redis-down is a warning, not fatal.
		// The expiration UPDATE still works; cache invalidations will just
		// log failures and the API's TTL backstop takes over.
		log.Printf("WARN: redis ping failed (%v) — invalidations will be no-ops", err)
	}

	// In-process notification queue + consumer (Phase 4). Each process
	// owns its own queue because Go channels don't cross process
	// boundaries. When we swap to SQS, both API and worker publish to
	// the same real queue and this local consumer moves to its own
	// binary.
	notifQueue := notifications.NewQueue(128)

	repo := repository.NewPostgresRepo(db)
	// The worker only calls ExpireOverdue, never Subscribe or the two
	// webhook-only Mark* methods, so it has no reason to hold a Stripe
	// client. Passing nil is safe as long as that invariant holds — a nil
	// interface will panic if actually invoked, which is louder than a
	// silent noop and easier to catch if someone regresses this.
	svc := services.NewSubscriptionService(repo, rdb, notifQueue, nil)

	// A context that we can cancel on SIGINT/SIGTERM so an in-flight sweep
	// aborts cleanly instead of being killed mid-UPDATE.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go notifQueue.Consume(ctx)

	// Watch for shutdown signals in a goroutine; cancel the ctx when one
	// arrives so the main loop exits at the next iteration.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-quit
		log.Printf("received %s, shutting down worker...", sig)
		cancel()
	}()

	log.Printf("worker started, sweep interval = %s", sweepInterval)

	// Run once immediately on boot so a freshly restarted worker doesn't
	// wait a full interval before catching up on anything that expired
	// while it was down.
	runSweep(ctx, svc)

	// time.NewTicker fires on a fixed cadence via a channel. The
	// `for { select {} }` pattern is idiomatic Go for "do X on a timer
	// AND stop when told to." Without the ctx.Done() branch the loop
	// would ignore shutdown signals.
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("worker stopped")
			return
		case <-ticker.C:
			runSweep(ctx, svc)
		}
	}
}

// runSweep executes one iteration and logs the result — always logs, even
// on zero-count ticks, so you can see the worker is alive.
func runSweep(ctx context.Context, svc *services.SubscriptionService) {
	// Per-sweep timeout: if a sweep hangs on the DB for more than 20s
	// (never happens in practice with our tiny data set, but the pattern
	// is important), we abort rather than pile up requests.
	sweepCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	start := time.Now()
	expired, err := svc.ExpireOverdue(sweepCtx)
	if err != nil {
		log.Printf("sweep FAILED after %s: %v", time.Since(start), err)
		return
	}
	if len(expired) == 0 {
		log.Printf("sweep OK in %s: 0 expired", time.Since(start))
		return
	}
	ids := make([]int, len(expired))
	for i, e := range expired {
		ids[i] = e.ID
	}
	log.Printf("sweep OK in %s: %d expired, ids=%v", time.Since(start), len(expired), ids)
}
