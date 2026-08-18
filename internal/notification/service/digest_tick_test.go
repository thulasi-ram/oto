package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
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
type digestTx struct{}

func (digestTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
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

func (p digestPolicies) ListWithUnackedReminder(
	context.Context, db.TenantScope, *int,
) ([]domain.Policy, error) {
	return nil, nil
}

func (p digestPolicies) ListWithDigest(
	context.Context, db.TenantScope,
) ([]domain.Policy, error) {
	return p.policies, nil
}

// digestSpans records every window the tick actually asked about, so a test can say
// which spans were queried and that they were half-open.
type digestSpans struct{ start, end time.Time }

// digestReads is the read model: the cursor and one window's buckets.
type digestReads struct {
	last    time.Time
	buckets []repository.DigestBucket
	asked   []digestSpans
}

func (d *digestReads) Buckets(
	_ context.Context, _ db.TenantScope, start, end time.Time, _ int,
) ([]repository.DigestBucket, error) {
	d.asked = append(d.asked, digestSpans{start: start, end: end})
	return d.buckets, nil
}

func (d *digestReads) LastWindow(
	context.Context, db.TenantScope, uuid.UUID,
) (time.Time, error) {
	return d.last, nil
}

// digestNotifications records the intents the tick minted, and can answer the way
// Postgres does when `notif_digest_uniq` has already been satisfied by another pod.
type digestNotifications struct {
	inserted []domain.Notification
	// conflict makes Insert raise a BARE 23505 on the digest index rather than the
	// idempotency one, which is the case `emit` has to read as "already covered".
	conflict bool
}

func (n *digestNotifications) Insert(
	_ context.Context, _ db.TenantScope, in domain.Notification,
) (domain.Notification, bool, error) {
	if n.conflict {
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

func (t *digestThreads) Freeze(context.Context, db.TenantScope, uuid.UUID, time.Time) error {
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
	context.Context, db.TenantScope, domain.Delivery, uuid.UUID, *uuid.UUID, string, time.Time,
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
		Tx:            digestTx{},
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

func bucket(namespace string, cases int) repository.DigestBucket {
	return repository.DigestBucket{
		GroupID:     uuid.New(),
		GroupLabels: map[string]string{"alertname": "Whatever", "namespace": namespace},
		Title:       "A group",
		Severity:    "critical",
		Cases:       cases,
	}
}

// The tick's frame of reference: 13:47 is inside the OPEN 13:40 window, so the
// newest CLOSED ten-minute window is 13:30.
var (
	tickNow        = time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)
	newestClosed   = time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)
	newestClosedTo = time.Date(2026, 8, 18, 13, 40, 0, 0, time.UTC)
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
		last:    time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		buckets: []repository.DigestBucket{bucket("observability", 3)},
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

	// The window that WAS examined is the newest closed one, half-open. An episode
	// that opened at exactly 13:40 belongs to the next window, not this one.
	require.Len(t, rig.reads.asked, 1)
	assert.Equal(t, digestSpans{start: newestClosed, end: newestClosedTo}, rig.reads.asked[0],
		"the tick counted the wrong span. Adjacent windows share a boundary instant, so a "+
			"closed interval would count an episode that opened on it in both windows")
}

// TestAWindowThatClearsItsFloorIsOneDigestAboutThePolicyAndTheWindow: the subject is
// the PAIR, and the row says so in three places at once.
func TestAWindowThatClearsItsFloorIsOneDigestAboutThePolicyAndTheWindow(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		last:    time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		buckets: []repository.DigestBucket{bucket("observability", 5)},
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
	assert.Equal(t, uuid.Nil, n.GroupID,
		"a digest claimed a delivery group. It spans many generations, which is the whole "+
			"reason migration 00058 relaxed `notifications.group_id`")
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
		domain.IdempotencyKey(p.OrgID, domain.SubjectDigest, p.ID, domain.ReasonDigest, n.StateVersion),
		n.IdempotencyKey)

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

	reads := &digestReads{
		last: time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		buckets: []repository.DigestBucket{
			bucket("observability", 4),
			bucket("payments", 40),
			bucket("observability", 3),
		},
	}
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

// TestAMissedTickWritesAtMostSixDigestsAndSaysWhatItDropped is done-when #2 across a
// restart, and the bound is the whole point.
//
// Covering everything owed is an outage amplifier — a five-minute policy down for a
// day owes 288 digests, all arriving in one second, which is the flood a digest
// exists to prevent produced by its own catch-up. Covering only the newest makes the
// cursor pointless. So: the newest `MaxDigestBackfill`, and the abandoned span is
// said out loud, because a damper that cannot report itself is the silent
// suppression §B.6 refuses.
func TestAMissedTickWritesAtMostSixDigestsAndSaysWhatItDropped(t *testing.T) {
	t.Parallel()

	reads := &digestReads{
		// Fifteen ten-minute windows are owed: 11:10 through 13:30.
		last:    time.Date(2026, 8, 18, 11, 5, 0, 0, time.UTC),
		buckets: []repository.DigestBucket{bucket("observability", 2)},
	}
	p := tickPolicy(10*time.Minute, 1)
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Equal(t, domain.MaxDigestBackfill, sent)
	require.Len(t, rig.notifs.inserted, domain.MaxDigestBackfill,
		"one tick wrote %d digests for one policy. `MaxDigestBackfill` is what caps a tick's "+
			"work at six queries per policy however long the process was gone",
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

	assert.Contains(t, rig.logs.String(), "skipped_windows",
		"the nine abandoned windows were dropped without a word. They are recoverable only "+
			"as an absence in the data, so the log line is the only place oto says it stopped "+
			"short")
	assert.Contains(t, rig.logs.String(), "too far behind")
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
			last:    newestClosed,
			buckets: []repository.DigestBucket{bucket("observability", 9)},
		}
		rig := newDigestRig(t, tickNow, tickPolicy(10*time.Minute, 1), reads, &digestNotifications{})

		sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
		require.NoError(t, err)

		assert.Zero(t, sent)
		assert.Empty(t, rig.notifs.inserted, "a window already covered was digested again")
		assert.Empty(t, rig.reads.asked,
			"the tick counted a window it owed nothing for. This is the answer on all but "+
				"the first tick of each window, and it is what makes a once-a-minute tick "+
				"affordable")
	})

	t.Run("another pod won the window", func(t *testing.T) {
		t.Parallel()

		reads := &digestReads{
			last:    time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
			buckets: []repository.DigestBucket{bucket("observability", 9)},
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
		last:    time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		buckets: []repository.DigestBucket{bucket("observability", 9)},
	}
	rig := newDigestRig(t, tickNow, p, reads, &digestNotifications{})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Zero(t, sent)
	assert.Empty(t, rig.notifs.inserted,
		"a policy whose `reasons` omit `digest` minted one anyway; every window of it would "+
			"be recorded and instantly suppressed as no_policy")
	assert.Contains(t, rig.logs.String(), "does not route the digest reason")
}
