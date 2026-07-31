// Package observability provides structured logging and Prometheus metrics.
package observability

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/sablierapp/sablier-extproc/internal/sablier"
)

// NewLogger creates a JSON logger at the configured level.
func NewLogger(level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)}))
}

// ErrorCategory maps an error to a bounded metrics and logging category.
func ErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	var httpStatusError *sablier.HTTPStatusError
	if errors.As(err, &httpStatusError) {
		return "http_status"
	}
	var sessionStatusError *sablier.SessionStatusError
	if errors.As(err, &sessionStatusError) {
		return "session_status"
	}
	if errors.Is(err, sablier.ErrResponseTooLarge) {
		return "response_too_large"
	}
	return "transport"
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
