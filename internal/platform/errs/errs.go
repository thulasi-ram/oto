package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, typed error code. It is the only thing that decides an HTTP
// status and a problem+json "type" URI — that mapping lives here and nowhere else.
type Code string

// The v1 code set.
const (
	CodeInternal             Code = "internal"
	CodeNotFound             Code = "not_found"
	CodeConflict             Code = "conflict"
	CodeValidationFailed     Code = "validation_failed"
	CodeUnauthorized         Code = "unauthorized"
	CodeForbidden            Code = "forbidden"
	CodePayloadTooLarge      Code = "payload_too_large"
	CodeUnavailable          Code = "unavailable"
	CodeTimeout              Code = "timeout"
	CodeCursorFilterMismatch Code = "cursor_filter_mismatch"
	CodeUnprocessable        Code = "unprocessable"
	CodeRateLimited          Code = "rate_limited"
	CodeNotImplemented       Code = "not_implemented"
)

// TypeBase prefixes every problem+json "type" URI.
const TypeBase = "https://oto.dev/errors/"

// FieldError is one field-level validation failure.
type FieldError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// Error is oto's typed error. It wraps a cause and carries the code, a human
// title/detail and any field errors.
type Error struct {
	Code   Code
	Title  string
	Detail string
	Fields []FieldError
	cause  error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return string(e.Code)
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// New builds a typed error.
func New(code Code, detail string) *Error {
	return &Error{Code: code, Title: titleFor(code), Detail: detail}
}

// Newf builds a typed error with a formatted detail.
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap attaches a code to an existing error. A nil err returns nil.
func Wrap(code Code, err error, detail string) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Title: titleFor(code), Detail: detail, cause: err}
}

// WithFields attaches field-level validation failures.
func (e *Error) WithFields(fields ...FieldError) *Error {
	e.Fields = append(e.Fields, fields...)
	return e
}

// NotFound reports that a named resource does not exist.
func NotFound(what string) *Error { return Newf(CodeNotFound, "%s not found", what) }

// Conflict reports that the request conflicts with the current state.
func Conflict(detail string) *Error { return New(CodeConflict, detail) }

// Invalid reports a failed validation at the DTO boundary.
func Invalid(detail string) *Error { return New(CodeValidationFailed, detail) }

// Unauthorized reports a missing or unusable credential.
func Unauthorized(d string) *Error { return New(CodeUnauthorized, d) }

// Forbidden reports an authenticated principal acting outside its org.
func Forbidden(d string) *Error { return New(CodeForbidden, d) }

// Unavailable reports a transient condition. On the ingest path this is the ONLY
// correct failure: 503 with Retry-After, never 429 and never any other 4xx (C4).
func Unavailable(d string) *Error { return New(CodeUnavailable, d) }

// Internal wraps an unexpected error. Its cause is logged, never rendered.
func Internal(err error) *Error { return Wrap(CodeInternal, err, "an internal error occurred") }

// CodeOf extracts the code from err, defaulting to CodeInternal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// As extracts the *Error from err, if there is one.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// StatusFor maps a code onto its HTTP status. This is the single mapping table.
//
// Note: the ingest path does NOT use this table for transient failures — SPEC C4
// binds it to 503 + Retry-After, never 429 and never any other 4xx.
func StatusFor(code Code) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeValidationFailed, CodeCursorFilterMismatch:
		return http.StatusUnprocessableEntity
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeUnprocessable:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// TypeURI is the problem+json "type" for a code.
func TypeURI(code Code) string { return TypeBase + string(code) }

func titleFor(code Code) string {
	switch code {
	case CodeNotFound:
		return "Not found"
	case CodeConflict:
		return "Conflict"
	case CodeValidationFailed:
		return "Validation failed"
	case CodeUnauthorized:
		return "Unauthorized"
	case CodeForbidden:
		return "Forbidden"
	case CodePayloadTooLarge:
		return "Payload too large"
	case CodeUnavailable:
		return "Service unavailable"
	case CodeTimeout:
		return "Timeout"
	case CodeCursorFilterMismatch:
		return "Cursor does not match the current filters"
	case CodeUnprocessable:
		return "Unprocessable"
	case CodeRateLimited:
		return "Rate limited"
	case CodeNotImplemented:
		return "Not implemented"
	default:
		return "Internal server error"
	}
}
