package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/thulasiram/oto/internal/platform/log"
)

// Job is what a Handler receives. It is a deliberate re-shaping of River's job
// row so that domain worker packages never import River: swapping the queue must
// not be a rewrite of every handler.
type Job[T river.JobArgs] struct {
	// ID is the queue's job id. Log it. It is the only handle an operator has.
	ID int64
	// Kind and Queue are the resolved routing of this execution.
	Kind  string
	Queue string
	// Attempt is 1 on the first execution. MaxAttempts is the ceiling from §G.6.
	Attempt     int
	MaxAttempts int
	// Args is the decoded, version-checked payload.
	Args T
	// CreatedAt is when the job was enqueued; ScheduledAt is when it became due.
	// The difference between ScheduledAt and now is queue latency; the difference
	// between CreatedAt and now is end-to-end lag. They are not the same number
	// and confusing them makes a backlog look like a slow handler.
	CreatedAt   time.Time
	ScheduledAt time.Time
	// EncodedArgs is the raw payload, preserved for the dead-letter.
	EncodedArgs []byte
}

// LastAttempt reports whether a failure now sends the job to the dead-letter.
func (j Job[T]) LastAttempt() bool { return j.Attempt >= j.MaxAttempts }

// Handler is the business logic of one job type. This is THE SEAM: platform/jobs
// owns scheduling, retries, metrics and logging; a domain's `worker` package owns
// what the job actually does, and is injected here.
//
// A Handler must be idempotent (SPEC §G.5), must respect ctx cancellation so the
// runtime can drain, and should return a typed errs error so Classify can decide
// whether a retry is worth anything.
type Handler[T river.JobArgs] func(ctx context.Context, job *Job[T]) error

// Snooze defers this execution by d WITHOUT consuming an attempt.
//
// This is the ordering gate's verb (SPEC §G.7): "an earlier delivery is still in
// flight" is not a failure and must not erode the retry budget. It is also §H.9's
// answer to a Slack rate limit.
//
// reason is recorded on oto_jobs_snoozed_total. Snoozing without a reason makes a
// wedged thread indistinguishable from a healthy one, so it is a required argument.
func Snooze(d time.Duration, reason string) error {
	return &snoozeError{d: d, reason: reason}
}

type snoozeError struct {
	d      time.Duration
	reason string
}

func (e *snoozeError) Error() string {
	return fmt.Sprintf("snooze %s: %s", e.d, e.reason)
}

// worker adapts a Handler to River, and is where every cross-cutting concern of
// the runtime lives: version gating, panic recovery, timing, metrics, logging and
// the terminal/dead-letter decision.
type worker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	rt   *Runtime
	spec Spec
	fn   Handler[T]
}

// Timeout applies the per-spec job timeout, falling back to the client default.
func (w *worker[T]) Timeout(*river.Job[T]) time.Duration { return w.spec.Timeout }

// Work executes one job. It never panics out and never returns an untyped error.
func (w *worker[T]) Work(ctx context.Context, rj *river.Job[T]) (err error) {
	kind, queue := rj.Kind, rj.Queue
	m := w.rt.Metrics

	logger := w.rt.Logger.With(
		"job_id", rj.ID,
		"job_kind", kind,
		"queue", queue,
		"attempt", rj.Attempt,
		"max_attempts", rj.MaxAttempts,
	)
	ctx = log.Into(ctx, logger)

	// ---- payload version gate (SPEC §G.3) -------------------------------
	// Checked BEFORE the handler runs and before any timing, because a payload
	// this build cannot interpret must not be half-processed. Park it: a newer
	// pod will work it, and if none exists the metric says so. Never guess.
	if v, ok := any(rj.Args).(versioned); ok {
		if got := v.PayloadVersion(); got > w.spec.PayloadVersion {
			m.UnknownVersion.WithLabelValues(kind, queue).Inc()
			m.Dead.WithLabelValues(kind, queue, string(ClassPermanent)).Inc()
			logger.ErrorContext(ctx, "jobs: parking job with unknown payload version",
				"payload_version", got, "supported_version", w.spec.PayloadVersion)
			w.rt.DeadLetter.Dead(ctx, DeadJob{
				ID: rj.ID, Kind: kind, Queue: queue,
				Attempt: rj.Attempt, Attempts: rj.MaxAttempts,
				Class:   ClassPermanent,
				Reason:  DeadUnknownPayloadVersion,
				Payload: json.RawMessage(rj.EncodedArgs),
				OccurAt: w.rt.Clock.Now(),
			})
			return river.JobCancel(fmt.Errorf(
				"jobs: payload version %d exceeds supported %d for %s", got, w.spec.PayloadVersion, kind))
		}
	}

	job := &Job[T]{
		ID: rj.ID, Kind: kind, Queue: queue,
		Attempt: rj.Attempt, MaxAttempts: rj.MaxAttempts,
		Args:        rj.Args,
		CreatedAt:   rj.CreatedAt,
		ScheduledAt: rj.ScheduledAt,
		EncodedArgs: rj.EncodedArgs,
	}

	m.Started.WithLabelValues(kind, queue).Inc()
	started := w.rt.Clock.Now()
	logger.DebugContext(ctx, "jobs: job started")

	// ---- panic recovery --------------------------------------------------
	// River recovers panics itself, but it cannot tell us WHICH handler blew up
	// in our own metric namespace, and it cannot decide oto's retry semantics.
	// A recovered panic is converted to a retryable error on purpose: a transient
	// nil dereference during a rolling deploy should not discard an alert, and a
	// genuine one hits the attempt ceiling within minutes and goes dead loudly.
	handlerErr := func() (herr error) {
		defer func() {
			if p := recover(); p != nil {
				m.Panics.WithLabelValues(kind, queue).Inc()
				logger.ErrorContext(ctx, "jobs: panic in job handler",
					"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				herr = fmt.Errorf("jobs: panic in %s: %v", kind, p)
			}
		}()
		return w.fn(ctx, job)
	}()

	elapsed := w.rt.Clock.Since(started)
	return w.finish(ctx, logger, job, handlerErr, elapsed)
}

// finish records the outcome and maps the handler's error onto River's vocabulary.
func (w *worker[T]) finish(
	ctx context.Context,
	logger *slog.Logger,
	job *Job[T],
	herr error,
	elapsed time.Duration,
) error {
	m := w.rt.Metrics
	kind, queue := job.Kind, job.Queue

	if herr == nil {
		m.Succeeded.WithLabelValues(kind, queue).Inc()
		m.Duration.WithLabelValues(kind, queue, "succeeded").Observe(elapsed.Seconds())
		logger.InfoContext(ctx, "jobs: job succeeded", "duration_ms", elapsed.Milliseconds())
		return nil
	}

	// A snooze is not a failure. It consumes no attempt and must not be counted
	// as one, or a busy thread looks like a broken one.
	var sn *snoozeError
	if errors.As(herr, &sn) {
		m.Snoozed.WithLabelValues(kind, queue, sn.reason).Inc()
		m.Duration.WithLabelValues(kind, queue, "snoozed").Observe(elapsed.Seconds())
		logger.DebugContext(ctx, "jobs: job snoozed",
			"snooze_for_ms", sn.d.Milliseconds(), "reason", sn.reason)
		return river.JobSnooze(sn.d)
	}

	class := Classify(herr)
	m.Failed.WithLabelValues(kind, queue, string(class)).Inc()
	m.Duration.WithLabelValues(kind, queue, "failed").Observe(elapsed.Seconds())

	switch {
	case class == ClassRateLimited:
		// §G.6 / §H.9: honour Retry-After exactly, and do it as a snooze so a
		// throttled channel does not burn its way to dead while Slack is merely
		// asking us to slow down. The 20-attempt ceiling still applies at the
		// delivery level, which is where it belongs.
		d := RetryAfter(herr)
		m.Snoozed.WithLabelValues(kind, queue, "rate_limited").Inc()
		logger.WarnContext(ctx, "jobs: rate limited, honouring retry-after",
			"retry_after_ms", d.Milliseconds(), "error", herr.Error())
		return river.JobSnooze(d)

	case class.Terminal():
		// SPEC §G.6: permanent, config_invalid and auth_expired NEVER retry.
		// Cancel rather than error, so River finalises the job immediately with
		// its payload intact rather than scheduling twelve pointless attempts.
		m.Dead.WithLabelValues(kind, queue, string(class)).Inc()
		logger.ErrorContext(ctx, "jobs: terminal error, job is dead",
			"error_class", string(class), "error", herr.Error(),
			"duration_ms", elapsed.Milliseconds())
		w.rt.DeadLetter.Dead(ctx, DeadJob{
			ID: job.ID, Kind: kind, Queue: queue,
			Attempt: job.Attempt, Attempts: job.MaxAttempts,
			Class:   class,
			Err:     herr,
			Reason:  DeadTerminalError,
			Payload: json.RawMessage(job.EncodedArgs),
			OccurAt: w.rt.Clock.Now(),
		})
		return river.JobCancel(herr)

	case job.LastAttempt():
		// The retry budget is spent. River will discard it; record it as dead
		// here so the payload is captured while we still hold it.
		m.Dead.WithLabelValues(kind, queue, string(class)).Inc()
		logger.ErrorContext(ctx, "jobs: attempts exhausted, job is dead",
			"error_class", string(class), "error", herr.Error())
		w.rt.DeadLetter.Dead(ctx, DeadJob{
			ID: job.ID, Kind: kind, Queue: queue,
			Attempt: job.Attempt, Attempts: job.MaxAttempts,
			Class:   class,
			Err:     herr,
			Reason:  DeadAttemptsExhausted,
			Payload: json.RawMessage(job.EncodedArgs),
			OccurAt: w.rt.Clock.Now(),
		})
		return herr

	default:
		logger.WarnContext(ctx, "jobs: job failed, will retry",
			"error_class", string(class), "error", herr.Error(),
			"duration_ms", elapsed.Milliseconds())
		return herr
	}
}

// errorHandler feeds River's own terminal paths into oto's metrics, so a job
// killed by the rescuer or by an unhandled panic below our recover still shows up.
type errorHandler struct{ rt *Runtime }

// HandleError records a job error River saw at a layer above the worker.
func (h *errorHandler) HandleError(ctx context.Context, job *rivertype.JobRow, err error) *river.ErrorHandlerResult {
	if job.Attempt >= job.MaxAttempts {
		h.rt.Logger.ErrorContext(ctx, "jobs: river finalised a failing job",
			"job_id", job.ID, "job_kind", job.Kind, "queue", job.Queue,
			"attempt", job.Attempt, "error", err.Error())
	}
	return nil
}

// HandlePanic records a panic River caught outside the worker's own recover.
func (h *errorHandler) HandlePanic(ctx context.Context, job *rivertype.JobRow, panicVal any, trace string) *river.ErrorHandlerResult {
	h.rt.Metrics.Panics.WithLabelValues(job.Kind, job.Queue).Inc()
	h.rt.Logger.ErrorContext(ctx, "jobs: panic escaped the worker",
		"job_id", job.ID, "job_kind", job.Kind, "queue", job.Queue,
		"panic", fmt.Sprint(panicVal), "stack", trace)
	return nil
}
