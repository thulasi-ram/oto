package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// The card's Alertmanager links are the one place oto invites an operator to
// LEAVE oto, mid-incident, on oto's word that there is something at the other
// end. `/#/alerts` and `/#/silences/new` are Alertmanager's OWN console routes;
// a `grafana` source's base_url is an Alertmanager-compatible API prefix whose
// console keeps silences at `/alerting/silences` instead, so the same three URLs
// built for a grafana source are three 404s — one of them wearing a "Silence"
// button, which is the single affordance v1 offers (R3, §H.3).
//
// ⛔⛔ THE GUARD HAD TWO HALVES AND THE SQL HALF NO LONGER EXISTS (git-bug
// `7570090`, migration `00069`). `groupFactsSQL` was what refused to hand a
// non-Alertmanager base_url upstairs, and it is deleted with `alert_groups`:
// `conversationFactsSQL` reads `alert_cases` joined to `alerts`, and NEITHER TABLE
// HAS A `source_id`, so there is no `alert_sources` row to ask about `kind` and
// `GroupFacts.AlertmanagerURL` is now PERMANENTLY EMPTY for every source. The
// field's own comment states it, and `repository/snapshot.go:152` states it again
// as a deliberate degradation: "Restoring the affordance needs a column on
// `alert_cases`, which is a schema change and is not this one."
//
// ⚠️ SO THE V1 SILENCE BUTTON IS GONE FROM EVERY CARD, NOT JUST FROM GRAFANA'S.
// That is a product regression, taken knowingly, and the first test below is what
// keeps it from being a silent one.

// TestNoCardCarriesADeepLinkOrASilenceButton pins the loss, THROUGH the real read
// model.
//
// ⛔⛔ IT ASSERTS A REGRESSION, in the shape `domain/refire_visibility_test.go` uses
// for the same purpose: the defect is visible in the test suite rather than
// discovered by an operator at 03:00.
//
// ⛔ IT REPLACES TWO TESTS AND IS NOT EITHER OF THEM.
// `TestAlertmanagerSourceGetsTheDeepLinks` asserted that an `alertmanager` source
// DOES get all three URL shapes and the Silence action; that behaviour is
// structurally unreachable, so the test is DELETED rather than retargeted — there
// is nothing left that could make it pass. `TestGrafanaSourceGetsNoDeepLink`
// asserted that a `grafana` source does NOT; it passes today for a reason that has
// nothing to do with `kind`, which makes it a test that can no longer fail for its
// own reason, so it is deleted too. What is worth pinning is the fact that
// replaced them both, asserted over the source kind that used to be the POSITIVE
// case — because that is the one whose passing proves the affordance is gone.
//
// ⭐ IF YOU ARE HERE BECAUSE THIS FAILED, the affordance has been restored: put
// `TestAlertmanagerSourceGetsTheDeepLinks` back from git history, restore the
// `kind = 'alertmanager'` predicate this file's header describes, and delete this
// test.
func TestNoCardCarriesADeepLinkOrASilenceButton(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	// A source of kind `alertmanager` with a real console root: the case the links
	// exist FOR, and the one that used to produce all three shapes.
	view := cardFor(t, h, "alertmanager", "https://am.example.com",
		map[string]string{"alertname": "HighErrorRate", "severity": "critical"})

	require.Empty(t, view.Links.Alertmanager,
		"the read model vouched for an Alertmanager console it can no longer identify: "+
			"neither `alerts` nor `alert_cases` carries a `source_id`, so any URL here is "+
			"fabricated")
	require.Empty(t, view.Links.AlertmanagerSilenceNew)
	require.False(t, hasSilenceAction(view),
		"a Silence button that 404s costs the operator the affordance AND the trust")

	// The rest of the card is untouched: this is a missing link, not a broken card.
	require.NotEmpty(t, view.Links.Group)
	require.NotEmpty(t, view.Actions, "the Acknowledge button is still there")
}

// cardFor seeds `org → cluster → source(kind) → alert → case` and projects the card
// oto would post for it, THROUGH the real read model.
//
// ⛔ IT SEEDED A GROUP, AND `cardForGroup`'s `legacyBare` LEG IS DELETED WITH IT.
// That leg blanked `alert_groups.group_labels` to reach a pre-00050 generation with
// nothing to filter on, so that the BARE console link (`/#/alerts`, no filter) was
// still exercised. There is no such row and no such link: `GroupFacts.GroupLabels`
// is now the one Alert's own label set, which `NewLabelSet` refuses to build without
// a non-empty `alertname`, so a label-less conversation cannot be reached at all.
//
// ⚠️ THE SOURCE IS STILL SEEDED, AND IT IS NOW INERT. Nothing joins to it — that is
// precisely what this file is about — but seeding it keeps the fixture honest about
// what the tenant contains, so a restored `source_id` join finds a row rather than
// passing for the wrong reason.
func cardFor(
	t *testing.T, h *harness.H, kind, baseURL string, alertLabels map[string]string,
) *service.NotificationView {
	t.Helper()

	org := h.Org()
	cluster := h.Cluster(org)
	_ = h.SourceOfKind(org, cluster, kind, baseURL)
	alert := h.AlertWith(org, cluster, alertLabels)
	ac := h.Case(alert)

	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: repository.NewSnapshotRepository(h.Pool, h.Clock),
		BaseURL:   "https://oto.example.com",
		Clock:     h.Clock,
	})
	require.NoError(t, err)

	alertID := alert.ID
	view, err := views.Build(h.Ctx, harness.Scope(t, org.ID), service.ViewRequest{
		Notification: domain.Notification{
			ConversationKind: domain.ConversationCase,
			ConversationID:   ac.ID,
			CaseID:           &ac.ID,
			AlertID:          &alertID,
			Reason:           domain.ReasonFired,
		},
	})
	require.NoError(t, err)
	return view
}

// hasSilenceAction reports whether the card carries v1's silence affordance.
func hasSilenceAction(view *service.NotificationView) bool {
	for _, a := range view.Actions {
		if a.ID == "oto.noop.silence" {
			return true
		}
	}
	return false
}

// TestSourceWithNoVouchedURLGetsNoDeepLink pins the renderer's own half of the
// guard, at the seam rather than at the database.
//
// ⭐ IT IS THE HALF THAT SURVIVED `7570090` UNCHANGED, AND IT IS NOW THE ONLY HALF.
// The database no longer chooses between a vouched URL and an empty one — it always
// answers empty — so this is what still holds the CONTRACT rather than today's
// implementation of it: whatever hands over an empty `AlertmanagerURL` must produce
// a card with no link and no button.
//
// `SnapshotSource` is a PORT: the read model is today's implementation, and the
// contract it satisfies is that `AlertmanagerURL` is a vouched UI root or the
// empty string. Whatever hands over an empty one — a source that resolved to
// nothing, a future `alerts/service` snapshot, a kind oto has not learned yet —
// must produce a card with no link and no button rather than `"/#/alerts"`
// hanging off an empty origin.
func TestSourceWithNoVouchedURLGetsNoDeepLink(t *testing.T) {
	t.Parallel()

	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: unvouchedSnapshots{},
		BaseURL:   "https://oto.example.com",
	})
	require.NoError(t, err)

	view, err := views.Build(t.Context(), db.TenantScope{}, service.ViewRequest{
		Notification: domain.Notification{Reason: domain.ReasonFired},
	})
	require.NoError(t, err)

	require.Empty(t, view.Links.Alertmanager)
	require.Empty(t, view.Links.AlertmanagerSilenceNew)
	require.False(t, hasSilenceAction(view))
}

// unvouchedSnapshots is a `SnapshotSource` that answers with group labels worth
// filtering on and NO Alertmanager URL — the shape every source oto cannot vouch
// for arrives in. It exists so the renderer's guard is provable without a
// database saying it first.
type unvouchedSnapshots struct{}

func (unvouchedSnapshots) Snapshot(
	_ context.Context, _ db.TenantScope, q domain.SnapshotQuery,
) (domain.Snapshot, error) {
	return domain.Snapshot{
		Group: domain.GroupFacts{
			ID: q.CaseID, GroupKey: "", Generation: 1, Title: "A case",
			GroupLabels:     map[string]string{"alertname": "HighErrorRate", "severity": "critical"},
			State:           "open",
			AlertmanagerURL: "",
		},
	}, nil
}
