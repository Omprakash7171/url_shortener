package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	AppPort         string
	BaseURL         string
	DatabaseURL     string
	RedisAddr       string
	RedisKeyPrefix  string
	RedisStream     string
	RateLimitPerMin int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	LogLevel        string
	PGMaxConns      int32
	MetricsAddr     string
}

func Load() (Config, error) {
	cfg := Config{
		AppPort:         getEnv("APP_PORT", "8080"),
		BaseURL:         getEnv("BASE_URL", "http://localhost:8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		RedisAddr:       getEnv("REDIS_ADDR", ""),
		RedisKeyPrefix:  getEnv("REDIS_KEY_PREFIX", "us:"),
		RedisStream:     getEnv("REDIS_STREAM", "us:events"),
		RateLimitPerMin: getInt("RATE_LIMIT_PER_MIN", 60),
		ReadTimeout:     getDuration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    getDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:     getDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout: getDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		PGMaxConns:      int32(getInt("PG_MAX_CONNS", 0)),
		MetricsAddr:     getEnv("METRICS_ADDR", ":8081"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}