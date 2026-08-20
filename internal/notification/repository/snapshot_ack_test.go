package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE CARD'S ACK COMES FROM THE CASE THE CARD IS ABOUT.
//
// A conversation holds exactly ONE Case (git-bug `7570090`), so the row the card
// reads IS the firing episode being rendered, and it carries the receipt given for
// that very firing. The read used to take `a.ack_state` off `alerts` instead — a
// projection of the CURRENT episode onto an entity that outlives every episode it
// has — and the two answers differ in exactly the situation that matters:
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
// ⛔ THE NEAR-MISS FIX IS WHAT THIS FILE ACTUALLY GUARDS, and deleting
// `alert_groups` did not retire it — it moved it one hop. There is no group row to
// read an ack from any more, so the two candidate answers are now "the Case named
// by the query" and "the case `alerts.current_case_id` points at", and those are
// DIFFERENT episodes whenever an alert has fired more than once. The world below
// seeds them to disagree about both alerts.
//
// ⛔ THIS IS AN INTEGRATION TEST AGAINST A REAL POSTGRES ON PURPOSE. There is no
// unit-testable seam here. The behaviour under test is which column a `SELECT`
// names, and a fake would name whichever one the fake's author believed in.

// ackWorld is two conversations, seeded so that the ALERT-scoped answer and the
// CASE-scoped answer disagree about both of them.
type ackWorld struct {
	fx snapFixture

	// carried is the alert whose ack belongs to a CLOSED episode, while the episode
	// this card is about is new and unacked. `alerts.current_case_id` points at the
	// closed one. Alert-scoped: acked. Case-scoped: unacked.
	carried uuid.UUID
	// carriedLive is `carried`'s live episode — the Case the conversation is about.
	carriedLive uuid.UUID
	// carriedClosed is the acked episode that has ENDED.
	carriedClosed uuid.UUID

	// fresh is the alert acked ON the episode this card is about, while `alerts`
	// never heard about it. Alert-scoped: unacked. Case-scoped: acked.
	fresh uuid.UUID
	// freshLive is `fresh`'s one episode.
	freshLive uuid.UUID
}

func newAckWorld(t *testing.T) ackWorld {
	t.Helper()

	fx := newSnapFixture(t)
	h := fx.h
	now := h.Now()

	w := ackWorld{fx: fx}

	// ---- the alert whose receipt is for a firing that ended -----------------
	//
	// Episode 1 ran, was acknowledged, and closed. Episode 2 is the one the card
	// is about, and nobody has looked at it.
	w.carried = seedAlert(t, h, fx, "AckCarriedForward")
	w.carriedClosed = seedCase(t, h, fx, w.carried, 1, occSeed{
		state: "closed", startedAt: now.Add(-2 * time.Hour), endedAt: ptr(now.Add(-time.Hour)),
		ackState: "acked", ackedAt: ptr(now.Add(-90 * time.Minute)), ackedByLabel: ptr("Priya R."),
	})
	// The successor at `seq` 2. A re-fire always opens a new episode, unacked
	// (ADR 0040 retired the road that carried an ack across one), which is exactly
	// the shape this file is about: the card must read THIS row's receipt.
	w.carriedLive = seedCase(t, h, fx, w.carried, 2, occSeed{
		state: "open", startedAt: now.Add(-30 * time.Minute), ackState: "unacked",
	})

	// ---- the alert acked on THIS episode ------------------------------------
	w.fresh = seedAlert(t, h, fx, "AckedOnThisEpisode")
	w.freshLive = seedCase(t, h, fx, w.fresh, 1, occSeed{
		state: "open", startedAt: now.Add(-40 * time.Minute), ackState: "acked",
		ackedAt: ptr(now.Add(-10 * time.Minute)), ackedByLabel: ptr("Ada L."),
	})

	// ⭐ NOTHING JOINS ANYTHING, AND SINCE `7570090` THERE IS NOTHING TO JOIN TO.
	// `seedCase` writes no `group_id` — migration 00069 dropped the column — so a
	// Case's conversation is the Case, and `carried`'s two episodes are two
	// conversations rather than two members of one. Which is also why the closed
	// episode can no longer leak onto its successor's card by being a sibling: it
	// leaks, if anything leaks, through `current_case_id` below.

	// ⛔ THE CURRENT-CASE POINTER IS DELIBERATELY THE WRONG ONE for the card's own
	// read. `carried` points at its CLOSED, acked episode and `fresh` points at
	// nothing. A read that reached the episode through `alerts.current_case_id` —
	// the obvious near-miss fix for the dropped column — passes every other
	// assertion in this file and fails these two.
	h.Exec(`UPDATE alerts SET current_case_id = $1 WHERE id = $2`, w.carriedClosed, w.carried)

	return w
}

func (w ackWorld) snapshot(t *testing.T, q domain.SnapshotQuery) domain.Snapshot {
	t.Helper()

	snap, err := repository.NewSnapshotRepository(w.fx.h.Pool, w.fx.h.Clock).
		Snapshot(w.fx.h.Ctx, w.fx.scope, q)
	require.NoError(t, err,
		"the snapshot read names a column `alerts` no longer has — a cross-module SQL read "+
			"has no import to break, so this is the failure the compiler could not give us")
	return snap
}

// TestTheCardReadsItsAckFromTheCaseItIsAbout is the assertion.
func TestTheCardReadsItsAckFromTheCaseItIsAbout(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)

	live := w.snapshot(t, domain.SnapshotQuery{CaseID: w.carriedLive})
	require.Len(t, live.Alerts, 1,
		"a Case has exactly one Alert, and it renders while the episode is live")
	require.Equal(t, "unacked", live.Alerts[0].AckState,
		"a receipt given for an episode that has ENDED must not travel into the next one. "+
			"This alert's closed episode was acknowledged, its live one was not, and "+
			"`current_case_id` still points at the closed one — reading `acked` here is a "+
			"fresh firing arriving pre-acknowledged, which is a firing nobody will look at.")
	require.NotNil(t, live.Case)
	require.Equal(t, "unacked", live.Case.AckState,
		"`Case` and `Alerts[0]` are two reads of the same episode and cannot disagree")

	fresh := w.snapshot(t, domain.SnapshotQuery{CaseID: w.freshLive})
	require.Len(t, fresh.Alerts, 1)
	require.Equal(t, "acked", fresh.Alerts[0].AckState,
		"the receipt on the episode this card IS about must be shown. Ack did not move off "+
			"`alerts` to nowhere — it moved to the case, and the card has to read it there. "+
			"This alert's `current_case_id` is NULL, so a read through it would say `unacked`.")
	require.Equal(t, 1, fresh.Group.AckedCount,
		"the card's acked count is projected over the one Alert, from the same row")
}

// TestTheCardsCountsAreProjectedOverItsOneAlert pins the six counts that used to
// be stored columns on `alert_groups`.
//
// ⭐ THEY CANNOT DISAGREE WITH THE CARD ANY MORE, and that is the whole reason
// they are derived rather than read: `GroupFacts.AllResolved` has always claimed
// the counts are a projection of what oto recorded, and while they were six columns
// maintained by a writer in another module that claim was a promise. It is now
// arithmetic over the row that produced `Alerts[0]`.
func TestTheCardsCountsAreProjectedOverItsOneAlert(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)

	live := w.snapshot(t, domain.SnapshotQuery{CaseID: w.carriedLive})
	require.Equal(t, 1, live.Group.TotalCount)
	require.Equal(t, 1, live.Group.FiringCount)
	require.Zero(t, live.Group.ResolvedCount)
	require.Zero(t, live.Group.AckedCount)
	require.False(t, live.Group.AllResolved())
	require.True(t, live.Group.Open())

	closed := w.snapshot(t, domain.SnapshotQuery{CaseID: w.carriedClosed})
	require.Equal(t, 1, closed.Group.TotalCount)
	require.Equal(t, 1, closed.Group.ResolvedCount)
	require.Zero(t, closed.Group.FiringCount)
	require.True(t, closed.Group.AllResolved(),
		"one member, resolved, is every member resolved — which is what earns the thread "+
			"reply §H.6 gives `all_resolved`")
	require.False(t, closed.Group.Open())
	require.Empty(t, closed.Alerts,
		"the member list is what is CURRENTLY firing, so a terminal card lists nothing — "+
			"unchanged from the generation card, and load-bearing: a `MemberCount` of 1 here "+
			"would let a live snooze suppress the resolution notification")
	require.Zero(t, closed.MemberCount)
	require.False(t, closed.AllMembersSnoozed())
}

// TestTheCardLevelFactsComeFromTheCaseAndItsAlert is the succession `alert_groups`
// left behind: the title, the severity and the cluster now have no group row to
// come from.
func TestTheCardLevelFactsComeFromTheCaseAndItsAlert(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)
	snap := w.snapshot(t, domain.SnapshotQuery{CaseID: w.freshLive})

	require.Equal(t, w.freshLive, snap.Group.ID,
		"the conversation IS the Case, so its id is the Case's")
	require.Equal(t, "AckedOnThisEpisode", snap.Group.Title,
		"the alert's own name is the card title now that no route supplies one")
	require.Equal(t, "critical", snap.Group.Severity)
	require.NotEmpty(t, snap.Group.ClusterKey,
		"`alert_groups` had a cluster_id and no cluster_key, so this field was never once "+
			"filled; the webhook renderer's `on <cluster>` clause can finally render")
	require.Equal(t, "AckedOnThisEpisode", snap.Group.GroupLabels["alertname"],
		"the alert's labels are the card's labels, which is what the policy matcher reads "+
			"when the query names no focus")
	require.Equal(t, 1, snap.Group.Generation,
		"`alert_cases.seq` answers which run of this alert the reader is looking at")
	require.Positive(t, snap.Group.StateVersion,
		"the §C.7 idempotency fallback needs a version that MOVES; a constant would make "+
			"every fact about this Case collide with the first one")
	require.False(t, snap.Group.StartedAt().IsZero())

	// ⛔ THE FIVE FIELDS THAT DIED WITH THE ROUTE. Each is empty because the
	// question has no row to be asked of, and every consumer already draws the
	// empty string as an answer rather than as a gap.
	require.Empty(t, snap.Group.GroupKey)
	require.Empty(t, snap.Group.Receiver)
	require.Empty(t, snap.Group.SourceGroupKey)
	require.Empty(t, snap.Group.NotificationReason)
	require.Empty(t, snap.Group.AlertmanagerURL,
		"`alert_groups.source_id` was the only path from a card to `alert_sources`, so the "+
			"Silence deep link is off until `alert_cases` carries a source. Empty is the "+
			"answer the renderer already draws as no button; a fabricated URL 404s at 03:00.")
}

// TestAMissingCaseFailsTheSnapshot is the succession of `group_not_found`.
//
// ⛔ IT FAILS RATHER THAN RENDERING A BLANK CARD. The conversation IS the subject
// now, so a snapshot that cannot find it has nothing to render, and a message sent
// to a channel with no subject in it is worse than a delivery that retries.
func TestAMissingCaseFailsTheSnapshot(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)

	_, err := repository.NewSnapshotRepository(w.fx.h.Pool, w.fx.h.Clock).
		Snapshot(w.fx.h.Ctx, w.fx.scope, domain.SnapshotQuery{CaseID: id.New()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "case",
		"the code a client reads moved from `group_not_found` to `case_not_found` with the "+
			"entity; the view service's own comment still names the old one")
}

// TestTheFocusAlertReadsItsAckFromItsCurrentEpisode is the other read that used
// to name `a.ack_state`. The focus is reached by `alert_id`, not through the
// conversation's Case, so its ack is the CURRENT case's — the same episode
// `include=current_case` expands on the alert list.
//
// ⚠️ THIS IS WHY `readFocus` IS STILL A SEPARATE READ under one-Case, and the
// disagreement below is the evidence: the card says `unacked` about the episode it
// is rendering while the focus says `acked` about the alert's current one. Both are
// true statements about different questions, and filling the focus from the Case's
// own alert would have silently collapsed them.
func TestTheFocusAlertReadsItsAckFromItsCurrentEpisode(t *testing.T) {
	t.Parallel()

	w := newAckWorld(t)
	h := w.fx.h

	snap := w.snapshot(t, domain.SnapshotQuery{CaseID: w.carriedLive, AlertID: &w.carried})
	require.NotNil(t, snap.Focus)
	require.Equal(t, "acked", snap.Focus.AckState,
		"the focus alert's current episode carries the receipt, so the card shows it")
	require.Equal(t, "unacked", snap.Alerts[0].AckState,
		"and the conversation's own Case is a different episode with a different answer")

	// An alert with NO episode at all reads `unacked` rather than empty: the card
	// renders the string, and the honest reading of "no receipt" is "unacked".
	orphan := seedAlert(t, h, w.fx, "NeverFired")
	snap = w.snapshot(t, domain.SnapshotQuery{CaseID: w.carriedLive, AlertID: &orphan})
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
	// still naming `a.ack_state` — or `alert_groups`, which no longer exists at all
	// — cannot parse, so this is the failure mode the compiler could not give us.
	w.snapshot(t, domain.SnapshotQuery{CaseID: w.freshLive, AlertID: &w.fresh})

	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'alerts'
		   AND column_name LIKE '%ack%'`).Scan(&n))
	require.Zero(t, n, "`alerts` has an acknowledgement column again")
}

// ---------------------------------------------------------------- the seeding

// snapFixture is these two files' own tenant, and it seeds AN ORG AND A CLUSTER
// AND NOTHING ELSE.
//
// ⛔ IT DOES NOT USE `newFixture`, AND THAT IS NOT DUPLICATION FOR ITS OWN SAKE
// (git-bug `7570090`). `newFixture` calls `harness.H.Group`, which INSERTs into
// `alert_groups` — a table migration `00069` drops — so every test built on it
// fails in the fixture, before reaching any assertion, until the harness is
// answered. These files have no group to seed: a conversation IS a Case. Keeping
// their own two-row tenant is what lets them state that, and it is the same reason
// the seeding helpers below are this file's own rather than shared.
type snapFixture struct {
	h     *harness.H
	scope db.TenantScope
}

func newSnapFixture(t *testing.T) snapFixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	// The cluster exists because `seedAlert` reads one out of the table rather than
	// being handed an id: `alerts.cluster_id` and `alerts.cluster_key` have to agree,
	// and letting SQL pick both from the same row is one fewer thing a seed can get
	// wrong.
	h.Cluster(org)
	return snapFixture{h: h, scope: org.Scope}
}

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

func seedAlert(t *testing.T, h *harness.H, fx snapFixture, name string) uuid.UUID {
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

// seedCase writes one episode and NAMES NO GROUP. `alert_cases.group_id` is
// dropped (git-bug `7570090`, migration `00069`), so naming it would not merely be
// stale — the INSERT would fail to parse.
func seedCase(
	t *testing.T, h *harness.H, fx snapFixture, alertID uuid.UUID, seq int, s occSeed,
) uuid.UUID {
	t.Helper()

	caseID := id.New()
	var resolveReason *string
	if s.endedAt != nil {
		r := "upstream"
		resolveReason = &r
	}
	h.Exec(`INSERT INTO alert_cases
	          (id, org_id, alert_id, seq, state, started_at, ended_at,
	           last_observed_at, source_starts_at, resolve_reason,
	           ack_state, acked_at, acked_by_label)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $6, $6, $8, $9, $10, $11)`,
		caseID, fx.scope.OrgID(), alertID, seq, s.state,
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
