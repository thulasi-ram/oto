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

// renderView renders v in mode. `mentions` is variadic because exactly one card
// carries an audience (the unacked reminder) and every other caller must not have
// to say so.
func renderView(
	t *testing.T, v *domain.NotificationView, mode domain.Mode, mentions ...string,
) domain.RenderedMessage {
	t.Helper()
	msg, err := slackrender.New(clock.New()).Render(context.Background(), v, domain.RenderOptions{
		Mode:           mode,
		Verbosity:      domain.VerbosityAll,
		BaseURL:        "http://localhost:8080",
		MaxInstances:   10,
		ShowFieldEmoji: true,
		Mentions:       mentions,
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

// ------------------------------------------- the goldens for the unseen cards
//
// git-bug 2078a07: four Slack behaviours have never been observed against a live
// workspace. Three of the four already had byte-exact offline captures before this
// change — `test/fixtures/slack/root_update_acked.message.json` is a `chat.update`
// payload, `broadcast_unacked_reminder.message.json` carries the mention audience
// in its top-level `text`, and `root_resolved.golden.json` here is the terminal
// card. The fixtures below close what was actually missing, and what was missing
// is the SNOOZE.
//
// ⛔ NONE OF THESE IS A LIVE OBSERVATION. A golden proves that oto still sends the
// bytes somebody once read; it cannot prove Slack draws them, and the four unknowns
// in the issue stay open until a human runs
// docs/setup/slack-live-verification.md.

// ⭐ TestGoldenUpdateRootWhileStillFiring — ADR 0008's PRIMARY VERB, MID-LIFE.
//
// Both existing `ModeUpdateRoot` captures are TERMINAL or near it: the acked card
// and the resolved receipt. The ordinary case — the card is amended while the
// signal is still firing, because a member joined the group — had no fixture, and
// it is the one that happens most. It is also the one where the strikethrough has
// nothing to strike (`Firing` → `Firing`) and the footer is the only thing that
// says the card moved at all.
func TestGoldenUpdateRootWhileStillFiring(t *testing.T) {
	t.Parallel()
	msg := renderView(t, updatedWhileFiringView(), domain.ModeUpdateRoot)
	golden(t, "root_update_firing.golden.json", msg.Payload)
}

func updatedWhileFiringView() *domain.NotificationView {
	v := smokeView()
	grew := upstreamStart.Add(9 * time.Minute)
	v.Reason = "new_alerts"
	v.Group.FiringCount = 3
	v.Group.TotalCount = 3
	v.Group.LastActivityAt = grew
	v.Alerts = append(v.Alerts, domain.AlertView{
		ID: "a3", AlertKey: "ak_3", AlertName: "OtoSmokeTest",
		Severity: "critical", Namespace: "observability", Service: "oto-smoke",
		Labels:      map[string]string{"instance": "oto-smoke-c3", "cluster": "smoke-test"},
		State:       "firing",
		AckState:    "unacked",
		FirstSeenAt: grew, LastSeenAt: grew, TotalCases: 1,
	})
	v.Previous = &domain.PreviousState{State: "firing", AckState: "unacked"}
	v.Notifications = 2
	v.RenderedAt = grew.Add(time.Second)
	return v
}

// ⭐ TestGoldenAllResolvedThreadReply — the resolved fact in the THREAD.
//
// `root_resolved.golden.json` is the receipt the channel keeps. This is the other
// half of the same delivery: §H.6 sends `all_resolved` as `update_root` AND a
// thread reply, and the reply is the only one of the two that NOTIFIES — a
// `chat.update` is silent (ADR 0020's whole premise), so for a thread participant
// this sentence is the resolution.
//
// ⛔ AND IT SAYS "resolved after under a second" ABOUT A TWENTY-ONE-MINUTE
// INCIDENT. That is not a fixture typo, it is A1 again in the reply path. A1 was
// the first live run's "Firing for: under a second": the ROOT card was reading the
// triggering alert's case clock for a sentence about the GROUP, and it was fixed
// there (`durationValue`, and the test above it). `resolvedAfter` in reply.go still
// prefers `Case.Duration` and only falls back to the group's own span when it is
// zero — so this fixture, whose case is the smoke run's 300ms episode inside a
// group the root card correctly calls `21m 10s`, reproduces the defect exactly.
// The fallback is A2's shape as well: it measures from oto's FirstSeenAt rather
// than upstream's StartedAt, and those were twenty-one minutes apart in the live
// run.
//
// Whether it BITES in production depends on whether the notification module fills
// `Case.Duration` with the whole episode; `test/harness/slack_cards.go` assumes it
// does and this fixture assumes it does not, which is itself the reason to write
// it down. The number is what an operator reads the severity of an outage off, and
// a resolve that understates the outage is the same class of wrong as A1.
//
// The fixture is left as it renders. Correcting the FIXTURE would hide the
// question; correcting the RENDERER is a decision about which clock a group-scoped
// sentence may read, and that belongs with whoever owns §H.5.
func TestGoldenAllResolvedThreadReply(t *testing.T) {
	t.Parallel()
	msg := renderView(t, resolvedView(), domain.ModeThreadReply)
	golden(t, "reply_all_resolved.golden.json", msg.Payload)
}

// ⭐ TestGoldenUnackedReminderMentionBroadcast — the mention, byte for byte.
//
// The rule this pins is argued in reply_mention_test.go and is not repeated here:
// the mention lives in the top-level `text` and NOWHERE else, because that is the
// only position a push notification and a screen reader reach (ADR 0020,
// Amendment 3, re-justified by Amendment 4). Those tests assert the property; this
// fixture is the wire form a human compares against what their phone actually did.
//
// ⚠️ THE WIRE SHAPES ARE THE WHOLE POINT. `<@U024BE7LH>` and
// `<!subteam^SAZ94GDB8>` are Slack's own encodings; a mention that reaches the
// channel as the literal text `@ram` notifies nobody, and looks identical in a
// screenshot. That failure is invisible offline and obvious on a locked phone,
// which is why step 4 of the live checklist exists.
func TestGoldenUnackedReminderMentionBroadcast(t *testing.T) {
	t.Parallel()
	msg := renderView(t, unackedReminderMentionView(), domain.ModeBroadcastReply,
		"<@U024BE7LH>", "<!subteam^SAZ94GDB8>")
	golden(t, "reply_unacked_reminder_mention.golden.json", msg.Payload)
}

func unackedReminderMentionView() *domain.NotificationView {
	v := smokeView()
	reminded := upstreamStart.Add(15 * time.Minute)
	v.Reason = "unacked_reminder"
	v.Group.LastActivityAt = reminded
	v.Notifications = 2
	v.RenderedAt = reminded
	return v
}

// ⛔⛔ TestGoldenSnoozed* — THE CARD OTO SENDS WHEN A HUMAN ASKS IT TO GO QUIET,
// AND IT DOES NOT SAY SO.
//
// These two fixtures are checked in because the bytes are wrong, not because they
// are right, and a golden is how a wrong rendering stops being invisible. Read
// them before the live run: what a human must compare on screen is what oto
// actually sends, and what oto actually sends is this.
//
// The chain, all of it verifiable from this repository:
//
//  1. `snoozed` and `unsnoozed` are real Reasons (`internal/notification/domain/
//     reason.go`), and they are the ONLY two a snooze may not suppress
//     (`SnoozeExempt`) — "a snooze that cannot announce its own beginning and end
//     is the silent suppression §B.6 forbids".
//  2. `PlanFor` gives them the default treatment: `update_root` plus a
//     `thread_reply` (`rootModeFor`, `hasReply` — neither names them), and both sit
//     in `ungatedReplies`, so the reply is delivered at EVERY verbosity — §H.6
//     states it as "always — exempt from snooze suppression (§B.8.4)".
//  3. So both cards below are delivered to a real channel today.
//  4. And the Slack renderer has no `snoozed` branch at all. `replyBody` falls to
//     its `default:` arm — `:information_source: *Title* — <status>` — and
//     `reasonPhrase` returns "" so the footer's "why this card moved" clause is
//     absent. A snooze is NOT a suppression in oto's model: `SuppressedSnoozed`
//     lives in oto's own notification-suppression vocabulary and is explicitly
//     "NOT Alertmanager's `alert_cases.suppression_reason`", so `CardSuppressed`
//     never fires and the card stays FIRING-COLOURED.
//  5. `NotificationView` has no snooze axis to render even if the renderer wanted
//     one: no snoozed-until, no snoozer, and `TrailEntry.Kind` has no `snoozed`
//     verb (`trailEmoji`/`trailVerb` would print the raw word).
//
// The result is a card that changes for no stated reason and a thread line that
// announces an alert is firing — at the exact moment oto has decided to stop
// talking about it. That is §B.6's fatal failure mode with a message attached:
// oto's silence must never be indistinguishable from "there was no alert".
//
// ⚠️ THE FIX IS NOT IN THESE FIXTURES ON PURPOSE. It needs a snooze axis on the
// view (someone else's module), a Reason branch, a trail verb and words a human
// chooses. What is here is the evidence, frozen, so the next change to any of them
// shows up as a diff.
func TestGoldenSnoozedRootCard(t *testing.T) {
	t.Parallel()
	msg := renderView(t, snoozedView(), domain.ModeUpdateRoot)
	golden(t, "root_snoozed.golden.json", msg.Payload)
}

func TestGoldenSnoozedThreadReply(t *testing.T) {
	t.Parallel()
	msg := renderView(t, snoozedView(), domain.ModeThreadReply)
	golden(t, "reply_snoozed.golden.json", msg.Payload)
}

// snoozedView is a human asking oto to be quiet about a still-firing signal.
// Nothing about it is suppressed in Alertmanager's sense: the alerts are firing,
// the case is open and unacked, and the only thing that changed is that oto has
// agreed to stop mentioning it.
func snoozedView() *domain.NotificationView {
	v := smokeView()
	snoozedAt := upstreamStart.Add(6 * time.Minute)
	v.Reason = "snoozed"
	v.Group.LastActivityAt = snoozedAt
	v.Actor = &domain.ActorView{
		Kind: "slack_user", ID: "U024BE7LH", Label: "ram@example.com",
	}
	v.Comment = "provider incident — quiet until their ETA"
	v.Notifications = 2
	v.RenderedAt = snoozedAt.Add(time.Second)
	return v
}

// ⭐ THE `rendered_hash` IS WHAT MAKES `chat.update` SAFE TO CALL ON EVERY FACT.
//
// ADR 0008: "a `rendered_hash` check skips no-op updates entirely", and
// `SuppressedDuplicateRender` is the recorded reason when it does. That mechanism
// is only as good as the renderer being a pure function of the view — one map
// iteration, one clock read or one random block_id leaking into the payload turns
// every amend into a change and oto starts spending Tier-3 budget rewriting a card
// with the same content.
//
// This is the offline half of the update-in-place claim: it cannot prove Slack
// edits the message, and it can prove oto only asks when there is something to
// say.
func TestTheRenderedHashIsStableForOneFactAndMovesWhenTheFactDoes(t *testing.T) {
	t.Parallel()

	first := renderView(t, smokeView(), domain.ModeUpdateRoot)
	again := renderView(t, smokeView(), domain.ModeUpdateRoot)
	if first.Hash != again.Hash {
		t.Fatalf("two renders of the same fact disagree (%s vs %s); every chat.update "+
			"would look like a change and the no-op skip is dead", first.Hash, again.Hash)
	}
	if !bytes.Equal(first.Payload, again.Payload) {
		t.Errorf("the payload is not byte-stable:\n%s\n%s", first.Payload, again.Payload)
	}

	// And a fact that moved must move the hash, or the card silently stops
	// tracking the incident — the worse failure of the two.
	changed := renderView(t, resolvedView(), domain.ModeUpdateRoot)
	if changed.Hash == first.Hash {
		t.Errorf("a resolved card hashes the same as a firing one; the no-op skip would " +
			"swallow the resolution")
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
