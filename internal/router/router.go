package router

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/analytics"
	"urlshortener/internal/handler"
	"urlshortener/internal/middleware"
	"urlshortener/internal/service"
)

type options struct {
	rateLimiter func(http.Handler) http.Handler
	recorder    *analytics.Recorder
	instanceID  string
	metrics     bool
}

type Option func(*options)

// WithMetrics enables Prometheus /metrics (default registry: custom counters +
// auto-registered Go/process collectors).
func WithMetrics() Option {
	return func(o *options) {
		o.metrics = true
	}
}

// WithRateLimiter wraps the create endpoint with a Redis-backed per-IP limit.
func WithRateLimiter(rdb redis.UniversalClient, keyBase string, perMinute int) Option {
	return func(o *options) {
		o.rateLimiter = middleware.RateLimit(rdb, keyBase, perMinute)
	}
}

// WithRecorder attaches an analytics recorder to the redirect path.
func WithRecorder(rec *analytics.Recorder) Option {
	return func(o *options) {
		o.recorder = rec
	}
}

// WithInstanceID stamps responses with an X-Instance header for LB observability.
func WithInstanceID(id string) Option {
	return func(o *options) {
		o.instanceID = id
	}
}

func New(logger *slog.Logger, pool *pgxpool.Pool, urls *service.URLService, opts ...Option) http.Handler {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	r := chi.NewRouter()

	if o.instanceID != "" {
		r.Use(middleware.InstanceID(o.instanceID))
	}
	r.Use(middleware.Recoverer(logger))
	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(logger))
	r.Use(middleware.Metrics)

	r.Get("/health", handler.Health(logger))
	r.Get("/ready", handler.Ready(logger, pool))

	if o.metrics {
		r.Method("GET", "/metrics", promhttp.Handler())
	}

	uh := handler.NewURLHandler(urls, logger, o.recorder)
	r.Route("/api/v1/urls", func(r chi.Router) {
		if o.rateLimiter != nil {
			r.With(o.rateLimiter).Post("/", uh.Create)
		} else {
			r.Post("/", uh.Create)
		}
		r.Get("/{code}", uh.Get)
		r.Delete("/{code}", uh.Delete)
		r.Get("/{code}/stats", uh.Stats)
	})
	r.Get("/{code}", uh.Redirect)

	return r
}