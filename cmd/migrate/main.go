package main

import (
	"context"
	"log/slog"
	"os"

	"urlshortener/internal/config"
	"urlshortener/internal/repository"
	"urlshortener/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()
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
	logger.Info("migrations applied")
}