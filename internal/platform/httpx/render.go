package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/log"
)

// ContentTypeJSON is the media type of every non-error response body.
const ContentTypeJSON = "application/json; charset=utf-8"

// Meta accompanies every response envelope.
type Meta struct {
	RequestID string `json:"request_id,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// Page describes a keyset page. There is no total and no offset (SPEC §E.1).
type Page struct {
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
	Limit      int    `json:"limit"`
}

// Envelope is the single-resource response shape: {"data": {...}, "meta": {...}}.
type Envelope[T any] struct {
	Data T    `json:"data"`
	Meta Meta `json:"meta"`
}

// ListEnvelope is the collection response shape: {"data": [...], "page": {...}, "meta": {...}}.
type ListEnvelope[T any] struct {
	Data []T  `json:"data"`
	Page Page `json:"page"`
	Meta Meta `json:"meta"`
}

// JSON writes v as a JSON body with the given status.
//
// A 204 gets NO Content-Type. RFC 9110 §15.3.5 says a 204 has no content, and
// advertising a media type for a body that cannot exist invites a strict client
// to parse zero bytes as JSON and fail — the delete endpoints were shipping
// `204 + application/json` and every one of them was a trap waiting for a
// sufficiently literal HTTP library.
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// The status line is already on the wire; all we can do is record it.
		log.From(r.Context()).Error("httpx: encode response", "error", err)
	}
}

// Data writes a single-resource envelope.
func Data[T any](w http.ResponseWriter, r *http.Request, status int, v T, started time.Time) {
	JSON(w, r, status, Envelope[T]{Data: v, Meta: metaFor(r, started)})
}

// List writes a collection envelope. A nil slice is rendered as [], never null.
func List[T any](w http.ResponseWriter, r *http.Request, v []T, page Page, started time.Time) {
	if v == nil {
		v = []T{}
	}
	JSON(w, r, http.StatusOK, ListEnvelope[T]{Data: v, Page: page, Meta: metaFor(r, started)})
}

func metaFor(r *http.Request, started time.Time) Meta {
	m := Meta{RequestID: log.RequestID(r.Context())}
	if !started.IsZero() {
		m.ElapsedMS = time.Since(started).Milliseconds()
	}
	return m
}

// Decode reads a JSON request body into dst.
//
// This is the trusted-input policy of SPEC §L.2: unknown fields, empty bodies and
// trailing content are all rejected loudly, because a client that sends them has
// a bug. It is the exact opposite of the untrusted Alertmanager payload path
// (§L.3), which decodes leniently and never fails a batch on an unknown field.
func Decode(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt := strings.TrimSpace(strings.Split(ct, ";")[0]); mt != "application/json" {
			return errs.Newf(errs.KindUnsupported, "unsupported_media_type",
				"Content-Type must be application/json, got %q", mt)
		}
	}
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errs.Malformed("trailing_content", "body contains trailing JSON")
	}
	return nil
}

func decodeError(err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError

	switch {
	case errors.As(err, &syn):
		return errs.Newf(errs.KindMalformed, "malformed_json", "malformed JSON at byte %d", syn.Offset)
	case errors.As(err, &typ):
		return errs.Newf(errs.KindValidation, "validation_failed",
			"field %q must be of type %s", typ.Field, typ.Type).
			WithViolations(errs.Violation{
				Field:   typ.Field,
				Code:    "type",
				Message: fmt.Sprintf("%s must be of type %s", typ.Field, typ.Type),
			})
	case errors.As(err, &tooLarge):
		return errs.Newf(errs.KindTooLarge, "payload_too_large",
			"request body exceeds %d bytes", tooLarge.Limit)
	case errors.Is(err, io.EOF):
		return errs.Malformed("empty_body", "request body must not be empty")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return errs.Newf(errs.KindValidation, "validation_failed", "unknown field %q", field).
			WithViolations(errs.Violation{
				Field:   field,
				Code:    "unknown_field",
				Message: fmt.Sprintf("%s is not a known field", field),
			})
	default:
		return errs.Wrap(err, errs.KindMalformed, "malformed_json", "request body could not be decoded")
	}
}
