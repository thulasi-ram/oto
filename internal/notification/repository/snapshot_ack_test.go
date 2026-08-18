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

// ⭐⭐ THE CARD'S PER-MEMBER ACK COMES FROM THE MEMBER'S OWN EPISODE.
//
// A member IS a case: membership is `alert_cases.group_id`, so the row the
// card reads carries the receipt given for the very firing it is about. The read
// used to take `a.ack_state` off `alerts` instead — a projection of the CURRENT
// episode onto an entity that outlives every episode it has — and the two answers
// differ in exactly the situation that matters:
//
//	an alert fires, somebody acks it, it resolves, and it fires again.
//
// The new episode is unacked, and the card is about the new episode. But the
// projection lags a beat behind every writer of it, and between the resolution
// and the next transition it says `acked` about a firing that has ENDED — so a
// Slack card could arrive for a fresh firing already wearing somebody's receipt
// from a firing that was over. Nobody looks at an acked alert. That is the whole
// defect, and it is invisible to the compiler: the read was a string literal in
// SQL, with no import and no Go edge to break.
//
// ⛔ THIS IS AN INTEGRATION TEST AGAINST A REAL POSTGRES ON PURPOSE. There is no
// unit-testable seam here. The behaviour under test is which column a `SELECT`
// names, and a fake would name whichever one the fake's author believed in.

// ackWorld is one group generation holding two members, seeded so that the
// ALERT-scoped answer and the CASE-scoped answer disagree about both of them.
type ackWorld struct {
	fx fixture

	// carried is the alert whose ack belongs to a CLOSED episode. Its member row
	// points at a NEW, unacked episode. Alert-scoped: acked. Case-scoped: unacked.
	carried uuid.UUID
	// fresh is the alert acked ON the episode this card is about, while the row
	// on `alerts` never heard about it. Alert-scoped: unacked. Case-scoped: acked.
	fresh uuid.UUID
}

func newAckWorld(t *testing.T) ackWorld {
	t.Helper()

	fx := newFixture(t)
	h := fx.h
	now := h.Now()

	w := ackWorld{fx: fx}

	// ---- the alert whose receipt is for a firing that ended -----------------
	//
	// Episode 1 ran, was acknowledged, and closed. Episode 2 is the one the card
	// is about, and nobody has looked at it.
	w.carried = seedAlert(t, h, fx, "AckCarriedForward")
	closed := seedCase(t, h, fx, w.carried, 1, occSeed{
		state: "closed", startedAt: now.Add(-2 * time.Hour), endedAt: ptr(now.Add(-time.Hour)),
		ackState: "acked", ackedAt: ptr(now.Add(-90 * time.Minute)), ackedByLabel: ptr("Priya R."),
	})
	_ = closed
	// The successor at `seq` 2. A re-fire always opens a new episode, unacked
	// (ADR 0040 retired the road that carried an ack across one), which is exactly
	// the shape this file is about: the card must read THIS row's receipt.
	successor := seedCase(t, h, fx, w.carried, 2, occSeed{
		state: "open", startedAt: now.Add(-30 * time.Minute), ackState: "unacked",
	})
	_ = successor

	// ---- the alert acked on THIS episode ------------------------------------
	w.fresh = seedAlert(t, h, fx, "AckedOnThisEpisode")
	live := seedCase(t, h, fx, w.fresh, 1, occSeed{
		state: "open", startedAt: now.Add(-40 * time.Minute), ackState: "acked",
		ackedAt: ptr(now.Add(-10 * time.Minute)), ackedByLabel: ptr("Ada L."),
	})
	_ = live

	// ⭐ NOTHING JOINS ANYTHING. `seedCase` writes `group_id`, and since
	// migration 00051 that IS the membership — there is no `alert_group_members`
	// row to insert. Which also means `closed`, episode 1 of `carried`, is a member
	// of this generation too, and the card must still not read its ack: it ENDED,
	// so `memberAlertsSQL`'s `ended_at IS NULL` is what keeps it off the card. That
	// predicate is the one the old join table could not express, because nothing
	// ever wrote the `left_at` it was spelled against.

	// ⛔ THE CURRENT-CASE POINTERS ARE DELIBERATELY THE WRONG ONES for a
	// member read. `carried` points at its CLOSED, acked episode and `fresh`
	// points at nothing. A member read that reached the episode through
	// `alerts.current_case_id` — the obvious near-miss fix for the dropped
	// column — passes every other assertion in this file and fails these two.
	h.Exec(`UPDATE alerts SET current_case_id = $1 WHERE id = $2`, closed, w.carried)

	return w
}

// TestTheGroupCardReadsEachMembersAckFromThatMembersEpisode is the assertion.
func TestTheGroupCardReadsEachMembersAckFromThatMembersEpisode(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)
	h := w.fx.h

	snap, err := repository.NewSnapshotRepository(h.Pool, h.Clock).
		Snapshot(h.Ctx, w.fx.scope, domain.SnapshotQuery{GroupID: w.fx.groupID, MaxAlerts: 10})
	require.NoError(t, err)
	require.Len(t, snap.Alerts, 2, "both members must render; ack is a chip, not a filter")

	byID := map[uuid.UUID]domain.AlertFacts{}
	for _, a := range snap.Alerts {
		byID[a.ID] = a
	}

	require.Equal(t, "unacked", byID[w.carried].AckState,
		"a receipt given for an episode that has ENDED must not travel into the next one. "+
			"This member's closed episode was acknowledged and its live episode was not; "+
			"reading `acked` here is a fresh firing arriving pre-acknowledged, which is a "+
			"firing nobody will look at.")

	require.Equal(t, "acked", byID[w.fresh].AckState,
		"the receipt on the episode this card IS about must be shown. Ack did not move off "+
			"`alerts` to nowhere — it moved to the case, and the card has to read it there.")
}

// TestTheFocusAlertReadsItsAckFromItsCurrentEpisode is the other read that used
// to name `a.ack_state`. There is no member row for a focus — the focus is an
// alert, sometimes one that has already LEFT the group — so its case is the
// current case, which is the same episode `include=current_case`
// expands on the alert list.
func TestTheFocusAlertReadsItsAckFromItsCurrentEpisode(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)
	h := w.fx.h

	snap, err := repository.NewSnapshotRepository(h.Pool, h.Clock).
		Snapshot(h.Ctx, w.fx.scope, domain.SnapshotQuery{
			GroupID: w.fx.groupID, AlertID: &w.carried, MaxAlerts: 10,
		})
	require.NoError(t, err)
	require.NotNil(t, snap.Focus)
	require.Equal(t, "acked", snap.Focus.AckState,
		"the focus alert's current episode carries the receipt, so the card shows it")

	// An alert with NO episode at all reads `unacked` rather than empty: the card
	// renders the string, and the honest reading of "no receipt" is "unacked".
	orphan := seedAlert(t, h, w.fx, "NeverFired")
	snap, err = repository.NewSnapshotRepository(h.Pool, h.Clock).
		Snapshot(h.Ctx, w.fx.scope, domain.SnapshotQuery{
			GroupID: w.fx.groupID, AlertID: &orphan, MaxAlerts: 10,
		})
	require.NoError(t, err)
	require.NotNil(t, snap.Focus)
	require.Equal(t, "unacked", snap.Focus.AckState,
		"an alert with no episode has nobody's receipt on it; over-reporting unacked costs "+
			"a glance, under-reporting it costs the alert")
}

// TestNothingInTheSnapshotPathNamesAnAckColumnOnAlerts is the sequencing guard.
//
// ⛔ A CROSS-MODULE SQL READ HAS NO IMPORT AND NO COMPILER ERROR. `notification`
// reaching into `alerts` is a string literal; if the column were dropped while a
// literal still named it, nothing in the build would notice and the failure would
// arrive at runtime, on the card path, as a 42703 nobody sees until a page does
// not go out. The reads were redirected BEFORE the drop, and this asserts the
// redirect held by asking the database to plan every one of them.
func TestNothingInTheSnapshotPathNamesAnAckColumnOnAlerts(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)
	h := w.fx.h

	// The whole read, planned and executed against the real schema. A statement
	// still naming `a.ack_state` cannot parse, so this is the failure mode the
	// compiler could not give us.
	_, err := repository.NewSnapshotRepository(h.Pool, h.Clock).
		Snapshot(h.Ctx, w.fx.scope, domain.SnapshotQuery{
			GroupID: w.fx.groupID, AlertID: &w.fresh, MaxAlerts: 10,
		})
	require.NoError(t, err, "the snapshot read names a column `alerts` no longer has")

	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'alerts'
		   AND column_name LIKE '%ack%'`).Scan(&n))
	require.Zero(t, n, "`alerts` has an acknowledgement column again")
}

// ---------------------------------------------------------------- the seeding

// occSeed is one episode, written directly because these tests are about which
// row a read reaches, not about how an episode comes to exist.
type occSeed struct {
	// state is `alert_cases.state` verbatim, so it is `open` or `closed` and
	// nothing else (ADR 0040). The four §B.2 names describe the ALERT; an episode
	// that ended says WHY in `resolve_reason`, which `seedCase` derives below.
	state        string
	startedAt    time.Time
	endedAt      *time.Time
	ackState     string
	ackedAt      *time.Time
	ackedByLabel *string
}

func seedAlert(t *testing.T, h *harness.H, fx fixture, name string) uuid.UUID {
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
		alertID, fx.scope.OrgID(), "ak_"+strings32(alertID), fingerprint16(alertID), name, now)
	return alertID
}

func seedCase(
	t *testing.T, h *harness.H, fx fixture, alertID uuid.UUID, seq int, s occSeed,
) uuid.UUID {
	t.Helper()

	caseID := id.New()
	var resolveReason *string
	if s.endedAt != nil {
		r := "upstream"
		resolveReason = &r
	}
	h.Exec(`INSERT INTO alert_cases
	          (id, org_id, alert_id, group_id, seq, state, started_at, ended_at,
	           last_observed_at, source_starts_at, resolve_reason,
	           ack_state, acked_at, acked_by_label)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $7, $9, $10, $11, $12)`,
		caseID, fx.scope.OrgID(), alertID, fx.groupID, seq, s.state,
		s.startedAt, s.endedAt, resolveReason, s.ackState, s.ackedAt, s.ackedByLabel)
	return caseID
}

func ptr[T any](v T) *T { return &v }

// strings32 and fingerprint16 satisfy `alerts_key_ck` and `alerts_srcfp_ck`,
// which are shape checks: these rows are never read through the domain
// constructors, so the only thing they owe is a legal shape.
func strings32(u uuid.UUID) string {
	const base32hex = "0123456789abcdefghijklmnopqrstuv"
	out := make([]byte, 26)
	b := u[:]
	for i := range out {
		out[i] = base32hex[int(b[i%len(b)])%len(base32hex)]
	}
	return string(out)
}

func fingerprint16(u uuid.UUID) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	b := u[:]
	for i := range out {
		out[i] = hex[int(b[i%len(b)])%len(hex)]
	}
	return string(out)
}
