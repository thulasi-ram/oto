package domain

import (
	"testing"
	"time"
)

// TestADigestRunIsArmedItsBudgetAheadAndNeverBeforeHalfTheWindow — git-bug 3e96f5a:
// the lead is the wall-time budget plus the slack, capped at half the window.
func TestADigestRunIsArmedItsBudgetAheadAndNeverBeforeHalfTheWindow(t *testing.T) {
	for _, tc := range []struct {
		window, wall, want time.Duration
	}{
		{time.Hour, 5 * time.Minute, 7 * time.Minute},
		{24 * time.Hour, 30 * time.Minute, 32 * time.Minute},
		{10 * time.Minute, 5 * time.Minute, 5 * time.Minute}, // capped at half
		{5 * time.Minute, 30 * time.Minute, 150 * time.Second},
	} {
		if got := DigestLead(tc.window, tc.wall); got != tc.want {
			t.Errorf("DigestLead(%s, %s) = %s, want %s", tc.window, tc.wall, got, tc.want)
		}
	}

	start := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	w, err := NewDigestWindow(start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lead := 7 * time.Minute
	for at, want := range map[time.Time]bool{
		start:                       false,
		start.Add(52 * time.Minute): false,
		start.Add(53 * time.Minute): true,
		start.Add(59 * time.Minute): true,
		start.Add(time.Hour):        false, // closed: the digest tick's, not a run's
	} {
		if got := w.Armable(at, lead); got != want {
			t.Errorf("Armable(%s) = %v, want %v", at.Format(time.Kitchen), got, want)
		}
	}
	if _, err := NewDigestWindow(start, start); err == nil {
		t.Fatal("an empty window was admitted")
	}
}

// TestOnlyAnEndedDigestRunWithAFindingIsCarried — the binding rule's predicate: queued,
// running, failed and skipped are the built-in body; completed and exhausted are carried.
func TestOnlyAnEndedDigestRunWithAFindingIsCarried(t *testing.T) {
	for status, want := range map[Status]bool{
		StatusQueued: false, StatusRunning: false, StatusFailed: false, StatusSkipped: false,
		StatusCompleted: true, StatusExhausted: true,
	} {
		inv := Investigation{SubjectKind: SubjectDigest, Status: status, Finding: "a summary"}
		if got := inv.CarriedByDigest(); got != want {
			t.Errorf("%s: carried = %v, want %v", status, got, want)
		}
	}
	if (Investigation{SubjectKind: SubjectDigest, Status: StatusCompleted}).CarriedByDigest() {
		t.Error("a run with no Finding was carried")
	}
	if (Investigation{SubjectKind: SubjectCase, Status: StatusCompleted, Finding: "x"}).CarriedByDigest() {
		t.Error("a Case's Finding was carried by a digest")
	}
}
