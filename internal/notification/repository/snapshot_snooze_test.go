package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ WHETHER OTO SPEAKS IS DECIDED AGAINST `alert_snoozes`, NOT AGAINST A
// TIMESTAMP SOMEBODY REMEMBERED TO COPY.
//
// The suppression path reads this snapshot and nothing else: `AllMembersSnoozed`
// gates a group card and `FocusSnoozed` gates a fact about one alert
// (`notification/service/notify.go`). Both used to be filled from
// `alerts.snoozed_until` — a bare timestamp maintained by three write paths in
// another module, reached as a string literal in SQL with no import and no
// compiler error to break — while the authoritative row, which knows who
// silenced it, why, and how it ended, was never consulted.
//
// The rows where the two answers differ are the ones seeded below:
//
//	ENDED     `ended_at` is set. The quiet period is over. The mirror agreed only
//	          once somebody remembered to clear it, and three separate
//	          transactions had to remember.
//	UNSWEPT   `ended_at` is still NULL and `snoozed_until` has passed. The
//	          60-second expiry job has not been round yet, and oto must ALREADY be
//	          speaking again — a card held back here is a card held back for a
//	          reason that stopped applying.
//
// ⛔ INTEGRATION AGAINST A REAL POSTGRES, ON PURPOSE. There is no unit-testable
// seam: the behaviour under test is which table a `SELECT` names and which rows
// its predicate admits, and a fake would name whichever table its author
// believed in.

// quietWorld is one group generation holding three members, one per way an alert
// can relate to a snooze.
type quietWorld struct {
	fx fixture

	// held is the member oto is genuinely quiet about.
	held uuid.UUID
	// woken had its snooze ended early: `ended_at` is set.
	woken uuid.UUID
	// unswept holds a live row whose clock ran out ten minutes ago.
	unswept uuid.UUID
}

func newQuietWorld(t *testing.T) quietWorld {
	t.Helper()

	fx := newFixture(t)
	h := fx.h
	now := h.Now()

	w := quietWorld{fx: fx}

	w.held = seedQuietAlert(t, h, fx, "HeldRightNow")
	w.woken = seedQuietAlert(t, h, fx, "WokenEarly")
	w.unswept = seedQuietAlert(t, h, fx, "ClockRanOutUnswept")

	// The episode IS the membership since 00051: `seedQuietCase` writes
	// `group_id`, and there is no second row to insert.
	for _, alertID := range []uuid.UUID{w.held, w.woken, w.unswept} {
		seedQuietCase(t, h, fx, alertID, now.Add(-30*time.Minute))
	}

	seedQuietSnooze(t, h, fx, w.held, now.Add(-time.Hour), now.Add(3*time.Hour), nil)
	seedQuietSnooze(t, h, fx, w.woken, now.Add(-4*time.Hour), now.Add(2*time.Hour),
		timePtr(now.Add(-30*time.Minute)))
	seedQuietSnooze(t, h, fx, w.unswept, now.Add(-2*time.Hour), now.Add(-10*time.Minute), nil)

	return w
}

func (w quietWorld) snapshot(t *testing.T, q domain.SnapshotQuery) domain.Snapshot {
	t.Helper()

	q.GroupID = w.fx.groupID
	if q.MaxAlerts == 0 {
		q.MaxAlerts = 10
	}
	snap, err := repository.NewSnapshotRepository(w.fx.h.Pool, w.fx.h.Clock).
		Snapshot(w.fx.h.Ctx, w.fx.scope, q)
	require.NoError(t, err,
		"the snapshot read names a column `alerts` no longer has — a cross-module SQL read "+
			"has no import to break, so this is the failure the compiler could not give us")
	return snap
}

// TestTheCardsSnoozeFactsComeFromTheAuthoritativeRow is the assertion the ticket
// is named for.
func TestTheCardsSnoozeFactsComeFromTheAuthoritativeRow(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	snap := w.snapshot(t, domain.SnapshotQuery{})

	require.Len(t, snap.Alerts, 3, "every member renders; a snooze is a chip, not a filter")
	require.Equal(t, 3, snap.MemberCount)

	require.Equal(t, 1, snap.SnoozedMemberCount,
		"exactly one member is genuinely quiet. Counting the ended snooze or the unswept one "+
			"would suppress a card for a quiet period that has already finished — an alert "+
			"nobody is told about for a reason that stopped applying.")

	require.Contains(t, snap.SnoozedAlerts, w.held)
	require.NotContains(t, snap.SnoozedAlerts, w.woken,
		"an ended snooze is over; `ended_at` is the row saying so")
	require.NotContains(t, snap.SnoozedAlerts, w.unswept,
		"a live row whose clock has run out is over too. The expiry sweep runs every 60 "+
			"seconds, so for up to a minute `ended_at IS NULL` and the quiet period is "+
			"finished — the read has to ask the clock, not just the column.")
}

// TestTheGroupIsSuppressedOnlyWhenEveryMemberIsGenuinelyQuiet is the decision the
// snapshot exists to feed.
//
// ⛔ THE CONSERVATIVE DIRECTION IS THE POINT. A group with ONE awake member is
// not snoozed: silencing the whole card because most of it is quiet hides the one
// alert nobody asked to be quiet about.
func TestTheGroupIsSuppressedOnlyWhenEveryMemberIsGenuinelyQuiet(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	require.False(t, w.snapshot(t, domain.SnapshotQuery{}).AllMembersSnoozed(),
		"two of three members are awake, so the card must still go out")

	// Give the other two live snoozes and the group goes quiet — the same read,
	// the same table, a different set of rows.
	h := w.fx.h
	now := h.Now()
	h.Exec(`UPDATE alert_snoozes SET ended_at = NULL, ended_reason = NULL,
	          snoozed_at = $2, snoozed_until = $3
	        WHERE alert_id IN ($4, $5) AND org_id = $1`,
		w.fx.scope.OrgID(), now.Add(-time.Hour), now.Add(3*time.Hour), w.woken, w.unswept)

	snap := w.snapshot(t, domain.SnapshotQuery{})
	require.Equal(t, snap.MemberCount, snap.SnoozedMemberCount)
	require.True(t, snap.AllMembersSnoozed())
}

// TestTheFocusSnoozeIsReadFromItsOwnRow covers the other half of the suppression
// decision: a fact about ONE alert is decided by that alert's own snooze (§B.8.1),
// never by the group's.
func TestTheFocusSnoozeIsReadFromItsOwnRow(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	now := w.fx.h.Now()

	held := w.snapshot(t, domain.SnapshotQuery{AlertID: &w.held})
	require.NotNil(t, held.Focus)
	require.True(t, held.FocusSnoozed(held.TakenAt))
	require.NotNil(t, held.Focus.SnoozedUntil)
	require.WithinDuration(t, now.Add(3*time.Hour), *held.Focus.SnoozedUntil, time.Second,
		"the wake-up time comes off the snooze row, which is the only thing that has one")

	woken := w.snapshot(t, domain.SnapshotQuery{AlertID: &w.woken})
	require.NotNil(t, woken.Focus)
	require.False(t, woken.FocusSnoozed(woken.TakenAt),
		"an ended snooze suppresses nothing; oto is speaking about this alert again")

	// ⚠️ THIS IS THE CASE `readFocus` USED TO GET WRONG. It wrote its map entry
	// with no `After(now)` guard while `readMembers` had one, so an unswept snooze
	// made the focus look quiet on one path and awake on the other. The two are
	// the same question and now give the same answer.
	unswept := w.snapshot(t, domain.SnapshotQuery{AlertID: &w.unswept})
	require.NotNil(t, unswept.Focus)
	require.False(t, unswept.FocusSnoozed(unswept.TakenAt),
		"the clock has run out; the sweep is a lag in the bookkeeping, not in the promise")
	require.NotContains(t, unswept.SnoozedAlerts, w.unswept,
		"the focus read applies the same clock guard the member read does")
}

// TestAnAlertThatWasNeverSnoozedIsSimplyAwake asserts the LEFT-ness of the join:
// the overwhelmingly common case is an alert with no snooze row at all, and it
// must still be a member of the card.
func TestAnAlertThatWasNeverSnoozedIsSimplyAwake(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	h := w.fx.h
	now := h.Now()

	plain := seedQuietAlert(t, h, w.fx, "NeverQuietened")
	seedQuietCase(t, h, w.fx, plain, now.Add(-20*time.Minute))

	snap := w.snapshot(t, domain.SnapshotQuery{AlertID: &plain})
	require.Len(t, snap.Alerts, 4,
		"an INNER join here would drop every alert nobody has ever snoozed, which is nearly "+
			"all of them — the members would silently become the snoozed members")
	require.Equal(t, 4, snap.MemberCount)
	require.NotNil(t, snap.Focus)
	require.Nil(t, snap.Focus.SnoozedUntil)
	require.False(t, snap.Focus.Snoozed(snap.TakenAt))
}

// TestTheSnapshotReadsNoSnoozeColumnOnAlerts is the sequencing guard. Every
// statement above was planned against the real schema, which is what proves the
// redirect landed BEFORE the drop; this states the other half.
func TestTheSnapshotReadsNoSnoozeColumnOnAlerts(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)

	var n int
	require.NoError(t, w.fx.h.Pool.QueryRow(w.fx.h.Ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'alerts'
		   AND column_name LIKE '%snooz%'`).Scan(&n))
	require.Zero(t, n, "`alerts` has a snooze column again; the row is the record")
}

// ---------------------------------------------------------------- the seeding
//
// These write rows directly, because what is under test is which row a read
// reaches and not how a row comes to exist. The names are this file's own so it
// stays compilable independently of its neighbours.

func seedQuietAlert(t *testing.T, h *harness.H, fx fixture, name string) uuid.UUID {
	t.Helper()

	alertID := id.New()
	now := h.Now()
	h.Exec(`INSERT INTO alerts
	          (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
	           cluster_key, labels, state, first_seen_at, last_seen_at, last_state_change_at,
	           total_cases)
	        SELECT $1, $2, c.id, $3, $4, $5, 'critical', c.cluster_key,
	               jsonb_build_object('alertname', $5::text), 'firing', $6, $6, $6, 1
	          FROM clusters c WHERE c.org_id = $2 LIMIT 1`,
		alertID, fx.scope.OrgID(), "ak_"+base32Of(alertID), hexOf(alertID), name, now)
	return alertID
}

func seedQuietCase(
	t *testing.T, h *harness.H, fx fixture, alertID uuid.UUID, startedAt time.Time,
) uuid.UUID {
	t.Helper()

	caseID := id.New()
	h.Exec(`INSERT INTO alert_cases
	          (id, org_id, alert_id, group_id, seq, state, started_at,
	           last_observed_at, source_starts_at, ack_state)
	        VALUES ($1, $2, $3, $4, 1, 'open', $5, $5, $5, 'unacked')`,
		caseID, fx.scope.OrgID(), alertID, fx.groupID, startedAt)
	return caseID
}

// seedQuietSnooze writes one `alert_snoozes` row. `endedAt` nil is a row still
// open — which is NOT the same as a quiet period still in force, and telling
// those two apart is most of what this file is about.
func seedQuietSnooze(
	t *testing.T, h *harness.H, fx fixture, alertID uuid.UUID,
	at, until time.Time, endedAt *time.Time,
) {
	t.Helper()

	var reason *string
	if endedAt != nil {
		r := "manual"
		reason = &r
	}
	h.Exec(`INSERT INTO alert_snoozes
	          (id, org_id, alert_id, alert_key, snoozed_at, snoozed_until,
	           snoozed_by_label, note, ended_at, ended_reason)
	        SELECT $1, $2, a.id, a.alert_key, $4, $5, 'Ada L.',
	               'deploy window, expected until 17:00', $6, $7
	          FROM alerts a WHERE a.id = $3 AND a.org_id = $2`,
		id.New(), fx.scope.OrgID(), alertID, at, until, endedAt, reason)
}

func timePtr(v time.Time) *time.Time { return &v }

// base32Of and hexOf satisfy `alerts_key_ck` and `alerts_srcfp_ck`, which are
// shape checks: these rows are never read through the domain constructors, so
// the only thing they owe is a legal shape.
func base32Of(u uuid.UUID) string {
	const base32hex = "0123456789abcdefghijklmnopqrstuv"
	out := make([]byte, 26)
	b := u[:]
	for i := range out {
		out[i] = base32hex[int(b[i%len(b)])%len(base32hex)]
	}
	return string(out)
}

func hexOf(u uuid.UUID) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	b := u[:]
	for i := range out {
		out[i] = hex[int(b[i%len(b)])%len(hex)]
	}
	return string(out)
}
