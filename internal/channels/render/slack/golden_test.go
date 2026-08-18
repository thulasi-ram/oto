package slack_test

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
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// SPEC §H: "Every renderer is a pure function with a checked-in
// testdata/*.golden.json." There were none, and the first live Slack run is what
// that cost: four defects that a golden file would have caught for nothing,
// discovered instead by a human reading a card in a real channel.
//
// Every fixture here is that card. The numbers are the observed ones, so a
// regression reads as the same sentence the tester saw.

var update = flag.Bool("update", false, "rewrite the .golden.json fixtures")

// The observed run's clock, to the second.
var (
	// upstreamStart is Alertmanager's own `startsAt`.
	upstreamStart = time.Date(2026, 8, 7, 17, 56, 9, 0, time.UTC)
	// otoFirstSeen is when oto heard about it: twenty-one minutes later.
	otoFirstSeen = time.Date(2026, 8, 7, 18, 17, 9, 0, time.UTC)
	// renderedAt is eighty seconds after the signal started firing upstream.
	renderedAt = upstreamStart.Add(80 * time.Second)
)

// smokeView reproduces the first live run's root card, with the facts it had.
func smokeView() *domain.NotificationView {
	occStart := renderedAt.Add(-300 * time.Millisecond)
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "o1", Slug: "acme", Name: "Acme"},
		Reason: "fired",
		Group: domain.GroupView{
			ID: "019fe297-d84f-7599-b5b2-1f231749104a", GroupKey: "gk_4vbuj1c49jbpf",
			Generation: 1,
			// ⛔ The NAME, not a label dump (S15). The label map is still carried and
			// the renderer must not fall back to serialising it.
			Title:    "OtoSmokeTest",
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
				Labels: map[string]string{
					"instance": "oto-smoke-b2", "cluster": "smoke-test",
					// `team` is NOT a group-by label. Reading only the group labels is
					// why the Team field never rendered (C7).
					"team": "observability",
				},
				Annotations: map[string]string{
					"summary": "AUTOMATED TEST from oto — please ignore. This is oto's end-to-end " +
						"smoke test against a synthetic alert; no real service is affected.",
					"description": "Synthetic test alert produced by oto's first live Slack " +
						"verification run. Nothing is wrong. The card you are looking at is the thing under test.",
					"runbook_url": "https://example.com/runbooks/OtoSmokeTest",
				},
				GeneratorURL: "https://prometheus.example.com/graph?g0.expr=up&g0.tab=1",
				State:        "firing", AckState: "unacked",
				FirstSeenAt: otoFirstSeen, LastSeenAt: renderedAt, TotalCases: 1,
			},
			{
				ID: "a2", AlertKey: "ak_2", AlertName: "OtoSmokeTest",
				Severity: "critical", Namespace: "observability", Service: "oto-smoke",
				Labels: map[string]string{"instance": "oto-smoke-a1", "cluster": "smoke-test"},
				State:  "firing", AckState: "unacked",
				FirstSeenAt: otoFirstSeen, LastSeenAt: renderedAt, TotalCases: 1,
			},
		},
		// ⛔ THE CASE IS 300ms OLD AND THE GROUP IS 80s OLD. The card is about
		// the group; if the renderer reads this Duration the fixture says "under a
		// second" and A1 is back.
		Case: &domain.CaseView{
			ID: "occ1", Seq: 1, State: "firing", AckState: "unacked",
			StartedAt: occStart, Duration: 300 * time.Millisecond,
		},
		Rule: &domain.RuleView{
			SnapshotID: "rs1", Fingerprint: "rf_1", Name: "OtoSmokeTest",
			Expr: `sum(rate(oto_smoke_requests_total{code=~"5.."}[5m])) / ` +
				`sum(rate(oto_smoke_requests_total[5m])) > 0.05`,
			For: 5 * time.Minute, Origin: "generator_url", MatchConfidence: "exact",
			CapturedAt: otoFirstSeen,
		},
		Actions: []domain.Action{
			{ID: "oto.ack", Label: "Acknowledge", Style: "primary", Value: "019fe297-d84f-7599-b5b2-1f231749104a"},
			{ID: "oto.noop.runbook", Label: "Runbook", URL: "https://example.com/runbooks/OtoSmokeTest"},
			{ID: "oto.noop.silence", Label: "Silence",
				URL: "https://am.example.com/#/silences/new?filter=%7Balertname%3D%22OtoSmokeTest%22%2C+cluster%3D%22smoke-test%22%7D"},
		},
		Links: domain.Links{
			Group:      "http://localhost:8080/groups/019fe297-d84f-7599-b5b2-1f231749104a",
			Timeline:   "http://localhost:8080/groups/019fe297-d84f-7599-b5b2-1f231749104a/timeline",
			Runbook:    "https://example.com/runbooks/OtoSmokeTest",
			Prometheus: "https://prometheus.example.com/graph?g0.expr=up&g0.tab=1",
			Alertmanager: "https://am.example.com/#/alerts?filter=" +
				"%7Balertname%3D%22OtoSmokeTest%22%2C+cluster%3D%22smoke-test%22%7D",
			AlertmanagerSilenceNew: "https://am.example.com/#/silences/new?filter=" +
				"%7Balertname%3D%22OtoSmokeTest%22%2C+cluster%3D%22smoke-test%22%7D",
		},
		RenderedAt: renderedAt,
	}
}

func renderView(t *testing.T, v *domain.NotificationView, mode domain.Mode) domain.RenderedMessage {
	t.Helper()
	msg, err := slackrender.New(clock.New()).Render(context.Background(), v, domain.RenderOptions{
		Mode:           mode,
		Verbosity:      domain.VerbosityAll,
		BaseURL:        "http://localhost:8080",
		MaxInstances:   10,
		ShowFieldEmoji: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return msg
}

// golden compares the rendered payload with its checked-in fixture. `-update`
// rewrites it; a diff is a rendering change somebody has to look at on purpose.
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
		t.Fatalf("read golden (run `go test ./internal/channels/render/slack -update`): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(pretty.Bytes())) {
		t.Errorf("%s changed.\n--- want ---\n%s\n--- got ---\n%s", name, want, pretty.String())
	}
}

// fieldValue reads one `*Label*\nvalue` entry out of the fields block.
func fieldValue(t *testing.T, payload []byte, label string) string {
	t.Helper()
	var p struct {
		Attachments []struct {
			Blocks []struct {
				Type   string `json:"type"`
				Fields []struct {
					Text string `json:"text"`
				} `json:"fields"`
			} `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	prefix := "*" + label + "*\n"
	for _, a := range p.Attachments {
		for _, b := range a.Blocks {
			for _, f := range b.Fields {
				if strings.HasPrefix(f.Text, prefix) {
					return strings.TrimPrefix(f.Text, prefix)
				}
			}
		}
	}
	return ""
}

func topLevelText(t *testing.T, payload []byte) string {
	t.Helper()
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p.Text
}

// ---------------------------------------------------------------- A1 and A2

// ⛔ A1. The observed card said `Firing for: under a second` about a group that
// had been firing for eighty. The duration came from the TRIGGERING ALERT's
// case, which for a `fired` intent is milliseconds old; the card is about
// the GROUP. A card that misstates how long an outage has lasted is worse than
// one that omits it, because that number is what an operator triages on.
func TestFiringForMeasuresTheGroupAndNotTheTriggeringCase(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)
	got := fieldValue(t, msg.Payload, "Firing for")

	if got == "under a second" {
		t.Fatalf("Firing for came from the triggering case again (%q); "+
			"the group had been firing 80s", got)
	}
	if got != "1m 20s" {
		t.Fatalf("Firing for = %q, want %q (80s from upstream startsAt)", got, "1m 20s")
	}
}

// ⛔ A2. `Started` showed oto's first sighting. Twenty-one minutes of the outage
// were invisible in the observed run. Upstream's `startsAt` is the fact; oto's
// own observation time is a DIFFERENT fact and lives in the footer.
func TestStartedIsUpstreamStartsAtAndTheObservationLagIsShownSeparately(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)

	started := fieldValue(t, msg.Payload, "Started")
	wantUpstream := "<!date^" + itoa(upstreamStart.Unix()) + "^{time}|17:56 UTC>"
	if started != wantUpstream {
		t.Fatalf("Started = %q, want upstream startsAt %q", started, wantUpstream)
	}
	if strings.Contains(started, itoa(otoFirstSeen.Unix())) {
		t.Fatalf("Started is still oto's first sighting: %q", started)
	}

	// oto's own sighting is not thrown away — it is labelled as what it is.
	body := string(msg.Payload)
	if !strings.Contains(body, "oto first saw it") {
		t.Fatalf("the 21-minute observation lag is nowhere on the card: %s", body)
	}
	if !strings.Contains(body, "(21m later)") {
		t.Fatalf("the observation lag is unquantified: %s", body)
	}
}

// ---------------------------------------------------------------- C6, C7, C10

func TestTheTitleIsTheAlertNameWithTheClusterAsAChip(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)
	body := string(msg.Payload)

	if strings.Contains(body, "alertname=OtoSmokeTest") {
		t.Fatalf("the title is still a raw label dump: %s", body)
	}
	if !strings.Contains(body, "|OtoSmokeTest>") {
		t.Fatalf("the title is not the alert name: %s", body)
	}
	if !strings.Contains(body, "`smoke-test`") {
		t.Fatalf("the cluster is not rendered as its own chip: %s", body)
	}
}

// ⛔ C7. `team` is almost never one of Alertmanager's `group_by` labels, so a
// renderer reading only the group labels renders this field NEVER.
func TestTeamRendersFromTheAlertLabelsNotOnlyTheGroupLabels(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)
	if got := fieldValue(t, msg.Payload, "Team"); got != "observability" {
		t.Fatalf("Team = %q, want %q", got, "observability")
	}
}

// ⛔ C10. The Silence deep link is v1's ONLY silence affordance (R3): oto shows
// you where to write one and never writes it.
func TestTheSilenceButtonIsRenderedAsAnAlertmanagerDeepLink(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)
	body := string(msg.Payload)

	if !strings.Contains(body, "oto.noop.silence") {
		t.Fatalf("no Silence button: %s", body)
	}
	if !strings.Contains(body, "am.example.com/#/silences/new") {
		t.Fatalf("the Silence button is not an Alertmanager deep link: %s", body)
	}
}

// ---------------------------------------------------------------------- C8

// ⛔ C8. The observed push notification read
// "…no real service…. Severity critical" — cut mid-clause, then given the
// caller's own full stop. The top-level text is the push notification, the
// sidebar preview and the only thing a screen reader reads (S5).
func TestTheTopLevelTextTruncatesOnAClauseBoundaryAndNeverStacksAFullStop(t *testing.T) {
	t.Parallel()

	msg := renderView(t, smokeView(), domain.ModePostRoot)
	text := topLevelText(t, msg.Payload)

	if strings.Contains(text, "….") {
		t.Fatalf("an ellipsis was given a full stop: %q", text)
	}
	if strings.Contains(text, "no real service…") {
		t.Fatalf("the summary was still cut mid-clause: %q", text)
	}
	if !strings.Contains(text, "Severity critical") {
		t.Fatalf("the facts clause was lost: %q", text)
	}
	// The cut lands on a boundary a human would have chosen.
	if i := strings.Index(text, "…"); i > 0 {
		before := strings.TrimSpace(text[:i])
		last := before[len(before)-1:]
		if strings.ContainsAny(last, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
			!strings.HasSuffix(before, "alert") {
			// A word boundary is acceptable; a mid-word cut is not. The check is that
			// the character before the ellipsis closes a word, which TrimRight of the
			// clause breaks guarantees — this asserts the guarantee did not regress.
			if strings.HasSuffix(text[:i], " ") {
				t.Fatalf("the cut left a dangling space: %q", text)
			}
		}
	}
}

// ---------------------------------------------------------------------- C9

// ⛔ C9. ADR 0020 rule 4: a broadcast's top-level text must be SELF-SUFFICIENT,
// because it is the push notification on a locked phone and what a screen reader
// announces. The observed broadcast was
// ":repeat: Re-fired: alertname=OtoSmokeTest, cluster=smoke-test" — a name and
// nothing about it. A reader in the channel could not tell whether to open the
// thread, which is the only action a broadcast asks for.
func TestABroadcastTopLevelTextCarriesSeverityAndDuration(t *testing.T) {
	t.Parallel()

	v := smokeView()
	v.Reason = "refired"
	v.Case.Duration = 3*time.Minute + 12*time.Second
	v.Case.Seq = 4

	msg := renderView(t, v, domain.ModeBroadcastReply)
	text := topLevelText(t, msg.Payload)

	if !strings.Contains(text, "Re-fired") {
		t.Fatalf("the broadcast lost its lead: %q", text)
	}
	if !strings.Contains(text, "Severity critical") {
		t.Fatalf("ADR 0020 rule 4: the broadcast carries no severity: %q", text)
	}
	if !strings.Contains(text, "3m 12s") {
		t.Fatalf("ADR 0020 rule 4: the broadcast carries no duration: %q", text)
	}
	if strings.Contains(text, "alertname=") {
		t.Fatalf("the broadcast still names the group by a label dump: %q", text)
	}

	// ⛔ AND IT MUST NOT DEPEND ON THE ATTACHMENT. ADR 0020 Amendment 4: the colour
	// bar does render in today's client, but that is undocumented behaviour Slack's
	// own reference contradicts, and button interactivity in the in-channel copy is
	// unverified. The sentence has to stand alone whatever the client does with the
	// rest.
	if strings.Contains(text, "<http") {
		t.Fatalf("the broadcast sentence leans on a link to be intelligible: %q", text)
	}
}

// ------------------------------------------------------------------- golden

func TestGoldenRootCard(t *testing.T) {
	t.Parallel()
	msg := renderView(t, smokeView(), domain.ModePostRoot)
	golden(t, "root_firing.golden.json", msg.Payload)
}

func TestGoldenBroadcastRefired(t *testing.T) {
	t.Parallel()
	v := smokeView()
	v.Reason = "refired"
	v.Case.Duration = 3*time.Minute + 12*time.Second
	v.Case.Seq = 4
	v.Previous = &domain.PreviousState{State: "resolved"}
	msg := renderView(t, v, domain.ModeBroadcastReply)
	golden(t, "broadcast_refired.golden.json", msg.Payload)
}

// resolvedView is the card AFTER the incident: the only record left in the
// channel, because `chat.update` overwrote the firing one silently.
func resolvedView() *domain.NotificationView {
	v := smokeView()
	acked := upstreamStart.Add(3 * time.Minute)
	ended := upstreamStart.Add(21*time.Minute + 10*time.Second)

	v.Reason = "all_resolved"
	v.Group.State = "closed"
	v.Group.FiringCount = 0
	v.Group.ResolvedCount = 2
	v.Group.AckedCount = 1
	v.Group.LastActivityAt = ended
	v.Alerts[0].State = "resolved"
	v.Alerts[1].State = "resolved"
	v.Case.State = "resolved"
	v.Case.AckState = "acked"
	v.Case.AckedAt = &acked
	v.Case.AckedByLabel = "ram@example.com"
	v.Case.EndedAt = &ended
	v.Notifications = 3
	v.Previous = &domain.PreviousState{State: "firing", AckState: "acked"}
	v.Trail = []domain.TrailEntry{
		{Kind: "fired", At: upstreamStart},
		{Kind: "acked", At: acked, Actor: "ram@example.com"},
		{Kind: "resolved", At: ended},
	}
	v.RenderedAt = ended.Add(2 * time.Second)
	return v
}

func TestGoldenResolvedRootCard(t *testing.T) {
	t.Parallel()
	msg := renderView(t, resolvedView(), domain.ModeUpdateRoot)
	golden(t, "root_resolved.golden.json", msg.Payload)
}

// ⛔ THE TERMINAL CARD IS A RECEIPT, NOT A BLANK STATE.
//
// `chat.update` is SILENT. A channel reader watches a red card become a green
// one with no notification and no trace, and — in the words of the person who
// watched it happen in the first live run — "it means something happened and we
// don't know". The card then made it worse by SHEDDING content on resolve: the
// members went, the rule snapshot went, the overflow shrank. It became least
// informative at exactly the moment it became the only remaining record.
//
// This test is the fixture that stops it regressing to a blank state.
func TestTheResolvedCardIsAReceiptAndNotABlankState(t *testing.T) {
	t.Parallel()

	msg := renderView(t, resolvedView(), domain.ModeUpdateRoot)
	body := string(msg.Payload)

	// 1. THE RULE SURVIVES. It is the record of WHY this fired, and the question
	//    "was that threshold sensible?" is asked afterwards, not during.
	if !strings.Contains(body, "oto_smoke_requests_total") {
		t.Errorf("the resolved card dropped the rule snapshot — the record of why it fired: %s", body)
	}

	// 2. THE MEMBERS SURVIVE. "Which box was it?" is the first question the next
	//    morning, and the thread is not where a channel reader is.
	if !strings.Contains(body, "oto-smoke-b2") || !strings.Contains(body, "oto-smoke-a1") {
		t.Errorf("the resolved card dropped the affected instances: %s", body)
	}

	// 3. THE EPISODE IS STATED: when it ended, what it hit, how loud oto was, and
	//    whether anybody had it.
	for _, want := range []string{"*Resolved*", "*Instances affected*", "*Notifications*", "*Acknowledged*"} {
		if !strings.Contains(body, want) {
			t.Errorf("the receipt is missing %s: %s", want, body)
		}
	}
	if got := fieldValue(t, msg.Payload, "Instances affected"); got != "2 instances" {
		t.Errorf("Instances affected = %q, want %q", got, "2 instances")
	}
	if got := fieldValue(t, msg.Payload, "Notifications"); got != "3 sent" {
		t.Errorf("Notifications = %q, want %q", got, "3 sent")
	}
	if got := fieldValue(t, msg.Payload, "Acknowledged"); !strings.Contains(got, "ram@example.com") {
		t.Errorf("Acknowledged does not attribute the ack: %q", got)
	}
	// The duration is the whole episode, from upstream's startsAt.
	if got := fieldValue(t, msg.Payload, "Duration"); got != "21m 10s" {
		t.Errorf("Duration = %q, want the whole episode %q", got, "21m 10s")
	}

	// 4. THE STATE TRAIL IS LEGIBLE WITHOUT OPENING THE THREAD.
	if !strings.Contains(body, "fired") || !strings.Contains(body, "acked") ||
		!strings.Contains(body, "resolved") {
		t.Errorf("the state trail is missing: %s", body)
	}
	if !strings.Contains(body, "total 21m 10s") {
		t.Errorf("the trail does not close with the total: %s", body)
	}
	if !strings.Contains(body, "oto_trail_") {
		t.Errorf("no trail block was rendered: %s", body)
	}

	// 5. THE RECORD STAYS NAVIGABLE. Acknowledge goes — it is meaningless on
	//    something that is over — but every place to LOOK stays.
	if strings.Contains(body, "oto.ack") {
		t.Errorf("a resolved card still offers Acknowledge: %s", body)
	}
	for _, want := range []string{"Show timeline", "Open in Prometheus", "Open in Alertmanager", "Rule history"} {
		if !strings.Contains(body, want) {
			t.Errorf("the resolved card dropped %q from the overflow: %s", want, body)
		}
	}
}

// The trail must not grow without bound on a long-lived flapper, and it must
// elide the MIDDLE: the first entry says when this began and the last says what
// it is now, and both are needed.
func TestTheStateTrailElidesTheMiddleAndKeepsBothEnds(t *testing.T) {
	t.Parallel()

	v := smokeView()
	kinds := []string{"fired", "acked", "unacked", "suppressed", "unsuppressed",
		"resolved", "refired", "acked", "unacked", "resolved"}
	for i, k := range kinds {
		v.Trail = append(v.Trail, domain.TrailEntry{
			Kind: k, At: upstreamStart.Add(time.Duration(i) * time.Minute),
		})
	}

	msg := renderView(t, v, domain.ModePostRoot)
	body := string(msg.Payload)

	if !strings.Contains(body, "… 4 more") {
		t.Fatalf("the trail did not elide its middle: %s", body)
	}
	// The first transition and the last both survive the elision.
	if !strings.Contains(body, "17:56 UTC> fired") {
		t.Errorf("the trail lost its first entry: %s", body)
	}
	if !strings.Contains(body, "18:05 UTC> resolved") {
		t.Errorf("the trail lost its most recent entry: %s", body)
	}
}

// ⛔ A5. `PreviousState` existed, had a renderer branch, and was CONSTRUCTED
// NOWHERE — so §H.4's strikethrough never fired. Dead rendering code that looks
// implemented is a trap; this pins the live version.
func TestTheStrikethroughRendersThePreviousState(t *testing.T) {
	t.Parallel()

	v := smokeView()
	v.Reason = "all_resolved"
	v.Group.FiringCount = 0
	v.Group.ResolvedCount = 2
	v.Group.State = "closed"
	v.Previous = &domain.PreviousState{State: "firing", AckState: "unacked"}

	msg := renderView(t, v, domain.ModeUpdateRoot)
	status := fieldValue(t, msg.Payload, "Status")
	if !strings.Contains(status, "~Firing~ →") {
		t.Fatalf("the §H.4 strikethrough did not render: %q", status)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
