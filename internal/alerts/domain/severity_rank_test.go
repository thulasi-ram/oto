package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/alerts/domain"
)

// TestUnknownSeverityIsOffTheScaleNotAtTheBottom.
//
// ⭐ THE PROPERTY THAT KEEPS ADR 0020 FROM BUYING NOISE. A broadcast cannot be
// un-sent, so `severity_raised` must fire only on a genuine increase. `unknown`
// means "oto has never seen this label" — the absence of a reading, not a
// severity below `info`. Ranking it as zero would make every rule that merely
// GAINS a `severity:` label look like an escalating alert, and every one of those
// would be a `chat.postMessage` into somebody's channel.
func TestUnknownSeverityIsOffTheScaleNotAtTheBottom(t *testing.T) {
	t.Parallel()

	if _, ok := domain.SeverityUnknown.Rank(); ok {
		t.Fatal("unknown is ordered; it must be off the scale")
	}
	if domain.Raised(domain.SeverityUnknown, domain.SeverityCritical) {
		t.Fatal("a label appearing where there was none counts as a rise")
	}
	if domain.Raised(domain.SeverityWarning, domain.SeverityUnknown) {
		t.Fatal("a label disappearing counts as a rise")
	}
}

// TestRaisedOnlyFiresOnAGenuineIncrease.
func TestRaisedOnlyFiresOnAGenuineIncrease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from, to domain.Severity
		want     bool
		why      string
	}{
		{domain.SeverityWarning, domain.SeverityCritical, true, "the ADR's headline case"},
		{domain.SeverityInfo, domain.SeverityWarning, true, "info to warning"},
		{domain.SeverityInfo, domain.SeverityCritical, true, "two steps at once"},
		{domain.SeverityNone, domain.SeverityCritical, true, "an explicit none is a reading"},

		// A DECREASE has no Reason and never will: good news is allowed to arrive
		// quietly, which is exactly what update-in-place is for.
		{domain.SeverityCritical, domain.SeverityWarning, false, "a decrease"},
		{domain.SeverityWarning, domain.SeverityWarning, false, "no change"},

		// `page` and `critical` are two spellings of one severity (§H.2). A rule
		// that switches spelling has not escalated, and must not broadcast.
		{domain.SeverityCritical, domain.SeverityPage, false, "critical to page is a respelling"},
		{domain.SeverityPage, domain.SeverityCritical, false, "page to critical is a respelling"},

		// `none` and `info` render identically (§H.2) and rank identically.
		{domain.SeverityNone, domain.SeverityInfo, false, "none to info is a respelling"},
	}

	for _, tc := range cases {
		if got := domain.Raised(tc.from, tc.to); got != tc.want {
			t.Errorf("Raised(%s, %s) = %v, want %v — %s", tc.from, tc.to, got, tc.want, tc.why)
		}
	}
}
