// Package logx sets up structured logging and request correlation.
package logx

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type ctxKey struct{}

// New builds a slog.Logger for the given format ("text"|"json") and level.
func New(format, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// With stores a logger on the context.
func With(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the context's logger, or the default logger.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

var reqSeq atomic.Uint64

// NextRequestID returns a short, monotonic, process-unique request id.
// Readable in logs and stable enough to correlate /debug entries.
func NextRequestID() string {
	n := reqSeq.Add(1)
	return "r" + time.Now().UTC().Format("150405") + "-" + itoa(n)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
