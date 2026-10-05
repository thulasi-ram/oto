package service

// AN INVESTIGATION RUNS AGAINST A CASE (ADR 0053 §3, §4, §6; git-bug 180a525).
//
// A human asks (RequestCaseInvestigation): the run is recorded `queued` and its job
// enqueued in ONE transaction, so there is never a run nobody will execute nor a job for
// a run that was never recorded. The job (RunInvestigation) starts it from `queued`,
// runs oto's own loop, and ends it — publishing the Finding as the Enrichment
// `investigator.<name>` on the Case in the same transaction as the ending.
//
// ⛔⛔ NOTHING HERE DECIDES, DELAYS OR CAUSES A NOTIFICATION (ADR 0053 §2). The run is a
// job on its own queue; the Finding is written to the enrichment store and nothing is
// enqueued after it; there is no port in this module onto `notification` or `channels`.
// A Finding changes what people READ, never WHETHER they are told.
//
// ⭐ THE KILL SWITCH IS RECORDED, NEVER SILENT (§6: "Enabled — Nothing starts"). It is
// read twice: when the run is asked for, and again when its job begins — "effective for
// runs not yet begun". Either time, a switch that is off ends the run `skipped` with
// reason `disabled` and a sentence saying WHICH switch, and nothing calls a model. A run
// already going when the switch flips is not cut off mid-turn: the switch governs what
// starts.
//
// ⭐ THE REST OF §6's TABLE IS ENFORCED HERE TOO (git-bug bf172fe), and each breach has
// its own verb:
//
//   - DAILY TOKEN BUDGET — SKIPPED, ON THE RECORD. Read when the run is asked for and
//     again when its job begins, like the kill switch: a request once the org's
//     recorded spend since 00:00 UTC has reached `investigation_daily_tokens` is a row
//     `skipped` with reason `budget`, never enqueued; a queued run that finds the day
//     spent when it would begin ends the same way. A run already going is bounded by
//     its own per-run token budget, not cut off.
//   - CONCURRENCY — WAITS. A job that finds the org's `investigation_concurrency`
//     running SNOOZES (no attempt consumed; River snoozes indefinitely) and the run
//     stays `queued`. The decisive count and the start are one transaction under the
//     org's advisory lock, so two workers cannot share one slot.
//   - MINIMUM INTERVAL — COALESCES. A membership-change trigger inside an
//     Investigator's interval resolves to the run already queued for that subject, or
//     becomes ONE run whose job is scheduled for when the interval is up (NotBefore).
//     A human's request is not held to it (domain.TriggerHuman).
//
// ⭐⭐ AN INCIDENT IS INVESTIGATED AS A WHOLE (ADR 0053 §4, git-bug 74ea849). Its draw
// and its membership changes reach IncidentChanged through `investigations.incident`,
// a job the membership change enqueued in its own transaction — so nothing here ever
// runs on, or holds up, the Incident's write. Each Investigator an operator opted in
// (`investigates_incidents`) gets one run per draw and one more per burst of churn
// under its minimum interval; going quiet raises nothing. And a Case IN an Incident
// starts nothing of its own automatically — requestCase refuses every automatic
// trigger on a held Case (domain.CoveredByIncident) — while a human may still ask
// about it. The Incident's run reads its member Cases' earlier Findings through the
// built-in Tool `oto_member_findings`, publishes its own Finding as an `incident`
// Enrichment, and declares it outbound as the Incident fact `finding`.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// InvestigationDetail is one run with its transcript.
type InvestigationDetail struct {
	Investigation domain.Investigation
	Steps         []domain.Step
}

// RequestCaseInvestigation records a human's request for one Investigator to run
// against one Case, and enqueues it.
//
// The returned run is `queued` — or `skipped`, with reason `disabled` when the org's
// or the Investigator's kill switch is off, or `budget` when the org's daily token
// budget is spent, in which case nothing is enqueued and the row is the record that
// somebody asked.
//
// ⭐ A CASE IN AN INCIDENT CAN STILL BE ASKED ABOUT. The Incident's run covers it only
// for what starts AUTOMATICALLY (ADR 0053 §4); a person asking about one Case of a
// storm gets a run about that Case.
func (s *Service) RequestCaseInvestigation(
	ctx context.Context, scope db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester,
) (domain.Investigation, error) {
	inv, _, err := s.requestCase(ctx, scope, caseID, investigatorID, by, domain.TriggerHuman)
	return inv, err
}

// requestCase records one trigger's run against one Case, and reports whether one
// was recorded at all.
//
// ⛔ AN AUTOMATIC TRIGGER ON A CASE THAT IS IN AN INCIDENT RECORDS NOTHING (ADR 0053
// §4: "A Case already in an Incident gets no Investigation of its own automatically —
// the Incident's covers it"). No automatic trigger reaches a Case today — the Incident
// is the only subject one is raised for — and this is the one door any future one must
// come through, so the rule is held here rather than at each caller. It is not a
// control being hit (§6), so there is no `skipped` row: nobody asked, and the
// Incident's own run is the record of the look the Case got.
func (s *Service) requestCase(
	ctx context.Context, scope db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester, trigger domain.Trigger,
) (domain.Investigation, bool, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigation{}, false, err
	}
	subject, err := s.cases.InvestigationCase(ctx, scope, caseID)
	if err != nil {
		return domain.Investigation{}, false, err
	}
	if trigger.Automatic() {
		holding, err := s.incidents.HoldingIncident(ctx, scope, subject.CaseID)
		if err != nil {
			return domain.Investigation{}, false, err
		}
		if domain.CoveredByIncident(trigger, holding) {
			return domain.Investigation{}, false, nil
		}
	}
	inv, err := s.request(ctx, scope, subjectRef{kind: domain.SubjectCase, id: subject.CaseID, alertKey: subject.AlertKey},
		investigatorID, by, trigger)
	return inv, err == nil, err
}

// RequestIncidentInvestigation records a human's request for one Investigator to run
// against one Incident as a whole, addressed by the number a human quotes, and
// enqueues it — under the kill switch, the daily budget and the concurrency, exactly
// as a Case's request is (RequestCaseInvestigation).
func (s *Service) RequestIncidentInvestigation(
	ctx context.Context, scope db.TenantScope, number int64, investigatorID uuid.UUID, by domain.Requester,
) (domain.Investigation, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigation{}, err
	}
	incident, err := s.incidents.InvestigationIncidentNumbered(ctx, scope, number)
	if err != nil {
		return domain.Investigation{}, err
	}
	return s.request(ctx, scope, incidentRef(incident), investigatorID, by, domain.TriggerHuman)
}

// IncidentChanged is the `investigations.incident` job: one Incident fact — drawn, or
// a Case joining or leaving — becomes at most one run per Investigator that
// investigates Incidents (ADR 0053 §4). It returns how many runs the trigger was
// recorded as or resolved to.
//
// ⭐ WHICH INVESTIGATORS IS THE OPERATOR'S WORD: every Investigator that is switched on
// AND says `investigates_incidents`. An Investigator switched off is not asked at all
// — it is not subscribed — while the org's own switch and daily budget are hit and
// recorded like any request's (`skipped`, with the reason). A draw resolves to the run
// a first delivery made; a membership change coalesces under each Investigator's
// minimum interval (domain.Admit).
//
// An Incident this org no longer has is done, not retried: there is nothing to look at.
func (s *Service) IncidentChanged(
	ctx context.Context, scope db.TenantScope, incidentID uuid.UUID, trigger domain.Trigger,
) (int, error) {
	if err := db.RequireScope(scope); err != nil {
		return 0, err
	}
	if trigger != domain.TriggerDrawn && trigger != domain.TriggerMembership {
		return 0, errs.Validation("investigation_trigger_invalid", "an Incident starts Investigations when it is drawn or its membership changes",
			errs.Violation{Field: "trigger", Code: "enum", Message: string(trigger)})
	}
	// ⭐ THE SUBSCRIBERS FIRST, THE INCIDENT ONLY IF ANYONE IS SUBSCRIBED (review B1). Most
	// orgs opt no Investigator into Incidents, and a Correlator storm is a trigger per
	// membership change: each one reads the Investigator list and stops there.
	investigators, err := s.investigators.List(ctx, scope)
	if err != nil {
		return 0, err
	}
	subscribed := investigators[:0:0]
	for _, inv := range investigators {
		if inv.Enabled && inv.InvestigatesIncidents {
			subscribed = append(subscribed, inv)
		}
	}
	if len(subscribed) == 0 {
		return 0, nil
	}
	// ⛔ AN ORG SWITCHED OFF IS UNSUBSCRIBED TOO (owner ruling O1, 2026-10-05): an automatic
	// trigger leaves no row, exactly as a switched-off Investigator's does. `investigations_enabled`
	// is itself the readable record; a human's request still records `skipped/disabled`.
	if controls, err := s.orgControls.InvestigationControls(ctx, scope); err != nil {
		return 0, err
	} else if !controls.Enabled {
		return 0, nil
	}
	incident, err := s.incidents.InvestigationIncident(ctx, scope, incidentID)
	if errs.IsKind(err, errs.KindNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	by, err := domain.NewRequester(uuid.Nil, triggerLabel(trigger, incident.Number))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, inv := range subscribed {
		if _, err := s.request(ctx, scope, incidentRef(incident), inv.ID, by, trigger); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// triggerLabel is who "asked" for an automatic run: oto, and why — frozen on the run
// as its requester label, so the run says what started it.
func triggerLabel(trigger domain.Trigger, number int64) string {
	if trigger == domain.TriggerDrawn {
		return fmt.Sprintf("oto: Incident #%d was drawn", number)
	}
	return fmt.Sprintf("oto: Incident #%d's membership changed", number)
}

// subjectRef is what a request needs to know about its subject.
type subjectRef struct {
	kind domain.SubjectKind
	id   uuid.UUID
	// alertKey is a Case's Alert's key; "" for an Incident.
	alertKey string
	// window is a digest subject's window — the other half of its identity, `id` being
	// the policy — and zero for every other subject.
	window domain.DigestWindow
}

func incidentRef(i domain.IncidentSubject) subjectRef {
	return subjectRef{kind: domain.SubjectIncident, id: i.IncidentID}
}

// request records one trigger's run against one subject. A human's is always a new
// run; a draw or a membership change is admitted under the Investigator's minimum
// interval and may resolve to a run already made (domain.Admit); a digest window is one
// run per window, which `investigations_digest_window_uniq` holds (domain.Trigger.
// Coalesces).
func (s *Service) request(
	ctx context.Context, scope db.TenantScope, subj subjectRef, investigatorID uuid.UUID, by domain.Requester, trigger domain.Trigger,
) (domain.Investigation, error) {
	investigator, err := s.investigators.Get(ctx, scope, investigatorID)
	if err != nil {
		return domain.Investigation{}, err
	}
	controls, err := s.orgControls.InvestigationControls(ctx, scope)
	if err != nil {
		return domain.Investigation{}, err
	}

	at := s.now()
	inv := domain.Investigation{
		OrgID:            scope.OrgID(),
		SubjectKind:      subj.kind,
		SubjectID:        subj.id,
		AlertKey:         subj.alertKey,
		DigestWindow:     subj.window,
		InvestigatorID:   investigator.ID,
		InvestigatorName: investigator.Name,
		VersionID:        investigator.Current.ID,
		VersionNumber:    investigator.Current.Number,
		Model:            investigator.Current.Model,
		Status:           domain.StatusQueued,
		Budgets:          investigator.Budgets,
		RequestedBy:      by,
		RequestedAt:      at,
	}
	skip := func(r domain.Reason, why string) {
		inv.Status, inv.Ending, inv.EndedAt, inv.NotBefore = domain.StatusSkipped, domain.EndedBy(r, why), at, time.Time{}
	}
	if off := switchedOff(controls, investigator); off != "" {
		skip(domain.ReasonDisabled, off)
	}

	var out domain.Investigation
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if inv.Status == domain.StatusQueued && trigger.Coalesces() {
			// Under the (Investigator, subject) lock, so two changes at once cannot
			// both find nothing queued and both insert.
			runs, err := s.investigations.LockSubjectRuns(ctx, scope, investigator.ID, inv.SubjectKind, inv.SubjectID)
			if err != nil {
				return err
			}
			adm := domain.Admit(trigger, investigator.MinInterval, runs, at)
			if adm.Coalesced() {
				out = resolvedRun(runs, adm.Onto)
				return nil
			}
			inv.NotBefore = adm.NotBefore
		}
		if inv.Status == domain.StatusQueued {
			spent, err := s.investigations.SpentSince(ctx, scope, domain.DayStart(at))
			if err != nil {
				return err
			}
			if why := controls.BudgetSpent(spent, at); why != "" {
				skip(domain.ReasonBudget, why)
			}
		}
		stored, err := s.investigations.Insert(ctx, scope, inv)
		if err != nil {
			return err
		}
		out = stored
		if stored.Status != domain.StatusQueued {
			return nil
		}
		var opts []db.JobOption
		if !stored.NotBefore.IsZero() {
			opts = append(opts, db.WithScheduledAt(stored.NotBefore))
		}
		_, err = s.queue.Enqueue(ctx, jobs.InvestigationsRunArgs{OrgID: scope.OrgID(), InvestigationID: stored.ID}, opts...)
		return err
	})
	if err != nil {
		return domain.Investigation{}, err
	}
	return out, nil
}

// resolvedRun is the run an admission resolved to: the queued one, or — for a
// redelivered draw — the one that already began.
func resolvedRun(runs domain.SubjectRuns, id uuid.UUID) domain.Investigation {
	if runs.Queued != nil && runs.Queued.ID == id {
		return *runs.Queued
	}
	return *runs.Last
}

// switchedOff returns why a run may not start — the org's switch or the
// Investigator's — or "" when both are on.
func switchedOff(controls domain.OrgControls, investigator domain.Investigator) string {
	switch {
	case !controls.Enabled:
		return "Investigations are switched off for this org (investigations_enabled is false)"
	case !investigator.Enabled:
		return fmt.Sprintf("the Investigator %s is disabled", investigator.Name)
	default:
		return ""
	}
}

// ConcurrencyWait is how long a run's job waits before it looks for a free slot
// again, when the org already has its `investigation_concurrency` running. A snooze,
// not a failure: it consumes no attempt, so a run can wait out any backlog and is
// never dropped (ADR 0053 §6).
const ConcurrencyWait = 15 * time.Second

// The snooze reasons, as `oto_jobs_snoozed_total` labels them.
const (
	snoozeConcurrency = "investigation_concurrency"
	snoozeInterval    = "investigation_interval"
)

// GetInvestigation reads one run and its Steps, in order.
func (s *Service) GetInvestigation(ctx context.Context, scope db.TenantScope, id uuid.UUID) (InvestigationDetail, error) {
	inv, err := s.investigations.Get(ctx, scope, id)
	if err != nil {
		return InvestigationDetail{}, err
	}
	steps, err := s.investigations.Steps(ctx, scope, id)
	if err != nil {
		return InvestigationDetail{}, err
	}
	return InvestigationDetail{Investigation: inv, Steps: steps}, nil
}

// ListCaseInvestigations reads a Case's runs, latest first. A Case this org does not
// have is a 404, like the Case itself.
func (s *Service) ListCaseInvestigations(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID, p db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if _, err := s.cases.InvestigationCase(ctx, scope, caseID); err != nil {
		return nil, db.Cursor{}, err
	}
	return s.investigations.ListBySubject(ctx, scope, domain.SubjectCase, caseID, p)
}

// ListIncidentInvestigations reads an Incident's runs, latest first, by the number a
// human quotes. An Incident this org does not have is a 404, like the Incident itself.
func (s *Service) ListIncidentInvestigations(
	ctx context.Context, scope db.TenantScope, number int64, p db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	incident, err := s.incidents.InvestigationIncidentNumbered(ctx, scope, number)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	return s.investigations.ListBySubject(ctx, scope, domain.SubjectIncident, incident.IncidentID, p)
}

// LatestIncidentFinding is the newest Finding any run reached on the Incident — the
// one its card shows and its `finding` fact carries (ADR 0053 §4: "the latest is
// shown") — and false when none has.
func (s *Service) LatestIncidentFinding(
	ctx context.Context, scope db.TenantScope, incidentID uuid.UUID,
) (domain.PriorFinding, bool, error) {
	found, err := s.investigations.SubjectFindings(ctx, scope, domain.SubjectIncident, []uuid.UUID{incidentID}, uuid.Nil, 1)
	if err != nil || len(found) == 0 {
		return domain.PriorFinding{}, false, err
	}
	return found[0], true, nil
}

// RunInvestigation is the `investigations.run` job: start one queued run, run it, end
// it.
//
// It returns an error only when the run could not be begun or its ending could not be
// recorded — both of which a retry can fix — or a jobs.Snooze when it must wait: for
// a free slot under the org's concurrency, or for the time an interval deferred it
// to. A snooze consumes no attempt, and the run stays `queued`. Every way the RUN
// goes wrong (no usage, a model error, a budget) is an ending, recorded, and a nil
// return.
func (s *Service) RunInvestigation(ctx context.Context, scope db.TenantScope, id uuid.UUID) error {
	inv, err := s.investigations.Get(ctx, scope, id)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return nil // nothing to run: the row is gone with its org or Investigator.
		}
		return err
	}
	switch inv.Status {
	case domain.StatusQueued:
	case domain.StatusRunning:
		// ⛔ A RUN FOUND `running` IS ONE WHOSE WORKER DIED, AND IT IS NOT RE-RUN. Every
		// turn it took was paid for; running it again pays twice and records a second
		// transcript over the first. It ends `interrupted`, with the Steps it wrote —
		// and with what they spent, summed from them, because the run's own counters
		// are written only at its end (review A5).
		spent, calls, err := s.investigations.SpentOn(ctx, scope, inv.ID, answerShapingTools())
		if err != nil {
			return err
		}
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonInterrupted,
			"the worker running this Investigation stopped before it ended; it is not re-run, because a re-run would pay for every turn again"),
			spent: spent, toolCalls: calls})
	default:
		return nil // ended: frozen.
	}
	if inv.SubjectKind == domain.SubjectDigest && !s.now().Before(inv.DigestWindow.End) {
		// ⭐ A DIGEST WINDOW'S RUN STILL WAITING WHEN ITS WINDOW CLOSED IS SKIPPED, ON THE
		// RECORD (owner ruling O4, 2026-10-05). The digest went out at the close with the
		// built-in body — it never waits for a Finding — so a Finding now would be read by
		// nothing, and its tokens would be spent for nothing.
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonWindowClosed, fmt.Sprintf(
			"the digest window closed at %s while this run was still waiting to start; the digest went out without it",
			inv.DigestWindow.End.UTC().Format(time.RFC3339)))})
	}

	investigator, err := s.investigators.Get(ctx, scope, inv.InvestigatorID)
	if err != nil {
		return err
	}
	controls, err := s.orgControls.InvestigationControls(ctx, scope)
	if err != nil {
		return err
	}
	if off := switchedOff(controls, investigator); off != "" {
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonDisabled, off)})
	}
	// ⭐ THE INTERVAL, THEN THE DAY, THEN THE SLOT — in the order each would let it run.
	// A run the interval deferred is not due yet: its job was scheduled for NotBefore,
	// and one that arrives early (a redelivery, a rescue) waits the rest.
	now := s.now()
	if inv.NotBefore.After(now) {
		return jobs.Snooze(inv.NotBefore.Sub(now), snoozeInterval)
	}
	spent, err := s.investigations.SpentSince(ctx, scope, domain.DayStart(now))
	if err != nil {
		return err
	}
	if why := controls.BudgetSpent(spent, now); why != "" {
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonBudget, why)})
	}
	// A peek before the ToolServers are dialled, so a run waiting on a slot does not
	// open a session with every one of them every ConcurrencyWait. Start decides.
	if running, err := s.investigations.CountRunning(ctx, scope); err != nil {
		return err
	} else if controls.AtCapacity(running) {
		return jobs.Snooze(ConcurrencyWait, snoozeConcurrency)
	}

	version, err := s.investigators.GetVersion(ctx, scope, inv.VersionID)
	if err != nil {
		return err
	}
	subject, message, gone, err := s.readSubject(ctx, scope, inv)
	if err != nil {
		return err
	}
	if gone != "" {
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonSubjectGone, gone)})
	}
	model, err := s.OpenProvider(ctx, scope, version.ProviderID)
	if err != nil {
		// An endpoint that cannot be opened — its key will not unseal, the adapter
		// refuses it — will not open on a retry either, and a run left `queued` behind
		// a retrying job is a silence. It ends, saying why.
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonModelError,
			"the model endpoint could not be opened: "+safeMessage(err))})
	}
	if model.Identity() != version.Model {
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonModelChanged, fmt.Sprintf(
			"version %d pinned %s, and its model endpoint now reports %s; write a new version to use it",
			version.Number, version.Model, model.Identity()))})
	}

	// ⭐ THE REDACTION RULES AND THE TOOLSERVER TOOLS ARE READ BEFORE THE RUN STARTS, so
	// a database that cannot answer is a retried job, not a run that began without
	// them. ⛔ A run never proceeds unredacted: no rules read is no run.
	redact, err := s.redaction.ToolResultRedactor(ctx, scope)
	if err != nil {
		return err
	}
	// ⭐ THE CLASS SET IS READ ONCE, HERE, AND IS THE SET THIS RUN'S FINDING IS
	// CLASSIFIED IN (ADR 0053 §5). An operator changing it mid-run changes the next run;
	// this one's word is checked against what it was told.
	classes, err := s.classes.ClassSet(ctx, scope)
	if err != nil {
		return err
	}
	fromServers, unavailable, closeSessions, err := s.toolServerTools(ctx, scope, version.Tools)
	if err != nil {
		return err
	}
	defer closeSessions()
	builtin, inapplicable := s.offeredTools(version.Tools, inv.SubjectKind)
	for name, why := range inapplicable {
		unavailable[name] = why
	}

	startedAt := s.now()
	var start domain.StartOutcome
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		start, err = s.investigations.Start(ctx, scope, inv.ID, startedAt, controls.Concurrency)
		return err
	})
	switch {
	case err != nil:
		return err
	case start == domain.StartAtCapacity:
		return jobs.Snooze(ConcurrencyWait, snoozeConcurrency) // still queued; never dropped
	case start != domain.StartBegan:
		return nil // not started: another worker has it, or it ended meanwhile.
	}

	p := plan{
		model:       model,
		prompt:      version.Prompt,
		subject:     message,
		offered:     append(builtin, fromServers...),
		allow:       version.Tools,
		unavailable: unavailable,
		redact:      redact,
		budgets:     inv.Budgets,
		scope:       scope,
		run:         subject,
		classes:     classes,
	}
	out, err := s.runLoop(ctx, p, startedAt, func(ctx context.Context, step domain.Step) error {
		return s.investigations.AppendStep(ctx, scope, inv.ID, step)
	})
	if err != nil {
		// The loop could not keep its record, or the worker is stopping. Record that
		// on a context the cancellation cannot reach; if even that fails, the retry
		// finds the run `running` and ends it `interrupted`.
		out.ending = domain.EndedBy(domain.ReasonInternal, "the run stopped because oto could not keep its record: "+safeMessage(err))
		if ctx.Err() != nil {
			out.ending = domain.EndedBy(domain.ReasonInterrupted, "the worker running this Investigation was stopped mid-run")
		}
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if ferr := s.finish(bg, scope, inv, out); ferr != nil {
			return err
		}
		return nil
	}
	inv.StartedAt = startedAt
	out.remedyWindow = controls.RemedyWindow()
	return s.finish(ctx, scope, inv, out)
}

// answerShapingTools are the Tools the loop answers itself, which are Steps but not Tool
// calls against the step budget (loop.go).
func answerShapingTools() []string {
	return []string{domain.ClassifyTool, ToolSuggestCountCondition, ToolSuggestMembership, ToolProposeRemedy}
}

// AbandonInvestigation ends a run that its job is giving up on — the last attempt failed,
// or the failure is one no retry can fix — `failed` with reason `internal`, the safe
// sentence of why, and what its Steps spent (review A5, D3). Without it a run whose job
// was discarded stays `queued` (and the UI waits on it forever) or `running` (and holds
// one of the org's concurrency slots forever). A run that has already ended is left as
// it is: its record stands.
func (s *Service) AbandonInvestigation(ctx context.Context, scope db.TenantScope, id uuid.UUID, cause error) error {
	if err := db.RequireScope(scope); err != nil {
		return err
	}
	inv, err := s.investigations.Get(ctx, scope, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if inv.Status.Terminal() {
		return nil
	}
	spent, calls, err := s.investigations.SpentOn(ctx, scope, id, answerShapingTools())
	if err != nil {
		return err
	}
	err = s.investigations.Finish(ctx, scope, id, domain.EndedBy(domain.ReasonInternal,
		"oto stopped retrying this Investigation's job: "+safeMessage(cause)), spent, calls, "", "", s.now())
	if errs.IsKind(err, errs.KindConflict) {
		return nil // it ended meanwhile; that record stands.
	}
	return err
}

// readSubject reads what one run is about and renders the message that tells the
// model so. A subject this org no longer has is not an error: `gone` says which, and
// the run ends `subject_gone`.
func (s *Service) readSubject(
	ctx context.Context, scope db.TenantScope, inv domain.Investigation,
) (run RunSubject, message, gone string, err error) {
	run = RunSubject{InvestigationID: inv.ID, Kind: inv.SubjectKind}
	switch inv.SubjectKind {
	case domain.SubjectDigest:
		run.Digest, err = s.digests.InvestigationDigest(ctx, scope, inv.SubjectID, inv.DigestWindow)
		if errs.IsKind(err, errs.KindNotFound) {
			return run, "", "the notification policy no longer exists, or no longer sends a digest", nil
		}
		return run, renderDigestSubject(run.Digest), "", err
	case domain.SubjectIncident:
		run.Incident, err = s.incidents.InvestigationIncident(ctx, scope, inv.SubjectID)
		if errs.IsKind(err, errs.KindNotFound) {
			return run, "", "the Incident no longer exists", nil
		}
		return run, renderIncidentSubject(run.Incident), "", err
	default:
		run.Case, err = s.cases.InvestigationCase(ctx, scope, inv.SubjectID)
		if errs.IsKind(err, errs.KindNotFound) {
			return run, "", "the Case no longer exists", nil
		}
		return run, renderCaseSubject(run.Case), "", err
	}
}

// offeredTools are the built-in Tools the allowlist names that can read this kind of
// subject, in registration order — and, for each allowlisted one that cannot, why, so
// a call to it is refused with the reason rather than as an unknown name.
func (s *Service) offeredTools(allow domain.Allowlist, kind domain.SubjectKind) ([]Tool, map[string]string) {
	out, inapplicable := []Tool{}, map[string]string{}
	for _, t := range s.tools {
		name := t.Schema().Name
		if !allow.Allows(name) {
			continue
		}
		if st, ok := t.(subjectTool); ok && !st.reads(kind) {
			inapplicable[name] = fmt.Sprintf("it reads a %s, and this Investigation is about %s", st.subjectNoun(), subjectNoun(kind))
			continue
		}
		out = append(out, t)
	}
	return out, inapplicable
}

// subjectNoun is a subject kind with its article, for a sentence.
func subjectNoun(kind domain.SubjectKind) string {
	switch kind {
	case domain.SubjectIncident:
		return "an Incident"
	case domain.SubjectDigest:
		return "a digest window"
	default:
		return "a Case"
	}
}

// finish ends a run and — when it reached a Finding — publishes it, in one
// transaction: the ending and the Enrichment are one fact or neither.
func (s *Service) finish(ctx context.Context, scope db.TenantScope, inv domain.Investigation, out outcome) error {
	at := s.now()
	finding, classification := "", ""
	if out.ending.Status == domain.StatusCompleted || out.ending.Status == domain.StatusExhausted {
		finding = out.finding
	}
	if finding != "" {
		// A class belongs to what was concluded: no Finding, no classification.
		classification = out.classification
	}
	// ⭐ A REMEDY'S TIER IS SET BEFORE THE TRANSACTION (git-bug eb4f21b): the rules are pure,
	// but the risk model is a call to an endpoint, and no row is held while it answers.
	var risks []domain.RemedyRisk
	if finding != "" && len(out.remedies) > 0 {
		var err error
		if risks, err = s.assessRemedies(ctx, scope, out.remedies); err != nil {
			return err
		}
	}
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.investigations.Finish(ctx, scope, inv.ID, out.ending, out.spent, out.toolCalls,
			finding, classification, at); err != nil {
			return err
		}
		if finding == "" {
			// ⭐ NO FINDING, NO SUGGESTION: a proposal belongs to what was concluded, and
			// the Steps still show what the run proposed before it stopped.
			return nil
		}
		if len(out.suggestions) > 0 {
			if err := s.suggestions.InsertSuggestions(ctx, scope, inv.ID, out.suggestions,
				at, at.Add(domain.SuggestionLapse)); err != nil {
				return err
			}
		}
		if len(out.remedies) > 0 {
			// ⭐ NO FINDING, NO REMEDY, for the Suggestion's reason; each one proposed here is
			// declared outbound in this same transaction (git-bug 4148256).
			if err := s.proposeRemedies(ctx, scope, inv, out.remedies, risks, at, out.remedyWindow); err != nil {
				return err
			}
		}
		if inv.SubjectKind == domain.SubjectDigest {
			// ⛔ A DIGEST WINDOW'S FINDING IS NOT PUBLISHED AS AN ENRICHMENT, AND IT IS NOT
			// DECLARED (git-bug 3e96f5a, migration 00102). An Enrichment is keyed by its
			// subject alone and replaced by the next run, so it could not name the window;
			// and nothing is sent because of it. It stays on this row, and the digest tick
			// copies it onto the digest IF it is here when the window closes — the run
			// never reaches the digest, the digest reads the run.
			return nil
		}
		started := inv.StartedAt
		if started.IsZero() {
			started = at
		}
		if inv.SubjectKind == domain.SubjectIncident {
			// ⭐ THE NEW FINDING GOES OUTBOUND AS A FACT ABOUT THE INCIDENT (ADR 0052 §5),
			// in this same transaction: published and declared together, or neither.
			// What a policy does with it is the notification layer's question; nothing
			// about any member Case's own notifications is decided here (ADR 0053 §2).
			if err := s.declarer.DeclareIncidentFinding(ctx, scope, inv.SubjectID, inv.ID); err != nil {
				return err
			}
		}
		return s.findings.PublishFinding(ctx, scope, domain.PublishedFinding{
			InvestigationID: inv.ID,
			SubjectKind:     inv.SubjectKind,
			SubjectID:       inv.SubjectID,
			Enricher:        inv.EnricherName(),
			Version:         inv.VersionNumber,
			VersionID:       inv.VersionID,
			Model:           inv.Model,
			Status:          out.ending.Status,
			Reason:          out.ending.Reason,
			Summary:         finding,
			Classification:  classification,
			Partial:         out.ending.Status == domain.StatusExhausted,
			Spent:           out.spent,
			ToolCalls:       out.toolCalls,
			StartedAt:       started,
			EndedAt:         at,
		})
	})
}

// maxRenderedMembers bounds how many current member Cases the subject message lists.
// A Correlator can grow an Incident past any number a model should be handed at once;
// the rest are counted, and `oto_member_findings` still reads every member.
const maxRenderedMembers = 100

// renderIncidentSubject is the user message for an Incident: the story as oto recorded
// it — its derived state, who drew it, and its member Cases — as JSON, after the
// sentence saying it is to be investigated as a whole.
func renderIncidentSubject(i domain.IncidentSubject) string {
	type member struct {
		CaseNumber int64             `json:"case_number"`
		CaseID     string            `json:"case_id"`
		Alertname  string            `json:"alertname"`
		State      string            `json:"state"`
		Labels     map[string]string `json:"labels"`
		AddedAt    any               `json:"added_at"`
	}
	state := "quiet"
	if i.Active {
		state = "active"
	}
	members, more, left := []member{}, 0, 0
	for _, m := range i.Members {
		switch {
		case !m.Current():
			left++
		case len(members) >= maxRenderedMembers:
			more++
		default:
			members = append(members, member{CaseNumber: m.CaseNumber, CaseID: m.CaseID.String(),
				Alertname: m.Alertname, State: m.State, Labels: m.Labels, AddedAt: timeOrNil(m.AddedAt)})
		}
	}
	b, err := json.Marshal(map[string]any{
		"incident_number":       i.Number,
		"incident_id":           i.IncidentID.String(),
		"state":                 state,
		"drawn_at":              timeOrNil(i.DrawnAt),
		"drawn_by":              i.DrawnBy,
		"member_cases":          members,
		"member_cases_unlisted": more,
		"cases_that_left":       left,
	})
	if err != nil {
		b = []byte(`{}`)
	}
	return fmt.Sprintf("Investigate Incident #%d as a whole: one story drawn over the Cases below, "+
		"which is why none of them is investigated on its own. Earlier Findings about its member "+
		"Cases are what %s reads. This is the Incident as oto recorded it:\n%s", i.Number, ToolMemberFindings, b)
}

// renderCaseSubject is the user message: the Case as oto recorded it, as JSON, after
// one sentence saying what is asked.
func renderCaseSubject(c domain.CaseSubject) string {
	b, err := json.Marshal(map[string]any{
		"case_number": c.Number,
		"case_id":     c.CaseID.String(),
		"alert_id":    c.AlertID.String(),
		"alertname":   c.Alertname,
		"state":       c.State,
		"started_at":  timeOrNil(c.StartedAt),
		"ended_at":    timeOrNil(c.EndedAt),
		"labels":      c.Labels,
		"annotations": c.Annotations,
	})
	if err != nil {
		b = []byte(`{}`)
	}
	return fmt.Sprintf("Investigate Case #%d. This is the Case as oto recorded it:\n%s", c.Number, b)
}
