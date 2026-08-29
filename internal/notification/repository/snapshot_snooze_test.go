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
// gates a card whose query named no focus and `FocusSnoozed` gates a fact about one
// alert (`notification/service/notify.go`). Both used to be filled from
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
// ⛔ THE CONSERVATIVE DIRECTION MOVED ONE LEVEL DOWN, IT DID NOT LEAVE
// (git-bug `7570090`). A conversation holds exactly ONE Case and a Case has one
// Alert, so there is no longer a group with one awake member for a majority-quiet
// card to hide. What is left of the same conservatism is WHICH ROWS COUNT: an ended
// snooze and an unswept one silence nothing, and a terminal Case is never quiet at
// all — because the alert nobody asked to be quiet about is now the alert this card
// IS about, and getting it wrong costs the whole alert rather than one line of it.
//
// ⛔ INTEGRATION AGAINST A REAL POSTGRES, ON PURPOSE. There is no unit-testable
// seam: the behaviour under test is which table a `SELECT` names and which rows
// its predicate admits, and a fake would name whichever table its author
// believed in.

// quietWorld is three conversations, one per way an alert can relate to a snooze.
type quietWorld struct {
	fx snapFixture

	// held is the alert oto is genuinely quiet about.
	held uuid.UUID
	// woken had its snooze ended early: `ended_at` is set.
	woken uuid.UUID
	// unswept holds a live row whose clock ran out ten minutes ago.
	unswept uuid.UUID

	// caseOf names each alert's one Case, which is also its conversation.
	caseOf map[uuid.UUID]uuid.UUID
}

func newQuietWorld(t *testing.T) quietWorld {
	t.Helper()

	fx := newSnapFixture(t)
	h := fx.h
	now := h.Now()

	w := quietWorld{fx: fx, caseOf: map[uuid.UUID]uuid.UUID{}}

	w.held = seedQuietAlert(t, h, fx, "HeldRightNow")
	w.woken = seedQuietAlert(t, h, fx, "WokenEarly")
	w.unswept = seedQuietAlert(t, h, fx, "ClockRanOutUnswept")

	// ⭐ THREE CONVERSATIONS AND NOT ONE. These used to be three members of one
	// generation, which is what made "every member quiet" a question worth asking of
	// a set; a Case has exactly one Alert, so each of them is its own card and the
	// question is asked three times.
	for _, alertID := range []uuid.UUID{w.held, w.woken, w.unswept} {
		w.caseOf[alertID] = seedQuietCase(t, h, fx, alertID, now.Add(-30*time.Minute))
	}

	seedQuietSnooze(t, h, fx, w.held, now.Add(-time.Hour), now.Add(3*time.Hour), nil)
	seedQuietSnooze(t, h, fx, w.woken, now.Add(-4*time.Hour), now.Add(2*time.Hour),
		timePtr(now.Add(-30*time.Minute)))
	seedQuietSnooze(t, h, fx, w.unswept, now.Add(-2*time.Hour), now.Add(-10*time.Minute), nil)

	return w
}

// snapshot reads the conversation of one alert. The Case is the conversation, so
// naming the alert names the card.
func (w quietWorld) snapshot(t *testing.T, alertID uuid.UUID, q domain.SnapshotQuery) domain.Snapshot {
	t.Helper()

	q.CaseID = w.caseOf[alertID]
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

	held := w.snapshot(t, w.held, domain.SnapshotQuery{})
	require.Len(t, held.Alerts, 1, "the one Alert renders; a snooze is a chip, not a filter")
	require.Equal(t, 1, held.MemberCount)
	require.Equal(t, 1, held.SnoozedMemberCount)
	require.Contains(t, held.SnoozedAlerts, w.held)
	require.NotNil(t, held.Alerts[0].SnoozedUntil,
		"the wake-up time comes off the snooze row, which is the only thing that has one")

	woken := w.snapshot(t, w.woken, domain.SnapshotQuery{})
	require.Len(t, woken.Alerts, 1)
	require.Equal(t, 1, woken.MemberCount)
	require.Zero(t, woken.SnoozedMemberCount,
		"an ended snooze is over; `ended_at` is the row saying so. Counting it would "+
			"suppress a card for a quiet period that has already finished — an alert nobody "+
			"is told about for a reason that stopped applying.")
	require.NotContains(t, woken.SnoozedAlerts, w.woken)

	unswept := w.snapshot(t, w.unswept, domain.SnapshotQuery{})
	require.Len(t, unswept.Alerts, 1)
	require.Zero(t, unswept.SnoozedMemberCount,
		"a live row whose clock has run out is over too. The expiry sweep runs every 60 "+
			"seconds, so for up to a minute `ended_at IS NULL` and the quiet period is "+
			"finished — the read has to ask the clock, not just the column.")
	require.NotContains(t, unswept.SnoozedAlerts, w.unswept)
}

// TestTheCardIsSuppressedOnlyWhenItsOneAlertIsGenuinelyQuiet is the decision the
// snapshot exists to feed.
func TestTheCardIsSuppressedOnlyWhenItsOneAlertIsGenuinelyQuiet(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)

	require.True(t, w.snapshot(t, w.held, domain.SnapshotQuery{}).AllMembersSnoozed(),
		"the one alert this card is about is genuinely quiet, so oto says nothing")
	require.False(t, w.snapshot(t, w.woken, domain.SnapshotQuery{}).AllMembersSnoozed(),
		"its snooze was ended early; oto is speaking about this alert again")
	require.False(t, w.snapshot(t, w.unswept, domain.SnapshotQuery{}).AllMembersSnoozed(),
		"the clock has run out; the sweep is a lag in the bookkeeping, not in the promise")
}

// TestTheTwoQuietTestsAgreeOnOneAlert is a NEW invariant, and it is one the
// collapse creates rather than one it inherits.
//
// ⭐ `notify.go` CHOOSES BETWEEN THEM ON WHETHER THE QUERY NAMED A FOCUS —
// `FocusSnoozed` when it did, `AllMembersSnoozed` when it did not — and while a
// group had many members those two could legitimately disagree. Over one Alert they
// are the same question asked twice, so a divergence would mean the SAME fact is
// suppressed or sent depending only on whether the caller happened to pass an
// `AlertID`. That is exactly the class of defect the missing `After(now)` guard on
// `readFocus` used to produce, and this pins it shut.
func TestTheTwoQuietTestsAgreeOnOneAlert(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)

	for _, alertID := range []uuid.UUID{w.held, w.woken, w.unswept} {
		withFocus := w.snapshot(t, alertID, domain.SnapshotQuery{AlertID: &alertID})
		without := w.snapshot(t, alertID, domain.SnapshotQuery{})
		require.NotNil(t, withFocus.Focus)
		require.Equal(t, without.AllMembersSnoozed(), withFocus.FocusSnoozed(withFocus.TakenAt),
			"quiet must not depend on whether the caller named the alert its Case is about")
	}
}

// TestTheFocusSnoozeIsReadFromItsOwnRow covers the other half of the suppression
// decision: a fact about ONE alert is decided by that alert's own snooze (§B.8.1).
func TestTheFocusSnoozeIsReadFromItsOwnRow(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	now := w.fx.h.Now()

	held := w.snapshot(t, w.held, domain.SnapshotQuery{AlertID: &w.held})
	require.NotNil(t, held.Focus)
	require.True(t, held.FocusSnoozed(held.TakenAt))
	require.NotNil(t, held.Focus.SnoozedUntil)
	require.WithinDuration(t, now.Add(3*time.Hour), *held.Focus.SnoozedUntil, time.Second,
		"the wake-up time comes off the snooze row, which is the only thing that has one")

	woken := w.snapshot(t, w.woken, domain.SnapshotQuery{AlertID: &w.woken})
	require.NotNil(t, woken.Focus)
	require.False(t, woken.FocusSnoozed(woken.TakenAt),
		"an ended snooze suppresses nothing; oto is speaking about this alert again")

	// ⚠️ THIS IS THE CASE `readFocus` USED TO GET WRONG. It wrote its map entry
	// with no `After(now)` guard while the member read had one, so an unswept snooze
	// made the focus look quiet on one path and awake on the other. The two are
	// the same question and now give the same answer.
	unswept := w.snapshot(t, w.unswept, domain.SnapshotQuery{AlertID: &w.unswept})
	require.NotNil(t, unswept.Focus)
	require.False(t, unswept.FocusSnoozed(unswept.TakenAt),
		"the clock has run out; the sweep is a lag in the bookkeeping, not in the promise")
	require.NotContains(t, unswept.SnoozedAlerts, w.unswept,
		"the focus read applies the same clock guard the conversation read does")
}

// TestTheFocusMayBeAnAlertThisCaseIsNotAbout is why `readFocus` is still its own
// read after the collapse made it derivable.
//
// ⛔ A CASE HAS EXACTLY ONE ALERT, SO THE OBVIOUS SIMPLIFICATION IS TO FILL THE
// FOCUS FROM IT. This is the query that would then be answered with the wrong
// alert: the policy preview takes one Case and, optionally, one alert, and a card
// that quietly rendered the Case's own alert under the heading of the one it was
// asked about would be a lie its reader cannot see.
func TestTheFocusMayBeAnAlertThisCaseIsNotAbout(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)

	snap := w.snapshot(t, w.woken, domain.SnapshotQuery{AlertID: &w.held})
	require.NotNil(t, snap.Focus)
	require.Equal(t, w.held, snap.Focus.ID,
		"the focus is the alert the caller named, not the Case's own")
	require.Len(t, snap.Alerts, 1)
	require.Equal(t, w.woken, snap.Alerts[0].ID,
		"and the member list is still the conversation's, which is a different alert")
	require.True(t, snap.FocusSnoozed(snap.TakenAt),
		"the fact is about `held`, which is quiet, so §B.8.1 says oto stays quiet")
	require.False(t, snap.AllMembersSnoozed(),
		"while the conversation's own alert is awake — the two disagree here precisely "+
			"because the query asked about two different alerts")
}

// TestAnAlertThatWasNeverSnoozedIsSimplyAwake asserts the LEFT-ness of the join:
// the overwhelmingly common case is an alert with no snooze row at all, and it
// must still appear on its own card.
func TestAnAlertThatWasNeverSnoozedIsSimplyAwake(t *testing.T) {
	t.Parallel()

	w := newQuietWorld(t)
	h := w.fx.h
	now := h.Now()

	plain := seedQuietAlert(t, h, w.fx, "NeverQuietened")
	w.caseOf[plain] = seedQuietCase(t, h, w.fx, plain, now.Add(-20*time.Minute))

	snap := w.snapshot(t, plain, domain.SnapshotQuery{AlertID: &plain})
	require.Len(t, snap.Alerts, 1,
		"an INNER join here would drop every alert nobody has ever snoozed, which is nearly "+
			"all of them — every card would render with no instance on it at all")
	require.Equal(t, 1, snap.MemberCount)
	require.Zero(t, snap.SnoozedMemberCount)
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

func seedQuietAlert(t *testing.T, h *harness.H, fx snapFixture, name string) uuid.UUID {
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

// seedQuietCase writes one episode and NAMES NO GROUP: `alert_cases.group_id` is
// dropped (git-bug `7570090`, migration `00069`), so the Case IS the conversation.
func seedQuietCase(
	t *testing.T, h *harness.H, fx snapFixture, alertID uuid.UUID, startedAt time.Time,
) uuid.UUID {
	t.Helper()

	caseID := id.New()
	h.Exec(`WITH allocated AS (
	          INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
	          ON CONFLICT (org_id) DO UPDATE
	                  SET next_number = org_case_numbers.next_number + 1
	            RETURNING next_number - 1 AS number
	        )
	        INSERT INTO alert_cases
	          (id, org_id, alert_id, seq, number, state, started_at,
	           last_observed_at, source_starts_at, ack_state)
	        SELECT $1, $2, $3, 1, (SELECT number FROM allocated),
	               'open', $4, $4, $4, 'unacked'`,
		caseID, fx.scope.OrgID(), alertID, startedAt)
	return caseID
}

// seedQuietSnooze writes one `alert_snoozes` row. `endedAt` nil is a row still
// open — which is NOT the same as a quiet period still in force, and telling
// those two apart is most of what this file is about.
func seedQuietSnooze(
	t *testing.T, h *harness.H, fx snapFixture, alertID uuid.UUID,
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
