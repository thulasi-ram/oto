package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/log"
)

// ContentTypeProblem is the RFC 9457 media type.
const ContentTypeProblem = errs.ContentTypeProblem

// Problem is the RFC 9457 problem detail defined in SPEC §L.2.2. The shape lives
// in platform/errs so that a pure domain package can build one; this alias is the
// transport-side name.
type Problem = errs.ProblemDTO

// DefaultRetryAfter is the fallback Retry-After on a 503.
var DefaultRetryAfter = 10 * time.Second

// WriteProblem maps err onto an RFC 9457 problem+json response and writes it.
// This is the ONE place in oto where an error becomes an HTTP status and a body.
//
// Internal errors are logged with the cause and rendered without it: an operator
// gets the detail from the log line correlated by request_id, a caller never does.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	p := errs.Problem(err, r.URL.Path, log.RequestID(r.Context()))

	if p.Status >= http.StatusInternalServerError {
		log.From(r.Context()).Error("request failed",
			slog.String("error", err.Error()),
			slog.String("kind", string(errs.KindOf(err))),
			slog.String("code", p.Code),
			slog.Int("status", p.Status),
			slog.String("path", r.URL.Path),
		)
	}

	// SPEC C4: any transient condition on the ingest path is a 503 with
	// Retry-After, never a 429 and never any other 4xx — Alertmanager permanently
	// drops an alert it saw a 4xx for.
	if retryAfterApplies(p.Status) && w.Header().Get("Retry-After") == "" {
		secs := p.RetryAfter
		if secs <= 0 {
			secs = int(DefaultRetryAfter.Seconds())
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}

	Write(w, r, p)
}

// Error is WriteProblem under its older name, kept so that every handler and
// middleware has one obvious call.
func Error(w http.ResponseWriter, r *http.Request, err error) { WriteProblem(w, r, err) }

// Unavailable writes a 503 with an explicit Retry-After. This is the only correct
// backpressure response on the ingest path (C4).
func Unavailable(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, message string) {
	WriteProblem(w, r, errs.Unavailable("unavailable", message, retryAfter))
}

// Write emits an already-built Problem.
func Write(w http.ResponseWriter, r *http.Request, p Problem) {
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		log.From(r.Context()).Error("httpx: encode problem", "error", err)
	}
}

func retryAfterApplies(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
}
