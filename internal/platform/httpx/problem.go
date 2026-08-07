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
const ContentTypeProblem = "application/problem+json"

// Problem is an RFC 9457 problem detail. This struct and Error below are the ONLY
// place in oto where an error becomes an HTTP status and a response body.
type Problem struct {
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Status    int               `json:"status"`
	Detail    string            `json:"detail,omitempty"`
	Instance  string            `json:"instance,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	Errors    []errs.FieldError `json:"errors,omitempty"`
}

// Error maps err onto a problem+json response and writes it.
//
// Internal errors are logged with the cause and rendered without it: an operator
// gets the detail from the log line correlated by request_id, a caller never does.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	code := errs.CodeOf(err)
	status := errs.StatusFor(code)

	p := Problem{
		Type:      errs.TypeURI(code),
		Title:     "Internal server error",
		Status:    status,
		Instance:  r.URL.Path,
		RequestID: log.RequestID(r.Context()),
	}

	if e, ok := errs.As(err); ok {
		p.Title = e.Title
		p.Errors = e.Fields
		if status < http.StatusInternalServerError {
			p.Detail = e.Detail
		}
	}

	if status >= http.StatusInternalServerError {
		log.From(r.Context()).Error("request failed",
			slog.String("error", err.Error()),
			slog.String("code", string(code)),
			slog.Int("status", status),
			slog.String("path", r.URL.Path),
		)
	}

	// SPEC C4: any transient condition on the ingest path is a 503 with Retry-After,
	// never a 429 and never any other 4xx. Alertmanager permanently drops 4xx alerts.
	if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", strconv.Itoa(int(DefaultRetryAfter.Seconds())))
	}

	Write(w, r, p)
}

// DefaultRetryAfter is the fallback Retry-After on a 503.
var DefaultRetryAfter = 10 * time.Second

// Unavailable writes a 503 with an explicit Retry-After. This is the only correct
// backpressure response on the ingest path.
func Unavailable(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	Error(w, r, errs.Unavailable(detail))
}

// Write emits an already-built Problem.
func Write(w http.ResponseWriter, r *http.Request, p Problem) {
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		log.From(r.Context()).Error("httpx: encode problem", "error", err)
	}
}
