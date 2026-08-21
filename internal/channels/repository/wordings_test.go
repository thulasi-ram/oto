package repository_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/test/harness"
)

// Resolve's ORDER BY IS the precedence decision of ADR 0049, and the service that
// consumes it only walks the list and takes the first match per Stanza. So the
// rule lives in SQL, and a fake store cannot test it: these run against Postgres.

func newWording(stanza, template string, channel *uuid.UUID, priority int) domain.NewWording {
	return domain.NewWording{
		ChannelID: channel, Stanza: stanza, Template: template,
		Priority: priority, Enabled: true,
	}
}

func TestResolveOrdersMostSpecificFirstThenPriority(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	conn := newWebhookConnection(t, h, org, "alerts receiver")
	repo := repository.NewChannelRepository(h.Pool, h.Clock)
	mine, err := repo.Create(h.Ctx, org.Scope, newWebhook("mine", conn))
	require.NoError(t, err)
	theirs, err := repo.Create(h.Ctx, org.Scope, newWebhook("theirs", conn))
	require.NoError(t, err)

	wordings := repository.NewWordingRepository(h.Pool, h.Clock)
	// Created in an order that does NOT match the wanted precedence, so a passing
	// test cannot be an accident of insertion order.
	_, err = wordings.Create(h.Ctx, org.Scope, newWording("body", "house, late", nil, 900))
	require.NoError(t, err)
	_, err = wordings.Create(h.Ctx, org.Scope, newWording("body", "mine, late", &mine.ID, 900))
	require.NoError(t, err)
	_, err = wordings.Create(h.Ctx, org.Scope, newWording("body", "house, early", nil, 10))
	require.NoError(t, err)
	_, err = wordings.Create(h.Ctx, org.Scope, newWording("body", "mine, early", &mine.ID, 10))
	require.NoError(t, err)
	_, err = wordings.Create(h.Ctx, org.Scope, newWording("body", "someone else's", &theirs.ID, 1))
	require.NoError(t, err)

	got, err := wordings.Resolve(h.Ctx, org.Scope, mine.ID)
	require.NoError(t, err)

	var order []string
	for _, w := range got {
		order = append(order, w.Template)
	}
	require.Equal(t, []string{
		"mine, early", "mine, late", "house, early", "house, late",
	}, order,
		"ADR 0049: this destination's own wordings come before the org-wide house "+
			"voice, and within each scope priority orders LOWER FIRST. Another "+
			"destination's row must not appear at all, whatever its priority.")
}

func TestResolveExcludesWhatMustNeverBeResolved(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	conn := newWebhookConnection(t, h, org, "alerts receiver")
	repo := repository.NewChannelRepository(h.Pool, h.Clock)
	ch, err := repo.Create(h.Ctx, org.Scope, newWebhook("mine", conn))
	require.NoError(t, err)

	wordings := repository.NewWordingRepository(h.Pool, h.Clock)

	live, err := wordings.Create(h.Ctx, org.Scope, newWording("body", "live", &ch.ID, 100))
	require.NoError(t, err)

	off := newWording("body", "disabled", &ch.ID, 1)
	off.Enabled = false
	_, err = wordings.Create(h.Ctx, org.Scope, off)
	require.NoError(t, err)

	gone, err := wordings.Create(h.Ctx, org.Scope, newWording("title", "deleted", &ch.ID, 1))
	require.NoError(t, err)
	require.NoError(t, wordings.Delete(h.Ctx, org.Scope, gone.ID))

	got, err := wordings.Resolve(h.Ctx, org.Scope, ch.ID)
	require.NoError(t, err)
	require.Len(t, got, 1, "a disabled and a soft-deleted wording must never be resolved")
	require.Equal(t, live.ID, got[0].ID)
}

// TestResolveIsScopedToTheOrg — the partial index is per-org and so is the query;
// a wording must never reach another tenant's card.
func TestResolveIsScopedToTheOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	mineOrg, otherOrg := h.Org(), h.Org()
	conn := newWebhookConnection(t, h, mineOrg, "alerts receiver")
	repo := repository.NewChannelRepository(h.Pool, h.Clock)
	ch, err := repo.Create(h.Ctx, mineOrg.Scope, newWebhook("mine", conn))
	require.NoError(t, err)

	wordings := repository.NewWordingRepository(h.Pool, h.Clock)
	_, err = wordings.Create(h.Ctx, mineOrg.Scope, newWording("body", "mine", nil, 100))
	require.NoError(t, err)

	got, err := wordings.Resolve(h.Ctx, otherOrg.Scope, ch.ID)
	require.NoError(t, err)
	require.Empty(t, got, "another tenant's house voice must not resolve against this channel")
}

// TestWordingTimestampsComeFromTheApplicationClock is the 00032/00033 rule, held
// on the new table. The harness clock is pinned months behind the container's, so
// a column that took the database's clock is a certainty rather than a flake.
func TestWordingTimestampsComeFromTheApplicationClock(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	wordings := repository.NewWordingRepository(h.Pool, h.Clock)

	w, err := wordings.Create(h.Ctx, org.Scope, newWording("body", "x", nil, 100))
	require.NoError(t, err)
	require.Equal(t, h.Clock.Now().UTC(), w.CreatedAt.UTC(),
		"created_at must come from the injected clock; a DEFAULT now() here is the "+
			"trap 00032 and 00033 removed from every other table")
	require.Equal(t, w.CreatedAt, w.UpdatedAt,
		"both clock columns come from ONE read, so updated_at >= created_at cannot "+
			"fail on a row that was never updated")

	tmpl := "y"
	updated, err := wordings.Update(h.Ctx, org.Scope, w.ID, domain.WordingPatch{Template: &tmpl})
	require.NoError(t, err)
	require.GreaterOrEqual(t, updated.UpdatedAt.UnixNano(), updated.CreatedAt.UnixNano(),
		"GREATEST keeps updated_at monotonic across pods whose clocks disagree")
}

// TestTheStanzaIsNotPatchable — moving a wording from body to title is a different
// wording, not an edit: the read set differs, the budget differs, and the row's
// history would claim it had always been the new one.
func TestTheStanzaIsNotPatchable(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	wordings := repository.NewWordingRepository(h.Pool, h.Clock)

	w, err := wordings.Create(h.Ctx, org.Scope, newWording("body", "x", nil, 100))
	require.NoError(t, err)

	tmpl := "y"
	updated, err := wordings.Update(h.Ctx, org.Scope, w.ID, domain.WordingPatch{Template: &tmpl})
	require.NoError(t, err)
	require.Equal(t, "body", updated.Stanza, "no patch field can move a wording's stanza")
}
