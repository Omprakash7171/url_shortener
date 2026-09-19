package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"urlshortener/internal/middleware"
)

func Health(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func Ready(logger *slog.Logger, pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		body := map[string]any{"status": "ready", "database": "ok"}
		status := http.StatusOK

		if err := pool.Ping(ctx); err != nil {
			logger.Warn("ready check failed",
				"request_id", middleware.RequestIDFrom(r.Context()),
				"err", err,
			)
			body = map[string]any{"status": "unavailable", "database": "down"}
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, body)
	}
}