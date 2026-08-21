package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⭐ THESE TESTS RUN ON FAKES AND THE REST OF THIS PACKAGE RUNS ON POSTGRES, WHICH
// IS A DELIBERATE SPLIT RATHER THAN AN INCONSISTENCY.
//
// The neighbours here — `TestEvaluateTwiceAllocatesOneSequence`, the dispatch
// tests — exist because their defect is an interaction between two SQL statements,
// and a fake store cannot have it. The digest TICK is the opposite: what it decides
// is which windows are owed, whether a count clears a floor, and what one row says.
// Every one of those is a decision made in Go over values a store hands back, so a
// database would only make the same statements slower and hide them behind fixture
// setup.
//
// ⚠️ TWO THINGS HERE GENUINELY NEED A DATABASE AND ARE NOT ASSERTED. `notif_digest_uniq`
// actually REFUSING the second row for one (org, policy, window_start) is a property
// of the index — this file asserts what the tick does WHEN Postgres raises it — and
// `digestBucketsSQL` counting `alert_cases` rather than alerts or notifications is a
// property of that query, which belongs to the repository's own tests.

// ---------------------------------------------------------------- the fake world
//
// Everything below is a port this module declares in ports.go. Only a handful of
// the methods are on the digest path; the rest exist because the interface has them,
// and they return zero values so that a test which reaches one is a test asserting
// something the digest tick was never supposed to do.

// digestTx runs the unit of work inline. `emit` uses the transaction for atomicity
// between the row, its deliveries and their jobs — a guarantee about the DATABASE,
// not about the decisions this file is testing.
//
// ⛔ IT MODELS EXACTLY ONE THING pgx DOES THAT AN INLINE CALL DOES NOT: a
// transaction whose statement failed with a SQLSTATE can only be ROLLED BACK.
// `db.Tx` commits whenever the closure answers nil, and pgx answers a commit on a
// failed transaction with `ErrTxCommitRollback` (`platform/db/tx.go`). Without
// this, a closure that SWALLOWS an expected 23505 and returns nil looks like a
// success here and comes back as an error in production — which is how "this
// window is already covered" turned into a failed tick that dropped the policy's
// remaining owed windows. A fake that always commits cannot see that bug.
type digestTx struct{ notifs *digestNotifications }

func (x digestTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if x.notifs != nil {
		x.notifs.raised = false
	}
	if err := fn(ctx); err != nil {
		return err // rolled back, which is always allowed
	}
	if x.notifs != nil && x.notifs.raised {
		return pgx.ErrTxCommitRollback
	}
	return nil
}

// digestPolicies serves the tenant's digest policies. `notify_seq_test.go`'s
// `policyStore` deliberately answers `ListWithDigest` with nothing, because the
// fan-out it tests is the SIGNAL one; this is its mirror image.
type digestPolicies struct{ policies []domain.Policy }

func (p digestPolicies) ListLive(context.Context, db.TenantScope) ([]domain.Policy, error) {
	return p.policies, nil
}

func (p digestPolicies) Get(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Policy, error) {
	if len(p.policies) == 0 {
		return domain.Policy{}, nil
	}
	return p.policies[0], nil
}

func (p digestPolicies) ListWithDigest(
	context.Context, db.TenantScope,
) ([]domain.Policy, error) {
	return p.policies, nil
}

// digestSpans records every span the tick actually asked about, so a test can say
// which reads were made and that they were half-open.
type digestSpans struct{ start, end time.Time }

// digestMarks is one call to Mark: which policy accounted for which episodes, and
// whether it reported them or merely examined them.
type digestMarks struct {
	policy     uuid.UUID
	reportedIn *uuid.UUID
	cases      []uuid.UUID
}

// digestReads is the store the tick reads and the little state it keeps: the coverage
// cursor, the episodes in a span, and the per-Case marks.
//
// ⛔ IT USED TO BE `Buckets` PLUS `LastWindow` AND BOTH ARE GONE (git-bug `893cee4`,
// `342e071`, `a8a4010`; migration `00070`). `Buckets` returned one aggregated
// `count(*)` per axis pair, which cannot express "these three episodes, minus the one
// already reported" — a number has no identity to subtract. `LastWindow` was
// `max(digest_window_start)`, which is a window START on a row that only exists when
// something was SENT, and so could answer neither "how far did the reader get" nor
// "how far in calendar time".
type digestReads struct {
	// coveredTo is the cursor: the instant this policy has been EXAMINED up to.
	coveredTo time.Time
	// cases answers one span. It is a function rather than a slice because a tick that
	// covers several windows must be able to see DIFFERENT episodes in each of them —
	// a fake that returned the same rows for every span would have the second window
	// fold to zero against the first window's own marks, which is the dedupe working
	// on an artefact of the fake.
	cases func(from, to time.Time) []repository.DigestCase
	// marked is the mark table. `Mark` writes into it, so dedupe inside ONE tick is
	// modelled exactly as the database models it across two.
	marked map[uuid.UUID]struct{}

	asked    []digestSpans
	advanced []time.Time
	marks    []digestMarks
	pruned   []time.Time
	limits   []int
}

func (d *digestReads) Cases(
	_ context.Context, _ db.TenantScope, from, to time.Time, limit int,
) ([]repository.DigestCase, error) {
	d.asked = append(d.asked, digestSpans{start: from, end: to})
	d.limits = append(d.limits, limit)
	if d.cases == nil {
		return nil, nil
	}
	return d.cases(from, to), nil
}

func (d *digestReads) CoveredTo(
	context.Context, db.TenantScope, uuid.UUID,
) (time.Time, error) {
	return d.coveredTo, nil
}

func (d *digestReads) AdvanceCoverage(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, reached, _ time.Time,
) error {
	d.advanced = append(d.advanced, reached)
	return nil
}

func (d *digestReads) Marked(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, from, to time.Time,
) (map[uuid.UUID]struct{}, error) {
	// The real query ranges on `started_at`, so the fake does too: a mark outside the
	// span must not hide an episode inside it.
	out := map[uuid.UUID]struct{}{}
	for id := range d.marked {
		if at, ok := caseStartedAt(id); ok && (at.Before(from) || !at.Before(to)) {
			continue
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func (d *digestReads) Mark(
	_ context.Context, _ db.TenantScope, policyID uuid.UUID,
	reportedIn *uuid.UUID, cases []repository.DigestCase, _ time.Time,
) error {
	call := digestMarks{policy: policyID, reportedIn: reportedIn}
	if d.marked == nil {
		d.marked = map[uuid.UUID]struct{}{}
	}
	for _, c := range cases {
		call.cases = append(call.cases, c.ID)
		// Write-once, exactly as `ON CONFLICT DO NOTHING` is.
		d.marked[c.ID] = struct{}{}
	}
	d.marks = append(d.marks, call)
	return nil
}

func (d *digestReads) PruneMarks(
	_ context.Context, _ db.TenantScope, before time.Time,
) (int64, error) {
	d.pruned = append(d.pruned, before)
	return 0, nil
}

// digestNotifications records the intents the tick minted, and can answer the way
// Postgres does when `notif_digest_uniq` has already been satisfied by another pod.
type digestNotifications struct {
	inserted []domain.Notification
	// conflict makes Insert raise a BARE 23505 on the digest index rather than the
	// idempotency one, which is the case `emit` has to read as "already covered".
	conflict bool
	// conflictOnce raises it on the FIRST insert only. That is the shape of the real
	// race: ONE of a policy's owed windows was covered by another tick, and the ones
	// behind it are still owed.
	conflictOnce bool
	// raised records that a statement in the CURRENT transaction failed, so digestTx
	// can refuse to commit it exactly as Postgres does.
	raised bool
}

func (n *digestNotifications) Insert(
	_ context.Context, _ db.TenantScope, in domain.Notification,
) (domain.Notification, bool, error) {
	if n.conflict || n.conflictOnce {
		n.conflictOnce = false
		n.raised = true
		return domain.Notification{}, false, &pgconn.PgError{
			Code: "23505", ConstraintName: "notif_digest_uniq",
			Message: "duplicate key value violates unique constraint",
		}
	}
	n.inserted = append(n.inserted, in)
	return in, true, nil
}

func (n *digestNotifications) Get(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Notification, error) {
	return domain.Notification{}, nil
}

func (n *digestNotifications) SetStatus(
	context.Context, db.TenantScope, uuid.UUID, domain.Status, time.Time,
) error {
	return nil
}

func (n *digestNotifications) CountRecent(
	context.Context, db.TenantScope, uuid.UUID, time.Time,
) (int, error) {
	return 0, nil
}

// The count condition's numerator. Zero here, as `CountRecent` is: the digest tick
// evaluates `digest_floor` over its own tiled window and never consults
// `count_min`, so a policy count is not a fact this fake has any opinion about.
func (n *digestNotifications) CountRecentSubjects(
	context.Context, db.TenantScope, uuid.UUID, domain.SubjectKind, uuid.UUID, time.Time, time.Time,
) (int, error) {
	return 0, nil
}

func (n *digestNotifications) ExistsForReason(
	context.Context, db.TenantScope, domain.SubjectKind, uuid.UUID, domain.Reason,
) (bool, error) {
	return false, nil
}

// digestDeliveries records the fan-out rows.
type digestDeliveries struct{ created []repository.NewDelivery }

func (d *digestDeliveries) Create(
	_ context.Context, _ db.TenantScope, in repository.NewDelivery,
) (domain.Delivery, bool, error) {
	d.created = append(d.created, in)
	return domain.Delivery{
		ID: in.ID, NotificationID: in.NotificationID, ChannelID: in.ChannelID,
		ThreadID: in.ThreadID, Mode: in.Mode,
	}, true, nil
}

func (d *digestDeliveries) SetThreadSeq(
	context.Context, db.TenantScope, uuid.UUID, int, time.Time,
) error {
	return nil
}

func (d *digestDeliveries) Get(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Delivery, error) {
	return domain.Delivery{}, nil
}

func (d *digestDeliveries) Claim(
	context.Context, db.TenantScope, uuid.UUID, time.Time, time.Time,
) (domain.Delivery, bool, error) {
	return domain.Delivery{}, false, nil
}

func (d *digestDeliveries) PersistRendered(
	context.Context, db.TenantScope, uuid.UUID, json.RawMessage, string, string, time.Time,
	map[string]string,
) error {
	return nil
}

func (d *digestDeliveries) MarkSent(
	context.Context, db.TenantScope, uuid.UUID, string, string, json.RawMessage, time.Time,
) (bool, error) {
	return false, nil
}

func (d *digestDeliveries) MarkFailed(
	context.Context, db.TenantScope, uuid.UUID, string, domain.ErrorClass, time.Time, time.Time,
) error {
	return nil
}

func (d *digestDeliveries) MarkDead(
	context.Context, db.TenantScope, uuid.UUID, string, domain.ErrorClass, time.Time,
) error {
	return nil
}

func (d *digestDeliveries) MarkSkipped(
	context.Context, db.TenantScope, uuid.UUID, string, time.Time,
) error {
	return nil
}

func (d *digestDeliveries) RepointToRoot(
	context.Context, db.TenantScope, uuid.UUID, string, time.Time,
) error {
	return nil
}

func (d *digestDeliveries) StatusesFor(
	context.Context, db.TenantScope, uuid.UUID,
) ([]domain.DeliveryStatus, error) {
	return nil, nil
}

func (d *digestDeliveries) LastRootHash(
	context.Context, db.TenantScope, uuid.UUID,
) (string, error) {
	return "", nil
}

// digestThreads must never be reached by these tests: the destination is a generic
// webhook, which can neither thread nor amend, so `needsThread` is false and every
// digest is a standalone `post_root`. A conversation per policy per channel is a
// property of `threadSubjectOf`, which is asserted where the thread is real.
type digestThreads struct{ ensured int }

func (t *digestThreads) Ensure(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, _ domain.SubjectKind,
	subjectID uuid.UUID, _ time.Time,
) (domain.Thread, error) {
	t.ensured++
	return domain.Thread{ID: uuid.New(), SubjectID: subjectID}, nil
}

func (t *digestThreads) Get(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Thread, error) {
	return domain.Thread{}, nil
}

func (t *digestThreads) AllocateSeq(
	context.Context, db.TenantScope, uuid.UUID, time.Time,
) (int, error) {
	return 1, nil
}

func (t *digestThreads) RecordRoot(
	context.Context, db.TenantScope, uuid.UUID, string, string, uuid.UUID, int, time.Time,
) error {
	return nil
}

func (t *digestThreads) RecordReply(
	context.Context, db.TenantScope, uuid.UUID, int, time.Time,
) error {
	return nil
}

func (t *digestThreads) AdvanceSent(
	context.Context, db.TenantScope, uuid.UUID, int, time.Time,
) error {
	return nil
}

func (t *digestThreads) MarkDead(
	context.Context, db.TenantScope, uuid.UUID, domain.DeadReason, time.Time,
) error {
	return nil
}

func (t *digestThreads) ClearPointer(context.Context, db.TenantScope, uuid.UUID, time.Time) error {
	return nil
}

// digestChannels is the destination store. A digest is not routed — its policy is
// the input — so this is the ONLY way the tick learns where to send.
type digestChannels struct {
	channels []domain.Channel
	listed   int
}

func (c *digestChannels) ListByIDs(
	context.Context, db.TenantScope, []uuid.UUID,
) ([]domain.Channel, error) {
	c.listed++
	return c.channels, nil
}

func (c *digestChannels) Get(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Channel, error) {
	if len(c.channels) == 0 {
		return domain.Channel{}, nil
	}
	return c.channels[0], nil
}

func (c *digestChannels) SetHealth(
	context.Context, db.TenantScope, uuid.UUID, domain.HealthStatus, string, time.Time,
) error {
	return nil
}

func (c *digestChannels) Credential(
	context.Context, db.TenantScope, uuid.UUID,
) (repository.SealedCredential, error) {
	return repository.SealedCredential{}, nil
}

// digestEvents is required by the constructor and untouched by the tick: `emit`
// does not go through `Evaluate`, so nothing on this path appends a timeline fact.
type digestEvents struct{}

func (digestEvents) AppendNotificationCreated(
	context.Context, db.TenantScope, domain.Notification, int, time.Time,
) error {
	return nil
}

func (digestEvents) AppendNotificationSuppressed(
	context.Context, db.TenantScope, domain.Notification, []domain.SuppressedReason, time.Time,
) error {
	return nil
}

func (digestEvents) AppendDeliveryOutcome(
	context.Context, db.TenantScope, domain.Delivery, *uuid.UUID, *uuid.UUID, string, time.Time,
) error {
	return nil
}

// digestSnapshots answers the group question a digest never asks. A digest has no
// generation; reaching this would mean `emit` had started reading one.
type digestSnapshots struct{ t *testing.T }

func (s digestSnapshots) Snapshot(
	context.Context, db.TenantScope, domain.SnapshotQuery,
) (domain.Snapshot, error) {
	s.t.Error("the digest path read a group snapshot. A digest spans many generations " +
		"and has none of its own; the query would be `WHERE g.id = <nil uuid>`")
	return domain.Snapshot{}, nil
}

// ------------------------------------------------------------------------- the rig

// digestRig is one tenant, one digest policy, one live webhook destination and a
// clock that stands still.
type digestRig struct {
	svc     *service.DigestService
	scope   db.TenantScope
	policy  domain.Policy
	reads   *digestReads
	notifs  *digestNotifications
	deliver *digestDeliveries
	threads *digestThreads
	dests   *digestChannels
	jobs    *enqueuer
	logs    *bytes.Buffer
}

func newDigestRig(
	t *testing.T, now time.Time, p domain.Policy, reads *digestReads, notifs *digestNotifications,
) digestRig {
	t.Helper()

	scope, err := db.NewTenantScope(p.OrgID)
	require.NoError(t, err)

	// A generic webhook: no threading and no amend, so `needsThread` is false and the
	// thread store is never consulted.
	dests := &digestChannels{channels: []domain.Channel{{
		ID: p.ChannelIDs[0], OrgID: p.OrgID, Type: domain.ChannelTypeWebhook,
		Name: "webhook", Config: json.RawMessage(`{}`), Renderer: "webhook.json",
		Verbosity: domain.VerbosityAll, Enabled: true, HealthStatus: domain.HealthUnknown,
	}}}
	policies := digestPolicies{policies: []domain.Policy{p}}

	policySvc, err := service.NewPolicyService(policies, dests)
	require.NoError(t, err)

	var (
		jobs    = &enqueuer{}
		deliver = &digestDeliveries{}
		threads = &digestThreads{}
		logs    = &bytes.Buffer{}
	)

	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            digestTx{notifs: notifs},
		Policies:      policySvc,
		Notifications: notifs,
		Deliveries:    deliver,
		Threads:       threads,
		Snapshots:     digestSnapshots{t: t},
		Events:        digestEvents{},
		Enqueuer:      jobs,
		Channels:      dests,
		Clock:         clock.NewFake(now),
	})
	require.NoError(t, err)

	svc, err := service.NewDigestService(service.DigestConfig{
		Policies: policies,
		Digests:  reads,
		Notifier: notifier,
		Clock:    clock.NewFake(now),
		Logger:   slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	require.NoError(t, err)

	return digestRig{
		svc: svc, scope: scope, policy: p, reads: reads, notifs: notifs,
		deliver: deliver, threads: threads, dests: dests, jobs: jobs, logs: logs,
	}
}

// tickPolicy is a live policy that digests: a window, the Reason that routes it, and
// one destination.
func tickPolicy(window time.Duration, floor int, matchers ...domain.Matcher) domain.Policy {
	return domain.Policy{
		ID:         uuid.New(),
		OrgID:      uuid.New(),
		Name:       "namespace-digest",
		Priority:   1,
		Enabled:    true,
		Matchers:   matchers,
		Reasons:    []domain.Reason{domain.ReasonFired, domain.ReasonDigest},
		ChannelIDs: []uuid.UUID{uuid.New()},
		Digest:     domain.Digest{Window: window, Floor: floor},
	}
}

// ⛔ THE AGGREGATE IS GONE AND THE ROW IS THE EPISODE (git-bug `342e071`/`a8a4010`,
// migration `00070`). `bucket(namespace, n)` used to build one `count(*)` per ADR-0038
// axis pair, and an aggregate has no identity to subtract — "these three episodes minus
// the one an earlier digest already reported" cannot be said about a number. A digest is
// now a bounded-lookback SET of Cases, so the fixture is a set of Cases.
//
// The id is DERIVED FROM THE START INSTANT rather than random, for two reasons the real
// query shares. It makes two reads of the same span return the same episodes — the real
// `ORDER BY started_at DESC, id DESC` is total precisely so that a truncated tail is not
// non-deterministic — and it lets `digestReads.Marked` range on `started_at` without a
// second map, which is how it models the index the real query rides.
func episode(namespace string, at time.Time, n int) repository.DigestCase {
	return repository.DigestCase{
		ID:        caseID(at, n),
		StartedAt: at,
		Labels:    map[string]string{"alertname": "Whatever", "namespace": namespace},
	}
}

// episodes is `n` episodes in one namespace, all opening at `at`.
func episodes(namespace string, at time.Time, n int) []repository.DigestCase {
	out := make([]repository.DigestCase, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, episode(namespace, at, i))
	}
	return out
}

// caseID packs the start instant into the first eight bytes and the ordinal into the
// last, so `caseStartedAt` can read it back. It is a fixture id and not a UUID anybody
// stores: no version bits, no randomness, and that is what makes it addressable.
func caseID(at time.Time, n int) uuid.UUID {
	var u uuid.UUID
	ns := at.UTC().UnixNano()
	for b := 0; b < 8; b++ {
		u[b] = byte(ns >> (8 * (7 - b)))
	}
	u[15] = byte(n)
	return u
}

// caseStartedAt reverses caseID, which is what lets the fake mark table answer a range
// query the way `digest_case_span_idx` does.
func caseStartedAt(id uuid.UUID) (time.Time, bool) {
	var ns int64
	for b := 0; b < 8; b++ {
		ns = ns<<8 | int64(id[b])
	}
	if ns <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns).UTC(), true
}

// sameEvery answers every span with the same episodes. It is for the tests that cover
// exactly ONE window, where "which window asked" cannot matter.
func sameEvery(cases ...repository.DigestCase) func(from, to time.Time) []repository.DigestCase {
	return func(time.Time, time.Time) []repository.DigestCase { return cases }
}

// perWindow answers each span with `n` episodes of its own, opening one minute before
// the span's END — which is inside the window proper for every admissible window
// length, and therefore NOT inside the `DigestLookback` tail of the window after it.
//
// ⭐ THAT PLACEMENT IS THE WHOLE POINT OF THE HELPER. A fake that put every window's
// episodes at the same instant would have the second window of a backfill fold to zero
// against the first window's marks, and the test would be asserting the fake rather than
// the tick.
func perWindow(namespace string, n int) func(from, to time.Time) []repository.DigestCase {
	return func(_, to time.Time) []repository.DigestCase {
		return episodes(namespace, to.Add(-time.Minute), n)
	}
}

// The tick's frame of reference: 13:47 is inside the OPEN 13:40 window, so the
// newest CLOSED ten-minute window is 13:30.
//
// ⚠️ `newestClosedFrom` IS THE READ'S START AND IS TWO MINUTES BEFORE THE WINDOW'S
// (git-bug `a8a4010`). A digest for `[13:30, 13:40)` reads
// `[13:30 - domain.DigestLookback, 13:40)`, because `alert_cases.started_at` is oto's
// clock read BEFORE the inserting transaction and a Case that had not committed when the
// 13:30 tick read `[13:20, 13:30)` is invisible to every later window's predicate.
var (
	tickNow          = time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)
	newestClosed     = time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)
	newestClosedTo   = time.Date(2026, 8, 18, 13, 40, 0, 0, time.UTC)
	newestClosedFrom = newestClosed.Add(-domain.DigestLookback)
	// coveredThrough1330 is the cursor value meaning "everything up to the start of the
	// 13:30 window has been examined", i.e. exactly the state in which the 13:30 window
	// is the one owed. Under the pre-00070 start cursor this was spelled `13:20`.
	coveredThrough1330 = newestClosed
)

// ----------------------------------------------------------------------- the tests

// TestAWindowBelowItsFloorProducesNoDigest is the ticket's done-when #3, and the
// half of it that is easy to get wrong: nothing is sent, and nothing is RECORDED
// either.
//
// ⭐ NO SUPPRESSED ROW, AND THAT IS NOT THE SILENT SUPPRESSION §B.6 FORBIDS. A
// suppressed Notification exists because oto DECIDED NOT TO SEND SOMETHING IT HAD;
// under the floor there was nothing to send. Minting one per quiet window would put
// a row per policy per ten minutes into the audit log forever and would make "oto
// withheld something from me" indistinguishable from "the namespace was quiet".
func TestAWindowBelowItsFloorProducesNoDigest(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 3)...),
	}
	rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 5), reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, sent, "three cases cleared a floor of five")
	assert.Empty(t, rig.notifs.inserted,
		"a window below its floor wrote a notification. Nothing was withheld — there was "+
			"nothing to withhold — and a row per quiet window would drown the one row that "+
			"means oto actually did suppress something")
	assert.Empty(t, rig.deliver.created, "a digest nobody minted was fanned out")

	// The span that WAS read is the newest closed window plus its lookback tail,
	// half-open. An episode that opened at exactly 13:40 belongs to the next window,
	// not this one.
	require.Len(t, rig.reads.asked, 1)
	assert.Equal(t, digestSpans{start: newestClosedFrom, end: newestClosedTo}, rig.reads.asked[0],
		"the tick read the wrong span. Adjacent windows share a boundary instant, so a "+
			"closed interval would count an episode that opened on it in both windows — and "+
			"the start must be pulled back by DigestLookback, or a Case that committed after "+
			"the previous tick read its window is counted by nothing at all")

	// ⭐⭐ THE THREE EPISODES ARE MARKED, WITH NO REPORT, AND COVERAGE ADVANCES PAST THE
	// WINDOW. This is the half of git-bug `893cee4` the arithmetic does not fix. The old
	// code recorded NOTHING for a below-floor window, under a comment calling the
	// consequence correct: "an unsent window advances no cursor, so a quiet stretch is
	// re-examined by the next tick and then falls off the MaxDigestBackfill horizon".
	// The re-examination is indeed harmless; the FROZEN CURSOR was not. It made the owed
	// span grow by one window every window, forever, so every tick ran six aggregate
	// queries and logged a data-loss warning about a backlog nothing was ever owed.
	require.Len(t, rig.reads.marks, 1,
		"the examined episodes were not marked, so the next tick cannot tell `looked at "+
			"this and it did not clear your floor` from `never looked`, which is the same "+
			"absence the old design could not distinguish")
	assert.Nil(t, rig.reads.marks[0].reportedIn,
		"a below-floor window claimed to have REPORTED its episodes. A mark with no report "+
			"is the §B.6 receipt — oto examined this and said nothing — and conflating it "+
			"with a real report would hide the one case the reconciler exists to find")
	assert.Len(t, rig.reads.marks[0].cases, 3)
	assert.Equal(t, []time.Time{newestClosedTo}, rig.reads.advanced,
		"coverage did not advance past a window that was examined and found quiet. That is "+
			"the leak: the same window is re-derived on every tick for as long as the "+
			"namespace stays quiet, which is forever")
}

// TestAWindowThatClearsItsFloorIsOneDigestAboutThePolicyAndTheWindow: the subject is
// the PAIR, and the row says so in three places at once.
func TestAWindowThatClearsItsFloorIsOneDigestAboutThePolicyAndTheWindow(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 5)...),
	}
	p := tickPolicy(10*time.Minute, 5)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Equal(t, 1, sent, "a window at exactly its floor sent nothing; the floor is a MINIMUM")
	require.Len(t, rig.notifs.inserted, 1)

	n := rig.notifs.inserted[0]
	assert.Equal(t, domain.SubjectDigest, n.SubjectKind)
	assert.Equal(t, domain.ReasonDigest, n.Reason)
	assert.Equal(t, p.ID, n.SubjectID,
		"`subject_id` must be the POLICY half of the pair. One UUID column cannot hold "+
			"(policy, window), and hashing the two into a synthetic id would make `subject_id` "+
			"resolve against no table")
	// ⛔ THIS ASSERTED `n.GroupID == uuid.Nil` — "a digest claimed a delivery group" —
	// AND `GroupID` IS DELETED (git-bug `7570090`, migration `00069`). The property is
	// asserted STRUCTURALLY rather than dropped, and it is now stronger than the
	// absence it used to be: the delivery target is the pair, a digest's conversation
	// is its own kind keyed by the POLICY, and the thing the old assertion forbade —
	// a digest landing in some signal's conversation — is now expressible as the
	// positive fact that it does not.
	assert.Equal(t, domain.ConversationDigest, n.ConversationKind,
		"a digest landed in a signal's conversation. It spans many episodes, so it opens "+
			"its own conversation keyed by its policy")
	assert.Equal(t, p.ID, n.ConversationID,
		"the digest's conversation is keyed by the POLICY: one ongoing conversation per "+
			"policy per channel, one reply per window")
	require.NotNil(t, n.PolicyID)
	assert.Equal(t, p.ID, *n.PolicyID)
	require.NotNil(t, n.DigestWindowStart)
	assert.Equal(t, newestClosed, n.DigestWindowStart.UTC(),
		"the digest reports on the wrong window; 13:40 is still open")
	require.NotNil(t, n.DigestCount)
	assert.Equal(t, 5, *n.DigestCount,
		"the stored count must be the number the floor was compared against — it is stored "+
			"rather than recomputed because `alert_cases` is reapable and a recomputed count "+
			"would shrink as the episodes aged out")
	assert.Equal(t, p.Digest.WindowOrdinal(newestClosed), n.StateVersion,
		"`state_version` carries the WINDOW ORDINAL (§C.7). Without it every window of one "+
			"policy would hash to the same idempotency key and only the first would ever be sent")
	assert.Equal(t,
		domain.IdempotencyKey(p.OrgID, domain.SubjectDigest, p.ID, domain.ReasonDigest,
			n.StateVersion, uuid.Nil),
		n.IdempotencyKey,
		"a digest names no §C.7 occasion — its window ordinal already discriminates — so its "+
			"stored key is byte-identical to the one minted before that field existed")

	// ⭐⭐ AND THE ROW CARRIES ITS OWN SPAN (git-bug `342e071`, migration `00070`).
	// `digest_window_start` alone is not a span: it is a span only in combination with
	// the window LENGTH that was in force when the digest was sent, and nothing stored
	// the length — so every reader that wanted one multiplied the start by the policy's
	// CURRENT `digest_window_s`, which means an operator who narrows a window
	// retroactively changes the span every card oto has ever drawn claims to cover.
	require.NotNil(t, n.DigestCoveredFrom)
	require.NotNil(t, n.DigestCoveredTo)
	assert.Equal(t, newestClosed, n.DigestCoveredFrom.UTC(),
		"nothing came out of the lookback tail here — all five episodes opened inside the "+
			"window — so the covered span starts exactly at the window start")
	assert.Equal(t, newestClosedTo, n.DigestCoveredTo.UTC(),
		"the covered span must END at the window's exclusive end. That instant is what the "+
			"next tick's cursor is derived from, and reading it back off the row is what "+
			"makes the cursor survive a change of window length")

	// The episodes are marked AS REPORTED, and by this notification. Without that, the
	// next window's two-minute lookback tail would report all five of them a second time
	// — a louder bug than the one the tail fixes.
	require.Len(t, rig.reads.marks, 1)
	require.NotNil(t, rig.reads.marks[0].reportedIn)
	assert.Equal(t, n.ID, *rig.reads.marks[0].reportedIn)
	assert.Len(t, rig.reads.marks[0].cases, 5)
	assert.Equal(t, []time.Time{newestClosedTo}, rig.reads.advanced)

	// It reached a destination, and exactly one.
	assert.Equal(t, 1, rig.dests.listed)
	require.Len(t, rig.deliver.created, 1)
	assert.Nil(t, rig.deliver.created[0].ThreadID,
		"a webhook can neither thread nor amend, so its digest is a standalone post and "+
			"`deliveries_thread_ck` permits the NULL")
	assert.Len(t, rig.jobs.jobs, 1, "the delivery job is enqueued in the creating transaction")
	assert.Zero(t, rig.threads.ensured)
}

// TestTheDigestCountsOnlyTheGenerationsThePolicySelects.
//
// ⭐ THE MATCHERS ARE THE NAMESPACE SELECTOR, and they are the SAME matchers the
// notification path uses against the SAME labels — `domain.Policy.Matches` is the
// single implementation. A policy that selected one set of alerts for its individual
// notifications and a different set for its digest would be two policies wearing one
// name.
//
// ⚠️ WHAT IS BEING SUMMED IS `DigestBucket.Cases` — episodes that OPENED in the
// window. Counting ALERTS would count an identity that has been broken all week as
// news; counting NOTIFICATIONS is circular, because a throttled channel would lower
// the number a digest exists to report. That the SQL behind the bucket does the
// former is asserted in the repository's tests, not here.
func TestTheDigestCountsOnlyTheGenerationsThePolicySelects(t *testing.T) {
	t.Parallel()

	at := newestClosed.Add(time.Minute)
	var rows []repository.DigestCase
	rows = append(rows, episodes("observability", at, 4)...)
	rows = append(rows, episodes("payments", at.Add(time.Second), 40)...)
	rows = append(rows, episodes("observability", at.Add(2*time.Second), 3)...)

	reads := &digestReads{coveredTo: coveredThrough1330, cases: sameEvery(rows...)}
	p := tickPolicy(10*time.Minute, 1,
		domain.Matcher{Name: "namespace", Op: domain.OpEqual, Value: "observability"})
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, rig.notifs.inserted, 1)

	require.NotNil(t, rig.notifs.inserted[0].DigestCount)
	assert.Equal(t, 7, *rig.notifs.inserted[0].DigestCount,
		"the digest reported %d cases. A policy matching `namespace=observability` must "+
			"count the four and the three that matched and NONE of the forty that did not — "+
			"a digest that folds every bucket is a digest that reports on the whole tenant "+
			"whatever the operator selected", *rig.notifs.inserted[0].DigestCount)
}

// TestAMissedTickWritesAtMostSixDigestsAndNamesTheRest is done-when #2
// across a restart, and the bound is still the whole point.
//
// Covering everything owed is an outage amplifier — a five-minute policy down for a
// day owes 288 digests, all arriving in one second, which is the flood a digest
// exists to prevent produced by its own catch-up. Covering only the newest makes the
// cursor pointless. So: the newest `MaxDigestBackfill`, and the rest are abandoned
// permanently.
//
// ⛔⛔ IT WAS `...AndSaysWhatItDropped`, THEN `...AndSaysNothingAboutTheRest`, AND THE
// TRUTH IS BETWEEN THE TWO (git-bug `893cee4`). The first version asserted a
// `skipped_windows: 9` WARN, under the argument that `MaxDigestBackfill` deliberately
// drops owed windows and a damper that cannot report itself is the silent suppression
// §B.6 refuses. The argument was sound. The NUMBER was not: because a below-floor window
// advanced no cursor, a policy over a namespace nobody had paged about since last Tuesday
// produced an "owed" span of thousands of windows, so that same WARN fired on every one
// of the 1 440 ticks a day for a backlog nothing was ever owed. An operator who alerted
// on it got paged by every quiet namespace they had. The second version deleted the
// number and with it every trace of the drop, which is the other error: nine windows were
// stepped over unexamined and NOTHING anywhere said so.
//
// ⭐ SO THE DROP IS NAMED AND THE FICTION IS NOT. Coverage now advances for every window
// EXAMINED, so an owed span is only ever "how far behind the reader is" — a quiet policy
// is one window behind and logs nothing at all (see
// TestAPolicyQuietForLongerThanTheBackfillHorizonGoesSilentAndStaysCheap, whose SECOND
// tick is the assertion that used to be made here). Reaching a tick fifteen windows
// behind means the tick did not run, and this one line, at the moment the cursor jumps,
// is the honest report of it.
//
// ⭐ AND THE ALERTABLE NUMBER IS STILL THE OTHER ONE. The nine abandoned windows are
// exactly the windows whose episodes carry NO MARK, so `DigestService.ReconcileOrg`
// counts them as EPISODES NOBODY WAS TOLD ABOUT — the thing an operator cares about
// rather than a count of windows, zero on a healthy install, and therefore the number to
// alarm on. The line asserted below is a breadcrumb, and it says so itself.
func TestAMissedTickWritesAtMostSixDigestsAndNamesTheRest(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		// Coverage reached 11:10; fifteen ten-minute windows are owed, 11:10 to 13:30.
		coveredTo: time.Date(2026, 8, 18, 11, 10, 0, 0, time.UTC),
		cases:     perWindow("observability", 2),
	}
	p := tickPolicy(10*time.Minute, 1)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Equal(t, domain.MaxDigestBackfill, sent)
	require.Len(t, rig.notifs.inserted, domain.MaxDigestBackfill,
		"one tick wrote %d digests for one policy. `MaxDigestBackfill` is what caps a tick's "+
			"work at six reads per policy however long the process was gone",
		len(rig.notifs.inserted))

	var got []time.Time
	for _, n := range rig.notifs.inserted {
		require.NotNil(t, n.DigestWindowStart)
		got = append(got, n.DigestWindowStart.UTC())
	}
	assert.Equal(t, []time.Time{
		time.Date(2026, 8, 18, 12, 40, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 12, 50, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 10, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		newestClosed,
	}, got,
		"the covered windows must be the NEWEST six. Covering the oldest six first would "+
			"catch up eventually while spending the next hour posting summaries of a morning "+
			"nobody is looking at any more")

	// Every window is its own intent, so the ordinals are consecutive and the keys
	// are all different.
	keys := map[string]struct{}{}
	for i, n := range rig.notifs.inserted {
		keys[n.IdempotencyKey] = struct{}{}
		if i > 0 {
			assert.Equal(t, rig.notifs.inserted[i-1].StateVersion+1, n.StateVersion,
				"consecutive windows must carry consecutive ordinals")
		}
	}
	assert.Len(t, keys, domain.MaxDigestBackfill,
		"two of the six backfilled windows share an idempotency key, so one of them would "+
			"be swallowed as a duplicate of the other")

	// ⛔ THE DELETED FIELD STAYS DELETED. It was not renamed: `skipped_windows` counted a
	// backlog nothing was ever owed, and no honest number can be spelled with it.
	assert.NotContains(t, rig.logs.String(), "skipped_windows",
		"the tick logged `skipped_windows`. That field was deleted rather than redefined "+
			"(git-bug `893cee4`): under a cursor that advances for every window EXAMINED, a "+
			"quiet policy is owed nothing, so the number could only ever be a count of "+
			"windows an outage cost")
	assert.NotContains(t, rig.logs.String(), "too far behind")

	// ⭐ AND THE NINE ARE NAMED, ONCE, AT THE MOMENT THE CURSOR JUMPS THEM. Fifteen
	// windows were owed and six were covered; the other nine are stepped over by the
	// coverage write below and no later tick will ever look at them. A drop this design
	// makes on purpose still has to be findable by the person it happened to.
	assert.Contains(t, rig.logs.String(), `"abandoned_windows":9`,
		"the tick abandoned nine owed windows and said nothing about it. `MaxDigestBackfill` "+
			"is a deliberate drop, and a deliberate drop nobody can see is the silent "+
			"suppression §B.6 refuses — the fix for the fictional count was not to stop "+
			"reporting the real one. Log: %s", rig.logs.String())
	assert.Contains(t, rig.logs.String(), "unreported_episodes",
		"the abandonment line does not point at the number an operator should actually "+
			"alarm on. Windows are not episodes: an abandoned window over a quiet namespace "+
			"lost nothing, and ReconcileOrg is what counts the harm. Log: %s",
		rig.logs.String())

	// Coverage jumps STRAIGHT to the newest covered window's end, so the nine abandoned
	// windows are never re-derived. Their episodes are the ones with no mark.
	require.NotEmpty(t, rig.reads.advanced)
	assert.Equal(t, newestClosedTo, rig.reads.advanced[len(rig.reads.advanced)-1],
		"coverage must reach the end of the newest window this tick covered, or the tick "+
			"re-derives the same fifteen-window span next minute — which is the bug")
	assert.Len(t, rig.reads.marks, domain.MaxDigestBackfill,
		"exactly the six covered windows may mark their episodes. Marking the abandoned "+
			"nine would erase the only evidence that they were lost")
}

// TestAStragglerThatCommittedTooLateIsCountedByTheNextDigest is git-bug `a8a4010`, and
// it is the race driven directly rather than described.
//
// ⛔ THE OLD BEHAVIOUR WAS A SILENT DROP WITH NO ROW, NO LOG LINE AND NO
// `suppressed_reason`. `alert_cases.started_at` is oto's clock read at the START of a
// batch's processing — `ingestion/service/process.go` takes one `now` in `plan` and
// stamps every alert in the batch with it — so everything between that read and the
// commit is unbounded latency: decoding, the rejection write, chunking, grouping, the
// lifecycle machine, an fsync. A batch stamped 13:29:59.95 whose transaction commits at
// 13:30:00.30 is invisible to the tick that fires at 13:30:00.10 and covers
// `[13:20, 13:30)`. That tick then wrote the digest and moved the cursor, and every
// later window's predicate started at or after 13:30 — the boundary the Case is on the
// wrong side of. The episode sat in the table, counted by nothing.
//
// ⭐ IT IS ALSO WHY THE ANSWER IS A RE-SCAN AND NOT A LAG. A lag — never digest a window
// that closed less than G ago — turns one unmeasurable number into permanent silent
// loss, and the effective margin is `G - (reader_clock - writer_clock)`, so inter-pod
// skew eats it invisibly. Under a re-scan, exceeding the budget produces a DUPLICATE,
// which somebody can see.
func TestAStragglerThatCommittedTooLateIsCountedByTheNextDigest(t *testing.T) {
	t.Parallel()

	// 13:29:59.95: inside the window the PREVIOUS tick already digested, and therefore
	// past every later window's start. It carries no mark, because no read ever saw it.
	straggler := episode("observability", newestClosed.Add(-50*time.Millisecond), 7)
	inWindow := episodes("observability", newestClosed.Add(time.Minute), 2)

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(append([]repository.DigestCase{straggler}, inWindow...)...),
	}
	p := tickPolicy(10*time.Minute, 1)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, rig.notifs.inserted, 1)

	n := rig.notifs.inserted[0]
	require.NotNil(t, n.DigestCount)
	assert.Equal(t, 3, *n.DigestCount,
		"the digest reported %d episodes and there were three: the two that opened inside "+
			"[13:30, 13:40) and the one whose transaction committed too late for the tick "+
			"that read [13:20, 13:30). Reporting two is the silent drop — the episode is in "+
			"the table with a started_at nothing will ever look at again, and the operator "+
			"who asked for a digest instead of individual alerts is the only person who "+
			"would have been told", *n.DigestCount)

	// ⭐⭐ AND THE ROW CLAIMS THE SPAN THAT ABUTS THE PREVIOUS ONE, NOT THE STRAGGLER'S
	// OWN INSTANT, WHICH IS THE ASSERTION THAT INVERTED. This used to require
	// `covered_from == straggler.StartedAt` — the digest reaching back to the episode it
	// swept up — and that value OVERLAPS the previous digest, which stored
	// `covered_to = 13:30` for the window before this one. The `Digest` doc in
	// `channels/render/webhookjson/envelope.go` promises consumers that "consecutive
	// digests from one policy ABUT; they do not overlap", and a consumer partitioning
	// time by these spans double-attributed the fifty milliseconds where they did. The
	// span is now clamped to the coverage instant the previous examination reached, so
	// the claim begins exactly where the last one ended.
	//
	// The episode is still REPORTED here — `DigestCount` above is three — and that is the
	// whole trade: the claimed boundary under-states by the width of the straggler
	// rather than two messages claiming one instant. See `coveredSpanOf`.
	require.NotNil(t, n.DigestCoveredFrom)
	assert.Equal(t, coveredThrough1330, n.DigestCoveredFrom.UTC(),
		"the digest claimed a span starting at %s, before the instant its policy's coverage "+
			"had already reached (%s). The previous digest stored that instant as its own "+
			"`covered_to`, so the two messages claim the same time twice and the abutment "+
			"the webhook envelope promises is false",
		n.DigestCoveredFrom.UTC(), coveredThrough1330)
	assert.Len(t, rig.reads.marks[0].cases, 3,
		"all three must be marked, the straggler included, or the NEXT window's lookback "+
			"tail reports it a second time")
}

// TestAnEpisodeAnEarlierDigestReportedIsNotReportedAgain is the other half of the
// lookback, and without it the fix would be louder than the bug.
//
// ⚠️ THE TAIL IS TWO MINUTES WIDE AND IS READ BY EVERY SINGLE DIGEST. Almost everything
// in it was already reported by the previous window's digest. With no per-Case
// subtraction, every digest would re-report two minutes of episodes — for a five-minute
// window, nearly half its content, every time, forever.
func TestAnEpisodeAnEarlierDigestReportedIsNotReportedAgain(t *testing.T) {
	t.Parallel()

	reported := episode("observability", newestClosed.Add(-30*time.Second), 3)
	fresh := episodes("observability", newestClosed.Add(time.Minute), 2)

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(append([]repository.DigestCase{reported}, fresh...)...),
		// The previous window's digest already accounted for it.
		marked: map[uuid.UUID]struct{}{reported.ID: {}},
	}
	p := tickPolicy(10*time.Minute, 1)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, rig.notifs.inserted, 1)

	n := rig.notifs.inserted[0]
	require.NotNil(t, n.DigestCount)
	assert.Equal(t, 2, *n.DigestCount,
		"the digest reported %d episodes. The one in the lookback tail is already in an "+
			"earlier digest, and counting it again would put the same firing in two "+
			"summaries — which is the double-report the marks exist to prevent", *n.DigestCount)
	require.NotNil(t, n.DigestCoveredFrom)
	assert.Equal(t, newestClosed, n.DigestCoveredFrom.UTC(),
		"the covered span reached back to include an episode this digest did not report. "+
			"`from` moves only for a straggler this message actually counted")
	assert.Len(t, rig.reads.marks[0].cases, 2,
		"a mark is write-once, so re-marking an episode an earlier digest reported is at "+
			"best a no-op and at worst an attempt to reassign it")
}

// TestAPolicyQuietForLongerThanTheBackfillHorizonGoesSilentAndStaysCheap is the mirror
// of the backfill test, and it is git-bug `893cee4`'s done-when in one function.
//
// ⛔⛔ THIS IS THE BUG. A ten-minute policy over a namespace nobody has paged about since
// last Tuesday: `lastCovered` is last Tuesday, `newest` is now, `total` is thousands,
// `abandoned` is `total - 6` and rises by one every ten minutes. The `notify.digest` tick
// runs every 60 s (SPEC §G.3), so all of it happened 1 440 times a day — six aggregate
// queries per quiet policy per tick, each one re-asking a question whose answer the
// previous tick already had, plus a WARN naming a data loss that had not occurred.
//
// The fix is not better arithmetic. It is that coverage advances for a window that was
// EXAMINED, so there is no owed span to clamp: the first tick after the quiet stretch
// does its six windows and then the policy is caught up forever.
func TestAPolicyQuietForLongerThanTheBackfillHorizonGoesSilentAndStaysCheap(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		// A week behind, and every window of it below the floor of five.
		coveredTo: tickNow.Add(-7 * 24 * time.Hour).Truncate(10 * time.Minute),
		cases:     perWindow("observability", 1),
	}
	p := tickPolicy(10*time.Minute, 5)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, sent)
	assert.Empty(t, rig.notifs.inserted)
	assert.Len(t, rig.reads.asked, domain.MaxDigestBackfill,
		"a policy a week behind read %d spans in one tick. The cap is what stops the "+
			"catch-up being its own outage", len(rig.reads.asked))
	// ⛔ THE FICTION MUST NOT COME BACK, AND THE TRUE FACT MUST BE SAID ONCE. A cursor a
	// week behind is no longer what a QUIET namespace looks like — the second half of
	// this test is the proof: coverage advances for every window EXAMINED, so a quiet
	// policy arrives one window behind. Reaching this tick a week behind means the tick
	// itself did not run for a week, and 1 002 windows really are being stepped over
	// unexamined. `skipped_windows` was deleted because it counted a backlog nothing was
	// ever owed; this line counts windows the cursor is about to jump, which is a
	// different fact and may not be silent (git-bug `893cee4`).
	assert.NotContains(t, rig.logs.String(), "skipped_windows",
		"the deleted field is back. Log: %s", rig.logs.String())
	assert.Contains(t, rig.logs.String(), `"abandoned_windows":1002`,
		"the tick stepped the cursor over 1 002 windows it never examined and said nothing "+
			"about them. That is the silence the old `skipped_windows` line was deleted FOR "+
			"— the number was fictional, the abandonment is not. Log: %s", rig.logs.String())

	// ⭐⭐ AND THE SECOND TICK IS THE ASSERTION THAT MATTERS. The old design's cursor did
	// not move, so the next tick did the same six reads and logged the same warning one
	// higher, forever. Coverage has now advanced, so the same policy one minute later
	// owes nothing at all.
	require.NotEmpty(t, rig.reads.advanced)
	reads.coveredTo = rig.reads.advanced[len(rig.reads.advanced)-1]
	reads.asked = nil
	reads.advanced = nil
	// And the log starts empty, because THIS is where the original assertion belongs: the
	// complaint against the deleted line was never that a recovered outage says something
	// once, it was that a quiet policy said it every single minute forever.
	rig.logs.Reset()

	sent, err = rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	assert.Zero(t, sent)
	assert.Empty(t, rig.reads.asked,
		"the tick one minute later read %d spans again. Nothing has closed since, so the "+
			"correct answer is zero reads — and the old design's answer was six, every "+
			"minute, for as long as the namespace stayed quiet", len(rig.reads.asked))
	assert.Empty(t, rig.reads.advanced,
		"coverage was written again with nothing examined, which is a statement per policy "+
			"per tick for no fact the previous one did not already carry")
	assert.Empty(t, rig.logs.String(),
		"a caught-up policy over a namespace that has been quiet for a week logged "+
			"something. There is nothing to say: nothing is owed, nothing was dropped and "+
			"nothing was withheld. The line this used to emit was a data-loss report, and "+
			"an operator who wired an alert on a message shaped like that — which is exactly "+
			"what it invited — got paged by every quiet namespace they had. Log: %s",
		rig.logs.String())
}

// TestAWindowAlreadyCoveredIsNeverDigestedTwice is done-when #4 and the ticket's
// "covered exactly once even across a restart".
//
// It has two halves because the guarantee does: the CURSOR stops the ordinary
// re-tick without a query at all, and `notif_digest_uniq` stops the two pods that
// ticked in the same second.
func TestAWindowAlreadyCoveredIsNeverDigestedTwice(t *testing.T) {
	t.Parallel()

	t.Run("the cursor is already at the newest closed window", func(t *testing.T) {
		t.Parallel()

		reads := &digestReads{
			// The INSTANT 13:40 — the end of the newest closed window. Under the
			// pre-00070 start cursor this was spelled `13:30`, and the re-spelling is
			// the fix: a start only means a span in combination with the length that was
			// in force when it was written (git-bug `342e071`).
			coveredTo: newestClosedTo,
			cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 9)...),
		}
		rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 1), reads, &digestNotifications{})

		sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
		require.NoError(t, err)

		assert.Zero(t, sent)
		assert.Empty(t, rig.notifs.inserted, "a window already covered was digested again")
		assert.Empty(t, rig.reads.asked,
			"the tick read a window it owed nothing for. This is the answer on all but "+
				"the first tick of each window, and it is what makes a once-a-minute tick "+
				"affordable")
		assert.Empty(t, rig.reads.advanced,
			"coverage was rewritten with nothing examined")
	})

	t.Run("another pod won the window", func(t *testing.T) {
		t.Parallel()

		reads := &digestReads{
			coveredTo: coveredThrough1330,
			cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 9)...),
		}
		// The row is already there, and Postgres reports the DIGEST index rather than
		// the idempotency one — which `Insert` cannot swallow, because it is not that
		// statement's arbiter.
		rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 1), reads,
			&digestNotifications{conflict: true})

		sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)

		require.NoError(t, err,
			"a bare 23505 on `notif_digest_uniq` came back as an error. It means precisely "+
				"`this window is already covered`, which is the answer — treating it as a "+
				"failure would fail the whole tenant's tick on the ordinary race between two pods")
		assert.Zero(t, sent, "a window somebody else covered must not be counted as sent")
		assert.Empty(t, rig.deliver.created,
			"the losing pod fanned out anyway, so the digest would be delivered twice")
		assert.Empty(t, rig.jobs.jobs)

		// ⭐ COVERAGE STILL ADVANCES, AND THE MARKS DO NOT. The window IS covered — by
		// the other pod's digest, in the other pod's transaction, along with the other
		// pod's marks. Refusing to advance would make this pod re-derive a window it can
		// never win; writing marks would claim credit for a report it did not make, in a
		// transaction that has already been rolled back.
		assert.Equal(t, []time.Time{newestClosedTo}, rig.reads.advanced,
			"the losing pod did not advance past a window that is demonstrably covered, so "+
				"it re-derives and re-loses it on every tick until the window falls off the "+
				"backfill horizon")
		assert.Empty(t, rig.reads.marks,
			"the losing pod marked episodes for a digest it did not write. Its transaction "+
				"was rolled back, so the marks would have to be a second statement outside "+
				"it — which is the write that can commit without its digest")
	})
}

// TestAPolicyThatDoesNotRouteTheDigestReasonIsSkipped.
//
// `policies_digest_reason_ck` makes the window imply the Reason, so this is a row the
// database says cannot exist. The tick checks anyway, because minting the intent
// would record it and immediately suppress it as `no_policy` — once per window,
// forever, in the audit log that exists to make real suppression visible.
func TestAPolicyThatDoesNotRouteTheDigestReasonIsSkipped(t *testing.T) {
	t.Parallel()

	p := tickPolicy(10*time.Minute, 1)
	p.Reasons = []domain.Reason{domain.ReasonFired}

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 9)...),
	}
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, sent)
	assert.Empty(t, rig.notifs.inserted,
		"a policy whose `reasons` omit `digest` minted one anyway; every window of it would "+
			"be recorded and instantly suppressed as no_policy")
	assert.Empty(t, rig.reads.advanced,
		"a policy the tick refuses to sweep had its coverage advanced, so the windows it "+
			"skipped are recorded as examined and its episodes are unmarked forever — "+
			"reported by nothing and invisible to the reconciler")
	assert.Contains(t, rig.logs.String(), "does not route the digest reason")
}

// TestAWindowAnotherTickAlreadyCoveredDoesNotCostThePolicyItsOtherWindows is the
// "another pod won the window" case read one level up: what happens to the windows
// BEHIND the one that was already covered.
//
// ⛔⛔ THE RACE IS REAL AND IT IS NOT THE SAME KEY. Two ticks straddling a
// `digest_window_s` edit compute the SAME `window_start` and a DIFFERENT window
// ordinal, so the §C.7 idempotency key differs and `notifications.Insert`'s
// `ON CONFLICT (org_id, idempotency_key)` arbiter does not fire. `notif_digest_uniq`
// (org_id, policy_id, digest_window_start) does, as a bare 23505 — and a 23505 that
// reaches Go has already aborted the transaction. Answering it with `nil` inside
// `InTx` asks for a COMMIT that Postgres can only refuse (`ErrTxCommitRollback`), so
// the benign race came back out of `emit` as an ERROR, `sweepPolicy` returned on it,
// and the two windows the policy still owed were dropped for the whole tick — the
// silence a digest exists to prevent, produced by the digest.
func TestAWindowAnotherTickAlreadyCoveredDoesNotCostThePolicyItsOtherWindows(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		// Three ten-minute windows are owed: 13:10, 13:20 and 13:30.
		coveredTo: time.Date(2026, 8, 18, 13, 10, 0, 0, time.UTC),
		cases:     perWindow("observability", 9),
	}
	// The FIRST of them is already covered by another pod; the two behind it are not.
	notifs := &digestNotifications{conflictOnce: true}
	rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 1), reads, notifs)

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Equal(t, 2, sent,
		"the tick sent %d digests. A window somebody else covered is an ANSWER, not a "+
			"failure: the two windows behind it are still owed and must still be written",
		sent)

	var got []time.Time
	for _, n := range notifs.inserted {
		require.NotNil(t, n.DigestWindowStart)
		got = append(got, n.DigestWindowStart.UTC())
	}
	assert.Equal(t, []time.Time{
		time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC),
	}, got, "the covered window is skipped and only it")
	assert.Len(t, rig.deliver.created, 2, "one fan-out per digest actually minted")
	assert.Equal(t, newestClosedTo, rig.reads.advanced[len(rig.reads.advanced)-1],
		"coverage must reach past all three windows: one was covered by another pod and two "+
			"were sent by this one, and every one of them was examined to a conclusion")
}

// TestAPolicyWithNoLiveDestinationKeepsOwingItsWindow is the one outcome that must NOT
// advance the cursor, and it is the case where the two halves of git-bug `893cee4` pull
// in opposite directions.
//
// ⭐⭐ "EXAMINED AND FOUND QUIET" AND "COULD NOT BE EXAMINED TO A CONCLUSION" ARE
// DIFFERENT, AND CONFLATING THEM TRADES ONE BUG FOR ANOTHER. Coverage MUST advance for a
// window below its floor, or a quiet policy re-derives it forever. It must NOT advance
// for a window whose policy has no live channel: nothing was recorded, on purpose —
// recording a `channel_disabled` row would burn the window's idempotency key, so
// re-enabling a channel two minutes later would produce no digest for a window that HAD
// cleared its floor. Advancing past it would turn a recoverable configuration mistake
// into permanent silence, which is the shape of the bug we came here to fix.
func TestAPolicyWithNoLiveDestinationKeepsOwingItsWindow(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(episodes("observability", newestClosed.Add(time.Minute), 9)...),
	}
	rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 1), reads, &digestNotifications{})
	// Every destination has gone: disabled, deleted, or the ids no longer resolve.
	rig.dests.channels = nil

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, sent)
	assert.Empty(t, rig.notifs.inserted,
		"a digest with nowhere to go was recorded anyway, which burns the window's "+
			"idempotency key: re-enabling the channel would then produce no digest for a "+
			"window that had cleared its floor")
	assert.Empty(t, rig.reads.advanced,
		"coverage advanced past a window that was never examined to a conclusion. The "+
			"window is still OWED — nothing was sent and nothing was recorded — and jumping "+
			"it turns `somebody disabled the channel for ten minutes` into a summary nobody "+
			"will ever receive")
	assert.Empty(t, rig.reads.marks,
		"the episodes were marked as accounted for by a digest that does not exist, so the "+
			"reconciler would report no gap while the operator was told nothing")
}

// TestTheReconcilerCountsEpisodesNobodyWasToldAbout is the half of the ruling that makes
// the bounded lookback defensible rather than hopeful.
//
// ⭐⭐ `DigestLookback` DOWNGRADES A MISSED CASE FROM AN INVISIBLE HOLE TO A DUPLICATE —
// BUT ONLY FOR LATENESS UNDER `L`. Past that the episode is still lost, and pre-release
// the goal is AUDITABLE rather than provably correct: if it happens, it must be found
// from a number somebody can alarm on and not from a customer. This is that number.
//
// ⛔ AND IT CANNOT BE A SQL ANTI-JOIN, WHICH IS WHY IT IS A FOLD. "Cases older than
// `now - L` that appear in no digest" cannot be written: a Case appears in no digest
// either because it was MISSED or because NO POLICY SELECTS IT, and which policies select
// it is decided in Go by `Policy.Matches` — an Alertmanager-anchored regular expression
// whose missing-label rule (absent means empty string) Postgres's `~` does not share. A
// detector with its own second implementation of matching would disagree with the tick,
// and every disagreement would look like data loss. So it reuses the tick's matcher and
// the tick's own read, which is what this test pins.
func TestTheReconcilerCountsEpisodesNobodyWasToldAbout(t *testing.T) {
	t.Parallel()

	// One episode the policy matched and nothing ever accounted for, and one in a
	// namespace the policy does not select at all.
	lost := episode("observability", tickNow.Add(-3*time.Hour), 1)
	notMine := episode("payments", tickNow.Add(-2*time.Hour), 2)
	accounted := episode("observability", tickNow.Add(-time.Hour), 3)

	reads := &digestReads{
		coveredTo: newestClosedTo,
		cases:     sameEvery(lost, notMine, accounted),
		marked:    map[uuid.UUID]struct{}{accounted.ID: {}},
	}
	p := tickPolicy(10*time.Minute, 1,
		domain.Matcher{Name: "namespace", Op: domain.OpEqual, Value: "observability"})
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	got, err := rig.svc.ReconcileOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Equal(t, 1, got.Policies)
	assert.Equal(t, 1, got.Unreported,
		"the reconciler counted %d unreported episodes and there is exactly one. Counting "+
			"the `payments` episode too would be the phantom gap: no digest mentions it "+
			"because no policy selects it, which is not data loss. Counting the marked one "+
			"would mean the receipt is being ignored", got.Unreported)
	assert.Equal(t, 1, got.Episodes)
	assert.Equal(t, lost.StartedAt.UTC(), got.Oldest.UTC(),
		"the oldest unreported episode is the field that says whether this is a live "+
			"problem or a scar")

	// ⛔⛔ THE CANDIDATE SPAN ENDS AT THIS POLICY'S OWN LAST CHANCE, NOT AT
	// `now - DigestLookback`. It used to end at the latter for the whole tenant, and that
	// bound reports a HEALTHY install: the tick only ever examines CLOSED windows, so
	// everything in the currently-open one is unmarked by construction. Here coverage has
	// reached 13:40 and the read must stop two minutes before it — every future window
	// this policy examines reads from `coveredTo - L` at the earliest, so 13:38 is the
	// instant past which an episode can never be reported again. See
	// `domain.Digest.UnreportableBefore`.
	require.Len(t, rig.reads.asked, 1)
	assert.Equal(t, newestClosedTo.Add(-domain.DigestLookback), rig.reads.asked[0].end,
		"the reconciler read up to %s. Anything later than this policy's own coverage "+
			"instant minus the lookback is an episode whose digest has not been minted yet, "+
			"and reporting it as unreported is the false alarm that makes the number "+
			"worthless", rig.reads.asked[0].end)
	assert.Equal(t, tickNow.Add(-domain.DigestReconcileHorizon), rig.reads.asked[0].start)

	assert.Contains(t, rig.logs.String(), "episodes nobody was told about")

	// The retention sweep rides the same job, because the mark table is the only
	// unbounded thing this design added and this is the only slow-cadence digest job
	// there is. Its horizon sits OUTSIDE the reconciler's, or the absence of a swept
	// mark would be read as a gap.
	require.Len(t, rig.reads.pruned, 1)
	assert.Equal(t, tickNow.Add(-domain.DigestMarkRetention), rig.reads.pruned[0])
	assert.True(t, rig.reads.pruned[0].Before(rig.reads.asked[0].start),
		"marks are pruned inside the window the reconciler reads, so every episode older "+
			"than the retention horizon would be reported as a gap on every single run")
}

// TestTheReconcilerIsQuietOnAHealthyDailyDigest is the false alarm the per-policy bound
// exists to stop, driven rather than described.
//
// ⛔⛔ THE OLD BOUND WARNED FOREVER ON AN INSTALL WITH NOTHING WRONG WITH IT. The
// candidate span ended at `now - DigestLookback` for the WHOLE TENANT, and the tick only
// ever examines CLOSED windows — so for `digest_window_s = 86400` an hourly pass at
// 12:00 read up to 11:58 while marks could only exist up to 00:00, and every episode that
// opened today was reported as "digest episodes nobody was told about". Forty Cases a day
// meant roughly twenty phantom gaps an hour, every hour, on the one number `DigestGap`
// claims is zero on a healthy install and safe to alert on. It is the exact shape of
// git-bug `893cee4`, re-created by the detector built to replace it.
//
// The three episodes below are the whole test: yesterday's is marked because yesterday's
// window was digested, and today's two are unmarked because today's window has not
// closed. A detector that counts either of the second pair is broken.
func TestTheReconcilerIsQuietOnAHealthyDailyDigest(t *testing.T) {
	t.Parallel()

	midnight := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	yesterday := episode("observability", midnight.Add(-3*time.Hour), 1)
	thisMorning := episode("observability", midnight.Add(2*time.Hour), 2)
	justNow := episode("observability", tickNow.Add(-time.Minute), 3)

	reads := &digestReads{
		// The tick digested yesterday's window shortly after midnight and has examined
		// nothing since, because nothing has closed since. This is what healthy looks
		// like for a one-day window.
		coveredTo: midnight,
		cases:     sameEvery(yesterday, thisMorning, justNow),
		marked:    map[uuid.UUID]struct{}{yesterday.ID: {}},
	}
	p := tickPolicy(24*time.Hour, 1,
		domain.Matcher{Name: "namespace", Op: domain.OpEqual, Value: "observability"})
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	got, err := rig.svc.ReconcileOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, got.Unreported,
		"the reconciler reported %d unreported pairs on a healthy install. Both of the "+
			"episodes it can only have counted opened inside the window that is still OPEN, "+
			"so no digest for them has been minted and none could have been: the tick "+
			"examines closed windows only", got.Unreported)
	assert.Zero(t, got.Episodes)
	assert.True(t, got.Oldest.IsZero())
	assert.NotContains(t, rig.logs.String(), "episodes nobody was told about",
		"a healthy install produced the WARN this detector exists to make trustworthy. An "+
			"hourly job that says this every hour forever is how the one run that matters "+
			"gets buried. Log: %s", rig.logs.String())

	// AND THE READ STOPPED AT THE COVERAGE INSTANT MINUS THE LOOKBACK, which is the
	// bound doing it rather than the fixture happening to agree.
	require.Len(t, rig.reads.asked, 1)
	assert.Equal(t, midnight.Add(-domain.DigestLookback), rig.reads.asked[0].end)
}

// TestTheReconcilerSkipsAPolicyThatHasNeverBeenExamined is the other half of the bound:
// a zero coverage instant means "ask nothing", not "everything is a gap".
//
// A brand-new digest policy is owed exactly ONE window — the most recent closed one —
// because enabling a digest must not replay last week into a channel (`DigestWindows`).
// Everything older was never owed, so a detector that folded it would make every policy
// an operator creates announce a day-long gap in the same minute it was saved.
func TestTheReconcilerSkipsAPolicyThatHasNeverBeenExamined(t *testing.T) {
	t.Parallel()

	old := episode("observability", tickNow.Add(-5*time.Hour), 1)
	reads := &digestReads{
		// No coverage row: the tick has never examined this policy.
		cases: sameEvery(old),
	}
	p := tickPolicy(10*time.Minute, 1)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	got, err := rig.svc.ReconcileOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, got.Policies,
		"a policy with no coverage instant was folded. It has never been examined, it is "+
			"owed exactly the newest closed window and nothing before it, so every episode "+
			"in the horizon would be reported as lost the moment the policy was created")
	assert.Zero(t, got.Unreported)
	assert.Empty(t, rig.reads.asked,
		"the tenant-wide episode read ran with no policy to judge against, which is the "+
			"most expensive query in the detector spent on nothing")
	require.Len(t, rig.reads.pruned, 1,
		"the retention sweep must run whatever else happens — it is first-class rather "+
			"than a favour, and this is the only slow-cadence digest job there is")
}
