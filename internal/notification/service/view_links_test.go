package service_test

import (
	"context"
	"net/url"
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
// The guard has two halves and they are only correct together, so the tests are
// split the same way. The kind half is SQL — `groupFactsSQL` is what refuses to
// hand a non-Alertmanager base_url upstairs — and the first two tests run it
// against a REAL Postgres, because a snapshot literal cannot be wrong about a
// query on the same terms. The renderer's half is "empty means no link and no
// button", which the third test pins from a snapshot literal, since that is the
// contract every `SnapshotSource` must be held to and not just this one.

// cardFor seeds `org → cluster → source(kind) → group(labels)` and projects the
// card oto would post for it, THROUGH the real read model.
func cardFor(
	t *testing.T, h *harness.H, kind, baseURL string, groupLabels map[string]string,
) *service.NotificationView {
	t.Helper()
	return cardForGroup(t, h, kind, baseURL, groupLabels, false)
}

// cardForGroup is cardFor with one extra question: whether to blank the group's
// labels after seeding it.
//
// ⭐ A LABEL-LESS GROUP CAN NO LONGER BE BUILT, ONLY INHERITED. Since ADR 0038 a
// group's `group_labels` are `SplitLabels` of the alert's own set, and a label
// set must carry a non-empty `alertname` — so every group opened from now on has
// at least one label to filter on. `{}` is the shape every PRE-00050 generation
// still has on disk (00050 adds no backfill and re-keying live groups was
// rejected), which is why the bare-console link still has to work and still has
// to be tested. Blanking after the fact is how a legacy row is reached.
func cardForGroup(
	t *testing.T, h *harness.H, kind, baseURL string,
	groupLabels map[string]string, legacyBare bool,
) *service.NotificationView {
	t.Helper()

	org := h.Org()
	cluster := h.Cluster(org)
	source := h.SourceOfKind(org, cluster, kind, baseURL)
	group := h.GroupWith(org, source, cluster, groupLabels)
	if legacyBare {
		h.Exec(`UPDATE alert_groups SET group_labels = '{}'::jsonb WHERE id = $1`, group.ID)
	}

	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: repository.NewSnapshotRepository(h.Pool, h.Clock),
		BaseURL:   "https://oto.example.com",
		Clock:     h.Clock,
	})
	require.NoError(t, err)

	view, err := views.Build(h.Ctx, harness.Scope(t, org.ID), service.ViewRequest{
		Notification: domain.Notification{GroupID: group.ID, Reason: domain.ReasonFired},
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

// TestAlertmanagerSourceGetsTheDeepLinks is the case the links exist for: for a
// source of kind `alertmanager` the API root and the UI root are the same
// origin, so all three URL shapes resolve and the Silence button is real.
func TestAlertmanagerSourceGetsTheDeepLinks(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	const base = "https://am.example.com"

	// The filter is built from the GROUP LABELS, so this card carries the two
	// filtered shapes.
	view := cardFor(t, h, "alertmanager", base, map[string]string{"alertname": "HighErrorRate", "severity": "critical"})

	// The filter names the AXES, not every label: `group_labels` are `SplitLabels`
	// of the alert's set (ADR 0038), and `severity` is not an axis. Filtering the
	// console by `alertname` is also the more useful of the two — it is the shape
	// the generation actually spans.
	escaped := url.QueryEscape(`{alertname="HighErrorRate"}`)
	require.Equal(t, base+"/#/alerts?filter="+escaped, view.Links.Alertmanager)
	require.Equal(t, base+"/#/silences/new?filter="+escaped, view.Links.AlertmanagerSilenceNew)
	require.True(t, hasSilenceAction(view),
		"an alertmanager source's card must carry the one action v1 offers")

	// A group with no labels to filter on still gets the third shape — the bare
	// alert list — because there is still a console to open.
	bare := cardForGroup(t, h, "alertmanager", base,
		map[string]string{"alertname": "HighErrorRate"}, true)
	require.Equal(t, base+"/#/alerts", bare.Links.Alertmanager)
}

// TestGrafanaSourceGetsNoDeepLink is the defect.
//
// The card is otherwise identical to the one above — same labels, same base_url
// shape, same everything but `alert_sources.kind` — and it must come out with
// nothing rather than with Alertmanager's URL shapes pointed at a Grafana. This
// is the verdict `silenceBaseURLs` already reaches on the silences feed by
// leaving such a source out of its map.
func TestGrafanaSourceGetsNoDeepLink(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	view := cardFor(t, h, "grafana",
		"https://grafana.example.com/api/alertmanager/grafana",
		map[string]string{"alertname": "HighErrorRate", "severity": "critical"})

	require.Empty(t, view.Links.Alertmanager,
		"a grafana base_url is an API prefix, not a console oto may send anyone to")
	require.Empty(t, view.Links.AlertmanagerSilenceNew)
	require.False(t, hasSilenceAction(view),
		"a Silence button that 404s costs the operator the affordance AND the trust")

	// The rest of the card is untouched: this is a missing link, not a broken card.
	require.NotEmpty(t, view.Links.Group)
	require.NotEmpty(t, view.Actions, "the Acknowledge button is still there")
}

// TestSourceWithNoVouchedURLGetsNoDeepLink pins the renderer's own half of the
// guard, at the seam rather than at the database.
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
		Snapshots:    unvouchedSnapshots{},
		BaseURL:      "https://oto.example.com",
		MaxInstances: 10,
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
			ID: q.GroupID, GroupKey: "gk", Generation: 1, Title: "A group",
			GroupLabels:     map[string]string{"alertname": "HighErrorRate", "severity": "critical"},
			State:           "open",
			AlertmanagerURL: "",
		},
	}, nil
}
