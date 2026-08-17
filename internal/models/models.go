// Package models holds the plain data structures shared across the app.
// No DB code, no HTTP code — just the shapes of things. Any layer
// (handlers, services, repository) is allowed to import this.
//
// Go convention: the *directory name* becomes the package name. Files in
// internal/models/ all declare `package models` and share a namespace.
//
// About "internal/": this is a magic directory name in Go. Code inside
// internal/ can ONLY be imported by code in the parent module. It's the
// language's built-in way to say "these are private implementation details,
// not a public API." Handy when you eventually open-source a library.
package models

import "time"

// A `struct` is a fixed set of named fields — the closest thing Go has to a
// class, but with no methods attached here and no inheritance ever.
//
// The backtick strings after each field are STRUCT TAGS. They're metadata
// that other packages read via reflection. `json:"id"` tells encoding/json
// (and Gin, which uses it) to serialize this field as "id" in JSON instead
// of the Go-idiomatic "ID". Without tags you'd leak Go naming to your API.
//
// Field names starting with a capital letter are EXPORTED (visible outside
// the package). Lowercase = package-private. This is enforced by the
// compiler, not convention — no `public`/`private` keywords exist.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	// Role — added Phase 11. "user" or "admin"; see
	// migrations/003_add_role_and_status_changed_at.sql for the CHECK
	// constraint that keeps the value set bounded.
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Role constants — Go's typed-const stand-in for an enum. Reference
// these from middleware / handler code instead of raw "admin"
// strings so a typo is a compile error rather than a silent 403.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type Creator struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Subscription mirrors the `subscriptions` table row-for-row.
// Keeping models 1:1 with tables is the simplest thing that works; if the
// API shape ever needs to diverge from the DB shape, you introduce a
// separate DTO/response struct rather than warping this one.
type Subscription struct {
	ID        int    `json:"id"`
	UserID    int    `json:"user_id"`
	CreatorID int    `json:"creator_id"`
	Plan      string `json:"plan"`
	Status    string `json:"status"`
	// PaymentIntentID is the Stripe PaymentIntent that created this row.
	// Nullable in the DB (pre-Phase-7 rows have none), so we use
	// *string here — a plain string can't represent NULL, and treating
	// "" as NULL is the kind of foot-gun that hides real data bugs.
	PaymentIntentID *string   `json:"payment_intent_id,omitempty"`
	StartDate       time.Time `json:"start_date"`
	ExpiresAt       time.Time `json:"expires_at"`
	AutoRenew       bool      `json:"auto_renew"`
	CreatedAt       time.Time `json:"created_at"`
}

// Status constants. Go doesn't have enums; the idiomatic replacement is
// a typed set of const strings (or ints). Using these instead of raw
// "active" literals scattered through the code prevents typos the compiler
// can't catch (`"activ"` would silently mean "never matches").
//
// StatusPending is new in Phase 7: a subscription starts pending when we
// create the Stripe PaymentIntent, then flips to active only when the
// payment_intent.succeeded webhook confirms the charge.
const (
	StatusPending   = "pending"
	StatusActive    = "active"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

type Transaction struct {
	ID             int     `json:"id"`
	SubscriptionID int     `json:"subscription_id"`
	Amount         float64 `json:"amount"`
	Currency       string  `json:"currency"`
	Status         string  `json:"status"`
	// StripeEventID is the webhook event id that caused this transaction
	// to be written. Nullable for the same reason as above (pre-Phase-7
	// rows have none). The DB has a partial UNIQUE index on non-NULL
	// values — that's what makes webhook processing idempotent.
	StripeEventID *string   `json:"stripe_event_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

const (
	TxStatusPending   = "pending"
	TxStatusSucceeded = "succeeded"
	TxStatusFailed    = "failed"
)
