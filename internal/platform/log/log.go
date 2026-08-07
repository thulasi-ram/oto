package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/thulasiram/oto/internal/platform/config"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// New builds the process logger from configuration. JSON is the production format.
func New(cfg config.LogConfig, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	opts := &slog.HandlerOptions{
		Level:     ParseLevel(cfg.Level),
		AddSource: cfg.SourceLocation,
	}

	var h slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// ParseLevel maps a configured level name onto a slog.Level, defaulting to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Into returns a context carrying l.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// From returns the request-scoped logger, or the default logger if none is set.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithRequestID returns a context carrying id, for correlation across log records,
// problem+json bodies and the response header.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id carried by ctx, or "".
func RequestID(ctx context.Context) string {
	if s, ok := ctx.Value(requestIDKey).(string); ok {
		return s
	}
	return ""
}

// Redactor blanks the values of configured sensitive label keys.
// Redaction is applied BEFORE a raw payload is persisted or logged.
type Redactor struct {
	keys map[string]struct{}
}

// NewRedactor builds a Redactor over the configured label keys.
func NewRedactor(keys []string) *Redactor {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(strings.ToLower(k)); k != "" {
			m[k] = struct{}{}
		}
	}
	return &Redactor{keys: m}
}

// Redacted is the placeholder written in place of a redacted value.
const Redacted = "[redacted]"

// Labels returns a copy of in with every configured key's value replaced.
func (r *Redactor) Labels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if _, hit := r.keys[strings.ToLower(k)]; hit {
			out[k] = Redacted
			continue
		}
		out[k] = v
	}
	return out
}

// Has reports whether key is redacted.
func (r *Redactor) Has(key string) bool {
	_, ok := r.keys[strings.ToLower(key)]
	return ok
}
