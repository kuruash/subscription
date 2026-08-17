// Package middleware holds Gin middlewares — HTTP-level cross-cutting
// concerns like auth, logging, rate limiting.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/auth"
)

// ctxKey is a package-private type so nothing outside this package can
// accidentally read or overwrite our context values with a plain string
// like "userID" — Go's context.Value uses the key's type identity, so
// using an unexported type makes collisions impossible.
type ctxKey int

const (
	userIDKey ctxKey = iota
	roleKey
)

// UserIDFrom returns the authenticated user's ID that RequireAuth stashed
// on the context. Second return is false if the request wasn't
// authenticated (i.e. this was called without the middleware in the
// chain — a programmer error, not a runtime one).
func UserIDFrom(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(userIDKey).(int)
	return v, ok
}

// WithUserID returns a child context with user_id stashed under the
// same package-private key RequireAuth uses. Exported so tests can
// simulate an authenticated request without minting a real JWT, and
// so any future auth path (mTLS, session cookie) can populate the
// same key that downstream code (handlers, rate-limiter keyers) reads
// via UserIDFrom.
func WithUserID(ctx context.Context, userID int) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// RoleFrom returns the role claim RequireAuth stashed on the context.
// Second return is false if there's no role at all — treat as "not
// admin" at the call site (that's what RequireAdmin does).
func RoleFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(roleKey).(string)
	return v, ok
}

// WithRole stashes a role on the context under the same key
// RequireAuth uses. Same purpose as WithUserID: tests and future
// alternative auth paths can populate it without going through the
// full JWT parse.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleKey, role)
}

// RequireAuth verifies a Bearer JWT and stashes the user ID on the
// request context for handlers to read via UserIDFrom.
//
// On any failure (missing header, malformed token, bad signature,
// expired) it aborts with 401 and does NOT call subsequent handlers.
// c.Abort() is what stops the chain — without it, the handler would
// still run after our JSON is written, which is a common footgun.
func RequireAuth(secret []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if h == "" {
			unauthorized(c, "missing Authorization header")
			return
		}
		// Expected format: "Bearer <token>". Case-insensitive scheme name
		// is technically required by RFC 6750; we're liberal on that.
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			unauthorized(c, "expected Bearer token")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
		if token == "" {
			unauthorized(c, "empty token")
			return
		}

		userID, role, err := auth.Parse(token, secret)
		if err != nil {
			// Deliberately not surfacing which check failed — see auth.Parse.
			unauthorized(c, "invalid or expired token")
			return
		}

		// Stash both on the request's context.Context (not just
		// gin.Context) so services/repositories that only accept a
		// plain ctx can read it if they ever need to.
		ctx := context.WithValue(c.Request.Context(), userIDKey, userID)
		ctx = context.WithValue(ctx, roleKey, role)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

func unauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
}
