package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/channels/render/webhookjson"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// TestAStoredWordingReachesTheCard is the feature, end to end, with nothing faked
// between the row and the rendered bytes: a real Postgres row, the real
// repository, the real resolver, the real renderers.
//
// ⭐ IT EXISTS BECAUSE EVERY LAYER OF THIS FEATURE PASSES ITS OWN TESTS AND THAT
// PROVED NOTHING. The repository's Create was broken in every case for as long as
// only a fake store exercised the walk above it — the layers each passed and the
// path between them had never been run once.
func TestAStoredWordingReachesTheCard(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	conns := repository.NewConnectionRepository(h.Pool, h.Clock)
	conn, err := conns.Create(h.Ctx, org.Scope, domain.NewConnection{
		Type: domain.TypeWebhook, Name: "receiver", Config: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	channels := repository.NewChannelRepository(h.Pool, h.Clock)
	ch, err := channels.Create(h.Ctx, org.Scope, domain.NewInstance{
		Type: domain.TypeWebhook, Name: "alerts", Config: json.RawMessage(`{}`),
		ConnectionID: conn.ID, Renderer: "webhook.json", Verbosity: domain.VerbosityAll,
		ThreadUpdates: true, ShowFieldEmoji: true, Enabled: true,
	})
	require.NoError(t, err)

	wordings := repository.NewWordingRepository(h.Pool, h.Clock)
	// The sentence the whole feature was justified by: oto's own facts, which
	// Prometheus does not know and therefore cannot be templated upstream.
	const tmpl = `{{ alert.name | default: "a signal" }} has been firing ` +
		`{{ group.firing_for | default: "a while" }}, ` +
		`{{ alert.total_cases | default: 1 | plural: "time", "times" }} this week, and is ` +
		`{{ case.ack_state | default: "unacked" | bold }}`
	require.Empty(t, service.ValidateWording("body", tmpl, nil, nil, 100),
		"the sentence this feature exists for must survive its own save-time gate")

	_, err = wordings.Create(h.Ctx, org.Scope, domain.NewWording{
		ChannelID: &ch.ID, Stanza: "body", Template: tmpl, Priority: 100, Enabled: true,
	})
	require.NoError(t, err)

	resolver := service.NewWordings(wordings)
	view := e2eView()
	resolved := resolver.For(h.Ctx, org.Scope, ch.ID, view)
	require.Equal(t, tmpl, resolved["body"], "the stored wording did not resolve")

	opts := domain.RenderOptions{
		Mode: domain.ModePostRoot, Verbosity: domain.VerbosityAll,
		BaseURL: "http://localhost:8080", MaxInstances: 10, ShowFieldEmoji: true,
		Wordings: resolved,
	}

	t.Run("slack", func(t *testing.T) {
		msg, err := slack.New(clock.New()).Render(context.Background(), view, opts)
		require.NoError(t, err)
		require.NoError(t, slack.Validate(msg.Payload), "the card must still pass §L.6")

		body := sectionContaining(t, msg.Payload, "has been firing")
		require.Contains(t, body, "HighErrorRate has been firing")
		require.Contains(t, body, "4 times this week")
		require.Contains(t, body, "*unacked*", "slack spells bold with ONE asterisk")
	})

	t.Run("webhook", func(t *testing.T) {
		msg, err := webhookjson.New(clock.New()).Render(context.Background(), view, opts)
		require.NoError(t, err)

		var env struct {
			Rendered map[string]string `json:"rendered"`
		}
		require.NoError(t, json.Unmarshal(msg.Payload, &env))
		require.Contains(t, env.Rendered["body"], "4 times this week")
		require.NotContains(t, env.Rendered["body"], "*",
			"the webhook is not a degraded Slack: one template, two spellings")
		require.Contains(t, env.Rendered["body"], "unacked",
			"the words survive when the markup does not")
	})
}

// sectionContaining finds the rendered block carrying want, so the assertion does
// not depend on block ordering.
func sectionContaining(t *testing.T, payload []byte, want string) string {
	t.Helper()
	var p struct {
		Attachments []struct {
			Blocks []struct {
				Text *struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		} `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(payload, &p))
	for _, a := range p.Attachments {
		for _, b := range a.Blocks {
			if b.Text != nil && strings.Contains(b.Text.Text, want) {
				return b.Text.Text
			}
		}
	}
	t.Fatalf("no block contains %q in %s", want, payload)
	return ""
}

func e2eView() *domain.NotificationView {
	at := harness.Epoch
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: uuid.NewString(), Slug: "acme", Name: "Acme"},
		Reason: "fired",
		Group: domain.GroupView{
			ID: uuid.NewString(), GroupKey: "gk", Generation: 1, Title: "HighErrorRate",
			State: "open", Severity: "critical", FiringCount: 1, TotalCount: 1,
			GroupLabels: map[string]string{"service": "checkout"},
			StartedAt:   at.Add(-23 * time.Minute), FirstSeenAt: at, LastActivityAt: at,
		},
		Alerts: []domain.AlertView{{
			ID: uuid.NewString(), AlertName: "HighErrorRate", Severity: "critical",
			Service: "checkout", State: "firing", AckState: "unacked",
			Annotations: map[string]string{"description": "oto's own default body"},
			TotalCases:  4,
		}},
		Case: &domain.CaseView{
			ID: uuid.NewString(), Seq: 4, State: "firing", AckState: "unacked", StartedAt: at,
		},
		RenderedAt: at,
	}
}
