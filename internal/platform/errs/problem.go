package errs

import (
	"math"
	"strconv"
	"time"
)

// TypeBase prefixes every problem+json "type" URI.
const TypeBase = "https://oto.dev/errors/"

// ContentTypeProblem is the RFC 9457 media type.
const ContentTypeProblem = "application/problem+json"

// ProblemDTO is the RFC 9457 problem detail oto renders for every failure. This
// shape is binding (SPEC §L.2.2) and is the only shape a 422 ever takes.
//
// The struct lives here, not in httpx, so that this package stays free of any
// transport import — internal/<domain>/domain packages import errs, and they may
// not depend on net/http (SPEC §L.4). httpx owns the write.
type ProblemDTO struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     int            `json:"status"`
	Detail     string         `json:"detail,omitempty"`
	Instance   string         `json:"instance,omitempty"`
	Code       string         `json:"code"`
	RequestID  string         `json:"request_id"`
	Violations []ViolationDTO `json:"violations,omitempty"`
	RetryAfter int            `json:"retry_after_seconds,omitempty"`
}

// ViolationDTO is one entry of the problem's violations[] array (SPEC §L.2.2).
type ViolationDTO struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HTTP status codes, spelled out rather than taken from net/http so that errs
// stays importable from a pure domain package.
const (
	statusBadRequest          = 400
	statusUnauthorized        = 401
	statusForbidden           = 403
	statusNotFound            = 404
	statusConflict            = 409
	statusPreconditionFailed  = 412
	statusPayloadTooLarge     = 413
	statusUnsupportedMedia    = 415
	statusUnprocessable       = 422
	statusTooManyRequests     = 429
	statusInternalServerError = 500
	statusBadGateway          = 502
	statusUnavailable         = 503
	statusGatewayTimeout      = 504
)

// HTTPStatus maps a Kind onto its HTTP status. This is the single mapping table
// in the codebase (SPEC §L.1); nothing else may decide a status from an error.
func HTTPStatus(k Kind) int {
	switch k {
	case KindValidation:
		return statusUnprocessable
	case KindMalformed:
		return statusBadRequest
	case KindUnauthorized:
		return statusUnauthorized
	case KindForbidden:
		return statusForbidden
	case KindNotFound:
		return statusNotFound
	case KindConflict:
		return statusConflict
	case KindPrecondition:
		return statusPreconditionFailed
	case KindTooLarge:
		return statusPayloadTooLarge
	case KindUnsupported:
		return statusUnsupportedMedia
	case KindRateLimited:
		return statusTooManyRequests
	case KindUpstreamDown:
		return statusBadGateway
	case KindUnavailable:
		return statusUnavailable
	case KindUpstreamSlow:
		return statusGatewayTimeout
	case KindInternal:
		return statusInternalServerError
	default:
		return statusInternalServerError
	}
}

// TypeURI is the problem+json "type" for a Kind.
func TypeURI(k Kind) string { return TypeBase + string(k) }

// Title is the human, stable one-liner rendered as the problem's "title".
func Title(k Kind) string {
	switch k {
	case KindValidation:
		return "Validation failed"
	case KindMalformed:
		return "Malformed request"
	case KindUnauthorized:
		return "Unauthenticated"
	case KindForbidden:
		return "Forbidden"
	case KindNotFound:
		return "Not found"
	case KindConflict:
		return "Conflict"
	case KindPrecondition:
		return "Precondition failed"
	case KindTooLarge:
		return "Payload too large"
	case KindUnsupported:
		return "Unsupported media type"
	case KindRateLimited:
		return "Rate limited"
	case KindUpstreamDown:
		return "Upstream unavailable"
	case KindUnavailable:
		return "Service unavailable"
	case KindUpstreamSlow:
		return "Upstream timeout"
	case KindInternal:
		return "Internal server error"
	default:
		return "Internal server error"
	}
}

// Problem renders err as an RFC 9457 problem detail.
//
// A 5xx never carries its detail to the caller: an operator reads the cause from
// the log line correlated by request_id, a caller never does.
func Problem(err error, instance, requestID string) ProblemDTO {
	kind := KindOf(err)
	status := HTTPStatus(kind)

	p := ProblemDTO{
		Type:      TypeURI(kind),
		Title:     Title(kind),
		Status:    status,
		Instance:  instance,
		Code:      string(kind),
		RequestID: requestID,
	}

	e, ok := As(err)
	if !ok {
		return p
	}
	if e.Code != "" {
		p.Code = e.Code
	}
	if status < statusInternalServerError {
		p.Detail = e.Message
	}
	if len(e.Violations) > 0 {
		p.Violations = make([]ViolationDTO, 0, len(e.Violations))
		for _, v := range e.Violations {
			p.Violations = append(p.Violations, ViolationDTO(v))
		}
		if p.Detail == "" {
			p.Detail = strconv.Itoa(len(e.Violations)) + " fields failed validation."
		}
	}
	p.RetryAfter = retryAfterSeconds(e.RetryAfter)
	return p
}

// retryAfterSeconds rounds a retry delay up to whole seconds, which is the only
// unit Retry-After has.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := math.Ceil(d.Seconds())
	if secs > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(secs)
}
