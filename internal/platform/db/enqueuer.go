package db

import (
	"context"
	"time"
)

// JobArgs is the payload of one enqueued job.
//
// Kind is the durable, stable discriminator that survives a Go type rename; it is
// the wire contract between the enqueuer and the worker and MUST NOT change once
// jobs of that kind exist in the database (SPEC §G.3).
//
// Every payload additionally carries an integer version under the JSON key "v".
// A worker that meets a version it does not understand parks the job rather than
// guessing (SPEC §G.3). platform/jobs supplies the embeddable that provides it.
//
// NOTE (SPEC gap): §F.5 names `db.Enqueuer` but never declares JobArgs or
// JobOption. This declaration is the minimal definition; the method set is
// deliberately identical to river.JobArgs so that a single struct satisfies both
// and no adapter type is needed.
type JobArgs interface {
	// Kind uniquely identifies the job type, e.g. "ingest.process_batch".
	Kind() string
}

// JobOptions are the per-insert overrides a caller may apply. The zero value
// means "use whatever the job type declares for itself", which is the normal
// case: queue and priority are properties of the job type, not of the call site.
type JobOptions struct {
	// Queue overrides the destination queue. Empty means the job type's own.
	Queue string
	// Priority is 1 (highest) to 4 (lowest). Zero means the job type's own.
	Priority int
	// MaxAttempts overrides the retry ceiling. Zero means the job type's own.
	MaxAttempts int
	// ScheduledAt delays the job. The zero time means "as soon as possible".
	ScheduledAt time.Time
	// Tags are free-form labels for operator filtering. They never affect execution.
	Tags []string
	// UniquePeriod, when non-zero, asks the queue to collapse jobs with identical
	// args and kind inside a rolling window. This is a CONVENIENCE, never the
	// correctness mechanism: idempotency in oto is owned by database constraints
	// (SPEC §G.5), because a job may be enqueued by a pod that then dies.
	UniquePeriod time.Duration
}

// JobOption mutates JobOptions. Options compose; the last one wins.
type JobOption func(*JobOptions)

// WithQueue routes this insert to a named queue.
func WithQueue(queue string) JobOption { return func(o *JobOptions) { o.Queue = queue } }

// WithPriority sets the insert priority, 1 (highest) to 4 (lowest).
func WithPriority(p int) JobOption { return func(o *JobOptions) { o.Priority = p } }

// WithMaxAttempts overrides the retry ceiling for this insert.
func WithMaxAttempts(n int) JobOption { return func(o *JobOptions) { o.MaxAttempts = n } }

// WithScheduledAt delays the job until t.
func WithScheduledAt(t time.Time) JobOption { return func(o *JobOptions) { o.ScheduledAt = t } }

// WithTags attaches operator-facing tags.
func WithTags(tags ...string) JobOption { return func(o *JobOptions) { o.Tags = tags } }

// WithUniquePeriod collapses identical jobs of this kind inside a rolling window.
func WithUniquePeriod(d time.Duration) JobOption {
	return func(o *JobOptions) { o.UniquePeriod = d }
}

// ApplyJobOptions folds opts onto a base. It is exported for enqueuer
// implementations; callers use the With… helpers.
func ApplyJobOptions(base JobOptions, opts ...JobOption) JobOptions {
	for _, o := range opts {
		if o != nil {
			o(&base)
		}
	}
	return base
}

// JobRequest is one entry of a batch insert.
type JobRequest struct {
	Args JobArgs
	Opts []JobOption
}

// EnqueueResult is what the queue recorded for one insert.
type EnqueueResult struct {
	// ID is the queue's own job id, for correlating logs with the job table.
	ID int64
	// Kind and Queue are the resolved destination.
	Kind  string
	Queue string
	// Skipped reports that a uniqueness rule collapsed this insert onto an
	// existing job. It is a normal outcome, never an error.
	Skipped bool
}

// Enqueuer is the port every service depends on to schedule asynchronous work
// (SPEC §F.5, §G.3). Nothing outside internal/platform/jobs may name River.
//
// THE TRANSACTIONAL CONTRACT, which is the entire point of this interface:
// Enqueue joins the transaction travelling in ctx (db.FromContext). A job is
// therefore enqueued in the SAME transaction as the state change that justifies
// it, and the two commit or roll back together. This is oto's transactional
// outbox — ADR 0001 — and it is why "202 Accepted is a promise" is true
// (SPEC §G.1). Calling Enqueue outside a transaction is legal and does a plain
// insert; it is simply not an outbox, so do not do it for anything that must
// agree with a row.
type Enqueuer interface {
	// Enqueue inserts one job, joining the caller's transaction if there is one.
	Enqueue(ctx context.Context, args JobArgs, opts ...JobOption) (EnqueueResult, error)

	// EnqueueMany inserts a batch in one round trip, joining the caller's
	// transaction if there is one. A 200-alert webhook must not become 200
	// inserts (SPEC §G.4).
	EnqueueMany(ctx context.Context, reqs []JobRequest) ([]EnqueueResult, error)
}
