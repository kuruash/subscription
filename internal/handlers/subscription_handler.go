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
	"subscription-service/internal/services"
)

type SubscriptionHandler struct {
	svc *services.SubscriptionService
}

func NewSubscriptionHandler(svc *services.SubscriptionService) *SubscriptionHandler {
	return &SubscriptionHandler{svc: svc}
}

// Register wires this handler's routes onto a Gin router. Keeping this
// method here (instead of in main.go) means main stays small and each
// feature owns its routing.
func (h *SubscriptionHandler) Register(r *gin.Engine) {
	r.POST("/subscriptions", h.create)
	r.GET("/subscriptions/:id", h.get)
	r.DELETE("/subscriptions/:id", h.cancel)
	r.POST("/subscriptions/:id/renew", h.renew)
	r.GET("/users/:id/subscriptions", h.listByUser)
}

type createRequest struct {
	// `binding:"required"` is Gin's validator tag — missing fields fail
	// ShouldBindJSON automatically, saving us a stack of `if x == 0` checks.
	UserID    int    `json:"user_id"    binding:"required"`
	CreatorID int    `json:"creator_id" binding:"required"`
	Plan      string `json:"plan"       binding:"required"`
}

func (h *SubscriptionHandler) create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// c.Request.Context() carries client-disconnect / timeout signals
	// all the way down to the DB driver. Never use context.Background()
	// inside a handler.
	sub, err := h.svc.Create(c.Request.Context(), services.CreateInput{
		UserID:    req.UserID,
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
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	sub, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sub)
}

func (h *SubscriptionHandler) listByUser(c *gin.Context) {
	userID, err := parseID(c, "id")
	if err != nil {
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
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	if err := h.svc.Cancel(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *SubscriptionHandler) renew(c *gin.Context) {
	id, err := parseID(c, "id")
	if err != nil {
		return
	}
	sub, err := h.svc.Renew(c.Request.Context(), id)
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
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
