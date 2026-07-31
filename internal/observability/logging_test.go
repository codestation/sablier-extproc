package observability

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/sablierapp/sablier-extproc/internal/sablier"
)

func TestNewLoggerUsesConfiguredLevel(t *testing.T) {
	tests := []struct {
		level      string
		enabled    slog.Level
		notEnabled slog.Level
	}{
		{level: "debug", enabled: slog.LevelDebug, notEnabled: slog.LevelDebug - 1},
		{level: "WARN", enabled: slog.LevelWarn, notEnabled: slog.LevelInfo},
		{level: "error", enabled: slog.LevelError, notEnabled: slog.LevelWarn},
		{level: "unknown", enabled: slog.LevelInfo, notEnabled: slog.LevelDebug},
	}

	for _, test := range tests {
		t.Run(test.level, func(t *testing.T) {
			logger := NewLogger(test.level)
			if !logger.Enabled(context.Background(), test.enabled) {
				t.Fatalf("level %s should be enabled", test.enabled)
			}
			if logger.Enabled(context.Background(), test.notEnabled) {
				t.Fatalf("level %s should be disabled", test.notEnabled)
			}
		})
	}
}

func TestErrorCategory(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "none", want: ""},
		{name: "deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "OS deadline", err: os.ErrDeadlineExceeded, want: "timeout"},
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "network timeout", err: timeoutError{}, want: "timeout"},
		{name: "HTTP status", err: &sablier.HTTPStatusError{StatusCode: 502}, want: "http_status"},
		{name: "session status", err: &sablier.SessionStatusError{Value: "starting"}, want: "session_status"},
		{name: "response too large", err: sablier.ErrResponseTooLarge, want: "response_too_large"},
		{name: "transport", err: errors.New("connection reset"), want: "transport"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCategory(test.err); got != test.want {
				t.Fatalf("ErrorCategory(%v) = %q; want %q", test.err, got, test.want)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

var _ net.Error = timeoutError{}
