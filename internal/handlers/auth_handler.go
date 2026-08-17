package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/auth"
)

// AuthHandler handles /login.
//
// GAP: this endpoint does NOT verify a password. It takes a user_id in
// the body and issues a token for it. Anyone can mint a token for any
// user_id. This is a placeholder so the rest of Phase 5 (middleware,
// ownership checks, protected routes) can be exercised end-to-end
// without also building a full user/password/hashing system.
//
// A real login endpoint would:
//   1. Take email + password.
//   2. Look up the user, compare the password against a bcrypt/argon2
//      hash stored in the users table.
//   3. Only then issue a token, using the DB row's id as the subject.
type AuthHandler struct {
	secret []byte
}

func NewAuthHandler(secret []byte) *AuthHandler {
	return &AuthHandler{secret: secret}
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
}

func (h *AuthHandler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token, expires, err := auth.Sign(req.UserID, h.secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}
	c.JSON(http.StatusOK, loginResponse{Token: token, ExpiresAt: expires.UTC().Format("2006-01-02T15:04:05Z")})
}
