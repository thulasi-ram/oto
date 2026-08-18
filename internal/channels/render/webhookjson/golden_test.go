package webhookjson_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/webhookjson"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// SPEC §H: "Every renderer is a pure function with a checked-in
// testdata/*.golden.json." This renderer had none, and the cost was nearly paid
// in full: the `Occurrence`→`Case` rename (ADR 0036) walked straight through the
// OUTBOUND struct tags and changed `occurrence` to `case` and
// `total_occurrences` to `total_cases` under an UNCHANGED `oto.notification.v1`,
// which SPEC §H.10 and SCOPE-BOUNDARY H-2 forbid. Nothing failed, because
// nothing looked.
//
// ⛔ THESE FIXTURES ARE A WIRE CONTRACT, NOT A RENDERING PREFERENCE. A diff here
// is a change every existing webhook consumer will see. The Go field may be
// renamed freely; the KEY may move only under a schema bump.
var update = flag.Bool("update", false, "rewrite the .golden.json fixtures")

var (
	upstreamStart = time.Date(2026, 8, 7, 17, 56, 9, 0, time.UTC)
	otoFirstSeen  = time.Date(2026, 8, 7, 18, 17, 9, 0, time.UTC)
	renderedAt    = upstreamStart.Add(80 * time.Second)
)

// baseView is one generation of one correlation, two members, both firing.
func baseView() *domain.NotificationView {
	caseStart := renderedAt.Add(-300 * time.Millisecond)
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "o1", Slug: "acme", Name: "Acme"},
		Reason: "fired",
		Group: domain.GroupView{
			ID: "019fe297-d84f-7599-b5b2-1f231749104a", GroupKey: "gk_4vbuj1c49jbpf",
			Generation: 1,
			Title:      "OtoSmokeTest",
			// `receiver` is PROVENANCE since ADR 0038, not identity — and it is on
			// this envelope, which is exactly why 00050 refused to drop the column.
			Receiver: "oto-smoke",
			GroupLabels: map[string]string{
				"alertname": "OtoSmokeTest", "cluster": "smoke-test",
			},
			State: "open", Severity: "critical",
			FiringCount: 2, TotalCount: 2,
			StartedAt: upstreamStart, FirstSeenAt: otoFirstSeen, LastActivityAt: renderedAt,
		},
		Alerts: []domain.AlertView{
			{
				ID: "a1", AlertKey: "ak_1", AlertName: "OtoSmokeTest",
				Severity: "critical", Namespace: "observability", Service: "oto-smoke",
				Labels:      map[string]string{"instance": "oto-smoke-b2", "cluster": "smoke-test"},
				State:       "firing",
				FirstSeenAt: otoFirstSeen, LastSeenAt: renderedAt, TotalCases: 1,
			},
			{
				ID: "a2", AlertKey: "ak_2", AlertName: "OtoSmokeTest",
				Severity: "critical", Namespace: "observability", Service: "oto-smoke",
				Labels:      map[string]string{"instance": "oto-smoke-a1", "cluster": "smoke-test"},
				State:       "firing",
				FirstSeenAt: otoFirstSeen, LastSeenAt: renderedAt, TotalCases: 1,
			},
		},
		Case: &domain.CaseView{
			ID: "case1", Seq: 1, State: "firing", AckState: "unacked",
			StartedAt: caseStart, Duration: 300 * time.Millisecond,
		},
		Links: domain.Links{
			Group:    "http://localhost:8080/cases/019fe297-d84f-7599-b5b2-1f231749104a",
			Timeline: "http://localhost:8080/cases/019fe297-d84f-7599-b5b2-1f231749104a/timeline",
		},
		RenderedAt: renderedAt,
	}
}

func render(t *testing.T, v *domain.NotificationView) domain.RenderedMessage {
	t.Helper()
	msg, err := webhookjson.New(clock.New()).Render(context.Background(), v, domain.RenderOptions{
		Mode:         domain.ModePostRoot,
		Verbosity:    domain.VerbosityAll,
		BaseURL:      "http://localhost:8080",
		MaxInstances: 10,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return msg
}

// TestTheRootFiringEnvelopeIsFrozen is the ordinary card: a live generation.
func TestTheRootFiringEnvelopeIsFrozen(t *testing.T) {
	t.Parallel()
	golden(t, "root_firing.golden.json", render(t, baseView()).Payload)
}

// TestTheAllResolvedEnvelopeIsFrozen is the generation on its way out.
func TestTheAllResolvedEnvelopeIsFrozen(t *testing.T) {
	t.Parallel()
	v := baseView()
	v.Reason = "resolved"
	v.Group.State = "closed"
	v.Group.FiringCount = 0
	for i := range v.Alerts {
		v.Alerts[i].State = "resolved"
	}
	v.Case.State = "resolved"
	golden(t, "all_resolved.golden.json", render(t, v).Payload)
}

// TestTheAckedEnvelopeIsFrozen carries the receipt.
//
// ⭐ ACK IS A PROPERTY OF THE CASE, NOT OF THE ALERT (ADR 0036; `alerts.ack_state`
// is dropped in 00049). This fixture is where that shows on the wire.
func TestTheAckedEnvelopeIsFrozen(t *testing.T) {
	t.Parallel()
	v := baseView()
	v.Reason = "acked"
	v.Case.AckState = "acked"
	golden(t, "acked.golden.json", render(t, v).Payload)
}

// TestEveryFixtureValidates runs the renderer's own validator over each frozen
// payload, so a golden that was updated on purpose still has to be legal.
func TestEveryFixtureValidates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"root_firing.golden.json", "all_resolved.golden.json", "acked.golden.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read %s (run with -update first): %v", name, err)
		}
		if err := webhookjson.Validate(json.RawMessage(raw)); err != nil {
			t.Errorf("%s does not satisfy the envelope's own validator: %v", name, err)
		}
	}
}

// TestTheWireSpellingIsTheFrozenOne is the assertion the rename needed and did
// not have. It reads the KEYS, not the Go names.
func TestTheWireSpellingIsTheFrozenOne(t *testing.T) {
	t.Parallel()
	payload := render(t, baseView()).Payload

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if got := string(bytes.Trim(envelope["schema"], `"`)); got != "oto.notification.v1" {
		t.Fatalf("schema is %q — every assertion below is about v1's shape, and a bump has to "+
			"be a deliberate, separately-reviewed change", got)
	}
	if _, ok := envelope["occurrence"]; !ok {
		t.Error("the envelope has no `occurrence` key — under oto.notification.v1 that key is " +
			"frozen. The Go field is `Case` (ADR 0036) and renaming it was right; moving the " +
			"KEY breaks every consumer that already parses this and must wait for a v2")
	}
	if _, ok := envelope["case"]; ok {
		t.Error("the envelope emits `case` under oto.notification.v1 — this is the ADR 0036 " +
			"rename leaking onto the wire, which SPEC §H.10 forbids without a schema bump")
	}
	if !bytes.Contains(payload, []byte(`"total_occurrences"`)) {
		t.Error("the envelope has no `total_occurrences` key — frozen at v1 for the same reason")
	}
	if bytes.Contains(payload, []byte(`"total_cases"`)) {
		t.Error("the envelope emits `total_cases` under oto.notification.v1")
	}
}

// golden compares the rendered payload with its checked-in fixture. `-update`
// rewrites it; a diff is a wire change somebody has to look at on purpose.
func golden(t *testing.T, name string, payload []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, payload, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	pretty.WriteByte('\n')

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/channels/render/webhookjson -update`): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(pretty.Bytes())) {
		t.Errorf("%s changed.\n--- want ---\n%s\n--- got ---\n%s", name, want, pretty.String())
	}
}
