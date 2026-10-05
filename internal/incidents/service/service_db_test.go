package service

// ADR 0052 §1–§4 AGAINST A REAL POSTGRES (git-bug b2672a1).
//
// ⭐ THE CLAIMS HERE ARE ABOUT SQL, SO THE DATABASE IS REAL. "A Case belongs to at
// most one Incident" is a partial unique index; "active/quiet is derived" is an
// aggregate over `alert_cases`; "a removed Case stays recorded as removed" is a
// tombstone column; "a move is atomic" is a transaction. A fake repository would
// agree with whatever the service asked of it, which is exactly the failure each of
// those four sentences exists to rule out.
//
// The timeline port is the one fake, and it is a RECORDER rather than a stub: the
// row it would write is `internal/app`'s concern (timeline_incident_db_test.go
// there drives the real seam), and what this file asks is which facts the service
// narrates and that a narration failure takes the membership change down with it.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/incidents/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// recordedTimeline is the Timeline port as a recorder. failOn, when set, makes the
// append of that one type fail — which is how a test proves the narration and the
// membership commit together.
type recordedTimeline struct {
	mu     sync.Mutex
	facts  []CaseFact
	failOn kernel.EventType
}

func (r *recordedTimeline) RecordIncidentFact(_ context.Context, _ db.TenantScope, f CaseFact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.failOn.IsZero() && f.Type == r.failOn {
		return errors.New("the timeline refused the fact")
	}
	r.facts = append(r.facts, f)
	return nil
}

func (r *recordedTimeline) of(t kernel.EventType) []CaseFact {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []CaseFact
	for _, f := range r.facts {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

// recordedAnnouncer is the Announcer port as a recorder: which Incident facts the
// service declared, in order. Whether a policy routes them is the notification
// layer's question and is tested there.
type recordedAnnouncer struct {
	mu    sync.Mutex
	facts []Announcement
}

func (r *recordedAnnouncer) Announce(_ context.Context, _ db.TenantScope, facts []Announcement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.facts = append(r.facts, facts...)
	return nil
}

// sequences returns the sequence each fact about one Incident was declared with, in
// the order they were declared.
func (r *recordedAnnouncer) sequences(incidentID uuid.UUID) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int64
	for _, f := range r.facts {
		if f.IncidentID == incidentID {
			out = append(out, f.Sequence)
		}
	}
	return out
}

// of returns the facts declared about one Incident, in order.
func (r *recordedAnnouncer) of(incidentID uuid.UUID) []domain.Fact {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Fact
	for _, f := range r.facts {
		if f.IncidentID == incidentID {
			out = append(out, f.Fact)
		}
	}
	return out
}

type rig struct {
	t        *testing.T
	h        *harness.H
	org      harness.Org
	cluster  harness.Cluster
	scope    db.TenantScope
	svc      *Service
	repo     *repository.IncidentRepository
	timeline *recordedTimeline
	announce *recordedAnnouncer
	alice    domain.Attribution
}

func newRig(t *testing.T) *rig {
	t.Helper()
	h := harness.New(t)
	org := h.Org()
	user := h.User(org)
	alice, err := domain.Human(user.ID, "alice")
	require.NoError(t, err)

	r := &rig{
		t: t, h: h, org: org, cluster: h.Cluster(org), scope: org.Scope,
		repo:     repository.NewIncidentRepository(h.Pool),
		timeline: &recordedTimeline{},
		announce: &recordedAnnouncer{},
		alice:    alice,
	}
	r.svc, err = New(Deps{
		Incidents: r.repo,
		Tx:        repository.NewTxRunner(h.Pool),
		Timeline:  r.timeline,
		Announcer: r.announce,
		Clock:     h.Clock,
	})
	require.NoError(t, err)
	return r
}

// openCase seeds one open Case of a fresh Alert, so every Case in a test belongs to
// a different identity, as the Cases of a real story usually do.
func (r *rig) openCase(alertname string) uuid.UUID {
	r.t.Helper()
	a := r.h.AlertWith(r.org, r.cluster, map[string]string{
		"alertname": alertname, "severity": "critical", "service": "checkout",
	})
	return r.h.Case(a).ID
}

// closeCase ends a Case the way the lifecycle does — state, ended_at and the
// resolve reason together, as `case_terminal_ended` and `case_resolve_ck` demand.
func (r *rig) closeCase(id uuid.UUID) {
	r.t.Helper()
	r.h.Exec(`UPDATE alert_cases
	             SET state = 'closed', ended_at = $2, resolve_reason = 'upstream'
	           WHERE id = $1`, id, r.h.Now().Add(time.Minute))
}

func (r *rig) get(number int64) domain.Detail {
	r.t.Helper()
	d, err := r.svc.Get(context.Background(), r.scope, number)
	require.NoError(r.t, err)
	return d
}

func current(d domain.Detail) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range d.Members {
		if m.Current() {
			out = append(out, m.CaseID)
		}
	}
	return out
}

// ------------------------------------------------------------------- draw

func TestADrawnIncidentHoldsItsCasesAndNarratesEachOne(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	a, b := r.openCase("HighErrorRate"), r.openCase("KubePodCrashLooping")
	d, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{a, b, a}, r.alice)
	require.NoError(t, err)

	assert.EqualValues(t, 1, d.Number, "every org counts from 1")
	assert.Equal(t, domain.StateActive, d.State())
	assert.Equal(t, 2, d.MemberCount, "a repeated id names the Case once: an Incident is a set")
	assert.Equal(t, []string{"HighErrorRate", "KubePodCrashLooping"}, d.Alertnames)
	assert.True(t, d.DrawnBy.IsHuman())
	assert.Equal(t, "alice", d.DrawnBy.Label())
	assert.ElementsMatch(t, []uuid.UUID{a, b}, current(d))

	facts := r.timeline.of(kernel.EventIncidentCaseAdded)
	require.Len(t, facts, 2, "one fact on each member Case's timeline")
	for _, f := range facts {
		assert.Equal(t, true, f.Payload["drawn"])
		assert.EqualValues(t, 1, f.Payload["incident_number"])
		assert.Contains(t, f.Summary, "Drawn into Incident #1 by alice")
		assert.NotEqual(t, uuid.Nil, f.AlertID, "the fact lands on the Alert's timeline too")
	}

	second, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("DiskFull")}, r.alice)
	require.NoError(t, err)
	assert.EqualValues(t, 2, second.Number, "numbers are per-org and monotonic")

	list, _, err := r.svc.List(ctx, r.scope, db.Keyset{Limit: 10}, domain.ListFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.EqualValues(t, 2, list[0].Number, "newest first")
}

// ------------------------------------------------------------ at most one

func TestACaseInAnIncidentCannotBeDrawnOrAddedIntoAnother(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	held := r.openCase("HighErrorRate")
	first, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{held}, r.alice)
	require.NoError(t, err)

	// A second draw naming the held Case is refused WHOLE — the free Case beside it
	// is not drawn either — and the refusal points at the move.
	free := r.openCase("DiskFull")
	_, err = r.svc.Draw(ctx, r.scope, []uuid.UUID{free, held}, r.alice)
	require.Error(t, err)
	assert.Equal(t, "case_in_incident", errs.CodeOf(err))
	assert.Contains(t, err.Error(), "/api/v1/incidents/1/cases/"+held.String()+"/move")
	list, _, err := r.svc.List(ctx, r.scope, db.Keyset{Limit: 10}, domain.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, list, 1, "a refused draw draws nothing")

	other, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{free}, r.alice)
	require.NoError(t, err)

	_, err = r.svc.Add(ctx, r.scope, other.Number, held, r.alice)
	assert.Equal(t, "case_in_incident", errs.CodeOf(err), "adding a Case held elsewhere points at the move")

	_, err = r.svc.Add(ctx, r.scope, first.Number, held, r.alice)
	assert.Equal(t, "already_a_member", errs.CodeOf(err))
}

// TestTheDatabaseNotTheServiceEnforcesAtMostOne goes UNDER the service's read and
// writes the second membership directly. The partial unique index is the rule; if
// it were only a service check, this write would succeed.
func TestTheDatabaseNotTheServiceEnforcesAtMostOne(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	c := r.openCase("HighErrorRate")
	first, err := r.repo.Insert(ctx, r.scope, r.h.Now(), r.alice)
	require.NoError(t, err)
	second, err := r.repo.Insert(ctx, r.scope, r.h.Now(), r.alice)
	require.NoError(t, err)

	require.NoError(t, r.repo.AddMember(ctx, r.scope, first.ID, c, r.h.Now(), r.alice))
	err = r.repo.AddMember(ctx, r.scope, second.ID, c, r.h.Now(), r.alice)
	require.Error(t, err, "incident_members_case_live_uniq must refuse a second live membership")
	assert.Equal(t, "case_in_incident", errs.CodeOf(err), "the race answers in the same code as the read")

	// A TOMBSTONE DOES NOT COUNT: once removed, the Case may join elsewhere.
	require.NoError(t, r.repo.RemoveMember(ctx, r.scope, first.ID, c, r.h.Now(), r.alice, uuid.Nil))
	require.NoError(t, r.repo.AddMember(ctx, r.scope, second.ID, c, r.h.Now(), r.alice))
}

// ---------------------------------------------------------- derived state

func TestTheStateIsReadOffTheMemberCasesAndNeverWritten(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	a, b := r.openCase("HighErrorRate"), r.openCase("KubePodCrashLooping")
	d, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{a, b}, r.alice)
	require.NoError(t, err)
	assert.Equal(t, domain.StateActive, d.State())

	r.closeCase(a)
	d = r.get(d.Number)
	assert.Equal(t, domain.StateActive, d.State(), "one open member is enough")
	assert.Equal(t, 1, d.OpenMemberCount)

	r.closeCase(b)
	d = r.get(d.Number)
	assert.Equal(t, domain.StateQuiet, d.State(), "no open member left: quiet — not resolved, not closed")
	assert.Equal(t, 2, d.MemberCount, "a closed Case is still a member; closing it is not removing it")

	// ⭐ ADDING AN OPEN CASE TO A QUIET INCIDENT MAKES IT ACTIVE AGAIN, and nothing
	// but the membership moved.
	c := r.openCase("DiskFull")
	d, err = r.svc.Add(ctx, r.scope, d.Number, c, r.alice)
	require.NoError(t, err)
	assert.Equal(t, domain.StateActive, d.State())

	// ⭐ AND REMOVING IT MAKES IT QUIET AGAIN. An Incident whose open members have
	// all been removed has nothing open in it.
	d, err = r.svc.Remove(ctx, r.scope, d.Number, c, r.alice)
	require.NoError(t, err)
	assert.Equal(t, domain.StateQuiet, d.State())
}

// ------------------------------------------------------------------ remove

func TestARemovedCaseStaysRecordedAsRemoved(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	a, b := r.openCase("HighErrorRate"), r.openCase("KubePodCrashLooping")
	d, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{a, b}, r.alice)
	require.NoError(t, err)

	r.h.Advance(time.Minute)
	d, err = r.svc.Remove(ctx, r.scope, d.Number, a, r.alice)
	require.NoError(t, err)

	assert.Equal(t, 1, d.MemberCount, "removed Cases are not counted")
	require.Len(t, d.Members, 2, "but they are still on the record")
	var tomb domain.Member
	for _, m := range d.Members {
		if m.CaseID == a {
			tomb = m
		}
	}
	assert.False(t, tomb.Current())
	assert.Equal(t, "alice", tomb.RemovedByLabel)
	assert.Zero(t, tomb.MovedToNumber, "a plain removal is not a move")
	assert.True(t, tomb.CaseState.IsOpen(), "removing a Case from a story does nothing to the Case")

	removed := r.timeline.of(kernel.EventIncidentCaseRemoved)
	require.Len(t, removed, 1)
	assert.Equal(t, a, removed[0].CaseID)
	assert.Contains(t, removed[0].Summary, "Removed from Incident #1 by alice")

	// A second removal finds no CURRENT membership: the tombstone is not one.
	_, err = r.svc.Remove(ctx, r.scope, d.Number, a, r.alice)
	assert.Equal(t, "incident_member_not_found", errs.CodeOf(err))

	// And a removed Case may come back, as a second spell beside the first.
	d, err = r.svc.Add(ctx, r.scope, d.Number, a, r.alice)
	require.NoError(t, err)
	assert.Len(t, d.Members, 3)
	assert.Equal(t, 2, d.MemberCount)
}

// TestAnEmptyIncidentIsKeptButLeftOffTheList — owner ruling 2026-10-04. An
// Incident whose every Case was removed or moved away stays in the database and is
// served by number, but the list leaves it out unless the filter asks for it.
//
// ⭐ "EMPTY" IS NO CURRENT MEMBER, NOT "QUIET". An Incident whose Cases all CLOSED
// still holds them and is still listed; only one with nothing left in it is hidden.
func TestAnEmptyIncidentIsKeptButLeftOffTheList(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	// #1 is emptied by a removal, #2 by a move into #3, and #4 is quiet but whole.
	removed := r.openCase("HighErrorRate")
	emptied, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{removed}, r.alice)
	require.NoError(t, err)
	moving := r.openCase("KubePodCrashLooping")
	left, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{moving}, r.alice)
	require.NoError(t, err)
	joined, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("DiskFull")}, r.alice)
	require.NoError(t, err)
	closed := r.openCase("Watchdog")
	quiet, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{closed}, r.alice)
	require.NoError(t, err)
	r.closeCase(closed)

	r.h.Advance(time.Minute)
	_, err = r.svc.Remove(ctx, r.scope, emptied.Number, removed, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Move(ctx, r.scope, left.Number, joined.Number, moving, r.alice)
	require.NoError(t, err)

	numbers := func(f domain.ListFilter) []int64 {
		t.Helper()
		list, _, err := r.svc.List(ctx, r.scope, db.Keyset{Limit: 10}, f)
		require.NoError(t, err)
		out := make([]int64, 0, len(list))
		for _, i := range list {
			out = append(out, i.Number)
		}
		return out
	}
	assert.Equal(t, []int64{quiet.Number, joined.Number}, numbers(domain.ListFilter{}),
		"the default list hides the two husks and keeps the quiet Incident that still holds its Case")
	assert.Equal(t, []int64{quiet.Number, joined.Number, left.Number, emptied.Number},
		numbers(domain.ListFilter{IncludeEmpty: true}), "asked, the list serves every Incident")

	// ⭐ A HIDDEN PAGE BOUNDARY IS STILL A POSITION. A page of one over the default
	// list must step past the husks rather than end on them.
	page, next, err := r.svc.List(ctx, r.scope, db.Keyset{Limit: 1}, domain.ListFilter{})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.True(t, next.HasMore)
	page, next, err = r.svc.List(ctx, r.scope, db.Keyset{Limit: 1, Cursor: next}, domain.ListFilter{})
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, joined.Number, page[0].Number)
	assert.False(t, next.HasMore, "nothing after #%d is listed by default", joined.Number)

	// The record is never hidden from its address.
	d := r.get(emptied.Number)
	assert.Zero(t, d.MemberCount)
	require.Len(t, d.Members, 1, "the removed spell is still on the record")
	assert.False(t, d.Members[0].Current())
}

// -------------------------------------------------------------------- move

func TestAMoveIsOneAttributedTransaction(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	moving := r.openCase("HighErrorRate")
	from, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{moving}, r.alice)
	require.NoError(t, err)
	to, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("KubePodCrashLooping")}, r.alice)
	require.NoError(t, err)

	dst, err := r.svc.Move(ctx, r.scope, from.Number, to.Number, moving, r.alice)
	require.NoError(t, err)
	assert.Equal(t, to.Number, dst.Number, "a move answers with where the Case went")
	assert.Contains(t, current(dst), moving)
	assert.Equal(t, 2, dst.MemberCount)

	src := r.get(from.Number)
	require.Len(t, src.Members, 1)
	assert.False(t, src.Members[0].Current())
	assert.Equal(t, to.Number, src.Members[0].MovedToNumber, "the tombstone says where it went")
	assert.Equal(t, "alice", src.Members[0].RemovedByLabel, "both halves are attributed")
	assert.Equal(t, domain.StateQuiet, src.State(), "an Incident left with no members is quiet")

	moved := r.timeline.of(kernel.EventIncidentCaseMoved)
	require.Len(t, moved, 1, "one decision, one fact — not a removal and an add")
	assert.EqualValues(t, from.Number, moved[0].Payload["from_number"])
	assert.EqualValues(t, to.Number, moved[0].Payload["to_number"])
	assert.Contains(t, moved[0].Summary, "Moved from Incident #1 to #2 by alice")

	// The source is NAMED: the Case is no longer in #1, so moving it "from #1" again
	// is a 404 rather than a move from somewhere it is not.
	_, err = r.svc.Move(ctx, r.scope, from.Number, to.Number, moving, r.alice)
	assert.Equal(t, "incident_member_not_found", errs.CodeOf(err))

	_, err = r.svc.Move(ctx, r.scope, to.Number, to.Number, moving, r.alice)
	assert.Equal(t, errs.KindValidation, errs.KindOf(err), "a move to itself is refused")
}

// TestAFailedMoveLeavesTheCaseWhereItWas — the tombstone, the new membership and
// the narration commit together. When the last of the three fails, the first two
// are rolled back: the Case is in its old Incident, current, and in no other.
func TestAFailedMoveLeavesTheCaseWhereItWas(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	moving := r.openCase("HighErrorRate")
	from, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{moving}, r.alice)
	require.NoError(t, err)
	to, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("KubePodCrashLooping")}, r.alice)
	require.NoError(t, err)

	r.timeline.failOn = kernel.EventIncidentCaseMoved
	_, err = r.svc.Move(ctx, r.scope, from.Number, to.Number, moving, r.alice)
	require.Error(t, err)

	src, dst := r.get(from.Number), r.get(to.Number)
	assert.Equal(t, []uuid.UUID{moving}, current(src), "the Case is still where it was")
	assert.Len(t, src.Members, 1, "and no tombstone was left behind")
	assert.NotContains(t, current(dst), moving, "and it never arrived")
	assert.Len(t, dst.Members, 1)
}

// ------------------------------------------------------------------ tenancy

func TestAnotherOrgsCasesAndIncidentsAreNotFound(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	mine, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("HighErrorRate")}, r.alice)
	require.NoError(t, err)

	other := r.h.Org()
	theirCluster := r.h.Cluster(other)
	theirCase := r.h.Case(r.h.Alert(other, theirCluster)).ID

	_, err = r.svc.Draw(ctx, r.scope, []uuid.UUID{theirCase}, r.alice)
	assert.Equal(t, "case_not_found", errs.CodeOf(err), "another org's Case is a Case this org does not have")
	_, err = r.svc.Add(ctx, r.scope, mine.Number, theirCase, r.alice)
	assert.Equal(t, "case_not_found", errs.CodeOf(err))

	// Every org counts from 1, so `#1` exists in both; theirs is not mine.
	_, err = r.svc.Get(ctx, other.Scope, mine.Number)
	assert.Equal(t, "incident_not_found", errs.CodeOf(err))
}

// ------------------------------------------------------------------ drills

// TestADrillsSyntheticCaseIsNeverDrawnOrAdded is the human half of the rule the
// Correlator already keeps (`correlationCaseSQL`): a delivery drill's Case would
// otherwise reach the org's incident tool through a human's draw, and the drill's
// disposal would leave the Incident with a member that no longer exists.
func TestADrillsSyntheticCaseIsNeverDrawnOrAdded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	drill := r.openCase("OtoDeliveryDrill")
	r.h.Exec(`UPDATE alerts SET synthetic = true
	           WHERE id = (SELECT alert_id FROM alert_cases WHERE id = $1)`, drill)
	free := r.openCase("HighErrorRate")

	_, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{free, drill}, r.alice)
	require.Error(t, err)
	assert.Equal(t, "case_synthetic", errs.CodeOf(err))
	list, _, err := r.svc.List(ctx, r.scope, db.Keyset{Limit: 10}, domain.ListFilter{})
	require.NoError(t, err)
	assert.Empty(t, list, "a draw naming a drill's Case is refused whole")

	in, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{free}, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Add(ctx, r.scope, in.Number, drill, r.alice)
	assert.Equal(t, "case_synthetic", errs.CodeOf(err))
	assert.Equal(t, []uuid.UUID{free}, current(r.get(in.Number)))
}

// ------------------------------------------------------ declared outbound

// TestEveryIncidentFactIsDeclaredAndTheStateEdgesFollowMembership is ADR 0052 §5's
// producer half (git-bug aa6d18b): each verb declares its membership fact, and a
// change that moves the derived state declares `quiet` or `active_again` after it —
// read under the Incident's lock, so the edge is never missed and never doubled.
func TestEveryIncidentFactIsDeclaredAndTheStateEdgesFollowMembership(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	open := r.openCase("HighErrorRate")
	d, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{open}, r.alice)
	require.NoError(t, err)
	id := d.ID
	assert.Equal(t, []domain.Fact{domain.FactDrawn}, r.announce.of(id),
		"a draw is one fact, not one per Case and not a state edge")

	// Removing the only open member quiets the Incident.
	_, err = r.svc.Remove(ctx, r.scope, d.Number, open, r.alice)
	require.NoError(t, err)
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseRemoved, domain.FactQuiet},
		r.announce.of(id))

	// Adding a CLOSED Case changes the membership and not the state.
	closed := r.openCase("DiskFull")
	r.closeCase(closed)
	_, err = r.svc.Add(ctx, r.scope, d.Number, closed, r.alice)
	require.NoError(t, err)
	assert.Equal(t, domain.FactCaseAdded, r.announce.of(id)[3])
	assert.Len(t, r.announce.of(id), 4, "a closed Case joining a quiet Incident moves no state")

	// Adding an OPEN one makes it active again.
	_, err = r.svc.Add(ctx, r.scope, d.Number, open, r.alice)
	require.NoError(t, err)
	assert.Equal(t, []domain.Fact{domain.FactCaseAdded, domain.FactActiveAgain}, r.announce.of(id)[4:])

	// ⛔ NO FACT EVER MEANS RESOLVE OR CLOSE.
	for _, f := range r.announce.facts {
		assert.NotContains(t, []string{"resolved", "closed", "mitigated"}, string(f.Fact))
		assert.NotEqual(t, uuid.Nil, f.Occasion, "every fact names its occasion")
	}

	// ⭐ AND EVERY FACT IS NUMBERED IN THE ORDER IT HAPPENED (migration 00093): 1 for
	// `drawn`, then one more each, so the `quiet` a removal produced is always after the
	// `case_removed` that caused it, whatever order the deliveries arrive in.
	assert.Equal(t, []int64{1, 2, 3, 4, 5, 6}, r.announce.sequences(id))
	var stored int64
	require.NoError(t, r.h.Pool.QueryRow(ctx,
		`SELECT fact_sequence FROM incidents WHERE id = $1`, id).Scan(&stored))
	assert.Equal(t, int64(6), stored, "the counter is the latest fact's number")
}

func TestAMoveDeclaresAFactOnEachIncident(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	moving := r.openCase("HighErrorRate")
	from, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{moving}, r.alice)
	require.NoError(t, err)
	closed := r.openCase("DiskFull")
	r.closeCase(closed)
	to, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{closed}, r.alice)
	require.NoError(t, err)

	_, err = r.svc.Move(ctx, r.scope, from.Number, to.Number, moving, r.alice)
	require.NoError(t, err)

	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseRemoved, domain.FactQuiet},
		r.announce.of(from.ID), "the Incident left behind lost its only open Case")
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseAdded, domain.FactActiveAgain},
		r.announce.of(to.ID), "the quiet Incident joined gained an open Case")
	// Each Incident numbers its OWN story: the move is facts 2 and 3 on both.
	assert.Equal(t, []int64{1, 2, 3}, r.announce.sequences(from.ID))
	assert.Equal(t, []int64{1, 2, 3}, r.announce.sequences(to.ID))
}

// TestACaseClosingQuietsItsIncidentOnce is the observer `alerts` calls inside the
// closing transaction. Only the close of the LAST open member is the edge.
func TestACaseClosingQuietsItsIncidentOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	a, b := r.openCase("HighErrorRate"), r.openCase("KubePodCrashLooping")
	d, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{a, b}, r.alice)
	require.NoError(t, err)

	r.closeCase(a)
	require.NoError(t, r.svc.CasesEnded(ctx, r.scope, []uuid.UUID{a}))
	assert.Equal(t, []domain.Fact{domain.FactDrawn}, r.announce.of(d.ID),
		"one member still open: the Incident is still active")

	r.closeCase(b)
	require.NoError(t, r.svc.CasesEnded(ctx, r.scope, []uuid.UUID{b}))
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactQuiet}, r.announce.of(d.ID))

	// A Case in no Incident ends and nothing is declared about anything.
	stray := r.openCase("DiskFull")
	r.closeCase(stray)
	require.NoError(t, r.svc.CasesEnded(ctx, r.scope, []uuid.UUID{stray}))
	assert.Len(t, r.announce.facts, 2)
}

// TestAVerbWaitsForACaseClosingBeneathIt is the Case lock `casesSQL` takes. A close
// that has not committed holds the Case's row; an add that read past it would count
// the Case open, declare `active_again` on a quiet Incident, and leave it reading
// quiet with no `quiet` to follow — the close's CasesEnded ran before the Case was
// a member. Sharing the lock queues the add behind the close, so it counts the
// Case closed and declares no edge at all.
func TestAVerbWaitsForACaseClosingBeneathIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	ended := r.openCase("DiskFull")
	r.closeCase(ended)
	quiet, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{ended}, r.alice)
	require.NoError(t, err)
	require.Equal(t, domain.StateQuiet, quiet.State())

	closing := r.openCase("HighErrorRate")
	closer, err := r.h.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = closer.Rollback(ctx) }()
	var closerPID int
	require.NoError(t, closer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&closerPID))
	_, err = closer.Exec(ctx, `UPDATE alert_cases
	                              SET state = 'closed', ended_at = $2, resolve_reason = 'upstream'
	                            WHERE id = $1`, closing, r.h.Now().Add(time.Minute))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := r.svc.Add(ctx, r.scope, quiet.Number, closing, r.alice)
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting int
		err := r.h.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`,
			closerPID).Scan(&waiting)
		return err == nil && waiting > 0
	}, 10*time.Second, 10*time.Millisecond, "the add must wait on the closing Case's row")
	require.NoError(t, closer.Commit(ctx))

	require.NoError(t, <-done)
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseAdded}, r.announce.of(quiet.ID),
		"the Case was closed by the time it joined: the Incident never went active")
	assert.Equal(t, domain.StateQuiet, r.get(quiet.Number).State())
}

// TestAnAppliedSuggestionIsTheOrdinaryEditWithItsProvenance — git-bug 8327c00: a human
// applying an Investigation's membership Suggestion goes through Add and Move like any
// hand edit — the same membership row, the same attribution to the human, the same facts
// declared — and the Case's timeline fact also says which Investigation suggested it. A
// hand edit carries no such key.
func TestAnAppliedSuggestionIsTheOrdinaryEditWithItsProvenance(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	investigation := uuid.New()
	suggested, err := r.alice.Suggested(investigation)
	require.NoError(t, err)

	first := r.openCase("HighErrorRate")
	story, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{first}, r.alice)
	require.NoError(t, err)
	joining := r.openCase("CheckoutSlow")
	d, err := r.svc.Add(ctx, r.scope, story.Number, joining, suggested)
	require.NoError(t, err)
	assert.Contains(t, current(d), joining)
	for _, m := range d.Members {
		if m.CaseID == joining {
			assert.Equal(t, "alice", m.AddedBy.Label(), "the applier is the actor")
		}
	}

	added := r.timeline.of(kernel.EventIncidentCaseAdded)
	require.Len(t, added, 2)
	_, handEdit := added[0].Payload["suggested_by_investigation_id"]
	assert.False(t, handEdit, "a draw nobody suggested carries no provenance key")
	assert.Equal(t, investigation.String(), added[1].Payload["suggested_by_investigation_id"])
	assert.Contains(t, added[1].Summary, "by alice, applying an Investigation's Suggestion")

	other, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("KubePodCrashLooping")}, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Move(ctx, r.scope, story.Number, other.Number, joining, suggested)
	require.NoError(t, err)
	moved := r.timeline.of(kernel.EventIncidentCaseMoved)
	require.Len(t, moved, 1)
	assert.Equal(t, investigation.String(), moved[0].Payload["suggested_by_investigation_id"])
}
