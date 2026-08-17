package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/auth"
	"subscription-service/internal/models"
)

// AuthHandler handles /login.
//
// GAP: this endpoint does NOT verify a password. It takes a user_id in
// the body and issues a token for it. Anyone can mint a token for any
// user_id — including a user_id that happens to have role='admin'.
// See internal/middleware/requireadmin.go for the full impact on
// Phase 11's admin gate.
//
// A real login endpoint would:
//  1. Take email + password.
//  2. Look up the user, compare the password against a bcrypt/argon2
//     hash stored in the users table.
//  3. Only then issue a token, using the DB row's id as the subject.
//
// Phase 11 change: we now read the user's role from the DB and bake
// it into the JWT claim. The DB is the source of truth for role — a
// client can't pass a role in the /login body (that would let anyone
// mint an admin token). The role is fixed for the lifetime of the
// token (see auth.Sign's PHASE 11 NOTE for the demotion-lag trade).
type AuthHandler struct {
	secret []byte
	db     *sql.DB
}

func NewAuthHandler(secret []byte, db *sql.DB) *AuthHandler {
	return &AuthHandler{secret: secret, db: db}
}

// Register attaches /login onto the given router. Takes gin.IRouter so
// the caller can pass either the root engine or a group with
// middleware pre-attached — that's how Phase 8's IP-based rate limiter
// gets in front of /login without this handler needing to know about it.
func (h *AuthHandler) Register(r gin.IRouter) {
	r.POST("/login", h.login)
}

type loginRequest struct {
	UserID int `json:"user_id" binding:"required"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	// Role echoed back so a UI knows which navigation to render. NOT
	// a security boundary — servers must still enforce via RequireAdmin;
	// this is a UX affordance.
	Role string `json:"role"`
}

func (h *AuthHandler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Look up the role from the DB. This is the ONLY authoritative
	// source — the client can't influence it. If the user row is
	// missing we still return 404 (not 401) because that's what's
	// true: we were asked to mint a token for a user that doesn't
	// exist. When Phase 5's password check lands, this becomes a
	// 401 to avoid leaking user existence.
	var role string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT role FROM users WHERE id = $1`, req.UserID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not look up user"})
		return
	}

	// Defense in depth: even though migration 003's CHECK constraint
	// restricts role to ('user','admin'), fall back to RoleUser if we
	// ever read an unexpected value rather than propagating garbage
	// into the JWT.
	if role != models.RoleUser && role != models.RoleAdmin {
		role = models.RoleUser
	}

	token, expires, err := auth.Sign(req.UserID, role, h.secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}
	c.JSON(http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: expires.UTC().Format("2006-01-02T15:04:05Z"),
		Role:      role,
	})
}
