package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// DeadJob is one job that will never run again, captured with its payload intact.
//
// The payload is preserved verbatim and deliberately: the whole reason a terminal
// error is not simply dropped is that the payload is the only record of what oto
// was asked to do. Losing it turns "this delivery failed" into "nothing happened",
// and oto's silence must never be indistinguishable from "no alert".
type DeadJob struct {
	ID       int64
	Kind     string
	Queue    string
	Attempt  int
	Class    ErrorClass
	Err      error
	Payload  json.RawMessage
	OccurAt  time.Time
	Reason   DeadReason
	Attempts int
}

// DeadReason distinguishes the two ways a job dies.
type DeadReason string

// The closed DeadReason set.
const (
	// DeadTerminalError is a permanent, config_invalid or auth_expired failure.
	// The job never retried at all (SPEC §G.6).
	DeadTerminalError DeadReason = "terminal_error"
	// DeadAttemptsExhausted is the retry ceiling being reached.
	DeadAttemptsExhausted DeadReason = "attempts_exhausted"
	// DeadUnknownPayloadVersion is a payload this build cannot interpret. The job
	// is parked rather than guessed at (SPEC §G.3).
	DeadUnknownPayloadVersion DeadReason = "unknown_payload_version"
)

// DeadLetter receives every job that will never run again.
//
// The default implementation logs; a later phase may persist. Whatever it does it
// MUST NOT return an error that stops the worker: the job is already lost, and
// failing the dead-letter write would retry a job that must never be retried.
type DeadLetter interface {
	Dead(ctx context.Context, j DeadJob)
}

// LogDeadLetter is the default sink: one ERROR record per dead job, carrying the
// full payload.
//
// Logging the payload at error level is a deliberate exception to "never log full
// payloads": this is the last copy, and it has already been through the source's
// redaction rules on the way in.
type LogDeadLetter struct {
	Logger *slog.Logger
}

// Dead records j.
func (l LogDeadLetter) Dead(ctx context.Context, j DeadJob) {
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}
	attrs := []any{
		"job_id", j.ID,
		"job_kind", j.Kind,
		"queue", j.Queue,
		"attempt", j.Attempt,
		"attempts", j.Attempts,
		"error_class", string(j.Class),
		"dead_reason", string(j.Reason),
		"payload", string(j.Payload),
	}
	if j.Err != nil {
		attrs = append(attrs, "error", j.Err.Error())
	}
	log.ErrorContext(ctx, "jobs: job is dead, it will never run again", attrs...)
}

// deadLetterOrDefault returns dl, or a logging sink.
func deadLetterOrDefault(dl DeadLetter, log *slog.Logger) DeadLetter {
	if dl != nil {
		return dl
	}
	return LogDeadLetter{Logger: log}
}
