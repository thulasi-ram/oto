package repository

// The behaviour behind migration 00051, asserted against a real database.
//
// These two tests are the ones the defect report asked for by name. They are
// in-package for the same reason member_plan_test.go is: the statements under
// test are unexported constants, and a copy of them in an external test would go
// on passing after the real ones drifted.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// TestTheMemberListHoldsOnlyLiveEpisodes is the assertion the whole change
// exists for.
//
// ⭐ WHAT WAS WRONG. Membership lived in `alert_group_members`, whose `left_at`
// was written by `Leave` — a method implemented at three layers, emitting
// `group.member_left`, and CALLED FROM NOWHERE. So `left_at` was always NULL and
// every `left_at IS NULL` read matched every row that had ever been inserted. The
// group card said "what is wrong now" and answered "everything that was ever
// wrong in this generation": resolved and expired episodes included, and the
// count above them overstated to match.
//
// ⭐ AND THE DUPLICATE. The join table was keyed `(group_id, case_id)`, so
// an alert that resolved and re-fired outside `refire_grace` but inside one
// generation got a SECOND row, also NULL — the same alert listed twice, once
// resolved and once firing. Both `refire_grace` and `group_close_delay` default to
// 1200s, so that is a twenty-minute window and not a pathological one.
//
// Both are now structural rather than remembered. Membership IS the episode:
// `ended_at IS NULL` is the predicate, `case_terminal_ended` makes it identical to
// `state IN ('firing','suppressed')`, and `case_one_open_idx` — UNIQUE (alert_id)
// WHERE ended_at IS NULL — means one alert CANNOT appear twice in a live
// membership.
func TestTheMemberListHoldsOnlyLiveEpisodes(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	// `alertname` is an AXIS since ADR 0038; `severity` is not. A group is seeded
	// by naming an alert, so the axis carries the identity and severity rides along
	// as an ordinary label.
	group := h.GroupWith(org, source, cluster, map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
	})

	scope, err := db.NewTenantScope(org.ID)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	repo := NewMemberRepository(h.Pool, clock.NewFake(harness.Epoch))

	alert := func(name string) harness.Alert {
		return h.AlertWith(org, cluster, map[string]string{
			"alertname": name,
			"severity":  "critical",
			"service":   "checkout",
		})
	}

	// One generation, six episodes, every terminal state represented — and one
	// alert holding TWO of them.
	resolved := alert("MembershipResolved")
	expired := alert("MembershipExpired")
	firing := alert("MembershipFiring")
	suppressed := alert("MembershipSuppressed")
	refired := alert("MembershipRefired")

	base := harness.Epoch
	seedEpisode(t, h, resolved, group.ID, 1, "resolved", base, ptr(base.Add(5*time.Minute)))
	seedEpisode(t, h, expired, group.ID, 1, "expired", base.Add(time.Minute), ptr(base.Add(6*time.Minute)))
	firingID := seedEpisode(t, h, firing, group.ID, 1, "firing", base.Add(2*time.Minute), nil)
	suppressedID := seedEpisode(t, h, suppressed, group.ID, 1, "suppressed", base.Add(3*time.Minute), nil)
	// ⭐ THE RE-FIRE. Episode 1 resolved inside this generation; episode 2 is the
	// same alert firing again, in the SAME generation. Two rows, one alert.
	staleRefire := seedEpisode(t, h, refired, group.ID, 1, "resolved",
		base.Add(4*time.Minute), ptr(base.Add(9*time.Minute)))
	liveRefire := seedEpisode(t, h, refired, group.ID, 2, "firing", base.Add(10*time.Minute), nil)

	members, cur, err := repo.ListCurrentMembers(h.Ctx, scope, group.ID, db.Keyset{Limit: 50})
	if err != nil {
		t.Fatalf("ListCurrentMembers: %v", err)
	}
	if cur.HasMore {
		t.Fatal("one page of fifty did not hold six episodes; the assertions below would be about a page, not the membership")
	}

	got := make(map[uuid.UUID]bool, len(members))
	byAlert := map[uuid.UUID]int{}
	for _, m := range members {
		got[m.CaseID()] = true
		byAlert[m.AlertID()]++
		if !m.IsCurrent() {
			t.Errorf("episode %s is in the current-member list with LeftAt %s: the list is "+
				"supposed to be exactly the episodes that have not ended",
				m.CaseID(), m.LeftAt())
		}
	}

	want := map[uuid.UUID]bool{firingID: true, suppressedID: true, liveRefire: true}
	if len(members) != len(want) {
		t.Fatalf("current members = %d, want %d — the list is answering "+
			"\"everything that was ever in this generation\" while looking like it answers "+
			"\"what is wrong now\"", len(members), len(want))
	}
	for caseID := range want {
		if !got[caseID] {
			t.Errorf("live episode %s is missing from the current-member list", caseID)
		}
	}
	if got[staleRefire] {
		t.Error("the RESOLVED episode of the re-fired alert is still listed as a current member, " +
			"so the alert appears twice — once resolved and once firing — which is exactly what " +
			"a member row per episode with a left_at nothing wrote produced")
	}
	if n := byAlert[refired.ID]; n != 1 {
		t.Errorf("the alert that resolved and re-fired inside one generation appears %d times, "+
			"want exactly 1", n)
	}

	// The count behind "500 of 5 000" must agree with the list, or a fan-out
	// reports a ceiling it never reached.
	n, err := repo.CountCurrentMembers(h.Ctx, scope, group.ID)
	if err != nil {
		t.Fatalf("CountCurrentMembers: %v", err)
	}
	if n != len(want) {
		t.Errorf("CountCurrentMembers = %d, want %d", n, len(want))
	}

	// The fan-out's candidate read is a different statement asking the same
	// question, and the two used to disagree the moment an episode ended.
	candidates, err := repo.CurrentMemberAlerts(h.Ctx, scope, group.ID, 0)
	if err != nil {
		t.Fatalf("CurrentMemberAlerts: %v", err)
	}
	if len(candidates) != len(want) {
		t.Errorf("fan-out candidates = %d, want %d", len(candidates), len(want))
	}
	for _, c := range candidates {
		if !want[c.CaseID] {
			t.Errorf("fan-out candidate %s is not a live episode of the generation", c.CaseID)
		}
	}

	// ⛔ AND THE ROLLUP IS DELIBERATELY NOT FILTERED. The card's breakdown — "6
	// alerts, 2 firing, 2 resolved, 1 expired" — is over EVERY episode the
	// generation has held. Restricting it to live members would make `resolved` and
	// `expired` permanently zero, which is the opposite mistake to the one above.
	counts, severity, err := repo.Rollup(h.Ctx, scope, group.ID)
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	if counts.Total != 6 {
		t.Errorf("rollup total = %d, want 6 — the rollup counts the generation's whole "+
			"membership, not its live members", counts.Total)
	}
	// ⭐ FIRING IS 3, NOT 2, AND THAT IS ADR 0041. Two audible firing members plus
	// the silenced one, which is firing too — `suppressed` is now a SUBSET of
	// `firing` and answers who is not being told, rather than removing a member
	// from the count of what is on fire. The old 2 was the under-report.
	if counts.Firing != 3 || counts.Suppressed != 1 || counts.Resolved != 2 || counts.Expired != 1 {
		t.Errorf("rollup firing/suppressed/resolved/expired = %d/%d/%d/%d, want 3/1/2/1",
			counts.Firing, counts.Suppressed, counts.Resolved, counts.Expired)
	}
	if severity != "critical" {
		t.Errorf("rollup severity = %q, want %q", severity, "critical")
	}
}

// TestTheReplayCanShrink closes the fourth consequence of the dead `Leave`: the
// point-in-time replay was MONOTONIC BY CONSTRUCTION.
//
// `membersAtSQL` reads `joined_at <= $3 AND (left_at IS NULL OR left_at > $3)`.
// With nothing ever writing `left_at` the second clause was a tautology, so
// "which members did this generation have at instant T" could only ever answer
// "every member it had acquired by T" — a curve that never goes down. The promise
// 00008 made about the card being replayable at any past instant was therefore
// unkeepable, and nothing said so.
//
// `ended_at` is written by the §B.3 state machine on every T5 and T6, so the same
// two clauses over the episode now describe a membership that shrinks.
func TestTheReplayCanShrink(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.GroupWith(org, source, cluster, map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
	})

	scope, err := db.NewTenantScope(org.ID)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	repo := NewMemberRepository(h.Pool, clock.NewFake(harness.Epoch))

	base := harness.Epoch
	early := h.AlertWith(org, cluster, map[string]string{"alertname": "ReplayEarly", "severity": "critical"})
	late := h.AlertWith(org, cluster, map[string]string{"alertname": "ReplayLate", "severity": "critical"})

	// `early` joins at T+0 and leaves at T+10. `late` joins at T+20 and stays.
	seedEpisode(t, h, early, group.ID, 1, "resolved", base, ptr(base.Add(10*time.Minute)))
	seedEpisode(t, h, late, group.ID, 1, "firing", base.Add(20*time.Minute), nil)

	at := func(d time.Duration) int {
		t.Helper()
		members, err := repo.MembersAt(h.Ctx, scope, group.ID, base.Add(d))
		if err != nil {
			t.Fatalf("MembersAt(+%s): %v", d, err)
		}
		return len(members)
	}

	if n := at(5 * time.Minute); n != 1 {
		t.Fatalf("members at +5m = %d, want 1 (only the early episode had started)", n)
	}
	// ⭐ THE ASSERTION. Between +5m and +15m the generation LOST a member and
	// gained nothing. Under the join table this number could not go down.
	if n := at(15 * time.Minute); n != 0 {
		t.Errorf("members at +15m = %d, want 0 — the early episode ended at +10m, so a replay "+
			"that still counts it is monotonic by construction and the group card cannot be "+
			"replayed at a past instant after all", n)
	}
	if n := at(25 * time.Minute); n != 1 {
		t.Errorf("members at +25m = %d, want 1 (only the late episode)", n)
	}

	// The whole-history read is the one that must NOT shrink: it is what makes a
	// replay possible in the first place.
	all, err := repo.AllMembers(h.Ctx, scope, group.ID)
	if err != nil {
		t.Fatalf("AllMembers: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllMembers = %d, want 2 — dropping the ended episodes here would delete the "+
			"history the replay reads", len(all))
	}
}

// seedEpisode writes one `alert_cases` row directly, satisfying the §D.4 CHECKs
// that pair `state` with `ended_at`, `resolve_reason` and `suppression_reason`.
//
// It writes the row rather than driving the state machine on purpose: these tests
// are about what the MEMBERSHIP READS return for a given set of episodes, and a
// test that had to reach a state through the lifecycle could not seed the one
// arrangement that matters most here — two episodes of one alert inside one
// generation.
//
// ⭐⭐ IT TAKES THE FOUR-WAY §B.2 WORD AND WRITES ADR 0040's TWO-WAY ONE, which is
// the derivation exercised on every call rather than described. `alert_cases.state`
// holds `open` or `closed` since migration 00054; what tells `firing` from
// `suppressed` is the ALERT, and what tells `resolved` from `expired` is
// `resolve_reason`. So this writes all three columns, and `alerts.state` with them
// whenever the episode is open — an open episode IS its alert's current one
// (case_one_open_idx), so a fixture that left `alerts.state` behind would be
// seeding a database the lifecycle cannot produce and the rollup would read a
// firing member as suppressed, or worse, as neither.
func seedEpisode(
	t *testing.T, h *harness.H, a harness.Alert, groupID uuid.UUID,
	seq int, state string, startedAt time.Time, endedAt *time.Time,
) uuid.UUID {
	t.Helper()

	var resolveReason, suppressionReason *string
	caseState := "open"
	switch state {
	case "resolved":
		caseState = "closed"
		resolveReason = ptr("upstream")
	case "expired":
		caseState = "closed"
		resolveReason = ptr("timeout")
	case "suppressed":
		suppressionReason = ptr("silence")
	}
	lastObserved := startedAt
	if endedAt != nil {
		lastObserved = *endedAt
	}

	caseID := id.New()
	h.Exec(`INSERT INTO alert_cases
	          (id, org_id, alert_id, group_id, seq, state, suppression_reason, resolve_reason,
	           started_at, ended_at, last_observed_at, source_starts_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $9)`,
		caseID, a.OrgID, a.ID, groupID, seq, caseState, suppressionReason, resolveReason,
		startedAt, endedAt, lastObserved)
	if caseState == "open" {
		// ⭐ ADR 0041: `alerts.state` admits `firing | resolved | expired` only, and
		// suppression is the axis beside it. A silenced member is seeded as FIRING
		// with a reason, which is exactly what the projection writes and what the
		// member roll-up has to count in both buckets.
		alertState := state
		var alertSuppression *string
		if state == "suppressed" {
			alertState = "firing"
			alertSuppression = ptr("silence")
		}
		h.Exec(`UPDATE alerts SET state = $2, suppression_reason = $4, current_case_id = $3
		         WHERE id = $1`,
			a.ID, alertState, caseID, alertSuppression)
	}
	return caseID
}

func ptr[T any](v T) *T { return &v }
