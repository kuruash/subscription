// Unit tests for the service layer.
//
// We use the EXTERNAL test package (services_test) on purpose so tests
// can only touch what a real caller would touch — the exported API. If
// a test wants access to an unexported helper, that's a signal the
// helper probably shouldn't be unexported, or the test is testing the
// wrong thing.
//
// No real Postgres and no real Redis:
//   - The repository is a hand-written fake struct that satisfies the
//     SubscriptionRepository interface. This is the payoff of doing
//     interfaces from Phase 1 onwards — the service was designed to be
//     mockable without any test framework.
//   - Redis is miniredis (an in-process fake). The service takes a real
//     *redis.Client, so we point it at miniredis's address. To observe
//     which DEL commands the service issued (needed for the dedupe
//     assertion), we attach a go-redis Hook that counts commands.
package services_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"subscription-service/internal/cache"
	"subscription-service/internal/models"
	"subscription-service/internal/notifications"
	"subscription-service/internal/payments"
	"subscription-service/internal/repository"
	"subscription-service/internal/services"
)

// -----------------------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------------------

// fakeRepo satisfies repository.SubscriptionRepository via per-method
// function overrides. Each test wires only the methods it cares about;
// any un-set method returns a zero value or an "unexpected call" error
// so a misconfigured test fails loudly instead of silently.
type fakeRepo struct {
	CreatePendingFn        func(ctx context.Context, sub *models.Subscription) (*models.Subscription, error)
	MarkPaymentSucceededFn func(ctx context.Context, paymentIntentID, stripeEventID string, amountCents int64) (*models.Subscription, error)
	MarkPaymentFailedFn    func(ctx context.Context, paymentIntentID string) (*models.Subscription, error)
	GetByIDFn              func(ctx context.Context, id int) (*models.Subscription, error)
	ListByUserFn           func(ctx context.Context, userID int) ([]models.Subscription, error)
	CancelFn               func(ctx context.Context, id int) (int, error)
	RenewFn                func(ctx context.Context, id int) (*models.Subscription, error)
	ExpireOverdueFn        func(ctx context.Context) ([]repository.ExpiredSub, error)
	// Phase 11 additions — not exercised by any test yet, so unset
	// fields will panic with "unexpected call" if a test accidentally
	// hits them.
	ListAllFn func(ctx context.Context, limit, offset int) ([]models.Subscription, error)
	StatsFn   func(ctx context.Context) (repository.AdminStats, error)
}

func (f *fakeRepo) CreatePending(ctx context.Context, sub *models.Subscription) (*models.Subscription, error) {
	if f.CreatePendingFn == nil {
		return nil, errors.New("fakeRepo.CreatePending: unexpected call")
	}
	return f.CreatePendingFn(ctx, sub)
}
func (f *fakeRepo) MarkPaymentSucceeded(ctx context.Context, paymentIntentID, stripeEventID string, amountCents int64) (*models.Subscription, error) {
	if f.MarkPaymentSucceededFn == nil {
		return nil, errors.New("fakeRepo.MarkPaymentSucceeded: unexpected call")
	}
	return f.MarkPaymentSucceededFn(ctx, paymentIntentID, stripeEventID, amountCents)
}
func (f *fakeRepo) MarkPaymentFailed(ctx context.Context, paymentIntentID string) (*models.Subscription, error) {
	if f.MarkPaymentFailedFn == nil {
		return nil, errors.New("fakeRepo.MarkPaymentFailed: unexpected call")
	}
	return f.MarkPaymentFailedFn(ctx, paymentIntentID)
}
func (f *fakeRepo) GetByID(ctx context.Context, id int) (*models.Subscription, error) {
	if f.GetByIDFn == nil {
		return nil, errors.New("fakeRepo.GetByID: unexpected call")
	}
	return f.GetByIDFn(ctx, id)
}
func (f *fakeRepo) ListByUser(ctx context.Context, userID int) ([]models.Subscription, error) {
	if f.ListByUserFn == nil {
		return nil, errors.New("fakeRepo.ListByUser: unexpected call")
	}
	return f.ListByUserFn(ctx, userID)
}
func (f *fakeRepo) Cancel(ctx context.Context, id int) (int, error) {
	if f.CancelFn == nil {
		return 0, errors.New("fakeRepo.Cancel: unexpected call")
	}
	return f.CancelFn(ctx, id)
}
func (f *fakeRepo) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	if f.RenewFn == nil {
		return nil, errors.New("fakeRepo.Renew: unexpected call")
	}
	return f.RenewFn(ctx, id)
}
func (f *fakeRepo) ExpireOverdue(ctx context.Context) ([]repository.ExpiredSub, error) {
	if f.ExpireOverdueFn == nil {
		return nil, errors.New("fakeRepo.ExpireOverdue: unexpected call")
	}
	return f.ExpireOverdueFn(ctx)
}
func (f *fakeRepo) ListAll(ctx context.Context, limit, offset int) ([]models.Subscription, error) {
	if f.ListAllFn == nil {
		return nil, errors.New("fakeRepo.ListAll: unexpected call")
	}
	return f.ListAllFn(ctx, limit, offset)
}
func (f *fakeRepo) Stats(ctx context.Context) (repository.AdminStats, error) {
	if f.StatsFn == nil {
		return repository.AdminStats{}, errors.New("fakeRepo.Stats: unexpected call")
	}
	return f.StatsFn(ctx)
}

// fakePayments satisfies payments.Client via per-method function
// overrides — same pattern as fakeRepo. Tests wire only the methods
// they exercise; anything unset returns a loud "unexpected call" error
// so a misconfigured test fails immediately with a clear signal instead
// of returning a plausible-looking zero value.
type fakePayments struct {
	CreatePaymentIntentFn    func(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*payments.PaymentIntent, error)
	VerifyWebhookSignatureFn func(payload []byte, sigHeader string) (*payments.Event, error)
	// CreatePaymentIntentCalls counts invocations — needed to prove
	// negative-space claims (e.g. "CreatePending was NOT called" is
	// asserted via the *repo* fake's call log, but "CreatePaymentIntent
	// was called exactly once" is asserted here).
	CreatePaymentIntentCalls int
}

func (p *fakePayments) CreatePaymentIntent(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*payments.PaymentIntent, error) {
	p.CreatePaymentIntentCalls++
	if p.CreatePaymentIntentFn == nil {
		return nil, errors.New("fakePayments.CreatePaymentIntent: unexpected call")
	}
	return p.CreatePaymentIntentFn(ctx, amountCents, currency, metadata)
}
func (p *fakePayments) VerifyWebhookSignature(payload []byte, sigHeader string) (*payments.Event, error) {
	if p.VerifyWebhookSignatureFn == nil {
		return nil, errors.New("fakePayments.VerifyWebhookSignature: unexpected call")
	}
	return p.VerifyWebhookSignatureFn(payload, sigHeader)
}

// fakePublisher records everything the service publishes so tests can
// assert both count and content.
type fakePublisher struct {
	mu     sync.Mutex
	events []notifications.Event
}

func (p *fakePublisher) Publish(ev notifications.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}
func (p *fakePublisher) snapshot() []notifications.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]notifications.Event, len(p.events))
	copy(out, p.events)
	return out
}

// delSpy is a go-redis Hook that counts DEL invocations per key. This is
// how we prove the "one DEL per unique user" dedupe in ExpireOverdue —
// counting on Redis alone can't tell "DEL called twice on the same
// already-missing key" from "DEL called once."
type delSpy struct {
	mu     sync.Mutex
	counts map[string]int
}

func newDelSpy() *delSpy                                      { return &delSpy{counts: map[string]int{}} }
func (s *delSpy) DialHook(next redis.DialHook) redis.DialHook { return next }
func (s *delSpy) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "del") {
			s.mu.Lock()
			for _, a := range cmd.Args()[1:] {
				if k, ok := a.(string); ok {
					s.counts[k]++
				}
			}
			s.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}
func (s *delSpy) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (s *delSpy) countFor(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key]
}

// -----------------------------------------------------------------------------
// Test harness
// -----------------------------------------------------------------------------

type harness struct {
	svc  *services.SubscriptionService
	repo *fakeRepo
	pub  *fakePublisher
	pay  *fakePayments
	dels *delSpy
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mr := miniredis.RunT(t) // auto-closed when t completes
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	spy := newDelSpy()
	rdb.AddHook(spy)

	repo := &fakeRepo{}
	pub := &fakePublisher{}
	pay := &fakePayments{}
	svc := services.NewSubscriptionService(repo, rdb, pub, pay)

	return &harness{svc: svc, repo: repo, pub: pub, pay: pay, dels: spy}
}

// -----------------------------------------------------------------------------
// Subscribe (Phase 7)
// -----------------------------------------------------------------------------

func TestSubscribe_Success(t *testing.T) {
	h := newHarness(t)

	// Track CreatePending calls so we can assert on the sub the service
	// hands to the repo (status must be pending, PI id must be attached).
	var createPendingCalls int
	var handedToRepo *models.Subscription
	h.pay.CreatePaymentIntentFn = func(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*payments.PaymentIntent, error) {
		if amountCents != 499 {
			t.Errorf("want amountCents=499 (monthly plan), got %d", amountCents)
		}
		if currency != "usd" {
			t.Errorf("want currency=usd, got %s", currency)
		}
		return &payments.PaymentIntent{ID: "pi_test_abc", ClientSecret: "pi_test_abc_secret_xyz"}, nil
	}
	h.repo.CreatePendingFn = func(ctx context.Context, sub *models.Subscription) (*models.Subscription, error) {
		createPendingCalls++
		handedToRepo = sub
		sub.ID = 42
		return sub, nil
	}

	got, clientSecret, err := h.svc.Subscribe(context.Background(), services.SubscribeInput{
		UserID: 7, CreatorID: 3, Plan: "monthly",
	})
	if err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	if got.ID != 42 || got.UserID != 7 || got.CreatorID != 3 {
		t.Errorf("unexpected sub: %+v", got)
	}
	if clientSecret != "pi_test_abc_secret_xyz" {
		t.Errorf("client_secret not propagated: got %q", clientSecret)
	}
	if createPendingCalls != 1 {
		t.Errorf("want 1 CreatePending call, got %d", createPendingCalls)
	}
	if handedToRepo.PaymentIntentID == nil || *handedToRepo.PaymentIntentID != "pi_test_abc" {
		t.Errorf("payment_intent_id not attached to sub before CreatePending: %+v", handedToRepo.PaymentIntentID)
	}
	// Subscribe must NOT publish EventSubscribed — that's the webhook's job.
	if evs := h.pub.snapshot(); len(evs) != 0 {
		t.Errorf("Subscribe must not publish; got %+v", evs)
	}
	// Cache invalidation should have fired for user 7.
	if got7 := h.dels.countFor(cache.UserListKey(7)); got7 != 1 {
		t.Errorf("expected 1 DEL for user 7, got %d", got7)
	}
}

// PaymentIntent failure must short-circuit BEFORE CreatePending is
// called. This is the Stripe-first ordering invariant: if we ever
// regress to DB-first, this test fails because CreatePending would
// get called and the "unexpected call" guard trips.
func TestSubscribe_PaymentIntentFailure_DoesNotTouchRepo(t *testing.T) {
	h := newHarness(t)
	h.pay.CreatePaymentIntentFn = func(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*payments.PaymentIntent, error) {
		return nil, errors.New("stripe unreachable")
	}
	// Deliberately leave h.repo.CreatePendingFn = nil. If Subscribe calls
	// it, the fake returns "unexpected call" and the test surfaces that.

	_, _, err := h.svc.Subscribe(context.Background(), services.SubscribeInput{
		UserID: 7, CreatorID: 3, Plan: "monthly",
	})
	if err == nil {
		t.Fatal("expected error when payments client fails")
	}
	if !strings.Contains(err.Error(), "stripe unreachable") {
		t.Errorf("original error should propagate; got %v", err)
	}
	if h.pay.CreatePaymentIntentCalls != 1 {
		t.Errorf("want CreatePaymentIntent called exactly once, got %d", h.pay.CreatePaymentIntentCalls)
	}
	if evs := h.pub.snapshot(); len(evs) != 0 {
		t.Errorf("no event should be published on payment failure; got %+v", evs)
	}
}

// If Stripe succeeded but the DB write failed, the caller must see the
// real error — don't silently swallow it because the PI happens to exist.
func TestSubscribe_CreatePendingFailure_SurfacesError(t *testing.T) {
	h := newHarness(t)
	h.pay.CreatePaymentIntentFn = func(ctx context.Context, amountCents int64, currency string, metadata map[string]string) (*payments.PaymentIntent, error) {
		return &payments.PaymentIntent{ID: "pi_test_dupe", ClientSecret: "pi_test_dupe_secret"}, nil
	}
	h.repo.CreatePendingFn = func(ctx context.Context, sub *models.Subscription) (*models.Subscription, error) {
		return nil, repository.ErrDuplicateActive
	}

	_, clientSecret, err := h.svc.Subscribe(context.Background(), services.SubscribeInput{
		UserID: 1, CreatorID: 1, Plan: "monthly",
	})
	if !errors.Is(err, services.ErrDuplicateActive) {
		t.Fatalf("want ErrDuplicateActive, got %v", err)
	}
	if clientSecret != "" {
		t.Errorf("no client_secret should be returned on DB failure; got %q", clientSecret)
	}
	if evs := h.pub.snapshot(); len(evs) != 0 {
		t.Errorf("no event should publish when Subscribe fails; got %+v", evs)
	}
}

// -----------------------------------------------------------------------------
// MarkPaymentSucceeded (Phase 7 webhook-only path)
// -----------------------------------------------------------------------------

// First delivery of an event flips the sub to active AND publishes
// EventSubscribed exactly once. This is where the "creator gets notified
// on subscribe" contract actually lives in Phase 7 — Subscribe itself
// no longer publishes.
func TestMarkPaymentSucceeded_PublishesOnceOnFirstDelivery(t *testing.T) {
	h := newHarness(t)
	h.repo.MarkPaymentSucceededFn = func(ctx context.Context, piID, evtID string, amountCents int64) (*models.Subscription, error) {
		return &models.Subscription{ID: 42, UserID: 7, CreatorID: 3, Status: models.StatusActive}, nil
	}

	sub, err := h.svc.MarkPaymentSucceeded(context.Background(), "pi_test_abc", "evt_first", 499)
	if err != nil {
		t.Fatalf("MarkPaymentSucceeded: %v", err)
	}
	if sub.Status != models.StatusActive {
		t.Errorf("want active, got %s", sub.Status)
	}
	evs := h.pub.snapshot()
	if len(evs) != 1 || evs[0].Type != notifications.EventSubscribed || evs[0].SubscriptionID != 42 {
		t.Errorf("want one EventSubscribed for sub 42, got %+v", evs)
	}
	if got7 := h.dels.countFor(cache.UserListKey(7)); got7 != 1 {
		t.Errorf("expected 1 DEL for user 7, got %d", got7)
	}
}

// Idempotency: a redelivered event returns ErrDuplicateEvent and does
// NOT publish a second notification. This is the CORE Phase 7 guarantee
// — the repo layer enforces it via the stripe_event_id UNIQUE index;
// this test proves the service respects it (short-circuits before the
// publish) instead of double-notifying.
func TestMarkPaymentSucceeded_DuplicateEventDoesNotDoublePublish(t *testing.T) {
	h := newHarness(t)

	var repoCalls int
	h.repo.MarkPaymentSucceededFn = func(ctx context.Context, piID, evtID string, amountCents int64) (*models.Subscription, error) {
		repoCalls++
		if repoCalls == 1 {
			return &models.Subscription{ID: 42, UserID: 7, CreatorID: 3, Status: models.StatusActive}, nil
		}
		// Second delivery simulates the partial UNIQUE index rejecting
		// the duplicate transaction insert.
		return nil, repository.ErrDuplicateEvent
	}

	// First delivery.
	if _, err := h.svc.MarkPaymentSucceeded(context.Background(), "pi_test_abc", "evt_same", 499); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Second delivery of the same event id.
	_, err := h.svc.MarkPaymentSucceeded(context.Background(), "pi_test_abc", "evt_same", 499)
	if !errors.Is(err, services.ErrDuplicateEvent) {
		t.Fatalf("second delivery: want ErrDuplicateEvent, got %v", err)
	}

	// Exactly one publish — even though MarkPaymentSucceeded was called
	// twice. This is the whole point.
	evs := h.pub.snapshot()
	if len(evs) != 1 {
		t.Errorf("want exactly 1 EventSubscribed across both deliveries, got %d: %+v", len(evs), evs)
	}
}

// -----------------------------------------------------------------------------
// MarkPaymentFailed (webhook-only)
// -----------------------------------------------------------------------------

// payment_failed cancels the pending row and invalidates the user's
// cache but publishes NOTHING — a failed payment attempt isn't a
// business event a creator cares about.
func TestMarkPaymentFailed_InvalidatesButDoesNotPublish(t *testing.T) {
	h := newHarness(t)
	h.repo.MarkPaymentFailedFn = func(ctx context.Context, piID string) (*models.Subscription, error) {
		return &models.Subscription{ID: 42, UserID: 7, CreatorID: 3, Status: models.StatusCancelled}, nil
	}

	sub, err := h.svc.MarkPaymentFailed(context.Background(), "pi_test_abc")
	if err != nil {
		t.Fatalf("MarkPaymentFailed: %v", err)
	}
	if sub.Status != models.StatusCancelled {
		t.Errorf("want cancelled, got %s", sub.Status)
	}
	if evs := h.pub.snapshot(); len(evs) != 0 {
		t.Errorf("MarkPaymentFailed must not publish; got %+v", evs)
	}
	if got7 := h.dels.countFor(cache.UserListKey(7)); got7 != 1 {
		t.Errorf("expected 1 DEL for user 7, got %d", got7)
	}
}

// -----------------------------------------------------------------------------
// Ownership: table-driven for CancelForUser / RenewForUser / GetForUser
// -----------------------------------------------------------------------------

// ownershipCase covers the three cases each *ForUser method must handle
// identically: owner (ok), not-found (404), forbidden (403).
type ownershipCase struct {
	name      string
	getResult *models.Subscription
	getErr    error
	wantErrIs error // nil means expect no error
}

var authUser = 5

var ownershipCases = []ownershipCase{
	{name: "owner", getResult: &models.Subscription{ID: 1, UserID: authUser, CreatorID: 9}, getErr: nil, wantErrIs: nil},
	{name: "not found", getResult: nil, getErr: repository.ErrNotFound, wantErrIs: services.ErrNotFound},
	{name: "wrong user", getResult: &models.Subscription{ID: 1, UserID: 999, CreatorID: 9}, getErr: nil, wantErrIs: services.ErrForbidden},
}

func TestGetForUser(t *testing.T) {
	for _, tc := range ownershipCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.repo.GetByIDFn = func(ctx context.Context, id int) (*models.Subscription, error) {
				return tc.getResult, tc.getErr
			}
			_, err := h.svc.GetForUser(context.Background(), 1, authUser)
			if tc.wantErrIs == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("want error %v, got %v", tc.wantErrIs, err)
			}
		})
	}
}

func TestCancelForUser(t *testing.T) {
	for _, tc := range ownershipCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.repo.GetByIDFn = func(ctx context.Context, id int) (*models.Subscription, error) {
				return tc.getResult, tc.getErr
			}
			// Only reached on the owner path.
			h.repo.CancelFn = func(ctx context.Context, id int) (int, error) {
				return authUser, nil
			}
			err := h.svc.CancelForUser(context.Background(), 1, authUser)
			if tc.wantErrIs == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("want error %v, got %v", tc.wantErrIs, err)
			}
		})
	}
}

func TestRenewForUser(t *testing.T) {
	for _, tc := range ownershipCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.repo.GetByIDFn = func(ctx context.Context, id int) (*models.Subscription, error) {
				return tc.getResult, tc.getErr
			}
			h.repo.RenewFn = func(ctx context.Context, id int) (*models.Subscription, error) {
				return &models.Subscription{ID: id, UserID: authUser}, nil
			}
			_, err := h.svc.RenewForUser(context.Background(), 1, authUser)
			if tc.wantErrIs == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("want error %v, got %v", tc.wantErrIs, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// ExpireOverdue: the Phase 3/4 behavior we specifically care about protecting.
//
// Given three expired subs — two for user=10, one for user=20 — the service
// must:
//   - publish ONE event PER SUBSCRIPTION (not per user), because each
//     creator wants to be notified for each expired sub
//   - issue ONE cache DEL PER UNIQUE USER (not per subscription), because
//     the cache is keyed by user, so hitting the same key twice would be
//     pointless work
//
// If someone regresses either behavior, this test fails.
// -----------------------------------------------------------------------------

func TestExpireOverdue_PublishesPerSubAndDedupesInvalidationPerUser(t *testing.T) {
	h := newHarness(t)
	expired := []repository.ExpiredSub{
		{ID: 100, UserID: 10, CreatorID: 1},
		{ID: 101, UserID: 10, CreatorID: 2}, // same user as sub 100
		{ID: 200, UserID: 20, CreatorID: 3},
	}
	h.repo.ExpireOverdueFn = func(ctx context.Context) ([]repository.ExpiredSub, error) {
		return expired, nil
	}

	got, err := h.svc.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("ExpireOverdue: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 expired returned, got %d", len(got))
	}

	// One event per subscription — not deduped by user.
	evs := h.pub.snapshot()
	if len(evs) != 3 {
		t.Fatalf("expected 3 published events (one per expired sub), got %d: %+v", len(evs), evs)
	}
	subsSeen := map[int]bool{}
	for _, e := range evs {
		if e.Type != notifications.EventExpired {
			t.Errorf("event %+v has wrong type", e)
		}
		subsSeen[e.SubscriptionID] = true
	}
	for _, want := range []int{100, 101, 200} {
		if !subsSeen[want] {
			t.Errorf("expected event for subscription %d, missing", want)
		}
	}

	// One cache DEL per unique user — deduped.
	// user 10 appears twice in `expired`; must still be invalidated only once.
	if got10 := h.dels.countFor(cache.UserListKey(10)); got10 != 1 {
		t.Errorf("expected 1 DEL for user 10, got %d", got10)
	}
	if got20 := h.dels.countFor(cache.UserListKey(20)); got20 != 1 {
		t.Errorf("expected 1 DEL for user 20, got %d", got20)
	}
}
