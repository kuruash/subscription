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
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"subscription-service/internal/models"
	"subscription-service/internal/repository"
)

var (
	ErrDuplicateActive = repository.ErrDuplicateActive
	ErrNotFound        = repository.ErrNotFound
	ErrInvalidInput    = errors.New("invalid input")
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
	repo repository.SubscriptionRepository
	rdb  *redis.Client
}

func NewSubscriptionService(repo repository.SubscriptionRepository, rdb *redis.Client) *SubscriptionService {
	return &SubscriptionService{repo: repo, rdb: rdb}
}

type CreateInput struct {
	UserID    int
	CreatorID int
	Plan      string
}

// userListKey centralizes the cache key format so every code path — read,
// write, invalidation — agrees on the exact string. A typo in one place
// would silently split the cache.
func userListKey(userID int) string {
	return "user:" + strconv.Itoa(userID) + ":subscriptions"
}

// -------- writes: mutate DB, then invalidate the affected user's cache --------

func (s *SubscriptionService) Create(ctx context.Context, in CreateInput) (*models.Subscription, error) {
	if in.UserID <= 0 || in.CreatorID <= 0 {
		return nil, fmt.Errorf("%w: user_id and creator_id must be positive", ErrInvalidInput)
	}
	price, ok := planPrices[in.Plan]
	if !ok {
		return nil, fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, in.Plan)
	}

	sub := &models.Subscription{
		UserID:    in.UserID,
		CreatorID: in.CreatorID,
		Plan:      in.Plan,
		AutoRenew: true,
	}
	created, err := s.repo.Create(ctx, sub, price)
	if err != nil {
		return nil, err
	}
	s.invalidateUserList(ctx, created.UserID)
	return created, nil
}

func (s *SubscriptionService) Get(ctx context.Context, id int) (*models.Subscription, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *SubscriptionService) Cancel(ctx context.Context, id int) error {
	userID, err := s.repo.Cancel(ctx, id)
	if err != nil {
		return err
	}
	s.invalidateUserList(ctx, userID)
	return nil
}

func (s *SubscriptionService) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	sub, err := s.repo.Renew(ctx, id)
	if err != nil {
		return nil, err
	}
	s.invalidateUserList(ctx, sub.UserID)
	return sub, nil
}

// -------- reads: cache-aside on the list endpoint --------

func (s *SubscriptionService) ListByUser(ctx context.Context, userID int) ([]models.Subscription, error) {
	key := userListKey(userID)

	// 1. Try Redis.
	if cached, err := s.rdb.Get(ctx, key).Bytes(); err == nil {
		// Cache HIT. Unmarshal and return without touching Postgres.
		var subs []models.Subscription
		jerr := json.Unmarshal(cached, &subs)
		if jerr == nil {
			log.Printf("cache HIT %s", key)
			return subs, nil
		}
		// Malformed cache entry — fall through to DB and overwrite it.
		log.Printf("cache CORRUPT %s: %v (falling back to DB)", key, jerr)
	} else if !errors.Is(err, redis.Nil) {
		// Any Redis error other than "key missing" is logged but non-fatal:
		// the DB is still the source of truth, so we degrade to serving
		// from Postgres. Redis being down should slow the site, not break it.
		log.Printf("cache GET %s failed: %v (falling back to DB)", key, err)
	} else {
		log.Printf("cache MISS %s", key)
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
	key := userListKey(userID)
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		log.Printf("cache DEL %s failed: %v (user may see stale list for up to %s)", key, err, listCacheTTL)
	}
}
