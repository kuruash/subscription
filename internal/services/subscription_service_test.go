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
	CreateFn        func(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error)
	GetByIDFn       func(ctx context.Context, id int) (*models.Subscription, error)
	ListByUserFn    func(ctx context.Context, userID int) ([]models.Subscription, error)
	CancelFn        func(ctx context.Context, id int) (int, error)
	RenewFn         func(ctx context.Context, id int) (*models.Subscription, error)
	ExpireOverdueFn func(ctx context.Context) ([]repository.ExpiredSub, error)
}

func (f *fakeRepo) Create(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error) {
	if f.CreateFn == nil {
		return nil, errors.New("fakeRepo.Create: unexpected call")
	}
	return f.CreateFn(ctx, sub, amount)
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

func newDelSpy() *delSpy { return &delSpy{counts: map[string]int{}} }
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
	svc := services.NewSubscriptionService(repo, rdb, pub)

	return &harness{svc: svc, repo: repo, pub: pub, dels: spy}
}

// -----------------------------------------------------------------------------
// Create
// -----------------------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	h := newHarness(t)
	h.repo.CreateFn = func(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error) {
		sub.ID = 42
		return sub, nil
	}

	got, err := h.svc.Create(context.Background(), services.CreateInput{
		UserID: 7, CreatorID: 3, Plan: "monthly",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if got.ID != 42 || got.UserID != 7 || got.CreatorID != 3 {
		t.Errorf("unexpected sub: %+v", got)
	}
	evs := h.pub.snapshot()
	if len(evs) != 1 || evs[0].Type != notifications.EventSubscribed || evs[0].SubscriptionID != 42 {
		t.Errorf("expected one subscribed event for sub 42, got %+v", evs)
	}
}

func TestCreate_DuplicateActive(t *testing.T) {
	h := newHarness(t)
	h.repo.CreateFn = func(ctx context.Context, sub *models.Subscription, amount float64) (*models.Subscription, error) {
		return nil, repository.ErrDuplicateActive
	}
	_, err := h.svc.Create(context.Background(), services.CreateInput{
		UserID: 1, CreatorID: 1, Plan: "monthly",
	})
	if !errors.Is(err, services.ErrDuplicateActive) {
		t.Fatalf("want ErrDuplicateActive, got %v", err)
	}
	if len(h.pub.snapshot()) != 0 {
		t.Errorf("no event should be published when Create fails")
	}
}

// -----------------------------------------------------------------------------
// Ownership: table-driven for CancelForUser / RenewForUser / GetForUser
// -----------------------------------------------------------------------------

// ownershipCase covers the three cases each *ForUser method must handle
// identically: owner (ok), not-found (404), forbidden (403).
type ownershipCase struct {
	name       string
	getResult  *models.Subscription
	getErr     error
	wantErrIs  error // nil means expect no error
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
