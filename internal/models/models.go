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
	ID        int       `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

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
	ID         int       `json:"id"`
	UserID     int       `json:"user_id"`
	CreatorID  int       `json:"creator_id"`
	Plan       string    `json:"plan"`
	Status     string    `json:"status"`
	StartDate  time.Time `json:"start_date"`
	ExpiresAt  time.Time `json:"expires_at"`
	AutoRenew  bool      `json:"auto_renew"`
	CreatedAt  time.Time `json:"created_at"`
}

// Status constants. Go doesn't have enums; the idiomatic replacement is
// a typed set of const strings (or ints). Using these instead of raw
// "active" literals scattered through the code prevents typos the compiler
// can't catch (`"activ"` would silently mean "never matches").
const (
	StatusActive    = "active"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

type Transaction struct {
	ID             int       `json:"id"`
	SubscriptionID int       `json:"subscription_id"`
	Amount         float64   `json:"amount"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

const (
	TxStatusPending   = "pending"
	TxStatusSucceeded = "succeeded"
	TxStatusFailed    = "failed"
)
