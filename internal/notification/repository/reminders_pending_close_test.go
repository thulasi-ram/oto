package repository_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/repository"
)

// ⭐⭐ W REMOVES NOISE FROM THE CASE LIST AND USED TO PUT IT BACK IN SLACK.
//
// Migration 00057 gave a Case a RETENTION WINDOW: when the alert resolves
// upstream the episode does not close, the close is held on
// `alert_cases.resolve_pending_at` for W, and a re-fire inside W lands in the
// still-open case. The whole point is that a flapping signal costs one case and
// one conversation instead of six.
//
// ⛔ THE UNACKED REMINDER COULD NOT SEE ANY OF THAT. Its liveness clause spells
// "still burning" as `alerts.state = 'firing'`, and the deferral is written as a
// transition with From == To, so `alerts.state` stays 'firing' for the entire
// window. A case holding an upstream resolve therefore looked identical to one
// whose alert is still going off, and the sweep kept nagging about an alert
// upstream had already called resolved — the noise W exists to remove, re-made
// by the one reader that never heard about W.
//
// ⛔ THIS IS AN INTEGRATION TEST AGAINST A REAL POSTGRES ON PURPOSE. The
// behaviour under test is which rows one SQL predicate keeps, and a fake would
// keep whichever rows the fake's author believed in.

// TestNoUnackedReminderIsMintedWhileTheCaseHoldsAnUpstreamResolve is the
// assertion. The SAME group, the SAME case and the SAME `alerts.state` are asked
// twice: once with no pending close, where the reminder is owed, and once with
// the receipt on the row, where it is not.
func TestNoUnackedReminderIsMintedWhileTheCaseHoldsAnUpstreamResolve(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	now := h.Now()

	// One generation, one member: an episode that has been unacknowledged for an
	// hour. The group has no other live member, so the group either qualifies on
	// this case alone or not at all.
	alertID := seedAlert(t, h, fx, "FlappingDiskPressure")
	caseID := seedCase(t, h, fx, alertID, 1, occSeed{
		state: "open", startedAt: now.Add(-time.Hour), ackState: "unacked",
	})

	repo := repository.NewReminderRepository(h.Pool)
	before := now.Add(-time.Minute)

	// ---- the control: nothing is pending, so the reminder is owed ------------
	owed, err := repo.ListUnackedGroups(h.Ctx, fx.scope, before, 10)
	require.NoError(t, err)
	require.Len(t, owed, 1,
		"an open, unacked, unsuppressed member older than the threshold IS a reminder candidate")
	require.Equal(t, fx.groupID, owed[0].GroupID)

	// ---- upstream resolves, and W holds the close ---------------------------
	//
	// This is exactly what the T5 deferral arm writes: both pending columns
	// together (case_pending_pair_ck), the `ended_at` the close will stamp taken
	// from the upstream claim and never from oto's clock. NOTHING ELSE MOVES —
	// the case is still `open` and still `unacked`, because it is.
	h.Exec(`UPDATE alert_cases
	           SET resolve_pending_at = $1, resolve_pending_end_at = $2
	         WHERE id = $3`,
		now.Add(20*time.Minute), now.Add(-10*time.Minute), caseID)

	// ⭐ AND THE ALERT IS STILL 'firing'. The deferral has From == To, so the
	// state column is unchanged and cannot distinguish "still burning" from
	// "resolved, close held". If this ever stops being true the predicate under
	// test becomes belt-and-braces rather than the only thing standing here, and
	// this line is what will say so.
	var state string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT state FROM alerts WHERE id = $1`, alertID).Scan(&state))
	require.Equal(t, "firing", state,
		"the deferral is a From == To transition; alerts.state stays firing for the whole window")

	// ---- the assertion ------------------------------------------------------
	held, err := repo.ListUnackedGroups(h.Ctx, fx.scope, before, 10)
	require.NoError(t, err)
	require.Empty(t, held,
		"a case holding an upstream resolve must mint no unacked reminder: "+
			"nagging about an alert upstream has already resolved is the noise W exists to remove")
}
