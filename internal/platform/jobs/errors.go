package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ErrorClass is the SPEC §G.6 retry taxonomy. It is the same closed set the
// schema stores in `notification_deliveries.error_class`
// (deliveries_errclass_ck), because an operator looking at a dead delivery and an
// operator looking at a dead job must be reading the same word.
type ErrorClass string

// The closed ErrorClass set (SPEC §G.6).
const (
	// ClassRetryable retries with exponential backoff, 12 attempts, then dead.
	ClassRetryable ErrorClass = "retryable"
	// ClassRateLimited honours Retry-After exactly, 20 attempts, then dead.
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassPermanent never retries. Dead immediately, payload preserved.
	ClassPermanent ErrorClass = "permanent"
	// ClassConfigInvalid never retries. Dead, and sets the channel's health to
	// config_invalid with a UI banner.
	ClassConfigInvalid ErrorClass = "config_invalid"
	// ClassAuthExpired never retries. Dead, and sets the channel's health to
	// auth_failed with a UI banner.
	//
	// config_invalid and auth_expired exist as separate classes on purpose: they
	// are the difference between "Slack is flaky" and "your token was revoked
	// three days ago and nobody noticed", and the second is a product feature.
	ClassAuthExpired ErrorClass = "auth_expired"
)

// Terminal reports whether a class must never be retried. A terminal error goes
// to the dead-letter with its payload preserved, immediately.
func (c ErrorClass) Terminal() bool {
	switch c {
	case ClassPermanent, ClassConfigInvalid, ClassAuthExpired:
		return true
	case ClassRetryable, ClassRateLimited:
		return false
	default:
		return false
	}
}

// classifiedError carries an explicit ErrorClass that overrides what the errs
// taxonomy would infer.
//
// It exists because §G.6's config_invalid and auth_expired are provider
// judgements — "Slack said token_revoked" — that no HTTP-status-shaped taxonomy
// can derive. A handler that knows makes it explicit; everything else is inferred.
type classifiedError struct {
	class ErrorClass
	err   error
}

func (e *classifiedError) Error() string { return string(e.class) + ": " + e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// WithClass tags err with an explicit ErrorClass. A nil err returns nil.
func WithClass(err error, class ErrorClass) error {
	if err == nil {
		return nil
	}
	return &classifiedError{class: class, err: err}
}

// Permanent marks err terminal: no retry, straight to the dead-letter.
func Permanent(err error) error { return WithClass(err, ClassPermanent) }

// ConfigInvalid marks err terminal because the channel's configuration is wrong.
func ConfigInvalid(err error) error { return WithClass(err, ClassConfigInvalid) }

// AuthExpired marks err terminal because the credential is no longer usable.
func AuthExpired(err error) error { return WithClass(err, ClassAuthExpired) }

// Classify maps an error onto its ErrorClass.
//
// An explicit tag wins. Otherwise the mapping is from errs.Kind, and it follows
// one rule: retry only what a retry could plausibly fix.
//
//   - unavailable / upstream_unavailable / upstream_timeout / conflict —
//     retryable. A dead Alertmanager, a saturated pool and a lost optimistic-lock
//     race all resolve on their own.
//   - rate_limited — its own class, so it gets Retry-After rather than backoff.
//   - validation / malformed / not_found / precondition / forbidden /
//     unauthenticated / payload_too_large / unsupported_media_type — permanent.
//     A malformed payload is malformed on the thirteenth attempt too, and
//     retrying it burns a worker slot that a real alert needs.
//   - internal — retryable. An oto bug MIGHT be a transient nil dereference on
//     one code path; the attempt ceiling turns a genuine one into a dead job
//     within minutes, which is loud enough.
//
// A context cancellation is retryable and is never the job's fault: it means the
// process is draining.
func Classify(err error) ErrorClass {
	if err == nil {
		return ClassRetryable
	}

	var ce *classifiedError
	if errors.As(err, &ce) {
		return ce.class
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassRetryable
	}

	switch errs.KindOf(err) {
	case errs.KindRateLimited:
		return ClassRateLimited
	case errs.KindValidation, errs.KindMalformed, errs.KindNotFound,
		errs.KindPrecondition, errs.KindForbidden, errs.KindUnauthorized,
		errs.KindTooLarge, errs.KindUnsupported:
		return ClassPermanent
	case errs.KindConflict, errs.KindInternal, errs.KindUnavailable,
		errs.KindUpstreamDown, errs.KindUpstreamSlow:
		return ClassRetryable
	default:
		return ClassRetryable
	}
}

// DefaultRateLimitDelay is the wait applied to a rate_limited error that carries
// no Retry-After (SPEC §G.6).
const DefaultRateLimitDelay = 60 * time.Second

// RetryAfter returns the delay a rate-limited error asks for, honouring an
// explicit Retry-After exactly and falling back to DefaultRateLimitDelay.
//
// "Honoured exactly" is binding: guessing shorter than the provider asked is how
// a soft rate limit becomes a hard block.
func RetryAfter(err error) time.Duration {
	if d, ok := errs.RetryAfterOf(err); ok && d > 0 {
		return d
	}
	return DefaultRateLimitDelay
}

// ErrNotImplemented is what every stub handler returns. It is deliberately
// KindInternal — and therefore retryable — so that a job enqueued before its
// handler exists is retried rather than silently discarded, and shows up loudly
// in oto_jobs_failed_total until somebody fills the seam in.
func ErrNotImplemented(kind string) error {
	return errs.Internal("not_implemented", errors.New("jobs: handler for "+kind+" is not implemented"))
}
