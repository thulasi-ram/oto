package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/log"
)

// HeaderRequestID is the correlation header. It is echoed on every response and
// appears in every log record and every problem+json body.
const HeaderRequestID = "X-Request-Id"

// RequestID assigns each request a UUIDv7, honouring an inbound header when one
// is supplied and looks sane, and puts it in the context and the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if rid == "" || len(rid) > 64 {
			rid = id.New().String()
		}
		w.Header().Set(HeaderRequestID, rid)
		next.ServeHTTP(w, r.WithContext(log.WithRequestID(r.Context(), rid)))
	})
}

// Logger attaches a request-scoped logger and emits one structured record per
// request on completion. Bodies are never logged.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rid := log.RequestID(ctx)

			l := base.With(slog.String("request_id", rid))
			ctx = log.Into(ctx, l)
			r = r.WithContext(ctx)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			started := time.Now()

			defer func() {
				attrs := []any{
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Int64("duration_ms", time.Since(started).Milliseconds()),
					slog.String("remote", r.RemoteAddr),
				}
				switch {
				case ww.Status() >= 500:
					l.Error("http request", attrs...)
				case ww.Status() >= 400:
					l.Warn("http request", attrs...)
				default:
					l.Info("http request", attrs...)
				}
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

// Recover converts a panic into a 500 problem+json response and keeps the process
// alive. The stack goes to the log, never to the client.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, not an error chain
				panic(rec)
			}
			log.From(r.Context()).Error("panic recovered",
				slog.Any("panic", rec),
				slog.String("path", r.URL.Path),
				slog.String("stack", string(debug.Stack())),
			)
			httpx.WriteProblem(w, r, errs.Internal("panic", nil))
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS configures cross-origin access for the SolidJS dev server and any
// configured UI origin. An empty origin list disables CORS entirely.
func CORS(cfg config.HTTPConfig) func(http.Handler) http.Handler {
	if len(cfg.CORSOrigins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", HeaderRequestID, "Idempotency-Key", "Last-Event-ID"},
		ExposedHeaders:   []string{HeaderRequestID, "Retry-After"},
		AllowCredentials: true,
		MaxAge:           300,
	})
}

// Timeout bounds a request. Streaming routes (SSE) must be mounted outside it.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return middleware.Timeout(d)
}

// MaxBody caps the request body size before any handler reads it.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
