package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/analytics"
	"urlshortener/internal/cache"
	"urlshortener/internal/config"
	"urlshortener/internal/idempotency"
	"urlshortener/internal/repository"
	"urlshortener/internal/router"
	"urlshortener/internal/service"
	"urlshortener/migrations"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := newLogger()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	pool, err := repository.NewPool(ctx, cfg.DatabaseURL, cfg.PGMaxConns)
	if err != nil {
		logger.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := repository.NewMigrator(pool, migrations.FS).Apply(ctx); err != nil {
		logger.Error("migrations failed", "err", err)
		os.Exit(1)
	}

	var urlCache *cache.URLCache
	var idem *idempotency.Store
	var rdb redis.UniversalClient
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			DialTimeout:  500 * time.Millisecond,
			ReadTimeout:  250 * time.Millisecond,
			WriteTimeout: 250 * time.Millisecond,
			PoolTimeout:  500 * time.Millisecond,
			MaxRetries:   1,
		})
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err == nil {
			logger.Info("redis connected", "addr", cfg.RedisAddr)
		} else {
			logger.Warn("redis unreachable at boot; starting with cache disabled", "addr", cfg.RedisAddr, "err", err)
		}
		cancel()
		defer rdb.Close()
		urlCache = cache.NewURLCache(rdb, cfg.RedisKeyPrefix)
		idem = idempotency.NewStore(rdb, cfg.RedisKeyPrefix)
	}

	var routeOpts []router.Option
	if rdb != nil {
		routeOpts = append(routeOpts,
			router.WithRateLimiter(rdb, cfg.RedisKeyPrefix, cfg.RateLimitPerMin),
			router.WithRecorder(analytics.NewRecorder(rdb, cfg.RedisStream)),
		)
	}

	urlSvc, err := service.NewURLService(repository.NewURLRepository(pool), urlCache, idem, cfg.BaseURL, logger)
	if err != nil {
		logger.Error("service setup failed", "err", err)
		os.Exit(1)
	}
	hostname, _ := os.Hostname()

	server := &http.Server{
		Addr:         ":" + cfg.AppPort,
		Handler:      router.New(logger, pool, urlSvc, append(routeOpts, router.WithInstanceID(hostname), router.WithMetrics())...),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
		}
		logger.Info("server stopped")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
		}
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}