// requireadmin.go — role gate for /admin/* routes.
//
// PHASE 5 GAP MATTERS HERE — READ BEFORE OVERSTATING THIS:
// This middleware only gates access based on the `role` claim in an
// already-verified JWT. That claim was set at /login time by reading
// the user's DB row. /login itself still has NO PASSWORD CHECK (Phase
// 5 documented gap): POST {"user_id": N} mints a token for user N.
//
// So the ACTUAL security posture right now is:
//
//	"Anyone who knows an admin user_id can mint an admin token."
//
// Not:
//
//	"Only authenticated admins can hit /admin/*."
//
// The middleware is correct — the gap is upstream in /login. Once
// Phase 5 gets a real password check, this middleware becomes a
// genuine authorization gate. Until then it's a NECESSARY layer for
// the RBAC plumbing but not a SUFFICIENT one for real access control.
// Do not deploy /admin/* to any network reachable by an attacker
// who might guess user_id=1 is an admin.
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/models"
)

// RequireAdmin returns a gin middleware that 403s any request whose
// role claim isn't "admin". MUST run AFTER RequireAuth in the chain —
// this middleware doesn't parse the JWT itself, it only reads the
// role that RequireAuth stashed on the request context.
//
// Return code choice — 403 not 401:
//   - 401 = "authenticate first." We don't want that; the user IS
//     authenticated, they just aren't authorized for this resource.
//   - 403 = "authenticated, but forbidden." Matches the semantics
//     and matches Phase 5's ownership-check behavior for consistency.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := RoleFrom(c.Request.Context())
		if !ok || role != models.RoleAdmin {
			// Terse message — don't leak "you'd be allowed if you were
			// role X" to a caller who shouldn't be probing.
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}
