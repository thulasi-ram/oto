package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ---------------------------------------------------------------------------
// Done-when 6 (git-bug 121e569): a comment is written ABOUT THE SIGNAL and
// LANDS IN THE FIRING IT WAS TYPED DURING.
//
// ⭐⭐ THE ENDPOINT STAYS ALERT-ADDRESSED AND THE SUBJECT IS STILL DERIVED.
// `POST /api/v1/alerts/{id}/comments` is the only way to say something, because
// a human types "restarted it" about the thing that is broken, not about an
// episode id they have never seen. Which episode was running when they pressed
// send is a fact oto already holds — `alerts.current_case_id` — so it is READ,
// not asked for. A second, case-addressed endpoint would have made the caller
// answer a question the database can answer better, and would have let two
// clients disagree about which firing a sentence belongs to.
//
// ⭐ AND THE NULL IS LOAD-BEARING. A comment typed while nothing is firing has
// no episode to belong to and must carry `alert_id` alone. Attaching it to the
// most recent CLOSED case would put words inside an outage that had already
// ended — the timeline would show an operator commenting on an incident nobody
// was in. `case_id IS NULL` is the honest record of "said about the signal,
// during no firing", and it is what keeps the case timeline a record of one
// episode rather than a filter that leaks.
//
// This runs the REAL service over a REAL Postgres because the claim is about a
// column: a fake events port would agree with whatever the service handed it,
// and `alert_events.case_id` is exactly what nobody had verified was populated.
// ---------------------------------------------------------------------------

// comment drives the production verb once, unkeyed, as a human.
func (f *fixture) comment(alertID uuid.UUID, body string) domain.Event {
	f.t.Helper()

	actor, err := domain.NewActor(domain.ActorUser, uuid.NewString(), "Ram")
	if err != nil {
		f.t.Fatalf("actor: %v", err)
	}
	ev, replayed, err := f.svc.Comment(f.t.Context(), f.scope, alertID, actor, body, Idempotency{})
	if err != nil {
		f.t.Fatalf("comment %q: %v", body, err)
	}
	if replayed {
		f.t.Fatalf("comment %q was replayed; an unkeyed call has nothing to replay", body)
	}
	return ev
}

// storedCaseID reads `alert_events.case_id` back out of the table, which is the
// only thing this test actually trusts. nil means the column is NULL.
func (f *fixture) storedCaseID(eventID uuid.UUID) *uuid.UUID {
	f.t.Helper()

	var caseID *uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT case_id FROM alert_events WHERE org_id = $1 AND id = $2`,
		f.orgID, eventID).Scan(&caseID); err != nil {
		f.t.Fatalf("read alert_events.case_id: %v", err)
	}
	return caseID
}

// timelineBodies returns the comment bodies one timeline page carries, so an
// assertion can name the sentence rather than an event id.
func timelineBodies(t *testing.T, evs []domain.Event) []string {
	t.Helper()

	var out []string
	for _, ev := range evs {
		if ev.Type() != domain.EventCommentAdded {
			continue
		}
		body, _ := ev.Payload()["body"].(string)
		out = append(out, body)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// TestCommentCarriesTheOpenCase is Done-when 6.
//
// It says the whole clause in one run: a comment made while an episode is open
// carries that episode; a comment made after it closed carries none; the CASE
// timeline shows only the first; the ALERT timeline shows both.
func TestCommentCarriesTheOpenCase(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, now)

	firstCase := f.openFiring(now.Add(-time.Minute), time.Time{})
	alertID := firstCase.AlertID()

	// ---- during the firing -------------------------------------------------
	during := f.comment(alertID, "restarted the checkout pods")

	if during.CaseID() != firstCase.ID() {
		t.Fatalf("a comment made while case %s was open carries case_id %v; "+
			"Done-when 6 requires it to name the open episode",
			firstCase.ID(), during.CaseID())
	}
	if got := f.storedCaseID(during.ID()); got == nil || *got != firstCase.ID() {
		t.Fatalf("alert_events.case_id is %v, want %s. The domain event carried the "+
			"case and the row did not, which means the write path dropped it.",
			got, firstCase.ID())
	}

	// ---- close the episode -------------------------------------------------
	f.clk.Advance(time.Minute)
	resolvedAt := f.clk.Now()
	obs := f.observation(domain.ObservedByIngest, "resolved",
		resolvedAt, now.Add(-time.Minute), resolvedAt)
	if _, err := f.svc.ObserveBatch(t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		t.Fatalf("resolve the case: %v", err)
	}
	if ac := f.currentCase(); !ac.State().IsClosed() {
		t.Fatalf("the fixture case is still %v; the rest of this test needs it closed", ac.State())
	}

	// ---- after it closed ---------------------------------------------------
	f.clk.Advance(time.Minute)
	after := f.comment(alertID, "root cause was a bad config push")

	if after.CaseID() != uuid.Nil {
		t.Fatalf("a comment made while nothing is firing carries case_id %v; it must "+
			"carry alert_id alone, or the words land inside an outage that had ended",
			after.CaseID())
	}
	if got := f.storedCaseID(after.ID()); got != nil {
		t.Fatalf("alert_events.case_id is %s for a comment made with no open case; want NULL", *got)
	}

	// ---- the two timelines -------------------------------------------------
	window := db.TimeWindow{From: now.Add(-time.Hour), To: f.clk.Now().Add(time.Hour)}

	caseTL, err := f.svc.CaseTimeline(t.Context(), f.scope, firstCase.ID(), window, db.Keyset{Limit: 50})
	if err != nil {
		t.Fatalf("case timeline: %v", err)
	}
	caseBodies := timelineBodies(t, caseTL.Events)
	if !contains(caseBodies, "restarted the checkout pods") {
		t.Errorf("the case timeline does not show what was said DURING that firing; got %v", caseBodies)
	}
	if contains(caseBodies, "root cause was a bad config push") {
		t.Errorf("the case timeline shows a comment made after the episode closed; got %v", caseBodies)
	}

	alertTL, err := f.svc.AlertTimeline(t.Context(), f.scope, alertID, window, db.Keyset{Limit: 50})
	if err != nil {
		t.Fatalf("alert timeline: %v", err)
	}
	alertBodies := timelineBodies(t, alertTL.Events)
	for _, want := range []string{"restarted the checkout pods", "root cause was a bad config push"} {
		if !contains(alertBodies, want) {
			t.Errorf("the alert timeline is missing %q; it must show everything ever said "+
				"about the signal, in or out of a firing. got %v", want, alertBodies)
		}
	}
}
