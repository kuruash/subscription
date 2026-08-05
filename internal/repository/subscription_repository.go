// Package repository is the ONLY layer allowed to write SQL.
// It exposes an interface (the contract) and a Postgres implementation.
// Services depend on the interface, not the concrete type — that's what
// lets tests swap in a fake without a real database.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"subscription-service/internal/models"
)

// ErrDuplicateActive is returned when the partial unique index rejects an
// insert because an active subscription already exists for (user, creator).
// Defining a sentinel error lets the service layer test with errors.Is()
// instead of string-matching driver messages.
var ErrDuplicateActive = errors.New("active subscription already exists for this user and creator")
var ErrNotFound = errors.New("subscription not found")

// SubscriptionRepository is a Go interface: a set of method signatures.
// Any type that has ALL these methods automatically "implements" it —
// no `implements` keyword, no explicit declaration. This is called
// structural / duck typing.
type SubscriptionRepository interface {
	Create(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error)
	GetByID(ctx context.Context, id int) (*models.Subscription, error)
	ListByUser(ctx context.Context, userID int) ([]models.Subscription, error)
	Cancel(ctx context.Context, id int) error
	Renew(ctx context.Context, id int) (*models.Subscription, error)
}

// postgresRepo is the concrete Postgres implementation.
// Lowercase name = unexported; callers get it only via NewPostgresRepo,
// and they hold it as the SubscriptionRepository interface.
type postgresRepo struct {
	db *sql.DB
}

// NewPostgresRepo returns the interface type, not the struct — this forces
// callers to program against the abstraction from day one.
func NewPostgresRepo(db *sql.DB) SubscriptionRepository {
	return &postgresRepo{db: db}
}

// Create inserts the subscription AND its initial transaction row atomically.
// Either both succeed or both roll back — no half-written state.
func (r *postgresRepo) Create(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error) {
	// BeginTx starts a SQL transaction. The `ctx` here means "if the caller
	// cancels (e.g. the HTTP client disconnects), abort this DB work too."
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// THE DEFER ROLLBACK PATTERN:
	// `defer` runs this line when the function returns, for ANY reason
	// (normal return, error return, panic). We schedule a Rollback right
	// away so that if we return early on an error below, cleanup happens
	// automatically.
	// If we successfully Commit() further down, the Rollback becomes a
	// no-op — Postgres just says "transaction already finished" and it's
	// safe. So this is not a bug; it's the standard "always clean up,
	// commit is the exception" idiom.
	defer tx.Rollback()

	now := time.Now().UTC()
	sub.StartDate = now
	sub.ExpiresAt = now.Add(30 * 24 * time.Hour)
	sub.Status = models.StatusActive

	// QueryRowContext with RETURNING gets the DB-assigned id + created_at
	// back in the same round trip. Scan() reads the returned columns into
	// the pointers you pass.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO subscriptions
		    (user_id, creator_id, plan, status, start_date, expires_at, auto_renew)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at
	`, sub.UserID, sub.CreatorID, sub.Plan, sub.Status,
		sub.StartDate, sub.ExpiresAt, sub.AutoRenew,
	).Scan(&sub.ID, &sub.CreatedAt)

	if err != nil {
		// Translate the Postgres-specific unique-violation code (23505)
		// into our sentinel error. The service/handler layers never need
		// to know pq exists.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrDuplicateActive
		}
		return nil, fmt.Errorf("insert subscription: %w", err)
	}

	// Second write in the same transaction — the payment record.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO transactions (subscription_id, amount, currency, status)
		VALUES ($1, $2, 'usd', $3)
	`, sub.ID, amount, models.TxStatusSucceeded)
	if err != nil {
		return nil, fmt.Errorf("insert transaction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return sub, nil
}

func (r *postgresRepo) GetByID(ctx context.Context, id int) (*models.Subscription, error) {
	var s models.Subscription
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, creator_id, plan, status, start_date, expires_at, auto_renew, created_at
		FROM subscriptions WHERE id = $1
	`, id).Scan(&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status,
		&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *postgresRepo) ListByUser(ctx context.Context, userID int) ([]models.Subscription, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, creator_id, plan, status, start_date, expires_at, auto_renew, created_at
		FROM subscriptions WHERE user_id = $1 ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	// Every rows.Query result MUST be closed — otherwise you leak a DB
	// connection back to the pool. `defer` guarantees it runs on any exit.
	defer rows.Close()

	// A nil slice and an empty slice both serialize as `[]` in JSON, so
	// we don't need to pre-allocate.
	var out []models.Subscription
	for rows.Next() {
		var s models.Subscription
		if err := rows.Scan(&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status,
			&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *postgresRepo) Cancel(ctx context.Context, id int) error {
	// Soft delete: flip status, don't actually remove the row. Preserves
	// history for the transactions FK and for support/analytics.
	res, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = $1
		WHERE id = $2 AND status = $3
	`, models.StatusCancelled, id, models.StatusActive)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound // either no such id, or it wasn't active
	}
	return nil
}

func (r *postgresRepo) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	// Add 30 days to the EXISTING expires_at, not to now() — otherwise
	// renewing a day early would cost you a day.
	var s models.Subscription
	err := r.db.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET expires_at = expires_at + interval '30 days'
		WHERE id = $1 AND status = $2
		RETURNING id, user_id, creator_id, plan, status, start_date, expires_at, auto_renew, created_at
	`, id, models.StatusActive).Scan(
		&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status,
		&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
