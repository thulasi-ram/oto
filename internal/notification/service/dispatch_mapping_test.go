package service_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	channelsservice "github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
)

// ADR 0055 §2 in the dispatcher (git-bug 2205620): a webhook Connection's payload
// mapping is applied at claim time, its bytes are what the row records, and a
// mapping that does not render is a dead `config_invalid` delivery with nothing sent
// — never the plain envelope, and never oto's own render-failure alarm.

func withMapper(c *service.DispatchConfig) { c.Mapper = channelsservice.NewMapper() }

// setMapping puts a payload mapping on the fixture channel's connection.
func setMapping(t *testing.T, h dispatchRig, mapping string) {
	t.Helper()
	_, err := h.fx.pool.Exec(t.Context(),
		`UPDATE channel_connections SET payload_mapping = $1::jsonb
		  WHERE id = (SELECT connection_id FROM channels WHERE id = $2)`,
		mapping, h.fx.channel.ID)
	require.NoError(t, err)
}

// TestAMappingThatFailsAtSendTimeIsADeadDeliveryAndNothingIsSent is the ticket's
// sentence: dead, `config_invalid`, the attempt and the error on the row, nothing
// sent, the plain envelope never sent, and RenderInvalid unmoved — that counter is
// an oto bug, and this is the operator's configuration.
func TestAMappingThatFailsAtSendTimeIsADeadDeliveryAndNothingIsSent(t *testing.T) {
	t.Parallel()

	tgt := &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)}
	h := newDispatchRig(t, tgt, withMapper)
	// The stub renderer's envelope carries `mode`; interpolated outside quotes it
	// renders `{"title": post_root}`, which is not JSON.
	setMapping(t, h, `{"body": "{\"title\": {{ mode }}}"}`)
	ctx := t.Context()

	rows := h.rowsFor(t, h.evaluate(t, domain.ReasonFired, 1))
	require.Len(t, rows, 1)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, rows[0].ID))

	dead, err := h.deliveries.Get(ctx, h.fx.scope, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliveryDead, dead.Status)
	require.Equal(t, domain.ClassConfigInvalid, dead.ErrorClass)
	require.Contains(t, dead.Error, "payload mapping",
		"the dead row must say what failed, not just a provider code")

	var attempt string
	require.NoError(t, json.Unmarshal(dead.Rendered, &attempt),
		"the refused attempt is kept on the row, as a string since it is not JSON")
	require.Contains(t, attempt, `"title": post_root`)

	require.Zero(t, tgt.delivers, "⛔ nothing may be sent when the mapping fails — least of all the envelope")
	require.Zero(t, counterValue(t,
		h.metrics.RenderInvalid.WithLabelValues("webhook", "webhook.json", "post_root")),
		"RenderInvalid means oto built a card it cannot send; a broken mapping is configuration")

	c, err := repository.NewChannelRepository(h.fx.pool).Get(ctx, h.fx.scope, h.fx.channel.ID)
	require.NoError(t, err)
	require.Equal(t, domain.HealthConfigInvalid, c.HealthStatus)
}

// TestAMappedDeliverySendsAndRecordsTheMappedBody: the destination is handed the
// mapped body, marked mapped, and the row records those bytes — not the envelope.
func TestAMappedDeliverySendsAndRecordsTheMappedBody(t *testing.T) {
	t.Parallel()

	tgt := &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)}
	h := newDispatchRig(t, tgt, withMapper)
	setMapping(t, h, `{"body": "{\"title\": \"{{ mode }}\"}", "headers": {"X-Vendor": "oto"}}`)
	ctx := t.Context()

	rows := h.rowsFor(t, h.evaluate(t, domain.ReasonFired, 1))
	require.Len(t, rows, 1)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, rows[0].ID))

	require.Len(t, tgt.sent, 1)
	sent := tgt.sent[0]
	require.True(t, sent.Mapped)
	require.JSONEq(t, `{"title": "post_root"}`, string(sent.Payload))
	require.Equal(t, "oto", sent.Headers["X-Vendor"])

	row, err := h.deliveries.Get(ctx, h.fx.scope, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliverySent, row.Status)
	require.JSONEq(t, `{"title": "post_root"}`, string(row.Rendered),
		"C11: the row records the bytes the destination was sent")
	require.Equal(t, sent.Hash, row.RenderedHash)
}

// TestAMappedChannelWithNoMapperIsDeadNotAnEnvelope: a deployment that cannot map
// must not quietly send the envelope to a channel configured never to get one.
func TestAMappedChannelWithNoMapperIsDeadNotAnEnvelope(t *testing.T) {
	t.Parallel()

	tgt := &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)}
	h := newDispatchRig(t, tgt)
	setMapping(t, h, `{"body": "{}"}`)
	ctx := t.Context()

	rows := h.rowsFor(t, h.evaluate(t, domain.ReasonFired, 1))
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, rows[0].ID))

	dead, err := h.deliveries.Get(ctx, h.fx.scope, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliveryDead, dead.Status)
	require.Equal(t, domain.ClassConfigInvalid, dead.ErrorClass)
	require.Zero(t, tgt.delivers)
}

// TestEveryReasonIsAMappingFact holds channels' copy of the fact list to this
// module's Reasons: a Reason added here and not there is a fact no mapping was ever
// checked against.
func TestEveryReasonIsAMappingFact(t *testing.T) {
	t.Parallel()
	var reasons []string
	for _, r := range domain.AllReasons() {
		reasons = append(reasons, string(r))
	}
	require.Equal(t, reasons, chdomain.MappingFacts(),
		"channels/domain.mappingFacts must list every Reason, in order")
}
