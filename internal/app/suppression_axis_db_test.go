package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	groupingrepo "github.com/thulasiram/oto/internal/grouping/repository"
	statsrepo "github.com/thulasiram/oto/internal/stats/repository"
	"github.com/thulasiram/oto/test/harness"
)

// ---------------------------------------------------------------------------
// git-bug 121e569, Done-when 4 — A FIRING ALERT THAT SOMEBODY HAS SILENCED IS
// STILL FIRING, AND EVERY COUNTER HAS TO SAY SO.
//
// ⭐⭐ THIS IS THE ASSERTION THAT FAILED BEFORE ADR 0041. `alerts.state` admitted
// `suppressed`, `Case.AlertState()` tested suppression FIRST, and the projection
// wrote that one word — so `StateFiring` was UNREACHABLE for a silenced open
// episode and every `state = 'firing'` reader returned zero for it. A silence is
// the most common thing an operator does to a firing alert, so what this lost was
// not an edge case: the dashboard under-reported during exactly the window
// somebody had silenced something.
//
// ⭐ IT LIVES IN `app` BECAUSE THE DEFECT DID. Three modules had each worked
// around the same overloaded column differently — `alerts` counted the roll-up,
// `grouping` counted the group card, `stats` counted the overview — so a test
// inside any one of them would have proved a third of the claim. This drives ONE
// write path and then asks all three readers, which is the only shape that can
// fail if one of them is fixed and the others are not.
//
// ⚠️ THE WRITE IS THE REAL RECONCILER PATH, not a seeded row. `suppressed` is
// reconciler-only by construction (Alertmanager's MuteStage drops muted alerts
// before the webhook fires), so the observation is built the way
// `sources/service.reconcile` builds one and driven through `ObserveBatch`. A
// hand-written INSERT would have proved the readers agree with a fixture rather
// than with the machine.
// ---------------------------------------------------------------------------

// TestAFiringAlertThatIsSilencedIsStillCountedAsFiring is Done-when 4.
func TestAFiringAlertThatIsSilencedIsStillCountedAsFiring(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)
	ctx := t.Context()

	labels := harness.Labels(t, harness.DefaultLabels())
	alertKey := harness.AlertKey(org.ID, cluster.Key, labels)

	svc, err := alertsservice.New(alertsservice.Deps{
		Alerts:     alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		Cases:      alertsrepo.NewCaseRepository(h.Pool),
		Events:     alertsrepo.NewEventRepository(h.Pool, h.Clock),
		Snoozes:    alertsrepo.NewSnoozeRepository(h.Pool, h.Clock),
		Tx:         alertsrepo.NewTxRunner(h.Pool),
		AlertBatch: alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		OccBatch:   alertsrepo.NewCaseRepository(h.Pool),
		Clock:      h.Clock,
		Logger:     quietLogger(),
	})
	require.NoError(t, err)

	now := h.Clock.Now()
	groupID := group.ID
	observe := func(src alertsdomain.ObservationSource, status, reason string, by alertsdomain.SuppressedBy) {
		t.Helper()
		obs := alertsdomain.Observation{
			Source:            src,
			ClusterID:         cluster.ID,
			ClusterKey:        cluster.Key,
			AlertKey:          alertKey,
			SourceFingerprint: alertsdomain.ComputeSourceFingerprint(labels),
			Labels:            labels,
			Annotations:       map[string]string{},
			Status:            status,
			SuppressionReason: reason,
			SuppressedBy:      by,
			SourceStartsAt:    now.Add(-time.Minute),
			SourceUpdatedAt:   h.Clock.Now(),
			ObservedAt:        h.Clock.Now(),
		}
		_, err := svc.ObserveBatch(ctx, org.Scope, []alertsdomain.Observation{obs},
			alertsservice.ObserveOptions{GroupID: &groupID})
		require.NoError(t, err)
	}

	// The alert fires, through the webhook, the way every alert starts.
	observe(alertsdomain.ObservedByIngest, "firing", "", alertsdomain.SuppressedBy{})

	// Then somebody silences it, and the reconciler is the only witness that can
	// see that happen.
	h.Clock.Advance(time.Minute)
	observe(alertsdomain.ObservedByReconciler, "suppressed", "silence",
		alertsdomain.SuppressedBy{SilencedBy: []string{"sil-abc"}})

	// ---- Done-when 2: the row itself ---------------------------------------

	var state string
	var supReason *string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT state, suppression_reason FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		org.ID, alertKey.String()).Scan(&state, &supReason))

	require.Equal(t, "firing", state,
		"a firing alert that is silenced must read state=firing. `suppressed` in this "+
			"column is the defect git-bug 121e569 is about: it occupies the slot `firing` "+
			"needs, and suppression is a statement about whether Alertmanager is "+
			"DELIVERING the signal, not a phase of the signal's life.")
	require.NotNil(t, supReason,
		"state=firing with no suppression_reason means the silence was LOST, not moved. "+
			"The two columns are one fact each and both have to be written.")
	require.Equal(t, "silence", *supReason)

	// ---- Done-when 4, reader 1: the alert roll-up (alert.go) ----------------

	alerts := alertsrepo.NewAlertRepository(h.Pool, h.Clock, false)
	buckets, _, err := alerts.Rollup(ctx, org.Scope, alertsdomain.AlertFilter{},
		alertsdomain.RollupByAlertName, "", 50)
	require.NoError(t, err)
	require.Len(t, buckets, 1)

	require.Equal(t, 1, buckets[0].Firing,
		"THE ASSERTION THAT FAILED BEFORE ADR 0041. `count(*) FILTER (WHERE state = "+
			"'firing')` returned 0 for a silenced firing alert, so a dashboard "+
			"under-reported during exactly the window somebody had silenced something.")
	require.Equal(t, 1, buckets[0].Suppressed,
		"and the second axis has to survive: `suppressed` is now a SUBSET of `firing` "+
			"and answers who is not being told, rather than replacing the fact that it fires.")
	require.Equal(t, alertsdomain.StateSuppressed, buckets[0].RollupState(),
		"the DISPLAY roll-up is unchanged: a bucket whose live members are ALL silenced "+
			"still reads suppressed. Because `Suppressed` is now a subset of `Firing`, "+
			"RollupState asks `Firing > Suppressed` rather than `Suppressed > 0` — the old "+
			"spelling would have made this bucket read firing and lost the badge.")

	// ---- Done-when 4, reader 2: the group card (grouping/member.go) ---------

	members := groupingrepo.NewMemberRepository(h.Pool, h.Clock)
	counts, _, err := members.Rollup(ctx, org.Scope, group.ID)
	require.NoError(t, err)

	require.Equal(t, 1, counts.Firing,
		"the group card's \"12 alerts, 3 firing\" counted `a.state = 'firing'` and "+
			"therefore skipped every silenced member.")
	require.Equal(t, 1, counts.Suppressed)
	require.Equal(t, 1, counts.Live(),
		"Live() must not double-count. `Firing` now includes the silenced members, so "+
			"the old `Firing + Suppressed` would hold a generation open on members that "+
			"do not exist.")

	// ---- Done-when 4, reader 3: the overview (stats/stats.go) ---------------

	stats := statsrepo.NewStatsRepository(h.Pool)
	overview, err := stats.Overview(ctx, org.Scope,
		now.Add(-24*time.Hour), h.Clock.Now().Add(time.Hour), nil)
	require.NoError(t, err)

	require.Equal(t, 1, overview.Alerts.Firing,
		"\"is anything still on fire?\" is the one question this number exists to "+
			"answer, and it answered no while an operator was looking at a silenced fire.")
	require.Equal(t, 1, overview.Alerts.Suppressed)

	// ---- and the display reading is unchanged for a human ------------------

	alert, err := alerts.GetByAlertKey(ctx, org.Scope, alertKey.String())
	require.NoError(t, err)
	require.Equal(t, alertsdomain.StateFiring, alert.State(),
		"the STORED state, which is what every aggregate counts")
	require.Equal(t, alertsdomain.StateSuppressed, alert.DisplayState(),
		"the DISPLAY state, which is what one row's chip renders. The API contract did "+
			"not change: only the column underneath it stopped hiding firing alerts.")
	require.Equal(t, []string{"sil-abc"}, alert.SuppressedBy().SilencedBy,
		"and the witness travels with it, so a card can still deep-link the silence")

	// ---- the silence lifts, and the axis clears -----------------------------

	h.Clock.Advance(time.Minute)
	observe(alertsdomain.ObservedByIngest, "firing", "", alertsdomain.SuppressedBy{})

	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT state, suppression_reason FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		org.ID, alertKey.String()).Scan(&state, &supReason))
	require.Equal(t, "firing", state)
	require.Nil(t, supReason,
		"a witness left behind after the silence expired makes oto go on saying "+
			"\"silenced by sil-abc\" about an alert nobody is silencing")

	// ---- and it clears on resolution too, which is what alerts_suppress_ck asks

	h.Clock.Advance(time.Minute)
	observe(alertsdomain.ObservedByReconciler, "suppressed", "silence",
		alertsdomain.SuppressedBy{SilencedBy: []string{"sil-def"}})
	h.Clock.Advance(time.Minute)
	observe(alertsdomain.ObservedByIngest, "resolved", "", alertsdomain.SuppressedBy{})

	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT state, suppression_reason FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		org.ID, alertKey.String()).Scan(&state, &supReason))
	require.Equal(t, "resolved", state)
	require.Nil(t, supReason,
		"alerts_suppress_ck refuses a reason on a terminal alert, and the projection "+
			"has to arrive already agreeing with it rather than earning a 23514")
}
