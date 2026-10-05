package repository_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// A template's reply body (ADR 0051, migration 00082) against real Postgres:
// stored as written, NULL for "oto's own replies" whichever way that is spelled,
// clearable, and versioned with the card it belongs to.
func TestATemplateCarriesItsReplyBodyAndVersionsWithIt(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewTemplateRepository(h.Pool, h.Clock)

	const reply = `{% if reason == 'all_resolved' %}[]{% endif %}`
	created, err := repo.Create(h.Ctx, org.Scope, domain.NewNotificationTemplate{
		ID: id.New(), Name: "calm", Provider: "slack", Format: "raw",
		Source: `[]`, ReplySource: reply, Enabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, reply, created.ReplySource)
	require.Equal(t, 1, created.Version)

	// "" on create is "no reply body", and the column's floor would refuse it as a
	// value — so it must reach the database as NULL, not as ''.
	bare, err := repo.Create(h.Ctx, org.Scope, domain.NewNotificationTemplate{
		ID: id.New(), Name: "bare", Provider: "slack", Format: "card", Source: "# x", Enabled: true,
	})
	require.NoError(t, err, "a template with no reply body must not trip notification_templates_reply_source_ck")
	require.Empty(t, bare.ReplySource)

	// A patch that does not mention the reply leaves it, and does not bump.
	name := "calmer"
	renamed, err := repo.Update(h.Ctx, org.Scope, created.ID, domain.NotificationTemplatePatch{Name: &name})
	require.NoError(t, err)
	require.Equal(t, reply, renamed.ReplySource)
	require.Equal(t, 1, renamed.Version, "a rename changes no bytes on any message")

	// Changing the reply is a new revision: a delivery attributed to v1 must not
	// silently mean a different reply than one attributed to v2.
	next := `{% if reason == 'acked' %}[]{% endif %}`
	changed, err := repo.Update(h.Ctx, org.Scope, created.ID, domain.NotificationTemplatePatch{ReplySource: &next})
	require.NoError(t, err)
	require.Equal(t, next, changed.ReplySource)
	require.Equal(t, 2, changed.Version)

	// "" clears it back to oto's own replies, and that is a revision too.
	empty := ""
	cleared, err := repo.Update(h.Ctx, org.Scope, created.ID, domain.NotificationTemplatePatch{ReplySource: &empty})
	require.NoError(t, err)
	require.Empty(t, cleared.ReplySource)
	require.Equal(t, 3, cleared.Version)

	// Clearing what is already clear changes nothing, so it is not a revision.
	again, err := repo.Update(h.Ctx, org.Scope, created.ID, domain.NotificationTemplatePatch{ReplySource: &empty})
	require.NoError(t, err)
	require.Equal(t, 3, again.Version)

	got, err := repo.Get(h.Ctx, org.Scope, created.ID)
	require.NoError(t, err)
	require.Empty(t, got.ReplySource)
}
