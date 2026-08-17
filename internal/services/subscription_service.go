// Package services holds business logic: validation, orchestration, defaults,
// AND cache coordination.
//
// Why cache logic lives here and NOT in the repository:
//   - The repository's job is "talk to Postgres." Making it also talk to
//     Redis would break single-responsibility and, more concretely, would
//     hide a network call from callers.
//   - Cache invalidation is a business decision ("after a write, users
//     should see fresh data within X"), not a storage decision. Different
//     endpoints might want different invalidation strategies against the
//     same repository methods.
//   - Tests: services already accept the repository via interface. Adding
//     the Redis client the same way (interface-shaped via *redis.Client
//     methods we use) keeps the layer boundaries clean.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"subscription-service/internal/cache"
	"subscription-service/internal/metrics"
	"subscription-service/internal/models"
	"subscription-service/internal/notifications"
	"subscription-service/internal/payments"
	"subscription-service/internal/repository"
)

var (
	ErrDuplicateActive = repository.ErrDuplicateActive
	ErrNotFound        = repository.ErrNotFound
	// ErrDuplicateEvent surfaces the repo-level idempotency sentinel to
	// the webhook handler under the service package's error namespace, so
	// handlers only ever import services (not repository) for error checks.
	ErrDuplicateEvent = repository.ErrDuplicateEvent
	ErrInvalidInput   = errors.New("invalid input")
	// ErrForbidden = "the subscription exists, but this authenticated user
	// doesn't own it." Deliberately distinct from ErrNotFound so handlers
	// can return 403 vs 404 correctly. See README for the info-leak tradeoff.
	ErrForbidden = errors.New("forbidden: you do not own this subscription")
)

var planPrices = map[string]float64{
	"monthly": 4.99,
	"annual":  49.99,
}

const (
	// TTL is a *backstop*, not the primary invalidation mechanism. Active
	// DEL on writes is what keeps the cache fresh; the TTL exists so that
	// if a DEL is ever missed (bug, Redis outage, missed code path), the
	// stale entry cannot outlive it by more than this window.
	listCacheTTL = 60 * time.Second
)

type SubscriptionService struct {
	repo      repository.SubscriptionRepository
	rdb       *redis.Client
	publisher notifications.Publisher
	// payments is the Stripe wrapper (interface, not the SDK) — see
	// internal/payments. We hold the interface, not a concrete client, so
	// tests can inject a fake without a Stripe API key or network access.
	payments payments.Client
}

func NewSubscriptionService(
	repo repository.SubscriptionRepository,
	rdb *redis.Client,
	publisher notifications.Publisher,
	pay payments.Client,
) *SubscriptionService {
	return &SubscriptionService{repo: repo, rdb: rdb, publisher: publisher, payments: pay}
}

// SubscribeInput is the same shape as the old CreateInput — renamed
// because "Create" now sounds like "already-active"; Subscribe reflects
// the Phase-7 flow where the row starts pending and only becomes active
// once Stripe confirms.
type SubscribeInput struct {
	UserID    int
	CreatorID int
	Plan      string
}

// Cache key format lives in internal/cache so the worker (which also
// invalidates) uses the exact same string. See cache.UserListKey.

// -------- writes: mutate DB, then invalidate the affected user's cache --------

// Subscribe kicks off a Phase-7 pending subscription.
//
// Flow:
//  1. Ask Stripe to create a PaymentIntent for the plan price.
//  2. Insert a subscription row in 'pending' state, tagged with the
//     PaymentIntent id.
//  3. Return (sub, client_secret, nil) — the client uses client_secret
//     with Stripe.js to actually confirm the payment.
//
// We do NOT publish EventSubscribed here. That event means "a user
// actively subscribed and paid," which we cannot claim until the
// payment_intent.succeeded webhook confirms the charge. See
// MarkPaymentSucceeded — that's the only place EventSubscribed is
// published in Phase 7.
//
// ORDERING CHOICE — Stripe first, then DB:
// I create the PaymentIntent BEFORE the DB row. Rationale:
//   - If the DB write fails, we leak an orphan PaymentIntent in Stripe.
//     Stripe auto-expires abandoned PIs after ~24h; the failure mode is
//     bounded and self-cleaning.
//   - The reverse order (DB first, PI second) leaves an orphan pending
//     row that the partial unique index would then block re-subscribe on
//     until we cleaned it up manually. Worse UX for a worse failure.
//   - Also: a single-write DB path is easier to reason about than
//     "insert + then update payment_intent_id if PI succeeds."
func (s *SubscriptionService) Subscribe(ctx context.Context, in SubscribeInput) (*models.Subscription, string, error) {
	if in.UserID <= 0 || in.CreatorID <= 0 {
		return nil, "", fmt.Errorf("%w: user_id and creator_id must be positive", ErrInvalidInput)
	}
	price, ok := planPrices[in.Plan]
	if !ok {
		return nil, "", fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, in.Plan)
	}

	// Convert dollars → cents ONCE at the boundary. Stripe's API and our
	// payments interface both take int64 cents; the internal price table
	// stays in dollars for readability.
	amountCents := int64(price * 100)

	pi, err := s.payments.CreatePaymentIntent(ctx, amountCents, "usd", map[string]string{
		// Purely informational — surfaces in the Stripe dashboard so a
		// human debugging a stuck payment can trace it back to our
		// domain. NOT used as a lookup key; the webhook path looks up by
		// payment_intent_id, which is authoritative.
		"user_id":    fmt.Sprintf("%d", in.UserID),
		"creator_id": fmt.Sprintf("%d", in.CreatorID),
		"plan":       in.Plan,
	})
	if err != nil {
		return nil, "", fmt.Errorf("create payment intent: %w", err)
	}

	// Attach the PI id to the sub before insert. lib/pq turns a non-nil
	// *string into a real value, nil into SQL NULL — see models.Subscription.
	sub := &models.Subscription{
		UserID:          in.UserID,
		CreatorID:       in.CreatorID,
		Plan:            in.Plan,
		AutoRenew:       true,
		PaymentIntentID: &pi.ID,
	}
	created, err := s.repo.CreatePending(ctx, sub)
	if err != nil {
		// DB write failed. The PaymentIntent (pi.ID) will linger in
		// Stripe and eventually auto-expire; we log for observability but
		// don't try to cancel it here — a Stripe cancel is itself a
		// network call that could fail, and the auto-expiry is the more
		// reliable cleanup path.
		log.Printf("Subscribe: DB CreatePending failed after Stripe PI %s created: %v", pi.ID, err)
		return nil, "", err
	}
	// Metrics: a pending sub was successfully created. Doesn't fire on
	// ErrDuplicateActive (409) — that's an intentionally-rejected retry,
	// not a business event.
	metrics.SubscriptionsCreatedTotal.Inc()
	// Invalidate cache — the new pending row belongs in this user's
	// list. (ListByUser returns all statuses.)
	s.invalidateUserList(ctx, created.UserID)

	return created, pi.ClientSecret, nil
}

// MarkPaymentSucceeded is called from the Stripe webhook handler ONLY.
// Never call this from any user-facing code path — Stripe events are
// the sole authority for "money moved," and gating on that in this
// service (rather than a handler) keeps the invariant enforced by
// composition, not convention.
//
// This method owns the post-repo side effects: cache invalidation, then
// publishing EventSubscribed. Both happen only when the repo call
// actually reports a state change; ErrDuplicateEvent (repeat webhook
// delivery) short-circuits before the publish so a creator isn't
// notified twice for the same subscribe.
func (s *SubscriptionService) MarkPaymentSucceeded(ctx context.Context, paymentIntentID, stripeEventID string, amountCents int64) (*models.Subscription, error) {
	sub, err := s.repo.MarkPaymentSucceeded(ctx, paymentIntentID, stripeEventID, amountCents)
	if err != nil {
		// ErrDuplicateEvent / ErrNotFound both flow through unchanged;
		// the webhook handler decides what to do with them (both should
		// end up as 2xx to Stripe so the retry loop stops).
		return nil, err
	}
	s.invalidateUserList(ctx, sub.UserID)
	// Publish AFTER the commit AND after cache invalidation. The event
	// only fires when we successfully flipped pending → active on a
	// fresh event, so creators are notified exactly once per successful
	// subscribe (idempotency at the notification layer comes for free
	// from the DB-level idempotency in MarkPaymentSucceeded).
	s.publisher.Publish(notifications.Event{
		SubscriptionID: sub.ID,
		UserID:         sub.UserID,
		CreatorID:      sub.CreatorID,
		Type:           notifications.EventSubscribed,
	})
	return sub, nil
}

// MarkPaymentFailed is also webhook-only. Cancels the pending row so
// the user can retry Subscribe (the partial unique index excludes
// cancelled rows). No notification is published — nothing happened from
// the creator's point of view; a failed payment attempt isn't a
// business event they care about.
func (s *SubscriptionService) MarkPaymentFailed(ctx context.Context, paymentIntentID string) (*models.Subscription, error) {
	sub, err := s.repo.MarkPaymentFailed(ctx, paymentIntentID)
	if err != nil {
		return nil, err
	}
	// Same counter as user-initiated cancel — from a "what state moved"
	// perspective, this is a subscription moving to cancelled. If ops
	// ever needs to split them out, add a "reason" label per the note
	// on SubscriptionsCancelledTotal.
	metrics.SubscriptionsCancelledTotal.Inc()
	s.invalidateUserList(ctx, sub.UserID)
	return sub, nil
}

func (s *SubscriptionService) Get(ctx context.Context, id int) (*models.Subscription, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *SubscriptionService) Cancel(ctx context.Context, id int) error {
	userID, err := s.repo.Cancel(ctx, id)
	if err != nil {
		return err
	}
	metrics.SubscriptionsCancelledTotal.Inc()
	s.invalidateUserList(ctx, userID)
	return nil
}

// CancelForUser is the authenticated variant. Verifies the subscription
// belongs to authUserID before cancelling.
//
// Two round trips (GetByID → Cancel) rather than one WHERE-clause check,
// on purpose: we need to distinguish "not found" (404) from "not yours"
// (403). A single UPDATE ... WHERE id=$1 AND user_id=$2 can't tell them
// apart — it just returns 0 rows in both cases.
func (s *SubscriptionService) CancelForUser(ctx context.Context, id, authUserID int) error {
	sub, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err // ErrNotFound flows through
	}
	if sub.UserID != authUserID {
		return ErrForbidden
	}
	return s.Cancel(ctx, id)
}

func (s *SubscriptionService) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	sub, err := s.repo.Renew(ctx, id)
	if err != nil {
		return nil, err
	}
	s.invalidateUserList(ctx, sub.UserID)
	return sub, nil
}

func (s *SubscriptionService) RenewForUser(ctx context.Context, id, authUserID int) (*models.Subscription, error) {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.UserID != authUserID {
		return nil, ErrForbidden
	}
	return s.Renew(ctx, id)
}

// GetForUser enforces ownership on a single-subscription fetch.
func (s *SubscriptionService) GetForUser(ctx context.Context, id, authUserID int) (*models.Subscription, error) {
	sub, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.UserID != authUserID {
		return nil, ErrForbidden
	}
	return sub, nil
}

// -------- reads: cache-aside on the list endpoint --------

func (s *SubscriptionService) ListByUser(ctx context.Context, userID int) ([]models.Subscription, error) {
	key := cache.UserListKey(userID)

	// 1. Try Redis.
	if cached, err := s.rdb.Get(ctx, key).Bytes(); err == nil {
		// Cache HIT. Unmarshal and return without touching Postgres.
		var subs []models.Subscription
		jerr := json.Unmarshal(cached, &subs)
		if jerr == nil {
			log.Printf("cache HIT %s", key)
			metrics.CacheHitsTotal.Inc()
			return subs, nil
		}
		// Malformed cache entry — fall through to DB and overwrite it.
		// Counts as a miss for hit-rate accounting since we're going
		// to hit Postgres anyway.
		log.Printf("cache CORRUPT %s: %v (falling back to DB)", key, jerr)
		metrics.CacheMissesTotal.Inc()
	} else if !errors.Is(err, redis.Nil) {
		// Any Redis error other than "key missing" is logged but non-fatal:
		// the DB is still the source of truth, so we degrade to serving
		// from Postgres. Redis being down should slow the site, not break it.
		// Also counts as a miss — we're hitting Postgres regardless of why.
		log.Printf("cache GET %s failed: %v (falling back to DB)", key, err)
		metrics.CacheMissesTotal.Inc()
	} else {
		log.Printf("cache MISS %s", key)
		metrics.CacheMissesTotal.Inc()
	}

	// 2. Cache miss (or Redis error) — fetch from Postgres.
	subs, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 3. Populate the cache. If this fails we still return the DB result;
	// staleness is bounded to the next successful set + TTL.
	if payload, jerr := json.Marshal(subs); jerr == nil {
		if serr := s.rdb.Set(ctx, key, payload, listCacheTTL).Err(); serr != nil {
			log.Printf("cache SET %s failed: %v", key, serr)
		}
	}
	return subs, nil
}

// ExpireOverdue runs the DB sweep and invalidates one Redis key per
// affected user. Called by the background worker on a ticker.
//
// The service (not the worker, not the repository) owns the invalidation
// step for the same reason as Phase 2: cache policy is a business decision
// and belongs next to the code that already knows about the cache. The
// worker stays a thin scheduler.
func (s *SubscriptionService) ExpireOverdue(ctx context.Context) ([]repository.ExpiredSub, error) {
	expired, err := s.repo.ExpireOverdue(ctx)
	if err != nil {
		return nil, err
	}
	// Metrics: one increment per row that actually flipped. Add once
	// with the batch size instead of Inc()ing in the loop so a big
	// sweep is a single atomic add on the counter rather than N.
	if n := len(expired); n > 0 {
		metrics.SubscriptionsExpiredTotal.Add(float64(n))
	}
	// A user with multiple expiring subs shouldn't get multiple DELs of
	// the same key. Dedupe user_ids first.
	seen := make(map[int]struct{}, len(expired))
	for _, e := range expired {
		// One "expired" event per subscription (not deduped by user —
		// each expired subscription is its own signal to its own creator).
		s.publisher.Publish(notifications.Event{
			SubscriptionID: e.ID,
			UserID:         e.UserID,
			CreatorID:      e.CreatorID,
			Type:           notifications.EventExpired,
		})
		if _, ok := seen[e.UserID]; ok {
			continue
		}
		seen[e.UserID] = struct{}{}
		s.invalidateUserList(ctx, e.UserID)
	}
	return expired, nil
}

// invalidateUserList removes the cached list after a successful write.
//
// FAILURE MODE (KNOWN GAP, DOCUMENTED):
// If the DB write commits but this DEL fails (Redis down, network blip,
// process killed between the two calls), a stale list can be served for
// up to `listCacheTTL` (60s). Consequences:
//   - After Cancel: user sees the cancelled sub as "active" for <=60s.
//   - After Subscribe: user doesn't see the new sub in the list for <=60s
//     (though GET /subscriptions/:id would show it immediately since that
//     endpoint isn't cached).
//   - After Renew: user sees the old expires_at for <=60s.
//
// Mitigations in place:
//   - Short TTL bounds worst-case staleness to 60s.
//   - We log failures so this shows up in monitoring rather than silently
//     rotting.
//
// Mitigations NOT in place (deliberate — Phase 2 gap, revisit later):
//   - No retry queue. Production systems put failed invalidations onto a
//     durable queue and retry until they succeed. That's Phase 4 territory
//     once the notification queue exists — this problem shape is identical.
//   - No two-phase commit / outbox pattern. Overkill until we hit a real
//     consistency SLA that 60s violates.
//
// We do NOT return the DEL error to the client. The client's write
// genuinely succeeded; failing the HTTP response would mislead them into
// retrying an already-committed operation.
func (s *SubscriptionService) invalidateUserList(ctx context.Context, userID int) {
	key := cache.UserListKey(userID)
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		log.Printf("cache DEL %s failed: %v (user may see stale list for up to %s)", key, err, listCacheTTL)
	}
}
