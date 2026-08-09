// Package handlers is the HTTP layer. Each handler does exactly three things:
//   1. Parse the request (URL params, JSON body)
//   2. Call the service
//   3. Translate the result (or error) into an HTTP status + JSON
//
// No business logic, no SQL. If a handler is getting long, the logic
// probably belongs in services/.
package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/middleware"
	"subscription-service/internal/services"
)

type SubscriptionHandler struct {
	svc *services.SubscriptionService
}

func NewSubscriptionHandler(svc *services.SubscriptionService) *SubscriptionHandler {
	return &SubscriptionHandler{svc: svc}
}

// Register wires this handler's routes onto a Gin router group.
//
// The caller (main.go) is responsible for putting the JWT middleware on
// the group before passing it in — that keeps auth policy visible at
// the composition site instead of hidden inside each handler.
func (h *SubscriptionHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/subscriptions", h.create)
	rg.GET("/subscriptions/:id", h.get)
	rg.DELETE("/subscriptions/:id", h.cancel)
	rg.POST("/subscriptions/:id/renew", h.renew)
	rg.GET("/users/:id/subscriptions", h.listByUser)
}

type createRequest struct {
	CreatorID int    `json:"creator_id" binding:"required"`
	Plan      string `json:"plan"       binding:"required"`
}

// create: the authenticated user is subscribing themselves — the user_id
// comes from the JWT, NOT the body. Letting the body pick user_id would
// let anyone subscribe on behalf of anyone else, defeating the whole
// point of the token.
func (h *SubscriptionHandler) create(c *gin.Context) {
	authUserID, ok := middleware.UserIDFrom(c.Request.Context())
	if !ok {
		writeError(c, services.ErrForbidden)
		return
	}
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	sub, err := h.svc.Create(c.Request.Context(), services.CreateInput{
		UserID:    authUserID,
		CreatorID: req.CreatorID,
		Plan:      req.Plan,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, sub)
}

func (h *SubscriptionHandler) get(c *gin.Context) {
	authUserID, ok := middleware.UserIDFrom(c.Request.Context())
	if !ok {
		writeError(c, services.ErrForbidden)
		return
	}
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	sub, err := h.svc.GetForUser(c.Request.Context(), id, authUserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sub)
}

func (h *SubscriptionHandler) listByUser(c *gin.Context) {
	authUserID, ok := middleware.UserIDFrom(c.Request.Context())
	if !ok {
		writeError(c, services.ErrForbidden)
		return
	}
	userID, err := parseID(c, "id")
	if err != nil {
		return
	}
	// A user can only list their own subscriptions.
	if userID != authUserID {
		writeError(c, services.ErrForbidden)
		return
	}
	subs, err := h.svc.ListByUser(c.Request.Context(), userID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, subs)
}

func (h *SubscriptionHandler) cancel(c *gin.Context) {
	authUserID, ok := middleware.UserIDFrom(c.Request.Context())
	if !ok {
		writeError(c, services.ErrForbidden)
		return
	}
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	if err := h.svc.CancelForUser(c.Request.Context(), id, authUserID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *SubscriptionHandler) renew(c *gin.Context) {
	authUserID, ok := middleware.UserIDFrom(c.Request.Context())
	if !ok {
		writeError(c, services.ErrForbidden)
		return
	}
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	sub, err := h.svc.RenewForUser(c.Request.Context(), id, authUserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sub)
}

// parseID extracts a positive integer URL param, writing a 400 on failure.
func parseID(c *gin.Context, name string) (int, error) {
	id, err := strconv.Atoi(c.Param(name))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return 0, errors.New("bad id")
	}
	return id, nil
}

// writeError centralizes error → HTTP status mapping. Handlers stay clean;
// adding a new error class only means adding one case here.
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrDuplicateActive):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrInvalidInput):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
