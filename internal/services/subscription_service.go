// Package services holds business logic: validation, orchestration, defaults.
// It knows nothing about HTTP and nothing about SQL. It talks to the
// repository through its INTERFACE.
//
// Why this matters: in a unit test you can hand SubscriptionService a fake
// repository (a struct with the same methods but hard-coded returns) and
// exercise the business rules without spinning up Postgres. That's only
// possible because the dependency is an interface, not `*postgresRepo`.
package services

import (
	"context"
	"errors"
	"fmt"

	"subscription-service/internal/models"
	"subscription-service/internal/repository"
)

// Re-export sentinel errors so handlers only import services.
var (
	ErrDuplicateActive = repository.ErrDuplicateActive
	ErrNotFound        = repository.ErrNotFound
	ErrInvalidInput    = errors.New("invalid input")
)

// Plan pricing lives here (business rule), not in the DB or handler.
var planPrices = map[string]float64{
	"monthly": 4.99,
	"annual":  49.99,
}

type SubscriptionService struct {
	repo repository.SubscriptionRepository // <-- INTERFACE, not concrete type
}

func NewSubscriptionService(repo repository.SubscriptionRepository) *SubscriptionService {
	return &SubscriptionService{repo: repo}
}

// CreateInput is a small DTO for what the service needs. Keeping it separate
// from the model means the API contract can evolve without changing DB rows.
type CreateInput struct {
	UserID    int
	CreatorID int
	Plan      string
}

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
	return s.repo.Create(ctx, sub, price)
}

func (s *SubscriptionService) Get(ctx context.Context, id int) (*models.Subscription, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *SubscriptionService) ListByUser(ctx context.Context, userID int) ([]models.Subscription, error) {
	return s.repo.ListByUser(ctx, userID)
}

func (s *SubscriptionService) Cancel(ctx context.Context, id int) error {
	return s.repo.Cancel(ctx, id)
}

func (s *SubscriptionService) Renew(ctx context.Context, id int) (*models.Subscription, error) {
	return s.repo.Renew(ctx, id)
}
