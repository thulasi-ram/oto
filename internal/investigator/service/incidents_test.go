package service

// git-bug 74ea849's "Done when", against the scripted model and in-memory ports
// (ADR 0053 §4): drawing an Incident starts one Investigation; membership churn inside
// the minimum interval yields one more; member Cases start none automatically, while
// a human may still ask about one; a Case's pre-existing Finding is readable by the
// Incident's Investigation; the latest Finding is stored as an `incident` Enrichment
// and declared outbound; going quiet triggers nothing. The SQL half — the widened
// CHECKs and the member-Findings read — is `repository/investigations_db_test.go`'s,
// and the trigger's wiring from the Incident's facts is `internal/app`'s.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/modelfake"
)

// incidentInvestigator writes an Investigator with its own endpoint. `incidents` is
// whether Incidents start runs of it on their own.
func (r *rig) incidentInvestigator(t *testing.T, name string, enabled, incidents bool, tools ...string) domain.Investigator {
	t.Helper()
	ctx := context.Background()
	cfg, err := r.svc.CreateProvider(ctx, r.scope, domain.ProviderDraft{
		Name: "gw-" + name, BaseURL: "https://gw.test/v1", Model: "m-1", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	allow, err := domain.NewAllowlist(tools)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := domain.NewVersionSpec(cfg.ID, "You read oto's history and say what the story is.", allow)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := r.svc.CreateInvestigator(ctx, r.scope, domain.InvestigatorDraft{
		Name: name, Enabled: enabled, Budgets: domain.DefaultBudgets(), MinInterval: domain.DefaultMinInterval(),
		InvestigatesIncidents: incidents, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// drawIncident puts an Incident over the given Cases into the Incident reader, and
// marks each Case as held by it.
func (r *rig) drawIncident(cases ...domain.CaseSubject) domain.IncidentSubject {
	i := domain.IncidentSubject{IncidentID: uuid.New(), Number: 4, Active: true,
		DrawnAt: r.clock.Now(), DrawnBy: "a Correlator (payments)"}
	for _, c := range cases {
		i.Members = append(i.Members, domain.IncidentMember{CaseID: c.CaseID, CaseNumber: c.Number,
			Alertname: c.Alertname, Labels: c.Labels, State: c.State, AddedAt: r.clock.Now()})
		r.incidents.holding[c.CaseID] = i.IncidentID
	}
	r.incidents.incidents[i.IncidentID] = i
	return i
}

func (r *rig) changed(t *testing.T, i domain.IncidentSubject, trigger domain.Trigger) int {
	t.Helper()
	n, err := r.svc.IncidentChanged(context.Background(), r.scope, i.IncidentID, trigger)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *rig) incidentRuns(t *testing.T, i domain.IncidentSubject) []domain.Investigation {
	t.Helper()
	runs, _, err := r.svc.ListIncidentInvestigations(context.Background(), r.scope, i.Number, db.Keyset{Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// TestDrawingAnIncidentStartsOneInvestigationPerOptedInInvestigator — §4: "an
// Incident is drawn" is a trigger, and which Investigators it reaches is the
// operator's word: switched on AND `investigates_incidents`. A redelivered draw is
// the same first look, not a second one.
func TestDrawingAnIncidentStartsOneInvestigationPerOptedInInvestigator(t *testing.T) {
	r := newRig(t)
	_, c := r.setup(t, domain.DefaultBudgets()) // `firstlook`: never opted in
	storm := r.incidentInvestigator(t, "storm", true, true, ToolMemberFindings)
	r.incidentInvestigator(t, "dormant", false, true, ToolMemberFindings) // opted in, switched off
	i := r.drawIncident(c)

	if n := r.changed(t, i, domain.TriggerDrawn); n != 1 {
		t.Fatalf("a draw reached %d Investigators, want the one switched on and opted in", n)
	}
	runs := r.incidentRuns(t, i)
	if len(runs) != 1 {
		t.Fatalf("%d runs on the Incident, want 1", len(runs))
	}
	run := runs[0]
	if run.InvestigatorID != storm.ID || run.SubjectKind != domain.SubjectIncident || run.SubjectID != i.IncidentID ||
		run.Status != domain.StatusQueued || run.AlertKey != "" || run.RequestedBy.Label != "oto: Incident #4 was drawn" {
		t.Fatalf("the draw's run = %+v", run)
	}
	if len(r.queue.jobs) != 1 {
		t.Fatalf("%d jobs for one run", len(r.queue.jobs))
	}
	if args, ok := r.queue.jobs[0].(jobs.InvestigationsRunArgs); !ok || args.InvestigationID != run.ID {
		t.Fatalf("enqueued %#v", r.queue.jobs[0])
	}

	// Redelivered — before the run starts, and after it ended: the same run, no job.
	r.changed(t, i, domain.TriggerDrawn)
	r.dial.script = []modelfake.Step{modelfake.Text("One deploy, three symptoms.", 200, 20)}
	r.run(t, run.ID)
	r.changed(t, i, domain.TriggerDrawn)
	if n := len(r.incidentRuns(t, i)); n != 1 || len(r.queue.jobs) != 1 {
		t.Fatalf("a redelivered draw made %d runs and %d jobs, want 1 and 1", n, len(r.queue.jobs))
	}
}

// TestMembershipChurnInsideTheIntervalYieldsOneMore — §6: "Membership-change triggers
// inside the interval coalesce into one run", which then runs once the interval is up.
// (bf172fe's coalescing, driven by the subject it was built for.)
func TestMembershipChurnInsideTheIntervalYieldsOneMore(t *testing.T) {
	r := newRig(t)
	inv := r.incidentInvestigator(t, "storm", true, true, ToolMemberFindings)
	every := 10 * time.Minute
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{MinInterval: &every}); err != nil {
		t.Fatal(err)
	}
	i := r.drawIncident()
	r.dial.script = []modelfake.Step{modelfake.Text("first look", 100, 10)}

	// Drawn: one run, now. A change before it starts: the same run.
	r.changed(t, i, domain.TriggerDrawn)
	first := r.incidentRuns(t, i)[0]
	r.changed(t, i, domain.TriggerMembership)
	if n := len(r.incidentRuns(t, i)); n != 1 || len(r.queue.jobs) != 1 {
		t.Fatalf("a change while the first run was still queued made %d runs, %d jobs", n, len(r.queue.jobs))
	}
	r.run(t, first.ID)
	startedAt := r.clock.Now()

	// Churn two to nine minutes later: ONE follow-up, scheduled for when the interval is up.
	r.clock.Advance(2 * time.Minute)
	r.changed(t, i, domain.TriggerMembership)
	follow := r.incidentRuns(t, i)[0]
	if follow.ID == first.ID || follow.Status != domain.StatusQueued || !follow.NotBefore.Equal(startedAt.Add(every)) ||
		follow.RequestedBy.Label != "oto: Incident #4's membership changed" {
		t.Fatalf("the first change inside the interval = %+v, want a run not before %s", follow, startedAt.Add(every))
	}
	if n := len(r.queue.jobs); n != 2 || !r.queue.opts[1].ScheduledAt.Equal(startedAt.Add(every)) {
		t.Fatalf("%d jobs; the follow-up's is scheduled for %v", n, r.queue.opts[n-1].ScheduledAt)
	}
	for _, d := range []time.Duration{time.Minute, 6 * time.Minute} {
		r.clock.Advance(d)
		r.changed(t, i, domain.TriggerMembership)
	}
	if n := len(r.incidentRuns(t, i)); n != 2 || len(r.queue.jobs) != 2 {
		t.Fatalf("churn inside the interval made %d runs and %d jobs, want 2 and 2", n, len(r.queue.jobs))
	}

	// Its job, arriving early, waits the rest; at the interval, it runs.
	if err := r.svc.RunInvestigation(context.Background(), r.scope, follow.ID); !isSnooze(err) ||
		!strings.Contains(err.Error(), "investigation_interval") {
		t.Fatalf("an early job: err = %v, want an interval snooze", err)
	}
	r.clock.Set(startedAt.Add(every))
	if got, _ := r.run(t, follow.ID); got.Status != domain.StatusCompleted {
		t.Fatalf("the follow-up ended %+v", got.Ending)
	}
}

// TestGoingQuietTriggersNothing — §4: "Not on quiet". The trigger set is closed at
// drawn and membership; anything else is refused before a single run is recorded, and
// the job that carried it is not retried.
func TestGoingQuietTriggersNothing(t *testing.T) {
	r := newRig(t)
	r.incidentInvestigator(t, "storm", true, true, ToolMemberFindings)
	i := r.drawIncident()
	for _, trigger := range []domain.Trigger{"quiet", "active_again", domain.TriggerHuman} {
		n, err := r.svc.IncidentChanged(context.Background(), r.scope, i.IncidentID, trigger)
		if !errs.IsKind(err, errs.KindValidation) || n != 0 {
			t.Fatalf("trigger %q: %d runs, err %v; want a refusal", trigger, n, err)
		}
	}
	if n := len(r.incidentRuns(t, i)); n != 0 || len(r.queue.jobs) != 0 {
		t.Fatalf("a quiet Incident has %d runs and %d jobs", n, len(r.queue.jobs))
	}
}

// TestAnIncidentThatIsGoneStartsNothing — a trigger for an Incident this org no longer
// has is done, not retried.
func TestAnIncidentThatIsGoneStartsNothing(t *testing.T) {
	r := newRig(t)
	r.incidentInvestigator(t, "storm", true, true)
	if n, err := r.svc.IncidentChanged(context.Background(), r.scope, uuid.New(), domain.TriggerDrawn); err != nil || n != 0 {
		t.Fatalf("a gone Incident: %d runs, err %v", n, err)
	}
}

// TestAMemberCaseStartsNothingAutomaticallyButAHumanMayAsk — §4: "A Case already in an
// Incident gets no Investigation of its own automatically — the Incident's covers it."
// No automatic trigger reaches a Case today; requestCase is the door every one must
// come through, and it holds the rule. A human is never covered.
func TestAMemberCaseStartsNothingAutomaticallyButAHumanMayAsk(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	free := c
	free.CaseID, free.Number = uuid.New(), 413
	r.history.cases[free.CaseID] = free
	r.drawIncident(c)
	by, _ := domain.NewRequester(uuid.Nil, "oto")

	for _, trigger := range []domain.Trigger{domain.TriggerDrawn, domain.TriggerMembership} {
		run, recorded, err := r.svc.requestCase(context.Background(), r.scope, c.CaseID, inv.ID, by, trigger)
		if err != nil || recorded || run.ID != uuid.Nil {
			t.Fatalf("an automatic %s trigger on a member Case: recorded %v, run %+v, err %v", trigger, recorded, run, err)
		}
	}
	listed, _, err := r.svc.ListCaseInvestigations(context.Background(), r.scope, c.CaseID, db.Keyset{Limit: 25})
	if err != nil || len(listed) != 0 || len(r.queue.jobs) != 0 {
		t.Fatalf("a member Case has %d runs and %d jobs, err %v", len(listed), len(r.queue.jobs), err)
	}

	// A Case in no Incident is not covered.
	if _, recorded, err := r.svc.requestCase(context.Background(), r.scope, free.CaseID, inv.ID, by, domain.TriggerMembership); err != nil || !recorded {
		t.Fatalf("an automatic trigger on a Case in no Incident: recorded %v, err %v", recorded, err)
	}
	// A human asking about the member Case gets a run about that Case.
	if run := r.request(t, inv, c); run.Status != domain.StatusQueued || run.SubjectID != c.CaseID {
		t.Fatalf("a human's request about a member Case = %+v", run)
	}
}

// TestAnIncidentsRunReadsItsMemberCasesFindingsAndIsDeclared — a Case's pre-existing
// Finding is readable by the Incident's Investigation (`oto_member_findings`); a Tool
// that reads one Case is refused to it with the reason; and its Finding is the
// Incident's Enrichment, declared outbound as the fact `finding` in the same
// transaction — while a Case's Finding is declared nowhere.
func TestAnIncidentsRunReadsItsMemberCasesFindingsAndIsDeclared(t *testing.T) {
	r := newRig(t)
	firstlook, c := r.setup(t, domain.DefaultBudgets())

	// Before the storm: a human asked about the Case, and got a Finding.
	r.dial.script = []modelfake.Step{modelfake.Text("The payments pod is out of memory.", 300, 30)}
	caseRun, _ := r.run(t, r.request(t, firstlook, c).ID)
	if caseRun.Finding == "" || len(r.declarer.declared) != 0 {
		t.Fatalf("the Case's run = %+v; declared %v — a Case's Finding is declared nowhere", caseRun, r.declarer.declared)
	}

	storm := r.incidentInvestigator(t, "storm", true, true, ToolMemberFindings, ToolPriorFindings, ToolCaseTimeline)
	i := r.drawIncident(c)
	r.clock.Advance(time.Minute)
	r.changed(t, i, domain.TriggerDrawn)
	run := r.incidentRuns(t, i)[0]
	r.dial.script = []modelfake.Step{
		modelfake.Calls(500, 30, call("m1", ToolMemberFindings, `{}`), call("t1", ToolCaseTimeline, `{}`)),
		modelfake.Text("One OOM in payments explains all of it.", 700, 40),
	}
	got, steps := r.run(t, run.ID)

	want := "model_turn tool_call:oto_member_findings:ok tool_call:oto_case_timeline:refused model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	if !strings.Contains(steps[1].Result, "The payments pod is out of memory.") ||
		!strings.Contains(steps[1].Result, caseRun.ID.String()) || !strings.Contains(steps[1].Result, `"subject_kind":"case"`) {
		t.Fatalf("the member Findings read answered %q", steps[1].Result)
	}
	if !strings.Contains(steps[2].Result, "it reads a Case, and this Investigation is about an Incident") {
		t.Fatalf("a Case-only Tool on an Incident's run answered %q", steps[2].Result)
	}
	// The model was told about the Incident, and was offered only what can read it.
	reqs := r.dial.model().Requests()
	if subject := reqs[0].Messages[1].Content; !strings.Contains(subject, "Investigate Incident #4 as a whole") ||
		!strings.Contains(subject, c.CaseID.String()) {
		t.Fatalf("the subject message was %q", subject)
	}
	for _, tool := range reqs[0].Tools {
		if tool.Name == ToolCaseTimeline {
			t.Fatal("a Tool that reads one Case was offered to an Incident's run")
		}
	}

	if got.Status != domain.StatusCompleted || got.SubjectKind != domain.SubjectIncident {
		t.Fatalf("the Incident's run = %+v", got)
	}
	f := r.findings.published[len(r.findings.published)-1]
	if f.SubjectKind != domain.SubjectIncident || f.SubjectID != i.IncidentID || f.Enricher != "investigator.storm" ||
		f.Summary != "One OOM in payments explains all of it." || f.InvestigationID != run.ID {
		t.Fatalf("published %+v", f)
	}
	if len(r.declarer.declared) != 1 || r.declarer.declared[0] != [2]uuid.UUID{i.IncidentID, run.ID} {
		t.Fatalf("declared %v, want the Incident's Finding once, keyed on its run", r.declarer.declared)
	}
	latest, ok, err := r.svc.LatestIncidentFinding(context.Background(), r.scope, i.IncidentID)
	if err != nil || !ok || latest.InvestigationID != run.ID || latest.InvestigatorName != storm.Name {
		t.Fatalf("the Incident's latest Finding = %+v, %v, %v", latest, ok, err)
	}

	// The next run on the Incident reads this one's Finding as its own prior.
	again := r.requestIncident(t, storm, i)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("p1", ToolPriorFindings, `{}`)),
		modelfake.Text("Still the OOM.", 100, 10),
	}
	_, steps = r.run(t, again.ID)
	if !strings.Contains(steps[1].Result, "One OOM in payments explains all of it.") ||
		!strings.Contains(steps[1].Result, `"subject_kind":"incident"`) {
		t.Fatalf("the Incident's prior Findings answered %q", steps[1].Result)
	}
}

// TestAHumanMayAskAboutAnIncidentByItsNumber — "a human asks" is a trigger for an
// Incident as for a Case, under the same controls, whether or not the Investigator
// investigates Incidents on its own; an Incident this org does not have is a 404.
func TestAHumanMayAskAboutAnIncidentByItsNumber(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	i := r.drawIncident(c)
	if run := r.requestIncident(t, inv, i); run.Status != domain.StatusQueued || run.SubjectID != i.IncidentID {
		t.Fatalf("a human's request about an Incident = %+v", run)
	}
	by, _ := domain.NewRequester(uuid.New(), "Ada Lovelace")
	if _, err := r.svc.RequestIncidentInvestigation(context.Background(), r.scope, 99, inv.ID, by); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("an unknown Incident: err = %v, want not found", err)
	}

	// The Incident goes before its run starts: the run ends `subject_gone`, saying so.
	run := r.requestIncident(t, inv, i)
	delete(r.incidents.incidents, i.IncidentID)
	got, _ := r.run(t, run.ID)
	if got.Ending.Reason != domain.ReasonSubjectGone || !strings.Contains(got.Ending.Detail, "Incident") {
		t.Fatalf("a run whose Incident went = %+v", got.Ending)
	}
}

func (r *rig) requestIncident(t *testing.T, inv domain.Investigator, i domain.IncidentSubject) domain.Investigation {
	t.Helper()
	by, err := domain.NewRequester(uuid.New(), "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.svc.RequestIncidentInvestigation(context.Background(), r.scope, i.Number, inv.ID, by)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// TestATriggerWithNoSubscriberNeverReadsTheIncident — review B1: a Correlator storm is a
// trigger per membership change, and an org that opted no Investigator into Incidents
// pays for one list of Investigators each, never a read of the Incident.
func TestATriggerWithNoSubscriberNeverReadsTheIncident(t *testing.T) {
	r := newRig(t)
	r.incidentInvestigator(t, "cases-only", true, false)
	r.incidentInvestigator(t, "switched-off", false, true)
	i := r.drawIncident()
	if n := r.changed(t, i, domain.TriggerMembership); n != 0 {
		t.Fatalf("%d runs with no subscriber", n)
	}
	if r.incidents.reads != 0 {
		t.Fatalf("the Incident was read %d time(s) for nobody", r.incidents.reads)
	}
}

// TestASwitchedOffOrgIsUnsubscribedFromIncidents — owner ruling O1: with the org's switch
// off, an Incident trigger leaves no row (the switch is the record), while a human asking
// about the same Incident is still recorded `skipped/disabled`.
func TestASwitchedOffOrgIsUnsubscribedFromIncidents(t *testing.T) {
	r := newRig(t)
	inv := r.incidentInvestigator(t, "storm", true, true, ToolMemberFindings)
	i := r.drawIncident()
	r.orgControls.on = false
	if n := r.changed(t, i, domain.TriggerDrawn); n != 0 {
		t.Fatalf("a switched-off org started %d runs", n)
	}
	if n := len(r.incidentRuns(t, i)); n != 0 {
		t.Fatalf("a switched-off org left %d rows for an automatic trigger", n)
	}
	by, err := domain.NewRequester(uuid.New(), "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.svc.RequestIncidentInvestigation(context.Background(), r.scope, i.Number, inv.ID, by)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.StatusSkipped || run.Ending.Reason != domain.ReasonDisabled {
		t.Fatalf("a human's request with the org off = %+v, want skipped/disabled on the record", run.Ending)
	}
}
