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
func (s *Service) RequestCaseInvestigation(
	ctx context.Context, scope db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester,
) (domain.Investigation, error) {
	return s.requestCase(ctx, scope, caseID, investigatorID, by, domain.TriggerHuman)
}

// requestCase records one trigger's run against one Case. A human's is always a new
// run; a membership change is admitted under the Investigator's minimum interval and
// may resolve to a run already queued (domain.Admit).
func (s *Service) requestCase(
	ctx context.Context, scope db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester, trigger domain.Trigger,
) (domain.Investigation, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigation{}, err
	}
	investigator, err := s.investigators.Get(ctx, scope, investigatorID)
	if err != nil {
		return domain.Investigation{}, err
	}
	subject, err := s.cases.InvestigationCase(ctx, scope, caseID)
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
		SubjectKind:      domain.SubjectCase,
		SubjectID:        subject.CaseID,
		AlertKey:         subject.AlertKey,
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
		if inv.Status == domain.StatusQueued && trigger != domain.TriggerHuman {
			// Under the (Investigator, subject) lock, so two changes at once cannot
			// both find nothing queued and both insert.
			runs, err := s.investigations.LockSubjectRuns(ctx, scope, investigator.ID, inv.SubjectKind, inv.SubjectID)
			if err != nil {
				return err
			}
			adm := domain.Admit(trigger, investigator.MinInterval, runs, at)
			if adm.Coalesced() {
				out = *runs.Queued
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
		// transcript over the first. It ends `interrupted`, with the Steps it wrote.
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonInterrupted,
			"the worker running this Investigation stopped before it ended; it is not re-run, because a re-run would pay for every turn again")})
	default:
		return nil // ended: frozen.
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
	subject, err := s.cases.InvestigationCase(ctx, scope, inv.SubjectID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonSubjectGone,
				"the Case no longer exists")})
		}
		return err
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
	fromServers, unavailable, closeSessions, err := s.toolServerTools(ctx, scope, version.Tools)
	if err != nil {
		return err
	}
	defer closeSessions()

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
		subject:     renderCaseSubject(subject),
		offered:     append(s.offeredTools(version.Tools), fromServers...),
		allow:       version.Tools,
		unavailable: unavailable,
		redact:      redact,
		budgets:     inv.Budgets,
		scope:       scope,
		run:         RunSubject{InvestigationID: inv.ID, Case: subject},
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
	return s.finish(ctx, scope, inv, out)
}

// offeredTools are the built-in Tools the allowlist names, in allowlist order.
func (s *Service) offeredTools(allow domain.Allowlist) []Tool {
	out := []Tool{}
	for _, t := range s.tools {
		if allow.Allows(t.Schema().Name) {
			out = append(out, t)
		}
	}
	return out
}

// finish ends a run and — when it reached a Finding — publishes it, in one
// transaction: the ending and the Enrichment are one fact or neither.
func (s *Service) finish(ctx context.Context, scope db.TenantScope, inv domain.Investigation, out outcome) error {
	at := s.now()
	finding := ""
	if out.ending.Status == domain.StatusCompleted || out.ending.Status == domain.StatusExhausted {
		finding = out.finding
	}
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.investigations.Finish(ctx, scope, inv.ID, out.ending, out.spent, out.toolCalls, finding, at); err != nil {
			return err
		}
		if finding == "" {
			return nil
		}
		started := inv.StartedAt
		if started.IsZero() {
			started = at
		}
		return s.findings.PublishFinding(ctx, scope, domain.PublishedFinding{
			InvestigationID: inv.ID,
			CaseID:          inv.SubjectID,
			Enricher:        inv.EnricherName(),
			Version:         inv.VersionNumber,
			VersionID:       inv.VersionID,
			Model:           inv.Model,
			Status:          out.ending.Status,
			Reason:          out.ending.Reason,
			Summary:         finding,
			Partial:         out.ending.Status == domain.StatusExhausted,
			Spent:           out.spent,
			ToolCalls:       out.toolCalls,
			StartedAt:       started,
			EndedAt:         at,
		})
	})
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
