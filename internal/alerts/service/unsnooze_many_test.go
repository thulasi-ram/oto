package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/test/harness"
)

// THE BULK WAKE, OVER REAL SQL.
//
// `UnsnoozeMany` is a fan-out of `Unsnooze`, so what needs proving is not the
// primitive — the single-alert tests own that — but the CLASSIFICATION: which
// outcomes are skips, which code each skip carries, and that a skip really did
// write nothing while its neighbours really did. Every one of those answers comes
// out of Postgres (`alert_snoozes_active_idx`, the tenant predicate on every read),
// so a fake would agree with whatever the code does and prove nothing.

// otherLabels is a second label set, so a fixture that ships with one alert can
// have two. It differs in `service`, which is a promoted label and therefore part
// of the §C.2 alert key.
func otherLabels() map[string]string {
	return map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
		"service":   "payments",
	}
}

// secondAlert opens a firing case for a SECOND alert in the same org and cluster,
// through the real ingest path, and returns its alert id.
func (f *fixture) secondAlert(t *testing.T, startsAt time.Time) uuid.UUID {
	t.Helper()

	labels := harness.Labels(t, otherLabels())
	obs := f.observation(domain.ObservedByIngest, "firing", f.clk.Now(), startsAt, time.Time{})
	obs.Labels = labels
	obs.AlertKey = harness.AlertKey(f.orgID, f.clusterKey, labels)
	obs.SourceFingerprint = domain.ComputeSourceFingerprint(labels)

	if _, err := f.svc.ObserveBatch(t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		t.Fatalf("open the second alert: %v", err)
	}
	return f.alertIDByKey(t, obs.AlertKey.String())
}

// alertIDByKey reads one alert id out of the database by its §C.2 key.
func (f *fixture) alertIDByKey(t *testing.T, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM alerts WHERE org_id = $1 AND alert_key = $2`, f.orgID, key).Scan(&id); err != nil {
		t.Fatalf("read alert %s: %v", key, err)
	}
	return id
}

// activeSnoozeCount counts the quiet periods still in force for this org.
func (f *fixture) activeSnoozeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM alert_snoozes WHERE org_id = $1 AND ended_at IS NULL`,
		f.orgID).Scan(&n); err != nil {
		t.Fatalf("count active snoozes: %v", err)
	}
	return n
}

// testActor is the human every verb in this file is attributed to.
//
// ⛔ IT IS A REAL SEEDED USER AND NOT A MINTED UUID. `alert_snoozes.snoozed_by` is
// a foreign key into `users`, so a snooze attributed to an id nobody owns is
// refused by the database — which is exactly the guarantee that makes a quiet
// period attributable after the fact.
func (f *fixture) testActor(t *testing.T) domain.Actor {
	t.Helper()
	u := f.h.User(f.org)
	a, err := domain.NewActor(domain.ActorUser, u.ID.String(), u.DisplayName)
	if err != nil {
		t.Fatalf("build actor: %v", err)
	}
	return a
}

// ⭐⭐ TestABulkWakeSkipsWhatItCannotWakeAndSaysWhy.
//
// The promise: one call over three ids — one snoozed, one awake, one belonging to
// nobody — wakes the first, SKIPS the other two, and names a different code for
// each skip. Nothing about the request fails.
//
// What broke: refusing the whole request because one alert had already woken makes
// the button unusable in exactly the situation it exists for — an operator waking a
// page of quiet alerts will routinely find some of them already awake. And a skip
// that could not say WHICH skip it was leaves the surface with "nothing happened",
// which has two completely different meanings here.
func TestABulkWakeSkipsWhatItCannotWakeAndSaysWhy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()
	actor := f.testActor(t)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	snoozed := f.alertIDByKey(t, f.alertKey.String())
	awake := f.secondAlert(t, now.Add(-9*time.Minute))
	// Syntactically perfect and belonging to nobody — the same shape another
	// tenant's id has, because a tenant-scoped read cannot tell them apart.
	stranger := uuid.New()

	if _, _, err := f.svc.Snooze(ctx, f.scope, snoozed, actor,
		now.Add(2*time.Hour), "deploy window", Idempotency{}); err != nil {
		t.Fatalf("snooze the first alert: %v", err)
	}
	if n := f.activeSnoozeCount(t); n != 1 {
		t.Fatalf("%d active snoozes before the wake, want 1", n)
	}

	res, err := f.svc.UnsnoozeMany(ctx, f.scope,
		[]uuid.UUID{snoozed, awake, stranger}, actor, "deploy finished early")
	if err != nil {
		t.Fatalf("UnsnoozeMany: %v", err)
	}

	if len(res.Outcomes) != 3 {
		t.Fatalf("%d outcomes, want one per requested id: %+v", len(res.Outcomes), res.Outcomes)
	}
	if res.Woken() != 1 || res.Skipped() != 2 {
		t.Fatalf("woken=%d skipped=%d, want 1 and 2", res.Woken(), res.Skipped())
	}

	// ⛔ ORDER AND IDENTITY ARE PART OF THE ACCOUNT. A caller renders it against the
	// rows it ticked, so an account it has to re-join by id is one that will be
	// re-joined wrongly.
	want := []struct {
		id   uuid.UUID
		woke bool
		code string
	}{
		{snoozed, true, ""},
		{awake, false, "not_snoozed"},
		{stranger, false, "alert_not_found"},
	}
	for i, w := range want {
		got := res.Outcomes[i]
		if got.AlertID != w.id {
			t.Errorf("outcomes[%d].AlertID = %s, want %s", i, got.AlertID, w.id)
		}
		if got.Woken != w.woke {
			t.Errorf("outcomes[%d].Woken = %v, want %v", i, got.Woken, w.woke)
		}
		if got.Code != w.code {
			t.Errorf("outcomes[%d].Code = %q, want %q", i, got.Code, w.code)
		}
	}

	// The wake is real, and the skips wrote nothing: one snooze closed, one
	// `alert.unsnoozed` event, no quiet period left standing.
	if n := f.activeSnoozeCount(t); n != 0 {
		t.Errorf("%d active snoozes after the wake, want 0", n)
	}
	if n := f.countEvents("alert.unsnoozed"); n != 1 {
		t.Errorf("%d `alert.unsnoozed` events, want exactly 1 — a skip must append nothing", n)
	}

	var reason string
	if err := f.pool.QueryRow(ctx,
		`SELECT ended_reason FROM alert_snoozes WHERE org_id = $1 AND alert_id = $2`,
		f.orgID, snoozed).Scan(&reason); err != nil {
		t.Fatalf("read the ended snooze: %v", err)
	}
	if reason != "manual" {
		t.Errorf("ended_reason = %q, want manual — a human woke this, not the clock", reason)
	}
}

// ⭐ TestABulkWakeIsSafeToRepeat.
//
// The promise: running the same call twice leaves the same state and the second
// answer says so — every id reports `not_snoozed`.
//
// What broke: this is WHY the endpoint claims no `Idempotency-Key`. Waking is a
// compare-and-set, so a retry after a dropped response finishes the job rather
// than doing it again — unlike snooze, which supersedes its own incumbent, and
// unlike comment, which appends. A second `alert.unsnoozed` event on the timeline
// that IS the record would be the same defect ticket a6cc834 fixed for those two.
func TestABulkWakeIsSafeToRepeat(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()
	actor := f.testActor(t)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())
	if _, _, err := f.svc.Snooze(ctx, f.scope, alertID, actor,
		now.Add(time.Hour), "", Idempotency{}); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	ids := []uuid.UUID{alertID}
	if _, err := f.svc.UnsnoozeMany(ctx, f.scope, ids, actor, ""); err != nil {
		t.Fatalf("first wake: %v", err)
	}
	res, err := f.svc.UnsnoozeMany(ctx, f.scope, ids, actor, "")
	if err != nil {
		t.Fatalf("second wake: %v", err)
	}

	if res.Woken() != 0 || res.Outcomes[0].Code != "not_snoozed" {
		t.Fatalf("the replay reported %+v; a second pass must find the alert already awake",
			res.Outcomes)
	}
	if n := f.countEvents("alert.unsnoozed"); n != 1 {
		t.Errorf("%d `alert.unsnoozed` events after two calls, want 1", n)
	}
}

// ⛔ TestABulkWakeRefusesANonHumanActor.
//
// The promise: the service itself refuses a non-human actor, before any id is
// touched. The HTTP layer refuses it too; both do, because a wake-up appends an
// attributed event per alert and a hundred entries signed by nobody is worse than
// one.
func TestABulkWakeRefusesANonHumanActor(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())

	if _, err := f.svc.UnsnoozeMany(t.Context(), f.scope,
		[]uuid.UUID{alertID}, domain.Actor{}, ""); err == nil {
		t.Fatal("a zero actor woke alerts in bulk")
	}
}
