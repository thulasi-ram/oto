package webhookjson_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
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
			Group:    "http://localhost:8080/groups/019fe297-d84f-7599-b5b2-1f231749104a",
			Timeline: "http://localhost:8080/groups/019fe297-d84f-7599-b5b2-1f231749104a/timeline",
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

// digestView is what `notification/service.ViewService.digest` builds, reproduced
// field-for-field: a Reason, a `DigestView` and a render time, and NOTHING else. No
// org, no group, no member list, no case, no action and no link.
//
// ⛔ THE EMPTINESS IS THE FIXTURE'S POINT, so it must not be "helpfully" filled in. A
// digest view really does arrive here with a zero `GroupView`, and the whole defect
// this file now guards was the renderer projecting that zero as a group.
func digestView(count int, from, to time.Time) *domain.NotificationView {
	return &domain.NotificationView{
		Reason:     "digest",
		Digest:     &domain.DigestView{Count: count, CoveredFrom: from, CoveredTo: to},
		RenderedAt: renderedAt,
	}
}

// TestTheDigestEnvelopeIsFrozen is the arm that did not exist (git-bug `78388fb`).
//
// ⭐⭐ IT IS THE ASSERTION WHOSE ABSENCE COST A RELEASE'S WORTH OF DIGESTS. `78388fb`
// moved the digest headline out of `Group.Title` into `NotificationView.Digest` and
// taught the Slack renderer a digest arm. This renderer was not taught one, kept
// reading `v.Group.Title`, and every digest went to every webhook consumer as
// `"summary": "[UNKNOWN] alert group (digest)"` over a `group` object of zeros and
// `0001-01-01T00:00:00Z` timestamps. Nothing failed, because — exactly as the note at
// the top of this file says about the `occurrence` rename — nothing looked. This
// fixture is the looking.
func TestTheDigestEnvelopeIsFrozen(t *testing.T) {
	t.Parallel()
	// A span deliberately LONGER than any plausible policy window, because a digest's
	// coverage reaches back for stragglers and the fixture must not imply that
	// `covered_to - covered_from` equals `digest_window_s`.
	from := renderedAt.Add(-70 * time.Minute)
	to := renderedAt.Add(-5 * time.Minute)
	golden(t, "digest.golden.json", render(t, digestView(4, from, to)).Payload)
}

// TestTheSpanlessDigestEnvelopeIsFrozen is the pre-00070 row: a count oto is sure of
// over a window it cannot name.
//
// ⛔ IT EXISTS TO FREEZE AN ABSENCE. The failure mode it guards is not a wrong number,
// it is a PLAUSIBLE one — a zero `time.Time` marshals as `0001-01-01T00:00:00Z`, which
// is well-formed, UTC, and passes the envelope's own W2 check, so a consumer would
// store a span in the year 1 and never know. The keys must be missing, and `count`
// must still be there: the count is recorded on every digest row ever written and is
// not in doubt.
func TestTheSpanlessDigestEnvelopeIsFrozen(t *testing.T) {
	t.Parallel()
	golden(t, "digest_no_span.golden.json", render(t, digestView(1, time.Time{}, time.Time{})).Payload)
}

// TestADigestAssertsNoGroup reads the KEYS, which is where the regression lived.
//
// A `summary` assertion alone would not have caught it: the old output's summary was
// wrong but non-empty, and the `group` object beside it was a well-formed lie. What
// makes a digest envelope correct is a `digest` key present and a `group` key ABSENT —
// the two are alternatives, and a consumer branches on exactly that.
func TestADigestAssertsNoGroup(t *testing.T) {
	t.Parallel()
	from := renderedAt.Add(-70 * time.Minute)
	payload := render(t, digestView(4, from, renderedAt.Add(-5*time.Minute))).Payload

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if _, ok := envelope["digest"]; !ok {
		t.Error("a digest envelope has no `digest` key — the renderer is not reading " +
			"`NotificationView.Digest`, which is the whole of git-bug `78388fb`")
	}
	if _, ok := envelope["group"]; ok {
		t.Errorf("a digest envelope carries a `group` key: %s. A digest view has no group "+
			"(`ViewService.digest`), so anything under that key is a zero `GroupView` "+
			"projected as fact — `state: \"\"`, every count 0, and two timestamps in the "+
			"year 1", envelope["group"])
	}
	// The span is read as a HALF-OPEN pair rather than as prose, because the convention
	// is what a consumer's arithmetic depends on: `covered_to` is EXCLUSIVE
	// (`notifications_digcover_ck`), so consecutive windows abut and never overlap. It
	// is checked against the two instants the view was given, so a renderer that
	// rounded, re-derived or swapped them fails here.
	var digest struct {
		Count       int        `json:"count"`
		CoveredFrom *time.Time `json:"covered_from"`
		CoveredTo   *time.Time `json:"covered_to"`
		SpanSeconds *float64   `json:"span_seconds"`
	}
	if err := json.Unmarshal(envelope["digest"], &digest); err != nil {
		t.Fatalf("unmarshal digest: %v", err)
	}
	if digest.Count != 4 {
		t.Errorf("digest count is %d, want 4 — the number a digest asserts is the one fact "+
			"it cannot get wrong", digest.Count)
	}
	if digest.CoveredFrom == nil || !digest.CoveredFrom.Equal(from) {
		t.Errorf("covered_from is %v, want %v", digest.CoveredFrom, from)
	}
	if digest.CoveredTo == nil || !digest.CoveredTo.Equal(renderedAt.Add(-5*time.Minute)) {
		t.Errorf("covered_to is %v, want %v", digest.CoveredTo, renderedAt.Add(-5*time.Minute))
	}
	// 65 minutes, SUBTRACTED FROM THE TWO ENDS and never read from the policy — that is
	// the point of storing both, and it is why the span here is longer than any window
	// a policy is likely to declare.
	if digest.SpanSeconds == nil || *digest.SpanSeconds != 65*60 {
		t.Errorf("span_seconds is %v, want %v — it must be `covered_to - covered_from` and "+
			"never a policy's current window", digest.SpanSeconds, 65*60)
	}
	// `summary` must not fall through to `summarise`'s group defaults. The exact
	// sentence is frozen in the fixture; this checks the one substring whose presence
	// proved the digest arm ran at all.
	var summary string
	if err := json.Unmarshal(envelope["summary"], &summary); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if !strings.HasPrefix(summary, "[DIGEST] ") {
		t.Errorf("summary is %q — a digest that reads `[UNKNOWN] alert group` is "+
			"`summarise` running on a view with no group, which is the regression", summary)
	}
	// Every " to " in the sentence must be an "up to " — that is the whole assertion,
	// and it is written as a count rather than as a substring search because "up to "
	// itself contains " to ".
	if !strings.Contains(summary, "up to ") ||
		strings.Count(summary, " to ") != strings.Count(summary, "up to ") {
		t.Errorf("summary is %q — the span is HALF-OPEN, so prose says \"up to\" and never "+
			"\"to\": the Case that opened at exactly `covered_to` is in the NEXT digest", summary)
	}
}

// TestEveryFixtureValidates runs the renderer's own validator over each frozen
// payload, so a golden that was updated on purpose still has to be legal.
func TestEveryFixtureValidates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"root_firing.golden.json", "all_resolved.golden.json", "acked.golden.json",
		// The two digest fixtures are here for a reason of their own: the spanless one
		// is the fixture most likely to be "fixed" by filling in a zero `time.Time`,
		// and W2 would happily pass `0001-01-01T00:00:00Z`. Running the validator over
		// it proves only that it is legal — that the keys are absent is the golden's
		// job — but a legal digest is the precondition for the golden meaning anything.
		"digest.golden.json", "digest_no_span.golden.json",
	} {
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
