package app

import (
	"context"
	"testing"

	"github.com/google/uuid"

	incidentsdomain "github.com/thulasiram/oto/internal/incidents/domain"
	incidentsservice "github.com/thulasiram/oto/internal/incidents/service"
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// capturingEnqueuer records what was enqueued, in order.
type capturingEnqueuer struct {
	jobs []db.JobArgs
}

func (e *capturingEnqueuer) Enqueue(_ context.Context, args db.JobArgs, _ ...db.JobOption) (db.EnqueueResult, error) {
	e.jobs = append(e.jobs, args)
	return db.EnqueueResult{}, nil
}

func (e *capturingEnqueuer) EnqueueMany(_ context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	for _, r := range reqs {
		e.jobs = append(e.jobs, r.Args)
	}
	return make([]db.EnqueueResult, len(reqs)), nil
}

// TestAnIncidentFactTriggersItsInvestigationsBesideItsDeclaration — git-bug 74ea849,
// ADR 0053 §4: "an Incident is drawn; its membership changes … Not on quiet." Every
// Incident fact is declared (`notify.incident`), and a draw and a Case joining or
// leaving ALSO enqueue `investigations.incident` — in the same call, so in the same
// transaction — while quiet and active-again enqueue none. The trigger names the
// org and the Incident and nothing the Investigator would have to trust.
func TestAnIncidentFactTriggersItsInvestigationsBesideItsDeclaration(t *testing.T) {
	t.Parallel()
	enq := &capturingEnqueuer{}
	out := incidentAnnouncers{incidentAnnouncer{enq: enq}, incidentInvestigationTriggers{enq: enq}}
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	incident := uuid.New()
	facts := []incidentsservice.Announcement{}
	for _, f := range []incidentsdomain.Fact{
		incidentsdomain.FactDrawn, incidentsdomain.FactCaseAdded, incidentsdomain.FactActiveAgain,
		incidentsdomain.FactCaseRemoved, incidentsdomain.FactQuiet,
	} {
		facts = append(facts, incidentsservice.Announcement{IncidentID: incident, Fact: f, Occasion: uuid.New()})
	}
	if err := out.Announce(context.Background(), scope, facts); err != nil {
		t.Fatal(err)
	}

	var declared, triggers []string
	for _, j := range enq.jobs {
		switch a := j.(type) {
		case jobs.NotifyIncidentArgs:
			declared = append(declared, a.Reason)
		case jobs.InvestigationsIncidentArgs:
			if a.OrgID != scope.OrgID() || a.IncidentID != incident {
				t.Fatalf("a trigger names org %s, Incident %s", a.OrgID, a.IncidentID)
			}
			if q := a.InsertOpts().Queue; q != jobs.QueueLifecycle {
				t.Fatalf("a trigger rides %q, want lifecycle", q)
			}
			triggers = append(triggers, a.Trigger)
		default:
			t.Fatalf("enqueued %#v", j)
		}
	}
	if len(declared) != 5 {
		t.Fatalf("declared %v; every Incident fact is declared, whatever the Investigator does", declared)
	}
	want := []string{string(investigatordomain.TriggerDrawn), string(investigatordomain.TriggerMembership),
		string(investigatordomain.TriggerMembership)}
	if len(triggers) != len(want) {
		t.Fatalf("triggers %v, want %v — quiet and active_again trigger nothing", triggers, want)
	}
	for i := range want {
		if triggers[i] != want[i] {
			t.Fatalf("triggers %v, want %v", triggers, want)
		}
	}
}

// TestAnIncidentsFindingIsDeclaredAsTheFactFinding — ADR 0052 §5's "new Finding",
// keyed on the Investigation as its occasion, so one run is declared once.
func TestAnIncidentsFindingIsDeclaredAsTheFactFinding(t *testing.T) {
	t.Parallel()
	enq := &capturingEnqueuer{}
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	incident, run := uuid.New(), uuid.New()
	if err := (findingDeclarer{enq: enq, seq: fixedSequence(7)}).DeclareIncidentFinding(context.Background(), scope, incident, run); err != nil {
		t.Fatal(err)
	}
	if len(enq.jobs) != 1 {
		t.Fatalf("%d jobs", len(enq.jobs))
	}
	a, ok := enq.jobs[0].(jobs.NotifyIncidentArgs)
	if !ok || a.IncidentID != incident || a.OccasionID != run || a.Reason != string(notifdomain.ReasonFinding) {
		t.Fatalf("declared %#v", enq.jobs[0])
	}
	if r := notifdomain.Reason(a.Reason); !r.Valid() || r.Subject() != notifdomain.SubjectIncident {
		t.Fatalf("%q is not an Incident notification reason", a.Reason)
	}
	// ⭐ Numbered like every other Incident fact (migration 00093): a `finding` without a
	// sequence is one a receiver cannot order against the membership facts around it.
	if a.Sequence != 7 {
		t.Fatalf("the finding fact carries sequence %d, want the Incident's next (7)", a.Sequence)
	}
}

// fixedSequence is an incidentFactSequencer that always hands out the same number.
type fixedSequence int64

func (f fixedSequence) NextFactSequence(context.Context, db.TenantScope, uuid.UUID) (int64, error) {
	return int64(f), nil
}
