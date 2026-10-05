package service

// git-bug a53c8b0's "Done when" (ADR 0054 §6): an executed Remedy enqueues ONE Investigation of
// its Incident, by the Investigator that proposed it, in the transaction that records it
// executed; a failed one enqueues none; a redelivered execution job does not double it; and the
// §6 controls hold — a switched-off Investigator or org records nothing (owner ruling O1), a
// spent day records `skipped`/`budget`, and a queued run coalesces it. A Remedy on a Case no
// Incident holds is followed up on the Case. No Docker.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// runsOn are every run recorded against one subject.
func (r *rig) runsOn(kind domain.SubjectKind, id uuid.UUID) []domain.Investigation {
	r.investigations.mu.Lock()
	defer r.investigations.mu.Unlock()
	var out []domain.Investigation
	for _, inv := range r.investigations.rows {
		if inv.SubjectKind == kind && inv.SubjectID == id {
			out = append(out, inv)
		}
	}
	return out
}

// runJobs counts the `investigations.run` jobs enqueued for one run.
func (r *rig) runJobs(id uuid.UUID) int {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	n := 0
	for _, j := range r.queue.jobs {
		if a, ok := j.(jobs.InvestigationsRunArgs); ok && a.InvestigationID == id {
			n++
		}
	}
	return n
}

// TestAnExecutedRemedyIsFollowedByOneInvestigationOfItsIncident — by the Investigator that
// proposed it, queued and enqueued; the job delivered again adds nothing.
func TestAnExecutedRemedyIsFollowedByOneInvestigationOfItsIncident(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	proposing, err := r.investigations.Get(context.Background(), r.scope, rem.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 0 {
		t.Fatalf("%d Incident runs before the execution", n)
	}

	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	runs := r.runsOn(domain.SubjectIncident, incident.IncidentID)
	if len(runs) != 1 {
		t.Fatalf("an executed Remedy raised %d Incident runs, want 1", len(runs))
	}
	follow := runs[0]
	if follow.Status != domain.StatusQueued || follow.InvestigatorID != proposing.InvestigatorID ||
		!strings.Contains(follow.RequestedBy.Label, "Remedy on Incident #7 was executed") || follow.RequestedBy.UserID != uuid.Nil {
		t.Fatalf("the follow-up = %+v", follow)
	}
	if n := r.runJobs(follow.ID); n != 1 {
		t.Fatalf("the follow-up has %d run jobs, want 1", n)
	}

	// ⛔ A redelivered execution job finds the Remedy executed: no second call, no second look.
	for i := 0; i < 3; i++ {
		if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 1 || r.runJobs(follow.ID) != 1 || rc.calls() != 1 {
		t.Fatalf("redelivery: %d runs, %d jobs, %d calls", n, r.runJobs(follow.ID), rc.calls())
	}
}

// TestAFollowUpThatCannotBeAskedForNeverCostsTheRecord — judgment 2, C6: the record of what the
// write Tool answered commits in its own transaction, so a follow-up that fails leaves the Remedy
// `executed` with its result, the job answers nil (a retry would find it executed anyway), and
// the Tool was called once.
func TestAFollowUpThatCannotBeAskedForNeverCostsTheRecord(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	r.orgControls.err = errs.New(errs.KindUnavailable, "db_down", "the database is not answering")

	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatalf("a failed follow-up failed the execution job: %v", err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyExecuted || !strings.Contains(got.Result, "restarted") {
		t.Fatalf("the record of a write that was made was lost: state %s, result %q", got.State, got.Result)
	}
	if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 0 || rc.calls() != 1 {
		t.Fatalf("%d follow-up runs, %d calls", n, rc.calls())
	}
	// ⛔ Redelivered, it neither calls again nor asks again.
	r.orgControls.err = nil
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 0 || rc.calls() != 1 {
		t.Fatalf("redelivery: %d follow-up runs, %d calls", n, rc.calls())
	}
}

// TestAFailedRemedyIsFollowedByNothing — a Tool error, and a record the sweep made after the
// worker died, each raise no follow-up: nothing changed that a look could judge.
func TestAFailedRemedyIsFollowedByNothing(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool(`deployments.apps "api" is forbidden`, true))
	jobsBefore := len(r.queue.jobs)
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.remedyNow(t, rem.ID); got.State != domain.RemedyFailed {
		t.Fatalf("state %s", got.State)
	}
	if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 0 || len(r.queue.jobs) != jobsBefore {
		t.Fatalf("a failed Remedy raised %d runs and %d jobs", n, len(r.queue.jobs)-jobsBefore)
	}
}

// TestASwitchedOffInvestigatorOrOrgIsUnsubscribedFromTheFollowUp — owner ruling O1: no row.
func TestASwitchedOffInvestigatorOrOrgIsUnsubscribedFromTheFollowUp(t *testing.T) {
	for name, off := range map[string]func(r *rig, investigatorID uuid.UUID){
		"investigator": func(r *rig, id uuid.UUID) {
			no := false
			if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, id, domain.InvestigatorChange{Enabled: &no}); err != nil {
				t.Fatal(err)
			}
		},
		"org": func(r *rig, _ uuid.UUID) { r.orgControls.on = false },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			rc := &recorder{}
			rem, _, incident, _ := r.approvedRemedy(t, rc.tool("restarted", false))
			proposing, _ := r.investigations.Get(context.Background(), r.scope, rem.InvestigationID)
			off(r, proposing.InvestigatorID)
			if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
				t.Fatal(err)
			}
			if r.remedyNow(t, rem.ID).State != domain.RemedyExecuted {
				t.Fatal("the switch stopped the Remedy's own record")
			}
			if n := len(r.runsOn(domain.SubjectIncident, incident.IncidentID)); n != 0 {
				t.Fatalf("a switched-off %s recorded %d follow-up runs, want none", name, n)
			}
		})
	}
}

// TestASpentDayRecordsTheFollowUpSkipped — §6: the daily budget is hit on the record, never
// queued; the Remedy's own record stands.
func TestASpentDayRecordsTheFollowUpSkipped(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	r.orgControls.dailyTokens = 1 // the proposing run's own turns spent it
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	runs := r.runsOn(domain.SubjectIncident, incident.IncidentID)
	if len(runs) != 1 || runs[0].Status != domain.StatusSkipped || runs[0].Ending.Reason != domain.ReasonBudget ||
		r.runJobs(runs[0].ID) != 0 {
		t.Fatalf("a follow-up on a spent day = %+v", runs)
	}
	if r.remedyNow(t, rem.ID).State != domain.RemedyExecuted {
		t.Fatal("a spent day stopped the Remedy's own record")
	}
}

// TestAQueuedRunCoalescesTheFollowUp — a run of the same Investigator on the Incident that has
// not started will read it after the change: the follow-up is that run.
func TestAQueuedRunCoalescesTheFollowUp(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	proposing, _ := r.investigations.Get(context.Background(), r.scope, rem.InvestigationID)
	by, _ := domain.NewRequester(uuid.Nil, "oto: Incident #7's membership changed")
	queued, err := r.svc.request(context.Background(), r.scope, incidentRef(incident), proposing.InvestigatorID, by, domain.TriggerMembership)
	if err != nil || queued.Status != domain.StatusQueued {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	if runs := r.runsOn(domain.SubjectIncident, incident.IncidentID); len(runs) != 1 || runs[0].ID != queued.ID ||
		r.runJobs(queued.ID) != 1 {
		t.Fatalf("the follow-up did not coalesce into the queued run: %+v", runs)
	}
}

// TestARemedyOnACaseNoIncidentHoldsIsFollowedUpOnTheCase — its own subject, whose timeline is
// where "did it help" is read.
func TestARemedyOnACaseNoIncidentHoldsIsFollowedUpOnTheCase(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	_, cfg := r.withWriteServer(t, rc.tool("restarted", false))
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c, restartProposal(proposedArgs))
	ctx := context.Background()
	for _, who := range []string{"Ada Lovelace", "Grace Hopper"} {
		if _, err := r.svc.ApproveRemedy(ctx, r.scope, list[0].ID, r.grant(cfg.ID, who), list[0].ArgumentsSHA256); err != nil {
			t.Fatal(err)
		}
	}
	before := len(r.runsOn(domain.SubjectCase, c.CaseID)) // the proposing run
	if err := r.svc.ExecuteRemedy(ctx, r.scope, list[0].ID); err != nil {
		t.Fatal(err)
	}
	runs := r.runsOn(domain.SubjectCase, c.CaseID)
	if len(runs) != before+1 {
		t.Fatalf("%d Case runs after the execution, want %d", len(runs), before+1)
	}
	for _, run := range runs {
		if run.ID != list[0].InvestigationID {
			if run.InvestigatorID != inv.ID || !strings.Contains(run.RequestedBy.Label, "Remedy on this Case was executed") {
				t.Fatalf("the Case follow-up = %+v", run)
			}
		}
	}
}
