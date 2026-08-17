// Package metrics owns every Prometheus metric this service exports.
//
// Why a single package instead of scattering metric definitions across
// the services/handlers/middleware they measure:
//   - promauto registers with prometheus.DefaultRegisterer at package
//     init. If two packages both defined a Counter with the same name
//     the second one would panic at init. Central definitions make
//     collisions impossible.
//   - It's the ONE place to eyeball "which metrics exist" — critical
//     when writing PromQL / Grafana panels or an alerting runbook.
//   - Tests that want to reset counters between runs (there are none
//     yet in Phase 9, but future ones) know exactly which var to touch.
//
// CARDINALITY POLICY — READ BEFORE ADDING A NEW LABEL:
// Prometheus stores one time-series per unique combination of label
// values. Labels with unbounded ranges (user_id, subscription_id,
// payment_intent_id) create UNBOUNDED cardinality — every user gets a
// new series, storage blows up, PromQL queries slow down, and by the
// time you notice you have millions of dead series taking up disk.
//
// Rules of thumb we follow here:
//   - Route label: bounded (~10 routes). Use gin's FullPath() so we
//     get "/subscriptions/:id", not "/subscriptions/42" — the : part
//     is what keeps cardinality finite.
//   - Status code: bounded (~50 codes actually used).
//   - Limiter name: bounded ("login", "sub").
//   - NEVER user_id, subscription_id, PI id, event id, or anything
//     else that grows with usage.
//
// If a debug question really needs per-user data, that's what logs
// or traces are for — not metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "subscription_service"

// -----------------------------------------------------------------------------
// RED (Rate, Errors, Duration) — the generic HTTP surface.
// Recorded automatically by internal/middleware/metrics.go on every request.
// -----------------------------------------------------------------------------

// HTTPRequestsTotal — rate. Labeled by route + status; both bounded.
var HTTPRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "Total HTTP requests handled, by matched route template and status code.",
	},
	[]string{"route", "status"},
)

// HTTPErrorsTotal — errors. A separate counter (not just derived from
// status="5xx" on HTTPRequestsTotal) so alerts can be written like
// `rate(subscription_service_http_errors_total[5m]) > 0` without
// having to filter labels.
var HTTPErrorsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_errors_total",
		Help:      "Total HTTP 5xx responses, by matched route template.",
	},
	[]string{"route"},
)

// HTTPDurationSeconds — duration. Histogram (not summary) because
// histograms are aggregatable across instances in Prometheus:
// p95 across all pods = histogram_quantile(0.95, sum by (le, route)(...)).
// Summaries compute quantiles client-side and can't be aggregated.
//
// Default buckets (5ms → 10s) fit an HTTP API. If we ever want tighter
// resolution for a specific hot endpoint, override with custom buckets
// per-route via a separate HistogramVec.
var HTTPDurationSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_duration_seconds",
		Help:      "HTTP request duration in seconds, by matched route template.",
		Buckets:   prometheus.DefBuckets,
	},
	[]string{"route"},
)

// -----------------------------------------------------------------------------
// Domain counters — the "what business events happened" surface.
// Instrumented at the service layer, not the handler layer, because
// business events belong to services (e.g. a webhook subscribing IS a
// subscription_created; a subscribe attempt that failed the partial
// unique index is NOT — this distinction lives in service code).
// -----------------------------------------------------------------------------

// SubscriptionsCreatedTotal — a Subscribe call that produced a
// pending row (the API-observable event). Does not fire on
// duplicate-active rejection (409 response).
var SubscriptionsCreatedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "subscriptions_created_total",
	Help:      "Total pending subscriptions created via POST /subscriptions.",
})

// SubscriptionsCancelledTotal — fires for user-initiated cancel AND
// for payment_intent.payment_failed cancellations. If we ever need to
// distinguish the two, add a "reason" label with a bounded value set
// (user, payment_failed) — small, safe cardinality.
var SubscriptionsCancelledTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "subscriptions_cancelled_total",
	Help:      "Total subscriptions moved to 'cancelled' status.",
})

// SubscriptionsExpiredTotal — the worker's sweep is the natural home;
// it's the only path that ever moves rows to 'expired'.
var SubscriptionsExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "subscriptions_expired_total",
	Help:      "Total subscriptions moved to 'expired' status by the worker's periodic sweep.",
})

// -----------------------------------------------------------------------------
// Cache counters — same information Phase 2 already prints as log
// lines, now also aggregatable. Ratio hits/(hits+misses) = hit rate.
// -----------------------------------------------------------------------------

var CacheHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "cache_hits_total",
	Help:      "Total Redis cache hits on the user-subscriptions list.",
})

var CacheMissesTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "cache_misses_total",
	Help:      "Total Redis cache misses on the user-subscriptions list (includes corrupt and Redis-down cases that fell back to DB).",
})

// -----------------------------------------------------------------------------
// Rate limit rejections — labeled by which limiter fired.
// -----------------------------------------------------------------------------

var RateLimitRejectionsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limit_rejections_total",
		Help:      "Total requests rejected with 429 by a Limiter, labeled by limiter name (e.g. 'login', 'sub').",
	},
	[]string{"limiter"},
)

// -----------------------------------------------------------------------------
// Active subscriptions — gauge, refreshed periodically.
//
// TRADEOFF (periodic query vs transition-based increment/decrement):
//
// Periodic query: SELECT COUNT(*) WHERE status='active' every N sec.
//   + Simple, single source of truth (the DB itself)
//   + Cannot drift — a direct SQL update from a psql prompt or a
//     future ad-hoc script is reflected on the next tick
//   + Works correctly across horizontally-scaled API + worker without
//     any coordination; every instance queries the shared DB
//   - Adds one SELECT COUNT per interval per instance. At 15s ticks
//     and 2 instances that's 8 queries/min — trivial for indexed
//     COUNT(*)
//
// Transition-based (Inc on Subscribe/activate, Dec on cancel/expire):
//   + No SELECT COUNT at all
//   + Instantaneous updates
//   - Seed problem: at startup the counter is 0 until events fire.
//     Fix requires exactly one initial COUNT anyway, so half the win
//     evaporates.
//   - DRIFT RISK: any state change that bypasses the service layer
//     (a hotfix SQL UPDATE, the worker on a different code version,
//     Cancel called on a stopped API) desyncs the counter from reality.
//   - N instances double-counting: if each API pod holds its own
//     counter, they diverge. Need a single-writer scheme or push to
//     a shared store — at which point you've reinvented the DB query
//
// Chose PERIODIC QUERY. The complexity delta for transition-based
// isn't paid back by the ~indexed-COUNT-per-15s cost saved.
// -----------------------------------------------------------------------------

var ActiveSubscriptions = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: namespace,
	Name:      "active_subscriptions",
	Help:      "Current count of subscriptions with status='active'. Refreshed on a periodic interval by cmd/api/main.go; may lag reality by up to one refresh interval.",
})
