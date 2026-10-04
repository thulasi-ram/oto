package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// TestADigestCarryingAFindingDrawsItAsItsBody — git-bug 3e96f5a (ADR 0053 §4): a digest
// whose window's run had ended with a Finding when the window closed carries it as its
// body, said the way an Incident's is — who concluded it, as seen at when, partial first
// — with the model's text escaped, and the count and span still beside it. A digest
// without one is the built-in card, byte for byte (the two digest goldens).
func TestADigestCarryingAFindingDrawsItAsItsBody(t *testing.T) {
	t.Parallel()
	plain := renderView(t, digestView(), domain.ModePostRoot)

	v := digestView()
	v.Digest.Finding = &domain.FindingView{
		InvestigationID: "0199a1b2-c3d4-7e5f-8a9b-00000000d16e",
		Investigator:    "digest",
		Version:         3,
		Summary:         "Seven crash loops in <payments>, one deploy & one node.",
		Classification:  "deploy-regression",
		Partial:         true,
		ConcludedAt:     v.Digest.CoveredTo.Add(-time.Minute),
	}
	carried := renderView(t, v, domain.ModePostRoot)
	raw := string(carried.Payload)

	for _, want := range []string{
		"*Partial Finding* by `digest v3`", "as seen at", "classified `deploy-regression`",
		"&lt;payments&gt;", "&amp; one node", "*Digest*", "New cases",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the summarised digest lacks %q: %s", want, raw)
		}
	}
	if strings.Contains(raw, "<payments>") {
		t.Error("the model's text reached the card unescaped")
	}
	if plain.Fallback != carried.Fallback {
		t.Errorf("the Finding changed the push text, which is the count and the span:\n%q\n%q",
			plain.Fallback, carried.Fallback)
	}
	if strings.Contains(string(plain.Payload), "Finding") {
		t.Error("the built-in digest card mentions a Finding")
	}
}
