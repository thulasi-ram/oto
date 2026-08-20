package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/riverqueue/river"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// Runtime is everything a worker needs that is not the job itself. It is passed
// once at construction and never reached for globally, which is what makes the
// whole runtime testable with a fake clock and a throwaway registry.
type Runtime struct {
	Logger     *slog.Logger
	Clock      clock.Clock
	Metrics    *Metrics
	DeadLetter DeadLetter
}

func (rt *Runtime) normalise() {
	if rt.Logger == nil {
		rt.Logger = slog.Default()
	}
	if rt.Clock == nil {
		rt.Clock = clock.New()
	}
	if rt.Metrics == nil {
		rt.Metrics = NewMetrics(nil)
	}
	rt.DeadLetter = deadLetterOrDefault(rt.DeadLetter, rt.Logger)
}

// Spec is the registration-time description of one job type: everything the
// runtime needs to know that is not the payload.
//
// It duplicates what the args struct's InsertOpts already says about queue and
// priority, on purpose — the spec is the WORKER side and the InsertOpts are the
// PRODUCER side, and they are read by different people at different times.
type Spec struct {
	// Kind is the durable job kind. Required.
	Kind string
	// Queue is the queue this kind is worked on. Informational at registration
	// time; the queue actually worked comes from the insert.
	Queue string
	// PayloadVersion is the highest payload version this build understands.
	// A job carrying a higher one is PARKED, never guessed at (SPEC §G.3).
	PayloadVersion int
	// Timeout bounds one execution. Zero inherits the client-level timeout.
	Timeout time.Duration
}

// Registry collects the typed workers and the periodic schedule before a Client
// is built. It is write-only until the Client takes it.
type Registry struct {
	rt       *Runtime
	workers  *river.Workers
	specs    map[string]Spec
	periodic []*river.PeriodicJob
}

// NewRegistry builds an empty registry over rt.
func NewRegistry(rt *Runtime) *Registry {
	if rt == nil {
		rt = &Runtime{}
	}
	rt.normalise()
	return &Registry{
		rt:      rt,
		workers: river.NewWorkers(),
		specs:   map[string]Spec{},
	}
}

// Runtime exposes the runtime the registry was built over.
func (r *Registry) Runtime() *Runtime { return r.rt }

// Specs returns every registered spec, ordered by kind.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// AddPeriodic adds a periodic job to the schedule. River elects one leader across
// the deployment, so a periodic job fires once per interval per DEPLOYMENT rather
// than once per pod.
func (r *Registry) AddPeriodic(j *river.PeriodicJob) { r.periodic = append(r.periodic, j) }

// Register binds a Handler to a job type.
//
// It is a free function rather than a method because Go does not permit type
// parameters on methods. T must be the args struct, which supplies the kind.
func Register[T river.JobArgs](r *Registry, spec Spec, fn Handler[T]) error {
	var zero T
	kind := zero.Kind()

	switch {
	case fn == nil:
		return fmt.Errorf("jobs: nil handler for %q", kind)
	case spec.Kind != "" && spec.Kind != kind:
		return fmt.Errorf("jobs: spec kind %q does not match args kind %q", spec.Kind, kind)
	}

	spec.Kind = kind
	if spec.PayloadVersion <= 0 {
		spec.PayloadVersion = 1
	}

	if err := river.AddWorkerSafely(r.workers, &worker[T]{rt: r.rt, spec: spec, fn: fn}); err != nil {
		return fmt.Errorf("jobs: register %q: %w", kind, err)
	}
	r.specs[kind] = spec
	return nil
}

// Handlers is the complete set of job handlers oto runs (SPEC §G.3).
//
// THIS IS THE SEAM. A nil field is registered as a stub returning
// errs.Internal("not implemented"), so the queue, the retry policy, the metrics
// and the schedule are all live and observable before any business logic exists.
// A domain fills in its own field from internal/app; nothing else changes.
type Handlers struct {
	IngestProcessBatch Handler[IngestProcessBatchArgs]
	EnrichRun          Handler[EnrichRunArgs]
	NotifyEvaluate     Handler[NotifyEvaluateArgs]
	DeliverDispatch    Handler[DeliverDispatchArgs]
	SourceReconcile    Handler[SourceReconcileArgs]
	SilencesSync       Handler[SilencesSyncArgs]
	SlackInteraction   Handler[SlackInteractionArgs]

	CaseReap     Handler[CaseReapArgs]
	NotifyDigest Handler[NotifyDigestArgs]

	PartitionsManage Handler[PartitionsManageArgs]
	RetentionPrune   Handler[RetentionPruneArgs]
	StatsRollup      Handler[StatsRollupArgs]
	CacheExpire      Handler[CacheExpireArgs]
}

// stub returns the not-implemented handler for a kind.
func stub[T river.JobArgs](kind string) Handler[T] {
	return func(context.Context, *Job[T]) error { return ErrNotImplemented(kind) }
}

// orStub returns fn, or the stub for kind when fn is nil.
func orStub[T river.JobArgs](fn Handler[T], kind string) Handler[T] {
	if fn != nil {
		return fn
	}
	return stub[T](kind)
}

// RegisterAll registers every job type in SPEC §G.3 onto r.
//
// Timeouts are per-kind rather than global because the shapes differ by two
// orders of magnitude: a delivery that has not answered in 30 s is hung, whereas
// a partition sweep legitimately takes minutes.
//
// ⭐ FOR THE PER-TENANT PERIODICS THE TIMEOUT IS NOW PER TENANT, and that is the
// whole of what 2d699d6 bought. `case.reap`'s two minutes used to be two
// minutes for EVERY tenant put together, so the number below meant something
// different on every install and got quietly tighter with each customer. It now
// bounds one tenant's sweep, which is a quantity somebody can reason about, and
// the fan-out tick that expands into those jobs does a page read and one batch
// insert well inside the same budget. See jobs.TenantFanOut.
//
// `source.reconcile`'s sixty seconds is now the same kind of number, across all
// three of its shapes: it bounds one tenant page plus one batch insert, or one
// tenant's `ListDue` and the enqueues that follow it, or one source's pass — and no
// longer the whole customer base's worth of the middle one.
func RegisterAll(r *Registry, h Handlers) error {
	regs := []func() error{
		func() error {
			return Register(r, Spec{Queue: QueueIngest, PayloadVersion: 1, Timeout: 60 * time.Second},
				orStub(h.IngestProcessBatch, KindIngestProcessBatch))
		},
		func() error {
			return Register(r, Spec{Queue: QueueEnrich, PayloadVersion: 1, Timeout: 60 * time.Second},
				orStub(h.EnrichRun, KindEnrichRun))
		},
		func() error {
			return Register(r, Spec{Queue: QueueNotify, PayloadVersion: 1, Timeout: 30 * time.Second},
				orStub(h.NotifyEvaluate, KindNotifyEvaluate))
		},
		func() error {
			return Register(r, Spec{Queue: QueueDeliverSlack, PayloadVersion: 1, Timeout: 30 * time.Second},
				orStub(h.DeliverDispatch, KindDeliverDispatch))
		},
		func() error {
			// 15 s, and shorter than every other kind here on purpose: a human
			// pressed a button and is watching the card. A press that cannot be
			// applied in fifteen seconds is better retried than left hanging.
			return Register(r, Spec{Queue: QueueNotify, PayloadVersion: 1, Timeout: 15 * time.Second},
				orStub(h.SlackInteraction, KindSlackInteraction))
		},
		func() error {
			return Register(r, Spec{Queue: QueueReconcile, PayloadVersion: 1, Timeout: 60 * time.Second},
				orStub(h.SourceReconcile, KindSourceReconcile))
		},
		func() error {
			return Register(r, Spec{Queue: QueueReconcile, PayloadVersion: 1, Timeout: 60 * time.Second},
				orStub(h.SilencesSync, KindSilencesSync))
		},
		func() error {
			return Register(r, Spec{Queue: QueueLifecycle, PayloadVersion: 1, Timeout: 2 * time.Minute},
				orStub(h.CaseReap, KindCaseReap))
		},
		// ⛔ `group.close` WAS REGISTERED HERE AND IS DELETED (git-bug `7570090`). It
		// swept open generations idle past `group_close_delay_s` and closed them, which
		// is what made the next fire post a brand-new Slack root. A conversation is a
		// Case now: it ends when the Case ends, so there is no idle generation to
		// close and no periodic sweep to run.
		func() error {
			// Two minutes, like every other per-tenant lifecycle sweep, and per TENANT
			// for the same reason: the fan-out tick does a page read and one batch
			// insert, and one tenant's pass is a bounded fold over at most
			// MaxDigestBackfill windows per digest policy.
			return Register(r, Spec{Queue: QueueLifecycle, PayloadVersion: 1, Timeout: 2 * time.Minute},
				orStub(h.NotifyDigest, KindNotifyDigest))
		},
		func() error {
			return Register(r, Spec{Queue: QueueMaintenance, PayloadVersion: 1, Timeout: 10 * time.Minute},
				orStub(h.PartitionsManage, KindPartitionsManage))
		},
		func() error {
			return Register(r, Spec{Queue: QueueMaintenance, PayloadVersion: 1, Timeout: 10 * time.Minute},
				orStub(h.RetentionPrune, KindRetentionPrune))
		},
		func() error {
			return Register(r, Spec{Queue: QueueMaintenance, PayloadVersion: 1, Timeout: 5 * time.Minute},
				orStub(h.StatsRollup, KindStatsRollup))
		},
		func() error {
			return Register(r, Spec{Queue: QueueMaintenance, PayloadVersion: 1, Timeout: 5 * time.Minute},
				orStub(h.CacheExpire, KindCacheExpire))
		},
	}

	for _, reg := range regs {
		if err := reg(); err != nil {
			return err
		}
	}
	return nil
}

// AddDefaultPeriodic installs the zero-payload periodic schedule of SPEC §G.3.
//
// The per-source jobs (source.reconcile, silences.sync) are deliberately NOT
// here: their payload names a source, so the fan-out needs the source list and
// belongs to the `sources` service, which enqueues them through db.Enqueuer.
// Their args already carry a matching uniqueness window, so a slow reconcile
// cannot stack up behind itself however often the fan-out runs. `source.reconcile`
// is per-tenant as well as per-source now, and that changes nothing here for the
// same reason as below: its schedule is still an args struct with a nil source id
// AND a nil org id, and both expansions happen in the handler.
//
// ⭐ THE PER-TENANT PERIODICS ARE STILL HERE, AND THE ZERO ARGS BELOW ARE WHY.
// `case.reap`, `group.close`,
// `notify.digest`, `retention.prune` and `stats.rollup` are all fanned out per tenant now
// (jobs.TenantFanOut), but their SCHEDULE still needs no list: an args struct
// with a nil OrgID IS the fan-out tick, and expanding it into one job per
// tenant happens in the handler, where
// the tenant list can be read. That is the difference from the per-source pair —
// a source job cannot even be constructed without knowing a source id, whereas
// these can, so the schedule stays declarative and in one place.
//
// clk is used only to derive the schedule's notion of now, so a test can drive it.
func AddDefaultPeriodic(r *Registry, clk clock.Clock) {
	if clk == nil {
		clk = clock.New()
	}

	add := func(interval time.Duration, id string, ctor river.PeriodicJobConstructor) {
		r.AddPeriodic(river.NewPeriodicJob(
			river.PeriodicInterval(interval),
			ctor,
			// RunOnStart hedges a leader election that happens just after a tick:
			// without it, a deploy can silently skip an hour of partition
			// management, and the first missing partition is an outage.
			&river.PeriodicJobOpts{ID: id, RunOnStart: true},
		))
	}

	add(time.Minute, KindCaseReap, func() (river.JobArgs, *river.InsertOpts) {
		return CaseReapArgs{}, nil
	})
	// The digest tick. Once a minute is not the window — the window is arithmetic on
	// the clock, aligned to the UTC day (`notification/domain.Digest.WindowStart`) —
	// so this only bounds how late a digest can be, at one minute. RunOnStart matters
	// here for the same reason it matters for `partitions.manage`: a deploy that landed
	// just after a boundary would otherwise owe a window it does not notice for a
	// minute, and the owed window is capped by MaxDigestBackfill rather than lost.
	add(time.Minute, KindNotifyDigest, func() (river.JobArgs, *river.InsertOpts) {
		return NotifyDigestArgs{}, nil
	})
	add(time.Hour, KindPartitionsManage, func() (river.JobArgs, *river.InsertOpts) {
		return PartitionsManageArgs{}, nil
	})
	add(time.Hour, KindRetentionPrune, func() (river.JobArgs, *river.InsertOpts) {
		return RetentionPruneArgs{}, nil
	})
	add(10*time.Minute, KindCacheExpire, func() (river.JobArgs, *river.InsertOpts) {
		return CacheExpireArgs{}, nil
	})
	// stats.rollup names the day it is rolling up. Today's row is recomputed
	// every 15 minutes and converges; yesterday's is finalised by the first tick
	// after midnight UTC.
	add(15*time.Minute, KindStatsRollup, func() (river.JobArgs, *river.InsertOpts) {
		return StatsRollupArgs{Day: clk.Now().UTC().Format(time.DateOnly)}, nil
	})
}
