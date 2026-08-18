package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// The Slack card corpus: one deterministic NotificationView per card variant oto
// can put in a channel, and the machinery that freezes each one into a
// checked-in JSON capture.
//
// ⭐⭐ THESE FILES EXIST BECAUSE NOTHING IN THIS REPOSITORY HAS EVER BEEN
// RENDERED BY SLACK. Every check oto runs over a card — the eighteen outbound
// rules in `internal/channels/render/slack/validate.go` — is oto's own BELIEF
// about Block Kit, asserted against itself. A belief that agrees with itself is
// not evidence. The one thing that can settle it is Slack's own renderer, and
// the cheapest way to reach Slack's own renderer without a workspace, a
// credential or an install is Block Kit Builder:
//
//	https://app.slack.com/block-kit-builder
//
// So each variant is captured TWICE:
//
//   - `<name>.message.json` — the EXACT bytes oto hands `chat.postMessage` /
//     `chat.update`, attachment wrapper and all. This is the wire truth and it
//     is what a diff should be read against.
//   - `<name>.blockkit.json` — the same card's block list lifted out of the
//     attachment and wrapped as `{"blocks": […]}`, which is the shape Block Kit
//     Builder accepts. ⛔ THE BUILDER CANNOT RENDER `attachments`, so pasting the
//     wire payload shows a reviewer nothing. This second file is the one a human
//     pastes.
//
// The split is itself a finding worth stating plainly: the colour bar — SPEC
// §H.2's peripheral-vision answer to "do I need to act?" — lives on the
// attachment, so it is the ONE part of the card Block Kit Builder can never
// show. It has to be checked in a real workspace, and the residual checklist in
// docs/setup/slack.md says so.
//
// Everything here is a pure function of frozen timestamps. There is no clock, no
// randomness and no map iteration in the output, because a capture that changes
// between two identical runs cannot be reviewed.

// The corpus clock. One incident, told seven ways.
var (
	// cardUpstreamStart is Alertmanager's own `startsAt` — when the SIGNAL began.
	cardUpstreamStart = time.Date(2026, 8, 7, 17, 56, 9, 0, time.UTC)
	// cardOtoFirstSeen is when oto heard about it: `group_wait` plus ingest.
	cardOtoFirstSeen = time.Date(2026, 8, 7, 18, 17, 9, 0, time.UTC)
	// cardAckedAt is when a human pressed Acknowledge.
	cardAckedAt = cardUpstreamStart.Add(3 * time.Minute)
	// cardEndedAt is when the last member resolved.
	cardEndedAt = cardUpstreamStart.Add(21*time.Minute + 10*time.Second)
)

// SlackCard is one capture: a named view plus the options it is rendered with.
type SlackCard struct {
	// Name is the capture's file stem. It is also the heading a human looks for
	// in the Block Kit Builder checklist.
	Name string
	// What is the one sentence a reviewer needs before they look at the card.
	What string
	// Mode is the delivery mode, which decides root-vs-reply rendering.
	Mode chdomain.Mode
	// View is the fact being communicated.
	View *chdomain.NotificationView
	// Options is the per-delivery render configuration.
	Options chdomain.RenderOptions
}

// SlackCards is the whole corpus, in the order a human should review it: the
// card is posted, replied to, amended, broadcast, and finally closed.
//
// ⛔ THERE IS NO "SNOOZED" CARD, AND ITS ABSENCE IS THE ANSWER TO A QUESTION.
// oto has snoozes (the `alert_snoozes` row, the expiry sweep in
// `internal/app/workers.go`) but the Slack renderer has no `snoozed` Reason and
// no `snoozed` CardState. What reaches a channel when a signal is quietened is
// the SUPPRESSED card — `CardSuppressed`, "Silenced", `#dddddd` — which is
// Alertmanager's silence, a different fact with a different cause. The
// `root_silenced` capture below is that card. If a `snoozed` card is ever
// wanted, it does not exist yet and no golden can pretend otherwise.
func SlackCards() []SlackCard {
	return []SlackCard{
		{
			Name: "root_firing",
			What: "The first card. Posted once per generation with chat.postMessage; " +
				"everything after it is an edit of this message.",
			Mode:    chdomain.ModePostRoot,
			View:    firingView(),
			Options: cardOptions(chdomain.ModePostRoot),
		},
		{
			Name: "thread_reply_acked",
			What: "A thread reply. Replies are the EXCEPTION (ADR 0008): one is posted " +
				"only when a human needs to be told something the card cannot say by changing.",
			Mode:    chdomain.ModeThreadReply,
			View:    ackedView(),
			Options: cardOptions(chdomain.ModeThreadReply),
		},
		{
			Name: "root_update_acked",
			What: "The SAME root message after chat.update. This is ADR 0008's primary " +
				"verb: colour, status and footer move; the ts does not.",
			Mode:    chdomain.ModeUpdateRoot,
			View:    ackedView(),
			Options: cardOptions(chdomain.ModeUpdateRoot),
		},
		{
			Name: "broadcast_unacked_reminder",
			What: "A reply posted with reply_broadcast=true. Its top-level text must be " +
				"self-sufficient (ADR 0020 rule 4) and it carries the mention audience.",
			Mode: chdomain.ModeBroadcastReply,
			View: unackedReminderView(),
			Options: withMentions(cardOptions(chdomain.ModeBroadcastReply),
				"<@U0123456789>", "<!subteam^S01SREONCALL>"),
		},
		{
			Name: "root_resolved",
			What: "The terminal card, produced by chat.update. It is the ONLY record left " +
				"in the channel, because the update that made it was silent.",
			Mode:    chdomain.ModeUpdateRoot,
			View:    resolvedCardView(),
			Options: cardOptions(chdomain.ModeUpdateRoot),
		},
		{
			Name: "root_silenced",
			What: "The silenced card — oto's nearest thing to \"snoozed\". Only the " +
				"reconciler can produce this state, and the colour is the near-white #dddddd.",
			Mode:    chdomain.ModeUpdateRoot,
			View:    silencedView(),
			Options: cardOptions(chdomain.ModeUpdateRoot),
		},
		{
			Name: "storm_notice",
			What: "The once-per-channel storm notice (ADR 0020 Amendment 1). It broadcasts, " +
				"and it is the only broadcast permitted while damping is on.",
			Mode:    chdomain.ModeBroadcastReply,
			View:    stormNoticeView(),
			Options: cardOptions(chdomain.ModeBroadcastReply),
		},
	}
}

func cardOptions(mode chdomain.Mode) chdomain.RenderOptions {
	return chdomain.RenderOptions{
		Mode:           mode,
		Verbosity:      chdomain.VerbosityAll,
		BaseURL:        "https://oto.example.com",
		MaxInstances:   10,
		ShowFieldEmoji: true,
	}
}

func withMentions(o chdomain.RenderOptions, mentions ...string) chdomain.RenderOptions {
	o.Mentions = mentions
	return o
}

// baseView is the incident every capture is a moment of: two instances of one
// critical alert, in one cluster, with a rule snapshot and three affordances.
func baseView() *chdomain.NotificationView {
	return &chdomain.NotificationView{
		Org:    chdomain.OrgRef{ID: "o1", Slug: "acme", Name: "Acme"},
		Reason: "fired",
		Group: chdomain.GroupView{
			ID:          "019fe297-d84f-7599-b5b2-1f231749104a",
			GroupKey:    "gk_4vbuj1c49jbpf",
			Generation:  1,
			Title:       "CheckoutErrorRateHigh",
			Receiver:    "platform-critical",
			GroupLabels: map[string]string{"alertname": "CheckoutErrorRateHigh", "cluster": "eu-west-1"},
			State:       "open",
			Severity:    "critical",
			FiringCount: 2, TotalCount: 2,
			ClusterKey:     "eu-west-1",
			StartedAt:      cardUpstreamStart,
			FirstSeenAt:    cardOtoFirstSeen,
			LastActivityAt: cardUpstreamStart.Add(80 * time.Second),
		},
		Alerts: []chdomain.AlertView{
			{
				ID: "a1", AlertKey: "ak_1", AlertName: "CheckoutErrorRateHigh",
				Severity: "critical", Namespace: "checkout", Service: "checkout-api",
				ClusterKey: "eu-west-1",
				Labels: map[string]string{
					"instance": "checkout-api-7d9f-b2", "cluster": "eu-west-1",
					// `team` is almost never a group_by label, which is why it is read
					// from the ALERT and not only from the group.
					"team": "payments",
				},
				Annotations: map[string]string{
					"summary": "5xx rate on checkout-api is 11.4% over the last five minutes, " +
						"against a 5% threshold.",
					"description": "Requests to POST /v2/checkout are failing upstream of the " +
						"payment provider. The error budget for the month is 38% spent.",
					"runbook_url": "https://runbooks.example.com/checkout-error-rate",
				},
				GeneratorURL: "https://prometheus.example.com/graph?g0.expr=up&g0.tab=1",
				State:        "firing", AckState: "unacked",
				FirstSeenAt: cardOtoFirstSeen, LastSeenAt: cardUpstreamStart.Add(80 * time.Second),
				TotalCases: 1,
				Value:      f64(0.114),
			},
			{
				ID: "a2", AlertKey: "ak_2", AlertName: "CheckoutErrorRateHigh",
				Severity: "critical", Namespace: "checkout", Service: "checkout-api",
				ClusterKey: "eu-west-1",
				Labels: map[string]string{
					"instance": "checkout-api-7d9f-a1", "cluster": "eu-west-1", "team": "payments",
				},
				State: "firing", AckState: "unacked",
				FirstSeenAt: cardOtoFirstSeen, LastSeenAt: cardUpstreamStart.Add(80 * time.Second),
				TotalCases: 1,
				Value:      f64(0.092),
			},
		},
		Case: &chdomain.CaseView{
			ID: "occ1", Seq: 1, State: "firing", AckState: "unacked",
			StartedAt: cardUpstreamStart,
		},
		Rule: &chdomain.RuleView{
			SnapshotID: "rs1", Fingerprint: "rf_1", Name: "CheckoutErrorRateHigh",
			File: "platform.rules.yml", Group: "checkout",
			Expr: `sum(rate(http_requests_total{job="checkout-api",code=~"5.."}[5m])) / ` +
				`sum(rate(http_requests_total{job="checkout-api"}[5m])) > 0.05`,
			For: 5 * time.Minute, Origin: "generator_url", MatchConfidence: "exact",
			CapturedAt: cardOtoFirstSeen,
		},
		Actions: []chdomain.Action{
			{
				ID: "oto.ack", Label: "Acknowledge", Style: "primary",
				Value: "019fe297-d84f-7599-b5b2-1f231749104a",
			},
			{
				ID: "oto.noop.runbook", Label: "Runbook",
				URL: "https://runbooks.example.com/checkout-error-rate",
			},
			{
				ID: "oto.noop.silence", Label: "Silence",
				URL: "https://am.example.com/#/silences/new?filter=%7Balertname%3D%22CheckoutErrorRateHigh%22%7D",
			},
		},
		Links: chdomain.Links{
			Group:        "https://oto.example.com/cases/019fe297-d84f-7599-b5b2-1f231749104a",
			Timeline:     "https://oto.example.com/cases/019fe297-d84f-7599-b5b2-1f231749104a/timeline",
			Runbook:      "https://runbooks.example.com/checkout-error-rate",
			Prometheus:   "https://prometheus.example.com/graph?g0.expr=up&g0.tab=1",
			Alertmanager: "https://am.example.com/#/alerts?filter=%7Balertname%3D%22CheckoutErrorRateHigh%22%7D",
			AlertmanagerSilenceNew: "https://am.example.com/#/silences/new?filter=" +
				"%7Balertname%3D%22CheckoutErrorRateHigh%22%7D",
		},
		Trail:      []chdomain.TrailEntry{{Kind: "fired", At: cardUpstreamStart}},
		RenderedAt: cardUpstreamStart.Add(80 * time.Second),
	}
}

func firingView() *chdomain.NotificationView { return baseView() }

// ackedView is the moment a human took it. It drives BOTH the thread reply and
// the update-in-place capture, on purpose: the two are the same fact rendered
// for two audiences, and a reviewer should be able to see that they agree.
func ackedView() *chdomain.NotificationView {
	v := baseView()
	v.Reason = "acked"
	v.Group.AckedCount = 2
	v.Group.LastActivityAt = cardAckedAt
	v.Actor = &chdomain.ActorView{Kind: "slack_user", ID: "U0123456789", Label: "ram"}
	v.Case.AckState = "acked"
	v.Case.AckedAt = tp(cardAckedAt)
	v.Case.AckedByLabel = "ram@example.com"
	v.Case.AckNote = "Looking at it — provider side, escalating to their support."
	v.Alerts[0].AckState = "acked"
	v.Alerts[1].AckState = "acked"
	v.Previous = &chdomain.PreviousState{State: "firing", AckState: "unacked"}
	v.Trail = append(v.Trail, chdomain.TrailEntry{
		Kind: "acked", At: cardAckedAt, Actor: "ram@example.com",
	})
	v.Notifications = 2
	v.RenderedAt = cardAckedAt.Add(time.Second)
	return v
}

// unackedReminderView is the one message oto sends whose entire purpose is to
// reach somebody who has NOT engaged. It is the only routine broadcast.
func unackedReminderView() *chdomain.NotificationView {
	v := baseView()
	v.Reason = "unacked_reminder"
	v.Group.LastActivityAt = cardUpstreamStart.Add(15 * time.Minute)
	v.Notifications = 2
	v.RenderedAt = cardUpstreamStart.Add(15 * time.Minute)
	return v
}

// resolvedCardView is the receipt: the last version of the root message, and
// the only trace of the incident a channel reader will ever scroll past.
func resolvedCardView() *chdomain.NotificationView {
	v := ackedView()
	v.Reason = "all_resolved"
	// ⛔ NO ACTOR. `Actor` is the actor of the FACT THIS CARD ANNOUNCES, and
	// nobody resolves an alert in oto — the signal stopped, which is the whole of
	// §B.2. Inheriting the ack's actor here modelled a card production cannot
	// produce, and it hid the very confusion this corpus exists to expose: the
	// receipt's Acknowledged field must name the acker from the case's own
	// frozen label, which is the only attribution a terminal card still has.
	//
	// ⚠️ WHAT THAT COSTS THE CORPUS, STATED SO NOBODY HAS TO REDISCOVER IT. This
	// was the only capture where an AMENDED card carried an actor of its own — the
	// case where the announced fact's actor and the ack's actor are two different
	// people, which is where every attribution defect in this area has lived. No
	// capture exercises it now. The card that would is a `comment` posted on a
	// resolved incident: the commenter is `v.Actor`, the acker is
	// `Case.AckedByLabel`, and the receipt must name the second while the
	// balloon names the first.
	//
	// ⭐ THE CORPUS CAN HOST IT — it is the colour budget, not the colour rule,
	// that is tight. `TestEachCardStateCarriesItsOwnColourForAHumanToVerify`
	// refuses more than TWO captures per colour, and firing (root + reminder) and
	// acknowledged (reply + update) are both full; resolved, silenced and storm
	// each hold one, so a resolved-coloured eighth capture fits. Adding it means
	// regenerating the checked-in captures — `go test ./test/harness -run Corpus
	// -update-slack-goldens` — which is why it is not in this change and is named
	// here instead. Until it lands, the property is asserted in the renderer's own
	// tests rather than in bytes a human can paste:
	// `TestACommentDoesNotCreditTheAcknowledgementToWhoeverSpokeLast` and
	// `TestACommentDoesNotTurnAHumanAcknowledgementIntoAnAutomaticOne`, both in
	// internal/channels/render/slack/attribution_test.go.
	v.Actor = nil
	v.Group.State = "closed"
	v.Group.FiringCount = 0
	v.Group.ResolvedCount = 2
	v.Group.AckedCount = 1
	v.Group.LastActivityAt = cardEndedAt
	v.Alerts[0].State = "resolved"
	v.Alerts[1].State = "resolved"
	v.Case.State = "resolved"
	v.Case.EndedAt = tp(cardEndedAt)
	v.Case.Duration = cardEndedAt.Sub(cardUpstreamStart)
	v.Notifications = 4
	v.Previous = &chdomain.PreviousState{State: "firing", AckState: "acked"}
	v.Trail = append(v.Trail, chdomain.TrailEntry{Kind: "resolved", At: cardEndedAt})
	v.RenderedAt = cardEndedAt.Add(2 * time.Second)
	return v
}

// silencedView is an Alertmanager silence covering the group. ⛔ It is NOT a
// resolution and the card must never let it read as one.
func silencedView() *chdomain.NotificationView {
	v := baseView()
	until := cardUpstreamStart.Add(4 * time.Hour)
	v.Reason = "suppressed"
	v.Group.FiringCount = 0
	v.Group.SuppressedCount = 2
	v.Group.LastActivityAt = cardUpstreamStart.Add(10 * time.Minute)
	v.Alerts[0].State = "suppressed"
	v.Alerts[1].State = "suppressed"
	v.Case.State = "suppressed"
	v.Case.SuppressionReason = "provider incident — silenced until their ETA"
	v.Case.EndedAt = tp(until)
	v.Actor = &chdomain.ActorView{Kind: "slack_user", ID: "U0123456789", Label: "ram"}
	v.Previous = &chdomain.PreviousState{State: "firing", AckState: "unacked"}
	v.Trail = append(v.Trail, chdomain.TrailEntry{
		Kind: "suppressed", At: cardUpstreamStart.Add(10 * time.Minute), Actor: "ram@example.com",
	})
	v.Notifications = 2
	v.RenderedAt = cardUpstreamStart.Add(10*time.Minute + time.Second)
	return v
}

// stormNoticeView is the once-per-channel damping announcement. ⛔ Storm mode is
// a VISIBLE state, never silent suppression: the notice is how a channel learns
// that oto has deliberately stopped talking.
func stormNoticeView() *chdomain.NotificationView {
	v := baseView()
	v.Reason = "storm"
	v.Group.StormMode = true
	v.Group.FiringCount = 214
	v.Group.TotalCount = 214
	v.Group.LastActivityAt = cardUpstreamStart.Add(6 * time.Minute)
	v.StormCount = 214
	v.Notifications = 3
	v.RenderedAt = cardUpstreamStart.Add(6 * time.Minute)
	return v
}

func f64(f float64) *float64    { return &f }
func tp(t time.Time) *time.Time { return &t }
func indentJSON(b []byte) []byte {
	var o bytes.Buffer
	_ = json.Indent(&o, b, "", "  ")
	return o.Bytes()
}
func withNewline(b []byte) []byte { return append(bytes.TrimRight(b, "\n"), '\n') }

// SlackCardCapture is one variant, rendered.
type SlackCardCapture struct {
	Card SlackCard
	// Wire is the exact payload oto hands the Slack SDK.
	Wire json.RawMessage
	// Builder is `{"blocks": […]}` — the attachment's block list, lifted out so
	// it can be pasted into Block Kit Builder, which cannot render attachments.
	Builder json.RawMessage
	// Colour is the attachment colour Block Kit Builder cannot show. It is
	// reported separately so the residual checklist can name what is untested.
	Colour string
	// Fallback is the attachment's legacy fallback string.
	Fallback string
	// Hash is the renderer's payload hash — the value the notification module
	// compares to decide whether a chat.update is worth making.
	Hash string
}

// RenderSlackCards renders every variant with the real renderer.
//
// ⛔ IT RETURNS THE VALIDATION ERROR RATHER THAN SWALLOWING IT. `Render` returns
// both the message and a terminal error when a payload fails §L.6, because the
// bytes have to reach `notification_deliveries.rendered` for the dead delivery to
// be debuggable. A capture that quietly froze an INVALID card would be worse than
// no capture at all: it would put a payload Slack rejects in front of a human as
// though it were the thing to check.
func RenderSlackCards() ([]SlackCardCapture, error) {
	r := slackrender.New(clock.New())
	out := make([]SlackCardCapture, 0, len(SlackCards()))
	for _, card := range SlackCards() {
		msg, err := r.Render(context.Background(), card.View, card.Options)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", card.Name, err)
		}
		builder, colour, fallback, err := builderPayload(msg.Payload)
		if err != nil {
			return nil, fmt.Errorf("lift blocks for %s: %w", card.Name, err)
		}
		out = append(out, SlackCardCapture{
			Card: card, Wire: msg.Payload, Builder: builder,
			Colour: colour, Fallback: fallback, Hash: msg.Hash,
		})
	}
	return out, nil
}

// builderPayload lifts the single attachment's blocks into the shape Block Kit
// Builder accepts.
//
// ⛔ IT IS A LOSSY EXTRACTION AND THAT IS THE WHOLE POINT OF SAYING SO. What is
// dropped is `color`, `fallback`, the top-level `text`, `unfurl_links`,
// `unfurl_media` and `metadata` — which between them are the push notification,
// the screen-reader content and the state cue. Block Kit Builder can prove the
// BLOCKS are legal and legible; it cannot prove any of the rest, and nothing
// offline can.
func builderPayload(wire json.RawMessage) (json.RawMessage, string, string, error) {
	var p struct {
		Attachments []struct {
			Color    string            `json:"color"`
			Fallback string            `json:"fallback"`
			Blocks   []json.RawMessage `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(wire, &p); err != nil {
		return nil, "", "", err
	}
	if len(p.Attachments) != 1 {
		return nil, "", "", fmt.Errorf("expected exactly 1 attachment, got %d", len(p.Attachments))
	}
	att := p.Attachments[0]
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The renderer disables HTML escaping so an mrkdwn link survives as `<url|…>`.
	// The capture must not re-introduce it, or the pasted payload differs from the
	// sent one in exactly the characters mrkdwn cares about.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"blocks": att.Blocks}); err != nil {
		return nil, "", "", err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), att.Color, att.Fallback, nil
}

// SlackGoldenDir is where the captures live, relative to the repository root.
const SlackGoldenDir = "test/fixtures/slack"

// SlackGoldenFiles renders the corpus and returns the exact file contents, keyed
// by base name. The map is the single definition of what is checked in, so the
// test that verifies the files and the generator that writes them cannot drift.
func SlackGoldenFiles() (map[string][]byte, error) {
	captures, err := RenderSlackCards()
	if err != nil {
		return nil, err
	}

	files := make(map[string][]byte, len(captures)*2+1)
	index := make([]map[string]any, 0, len(captures))

	for _, c := range captures {
		files[c.Card.Name+".message.json"] = withNewline(indentJSON(c.Wire))
		files[c.Card.Name+".blockkit.json"] = withNewline(indentJSON(c.Builder))
		index = append(index, map[string]any{
			"name":               c.Card.Name,
			"what":               c.Card.What,
			"mode":               string(c.Card.Mode),
			"reason":             c.Card.View.Reason,
			"attachment_color":   c.Colour,
			"fallback":           c.Fallback,
			"rendered_hash":      c.Hash,
			"wire_bytes":         len(c.Wire),
			"blocks":             countBlocks(c.Builder),
			"paste_into_builder": c.Card.Name + ".blockkit.json",
			"exact_wire_payload": c.Card.Name + ".message.json",
		})
	}
	sort.Slice(index, func(i, j int) bool {
		return index[i]["name"].(string) < index[j]["name"].(string)
	})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"note": "Generated by test/harness/slack_cards.go. Paste the *.blockkit.json " +
			"files into https://app.slack.com/block-kit-builder — it cannot render " +
			"attachments, so the colour bar, the top-level text and the metadata are " +
			"NOT covered by that check. See docs/setup/slack.md.",
		"cards": index,
	}); err != nil {
		return nil, err
	}
	files["index.json"] = withNewline(buf.Bytes())

	return files, nil
}

func countBlocks(builder json.RawMessage) int {
	var p struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	_ = json.Unmarshal(builder, &p)
	return len(p.Blocks)
}

// WriteSlackGoldens writes the corpus into dir, creating it if needed. It is the
// `-update` path of the golden test and the whole of the generator.
func WriteSlackGoldens(dir string) error {
	files, err := SlackGoldenFiles()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}
