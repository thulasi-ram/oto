package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ObserveOptions carries what the ORCHESTRATOR resolved before the state machine
// ran.
//
// GroupID is here rather than resolved inside this service because §G.4 puts
// group resolution at step 4 — between the alert upsert and the state machine —
// and because `alerts` must not depend on `grouping` to record a signal. An
// observation with no group is still recorded in full; it simply produces no
// `notify.evaluate`, since a notification intent with no group is an intent about
// nothing.
type ObserveOptions struct {
	// GroupID is the AlertGroup generation these observations join.
	GroupID *uuid.UUID
	// GroupReason is a GROUP-SCOPED §H.6 Reason the orchestrator derived from the
	// batch itself rather than from any alert's transition — today, exactly
	// `repeat`, which Alertmanager's `notification_reason` is the only witness to.
	//
	// ⛔ It is passed IN rather than derived here on purpose. Mapping
	// Alertmanager's wire vocabulary onto oto's Reason enum is `notification`'s
	// job (§H.6), and this module must not learn a second copy of that table.
	// `alerts` is told the answer; it never asks the question.
	GroupReason string
}

// ObserveOutcome is what one Observation did. It is the caller's audit of a
// batch, and it is deliberately flat: the reconciler counts divergences from it
// and the ingest worker logs it.
type ObserveOutcome struct {
	AlertID  uuid.UUID
	AlertKey string
	// CaseID is the episode the observation landed on, or uuid.Nil when it
	// landed on none — a `resolved` observation for an Alert with no open episode
	// resolves nothing.
	CaseID uuid.UUID
	// Transition names the §B.3 row that ran, or "" when none did.
	Transition string
	// From and To are the case states either side of the edge.
	From string
	To   string
	// AlertCreated is true on the first ever sighting of this alert_key.
	AlertCreated bool
	// CaseOpened is true for T1 and T7.
	CaseOpened bool
	// Clamped records that §B.3.2 pulled `ended_at` forward because the upstream
	// clock ran backwards. THE CALLER MUST accumulate ClampSkew into
	// source_health.clock_skew_ms: the skew is measured and surfaced, never
	// rejected (C12).
	Clamped   bool
	ClampSkew time.Duration
}

// ObserveResult is the outcome of one batch.
type ObserveResult struct {
	Outcomes      []ObserveOutcome
	EventsWritten int
	JobsEnqueued  int
}

// Observe runs the §B.3 state machine over one Observation.
func (s *Service) Observe(
	ctx context.Context, scope db.TenantScope, o domain.Observation, opt ObserveOptions,
) (ObserveResult, error) {
	return s.ObserveBatch(ctx, scope, []domain.Observation{o}, opt)
}

// ObserveBatch is the write path of §G.4, steps 3 through 8, in ONE transaction.
//
// Upsert the alerts, run the state machine per observation, append the events it
// produced, publish the UI frames and enqueue the follow-on jobs. All of it or
// none of it: a job enqueued for a transition that rolled back is a promise oto
// cannot keep, and an event without its projection is a timeline that disagrees
// with the list.
func (s *Service) ObserveBatch(
	ctx context.Context, scope db.TenantScope, obs []domain.Observation, opt ObserveOptions,
) (ObserveResult, error) {
	if len(obs) == 0 {
		return ObserveResult{}, nil
	}
	cfg := s.lifecycleSettings(ctx, scope)

	// ⭐ RE-READ AND RE-DECIDE is this path's answer to a lost compare-and-set.
	//
	// A conflict means the case moved between the batch's read and its
	// write, so the verdict was reached against a row that no longer exists. The
	// whole attempt has already rolled back — every write in `observe` is inside
	// the transaction, the events carry §C.8 dedupe keys and the alert upsert is
	// ON CONFLICT — so running it again from a fresh read is both safe and the
	// only way to reach the right answer.
	//
	// ⛔ NOT retried when a caller's transaction is already open: `db.Tx` nests,
	// so `InTx` here would join the outer unit of work and a "retry" would replay
	// against writes that were never rolled back. The ingest worker wraps each
	// chunk in its own `db.Tx`, so the conflict surfaces there and the job's own
	// retry re-reads instead. The reconciler calls this method directly and is
	// retried in place.
	var res ObserveResult
	var err error
	for attempt := 1; ; attempt++ {
		err = s.tx.InTx(ctx, func(ctx context.Context) error {
			var ierr error
			res, ierr = s.observe(ctx, scope, obs, opt, cfg)
			return ierr
		})
		if err == nil {
			return res, nil
		}
		if !errs.IsKind(err, errs.KindConflict) || attempt >= observeMaxAttempts || db.InTx(ctx) {
			return ObserveResult{}, err
		}
		s.log.InfoContext(ctx, "alerts: observation batch lost a compare-and-set, re-deciding",
			"org_id", scope.OrgID(), "attempt", attempt, "observations", len(obs),
			"error", err)
	}
}

// observeMaxAttempts bounds the re-decide loop. A batch that loses three
// consecutive compare-and-sets is contending with something that is winning every
// time, and spinning on it would starve the worker rather than fix anything: the
// conflict is returned and the job's own retry budget takes over.
const observeMaxAttempts = 3

// ⚠️ LOCK ORDER: THIS TRANSACTION TAKES `alerts` BEFORE `alert_cases`.
// Step 2's `UpsertBatch` locks the alert row, and the case is only read and
// written afterwards. `Service.expire` (sweep.go) takes them the OTHER WAY ROUND.
// The two orders form a cycle; see the longer note at `expire` before adding any
// explicit lock to either site.
//
// The accidental upside of locking `alerts` first is that two ObserveBatch calls
// touching one alert serialise here, so neither can read a stale case — the
// compare-and-set below is contended almost exclusively by the reaper.
//
// The §B.3 decision itself is NOT here. `domain.Decide` names the row and
// `observeOne` applies what it named: this method is the batch — the reads that
// happen once, the loop, and the writes that happen once at the end.
func (s *Service) observe(
	ctx context.Context, scope db.TenantScope, obs []domain.Observation,
	opt ObserveOptions, cfg Settings,
) (ObserveResult, error) {
	// 1. The material-change probe (T2) must read the Alert BEFORE the upsert
	//    overwrites its annotations and severity. One round trip, once per batch.
	prior, err := s.priorAlerts(ctx, scope, obs)
	if err != nil {
		return ObserveResult{}, err
	}

	// 2. Upsert every observed identity in one round trip (§D.12c). Dedup is the
	//    alerts_key_uniq constraint's job, never a read-then-write check.
	upserts, err := buildUpserts(obs)
	if err != nil {
		return ObserveResult{}, err
	}
	results, err := s.alerts.UpsertBatch(ctx, scope, upserts)
	if err != nil {
		return ObserveResult{}, err
	}

	alertIDs := make([]uuid.UUID, 0, len(results))
	seenAlert := map[uuid.UUID]struct{}{}
	for _, r := range results {
		if _, dup := seenAlert[r.Alert.ID()]; dup {
			continue
		}
		seenAlert[r.Alert.ID()] = struct{}{}
		alertIDs = append(alertIDs, r.Alert.ID())
	}

	// 3. The latest episode of every alert in the batch, in one round trip.
	latest, err := s.latestCases(ctx, scope, alertIDs)
	if err != nil {
		return ObserveResult{}, err
	}

	acc := &observeAccum{
		latest:     latest,
		newEpisode: map[uuid.UUID]int{},
		outcomes:   make([]ObserveOutcome, 0, len(obs)),
	}

	for i, o := range obs {
		if err := s.observeOne(ctx, scope, o, results[i],
			prior[results[i].Alert.Key().String()], opt, cfg, acc); err != nil {
			return ObserveResult{}, err
		}
	}

	// A batch that changed nothing about any alert can still be a fact about the
	// GROUP: `repeat interval elapsed` means Alertmanager is telling us the same
	// thing again, and §H.6 turns that into a root UPDATE and never a repost. It
	// is the largest noise reduction available to oto and it is the one Reason no
	// per-alert transition can produce, because nothing transitioned.
	if opt.GroupReason != "" && opt.GroupID != nil && *opt.GroupID != uuid.Nil {
		acc.notifies = append(acc.notifies, notifyRequest{
			groupID: *opt.GroupID,
			reason:  opt.GroupReason,
			actor:   string(domain.ActorIngest.String()),
		})
	}

	// 4. The batch's writes, ONE round trip each, mirroring the reads above
	//    (§G.4): the projections collapsed to the last write per alert, the
	//    timeline entries, then every UI frame the loop queued — case and
	//    alert frames in observation order first, event frames after, which is
	//    the order the per-item appends used to produce.
	if err := s.flushProjections(ctx, scope, acc); err != nil {
		return ObserveResult{}, err
	}
	written, err := s.appendEventsBatched(ctx, scope, acc)
	if err != nil {
		return ObserveResult{}, err
	}
	if err := s.flushFrames(ctx, scope, acc.frames); err != nil {
		return ObserveResult{}, err
	}
	enrichN, err := s.enqueueEnrich(ctx, acc.enrichIDs)
	if err != nil {
		return ObserveResult{}, err
	}
	// ⭐ PHASE ORDERING (SPEC §F.3). A `fired` evaluation for an episode that has
	// just been queued for the INLINE enrichment pass is scheduled at the far end
	// of the pre-notification budget rather than immediately, so the first card
	// can carry the rule snapshot that pass captures. The pipeline releases it
	// early the moment the pass finishes; this is only the backstop. See
	// enqueueNotify.
	deferred := map[uuid.UUID]struct{}{}
	if enrichN > 0 {
		for _, caseID := range acc.enrichIDs {
			deferred[caseID] = struct{}{}
		}
	}
	notifyN, err := s.enqueueNotify(ctx, scope, acc.notifies, deferred)
	if err != nil {
		return ObserveResult{}, err
	}

	return ObserveResult{
		Outcomes:      acc.outcomes,
		EventsWritten: written,
		JobsEnqueued:  enrichN + notifyN,
	}, nil
}

// observeAccum is what a batch builds up as it decides one observation at a time.
//
// It exists so that `observeOne` can be a method with ONE mutable argument
// instead of six named accumulators closed over by a loop body: everything below
// is either written once for the whole batch at the end (§F.3's phase ordering
// depends on the events and the jobs being batched) or is per-alert state the
// NEXT observation of the same alert must see.
type observeAccum struct {
	// latest is the batch's view of each Alert's current episode. It starts as the
	// one round trip in step 3 and is UPDATED IN PLACE, so a second observation of
	// the same alert inside one batch decides against what the first one did
	// rather than against a stale read.
	latest map[uuid.UUID]domain.Case
	// newEpisode counts the episodes opened per Alert IN THIS BATCH, which is what
	// the projection adds to total_cases.
	newEpisode map[uuid.UUID]int

	// projections is the batch's pending `alerts` summary per alert, and
	// projectionOrder is first-staged order so the flush is deterministic. M
	// observations of one alert stage M times and FLUSH ONCE: only the last
	// write could survive anyway, so the earlier round trips bought nothing.
	projections     map[uuid.UUID]domain.AlertProjection
	projectionOrder []uuid.UUID
	// frames is every §E.4 change notice the batch queued, in the order the
	// per-item appends used to publish them; it is flushed in ONE round trip.
	frames []StreamFrame

	events    []domain.Event
	notifies  []notifyRequest
	enrichIDs []uuid.UUID
	outcomes  []ObserveOutcome
}

// stageProjection records the projection an observation decided, collapsing to
// one write per alert.
//
// Last write wins for every field EXCEPT LastSeenAt, which keeps the maximum
// staged: SetProjection's SQL folds each write through
// GREATEST(last_seen_at, $7), so M sequential writes left max(all M) behind, and
// a collapse that kept only the last observation's clock reading would let an
// out-of-order pair inside one batch rewind what the sequential code preserved.
// Every other column was a plain overwrite (last_state_change_at's GREATEST is
// against first_seen_at, which no write here moves), so for those the last
// staged value IS the sequential result.
func (a *observeAccum) stageProjection(alertID uuid.UUID, p domain.AlertProjection) {
	if a.projections == nil {
		a.projections = map[uuid.UUID]domain.AlertProjection{}
	}
	prev, seen := a.projections[alertID]
	if !seen {
		a.projectionOrder = append(a.projectionOrder, alertID)
		a.projections[alertID] = p
		return
	}
	if prev.LastSeenAt.After(p.LastSeenAt) {
		p.LastSeenAt = prev.LastSeenAt
	}
	a.projections[alertID] = p
}

// observeOne runs SPEC §B.3 over ONE observation and applies what it decided.
//
// ⭐ THE DECISION IS `domain.Decide` AND THE EFFECTS ARE HERE. That split is the
// point of this method. `Decide` is a pure function of the observation and the
// episode it lands on, so every row of the table is drivable from a test with no
// database; and there is exactly ONE place — `applyOpen` — that turns "open a new
// episode" into writes. There used to be two, forty lines apart inside a 294-line
// function, and they had already drifted apart on whether T10 notifies.
func (s *Service) observeOne(
	ctx context.Context, scope db.TenantScope, o domain.Observation,
	up domain.AlertUpsertResult, prior domain.Alert, opt ObserveOptions, cfg Settings,
	acc *observeAccum,
) error {
	alert := up.Alert
	out := ObserveOutcome{
		AlertID:      alert.ID(),
		AlertKey:     alert.Key().String(),
		AlertCreated: up.WasInserted,
	}

	at, err := observationTime(o, s.Now())
	if err != nil {
		return err
	}
	actor, err := actorFor(o.Source)
	if err != nil {
		return err
	}
	if up.WasInserted {
		ev, err := alertCreatedEvent(alert, at, actor)
		if err != nil {
			return err
		}
		acc.events = append(acc.events, ev)
	}

	// The Alert's latest episode, or the ZERO Case when it has none — which
	// is StateNone, the state T1's row comes from. `Decide` needs no second
	// argument to be told the difference.
	current := acc.latest[alert.ID()]
	trigger := triggerFor(o.Status)

	d, err := domain.Decide(current, routingCommand(trigger, actor, at, cfg))
	if err != nil {
		if errs.IsKind(err, errs.KindPrecondition) {
			s.noTransition(ctx, alert, current, trigger, out, acc)
			return nil
		}
		return err
	}

	var (
		ac           domain.Case
		stateChanged bool
	)
	if d.OpensEpisode {
		// A new episode is always a state change: there was no state before it.
		ac, err = s.applyOpen(ctx, scope, alert, o, at, actor, opt, d, &out, acc)
		if err != nil {
			return err
		}
		stateChanged = true
	} else {
		// ⛔ THE FALLIBLE HALF OF THE COMMAND IS ASSEMBLED HERE AND NOT BEFORE
		// `Decide`. Naming the Alertmanager witness that suppressed an observation
		// can fail, and an unwitnessed `suppressed` observation for an Alert with no
		// episode runs no row at all — asking the question early would cost the
		// whole batch for an observation that changes nothing.
		cmd, err := s.edgeCommand(o, at, actor, trigger, cfg, prior, alert)
		if err != nil {
			return err
		}
		r, err := domain.Apply(current, cmd)
		if err != nil {
			// A precondition failure is the machine saying "no §B.3 row permits this
			// from here". That is a normal outcome for a noisy upstream — a duplicate
			// `resolved`, say — and must not fail the batch and cost every other alert
			// in it.
			if errs.IsKind(err, errs.KindPrecondition) {
				s.noTransition(ctx, alert, current, trigger, out, acc)
				return nil
			}
			return err
		}
		ac, stateChanged, err = s.applyEdge(ctx, scope, alert, o, r, actor, opt, &out, acc)
		if err != nil {
			return err
		}
	}

	// ⛔ NO `severity_raised` NOTIFICATION IS EMITTED HERE, AND THE CODE THAT ONCE
	// DID WAS UNREACHABLE.
	//
	// It read the pre-upsert snapshot, compared `was.Severity()` with
	// `alert.Severity()` under `!WasInserted`, and could never be true: the lookup
	// is by `alert.Key()`, and `severity` is hashed INTO that key (§C.2), so a
	// changed severity is a changed key — a MISS in `prior` and a fresh insert,
	// never an update. Two severities of one rule are two Alerts. ADR 0020 records
	// the finding; `test/integration/alert_identity_test.go` proves it against a
	// real database.

	acc.latest[alert.ID()] = ac
	out.CaseID = ac.ID()
	// The projection and the two frames are STAGED, not written: the batch
	// flushes them once at the end (§G.4), inside the same transaction, so an
	// event and its projection still commit together or not at all.
	acc.stageProjection(alert.ID(), projectionFor(alert, ac, at, stateChanged,
		acc.newEpisode[alert.ID()]))
	if err := s.queueFrame(acc, StreamCaseUpserted, ac.ID(),
		caseFramePayload(ac)); err != nil {
		return err
	}
	if err := s.queueFrame(acc, StreamAlertUpserted, alert.ID(), map[string]any{
		"alert_key": alert.Key().String(),
		"state":     alert.State().String(),
	}); err != nil {
		return err
	}
	acc.outcomes = append(acc.outcomes, out)
	return nil
}

// noTransition records an observation that ran no §B.3 row.
//
// The Alert row and its `alert.created` event are already in the transaction, so
// the identity is never lost; what does not happen is a projection write and a
// stream frame. ⛔ THAT IS THIS PATH'S LONG-STANDING BEHAVIOUR AND IT IS NOT THIS
// REFACTOR'S TO CHANGE: the branch that used to sit under the loop's `haveCase`
// test to project such an Alert was unreachable, because both give-up paths
// `continue`d above it. It is also very nearly harmless — the upsert's INSERT
// already writes `state`, `last_seen_at` and `last_state_change_at` for a
// first-ever sighting, which is exactly what that branch re-wrote.
func (s *Service) noTransition(
	ctx context.Context, alert domain.Alert, current domain.Case,
	trigger domain.Trigger, out ObserveOutcome, acc *observeAccum,
) {
	s.log.DebugContext(ctx, "alerts: observation makes no legal transition",
		// AlertState, because the question this line answers is which of the four
		// §B.2 states had no edge under this trigger; `open`/`closed` would not
		// name the row of the table that was looked for.
		"alert_key", alert.Key().String(), "state", current.AlertState().String(),
		"trigger", trigger.String())
	acc.outcomes = append(acc.outcomes, out)
}

// applyOpen opens the episode a Decision asked for, AND IS THE ONLY PLACE THAT
// DOES.
//
// T1 (the Alert's first episode) and T7 (a re-fire) differ in exactly one value
// — the sequence number — and `Decide` has already computed it from the table.
// Everything else about them is identical, which is why they are one function and
// not two branches, and since ADR 0040 retired T8 those two rows are the whole
// population of "an episode begins".
func (s *Service) applyOpen(
	ctx context.Context, scope db.TenantScope, alert domain.Alert, o domain.Observation,
	at domain.ObservationTime, actor domain.Actor, opt ObserveOptions, d domain.Decision,
	out *ObserveOutcome, acc *observeAccum,
) (domain.Case, error) {
	opened, evs, err := s.openEpisode(ctx, scope, alert, o, at, actor, opt, d.Seq)
	if err != nil {
		return domain.Case{}, err
	}
	acc.events = append(acc.events, evs...)
	acc.enrichIDs = append(acc.enrichIDs, opened.ID())
	acc.newEpisode[alert.ID()]++

	out.CaseOpened = true
	out.Transition = d.ID.String()
	// ⭐ `From` IS THE STATE THE EPISODE CAME FROM, and for T7 that is `resolved`
	// or `expired`, never the empty string. It is empty for T1 alone, where
	// StateNone is the honest answer: there was no episode. The two branches this
	// replaced disagreed about exactly this, and the one that reported "" for a
	// re-fire was describing a first sighting that had not happened.
	//
	// ⛔ IT IS `AlertState`, NOT `State`. `ObserveOutcome.From`/`To` are the §B.2
	// words on every other assignment, and since ADR 0040 `Case.State()` answers
	// `open`/`closed` — so reading it here would report a re-fire as
	// "resolved -> open", which is a sentence in two vocabularies at once.
	out.From, out.To = d.From.String(), opened.AlertState().String()

	// T10: an acknowledgement does NOT survive into a new episode. The previous
	// case keeps its ack in the record — rewriting a terminal episode's
	// attribution would be rewriting history — and the fact that the ack no longer
	// applies is recorded on the NEW episode.
	if d.DropsAck {
		ev, err := autoUnackEvent(opened, at)
		if err != nil {
			return domain.Case{}, err
		}
		acc.events = append(acc.events, ev)
		// ⭐ AND IT NOTIFIES, which is the half the two branches disagreed about.
		// SPEC §B.3 T10 is explicit — "Emit `case.unacknowledged` … enqueue
		// `notify.evaluate(reason=unacked)`" — and §H.5's `unack` block has a
		// rendering for precisely this road: "*Un-acknowledged* — new case
		// opened". `unacked` is a root UPDATE with no reply (§H.6), so it costs a
		// card edit and never a repost, and without it the only witness that a
		// human's acknowledgement stopped applying is a timeline entry nobody is
		// told about.
		acc.notifies = append(acc.notifies, notifyRequest{
			groupID: groupOf(opt, opened),
			reason:  reasonUnacked,
			alertID: ptr(alert.ID()),
			caseID:  ptr(opened.ID()),
			actor:   actor.Kind().String(),
		})
	}
	acc.notifies = append(acc.notifies, notifyRequest{
		groupID: groupOf(opt, opened),
		reason:  reasonFired,
		alertID: ptr(alert.ID()),
		caseID:  ptr(opened.ID()),
		actor:   actor.Kind().String(),
	})
	return opened, nil
}

// applyEdge persists an edge that MOVED the episode it was decided against, and
// reports whether the move was a state change.
//
// ⛔ T7 CANNOT REACH HERE. `Decide` and `Apply` read the same row from the same
// table for the same command, so an edge that opens an episode has already been
// routed to `applyOpen`.
func (s *Service) applyEdge(
	ctx context.Context, scope db.TenantScope, alert domain.Alert, o domain.Observation,
	r domain.TransitionResult, actor domain.Actor, opt ObserveOptions,
	out *ObserveOutcome, acc *observeAccum,
) (domain.Case, bool, error) {
	if err := s.persistTransition(ctx, scope, r, o); err != nil {
		return domain.Case{}, false, err
	}
	ac := r.Case
	acc.events = append(acc.events, r.Events...)
	out.Transition = r.ID.String()
	out.From, out.To = r.From.String(), r.To.String()
	out.Clamped, out.ClampSkew = r.Clamped, r.ClampSkew
	if reason := reasonFor(r.ID); reason != "" {
		acc.notifies = append(acc.notifies, notifyRequest{
			groupID: groupOf(opt, ac),
			reason:  reason,
			alertID: ptr(alert.ID()),
			caseID:  ptr(ac.ID()),
			actor:   actor.Kind().String(),
		})
	}
	return ac, r.From != r.To, nil
}

// openEpisode opens a new AlertCase and returns it with its
// `case.opened` event. A new episode ALWAYS starts unacked (T10).
func (s *Service) openEpisode(
	ctx context.Context, scope db.TenantScope, alert domain.Alert, o domain.Observation,
	at domain.ObservationTime, actor domain.Actor, opt ObserveOptions, seq int,
) (domain.Case, []domain.Event, error) {
	caseID := id.New()

	// The domain factory proves the invariants and mints the event; the
	// repository writes the row. Both must describe the same episode, so the id
	// is minted once, here.
	draft, evs, err := domain.OpenNewCase(domain.OpenCaseParams{
		ID:              caseID,
		OrgID:           scope.OrgID(),
		AlertID:         alert.ID(),
		GroupID:         idOrNil(opt.GroupID),
		Seq:             seq,
		Actor:           actor,
		At:              at,
		SourceStartsAt:  o.SourceStartsAt,
		SourceEndsAt:    o.SourceEndsAt,
		SourceUpdatedAt: o.SourceUpdatedAt,
		Value:           o.Value,
		ObservedSkew:    time.Duration(o.SkewMS) * time.Millisecond,
		EventID:         id.New(),
	})
	if err != nil {
		return domain.Case{}, nil, err
	}

	persisted, err := s.cases.OpenCase(ctx, scope, domain.OpenCase{
		ID:              draft.ID(),
		AlertID:         draft.AlertID(),
		GroupID:         opt.GroupID,
		Seq:             draft.Seq(),
		StartedAt:       draft.StartedAt(),
		SourceStartsAt:  draft.SourceStartsAt(),
		SourceEndsAt:    nilTime(draft.SourceEndsAt()),
		SourceUpdatedAt: nilTime(draft.SourceUpdatedAt()),
		Value:           draft.Value(),
		SkewMS:          o.SkewMS,
	})
	if err != nil {
		return domain.Case{}, nil, err
	}
	return persisted, evs, nil
}

// persistTransition writes the edge the machine produced. T2 is an Observe —
// there is no state to move, only upstream fields to fold in — and every other
// edge is a Transition.
func (s *Service) persistTransition(
	ctx context.Context, scope db.TenantScope, r domain.TransitionResult, o domain.Observation,
) error {
	if r.ID == domain.TransitionT2 {
		return s.cases.Observe(ctx, scope, r.Case.ID(), o)
	}
	// The observation's witnesses travel with the edge. They are the ONLY place
	// Alertmanager names which silence, inhibition or mute interval is muting this
	// alert, and `alert_cases.suppressed_by` is the column every read path
	// answers "what is suppressing this?" from.
	return s.cases.Transition(ctx, scope, r.Case.ID(), transitionOf(r, o.SuppressedBy))
}

// transitionOf reads the persisted effect straight off the case the domain
// machine produced. Nothing here re-derives a value — in particular `ended_at`,
// which §B.3.2 has already clamped.
//
// `witnesses` are the ids Alertmanager named on the observation that caused this
// edge — `silencedBy`, `inhibitedBy` and `mutedBy`, all three, from the same
// `status` object. A caller with no observation (the reaper) passes the zero
// value, which is correct: an expiry names no suppressor.
func transitionOf(r domain.TransitionResult, witnesses domain.SuppressedBy) domain.Transition {
	o := r.Case
	t := domain.Transition{
		Kind:           kindOf(r.ID),
		ToState:        o.State(),
		EndedAt:        nilTime(o.EndedAt()),
		LastObservedAt: o.LastObservedAt(),
		SourceEndsAt:   nilTime(o.SourceEndsAt()),
		Value:          o.Value(),
		Clamped:        r.Clamped,
		// ⭐ The compare-and-set pre-image, taken from the row the machine actually
		// read. It is derived here and nowhere else, so no call site can persist an
		// edge against a pre-image it did not decide from.
		Expected: domain.PreconditionFor(r.Before),
	}
	if !o.SourceUpdatedAt().IsZero() {
		t.SourceUpdatedAt = nilTime(o.SourceUpdatedAt())
	}
	if !o.SuppressionReason().IsZero() {
		v := o.SuppressionReason().String()
		t.SuppressionReason = &v
	}
	if !o.ResolveReason().IsZero() {
		v := o.ResolveReason().String()
		t.ResolveReason = &v
	}
	// ⭐ suppressed_by is written on EVERY edge, not only T3 — but it is written
	// with the OBSERVED witnesses when the case lands in `suppressed`, and
	// cleared on every other edge.
	//
	// It used to be hard-coded to the empty struct on all of them. The clearing
	// half of that was right and is kept: leaving Alertmanager's witnesses behind
	// on an unsuppressed case would make oto keep saying "silenced by
	// <id>" about an alert that is demonstrably firing, and T4's whole meaning is
	// that the suppression is over. The other half silently dropped the ids on
	// the floor: T3 fired correctly, the real silence ids arrived on the
	// observation, and `alert_cases.suppressed_by` was written `{}` anyway.
	// They survived only in the `alert.suppressed` event payload, so the column
	// every API consumer and the UI read to answer "which silence is muting
	// this?" was permanently empty.
	//
	// The gate is the RESULTING STATE and not the edge id, because the witnesses
	// are meaningful in exactly the states the reason is and in no others. Since
	// ADR 0040 `suppressed` is a DERIVED reading — an open episode carrying a
	// suppression reason — so the gate asks `AlertState`, which is the one place
	// that reading is defined.
	//
	// ⛔ All THREE witnesses are carried. `silencedBy`, `inhibitedBy` and
	// `mutedBy` come off the same Alertmanager `status` object, and keeping only
	// silences would leave an inhibited alert just as unexplained as before.
	//
	// ⛔ This is Alertmanager's vocabulary and nothing else. A snooze never
	// appears here: it is a `notifications.suppressed_reason`, oto's own enum, and
	// writing it onto the case would claim Alertmanager is suppressing
	// something it has never heard of (§B.8.2).
	sb := domain.SuppressedBy{}
	if !o.SuppressionReason().IsZero() {
		sb = witnesses
	}
	t.SuppressedBy = &sb
	if n := o.SuppressCount(); n > 0 {
		t.SuppressCount = &n
	}
	if r.DetectedBy == domain.DetectedByReconciler {
		t.DetectedBy = domain.ObservedByReconciler
	} else {
		t.DetectedBy = domain.ObservedByIngest
	}
	return t
}

// projectFromCase writes `alerts`' denormalised summary in the SAME
// transaction as the transition that caused it. The observe path does not come
// through here — it stages projectionFor's result and flushes the batch in one
// statement — this is the per-item write the sweep still makes.
func (s *Service) projectFromCase(
	ctx context.Context, scope db.TenantScope, alert domain.Alert, ac domain.Case,
	at domain.ObservationTime, stateChanged bool, newEpisodes int,
) error {
	return s.alerts.SetProjection(ctx, scope, alert.ID(),
		projectionFor(alert, ac, at, stateChanged, newEpisodes))
}

// projectionFor derives the `alerts` summary one case state implies.
//
// ⭐ IT CANNOT WAKE A SNOOZED ALERT UP ANY MORE, BECAUSE IT CANNOT REACH THE
// SNOOZE. Snooze is the third orthogonal axis (§B.1) and lives entirely on
// `alert_snoozes`; this function used to carry a mirrored `SnoozedUntil` through
// every state transition, which meant one forgotten field here would silently
// end a quiet period a human had asked for.
func projectionFor(
	alert domain.Alert, ac domain.Case, at domain.ObservationTime,
	stateChanged bool, newEpisodes int,
) domain.AlertProjection {
	lastChange := alert.LastStateChangeAt()
	if stateChanged || lastChange.IsZero() {
		lastChange = at.RecordedAt()
	}
	var currentCase *uuid.UUID
	if ac.IsOpen() {
		currentCase = ptr(ac.ID())
	}
	return domain.AlertProjection{
		// ⭐ THE ALERT'S STATE, DERIVED FROM THE EPISODE. `alerts.state` is the
		// three-value §B.2 enum since ADR 0041 and `alert_cases.state` is
		// `open | closed`, so this is the one place the projection crosses ADR
		// 0040's line — and AlertState is total, so the crossing loses nothing.
		State: ac.AlertState(),
		// ⭐ AND THE AXIS TRAVELS BESIDE IT, NEVER INSIDE IT (ADR 0041). These two
		// are read off the SAME case as `State`, in the same statement, so the
		// Alert can never claim a suppression its current episode is not carrying
		// — which is the disagreement that made `alerts.state = 'suppressed'`
		// authoritative over the case that produced it.
		//
		// `SuppressedBy()` self-gates on the reason, so a witness left on a row by
		// anything else never reaches the projection.
		SuppressionReason: ac.SuppressionReason(),
		SuppressedBy:      ac.SuppressedBy(),
		CurrentCaseID:     currentCase,
		LastSeenAt:        at.RecordedAt(),
		LastStateChangeAt: lastChange,
		TotalCases:        alert.TotalCases() + newEpisodes,
	}
}

// flushProjections writes the batch's collapsed projections in ONE statement.
//
// It runs inside the batch's transaction, whose UpsertBatch already holds every
// target row's lock, so the batched UPDATE re-locks rows this transaction owns
// and the `alerts`-before-`alert_cases` lock order documented on `observe`
// is unchanged. A missing alert fails the transaction exactly as the per-item
// SetProjection's NotFound did.
func (s *Service) flushProjections(ctx context.Context, scope db.TenantScope, acc *observeAccum) error {
	if len(acc.projectionOrder) == 0 {
		return nil
	}
	writes := make([]domain.AlertProjectionWrite, 0, len(acc.projectionOrder))
	for _, alertID := range acc.projectionOrder {
		writes = append(writes, domain.AlertProjectionWrite{
			AlertID:    alertID,
			Projection: acc.projections[alertID],
		})
	}
	return s.alerts.SetProjectionBatch(ctx, scope, writes)
}

// ------------------------------------------------------------------ helpers

// priorAlerts reads the pre-upsert state of every observed identity, which is
// the only moment at which "did anything material change?" can still be asked.
func (s *Service) priorAlerts(
	ctx context.Context, scope db.TenantScope, obs []domain.Observation,
) (map[string]domain.Alert, error) {
	keys := make([]string, 0, len(obs))
	seen := map[string]struct{}{}
	for _, o := range obs {
		k := o.AlertKey.String()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	if s.alertBatch != nil {
		return s.alertBatch.GetByAlertKeys(ctx, scope, keys)
	}

	out := map[string]domain.Alert{}
	for _, key := range keys {
		a, err := s.alerts.GetByAlertKey(ctx, scope, key)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				continue
			}
			return nil, err
		}
		out[key] = a
	}
	return out, nil
}

func (s *Service) latestCases(
	ctx context.Context, scope db.TenantScope, alertIDs []uuid.UUID,
) (map[uuid.UUID]domain.Case, error) {
	if s.occBatch != nil {
		return s.occBatch.LatestByAlerts(ctx, scope, alertIDs)
	}
	out := make(map[uuid.UUID]domain.Case, len(alertIDs))
	for _, aid := range alertIDs {
		o, ok, err := s.cases.GetLatestByAlert(ctx, scope, aid)
		if err != nil {
			return nil, err
		}
		if ok {
			out[aid] = o
		}
	}
	return out, nil
}

// routingCommand carries the facts that NAME a §B.3 row: the trigger, the two
// clock readings and the re-fire grace that resolves T7 against T8. It cannot
// fail, which is why the routing question can be asked of every observation.
//
// ⛔ THE GRACE IS PASSED, NOT RE-ASKED. This service used to answer "is this
// re-fire beyond refire_grace?" itself, with its own default handling, next to a
// machine that answers it again from the same table. One of the two would
// eventually have moved.
func routingCommand(
	trigger domain.Trigger, actor domain.Actor, at domain.ObservationTime, cfg Settings,
) domain.TransitionCommand {
	return domain.TransitionCommand{
		Trigger:      trigger,
		Actor:        actor,
		At:           at,
		ResolveGrace: cfg.ResolveGrace,
	}
}

// edgeCommand completes the machine's input for an edge that is about to be
// applied. It never decides an edge — selectRule does — it only supplies the
// facts the table needs.
func (s *Service) edgeCommand(
	o domain.Observation, at domain.ObservationTime, actor domain.Actor,
	trigger domain.Trigger, cfg Settings, prior domain.Alert, current domain.Alert,
) (domain.TransitionCommand, error) {
	cmd := routingCommand(trigger, actor, at, cfg)
	cmd.EventID = id.New()
	cmd.SourceEndsAt = o.SourceEndsAt
	cmd.SourceUpdatedAt = o.SourceUpdatedAt
	cmd.Value = o.Value
	cmd.ObservedSkew = time.Duration(o.SkewMS) * time.Millisecond
	if trigger == domain.TriggerObserveSuppressed {
		reason, err := suppressionReasonOf(o)
		if err != nil {
			return domain.TransitionCommand{}, err
		}
		cmd.SuppressionReason = reason
		cmd.SuppressedBy = o.SuppressedBy
		cmd.Payload = map[string]any{
			"silenced_by":  o.SuppressedBy.SilencedBy,
			"inhibited_by": o.SuppressedBy.InhibitedBy,
			"muted_by":     o.SuppressedBy.MutedBy,
		}
	}
	// A repeat observation is silent unless a MATERIAL field moved (§B.3 T2):
	// severity, an annotation or the generator URL. Anything less and a
	// five-second scrape interval would drown the timeline.
	if prior.ID() != uuid.Nil {
		cmd.MaterialChange = prior.Materially(current.Labels(), current.Annotations(),
			current.GeneratorURL())
	}
	return cmd, nil
}

// suppressionReasonOf maps Alertmanager's witnesses onto its four suppression
// reasons, in the §G.8.3 order: silencedBy, then inhibitedBy, then mutedBy.
//
// ⛔ `snoozed` is NOT one of them and MUST NEVER be added: this enum mirrors what
// ALERTMANAGER is doing, and a human asking oto to be quiet is a different fact
// about a different system (§B.8.2).
func suppressionReasonOf(o domain.Observation) (domain.SuppressionReason, error) {
	if r := strings.TrimSpace(o.SuppressionReason); r != "" {
		return domain.NewSuppressionReason(r)
	}
	switch {
	case len(o.SuppressedBy.SilencedBy) > 0:
		return domain.SuppressionSilence, nil
	case len(o.SuppressedBy.InhibitedBy) > 0:
		return domain.SuppressionInhibition, nil
	case len(o.SuppressedBy.MutedBy) > 0:
		return domain.SuppressionMuteTimeInterval, nil
	default:
		return domain.SuppressionReason{}, errs.Validation("suppression_reason_unknown",
			"a suppressed observation must name which Alertmanager rule suppressed it")
	}
}

// buildUpserts turns Observations into §D.12(c) upsert rows.
func buildUpserts(obs []domain.Observation) ([]domain.AlertUpsert, error) {
	out := make([]domain.AlertUpsert, 0, len(obs))
	for _, o := range obs {
		if o.Labels.IsZero() {
			return nil, errs.Validation("observation_labels_required",
				"an observation must carry its label set")
		}
		raw, _ := o.Labels.Get(domain.LabelSeverity)
		severity, err := domain.NewRawSeverity(raw)
		if err != nil {
			return nil, err
		}
		state := domain.StateFiring
		if triggerFor(o.Status) == domain.TriggerObserveResolved {
			state = domain.StateResolved
		}
		seenAt := o.ObservedAt
		if seenAt.IsZero() {
			seenAt = o.SourceStartsAt
		}
		out = append(out, domain.AlertUpsert{
			ID:           id.New(),
			ClusterID:    o.ClusterID,
			AlertKey:     o.AlertKey,
			Fingerprint:  o.SourceFingerprint,
			AlertName:    o.Labels.AlertName(),
			Severity:     severity,
			Namespace:    strPtr(o.Labels.Namespace()),
			Service:      strPtr(o.Labels.Service()),
			ClusterKey:   o.ClusterKey,
			Labels:       o.Labels,
			Annotations:  o.Annotations,
			GeneratorURL: strPtr(o.GeneratorURL),
			State:        state,
			SeenAt:       seenAt,
			// Carried from the batch's mode, never read out of the labels.
			Synthetic: o.Synthetic,
		})
	}
	return out, nil
}

// observationTime pairs the upstream claim with oto's clock (C12).
//
// For a `resolved` observation the upstream claim is `endsAt` when there is one,
// because that is the instant T5 clamps `ended_at` to. For everything else it is
// `startsAt`. The two are NEVER conflated with `recorded_at`, which is what the
// timeline orders by.
func observationTime(o domain.Observation, now time.Time) (domain.ObservationTime, error) {
	occurred := o.SourceStartsAt
	if triggerFor(o.Status) == domain.TriggerObserveResolved && !o.SourceEndsAt.IsZero() {
		occurred = o.SourceEndsAt
	}
	recorded := o.ObservedAt
	if recorded.IsZero() {
		recorded = now
	}
	if occurred.IsZero() {
		occurred = recorded
	}
	return domain.NewObservationTime(occurred, recorded)
}

// triggerFor maps an upstream status onto a §B.3 trigger.
//
// An unrecognised status is treated as `firing`: the observation ARRIVED, which
// is evidence the label set is active, and §L.3's governing rule is that an
// upstream value oto has never seen must never cost an alert.
func triggerFor(status string) domain.Trigger {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved":
		return domain.TriggerObserveResolved
	case "suppressed":
		return domain.TriggerObserveSuppressed
	default:
		return domain.TriggerObserveFiring
	}
}

func actorFor(src domain.ObservationSource) (domain.Actor, error) {
	if src == domain.ObservedByReconciler {
		return domain.SystemActor(domain.ActorReconciler)
	}
	return domain.SystemActor(domain.ActorIngest)
}

func kindOf(t domain.TransitionID) domain.TransitionKind {
	switch t {
	case domain.TransitionT3:
		return domain.TransitionSuppress
	case domain.TransitionT4:
		return domain.TransitionUnsuppress
	case domain.TransitionT5:
		return domain.TransitionResolve
	case domain.TransitionT6:
		return domain.TransitionExpire
	default:
		return domain.TransitionObserve
	}
}

func groupOf(opt ObserveOptions, o domain.Case) uuid.UUID {
	if opt.GroupID != nil && *opt.GroupID != uuid.Nil {
		return *opt.GroupID
	}
	return o.GroupID()
}

func alertCreatedEvent(a domain.Alert, at domain.ObservationTime, actor domain.Actor) (domain.Event, error) {
	return domain.NewEvent(domain.EventParams{
		ID:        id.New(),
		OrgID:     a.OrgID(),
		AlertID:   a.ID(),
		Type:      domain.EventAlertCreated,
		At:        at,
		Actor:     actor,
		Summary:   "Alert first seen: " + a.AlertName(),
		Payload:   map[string]any{"alert_key": a.Key().String()},
		DedupeKey: "alert:" + a.ID().String() + ":created",
	})
}

// autoUnackEvent records that an acknowledgement did not survive into a new
// episode (T10, reason `new_case`).
//
// It is written against the NEW case and the previous one is left exactly
// as it was. The alternative — clearing the old row's acked_by and acked_at —
// would erase who took a closed episode, and `acked_by_label` is denormalised
// precisely so that the timeline reads the same in a year.
func autoUnackEvent(o domain.Case, at domain.ObservationTime) (domain.Event, error) {
	actor, err := domain.SystemActor(domain.ActorSystem)
	if err != nil {
		return domain.Event{}, err
	}
	return domain.NewEvent(domain.EventParams{
		ID:        id.New(),
		OrgID:     o.OrgID(),
		AlertID:   o.AlertID(),
		CaseID:    o.ID(),
		GroupID:   o.GroupID(),
		Type:      domain.EventCaseUnacknowledged,
		At:        at,
		Actor:     actor,
		Summary:   "Acknowledgement did not carry into the new case",
		Payload:   map[string]any{"reason": domain.UnackReasonNewCase},
		DedupeKey: "case:" + o.ID().String() + ":unacknowledged:new_case",
	})
}

func ptr[T any](v T) *T { return &v }

func nilTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func nilID(v uuid.UUID) *uuid.UUID {
	if v == uuid.Nil {
		return nil
	}
	out := v
	return &out
}

func idOrNil(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	out := v
	return &out
}
