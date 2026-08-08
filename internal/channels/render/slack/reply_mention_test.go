package slack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// ⛔⛔ THE MENTION MUST BE IN THE TOP-LEVEL `text`, AND THIS FILE IS WHY.
//
// SPEC §H.1 S3 puts every oto block inside exactly ONE attachment, because that
// attachment is the only way to get the colour bar. Slack's in-channel
// `thread_broadcast` reference "cannot contain attachments or message buttons"
// (ADR 0020). So a mention rendered into a block is not merely un-notifying in
// the channel — it is not present in the channel copy at all. The reminder is the
// one message oto sends whose whole purpose is to reach somebody who has not
// engaged, and putting its mention where the channel cannot see it would be a
// feature that does nothing while appearing to work.
//
// This is a rendering rule that reads as a style preference and is not one, which
// is exactly the kind of rule that gets refactored away. Hence a test.

func reminderView() *domain.NotificationView {
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "o1", Slug: "acme", Name: "Acme"},
		Reason: "unacked_reminder",
		Group: domain.GroupView{
			ID: "g1", GroupKey: "gk_x", Generation: 1,
			Title: "HighErrorRate", Severity: "critical", State: "open",
			GroupLabels: map[string]string{"alertname": "HighErrorRate"},
			FiringCount: 1, TotalCount: 1, ClusterKey: "prod-eu",
		},
		Links: domain.Links{Group: "https://oto.example.com/groups/g1"},
	}
}

func render(t *testing.T, mentions []string) (topLevelText string, raw string) {
	t.Helper()
	r := slackrender.New(clock.New())
	msg, err := r.Render(context.Background(), reminderView(), domain.RenderOptions{
		Mode:         domain.ModeBroadcastReply,
		Verbosity:    domain.VerbosityAll,
		BaseURL:      "https://oto.example.com",
		MaxInstances: 10,
		Mentions:     mentions,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var payload struct {
		Text        string            `json:"text"`
		Attachments []json.RawMessage `json:"attachments"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	blocks := ""
	for _, a := range payload.Attachments {
		blocks += string(a)
	}
	return payload.Text, blocks
}

func TestTheReminderMentionGoesInTheTopLevelTextAndNotInABlock(t *testing.T) {
	t.Parallel()

	const user = "<@U024BE7LH>"
	const group = "<!subteam^SAZ94GDB8>"

	text, blocks := render(t, []string{user, group})

	if !strings.Contains(text, user) || !strings.Contains(text, group) {
		t.Fatalf("the mention is not in the top-level text: %q", text)
	}
	// ⛔ AND NOWHERE ELSE. A mention inside the attachment is invisible in the
	// channel copy of a broadcast.
	if strings.Contains(blocks, user) || strings.Contains(blocks, group) {
		t.Fatalf("the mention leaked into a block, where a broadcast cannot carry it: %s", blocks)
	}

	// The sentence still reads for the people who were NOT mentioned: the mention
	// is appended, never substituted for the fact.
	if !strings.Contains(text, "HighErrorRate") {
		t.Fatalf("the mention replaced the sentence: %q", text)
	}
}

func TestNoMentionsRendersNoMentionAtAll(t *testing.T) {
	t.Parallel()

	text, _ := render(t, nil)
	if strings.Contains(text, "<@") || strings.Contains(text, "<!") {
		t.Fatalf("an empty audience still produced a mention: %q", text)
	}
}

// ⛔ TestUnrecognisedMentionTokensAreDropped. This string goes verbatim into a
// Slack message, so anything that is not a known mention shape is an injection
// surface, not a mention.
func TestUnrecognisedMentionTokensAreDropped(t *testing.T) {
	t.Parallel()

	text, _ := render(t, []string{
		"@everyone", "<!everyone>", "plain words", "<https://evil.example|click>", "<@U024BE7LH>",
	})
	if !strings.Contains(text, "<@U024BE7LH>") {
		t.Fatalf("the one valid mention was dropped: %q", text)
	}
	for _, bad := range []string{"@everyone", "<!everyone>", "plain words", "evil.example"} {
		if strings.Contains(text, bad) {
			t.Errorf("%q reached the message text: %q", bad, text)
		}
	}
}
