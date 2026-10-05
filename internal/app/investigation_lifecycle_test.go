package app

// Pure, no database: the two app-side halves of the Investigator review's lifecycle
// fixes (A5/D3 and B2/D15).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	incidentsdomain "github.com/thulasiram/oto/internal/incidents/domain"
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

type countingAbandoner struct{ abandoned []uuid.UUID }

func (a *countingAbandoner) AbandonInvestigation(_ context.Context, _ db.TenantScope, id uuid.UUID, _ error) error {
	a.abandoned = append(a.abandoned, id)
	return nil
}

// TestAJobThatGivesUpEndsItsRun — review A5 / D3: on its last attempt, or on an error no
// retry can fix, the job ends its run on the record before River discards it; a retry
// still to come, a snooze or a success leaves the run alone.
func TestAJobThatGivesUpEndsItsRun(t *testing.T) {
	t.Parallel()
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	transient := errs.Internal("db_down", errors.New("connection refused"))
	for name, tc := range map[string]struct {
		last    bool
		err     error
		abandon bool
	}{
		"a retry is still to come":       {last: false, err: transient},
		"the last attempt failed":        {last: true, err: transient, abandon: true},
		"no retry can fix it":            {last: false, err: errs.NotFound("investigator_not_found", "gone"), abandon: true},
		"a validation error is terminal": {last: false, err: errs.Validation("x", "bad"), abandon: true},
		"a snooze on the last attempt":   {last: true, err: jobs.Snooze(15*time.Second, "investigation_concurrency")},
		"it succeeded":                   {last: true},
	} {
		t.Run(name, func(t *testing.T) {
			a := &countingAbandoner{}
			id := uuid.New()
			got := abandonOnGivingUp(context.Background(), a, scope, id, tc.last, tc.err)
			if (len(a.abandoned) == 1) != tc.abandon {
				t.Fatalf("abandoned %v, want %v", a.abandoned, tc.abandon)
			}
			if (got == nil) != (tc.err == nil) {
				t.Fatalf("returned %v for %v: the dead-letter must still see the failure", got, tc.err)
			}
		})
	}
}

type stubIncidentDetails struct{}

func (stubIncidentDetails) GetByID(context.Context, db.TenantScope, uuid.UUID) (incidentsdomain.Detail, error) {
	return incidentsdomain.Detail{}, nil
}

func (stubIncidentDetails) ConversationFor(context.Context, db.TenantScope, uuid.UUID) (incidentsdomain.Ref, bool, error) {
	return incidentsdomain.Ref{}, false, nil
}

type failingFindings struct{}

func (failingFindings) LatestIncidentFinding(context.Context, db.TenantScope, uuid.UUID) (investigatordomain.PriorFinding, bool, error) {
	return investigatordomain.PriorFinding{}, false, errs.Internal("investigations_down", errors.New("relation is locked"))
}

// TestAnUnreadableFindingNeverFailsAnIncidentsNotification — review B2 / D15: the
// Incident reader sits on the notification evaluation and claim path, so an
// Investigation table that cannot answer leaves the card without a Finding; it never
// fails or retries the Incident's deliveries.
func TestAnUnreadableFindingNeverFailsAnIncidentsNotification(t *testing.T) {
	t.Parallel()
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	r := &incidentFacts{svc: stubIncidentDetails{}, investigations: failingFindings{}}
	facts, err := r.Incident(context.Background(), scope, uuid.New())
	if err != nil {
		t.Fatalf("a Finding read failure failed the Incident: %v", err)
	}
	if facts.Finding != nil {
		t.Fatalf("finding = %+v, want none", facts.Finding)
	}
}
