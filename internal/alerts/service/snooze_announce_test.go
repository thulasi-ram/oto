package service

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// claimedService is `queuedService` plus the claim store, which is what a KEYED
// human verb needs: `idempotency.Require` REFUSES a keyed request on a deployment
// where the store is nil, so a replay cannot be exercised without it.
func (f *fixture) claimedService(enq db.Enqueuer) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:      repository.NewCaseRepository(f.pool),
		Events:     repository.NewEventRepository(f.pool, f.clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:   repository.NewCaseRepository(f.pool),
		Enqueuer:   enq,
		Claims:     idempotency.NewRepository(f.pool),
		Clock:      f.clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

// activeSnoozeID reads the one live snooze for an alert.
func (f *fixture) activeSnoozeID(t *testing.T, alertID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM alert_snoozes
		  WHERE org_id = $1 AND alert_id = $2 AND ended_at IS NULL`,
		f.orgID, alertID).Scan(&id); err != nil {
		t.Fatalf("read the active snooze: %v", err)
	}
	return id
}

// ⭐⭐ TestASecondSnoozeAnnouncesItselfWithItsOwnOccasion.
//
// The promise: an operator who snoozes for 1h and then for 4h gets TWO
// announcements, and the second one names the snooze it is about.
//
// What broke: the §C.7 idempotency key is
// (org, subject_kind, subject_id, reason, state_version), and a re-snooze holds
// all five constant. `subject_id` is the alert either way, the reason is
// `snoozed` either way, and `state_version` is `alert_cases.state_version` — the
// CASE's optimistic lock, which a snooze cannot move, because `StartSnooze` takes
// an Alert and §B.8 is emphatic that a snooze is neither a state nor a
// suppression. So the second evaluation minted a byte-identical key,
// `notifications_idem_uniq` swallowed it, and the channel kept showing the FIRST
// quiet period while the row said something else. The data was never wrong; the
// announcement was simply absent, which is the silence §B.6 forbids.
//
// The occasion is the `alert_snoozes.id`, so "which snooze" is part of the fact.
func TestASecondSnoozeAnnouncesItselfWithItsOwnOccasion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()
	actor := f.testActor(t)

	enq := &recordingEnqueuer{}
	svc := f.claimedService(enq)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())

	if _, _, err := svc.Snooze(ctx, f.scope, alertID, actor,
		now.Add(1*time.Hour), "deploy window", Idempotency{}); err != nil {
		t.Fatalf("first snooze: %v", err)
	}
	firstSnooze := f.activeSnoozeID(t, alertID)

	// Thinking better of it: the same alert, the same actor, a longer quiet period.
	if _, _, err := svc.Snooze(ctx, f.scope, alertID, actor,
		now.Add(4*time.Hour), "longer than I thought", Idempotency{}); err != nil {
		t.Fatalf("second snooze: %v", err)
	}
	secondSnooze := f.activeSnoozeID(t, alertID)

	if firstSnooze == secondSnooze {
		t.Fatalf("the incumbent was not superseded: one row served both snoozes")
	}

	got := enq.notifyEvaluations(reasonSnoozed)
	if len(got) != 2 {
		t.Fatalf("%d snoozed evaluations enqueued, want 2 — a re-snooze is a fact and "+
			"has to be announced", len(got))
	}
	if got[0].OccasionID != firstSnooze {
		t.Fatalf("the first announcement names occasion %s, want the snooze it announced (%s)",
			got[0].OccasionID, firstSnooze)
	}
	if got[1].OccasionID != secondSnooze {
		t.Fatalf("the second announcement names occasion %s, want the NEW snooze (%s)",
			got[1].OccasionID, secondSnooze)
	}
	if got[0].OccasionID == got[1].OccasionID {
		t.Fatal("both announcements named one occasion, so they still hash to one §C.7 key")
	}
	if got[0].StateVersion != got[1].StateVersion {
		t.Fatalf("the state versions differ (%d, %d) — the occasion is supposed to be the "+
			"discriminator here, because a snooze moves no Case lock",
			got[0].StateVersion, got[1].StateVersion)
	}
}

// TestAnUnsnoozeNamesTheSnoozeItEnded is the same defect on the other verb, and it
// is why the occasion is general rather than a `snooze_id` bolted onto one reason.
//
// snooze → wake → snooze → wake inside ONE episode produced two `unsnoozed` facts
// at the same `state_version` about the same alert, and the second was swallowed
// exactly as the second `snoozed` was: the channel was told the alert had woken
// once, and never told about the second wake-up at all.
func TestAnUnsnoozeNamesTheSnoozeItEnded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()
	actor := f.testActor(t)

	enq := &recordingEnqueuer{}
	svc := f.claimedService(enq)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())

	var ended []uuid.UUID
	for i := range 2 {
		if _, _, err := svc.Snooze(ctx, f.scope, alertID, actor,
			now.Add(time.Duration(i+1)*time.Hour), "", Idempotency{}); err != nil {
			t.Fatalf("snooze %d: %v", i, err)
		}
		ended = append(ended, f.activeSnoozeID(t, alertID))
		if _, err := svc.Unsnooze(ctx, f.scope, alertID, actor, ""); err != nil {
			t.Fatalf("unsnooze %d: %v", i, err)
		}
	}

	got := enq.notifyEvaluations(reasonUnsnoozed)
	if len(got) != 2 {
		t.Fatalf("%d unsnoozed evaluations enqueued, want 2", len(got))
	}
	for i, args := range got {
		if args.OccasionID != ended[i] {
			t.Fatalf("wake-up %d names occasion %s, want the snooze it ended (%s)",
				i, args.OccasionID, ended[i])
		}
	}
}

// ⭐ TestARetriedPressStillAnnouncesOnce is the invariant the occasion must NOT
// break, and it is a different mechanism from the one above.
//
// A Slack snooze press carries an idempotency key oto mints for itself from the
// interaction's `response_url` (§B.8.3). It guards ONE PRESS BEING APPLIED TWICE:
// the worker commits the snooze, dies before River marks the job done, and the
// rescued job replays — superseding the very snooze that press had just created.
// The replay's transaction is ROLLED BACK, so no `notify.evaluate` is enqueued and
// there is no second card.
//
// The occasion does not participate in that guard and must not weaken it: one
// press, one snooze row, one announcement, whatever the queue does.
func TestARetriedPressStillAnnouncesOnce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()
	actor := f.testActor(t)

	enq := &recordingEnqueuer{}
	svc := f.claimedService(enq)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())

	op, err := idempotency.NewOperation("snoozeAlert")
	if err != nil {
		t.Fatalf("operation: %v", err)
	}
	key, err := idempotency.NewKey("the-same-press")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	press := Idempotency{
		Keyed:     true,
		Key:       key,
		Operation: op,
		Principal: authn.Principal{
			Kind:   authn.KindSession,
			UserID: uuid.MustParse(actor.ID()),
		},
		RequestHash: idempotency.HashTargetedRequest(alertID, nil),
	}

	first, replayed, err := svc.Snooze(ctx, f.scope, alertID, actor,
		now.Add(1*time.Hour), "one press", press)
	if err != nil {
		t.Fatalf("the press: %v", err)
	}
	if replayed {
		t.Fatal("the first press is not a replay of anything")
	}

	again, replayed, err := svc.Snooze(ctx, f.scope, alertID, actor,
		now.Add(1*time.Hour), "one press", press)
	if err != nil {
		t.Fatalf("the rescued job: %v", err)
	}
	if !replayed {
		t.Fatal("the second application of one press must be recognised as a replay")
	}
	if again.ID() != first.ID() {
		t.Fatalf("the replay handed back snooze %s, want the one the press granted (%s)",
			again.ID(), first.ID())
	}

	if got := enq.notifyEvaluations(reasonSnoozed); len(got) != 1 {
		t.Fatalf("%d snoozed evaluations enqueued for ONE press, want 1: a replayed press "+
			"must not produce a second card", len(got))
	}
	if n := f.activeSnoozeCount(t); n != 1 {
		t.Fatalf("%d active snoozes after one press, want 1", n)
	}
}

// TestAFiredEvaluationNamesNoOccasion is the additive half of the change.
//
// The occasion is written into the §C.7 pre-image ONLY when it is non-nil, so
// every reason that does not name one keeps the exact key it had. This asserts the
// wire rather than the hash — the hash is pinned in `alerts/domain`'s golden
// pre-image test — because the way to re-key `fired` by accident is to set this
// field in the batch loop for every request instead of for the snooze path.
func TestAFiredEvaluationNamesNoOccasion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)

	enq := &recordingEnqueuer{}
	svc := f.queuedService(enq)

	obs := f.observation(domain.ObservedByIngest, "firing", now, now.Add(-time.Minute), time.Time{})
	if _, err := svc.ObserveBatch(t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		t.Fatalf("open the alert: %v", err)
	}

	got := enq.notifyEvaluations(reasonFired)
	if len(got) == 0 {
		t.Fatal("a firing alert must enqueue its own announcement")
	}
	for _, args := range got {
		if args.OccasionID != uuid.Nil {
			t.Fatalf("a fired evaluation named occasion %s; every reason but the two snooze "+
				"ones must leave it zero, or its idempotency key moves", args.OccasionID)
		}
	}
}

// namedPress is what `app.slackIdempotency` builds for a Slack presser with NO
// linked oto account: no claim, because `idempotency_claims.principal_id` is NOT
// NULL and such a person has no principal uuid to claim under, and a NAME, derived
// from the interaction's already-hashed `response_url`.
func namedPress(t *testing.T, interaction string) Idempotency {
	t.Helper()
	k, err := idempotency.NewKey(interaction)
	if err != nil {
		t.Fatalf("interaction key: %v", err)
	}
	idem := Idempotency{KeyID: k.ID()}
	if idem.Keyed {
		t.Fatal("an unlinked presser is unclaimable; the unkeyed path is what is under test")
	}
	if idem.KeyID == uuid.Nil {
		t.Fatal("a named interaction must never reduce to uuid.Nil: that is the " +
			"\"no occasion\" sentinel, and it would re-collide the two snooze reasons")
	}
	return idem
}

// ⭐⭐ TestARedeliveredPressByAnUnlinkedMemberAnnouncesOnce is the regression the
// occasion introduced, and the reason `Idempotency.KeyID` exists.
//
// An unlinked Slack member is a FIRST-CLASS state, not an edge case: requiring a
// link before oto accepts a button press would silently lose the acknowledgements
// of everyone who has not been onboarded (`identity/domain.SlackIdentity`).
//
// ⛔⛔ THE PRESS THIS TEST DESCRIBES IS NO LONGER THE UNLINKED MEMBER'S NORMAL PATH,
// AND THE TEST IS KEPT BECAUSE THE PATH IS. git-bug a74d6b2 settled the question
// migration 00041 left open — where a Slack principal's uuid comes from — by minting
// a SHADOW MEMBER on first press (`identity/service.ResolveSlackPresser`, migration
// 00074), so an unlinked presser now HAS a principal, `app.slackIdempotency` returns
// a KEYED intent, and a redelivery is refused before it can execute. What still
// arrives here UNKEYED BUT NAMED is the DEGRADED press: the identity write or read
// failed, or the link points at a user row that is gone, and
// `channels/service.actor` fell back to the raw Slack member id rather than let a
// directory lookup cost an acknowledgement. That fallback is deliberate and
// permanent, so this layer's behaviour on an unclaimable-but-named intent is a live
// contract and not a historical curiosity.
//
// Such a press cannot be claimed — `idempotency_claims.principal_id` is NOT NULL and
// `Claim.validate` refuses `uuid.Nil` — so a redelivered interaction genuinely runs
// `Snooze` a SECOND time: the incumbent is superseded, a new row is inserted, and a
// second `notify.evaluate` is enqueued. That the CARD nevertheless converges is what
// this test is about; that the ACT converges for the normal path is
// `internal/app`'s `TestARedeliveredPressByAnUnlinkedMemberIsAppliedOnce`, which
// needs a claim store and a database and therefore cannot live here.
//
// ⛔ THAT USED TO BE HARMLESS BY ACCIDENT AND THE OCCASION BROKE IT. The second
// intent hashed byte-identically — nothing in the §C.7 key distinguished two
// snoozes at all — so `notifications_idem_uniq` swallowed it, and the key was
// silently doing double duty as a duplicate-CARD guard on the unclaimed path. Once
// the occasion became the freshly minted `alert_snoozes.id`, the redelivery minted
// a fresh key and the channel got a SECOND "snoozed for 1h" amendment for one human
// press. Keying the announcement on the INTERACTION instead restores the guard
// deliberately: the occasion is stable across a redelivery of one interaction and
// different for the next one.
//
// The two halves are asserted together because either alone is satisfiable by a
// trivial wrong answer — a constant occasion converges the redelivery and silences
// the re-snooze, a fresh one announces the re-snooze and duplicates the redelivery.
// This layer's claim is about the OCCASION; that one occasion means one card is
// `notification/service`'s own `TestARetriedPressProducesOneCard`, which pins the
// `notifications_idem_uniq` half at the layer that owns the table.
func TestARedeliveredPressByAnUnlinkedMemberAnnouncesOnce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newFixture(t, now)
	ctx := t.Context()

	// No claim store: an unkeyed intent must not need one, and wiring one here would
	// hide a `KeyID` that had accidentally started claiming something.
	enq := &recordingEnqueuer{}
	svc := f.queuedService(enq)

	f.openFiring(now.Add(-10*time.Minute), time.Time{})
	alertID := f.alertIDByKey(t, f.alertKey.String())

	press := namedPress(t, "slack:0000000000000000000000000000000000000000000000000000000000000001")

	// The press. `U024BE7LH` is a Slack member id and not a uuid, which is precisely
	// why nothing can be claimed for it — and since a74d6b2 that is what `actor()`
	// reports only when the identity lookup FAILED, the shadow member having removed
	// it from the ordinary path.
	replayed, err := svc.SnoozeAs(ctx, f.scope, alertID, "slack", "U024BE7LH", "@ada",
		now.Add(1*time.Hour), "", press)
	if err != nil {
		t.Fatalf("the press: %v", err)
	}
	if replayed {
		t.Fatal("the first press is not a replay of anything")
	}
	firstRow := f.activeSnoozeID(t, alertID)

	// Slack did not get its 200 in time and sent the SAME interaction again.
	replayed, err = svc.SnoozeAs(ctx, f.scope, alertID, "slack", "U024BE7LH", "@ada",
		now.Add(1*time.Hour), "", press)
	if err != nil {
		t.Fatalf("the redelivery: %v", err)
	}
	if replayed {
		t.Fatal("nothing claimed this press, so nothing can report it as a replay — which " +
			"is exactly why the ANNOUNCEMENT has to converge on its own")
	}
	secondRow := f.activeSnoozeID(t, alertID)

	if firstRow == secondRow {
		t.Fatal("the redelivery did not re-execute the snooze, so this test proves nothing: " +
			"the unclaimed press being APPLIED twice is the whole premise")
	}

	got := enq.notifyEvaluations(reasonSnoozed)
	if len(got) != 2 {
		t.Fatalf("%d snoozed evaluations enqueued, want 2 — one per execution, which is what "+
			"an unclaimed redelivery does", len(got))
	}
	if got[0].OccasionID != got[1].OccasionID {
		t.Fatalf("one press named two occasions (%s, %s), so it mints two §C.7 keys and the "+
			"channel shows two amendments for one human gesture",
			got[0].OccasionID, got[1].OccasionID)
	}
	if got[0].OccasionID != press.KeyID {
		t.Fatalf("the occasion is %s, want the interaction's own id (%s)",
			got[0].OccasionID, press.KeyID)
	}
	if got[0].OccasionID == firstRow || got[1].OccasionID == secondRow {
		t.Fatal("the occasion is a snooze row, which is minted per EXECUTION: that is the " +
			"regression, because a redelivery executes a second time")
	}
	if got[0].StateVersion != got[1].StateVersion {
		t.Fatalf("the state versions differ (%d, %d) — the occasion is the only discriminator "+
			"here, because a snooze moves no Case lock",
			got[0].StateVersion, got[1].StateVersion)
	}

	// ⭐ AND THE OTHER HALF, ON THE SAME ALERT AND THE SAME PERSON: a genuine second
	// gesture. Slack mints a new `response_url` per interaction, so this is a
	// different name — and a re-snooze from 1h to 4h is a fact the channel is owed.
	again := namedPress(t, "slack:0000000000000000000000000000000000000000000000000000000000000002")
	if again.KeyID == press.KeyID {
		t.Fatal("two different interactions reduced to one id; two presses would then be " +
			"one announcement, which is the defect the occasion was added to fix")
	}
	if _, err := svc.SnoozeAs(ctx, f.scope, alertID, "slack", "U024BE7LH", "@ada",
		now.Add(4*time.Hour), "longer than I thought", again); err != nil {
		t.Fatalf("the second press: %v", err)
	}

	got = enq.notifyEvaluations(reasonSnoozed)
	if len(got) != 3 {
		t.Fatalf("%d snoozed evaluations enqueued, want 3", len(got))
	}
	if got[2].OccasionID != again.KeyID {
		t.Fatalf("the second press named occasion %s, want its own interaction (%s)",
			got[2].OccasionID, again.KeyID)
	}
	if got[2].OccasionID == got[1].OccasionID {
		t.Fatal("the second press reused the first press's occasion, so its §C.7 key is " +
			"byte-identical and the operator's 4h is never announced")
	}
}
