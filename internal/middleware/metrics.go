// metrics.go — Gin middleware that records the RED (Rate, Errors,
// Duration) metrics defined in internal/metrics for every request.
//
// PERFORMANCE — what this actually costs per request:
//   - time.Now() twice: ~50–100 ns total on modern x86/ARM.
//   - counter.WithLabelValues(...).Inc(): a labeled lookup (map read
//     under an RWMutex, cached after first call for the same label
//     combo) plus one atomic add — <100 ns steady state, roughly a
//     handful of hundred ns on the very first observation of a new
//     label pair while the CounterVec's map fills in.
//   - histogram Observe(): atomic bucket increment + sum add — ~100 ns.
//   - Total: <300 ns per request steady state, plus ~100 bytes GC
//     churn from the strconv on status (avoidable via a small lookup
//     table if we ever cared).
//
// At 1000 RPS this is 0.3 ms/s = 0.03% of one core. Truly negligible
// compared to any real handler work (a Redis GET is ~50 µs, a Postgres
// query is ~500 µs). We are not being generous when we say "the cost
// is fine here" — a Go handler serving JSON out of memory typically
// takes 30–100 µs, so instrumentation is <1% of that.
//
// What DOES cost measurably is high cardinality — see the cardinality
// warning in internal/metrics/metrics.go. Which is exactly why we use
// c.FullPath() (the route TEMPLATE) as the label, not c.Request.URL.Path
// (the concrete URL with :id substituted).
package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"subscription-service/internal/metrics"
)

// Metrics returns Gin middleware that records request count, error
// count, and latency for every route. Attach with r.Use(Metrics())
// EARLY in main.go so it wraps the full handler + downstream middleware
// timing (including auth, rate limiting, DB calls).
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		elapsed := time.Since(start).Seconds()

		// c.FullPath() returns the ROUTE TEMPLATE ("/subscriptions/:id"),
		// not the concrete URL ("/subscriptions/42"). Using the template
		// keeps cardinality bounded to ~one series per registered route.
		route := c.FullPath()
		if route == "" {
			// Unmatched route (404). Collapse all of these under a
			// single label to avoid an attacker filling our metric
			// store by hitting /foo, /bar, /baz, ... — each of which
			// would otherwise be a distinct label value.
			route = "unmatched"
		}
		status := strconv.Itoa(c.Writer.Status())

		metrics.HTTPRequestsTotal.WithLabelValues(route, status).Inc()
		metrics.HTTPDurationSeconds.WithLabelValues(route).Observe(elapsed)
		if c.Writer.Status() >= 500 {
			metrics.HTTPErrorsTotal.WithLabelValues(route).Inc()
		}
	}
}
