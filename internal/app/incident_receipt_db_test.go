package app

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	incidentsdomain "github.com/thulasiram/oto/internal/incidents/domain"
	incidentsrepo "github.com/thulasiram/oto/internal/incidents/repository"
	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ ADR 0052 §5'S OUTBOUND MAPPING, ACROSS THE SEAM IT CROSSES (git-bug 506ff21,
// migration 00089). `notification` writes the receipt — only the dispatcher sees a
// receiver's answer — and `incidents` reads it, because it hangs off the Incident and
// is shown wherever the Incident is: its page, and its card, which the notification
// layer builds from the very Detail read here. So this drives the two real
// repositories against one real table rather than either side against a fake.

// TestAnEchoedIncidentIsRecordedOncePerChannelAndShownOnTheIncident is the receipt's
// whole life: the first valid echo is kept; a retry of the same delivery and a later
// fact answered with a different link record nothing; the Incident's Detail carries
// it, named by the channel it came back from.
func TestAnEchoedIncidentIsRecordedOncePerChannelAndShownOnTheIncident(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	user := h.User(org)
	alice, err := incidentsdomain.Human(user.ID, "alice")
	require.NoError(t, err)

	incidents := incidentsrepo.NewIncidentRepository(h.Pool)
	ref, err := incidents.Insert(h.Ctx, org.Scope, h.Now(), alice)
	require.NoError(t, err)
	member := h.Case(h.Alert(org, h.Cluster(org)))
	require.NoError(t, incidents.AddMember(h.Ctx, org.Scope, ref.ID, member.ID, h.Now(), alice))

	conn := h.WebhookConnection(org)
	channelID := id.New()
	h.Exec(`INSERT INTO channels (id, org_id, type, name, config, connection_id, renderer, created_at, updated_at)
	        VALUES ($1, $2, 'webhook', 'incident-tool', '{}'::jsonb, $3, 'webhook.json', $4, $4)`,
		channelID, org.ID, conn.ID, h.Now())

	receipts := notifrepo.NewIncidentReceiptRepository(h.Pool)
	first := notifdomain.IncidentReceipt{
		IncidentID: ref.ID, ChannelID: channelID,
		ExternalURL: "https://tool.example/incidents/42", ExternalID: "INC-42",
		RecordedAt: h.Now(),
	}
	wrote, err := receipts.Record(h.Ctx, org.Scope, first)
	require.NoError(t, err)
	require.True(t, wrote, "the first valid echo for an (Incident, channel) is the receipt")

	// The same delivery retried (oto's queue is at-least-once), and then a later fact
	// the tool answered with a different link: neither is believed over the first.
	h.Advance(time.Minute)
	wrote, err = receipts.Record(h.Ctx, org.Scope, first)
	require.NoError(t, err)
	require.False(t, wrote, "a retried delivery recorded its echo a second time")
	later := first
	later.ExternalURL, later.ExternalID, later.RecordedAt = "https://tool.example/incidents/99", "INC-99", h.Now()
	wrote, err = receipts.Record(h.Ctx, org.Scope, later)
	require.NoError(t, err)
	require.False(t, wrote, "a later answer overwrote the incident the tool first opened")

	detail, err := incidents.Get(h.Ctx, org.Scope, ref.Number)
	require.NoError(t, err)
	require.Len(t, detail.Outbound, 1)
	got := detail.Outbound[0]
	require.Equal(t, channelID, got.ChannelID)
	require.Equal(t, "incident-tool", got.ChannelName)
	require.Equal(t, "https://tool.example/incidents/42", got.ExternalURL)
	require.Equal(t, "INC-42", got.ExternalID)
	require.Equal(t, "active", detail.State().String(),
		"the receipt is not state: the Incident is still read off its member Cases")

	// Another tenant cannot write a receipt against this Incident, nor read it.
	other := h.Org()
	wrote, err = receipts.Record(h.Ctx, other.Scope, notifdomain.IncidentReceipt{
		IncidentID: ref.ID, ChannelID: channelID, ExternalID: "theirs", RecordedAt: h.Now(),
	})
	require.NoError(t, err)
	require.False(t, wrote, "a receipt was written against another org's Incident")
}

// TestAReceiptTheTableWouldRefuseIsDroppedNotRaised: the write runs in the
// dispatcher's TX 2, beside the row that says the message went out, and an error
// there would roll `sent` back and deliver the message again. So a value that
// slipped past the provider's validation is no row, never an error.
func TestAReceiptTheTableWouldRefuseIsDroppedNotRaised(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	user := h.User(org)
	alice, err := incidentsdomain.Human(user.ID, "alice")
	require.NoError(t, err)
	incidents := incidentsrepo.NewIncidentRepository(h.Pool)
	ref, err := incidents.Insert(h.Ctx, org.Scope, h.Now(), alice)
	require.NoError(t, err)

	conn := h.WebhookConnection(org)
	channelID := id.New()
	h.Exec(`INSERT INTO channels (id, org_id, type, name, config, connection_id, renderer, created_at, updated_at)
	        VALUES ($1, $2, 'webhook', 'incident-tool', '{}'::jsonb, $3, 'webhook.json', $4, $4)`,
		channelID, org.ID, conn.ID, h.Now())

	receipts := notifrepo.NewIncidentReceiptRepository(h.Pool)
	for _, bad := range []notifdomain.IncidentReceipt{
		{IncidentID: ref.ID, ExternalURL: "http://tool.example/i/1"},
		{IncidentID: ref.ID, ExternalURL: "HTTPS://tool.example/i/1"},
		{IncidentID: ref.ID, ExternalID: strings.Repeat("x", 300)},
		{IncidentID: id.New(), ExternalID: "INC-1"}, // an Incident that does not exist
	} {
		bad.ChannelID, bad.RecordedAt = channelID, h.Now()
		wrote, err := receipts.Record(h.Ctx, org.Scope, bad)
		require.NoError(t, err, "a refusable receipt raised instead of being dropped: %+v", bad)
		require.False(t, wrote, "a receipt the table forbids was written: %+v", bad)
	}

	detail, err := incidents.Get(h.Ctx, org.Scope, ref.Number)
	require.NoError(t, err)
	require.Empty(t, detail.Outbound)
}
