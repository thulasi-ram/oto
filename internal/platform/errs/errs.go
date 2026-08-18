package errs

import (
	"errors"
	"fmt"
	"time"
)

// Kind is the error taxonomy of SPEC §L.1. Every error that crosses a service
// boundary carries exactly one Kind, and a Kind is the only thing that decides an
// HTTP status. The distinguishing rules are binding:
//
//   - validation — the caller can fix it by changing the request.
//   - conflict — the caller must re-read and retry.
//   - precondition — the request is valid but the entity is in the wrong state
//     (acknowledging a resolved AlertCase, for example).
//   - upstream — nothing the caller did is wrong; a dead Alertmanager is never
//     the caller's fault.
//   - unavailable — oto's own backpressure. On the ingest path this is the ONLY
//     correct failure (C4): 503 with Retry-After, never 429, never any other 4xx,
//     because Alertmanager permanently deletes an alert it saw a 4xx for.
type Kind string

// The closed Kind set (SPEC §L.1). Adding one requires a SPEC amendment.
const (
	KindValidation   Kind = "validation_failed"      // 422 — well-formed, semantically invalid
	KindMalformed    Kind = "malformed_request"      // 400 — unparseable body / bad query param
	KindUnauthorized Kind = "unauthenticated"        // 401 — no or bad credential
	KindForbidden    Kind = "forbidden"              // 403 — cross-org access (v1: the only cause)
	KindNotFound     Kind = "not_found"              // 404
	KindConflict     Kind = "conflict"               // 409 — unique violation, concurrent update
	KindPrecondition Kind = "precondition_failed"    // 412 — illegal state transition
	KindTooLarge     Kind = "payload_too_large"      // 413
	KindUnsupported  Kind = "unsupported_media_type" // 415
	KindRateLimited  Kind = "rate_limited"           // 429 — READ API ONLY. NEVER on /ingest (C4).
	KindInternal     Kind = "internal_error"         // 500
	KindUpstreamDown Kind = "upstream_unavailable"   // 502 — Alertmanager/Prometheus/Slack failed
	KindUnavailable  Kind = "unavailable"            // 503 — our backpressure (ingest, pool exhausted)
	KindUpstreamSlow Kind = "upstream_timeout"       // 504
)

// Violation names ONE MEMBER OF THE REQUEST that a refusal is about — usually a
// body field on a KindValidation error, but also a query parameter on the
// KindMalformed refusals that can name one (see WithViolations).
//
// Field is a JSON-name path, '/'-separated, with array indices as numeric
// segments and map keys verbatim: matchers[0].name is reported as
// "matchers/0/name" (SPEC §L.2.2). Code comes from the closed tag→code map in
// §L.2.3. Message is human and always safe to render.
type Violation struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is oto's structured error. It carries the taxonomy Kind, a stable machine
// Code, a human Message that is always safe to show, the field Violations of a
// validation failure, retry guidance and the wrapped cause.
//
// The Message NEVER contains a secret, a raw upstream payload or a SQL string.
// The Cause is for logs; it is never rendered to a caller.
type Error struct {
	Kind       Kind
	Code       string
	Message    string
	Violations []Violation
	Retryable  bool
	RetryAfter time.Duration
	Cause      error
}

// Error implements the error interface.
func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.Code != "":
		return string(e.Kind) + "/" + e.Code + ": " + e.Message
	case e.Message != "":
		return string(e.Kind) + ": " + e.Message
	case e.Code != "":
		return string(e.Kind) + "/" + e.Code
	default:
		return string(e.Kind)
	}
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Is makes errors.Is work against a Kind sentinel: errors.Is(err, errs.ErrNotFound)
// is true for any not_found error. A target that also carries a Code matches only
// errors with that same Code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	if t.Kind != e.Kind {
		return false
	}
	return t.Code == "" || t.Code == e.Code
}

// Kind sentinels, for errors.Is. They carry no message and are never returned.
var (
	ErrValidation   error = &Error{Kind: KindValidation}
	ErrMalformed    error = &Error{Kind: KindMalformed}
	ErrUnauthorized error = &Error{Kind: KindUnauthorized}
	ErrForbidden    error = &Error{Kind: KindForbidden}
	ErrNotFound     error = &Error{Kind: KindNotFound}
	ErrConflict     error = &Error{Kind: KindConflict}
	ErrPrecondition error = &Error{Kind: KindPrecondition}
	ErrTooLarge     error = &Error{Kind: KindTooLarge}
	ErrUnsupported  error = &Error{Kind: KindUnsupported}
	ErrRateLimited  error = &Error{Kind: KindRateLimited}
	ErrInternal     error = &Error{Kind: KindInternal}
	ErrUpstreamDown error = &Error{Kind: KindUpstreamDown}
	ErrUnavailable  error = &Error{Kind: KindUnavailable}
	ErrUpstreamSlow error = &Error{Kind: KindUpstreamSlow}
)

// New builds an Error of the given Kind.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Newf builds an Error with a formatted message.
func Newf(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a Kind and a code to an existing error. A nil err returns nil, so
// Wrap is safe to use in a one-line return.
func Wrap(err error, kind Kind, code, message string) *Error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Code: code, Message: message, Cause: err}
}

// WithViolations returns e carrying the given field violations.
//
// Violations NAME A MEMBER OF THE REQUEST; they are not a property of a status
// code (SPEC §L.1). Every KindValidation error carries them. Another Kind may
// only when its refusal is about an identifiable member the caller can act on —
// `unknown_parameter` and `source_id_required` (KindMalformed) name the query
// parameter, `setting_managed_by_config` (KindConflict) names the setting. A
// refusal that is not about a member of the request — unauthenticated,
// forbidden, not found, precondition, and every 5xx — carries none.
func (e *Error) WithViolations(v ...Violation) *Error {
	e.Violations = append(e.Violations, v...)
	return e
}

// WithCause returns e carrying cause. The cause is logged, never rendered.
func (e *Error) WithCause(cause error) *Error {
	e.Cause = cause
	return e
}

// WithRetryAfter marks e retryable after d. On the ingest path this is what turns
// backpressure into a 503 + Retry-After rather than a lost alert (C4).
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	e.Retryable = true
	e.RetryAfter = d
	return e
}

// Validation reports a well-formed but semantically invalid request. The caller
// can fix it by changing what it sent.
func Validation(code, message string, violations ...Violation) *Error {
	return New(KindValidation, code, message).WithViolations(violations...)
}

// Malformed reports a body or query parameter that could not be parsed at all.
func Malformed(code, message string) *Error { return New(KindMalformed, code, message) }

// Unauthorized reports a missing or unusable credential.
func Unauthorized(code, message string) *Error { return New(KindUnauthorized, code, message) }

// Forbidden reports an authenticated principal acting outside its Org. In v1 that
// is the only cause (R2: no RBAC, no roles).
func Forbidden(code, message string) *Error { return New(KindForbidden, code, message) }

// NotFound reports that a resource does not exist within the caller's Org.
func NotFound(code, message string) *Error { return New(KindNotFound, code, message) }

// Conflict reports that the caller must re-read and retry: a unique violation on a
// key the user supplied, or a concurrent update. A unique violation on a key oto
// computed (alert_key, idempotency_key) is NOT an error — it is the idempotency
// mechanism, swallowed by ON CONFLICT (SPEC §L.1).
func Conflict(code, message string) *Error { return New(KindConflict, code, message) }

// Precondition reports that the request is valid but the entity is in the wrong
// state — an illegal AlertCase lifecycle transition, or acknowledging an
// case that has already resolved.
func Precondition(code, message string) *Error { return New(KindPrecondition, code, message) }

// TooLarge reports a body over the layer's hard bound. On the ingest path this is
// bound B1 (8 MiB) and is one of the only three permitted 4xx (SPEC §L.3.2).
func TooLarge(code, message string) *Error { return New(KindTooLarge, code, message) }

// Unsupported reports a media type oto will not decode.
func Unsupported(code, message string) *Error { return New(KindUnsupported, code, message) }

// RateLimited reports a read-API rate limit. It MUST NEVER be used on the ingest
// path: a 429 makes Alertmanager drop the alert permanently (C4).
func RateLimited(code, message string, retryAfter time.Duration) *Error {
	return New(KindRateLimited, code, message).WithRetryAfter(retryAfter)
}

// Internal reports an oto bug. Its cause is logged; the caller sees only the
// message. A DDL CHECK violation reaching this point means layers 1–3 have a hole.
func Internal(code string, cause error) *Error {
	return &Error{Kind: KindInternal, Code: code, Message: "an internal error occurred", Cause: cause}
}

// UpstreamDown reports that an AlertSource, Prometheus or a Channel provider
// failed. An upstream failure is never the caller's fault (SPEC §L.1).
func UpstreamDown(code, message string, cause error) *Error {
	return &Error{Kind: KindUpstreamDown, Code: code, Message: message, Cause: cause}
}

// UpstreamSlow reports that an upstream exceeded its timeout budget.
func UpstreamSlow(code, message string, cause error) *Error {
	return &Error{Kind: KindUpstreamSlow, Code: code, Message: message, Cause: cause}
}

// Unavailable reports oto's own backpressure — ingest overload, pool exhaustion, a
// slow Postgres. This is the only correct ingest failure (C4).
func Unavailable(code, message string, retryAfter time.Duration) *Error {
	return New(KindUnavailable, code, message).WithRetryAfter(retryAfter)
}

// As extracts the *Error from err, if there is one anywhere in its chain.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// KindOf reports the Kind of err, defaulting to KindInternal for an untyped error.
// An error that reaches the transport without a Kind is a bug, and 500 says so.
func KindOf(err error) Kind {
	if e, ok := As(err); ok {
		return e.Kind
	}
	return KindInternal
}

// IsKind reports whether err carries the given Kind.
func IsKind(err error, k Kind) bool { return KindOf(err) == k }

// CodeOf reports the stable machine code of err, or "" if it has none.
func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return ""
}

// ViolationsOf returns the field violations carried by err, if any.
func ViolationsOf(err error) []Violation {
	if e, ok := As(err); ok {
		return e.Violations
	}
	return nil
}

// RetryAfterOf returns the retry delay err advertises, and whether it is retryable.
func RetryAfterOf(err error) (time.Duration, bool) {
	if e, ok := As(err); ok {
		return e.RetryAfter, e.Retryable
	}
	return 0, false
}
