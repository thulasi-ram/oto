package service

// ADR 0052 §2 and §4 — THE CORRELATOR — AGAINST A REAL POSTGRES (git-bug 61eeddf).
//
// ⭐ EVERY DONE-WHEN OF THE TICKET IS A TEST HERE, and each one is a claim about SQL
// as much as about Go: "first wins" is a primary key on the Case, "skipped when
// already in an Incident" and "never re-added after a human removed it" are reads of
// `incident_members` tombstones, "one Incident on the fifth" is a window read over
// `correlator_matches`. A fake store would agree with whatever the evaluator asked.
// The timeline and the announcer are the recorders newRig already builds.
//
// "An evaluation failure never blocks or delays a notification" is not here,
// because it is not a property of this function: it is the queue the job rides,
// pinned in `platform/jobs` (TestCorrelationNeverSharesAQueueWithANotification),
// and the opening path's only-enqueue contract, pinned in `alerts/service`
// (TestACaseOpeningIsToldToTheOpeningsPortBesideItsNotification).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/incidents/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

type correlatorRig struct {
	*rig
	k *Correlators
}

func newCorrelatorRig(t *testing.T) *correlatorRig {
	t.Helper()
	r := newRig(t)
	k, err := NewCorrelators(r.svc, repository.NewCorrelatorRepository(r.h.Pool))
	require.NoError(t, err)
	return &correlatorRig{rig: r, k: k}
}

// write creates one Correlator.
func (r *correlatorRig) write(name string, priority int, count domain.Count, ms ...kernel.Matcher) domain.Correlator {
	r.t.Helper()
	k, err := r.k.Create(context.Background(), r.scope, domain.CorrelatorDraft{
		Name: name, Priority: &priority, Matchers: ms, Count: count,
	})
	require.NoError(r.t, err)
	return k
}

// openLabelled opens one Case of a fresh Alert carrying exactly these labels, at
// the harness clock's current instant.
func (r *correlatorRig) openLabelled(kv map[string]string) uuid.UUID {
	r.t.Helper()
	a := r.h.AlertWith(r.org, r.cluster, kv)
	return r.h.Case(a).ID
}

func (r *correlatorRig) correlate(caseID uuid.UUID) Correlation {
	r.t.Helper()
	out, err := r.k.Correlate(context.Background(), r.scope, caseID)
	require.NoError(r.t, err)
	return out
}

func eq(name, value string) kernel.Matcher {
	return kernel.Matcher{Name: name, Op: kernel.OpEqual, Value: value}
}

func payments(i int) map[string]string {
	return map[string]string{
		"alertname": "PaymentsErrors" + string(rune('A'+i)),
		"cluster":   "prod", "namespace": "payments", "severity": "warning",
	}
}

// ------------------------------------------------------------------ drawing

func TestACorrelatorWithNoCountDrawsAOneCaseIncident(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	k := r.write("criticals", 100, domain.Count{}, eq("severity", "critical"))

	c := r.openCase("HighErrorRate") // severity=critical
	got := r.correlate(c)
	require.Equal(t, VerdictDrew, got.Verdict)
	assert.Equal(t, k.ID, got.CorrelatorID)

	d := r.get(got.Incident.Number)
	assert.Equal(t, 1, d.MemberCount, "one matching Case, one-Case Incident")
	assert.False(t, d.DrawnBy.IsHuman())
	assert.Equal(t, k.ID, d.DrawnBy.CorrelatorID(), "the Incident records which Correlator drew it")
	assert.Equal(t, k.ID, d.Members[0].AddedBy.CorrelatorID())
	assert.Equal(t, []domain.Fact{domain.FactDrawn}, r.announce.of(d.ID))

	facts := r.timeline.of(kernel.EventIncidentCaseAdded)
	require.Len(t, facts, 1)
	assert.Equal(t, "criticals", facts[0].CorrelatorName, "the timeline names the Correlator someone wrote")
	assert.Contains(t, facts[0].Summary, `by Correlator "criticals"`)
	assert.Equal(t, k.ID.String(), facts[0].Payload["correlator_id"])
}

func TestACountedCorrelatorDrawsOnTheFifthAndAddsLaterMatchesWhileActive(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	r.write("payments storm", 100, domain.Count{Min: 5, Window: 600 * time.Second},
		eq("cluster", "prod"), eq("namespace", "payments"))

	var first4 []uuid.UUID
	for i := 0; i < 4; i++ {
		c := r.openLabelled(payments(i))
		first4 = append(first4, c)
		assert.Equal(t, VerdictBelowCount, r.correlate(c).Verdict, "Case %d of 5 draws nothing", i+1)
		r.h.Advance(time.Minute)
	}
	incs, _, err := r.svc.List(context.Background(), r.scope, db.Keyset{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, incs, "four below a count of five is no Incident")

	fifth := r.openLabelled(payments(4))
	got := r.correlate(fifth)
	require.Equal(t, VerdictDrew, got.Verdict, "the fifth Case inside 600 s clears the count")
	d := r.get(got.Incident.Number)
	assert.ElementsMatch(t, append(first4, fifth), current(d), "drawn over all five at once")

	// A Case that does not match is no business of this Correlator.
	r.h.Advance(time.Minute)
	other := r.openLabelled(map[string]string{"alertname": "Unrelated", "cluster": "prod", "namespace": "web"})
	assert.Equal(t, VerdictNoMatch, r.correlate(other).Verdict)

	// A later match joins while the Incident is active — whatever the count.
	sixth := r.openLabelled(payments(5))
	joined := r.correlate(sixth)
	require.Equal(t, VerdictJoined, joined.Verdict)
	assert.Equal(t, got.Incident.Number, joined.Incident.Number)
	assert.Len(t, current(r.get(got.Incident.Number)), 6)
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseAdded}, r.announce.of(d.ID),
		"joining an active Incident is case_added and no state fact")
}

func TestTheFirstCorrelatorInOrderWinsAndTheSecondDoesNothing(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	// Written second-first, so the order is the operator's priority and not age.
	second := r.write("second", 20, domain.Count{}, eq("severity", "critical"))
	first := r.write("first", 10, domain.Count{}, eq("severity", "critical"))

	c := r.openCase("HighErrorRate")
	got := r.correlate(c)
	require.Equal(t, VerdictDrew, got.Verdict)
	assert.Equal(t, first.ID, got.CorrelatorID)
	assert.Equal(t, first.ID, r.get(got.Incident.Number).DrawnBy.CorrelatorID())

	_, drew, err := repository.NewCorrelatorRepository(r.h.Pool).LatestDrawn(context.Background(), r.scope, second.ID)
	require.NoError(t, err)
	assert.False(t, drew, "the second matching Correlator draws nothing")

	// ⭐ AND IT STAYS OUT EVEN WHEN THE FIRST DECLINES TO DRAW. Quiet the first's
	// Incident so there is nothing to join, and give it a count it cannot meet: it
	// still claims the Case (policy semantics: matchers decide, the count only holds
	// back), so the second is never asked.
	r.closeCase(c)
	five := domain.Count{Min: 5, Window: time.Hour}
	_, err = r.k.Update(context.Background(), r.scope, first.ID, domain.CorrelatorPatch{Count: &five})
	require.NoError(t, err)
	r.h.Advance(time.Minute)
	held := r.correlate(r.openCase("DiskFull"))
	assert.Equal(t, VerdictBelowCount, held.Verdict)
	assert.Equal(t, first.ID, held.CorrelatorID)
	_, drew, err = repository.NewCorrelatorRepository(r.h.Pool).LatestDrawn(context.Background(), r.scope, second.ID)
	require.NoError(t, err)
	assert.False(t, drew)
}

// --------------------------------------------------------- what is skipped

func TestACaseAlreadyInAnIncidentIsSkippedByEveryCorrelator(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	r.write("criticals", 100, domain.Count{}, eq("severity", "critical"))

	c := r.openCase("HighErrorRate")
	human, err := r.svc.Draw(context.Background(), r.scope, []uuid.UUID{c}, r.alice)
	require.NoError(t, err)

	assert.Equal(t, VerdictUnclaimable, r.correlate(c).Verdict)
	assert.Equal(t, 1, r.get(human.Number).MemberCount)
	assert.Equal(t, []domain.Fact{domain.FactDrawn}, r.announce.of(human.ID))
}

func TestAHumanDrawnIncidentNeverGainsACaseFromACorrelator(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	r.write("criticals", 100, domain.Count{}, eq("severity", "critical"))

	// The human's Incident is active and every Case in it matches the Correlator.
	human, err := r.svc.Draw(context.Background(), r.scope, []uuid.UUID{r.openCase("HighErrorRate")}, r.alice)
	require.NoError(t, err)

	got := r.correlate(r.openCase("KubePodCrashLooping"))
	require.Equal(t, VerdictDrew, got.Verdict, "it draws its own rather than growing the human's")
	assert.NotEqual(t, human.Number, got.Incident.Number)
	assert.Equal(t, 1, r.get(human.Number).MemberCount, "the human-drawn Incident did not grow by itself")
}

func TestACaseAHumanRemovedOrMovedIsNeverReAddedByACorrelator(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	ctx := context.Background()
	r.write("criticals", 100, domain.Count{}, eq("severity", "critical"))

	a := r.openCase("HighErrorRate")
	drawn := r.correlate(a)
	require.Equal(t, VerdictDrew, drawn.Verdict)
	b := r.openCase("KubePodCrashLooping")
	require.Equal(t, VerdictJoined, r.correlate(b).Verdict)

	// Removed: a human takes A out. A redelivered or re-run evaluation of A leaves
	// it out.
	_, err := r.svc.Remove(ctx, r.scope, drawn.Incident.Number, a, r.alice)
	require.NoError(t, err)
	assert.Equal(t, VerdictUnclaimable, r.correlate(a).Verdict)

	// Moved, then removed from where it went: B carries two tombstones and no live
	// membership, and is still never put back.
	other, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{r.openCase("DiskFull")}, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Move(ctx, r.scope, drawn.Incident.Number, other.Number, b, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Remove(ctx, r.scope, other.Number, b, r.alice)
	require.NoError(t, err)
	assert.Equal(t, VerdictUnclaimable, r.correlate(b).Verdict)

	assert.Empty(t, current(r.get(drawn.Incident.Number)), "neither Case went back")
}

func TestACaseAHumanRemovedDoesNotCountTowardsANewStory(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	ctx := context.Background()
	r.write("payments storm", 100, domain.Count{Min: 5, Window: 600 * time.Second},
		eq("cluster", "prod"), eq("namespace", "payments"))

	var claimed []uuid.UUID
	for i := 0; i < 4; i++ {
		c := r.openLabelled(payments(i))
		claimed = append(claimed, c)
		require.Equal(t, VerdictBelowCount, r.correlate(c).Verdict)
	}
	// A human puts the second Case in a story of their own and then takes it out.
	h, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{claimed[1]}, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Remove(ctx, r.scope, h.Number, claimed[1], r.alice)
	require.NoError(t, err)

	// The fifth matching Case now finds only three free claims beside it.
	assert.Equal(t, VerdictBelowCount, r.correlate(r.openLabelled(payments(4))).Verdict)
}

// ------------------------------------------------- quiet grace (git-bug 34a27c5)

// endCase closes a Case at exactly `at`, the way the lifecycle does.
func (r *correlatorRig) endCase(id uuid.UUID, at time.Time) {
	r.t.Helper()
	r.h.Exec(`UPDATE alert_cases
	             SET state = 'closed', ended_at = $2, resolve_reason = 'upstream'
	           WHERE id = $1`, id, at)
}

// flap opens one firing of the disk alert at the harness clock's instant. Each
// firing is its own Alert here, which is what the Correlator sees either way: a
// fresh Case whose labels match.
func (r *correlatorRig) flap(i int) uuid.UUID {
	r.t.Helper()
	return r.openLabelled(map[string]string{
		"alertname": "NodeDiskPressure" + string(rune('A'+i)), "severity": "critical",
	})
}

func TestAReFireInsideTheGraceJoinsTheQuietIncidentAndOneOutsideDrawsANewOne(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	ctx := context.Background()
	k := r.write("disk", 100, domain.Count{}, eq("severity", "critical"))
	grace := 1800 * time.Second
	_, err := r.k.Update(ctx, r.scope, k.ID, domain.CorrelatorPatch{QuietGrace: &grace})
	require.NoError(t, err)

	first := r.flap(0)
	drawn := r.correlate(first)
	require.Equal(t, VerdictDrew, drawn.Verdict)

	// The firing ends: the Incident is quiet.
	r.h.Advance(5 * time.Minute)
	quietAt := r.h.Now()
	r.endCase(first, quietAt)
	require.Equal(t, domain.StateQuiet, r.get(drawn.Incident.Number).State())

	// Twenty minutes later it fires again: it joins, and the Incident reads active.
	r.h.Advance(20 * time.Minute)
	again := r.flap(1)
	joined := r.correlate(again)
	require.Equal(t, VerdictJoined, joined.Verdict, "a re-fire 20 min after quiet is the same story")
	assert.Equal(t, drawn.Incident.Number, joined.Incident.Number)
	d := r.get(drawn.Incident.Number)
	assert.Equal(t, domain.StateActive, d.State(), "it reads active again")
	assert.Equal(t, []domain.Fact{domain.FactDrawn, domain.FactCaseAdded, domain.FactActiveAgain},
		r.announce.of(d.ID), "joining a quiet Incident declares case_added and active_again")

	// It ends again, and the next firing comes forty minutes later: a new Incident.
	r.h.Advance(5 * time.Minute)
	r.endCase(again, r.h.Now())
	r.h.Advance(40 * time.Minute)
	late := r.correlate(r.flap(2))
	require.Equal(t, VerdictDrew, late.Verdict, "a re-fire 40 min after quiet is past the grace")
	assert.NotEqual(t, drawn.Incident.Number, late.Incident.Number)
	assert.Len(t, current(r.get(drawn.Incident.Number)), 2, "the old story kept its two firings")
}

func TestTheGraceBoundaryIsInclusiveToTheSecond(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		after time.Duration
		want  Verdict
	}{
		{"exactly the grace", 1800 * time.Second, VerdictJoined},
		{"one second past it", 1801 * time.Second, VerdictDrew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCorrelatorRig(t)
			ctx := context.Background()
			k := r.write("disk", 100, domain.Count{}, eq("severity", "critical"))
			grace := 1800 * time.Second
			_, err := r.k.Update(ctx, r.scope, k.ID, domain.CorrelatorPatch{QuietGrace: &grace})
			require.NoError(t, err)

			first := r.flap(0)
			require.Equal(t, VerdictDrew, r.correlate(first).Verdict)
			r.h.Advance(time.Minute)
			quietAt := r.h.Now()
			r.endCase(first, quietAt)

			r.h.Advance(tc.after)
			assert.Equal(t, tc.want, r.correlate(r.flap(1)).Verdict)
		})
	}
}

func TestWithNoGraceAReFireAfterQuietDrawsANewIncident(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	r.write("disk", 100, domain.Count{}, eq("severity", "critical"))

	first := r.flap(0)
	drawn := r.correlate(first)
	require.Equal(t, VerdictDrew, drawn.Verdict)
	r.h.Advance(time.Minute)
	r.endCase(first, r.h.Now())

	r.h.Advance(time.Minute)
	again := r.correlate(r.flap(1))
	assert.Equal(t, VerdictDrew, again.Verdict, "no grace: a quiet Incident is never taken back")
	assert.NotEqual(t, drawn.Incident.Number, again.Incident.Number)
}

func TestAHumanDrawnQuietIncidentNeverTakesBackAReFire(t *testing.T) {
	t.Parallel()
	r := newCorrelatorRig(t)
	ctx := context.Background()
	k := r.write("disk", 100, domain.Count{}, eq("severity", "critical"))
	grace := 24 * time.Hour
	_, err := r.k.Update(ctx, r.scope, k.ID, domain.CorrelatorPatch{QuietGrace: &grace})
	require.NoError(t, err)

	c := r.flap(0)
	human, err := r.svc.Draw(ctx, r.scope, []uuid.UUID{c}, r.alice)
	require.NoError(t, err)
	r.h.Advance(time.Minute)
	r.endCase(c, r.h.Now())

	r.h.Advance(time.Minute)
	got := r.correlate(r.flap(1))
	require.Equal(t, VerdictDrew, got.Verdict, "a grace never reaches an Incident the Correlator did not draw")
	assert.Equal(t, 1, r.get(human.Number).MemberCount)
}
