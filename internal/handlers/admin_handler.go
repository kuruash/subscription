// admin_handler.go — read-only admin dashboard endpoints (Phase 11).
//
// All routes registered here MUST be attached to a router group with
// BOTH RequireAuth AND RequireAdmin already applied (see cmd/api/main.go).
// This handler does not enforce authorization itself — that lives in
// middleware so the check can't be forgotten on any single route.
package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/models"
	"subscription-service/internal/services"
)

type AdminHandler struct {
	svc *services.SubscriptionService
}

func NewAdminHandler(svc *services.SubscriptionService) *AdminHandler {
	return &AdminHandler{svc: svc}
}

func (h *AdminHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/admin/subscriptions", h.listAll)
	rg.GET("/admin/stats", h.stats)
}

// Pagination defaults + caps for /admin/subscriptions.
//
// defaultLimit=50: reasonable for a UI page or an eyeballing psql-style
// export. maxLimit=500: a client CAN request bigger but not unbounded —
// unbounded would let an admin accidentally OOM the process by asking
// for all rows on a large table.
const (
	defaultLimit = 50
	maxLimit     = 500
)

type listAllResponse struct {
	Subscriptions []models.Subscription `json:"subscriptions"`
	Limit         int                   `json:"limit"`
	Offset        int                   `json:"offset"`
	Count         int                   `json:"count"` // rows in THIS page, not total
}

func (h *AdminHandler) listAll(c *gin.Context) {
	limit := parseIntQuery(c, "limit", defaultLimit)
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}
	offset := parseIntQuery(c, "offset", 0)
	if offset < 0 {
		offset = 0
	}

	subs, err := h.svc.ListAll(c.Request.Context(), limit, offset)
	if err != nil {
		writeError(c, err)
		return
	}
	// A nil slice serializes to null in JSON, an empty slice serializes
	// to []. Clients generally expect the array form, so normalize.
	if subs == nil {
		subs = []models.Subscription{}
	}
	c.JSON(http.StatusOK, listAllResponse{
		Subscriptions: subs,
		Limit:         limit,
		Offset:        offset,
		Count:         len(subs),
	})
}

func (h *AdminHandler) stats(c *gin.Context) {
	stats, err := h.svc.Stats(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// parseIntQuery reads a query param as an int, returning def if
// missing or unparseable. Bounds-checking (limit>0, offset>=0) is the
// caller's job — that's where the correct fallback for out-of-range
// values lives.
func parseIntQuery(c *gin.Context, name string, def int) int {
	raw := c.Query(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
