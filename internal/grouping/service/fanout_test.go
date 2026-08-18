package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

// The group fan-out on real Postgres.
//
// ⭐ WHY THESE THREE AND NOT MORE. A fan-out is one write transaction per member,
// which makes it a shape with exactly three ways to be wrong that unit tests
// cannot see: it can do UNBOUNDED work, it can lose the record of what it did
// when it fails partway, and it can apply the same verb twice. Each of those is a
// claim about what committed, so each needs a database.
//
// `joinmany_test.go` next door counts work through fake ports and needs no
// database, and that is the right test for THAT question. This file is about
// what survives a commit.
//
// ⚠️ THE THIRD ONE IS ANSWERED PER VERB AND THE ANSWERS DIFFER. Ack refuses a
// second pass in the domain, so its retry test asserts idempotence. A comment
// cannot refuse anything — it is an append — so its retry test asserts the
// DUPLICATION that actually happens. Both are pinned here, because a gap nobody
// has written down is a gap somebody will one day claim is closed.

func TestMain(m *testing.M) { harness.Main(m) }

// ---------------------------------------------------------------- the world

type fanOutWorld struct {
	h     *harness.H
	org   harness.Org
	group harness.Group
	// alerts is the actual alerts service, wired to the same database. The fan-out
	// is only interesting when the member verb is the real one: `already_acked`
	// has to come from the domain, not from a fake that agreed to say it.
	alerts *alerts.Service

	actorID    string
	actorLabel string

	cluster harness.Cluster
	source  harness.Source
	seq     int
}

func newFanOutWorld(t *testing.T) *fanOutWorld {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)
	user := h.User(org)

	alerts, err := alerts.New(alerts.Deps{
		Alerts:  alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		Cases:   alertsrepo.NewCaseRepository(h.Pool),
		Events:  alertsrepo.NewEventRepository(h.Pool, h.Clock),
		Snoozes: alertsrepo.NewSnoozeRepository(h.Pool, h.Clock),
		Tx:      alertsrepo.NewTxRunner(h.Pool),
		Clock:   h.Clock,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("alerts service: %v", err)
	}

	return &fanOutWorld{
		h: h, org: org, group: group, alerts: alerts,
		actorID: user.ID.String(), actorLabel: "Ada",
		cluster: cluster, source: source,
	}
}

// grouping builds the service under test over the real repositories, with
// `actions` chosen by the test.
func (w *fanOutWorld) grouping(t *testing.T, actions MemberActions) *Service {
	t.Helper()

	svc, err := New(Deps{
		Groups:  repository.NewGroupRepository(w.h.Pool, w.h.Clock),
		Members: repository.NewMemberRepository(w.h.Pool, w.h.Clock),
		Tx:      repository.NewTxRunner(w.h.Pool),
		Actions: actions,
		Clock:   w.h.Clock,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("grouping service: %v", err)
	}
	return svc
}

// seedMembers joins n firing alerts to the generation, oldest join first, and
// returns their alert ids in that order — which is the order the fan-out reads
// them in, and therefore the order a ceiling cuts.
func (w *fanOutWorld) seedMembers(t *testing.T, n int) []uuid.UUID {
	t.Helper()

	out := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		w.seq++
		alert := w.h.AlertWith(w.org, w.cluster, map[string]string{
			"alertname": "HighErrorRate",
			"severity":  "critical",
			"instance":  "i-" + strconv.Itoa(w.seq),
		})
		// The episode IS the membership since 00051 — `h.Case` writes
		// `group_id`, and there is no second row to insert. What still has to be
		// arranged is the ORDER: the fan-out reads oldest first, and a harness that
		// starts every episode at the same instant would leave the ceiling cutting
		// an arbitrary set.
		ac := w.h.Case(alert, w.group)
		startedAt := w.h.Now().Add(time.Duration(w.seq) * time.Second)
		w.h.Exec(`UPDATE alert_cases
		             SET started_at = $2, last_observed_at = $2, source_starts_at = $2
		           WHERE id = $1`, ac.ID, startedAt)
		out = append(out, alert.ID)
	}
	return out
}

func (w *fanOutWorld) ack(t *testing.T, svc *Service) (FanOutResult, error) {
	t.Helper()
	return svc.Acknowledge(w.h.Ctx, w.org.Scope, w.group.ID,
		"user", w.actorID, w.actorLabel, "")
}

func (w *fanOutWorld) comment(t *testing.T, svc *Service, body string) (CommentResult, error) {
	t.Helper()
	return svc.Comment(w.h.Ctx, w.org.Scope, w.group.ID,
		"user", w.actorID, w.actorLabel, body, alerts.Idempotency{})
}

// ackedCases is how many of the generation's episodes carry a receipt.
func (w *fanOutWorld) ackedCases(t *testing.T) int {
	t.Helper()
	var n int
	err := w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT count(*) FROM alert_cases
		  WHERE org_id = $1 AND group_id = $2 AND ack_state = 'acked'`,
		w.org.ID, w.group.ID).Scan(&n)
	if err != nil {
		t.Fatalf("count acked cases: %v", err)
	}
	return n
}

// ackEvents is how many acknowledgement facts are on the timeline. It is the
// double-apply detector: the receipt is a projection and can be overwritten
// invisibly, but the timeline is append-only and cannot.
func (w *fanOutWorld) ackEvents(t *testing.T) int {
	t.Helper()
	return w.eventsOfType(t, kernel.EventCaseAcknowledged)
}

// commentEvents is the same detector pointed at the one group verb that has no
// refusal to protect it.
func (w *fanOutWorld) commentEvents(t *testing.T) int {
	t.Helper()
	return w.eventsOfType(t, kernel.EventCommentAdded)
}

func (w *fanOutWorld) eventsOfType(t *testing.T, typ kernel.EventType) int {
	t.Helper()
	var n int
	err := w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT count(*) FROM alert_events WHERE org_id = $1 AND type = $2`,
		w.org.ID, typ.String()).Scan(&n)
	if err != nil {
		t.Fatalf("count %s events: %v", typ, err)
	}
	return n
}

// ⚠️ THIS WORLD RUNS ON h.Clock, WHICH IS harness.Epoch, and that is now safe to
// say. It used to replace the harness clock with one derived from the wall clock,
// because `alert_events` is partitioned with NO default partition and the
// partition manager built its months around the database's `now()` — so a
// timeline append stamped at Epoch matched no range and failed with SQLSTATE
// 23514, "no partition of relation alert_events found for row". The harness
// template now builds a window around Epoch too (git-bug 6547228), so the fan-out
// tests seed, ack and append at the same instant every other harness test uses.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ------------------------------------------------------------- fake actions

// recordingActions is the member verb reduced to a tally.
//
// It is deliberately NOT the real one for the ceiling test: the question there
// is how many times the fan-out calls a member verb, and answering it with real
// transactions would measure Postgres instead.
type recordingActions struct{ acked []uuid.UUID }

func (a *recordingActions) AcknowledgeAs(
	_ context.Context, _ db.TenantScope, alertID uuid.UUID, _, _, _, _ string,
) error {
	a.acked = append(a.acked, alertID)
	return nil
}

// UnacknowledgeAs records into the same slice: the ceiling this fake exists to
// measure is a property of the fan-out and not of the verb, so the withdrawal is
// counted the same way the receipt is.
func (a *recordingActions) UnacknowledgeAs(
	_ context.Context, _ db.TenantScope, alertID uuid.UUID, _, _, _, _ string,
) error {
	a.acked = append(a.acked, alertID)
	return nil
}

func (a *recordingActions) CommentAs(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, _, _, _, _ string, _ alerts.Idempotency,
) (kernel.Event, bool, error) {
	return kernel.Event{}, false, nil
}

func (a *recordingActions) SnoozeAs(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, _, _, _ string, _ time.Time, _ string,
	_ alerts.Idempotency,
) (bool, error) {
	return false, nil
}

func (a *recordingActions) UnsnoozeAs(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, _, _, _, _ string,
) error {
	return nil
}

// failingActions applies the REAL verb until the nth member and then fails the
// way a database going away fails: an internal error, not a refusal the fan-out
// is allowed to count and carry on past.
type failingActions struct {
	inner MemberActions
	after int
	calls int
}

func (a *failingActions) AcknowledgeAs(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	a.calls++
	if a.calls > a.after {
		return errs.Internal("member_action_failed", errors.New("the connection went away"))
	}
	return a.inner.AcknowledgeAs(ctx, s, alertID, actorKind, actorID, actorLabel, note)
}

func (a *failingActions) UnacknowledgeAs(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	return a.inner.UnacknowledgeAs(ctx, s, alertID, actorKind, actorID, actorLabel, note)
}

func (a *failingActions) CommentAs(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, body string, idem alerts.Idempotency,
) (kernel.Event, bool, error) {
	return a.inner.CommentAs(ctx, s, alertID, actorKind, actorID, actorLabel, body, idem)
}

func (a *failingActions) SnoozeAs(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel string, until time.Time, note string, idem alerts.Idempotency,
) (bool, error) {
	return a.inner.SnoozeAs(ctx, s, alertID, actorKind, actorID, actorLabel, until, note, idem)
}

func (a *failingActions) UnsnoozeAs(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) error {
	return a.inner.UnsnoozeAs(ctx, s, alertID, actorKind, actorID, actorLabel, note)
}

// ------------------------------------------------------------------- tests

// TestFanOutStopsAtTheMemberCeiling is the bound itself.
//
// Before the ceiling, one press of the group Acknowledge button opened one write
// transaction per member with nothing capping the member count — five thousand
// commits inside one request, under a Slack interaction timeout of fifteen
// seconds. The assertion that matters is not that the result says 500; it is that
// the member verb was CALLED 500 times and no more, with 507 members present.
func TestFanOutStopsAtTheMemberCeiling(t *testing.T) {
	t.Parallel()

	w := newFanOutWorld(t)
	const beyond = 7
	seeded := w.seedMembers(t, domain.FanOutLimit+beyond)

	actions := &recordingActions{}
	res, err := w.ack(t, w.grouping(t, actions))
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	if got := len(actions.acked); got != domain.FanOutLimit {
		t.Fatalf("member verb called %d times, want exactly %d: the fan-out is not bounded",
			got, domain.FanOutLimit)
	}
	if res.Members != domain.FanOutLimit || res.Applied != domain.FanOutLimit {
		t.Fatalf("result = members %d applied %d, want %d and %d",
			res.Members, res.Applied, domain.FanOutLimit, domain.FanOutLimit)
	}
	if res.Unreached != beyond {
		t.Fatalf("unreached = %d, want %d: a truncated fan-out that does not say so "+
			"is a button that looks like it worked", res.Unreached, beyond)
	}

	// The 500 reached are the 500 OLDEST joins, so the cut is reproducible rather
	// than whichever rows Postgres felt like returning.
	for i, want := range seeded[:domain.FanOutLimit] {
		if actions.acked[i] != want {
			t.Fatalf("member %d = %s, want %s: the fan-out is not reading in join order",
				i, actions.acked[i], want)
		}
	}
}

// TestFanOutBelowTheCeilingIsComplete pins the ordinary group: nothing about the
// bound may change what a group of forty does, and `Unreached` must be zero
// rather than merely small, because zero is the only reading of "this group has
// been acked" that is true.
func TestFanOutBelowTheCeilingIsComplete(t *testing.T) {
	t.Parallel()

	w := newFanOutWorld(t)
	w.seedMembers(t, 6)

	actions := &recordingActions{}
	res, err := w.ack(t, w.grouping(t, actions))
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if res.Members != 6 || res.Applied != 6 || res.Unreached != 0 {
		t.Fatalf("result = %+v, want 6 members, 6 applied, 0 unreached", res)
	}
	if got := len(actions.acked); got != 6 {
		t.Fatalf("member verb called %d times, want 6", got)
	}
}

// TestFanOutPartialFailureLosesNoMember is the failure half of the bound.
//
// A fan-out is not atomic and must not be: the members already applied are
// committed, and each of them is a true receipt on a real signal. What must not
// happen is the ACCOUNT of them going missing — the fan-out used to return a zero
// FanOutResult beside the error, so the only record that 3 of 6 alerts had just
// been acknowledged was destroyed by the failure of the fourth.
func TestFanOutPartialFailureLosesNoMember(t *testing.T) {
	t.Parallel()

	w := newFanOutWorld(t)
	const members, applied = 6, 3
	w.seedMembers(t, members)

	broken := &failingActions{inner: w.alerts, after: applied}
	res, err := w.ack(t, w.grouping(t, broken))
	if err == nil {
		t.Fatal("acknowledge succeeded, want the injected failure")
	}
	if res.Applied != applied || res.Members != applied {
		t.Fatalf("result = members %d applied %d, want %d and %d: the audit of what "+
			"committed did not survive the error", res.Members, res.Applied, applied, applied)
	}
	// Every member is accounted for exactly once: concluded on, or counted as
	// unreached. That is what "no member is lost" means when there is no rollback.
	if got := res.Applied + res.Skipped() + res.Unreached; got != members {
		t.Fatalf("applied %d + skipped %d + unreached %d = %d, want %d members accounted for",
			res.Applied, res.Skipped(), res.Unreached, got, members)
	}
	if n := w.ackedCases(t); n != applied {
		t.Fatalf("%d cases acked in the database, want %d: the receipts written "+
			"before the failure must stand", n, applied)
	}

	// And the run that follows finishes the job without disturbing the receipts
	// already written: 3 refuse, 3 apply, 6 acked, one event each.
	res2, err := w.ack(t, w.grouping(t, w.alerts))
	if err != nil {
		t.Fatalf("second acknowledge: %v", err)
	}
	if res2.Applied != members-applied || res2.SkippedCodes["already_acked"] != applied {
		t.Fatalf("second run = %+v, want %d applied and %d already_acked",
			res2, members-applied, applied)
	}
	if n := w.ackedCases(t); n != members {
		t.Fatalf("%d cases acked after the retry, want %d", n, members)
	}
	if n := w.ackEvents(t); n != members {
		t.Fatalf("%d acknowledgement events, want %d: the retry re-applied the verb",
			n, members)
	}
}

// TestFanOutRetryDoesNotDoubleApply is the retry half, FOR THE ACK.
//
// The Slack interaction path runs the fan-out inside a job, and a job that
// outlives its timeout is RETRIED from the beginning of the membership. Applying
// the ack twice must therefore be impossible rather than unlikely: the second
// pass has to refuse every member it already acknowledged, by code, and must
// leave exactly one fact per signal on the append-only timeline.
//
// ⛔ THIS IS NOT A STATEMENT ABOUT THE OTHER GROUP VERBS, and it was read as one.
// `TestFanOutRetryOfACommentDuplicatesIt` below is what a comment does.
func TestFanOutRetryDoesNotDoubleApply(t *testing.T) {
	t.Parallel()

	w := newFanOutWorld(t)
	const members = 4
	w.seedMembers(t, members)
	svc := w.grouping(t, w.alerts)

	first, err := w.ack(t, svc)
	if err != nil {
		t.Fatalf("first acknowledge: %v", err)
	}
	if first.Applied != members || first.Unreached != 0 {
		t.Fatalf("first run = %+v, want %d applied and nothing unreached", first, members)
	}

	var ackedAt time.Time
	if err := w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT max(acked_at) FROM alert_cases WHERE org_id = $1 AND group_id = $2`,
		w.org.ID, w.group.ID).Scan(&ackedAt); err != nil {
		t.Fatalf("read acked_at: %v", err)
	}

	w.h.Advance(time.Minute)

	second, err := w.ack(t, svc)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Applied != 0 {
		t.Fatalf("retry applied %d, want 0: the verb was applied twice", second.Applied)
	}
	if second.SkippedCodes["already_acked"] != members {
		t.Fatalf("retry skipped %v, want %d already_acked: a retry that refuses for the "+
			"wrong reason is not idempotence", second.SkippedCodes, members)
	}
	if second.Members != members || second.Unreached != 0 {
		t.Fatalf("retry = %+v, want %d members and nothing unreached", second, members)
	}

	if n := w.ackEvents(t); n != members {
		t.Fatalf("%d acknowledgement events after two runs, want %d", n, members)
	}
	var after time.Time
	if err := w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT max(acked_at) FROM alert_cases WHERE org_id = $1 AND group_id = $2`,
		w.org.ID, w.group.ID).Scan(&after); err != nil {
		t.Fatalf("re-read acked_at: %v", err)
	}
	if !after.Equal(ackedAt) {
		t.Fatalf("acked_at moved from %s to %s: the retry rewrote the receipt", ackedAt, after)
	}
}

// TestFanOutRetryOfACommentDuplicatesIt pins the gap, not a guarantee.
//
// ⛔ IT ASSERTS THE BEHAVIOUR OTO ACTUALLY HAS. A group comment is an APPEND onto
// every member's timeline and there is no refusal it can meet: the §C.8 dedupe
// key `alerts/service.Comment` mints is `comment:<alert>:<now>` in RFC3339Nano,
// so a retry a minute later — a redelivered job, a re-pressed button, a caller
// retrying the error a partially-failed fan-out returns WITH its account — is a
// different key, `alert_event_keys` sees no repeat, and every member that was
// annotated is annotated again.
//
// ⭐ WHY IT IS WRITTEN AS A PIN AND NOT AS A BUG. The fix is not a content-hashed
// key: two people who type "restarted it" ten minutes apart wrote two facts, and
// collapsing them would lose a human's words, which is worse than keeping one too
// many. The mechanism is the caller's `Idempotency-Key` on `commentOnAlertGroup`,
// which is open ticket a6cc834. Until that lands this test is the honest record;
// when it lands, THIS is the test that has to change, and the change is the
// evidence the ticket did its job.
func TestFanOutRetryOfACommentDuplicatesIt(t *testing.T) {
	t.Parallel()

	w := newFanOutWorld(t)
	const members = 3
	w.seedMembers(t, members)
	svc := w.grouping(t, w.alerts)

	first, err := w.comment(t, svc, "restarted it")
	if err != nil {
		t.Fatalf("first comment: %v", err)
	}
	if first.FanOut.Applied != members || first.FanOut.Unreached != 0 {
		t.Fatalf("first run = %+v, want %d annotated and nothing unreached",
			first.FanOut, members)
	}
	if n := w.commentEvents(t); n != members {
		t.Fatalf("%d comment events, want %d: one annotation per member", n, members)
	}

	// A retry does not happen in the same nanosecond, which is the whole point:
	// the dedupe key is minted from the clock, so moving the clock is what a real
	// retry does to it.
	w.h.Advance(time.Minute)

	second, err := w.comment(t, svc, "restarted it")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.FanOut.Applied != members {
		t.Fatalf("retry annotated %d members, want %d: nothing refuses a comment",
			second.FanOut.Applied, members)
	}
	if second.FanOut.Skipped() != 0 {
		t.Fatalf("retry skipped %v, want nothing skipped: a comment has no refusal "+
			"to be skipped by", second.FanOut.SkippedCodes)
	}
	if n := w.commentEvents(t); n != 2*members {
		t.Fatalf("%d comment events after two runs, want %d. If this is now %d, a retry "+
			"mechanism has landed (see ticket a6cc834) and this test is the one that "+
			"should change", n, 2*members, members)
	}
}
