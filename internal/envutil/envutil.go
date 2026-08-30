// Package envutil provides helpers for parsing environment variables at
// application startup. Invalid values cause the process to exit with a clear
// error message rather than silently falling back to a default.
package envutil

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

// String returns the value of the named environment variable, or fallback if
// the variable is unset or empty.
func String(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Bool parses the named environment variable as a boolean using
// strconv.ParseBool. An empty value returns fallback. An invalid value logs
// an error and calls os.Exit(1).
func Bool(logger *slog.Logger, key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		logger.Error("invalid boolean environment variable",
			slog.String("key", key), slog.String("value", v))
		os.Exit(1)
	}
	return b
}

// HoldSeconds parses the named environment variable as a non-negative integer
// number of seconds and returns it as a time.Duration. An empty value returns
// fallback. An invalid or negative value logs an error and calls os.Exit(1).
func HoldSeconds(logger *slog.Logger, key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Error("invalid integer environment variable",
			slog.String("key", key), slog.String("value", v))
		os.Exit(1)
	}
	if n < 0 {
		logger.Error("environment variable must not be negative",
			slog.String("key", key), slog.Int("value", n))
		os.Exit(1)
	}
	return time.Duration(n) * time.Second
}
