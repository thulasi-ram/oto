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
// The returned run is `queued` — or `skipped` with reason `disabled` when the org's or
// the Investigator's kill switch is off, in which case nothing is enqueued and the row
// is the record that somebody asked.
func (s *Service) RequestCaseInvestigation(
	ctx context.Context, scope db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester,
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
	off, err := s.switchedOff(ctx, scope, investigator)
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
	if off != "" {
		inv.Status = domain.StatusSkipped
		inv.Ending = domain.EndedBy(domain.ReasonDisabled, off)
		inv.EndedAt = at
	}

	var out domain.Investigation
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		stored, err := s.investigations.Insert(ctx, scope, inv)
		if err != nil {
			return err
		}
		out = stored
		if stored.Status != domain.StatusQueued {
			return nil
		}
		_, err = s.queue.Enqueue(ctx, jobs.InvestigationsRunArgs{OrgID: scope.OrgID(), InvestigationID: stored.ID})
		return err
	})
	if err != nil {
		return domain.Investigation{}, err
	}
	return out, nil
}

// switchedOff returns why a run may not start — the org's switch or the
// Investigator's — or "" when both are on.
func (s *Service) switchedOff(ctx context.Context, scope db.TenantScope, investigator domain.Investigator) (string, error) {
	on, err := s.orgSwitch.InvestigationsEnabled(ctx, scope)
	if err != nil {
		return "", err
	}
	switch {
	case !on:
		return "Investigations are switched off for this org (investigations_enabled is false)", nil
	case !investigator.Enabled:
		return fmt.Sprintf("the Investigator %s is disabled", investigator.Name), nil
	default:
		return "", nil
	}
}

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
// recorded — both of which a retry can fix. Every way the RUN goes wrong (no usage, a
// model error, a budget) is an ending, recorded, and a nil return.
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
	if off, err := s.switchedOff(ctx, scope, investigator); err != nil {
		return err
	} else if off != "" {
		return s.finish(ctx, scope, inv, outcome{ending: domain.EndedBy(domain.ReasonDisabled, off)})
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

	startedAt := s.now()
	started, err := s.investigations.Start(ctx, scope, inv.ID, startedAt)
	if err != nil || !started {
		return err // not started: another worker has it, or it ended meanwhile.
	}

	p := plan{
		model:   model,
		prompt:  version.Prompt,
		subject: renderCaseSubject(subject),
		offered: s.offeredTools(version.Tools),
		allow:   version.Tools,
		budgets: inv.Budgets,
		scope:   scope,
		run:     RunSubject{InvestigationID: inv.ID, Case: subject},
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
