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

// ErrDuplicateEvent is returned by MarkPaymentSucceeded when the same
// Stripe event id has already produced a transaction row. This is the
// idempotency path — Stripe retries webhook deliveries on any non-2xx,
// so a duplicate delivery must fail the INSERT (via the partial UNIQUE
// index on stripe_event_id) and get translated into a 200 OK upstream
// so Stripe stops retrying. Distinct from ErrDuplicateActive so the
// handler knows *why* the insert failed.
var ErrDuplicateEvent = errors.New("stripe event already processed")

// ExpiredSub is what ExpireOverdue returns per row: enough for the caller
// to invalidate the affected user's cache AND publish a notification
// event without a second lookup.
type ExpiredSub struct {
	ID        int
	UserID    int
	CreatorID int
}

// SubscriptionRepository is a Go interface: a set of method signatures.
// Any type that has ALL these methods automatically "implements" it —
// no `implements` keyword, no explicit declaration. This is called
// structural / duck typing.
type SubscriptionRepository interface {
	// CreatePending inserts a subscription row in 'pending' state with the
	// caller-supplied PaymentIntentID. No transaction row is written — the
	// transaction only exists once Stripe confirms the payment via webhook.
	// Returns ErrDuplicateActive if the partial unique index rejects this
	// as a duplicate active-or-pending row for (user_id, creator_id).
	CreatePending(ctx context.Context, sub *models.Subscription) (*models.Subscription, error)

	// MarkPaymentSucceeded is called from the Stripe webhook path when we
	// receive payment_intent.succeeded. It atomically:
	//   1. Inserts a transactions row tagged with stripe_event_id — the
	//      partial UNIQUE index makes duplicate deliveries fail here.
	//   2. Flips the matching subscription from 'pending' to 'active'.
	// Returns ErrDuplicateEvent if the event was already processed (safe
	// to translate into 200 OK for Stripe). Returns ErrNotFound if no
	// subscription exists for the given PaymentIntent id.
	MarkPaymentSucceeded(ctx context.Context, paymentIntentID, stripeEventID string, amountCents int64) (*models.Subscription, error)

	// MarkPaymentFailed cancels the pending subscription for a
	// PaymentIntent. Cancelling (not deleting) so the row's history is
	// preserved and the partial unique index no longer blocks re-subscribe.
	MarkPaymentFailed(ctx context.Context, paymentIntentID string) (*models.Subscription, error)

	GetByID(ctx context.Context, id int) (*models.Subscription, error)
	ListByUser(ctx context.Context, userID int) ([]models.Subscription, error)
	// Cancel returns the affected subscription's user_id so callers can
	// invalidate that user's cache. Returns ErrNotFound if the row doesn't
	// exist or isn't active.
	Cancel(ctx context.Context, id int) (userID int, err error)
	Renew(ctx context.Context, id int) (*models.Subscription, error)

	// ExpireOverdue flips every active-but-past-expiration subscription to
	// 'expired' in a single atomic UPDATE and returns what changed. Safe
	// to call as often as you want — running it on already-expired rows
	// is a no-op (see IDEMPOTENCY note on the implementation).
	ExpireOverdue(ctx context.Context) ([]ExpiredSub, error)
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

// CreatePending inserts a subscription in 'pending' state.
//
// No transactions row is written here — a transaction row means "money
// moved" and money has not moved yet. Only the Stripe webhook path,
// through MarkPaymentSucceeded, is allowed to write to transactions in
// Phase 7. Keeping this rule crisp is what makes the
// transactions.stripe_event_id UNIQUE constraint a meaningful idempotency
// key: if there's a transaction row, we processed a Stripe event; if
// there isn't, we didn't.
func (r *postgresRepo) CreatePending(ctx context.Context, sub *models.Subscription) (*models.Subscription, error) {
	now := time.Now().UTC()
	sub.StartDate = now
	sub.ExpiresAt = now.Add(30 * 24 * time.Hour)
	sub.Status = models.StatusPending

	// Single-statement INSERT — no BEGIN/COMMIT needed since there's
	// only one write. The partial unique index on (user_id, creator_id)
	// WHERE status IN ('active','pending') is what prevents two
	// concurrent Subscribe calls from producing two pending rows for
	// the same pair; one wins, the other gets 23505 → ErrDuplicateActive.
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO subscriptions
		    (user_id, creator_id, plan, status, start_date, expires_at, auto_renew, payment_intent_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at
	`, sub.UserID, sub.CreatorID, sub.Plan, sub.Status,
		sub.StartDate, sub.ExpiresAt, sub.AutoRenew, sub.PaymentIntentID,
	).Scan(&sub.ID, &sub.CreatedAt)

	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrDuplicateActive
		}
		return nil, fmt.Errorf("create pending subscription: %w", err)
	}
	return sub, nil
}

// MarkPaymentSucceeded flips pending → active AND records the payment.
//
// IDEMPOTENCY:
// The order matters. We INSERT the transaction row (which carries
// stripe_event_id) BEFORE the UPDATE. If Stripe redelivers the same
// event, the INSERT fails with 23505 — the partial UNIQUE index on
// stripe_event_id catches it — and we return ErrDuplicateEvent. The
// caller (webhook handler) translates that into 200 OK so Stripe stops
// retrying an event we've already handled.
//
// Doing INSERT first means "we recorded processing this event" is the
// atomic gate. If we UPDATE'd first and then INSERT'd, a redelivery
// would UPDATE a no-op (pending is already active), then fail the
// INSERT — same outcome, but the state machine is harder to reason
// about because "row is active" isn't equivalent to "event was recorded."
//
// UPDATE-returns-0-rows case: the subscription exists but isn't pending
// (e.g. it was cancelled between PI creation and confirmation). We
// deliberately DO NOT resurrect a cancelled row. The transaction row
// still gets committed — that's the money-movement record for
// reconciliation later. Callers see the row's current status in the
// returned struct.
func (r *postgresRepo) MarkPaymentSucceeded(ctx context.Context, paymentIntentID, stripeEventID string, amountCents int64) (*models.Subscription, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Find the subscription this PaymentIntent belongs to. We fail
	//    fast with ErrNotFound if it's missing — a payment for a
	//    subscription we don't know about is a data-integrity signal,
	//    not a business event to process.
	var s models.Subscription
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, creator_id, plan, status, payment_intent_id,
		       start_date, expires_at, auto_renew, created_at
		FROM subscriptions
		WHERE payment_intent_id = $1
	`, paymentIntentID).Scan(
		&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status, &s.PaymentIntentID,
		&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup by payment intent: %w", err)
	}

	// 2. Insert the transaction row. The stripe_event_id UNIQUE index
	//    is the idempotency gate.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO transactions (subscription_id, amount, currency, status, stripe_event_id)
		VALUES ($1, $2, 'usd', $3, $4)
	`, s.ID, float64(amountCents)/100.0, models.TxStatusSucceeded, stripeEventID)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrDuplicateEvent
		}
		return nil, fmt.Errorf("insert transaction: %w", err)
	}

	// 3. Flip pending → active. Guarded by status='pending' so a
	//    cancelled row isn't accidentally reactivated.
	var newStatus string
	err = tx.QueryRowContext(ctx, `
		UPDATE subscriptions SET status = $1
		WHERE id = $2 AND status = $3
		RETURNING status
	`, models.StatusActive, s.ID, models.StatusPending).Scan(&newStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("update subscription: %w", err)
	}
	if err == nil {
		s.Status = newStatus
	}
	// If sql.ErrNoRows: the row exists but wasn't pending. Commit the
	// transaction row anyway; caller sees the row in whatever state it
	// was actually in via s.Status.

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &s, nil
}

// MarkPaymentFailed cancels a pending subscription when Stripe reports
// payment_intent.payment_failed. Cancel (soft), don't delete: preserves
// the row's history, keeps the partial unique index from being upset
// (cancelled rows are excluded from it, so the user can re-subscribe),
// and matches the "never hard delete" project rule.
//
// Returns ErrNotFound if there's no pending row for this PaymentIntent
// (e.g. the user already cancelled, or the payment_succeeded webhook
// beat this one). That's not an error condition worth retrying — the
// caller should ack 200 to Stripe.
func (r *postgresRepo) MarkPaymentFailed(ctx context.Context, paymentIntentID string) (*models.Subscription, error) {
	var s models.Subscription
	err := r.db.QueryRowContext(ctx, `
		UPDATE subscriptions SET status = $1
		WHERE payment_intent_id = $2 AND status = $3
		RETURNING id, user_id, creator_id, plan, status, payment_intent_id,
		          start_date, expires_at, auto_renew, created_at
	`, models.StatusCancelled, paymentIntentID, models.StatusPending).Scan(
		&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status, &s.PaymentIntentID,
		&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mark payment failed: %w", err)
	}
	return &s, nil
}

func (r *postgresRepo) GetByID(ctx context.Context, id int) (*models.Subscription, error) {
	var s models.Subscription
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, creator_id, plan, status, payment_intent_id,
		       start_date, expires_at, auto_renew, created_at
		FROM subscriptions WHERE id = $1
	`, id).Scan(&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status, &s.PaymentIntentID,
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
		SELECT id, user_id, creator_id, plan, status, payment_intent_id,
		       start_date, expires_at, auto_renew, created_at
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
		if err := rows.Scan(&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status, &s.PaymentIntentID,
			&s.StartDate, &s.ExpiresAt, &s.AutoRenew, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *postgresRepo) Cancel(ctx context.Context, id int) (int, error) {
	// Soft delete: flip status, don't actually remove the row. Preserves
	// history for the transactions FK and for support/analytics.
	// RETURNING user_id lets the caller know whose cache to invalidate
	// without a second round trip.
	var userID int
	err := r.db.QueryRowContext(ctx, `
		UPDATE subscriptions SET status = $1
		WHERE id = $2 AND status = $3
		RETURNING user_id
	`, models.StatusCancelled, id, models.StatusActive).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound // either no such id, or it wasn't active
	}
	if err != nil {
		return 0, err
	}
	return userID, nil
}

// ExpireOverdue flips overdue active subscriptions to 'expired' and
// returns (id, user_id) for each row it changed.
//
// IDEMPOTENCY — why running this twice is safe:
//   - The WHERE clause requires status = 'active'. Once a row has been
//     flipped to 'expired' by a previous sweep, the next sweep's WHERE
//     no longer matches it — the UPDATE returns 0 rows for that id.
//   - This is the same "let the DB prevent the bad state" instinct as
//     the partial unique index in Phase 1: instead of tracking "have I
//     already processed this row?" in application code, we let the
//     predicate itself make double-processing impossible.
//   - RETURNING therefore only lists rows this specific sweep actually
//     changed, which is what downstream side effects (cache invalidation,
//     future notifications) want.
//
// CONCURRENCY — two workers running at once:
//   - Postgres takes a row-level lock during UPDATE. If worker A and
//     worker B fire simultaneously, one waits for the other, then sees
//     status is no longer 'active' and matches zero rows. So DB-level
//     "exactly once" is preserved even without SELECT ... FOR UPDATE
//     SKIP LOCKED.
//   - SKIP LOCKED would be needed if we wanted N workers to PARTITION
//     the work (each grabbing a distinct batch to parallelize a huge
//     backlog). We don't — one worker sweeping the whole table on a
//     timer is fine at this scale. Documented as a Phase 3 gap; the
//     minimal fix if we ever needed it would be to SELECT ... FOR UPDATE
//     SKIP LOCKED LIMIT N, then UPDATE by id.
func (r *postgresRepo) ExpireOverdue(ctx context.Context) ([]ExpiredSub, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE subscriptions
		SET status = $1
		WHERE status = $2 AND expires_at < NOW()
		RETURNING id, user_id, creator_id
	`, models.StatusExpired, models.StatusActive)
	if err != nil {
		return nil, fmt.Errorf("expire overdue: %w", err)
	}
	defer rows.Close()

	var out []ExpiredSub
	for rows.Next() {
		var e ExpiredSub
		if err := rows.Scan(&e.ID, &e.UserID, &e.CreatorID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *postgresRepo) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	// Add 30 days to the EXISTING expires_at, not to now() — otherwise
	// renewing a day early would cost you a day.
	var s models.Subscription
	err := r.db.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET expires_at = expires_at + interval '30 days'
		WHERE id = $1 AND status = $2
		RETURNING id, user_id, creator_id, plan, status, payment_intent_id,
		          start_date, expires_at, auto_renew, created_at
	`, id, models.StatusActive).Scan(
		&s.ID, &s.UserID, &s.CreatorID, &s.Plan, &s.Status, &s.PaymentIntentID,
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
