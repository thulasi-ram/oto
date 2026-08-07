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
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	if v == nil || status == http.StatusNoContent {
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

// Decode reads a JSON request body into dst. It rejects unknown fields, empty
// bodies and trailing content, and returns typed validation errors so the caller
// never has to translate encoding/json's messages itself.
func Decode(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt := strings.TrimSpace(strings.Split(ct, ";")[0]); mt != "application/json" {
			return errs.Newf(errs.CodeValidationFailed, "Content-Type must be application/json, got %q", mt)
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
		return errs.New(errs.CodeValidationFailed, "body must contain exactly one JSON value")
	}
	return nil
}

func decodeError(err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError

	switch {
	case errors.As(err, &syn):
		return errs.Newf(errs.CodeValidationFailed, "malformed JSON at byte %d", syn.Offset)
	case errors.As(err, &typ):
		return errs.Newf(errs.CodeValidationFailed, "field %q must be of type %s", typ.Field, typ.Type).
			WithFields(errs.FieldError{Field: typ.Field, Code: "type"})
	case errors.As(err, &tooLarge):
		return errs.Newf(errs.CodePayloadTooLarge, "request body exceeds %d bytes", tooLarge.Limit)
	case errors.Is(err, io.EOF):
		return errs.New(errs.CodeValidationFailed, "request body must not be empty")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return errs.Newf(errs.CodeValidationFailed, "unknown field %q", field).
			WithFields(errs.FieldError{Field: field, Code: "unknown"})
	default:
		return errs.Wrap(errs.CodeValidationFailed, err, fmt.Sprintf("cannot decode body: %v", err))
	}
}
