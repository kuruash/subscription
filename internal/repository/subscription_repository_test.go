//go:build integration

// Integration tests for the Postgres repository.
//
// Gated by the `integration` build tag so `go test ./...` stays fast and
// doesn't need Docker running. To execute this file:
//   go test -tags integration ./internal/repository/...
//
// APPROACH: testcontainers-go/modules/postgres.
// Each `go test` run spins up a fresh Postgres 16 container, applies our
// migration once, and tears it down when TestMain returns. Reasons:
//   - Self-contained: only requires Docker to be running; no separate
//     compose stack, no manually-created test DB, no "did you remember
//     to..." step.
//   - Fully disposable: each test-binary run gets a clean container. No
//     dev-side data can leak into tests, no test data can leak into dev.
//   - Matches the modern Go convention interviewers recognize.
//   - Alternative considered (a separate `subscriptions_test` database
//     inside the running docker-compose Postgres): simpler dep-wise but
//     requires manual setup, and if a dev is mid-experiment on the same
//     Postgres, tests could interfere with their work.
//
// TEST ISOLATION between individual tests:
// The container is shared for the whole test-binary run (start-up is
// slow; per-test would be brutal). Each test starts with resetDB(t)
// which TRUNCATE...RESTART IDENTITY CASCADEs every table so it always
// begins from a known empty state. That's what makes `go test` twice
// in a row produce identical results.
package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"subscription-service/internal/models"
	"subscription-service/internal/repository"
)

var testDB *sql.DB

// TestMain is Go's per-package test lifecycle hook. Anything here runs
// once before all tests in the package and once after. Perfect for
// "expensive setup, share across tests" like a container.
func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("subs_test"),
		tcpostgres.WithUsername("subs"),
		tcpostgres.WithPassword("subs"),
		tcpostgres.WithInitScripts("../../migrations/001_init.sql"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("terminate: %v", err)
		}
	}()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}

	testDB, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer testDB.Close()

	if err := testDB.PingContext(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	os.Exit(m.Run())
}

// resetDB wipes every table and re-seeds one user and one creator that
// most tests want. RESTART IDENTITY re-zeros the sequence so ids are
// deterministic across runs. CASCADE deals with the transactions FK.
func resetDB(t *testing.T) {
	t.Helper()
	if _, err := testDB.Exec(`TRUNCATE users, creators, subscriptions, transactions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := testDB.Exec(`INSERT INTO users (username, email) VALUES ('alice', 'alice@test.local')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := testDB.Exec(`INSERT INTO creators (name) VALUES ('Ninja')`); err != nil {
		t.Fatalf("seed creator: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The partial unique index guarantee (Phase 1)
// -----------------------------------------------------------------------------

func TestCreate_PartialUniqueIndexRejectsSecondActive(t *testing.T) {
	resetDB(t)
	repo := repository.NewPostgresRepo(testDB)
	ctx := context.Background()

	// First subscribe — should succeed.
	first := &models.Subscription{UserID: 1, CreatorID: 1, Plan: "monthly", AutoRenew: true}
	if _, err := repo.Create(ctx, first, 4.99); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second subscribe for the same (user, creator) while first is still
	// active — the partial unique index must reject it. This is the
	// Phase 1 race-condition guarantee.
	second := &models.Subscription{UserID: 1, CreatorID: 1, Plan: "monthly", AutoRenew: true}
	_, err := repo.Create(ctx, second, 4.99)
	if !errors.Is(err, repository.ErrDuplicateActive) {
		t.Fatalf("expected ErrDuplicateActive, got %v", err)
	}

	// Cancel the first, then a re-subscribe should succeed (the index
	// only cares about ACTIVE rows).
	if _, err := repo.Cancel(ctx, first.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	third := &models.Subscription{UserID: 1, CreatorID: 1, Plan: "monthly", AutoRenew: true}
	if _, err := repo.Create(ctx, third, 4.99); err != nil {
		t.Fatalf("re-subscribe after cancel should succeed: %v", err)
	}
}

// -----------------------------------------------------------------------------
// ExpireOverdue selectivity (Phase 3)
// -----------------------------------------------------------------------------

func TestExpireOverdue_OnlyTouchesActiveOverdueRows(t *testing.T) {
	resetDB(t)
	// Second creator so we can have multiple simultaneous active subs.
	if _, err := testDB.Exec(`INSERT INTO creators (name) VALUES ('Pokimane')`); err != nil {
		t.Fatalf("seed creator2: %v", err)
	}
	// Second user for the cancelled-row case (avoids the partial-unique-
	// index blocking us from inserting three rows for the same pair).
	if _, err := testDB.Exec(`INSERT INTO users (username, email) VALUES ('bob', 'bob@test.local')`); err != nil {
		t.Fatalf("seed user2: %v", err)
	}

	now := time.Now().UTC()
	insertSub := func(userID, creatorID int, status string, expires time.Time) int {
		var id int
		err := testDB.QueryRow(`
			INSERT INTO subscriptions (user_id, creator_id, plan, status, start_date, expires_at, auto_renew)
			VALUES ($1, $2, 'monthly', $3, $4, $5, true)
			RETURNING id`,
			userID, creatorID, status, now.Add(-30*24*time.Hour), expires,
		).Scan(&id)
		if err != nil {
			t.Fatalf("insert sub: %v", err)
		}
		return id
	}

	overdueActive := insertSub(1, 1, models.StatusActive, now.Add(-time.Hour))     // should flip
	freshActive := insertSub(1, 2, models.StatusActive, now.Add(24*time.Hour))     // must NOT flip
	alreadyCancelled := insertSub(2, 1, models.StatusCancelled, now.Add(-time.Hour)) // overdue but not active — must NOT flip

	repo := repository.NewPostgresRepo(testDB)
	expired, err := repo.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("ExpireOverdue: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != overdueActive {
		t.Fatalf("expected exactly [%d], got %+v", overdueActive, expired)
	}

	// Confirm the DB state matches the RETURNING output.
	statusOf := func(id int) string {
		var s string
		if err := testDB.QueryRow(`SELECT status FROM subscriptions WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("select status %d: %v", id, err)
		}
		return s
	}
	if got := statusOf(overdueActive); got != models.StatusExpired {
		t.Errorf("overdue active: want expired, got %s", got)
	}
	if got := statusOf(freshActive); got != models.StatusActive {
		t.Errorf("fresh active: want active, got %s", got)
	}
	if got := statusOf(alreadyCancelled); got != models.StatusCancelled {
		t.Errorf("cancelled: want cancelled (untouched), got %s", got)
	}

	// Idempotency check: a second sweep should return zero rows.
	second, err := repo.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("expected zero rows on second sweep, got %+v", second)
	}
}
