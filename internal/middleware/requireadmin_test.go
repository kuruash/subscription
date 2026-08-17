// Tests for RequireAdmin. Same pattern as ratelimit_test.go:
// external test package, gin.TestMode, minimal harness router.
//
// Middleware normally lives in the "don't test glue" bucket per
// CLAUDE.md, but authorization gates carry real logic — a single
// off-by-one in the role comparison is a critical security bug and
// exactly the class of change worth locking behind tests.
package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/middleware"
	"subscription-service/internal/models"
)

// adminRouter constructs a router with the given "stash" pre-middleware
// (simulating RequireAuth putting user_id + role on ctx) followed by
// RequireAdmin, terminating in a trivial 200 handler. The tests below
// swap the stash to hit each authorization path.
func adminRouter(stash gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.GET("/admin/x",
		stash,
		middleware.RequireAdmin(),
		func(c *gin.Context) { c.String(http.StatusOK, "admin-ok") },
	)
	return r
}

func adminCall(r *gin.Engine) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	r.ServeHTTP(w, req)
	return w.Code
}

// -----------------------------------------------------------------------------
// admin role → 200
// -----------------------------------------------------------------------------

func TestRequireAdmin_AdminRolePasses(t *testing.T) {
	stash := func(c *gin.Context) {
		ctx := middleware.WithUserID(c.Request.Context(), 1)
		ctx = middleware.WithRole(ctx, models.RoleAdmin)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
	if code := adminCall(adminRouter(stash)); code != http.StatusOK {
		t.Fatalf("admin call: want 200, got %d", code)
	}
}

// -----------------------------------------------------------------------------
// user role → 403
// -----------------------------------------------------------------------------

func TestRequireAdmin_NonAdminRoleGets403(t *testing.T) {
	stash := func(c *gin.Context) {
		ctx := middleware.WithUserID(c.Request.Context(), 2)
		ctx = middleware.WithRole(ctx, models.RoleUser)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
	if code := adminCall(adminRouter(stash)); code != http.StatusForbidden {
		t.Fatalf("non-admin call: want 403, got %d", code)
	}
}

// -----------------------------------------------------------------------------
// missing role claim → 403 (fail-closed)
//
// A pre-Phase-11 JWT (or any token without a role claim) reaches this
// middleware with RoleFrom returning ok=false. RequireAdmin must
// treat that as "not admin" — the alternative (defaulting to admin
// or letting it pass) would be a critical fail-open bug.
// -----------------------------------------------------------------------------

func TestRequireAdmin_MissingRoleClaimGets403(t *testing.T) {
	stash := func(c *gin.Context) {
		// Set user_id but deliberately DO NOT set role. Simulates a
		// legacy token or an auth path that didn't include a role.
		ctx := middleware.WithUserID(c.Request.Context(), 3)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
	if code := adminCall(adminRouter(stash)); code != http.StatusForbidden {
		t.Fatalf("missing-role call: want 403 (fail closed), got %d", code)
	}
}

// -----------------------------------------------------------------------------
// empty-string role → 403
//
// Belt-and-suspenders: an explicit role="" (as opposed to no role at
// all) should also fail. This can happen if someone codes a login
// path that populates the claim but forgets to look up the actual
// role from the DB. Compile-time constant models.RoleUser and
// models.RoleAdmin keep us honest at type-check time; this test
// covers the string-comparison level.
// -----------------------------------------------------------------------------

func TestRequireAdmin_EmptyRoleGets403(t *testing.T) {
	stash := func(c *gin.Context) {
		ctx := middleware.WithUserID(c.Request.Context(), 4)
		ctx = middleware.WithRole(ctx, "")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
	if code := adminCall(adminRouter(stash)); code != http.StatusForbidden {
		t.Fatalf("empty-role call: want 403, got %d", code)
	}
}

// Compile-time sanity that RoleFrom/WithRole exist and typecheck.
var _ = func() bool {
	ctx := middleware.WithRole(context.Background(), "admin")
	_, _ = middleware.RoleFrom(ctx)
	return true
}()
