package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Options are the service's dependencies.
type Options struct {
	Store   DrillStore
	Ingest  IngestAcceptor
	Sources SourceReader
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Service runs delivery drills.
type Service struct {
	store   DrillStore
	ingest  IngestAcceptor
	sources SourceReader
	clk     clock.Clock
	log     *slog.Logger
}

// New builds the service.
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Ingest == nil || o.Sources == nil {
		return nil, errs.New(errs.KindInternal, "drill_deps",
			"a drill needs a store, an ingest acceptor and a source reader")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: o.Store, ingest: o.Ingest, sources: o.Sources, clk: clk, log: log}, nil
}

// StartCommand is one request to run a drill.
type StartCommand struct {
	SourceID uuid.UUID
	// Severity is the raw label value the synthetic alert fires at, in the
	// operator's own vocabulary (§L.4.2). Empty means domain.DefaultSeverity.
	//
	// ⭐ It is settable because SEVERITY IS OFTEN WHAT A POLICY MATCHES ON. An
	// operator whose only notification policy routes `severity=critical` would
	// otherwise get `no_policy` from every drill and conclude their install is
	// broken when it is working exactly as configured.
	Severity string
	// ActorID and ActorLabel are past-tense attribution in the `acked_by` mould.
	// The label is frozen at write time so the receipt stays readable after the
	// user is gone.
	ActorID    uuid.UUID
	ActorLabel string
}

// Start mints a drill and pushes its synthetic payload through the REAL ingest
// path, then returns immediately.
//
// ⭐⭐ IT RETURNS AFTER THE ACCEPT AND NOT AFTER THE DELIVERY, and that is a
// property of the thing being tested rather than a design shortcut. oto's ingest
// path is asynchronous by contract (§G.1: durably record, enqueue, answer) — a
// drill that blocked until Slack answered would be exercising a code path that
// does not exist. The caller polls `Get`, which reads the real rows.
//
// ⛔ AT MOST ONE DRILL PER SOURCE IS IN FLIGHT. Not for load — a drill is one
// alert — but because two concurrent drills on one source would post two cards
// into a channel from one button, and an operator debugging silence does not need
// help making noise.
func (s *Service) Start(
	ctx context.Context, scope db.TenantScope, cmd StartCommand,
) (domain.Drill, domain.Result, error) {
	now := s.clk.Now().UTC()

	target, err := s.sources.DrillTarget(ctx, scope, cmd.SourceID)
	if err != nil {
		return domain.Drill{}, domain.Result{}, err
	}
	if target.Deleted {
		return domain.Drill{}, domain.Result{},
			errs.NotFound("source_deleted", "this source has been deleted")
	}

	if err := s.refuseIfRunning(ctx, scope, cmd.SourceID, now); err != nil {
		return domain.Drill{}, domain.Result{}, err
	}

	drillID := id.New()
	body, err := domain.BuildPayload(domain.PayloadInput{
		DrillID:    drillID,
		ClusterKey: target.ClusterKey,
		Severity:   cmd.Severity,
		Now:        now,
	})
	if err != nil {
		return domain.Drill{}, domain.Result{}, err
	}

	// ⭐ THE ROW IS WRITTEN BEFORE THE PAYLOAD IS PUSHED. If the process dies in
	// between, what is left is a visible, reapable drill rather than an
	// unattributable synthetic alert nobody can find — and the `oto_drill` label
	// is this row's id, so its artefacts stay discoverable from it.
	drill, err := s.store.Create(ctx, scope, domain.NewDrill{
		ID:             drillID,
		SourceID:       cmd.SourceID,
		Severity:       severityOf(cmd.Severity),
		StartedBy:      cmd.ActorID,
		StartedByLabel: actorLabel(cmd.ActorLabel),
		StartedAt:      now,
		DeadlineAt:     now.Add(domain.Deadline),
	})
	if err != nil {
		return domain.Drill{}, domain.Result{}, err
	}

	res, err := s.ingest.Accept(ctx, scope, AcceptCommand{SourceID: cmd.SourceID, Body: body})
	if err != nil {
		// The accept failed, so nothing downstream will ever happen. Settle the
		// drill NOW rather than making an operator watch a spinner for ninety
		// seconds to learn something already known.
		failed := domain.Result{
			Status:      domain.DrillFailed,
			FailedStage: domain.StageAccept,
			Stages:      stagesFailingAt(domain.StageAccept, acceptFailureDetail(err)),
		}
		if ferr := s.store.Freeze(ctx, scope, drill.ID, failed, s.clk.Now().UTC()); ferr != nil {
			s.log.ErrorContext(ctx, "drill: could not record the accept failure",
				"drill_id", drill.ID, "error", ferr)
		}
		drill.FinishedAt = now
		drill.Outcome = &failed
		return drill, failed, nil
	}

	if res.Duplicate {
		// Impossible by construction — every drill's payload carries a fresh nonce,
		// so no two can hash to the same §C.5 dedup key. If it happens anyway, the
		// drill is about to report on somebody else's artefacts and an operator
		// deserves to know the answer is untrustworthy.
		s.log.WarnContext(ctx, "drill: the synthetic batch was collapsed onto an earlier one",
			"drill_id", drill.ID, "batch_id", res.BatchID)
	}
	if err := s.store.SetBatch(ctx, scope, drill.ID, res.BatchID); err != nil {
		return domain.Drill{}, domain.Result{}, err
	}
	drill.BatchID = res.BatchID

	return s.resolve(ctx, scope, drill)
}

// Get returns a drill and its staged result.
func (s *Service) Get(
	ctx context.Context, scope db.TenantScope, drillID uuid.UUID,
) (domain.Drill, domain.Result, error) {
	drill, err := s.store.Get(ctx, scope, drillID)
	if err != nil {
		return domain.Drill{}, domain.Result{}, err
	}
	return s.resolve(ctx, scope, drill)
}

// List returns a source's recent drills with their results.
func (s *Service) List(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID, limit int,
) ([]domain.Drill, []domain.Result, error) {
	drills, err := s.store.ListForSource(ctx, scope, sourceID, limit)
	if err != nil {
		return nil, nil, err
	}
	results := make([]domain.Result, len(drills))
	for i, d := range drills {
		_, res, rerr := s.resolve(ctx, scope, d)
		if rerr != nil {
			return nil, nil, rerr
		}
		results[i] = res
	}
	return drills, results, nil
}

// resolve produces the staged result for one drill, freezing it the first time it
// settles.
//
// ⭐⭐ A SETTLED DRILL RETURNS ITS FROZEN BYTES AND NEVER RECOMPUTES. That is what
// lets disposal delete the evidence while the answer survives — and it also means
// two operators reading the same drill an hour apart read the same verdict, which
// a recomputed one could not promise once the Case resolved or the thread went dead.
//
// ⚠️ THAT CLAUSE USED TO SAY "once a group closed or a thread FROZE", and both halves
// aged out: generations are gone (git-bug `7570090`) and a thread is never frozen
// (migration 00066). The reason is unchanged and is why the sentence is re-pointed
// rather than cut — the live rows a verdict is computed from keep moving, so freezing
// is what makes the answer stable.
func (s *Service) resolve(
	ctx context.Context, scope db.TenantScope, d domain.Drill,
) (domain.Drill, domain.Result, error) {
	if d.Outcome != nil {
		return d, *d.Outcome, nil
	}

	arts, err := s.store.Artifacts(ctx, scope, d)
	if err != nil {
		return domain.Drill{}, domain.Result{}, err
	}
	arts.RuleLookupPossible = s.rulesPossible(ctx, scope, d)

	now := s.clk.Now().UTC()
	res := domain.Observe(arts, !now.Before(d.DeadlineAt))

	// The disposal manifest is kept up to date on every poll, so a drill that is
	// never polled again can still be cleaned up from whatever it did reach.
	if err := s.store.RecordArtefacts(ctx, scope, d.ID, arts); err != nil {
		return domain.Drill{}, domain.Result{}, err
	}
	d.AlertID = firstNonNil(d.AlertID, arts.Alert.ID)
	d.CaseID = firstNonNil(d.CaseID, arts.Case.ID)
	d.NotificationID = firstNonNil(d.NotificationID, arts.Notification.ID)

	if res.Status.Terminal() {
		if err := s.store.Freeze(ctx, scope, d.ID, res, now); err != nil {
			return domain.Drill{}, domain.Result{}, err
		}
		d.FinishedAt = now
		d.Outcome = &res
	}
	return d, res, nil
}

// rulesPossible reports whether this source could have produced a rule snapshot
// at all, so the rule stage can explain itself honestly. A source that has gone
// away since the drill started is not a reason to fail the drill.
func (s *Service) rulesPossible(ctx context.Context, scope db.TenantScope, d domain.Drill) bool {
	target, err := s.sources.DrillTarget(ctx, scope, d.SourceID)
	if err != nil {
		return false
	}
	return target.HasPrometheus
}

// refuseIfRunning enforces one live drill per source.
//
// A drill whose DEADLINE HAS PASSED does not block a new one even if nothing has
// settled it yet: the sweep that finalises abandoned drills runs hourly, and an
// operator must not have to wait for it to try again.
func (s *Service) refuseIfRunning(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID, now time.Time,
) error {
	recent, err := s.store.ListForSource(ctx, scope, sourceID, 5)
	if err != nil {
		return err
	}
	for _, d := range recent {
		if !d.Settled() && now.Before(d.DeadlineAt) {
			return errs.Precondition("drill_already_running",
				"a delivery drill for this source is still in flight; wait for it to finish")
		}
	}
	return nil
}

// Sweep is the `retention.prune` hook: settle abandoned drills, then dispose of
// the synthetic rows of drills that settled long enough ago.
//
// ⭐ IT IS TWO PHASES AND THE ORDER MATTERS. A drill is only disposable once its
// verdict is frozen, because disposal deletes the evidence the verdict is
// computed from. Finalising first means an abandoned drill is cleaned up one
// sweep later rather than never.
func (s *Service) Sweep(ctx context.Context, scope db.TenantScope, limit int) (finalised, disposed int, err error) {
	if limit <= 0 {
		limit = 100
	}
	now := s.clk.Now().UTC()

	open, err := s.store.Unfinished(ctx, scope, limit)
	if err != nil {
		return 0, 0, err
	}
	for _, d := range open {
		if now.Before(d.DeadlineAt) {
			continue
		}
		settled, _, rerr := s.resolve(ctx, scope, d)
		if rerr != nil {
			s.log.ErrorContext(ctx, "drill: could not finalise an abandoned drill",
				"drill_id", d.ID, "error", rerr)
			continue
		}
		if settled.Settled() {
			finalised++
		}
	}

	old, err := s.store.Disposable(ctx, scope, now.Add(-domain.DisposeAfter), limit)
	if err != nil {
		return finalised, 0, err
	}
	for _, d := range old {
		if derr := s.store.Dispose(ctx, scope, d, now); derr != nil {
			s.log.ErrorContext(ctx, "drill: could not dispose of a drill's synthetic rows",
				"drill_id", d.ID, "error", derr)
			continue
		}
		disposed++
	}
	return finalised, disposed, nil
}

// DisposeNow is the explicit "clean this up, I am done looking at it" action.
//
// It refuses a drill that has not settled: deleting the rows a running drill is
// still being judged from would make it report failures that never happened.
func (s *Service) DisposeNow(ctx context.Context, scope db.TenantScope, drillID uuid.UUID) error {
	d, _, err := s.Get(ctx, scope, drillID)
	if err != nil {
		return err
	}
	if !d.Settled() {
		return errs.Precondition("drill_still_running",
			"this drill has not finished; wait for it to settle before disposing of it")
	}
	if d.Disposed() {
		return nil
	}
	return s.store.Dispose(ctx, scope, d, s.clk.Now().UTC())
}

// stagesFailingAt renders the stage list for a drill that died at its first
// stage, so a caller always gets the full chain rather than a bare error.
func stagesFailingAt(at domain.StageName, detail string) []domain.Stage {
	names := domain.AllStages()
	out := make([]domain.Stage, 0, len(names))
	for _, n := range names {
		st := domain.Stage{Name: n, Status: domain.StatusPending}
		if n == at {
			st.Status = domain.StatusFailed
			st.Detail = detail
		}
		out = append(out, st)
	}
	return out
}

// acceptFailureDetail turns an accept error into a sentence.
//
// ⭐ It reports the errs CODE and MESSAGE, never the wrapped error string: an
// ingest failure's chain can contain a DSN, and a diagnostic screen is exactly
// the place somebody screenshots and pastes into a ticket.
func acceptFailureDetail(err error) string {
	msg := "oto could not durably record the synthetic batch"
	if e, ok := errs.As(err); ok && e.Message != "" {
		msg = e.Message
	}
	if code := errs.CodeOf(err); code != "" {
		return msg + " (" + code + ")"
	}
	return msg
}

func severityOf(in string) string {
	if in == "" {
		return domain.DefaultSeverity
	}
	return in
}

func actorLabel(in string) string {
	if in == "" {
		return "an operator"
	}
	return in
}

func firstNonNil(a, b uuid.UUID) uuid.UUID {
	if a != uuid.Nil {
		return a
	}
	return b
}
