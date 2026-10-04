package webhookjson_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// TestADigestEnvelopeCarriesItsFindingOnlyWhenItHasOne — git-bug 3e96f5a: the Finding a
// digest carried goes out as `digest.finding`, and a digest without one has no such key
// (the frozen digest goldens are unchanged: the key is additive and `omitempty`).
func TestADigestEnvelopeCarriesItsFindingOnlyWhenItHasOne(t *testing.T) {
	t.Parallel()
	from, to := renderedAt.Add(-70*time.Minute), renderedAt.Add(-5*time.Minute)
	digestOf := func(v *domain.NotificationView) map[string]json.RawMessage {
		t.Helper()
		var env struct {
			Digest map[string]json.RawMessage `json:"digest"`
		}
		if err := json.Unmarshal(render(t, v).Payload, &env); err != nil {
			t.Fatal(err)
		}
		return env.Digest
	}
	if _, ok := digestOf(digestView(4, from, to))["finding"]; ok {
		t.Error("a digest with the built-in body sent a finding")
	}

	v := digestView(4, from, to)
	v.Digest.Finding = &domain.FindingView{
		InvestigationID: "0199a1b2-c3d4-7e5f-8a9b-00000000d16e", Investigator: "digest", Version: 1,
		Summary: "Four crash loops, one deploy.", Partial: true, ConcludedAt: to.Add(-time.Minute),
	}
	raw, ok := digestOf(v)["finding"]
	if !ok {
		t.Fatal("a digest carrying a Finding sent none")
	}
	var f struct {
		Investigator string `json:"investigator"`
		Summary      string `json:"summary"`
		Partial      bool   `json:"partial"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Investigator != "digest" || f.Summary != "Four crash loops, one deploy." || !f.Partial {
		t.Errorf("digest.finding = %s", raw)
	}
}
