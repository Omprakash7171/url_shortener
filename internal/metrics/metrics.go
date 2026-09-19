// Package metrics exposes prometheus instrumentation used across stages 12-20.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var (
	registerOnce sync.Once

	CacheHits     *prometheus.CounterVec
	CacheMisses   *prometheus.CounterVec
	CacheFailOpen *prometheus.CounterVec

	RateLimitFailOpen prometheus.Counter

	EventsPublished prometheus.Counter
	EventsDropped   prometheus.Counter
	EventsFailed    prometheus.Counter

	AnalyticsProcessed prometheus.Counter
	AnalyticsDeduped   prometheus.Counter

	HTTPRequests prometheus.Histogram

	IdempotencyReplays prometheus.Counter
	IdempotencySkipped prometheus.Counter
)

func init() {
	registerOnce.Do(func() {
		// The Go runtime + process collectors are auto-registered into the
		// default registry by client_golang itself; registering them again here
		// would panic with a duplicate-registration error.
		CacheHits = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "url_cache_hits_total",
				Help: "Cache hits, labelled by cache layer.",
			},
			[]string{"layer"},
		)
		CacheMisses = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "url_cache_misses_total",
				Help: "Cache misses, labelled by cache layer.",
			},
			[]string{"layer"},
		)
		CacheFailOpen = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "url_cache_fail_open_total",
				Help: "Cache-like layer fell back to the source of truth.",
			},
			[]string{"layer"},
		)
		RateLimitFailOpen = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_ratelimit_fail_open_total",
				Help: "Rate limiter allowed a request because Redis was unreachable.",
			},
		)
		EventsPublished = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_events_published_total",
				Help: "Analytics events appended to the Redis stream.",
			},
		)
		EventsDropped = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_events_dropped_total",
				Help: "Analytics events dropped because the outgoing buffer was full.",
			},
		)
		EventsFailed = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_events_failed_total",
				Help: "Analytics events that could not be appended to Redis.",
			},
		)
		AnalyticsProcessed = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_analytics_processed_total",
				Help: "Analytics events aggregated into PostgreSQL by the worker.",
			},
		)
		AnalyticsDeduped = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_analytics_deduped_total",
				Help: "Analytics events skipped as duplicates.",
			},
		)
		HTTPRequests = prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "url_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds (measured at the router).",
				Buckets: prometheus.DefBuckets,
			},
		)
		IdempotencyReplays = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_idempotency_replays_total",
				Help: "Create requests answered with a previously stored short code.",
			},
		)
		IdempotencySkipped = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "url_idempotency_skipped_total",
				Help: "Create requests that bypassed the idempotency store because it was unreachable.",
			},
		)
		prometheus.MustRegister(
			CacheHits,
			CacheMisses,
			CacheFailOpen,
			RateLimitFailOpen,
			EventsPublished,
			EventsDropped,
			EventsFailed,
			AnalyticsProcessed,
			AnalyticsDeduped,
			HTTPRequests,
			IdempotencyReplays,
			IdempotencySkipped,
		)
	})
}

func cacheCounterValue(c *prometheus.CounterVec, label string) float64 {
	m := &dto.Metric{}
	if err := c.WithLabelValues(label).Write(m); err != nil {
		return 0
	}
	return *m.Counter.Value
}

func CacheHitCount(layer string) float64 {
	return cacheCounterValue(CacheHits, layer)
}

func CacheMissCount(layer string) float64 {
	return cacheCounterValue(CacheMisses, layer)
}

func CacheFailOpenCount(layer string) float64 {
	return cacheCounterValue(CacheFailOpen, layer)
}

func CounterValue(c prometheus.Counter) float64 {
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		return 0
	}
	return *m.Counter.Value
}