package httpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The stable error-code suffixes this package produces. A client namespaces them
// with its own prefix (Config.ErrPrefix), so an Alertmanager DNS failure is
// `alertmanager_unreachable` and a Prometheus one is `prometheus_unreachable`.
//
// The split that matters operationally (and that SPEC §L.1 insists on) is
// "the source is DOWN" versus "the source answered with garbage". The first set
// means oto could not get an answer; the second means it got one it cannot use,
// which is almost always a misconfigured base URL pointing at a proxy, a login
// page or the wrong service. IsUnreachable and IsMalformed are the predicates.
const (
	// CodeInvalidBaseURL means the configured base URL is not an absolute
	// http(s) URL without a trailing slash (alert_sources_base_ck).
	CodeInvalidBaseURL = "invalid_base_url"
	// CodeInvalidConfig means the client was constructed with unusable options.
	CodeInvalidConfig = "invalid_config"
	// CodeInvalidRequest means oto could not even build the request. A bug.
	CodeInvalidRequest = "invalid_request"

	// CodeUnreachable means DNS, connect or read failed. The source is down.
	CodeUnreachable = "unreachable"
	// CodeTimeout means the attempt budget expired. The source is slow.
	CodeTimeout = "timeout"
	// CodeCanceled means the CALLER's context was cancelled. Not the upstream's
	// fault, so it is oto backpressure rather than an upstream failure.
	CodeCanceled = "canceled"
	// CodeTLS means the TLS handshake or certificate verification failed.
	CodeTLS = "tls_error"

	// CodeUnauthorized is a 401 from the upstream: the stored credential is
	// wrong, missing or revoked.
	CodeUnauthorized = "unauthorized"
	// CodeForbidden is a 403 from the upstream.
	CodeForbidden = "forbidden"
	// CodeNotFound is a 404. On a collection path it usually means the base URL
	// does not point at the service oto thinks it does.
	CodeNotFound = "not_found"
	// CodeGone is a 410. On Alertmanager this is the v1 API, removed in 0.27.0.
	CodeGone = "api_gone"
	// CodeRateLimited is a 429. It is NEVER retried in band (4xx are terminal
	// here); the Retry-After it advertises is attached to the error instead.
	CodeRateLimited = "rate_limited"
	// CodeRejected is any other 4xx: the upstream understood and refused.
	CodeRejected = "rejected"

	// CodeServerError is a 5xx that survived every retry.
	CodeServerError = "server_error"
	// CodeServerUnavailable is a 503 that survived every retry.
	CodeServerUnavailable = "server_unavailable"

	// CodeResponseTooLarge means the body exceeded Config.MaxResponseBytes.
	CodeResponseTooLarge = "response_too_large"
	// CodeMalformedResponse means a 2xx body that would not decode.
	CodeMalformedResponse = "malformed_response"
	// CodeUnexpectedContentType means a 2xx body that was not JSON at all --
	// the classic "an HTML login page from an authenticating proxy" failure.
	CodeUnexpectedContentType = "unexpected_content_type"
)

// unreachableCodes are the suffixes that mean "oto never got an answer".
var unreachableCodes = map[string]struct{}{
	CodeUnreachable:       {},
	CodeTimeout:           {},
	CodeTLS:               {},
	CodeServerError:       {},
	CodeServerUnavailable: {},
}

// malformedCodes are the suffixes that mean "oto got an answer it cannot use".
var malformedCodes = map[string]struct{}{
	CodeResponseTooLarge:      {},
	CodeMalformedResponse:     {},
	CodeUnexpectedContentType: {},
	CodeGone:                  {},
}

// Code renders the namespaced, stable error code for a suffix.
func Code(prefix, suffix string) string {
	if prefix == "" {
		return suffix
	}
	return prefix + "_" + suffix
}

// HasCode reports whether err carries the given code suffix under any prefix, so
// that a caller can ask "was this a timeout?" without knowing which upstream
// raised it.
func HasCode(err error, suffix string) bool {
	code := errs.CodeOf(err)
	return code != "" && (code == suffix || strings.HasSuffix(code, "_"+suffix))
}

// SuffixOf returns the unnamespaced code suffix carried by err, or "" when err
// carries no code this package minted.
func SuffixOf(err error) string {
	for suffix := range unreachableCodes {
		if HasCode(err, suffix) {
			return suffix
		}
	}
	for suffix := range malformedCodes {
		if HasCode(err, suffix) {
			return suffix
		}
	}
	for _, suffix := range []string{
		CodeCanceled, CodeUnauthorized, CodeForbidden, CodeNotFound, CodeRateLimited,
		CodeRejected, CodeInvalidBaseURL, CodeInvalidConfig, CodeInvalidRequest,
	} {
		if HasCode(err, suffix) {
			return suffix
		}
	}
	return ""
}

// IsUnreachable reports whether err means the upstream never answered — DNS,
// connect, TLS, timeout or a 5xx that outlived its retries. This is the
// distinction that decides source_health.status and, through it, whether the
// reaper is allowed to run (SPEC §B.4).
func IsUnreachable(err error) bool {
	for suffix := range unreachableCodes {
		if HasCode(err, suffix) {
			return true
		}
	}
	return false
}

// IsMalformed reports whether err means the upstream answered with something oto
// cannot use. A malformed source is NOT a down source: the reaper guard must not
// treat a proxy serving HTML as a dead Alertmanager, and an operator needs to be
// told to fix the URL rather than to restart the service.
func IsMalformed(err error) bool {
	for suffix := range malformedCodes {
		if HasCode(err, suffix) {
			return true
		}
	}
	return false
}

// bodyError carries a bounded, control-character-stripped snippet of an upstream
// body. It is a CAUSE, never a Message: SPEC §L.1 forbids a raw upstream payload
// from reaching a rendered problem+json body.
type bodyError struct {
	status  int
	snippet string
}

func (e *bodyError) Error() string {
	if e.snippet == "" {
		return "upstream body unavailable"
	}
	return "upstream body: " + e.snippet
}

// classifyTransport maps a RoundTrip failure onto a code suffix. The caller's
// context is inspected first: a cancelled parent context is oto's own decision,
// not an upstream fault.
func classifyTransport(parent context.Context, err error) string {
	switch {
	case parent.Err() != nil && errors.Is(parent.Err(), context.Canceled):
		return CodeCanceled
	case parent.Err() != nil && errors.Is(parent.Err(), context.DeadlineExceeded):
		return CodeTimeout
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	case errors.Is(err, context.Canceled):
		return CodeCanceled
	}

	if isTLSError(err) {
		return CodeTLS
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CodeTimeout
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return CodeTimeout
	}
	return CodeUnreachable
}

// isTLSError reports whether err came out of the TLS handshake. crypto/tls does
// not export one error type for this, so the check is structural for the x509
// verification failures that have types and textual for the rest.
func isTLSError(err error) bool {
	var (
		unknownAuthority x509.UnknownAuthorityError
		certInvalid      x509.CertificateInvalidError
		hostname         x509.HostnameError
		recordHeader     tls.RecordHeaderError
		certVerify       *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &unknownAuthority),
		errors.As(err, &certInvalid),
		errors.As(err, &hostname),
		errors.As(err, &recordHeader),
		errors.As(err, &certVerify):
		return true
	}

	msg := err.Error()
	for _, needle := range []string{"x509:", "tls:", "remote error: tls"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// kindFor maps a code suffix onto the errs.Kind taxonomy of SPEC §L.1.
func kindFor(suffix string) errs.Kind {
	switch suffix {
	case CodeInvalidBaseURL, CodeInvalidConfig:
		return errs.KindValidation
	case CodeInvalidRequest:
		return errs.KindInternal
	case CodeTimeout:
		return errs.KindUpstreamSlow
	case CodeCanceled:
		// The caller walked away. That is oto's own backpressure, not a fault of
		// the Alertmanager on the other end.
		return errs.KindUnavailable
	case CodeNotFound:
		return errs.KindNotFound
	default:
		return errs.KindUpstreamDown
	}
}

// messageFor is the human, always-safe-to-render half of an error. It never
// contains a secret, a URL with credentials or an upstream payload.
func messageFor(suffix string) string {
	switch suffix {
	case CodeInvalidBaseURL:
		return "the configured base URL must be an absolute http(s) URL with no trailing slash"
	case CodeInvalidConfig:
		return "the upstream client configuration is not usable"
	case CodeInvalidRequest:
		return "the upstream request could not be built"
	case CodeUnreachable:
		return "the upstream could not be reached"
	case CodeTimeout:
		return "the upstream did not answer within its timeout budget"
	case CodeCanceled:
		return "the request was cancelled before the upstream answered"
	case CodeTLS:
		return "the upstream TLS certificate could not be verified"
	case CodeUnauthorized:
		return "the upstream rejected oto's credential"
	case CodeForbidden:
		return "the upstream refused access with the configured credential"
	case CodeNotFound:
		return "the upstream has no such resource"
	case CodeGone:
		return "the upstream reports this API version as removed"
	case CodeRateLimited:
		return "the upstream is rate limiting oto"
	case CodeRejected:
		return "the upstream rejected the request"
	case CodeServerError:
		return "the upstream returned a server error"
	case CodeServerUnavailable:
		return "the upstream reported itself unavailable"
	case CodeResponseTooLarge:
		return "the upstream response exceeded oto's size cap"
	case CodeMalformedResponse:
		return "the upstream response could not be decoded"
	case CodeUnexpectedContentType:
		return "the upstream returned a non-JSON response"
	default:
		return "the upstream request failed"
	}
}

// newError builds the namespaced error for a suffix.
func newError(prefix, suffix string, cause error) *errs.Error {
	return &errs.Error{
		Kind:    kindFor(suffix),
		Code:    Code(prefix, suffix),
		Message: messageFor(suffix),
		Cause:   cause,
	}
}

// statusSuffix maps an HTTP status onto a code suffix.
func statusSuffix(status int) string {
	switch status {
	case 401:
		return CodeUnauthorized
	case 403:
		return CodeForbidden
	case 404:
		return CodeNotFound
	case 410:
		return CodeGone
	case 429:
		return CodeRateLimited
	case 503:
		return CodeServerUnavailable
	}
	switch {
	case status >= 500:
		return CodeServerError
	case status >= 400:
		return CodeRejected
	default:
		return CodeRejected
	}
}
