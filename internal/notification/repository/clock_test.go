package repository_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about ONE defect on this module's four tables:
// `internal_error/policies_time_ck`, `notifications_time_ck`,
// `deliveries_time_ck` and `threads_time_ck` on an ordinary write, with nothing
// wrong.
//
// All four CHECKs are `updated_at >= created_at`. All four tables stamp both
// columns from the APPLICATION on INSERT — so unlike `channels` in 00032 there
// is no database clock involved — and then wrote a plain `updated_at = $n` on
// UPDATE. That is enough on its own, because "the application owns time" is not
// "one clock": oto runs N pods with N clocks, and on THIS module the writer is
// systematically a different process from the creator. A policy is created by an
// API pod and touched by the operator's next request; a notification is recorded
// by the pod that evaluated the lifecycle event and folded by a dispatch worker;
// a delivery and a thread are created inside `notify.evaluate` and then written
// by whichever worker `deliver.dispatch` landed on. A few milliseconds of lag
// writes an `updated_at` BELOW `created_at` and 23514s.
//
// ⛔ ON THE DELIVERY AND THREAD TABLES THAT IS NOT MERELY A 500. Those writes
// bracket a network call to a chat provider, so a constraint failure on the
// write AFTER the send loses the record of a message that has already landed:
// the job retries, sends again, and a human gets a duplicate at 3am for a clock
// reason. The writers now advance `updated_at` monotonically,
// GREATEST(updated_at, $n) — the idiom OrderingStore.Advance already used and
// whose comment named this hazard.
//
// The lag is deterministic and enormous rather than a flake: these repositories
// take `now` as an argument, so a second pod running two seconds behind is
// exactly a second call with an earlier instant.

func TestMain(m *testing.M) { harness.Main(m) }

// lag is how far behind the creating pod the writing pod's clock is.
const lag = 2 * time.Second

// fixture seeds the FK graph these four tables hang off: an org, a group
// generation to be the subject, and one webhook destination.
type fixture struct {
	h       *harness.H
	scope   db.TenantScope
	groupID uuid.UUID
	channel uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)

	channelID := id.New()
	// `created_at`/`updated_at` are NAMED and take the harness clock: 00032 took
	// this table's DEFAULT now() away.
	h.Exec(`INSERT INTO channels (id, org_id, type, name, config, renderer,
	           created_at, updated_at)
	        VALUES ($1, $2, 'webhook', $3, '{}'::jsonb, 'webhook.json', $4, $4)`,
		channelID, org.ID, "dest-"+org.Slug, h.Now())

	return fixture{h: h, scope: org.Scope, groupID: group.ID, channel: channelID}
}

// idem builds a `notifications_idem_ck`-shaped key: 64 lowercase hex characters.
func idem(seed string) string { return strings.Repeat(seed, 64) }

func TestPolicyEditSurvivesAPodBehindTheOneThatCreatedIt(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h

	creator := repository.NewConfigRepository(h.Pool, h.Clock)
	pol, err := creator.CreatePolicy(h.Ctx, fx.scope, domain.PolicyDraft{
		Name:       "page-sre",
		Reasons:    []domain.Reason{domain.ReasonFired},
		ChannelIDs: []uuid.UUID{fx.channel},
	})
	require.NoError(t, err)
	require.Equal(t, h.Now(), pol.CreatedAt.UTC(),
		"created_at must come from the injected clock, not from the database")

	name := "page-sre-eu"
	lagging := repository.NewConfigRepository(h.Pool, clock.NewFake(harness.Epoch.Add(-lag)))
	edited, err := lagging.UpdatePolicy(h.Ctx, fx.scope, pol.ID, domain.PolicyPatch{Name: &name})
	require.NoError(t, err,
		"a pod whose clock lags the row's creator must not 500 on policies_time_ck")
	require.Equal(t, name, edited.Name, "the edit itself still happened")
	require.Equal(t, pol.CreatedAt.UTC(), edited.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")

	// The other writer of the same column, which a fix applied only to
	// UpdatePolicy would leave behind.
	require.NoError(t, lagging.SoftDeletePolicy(h.Ctx, fx.scope, pol.ID),
		"a pod whose clock lags the row's creator must not 500 on policies_time_ck")

	deleted, err := creator.GetPolicy(h.Ctx, fx.scope, pol.ID)
	require.NoError(t, err)
	require.False(t, deleted.UpdatedAt.Before(deleted.CreatedAt),
		"policies_time_ck, restated in Go so the failure names the invariant")
	require.NotNil(t, deleted.DeletedAt)
	require.Equal(t, harness.Epoch.Add(-lag), deleted.DeletedAt.UTC(),
		"deleted_at is the deleting pod's OWN instant and is recorded verbatim")
}

// TestNotificationStatusSurvivesADispatcherBehindTheEvaluator is the one whose
// failure costs a duplicate message: the aggregate status is folded AFTER the
// fan-out has gone out, so a 23514 here retries a job whose side effect has
// already happened.
func TestNotificationStatusSurvivesADispatcherBehindTheEvaluator(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	repo := repository.NewNotificationRepository(h.Pool)

	n, created, err := repo.Insert(h.Ctx, fx.scope, domain.Notification{
		ID:             id.New(),
		OrgID:          fx.scope.OrgID(),
		SubjectKind:    domain.SubjectAlertGroup,
		SubjectID:      fx.groupID,
		GroupID:        fx.groupID,
		Reason:         domain.ReasonFired,
		StateVersion:   1,
		IdempotencyKey: idem("a"),
		Status:         domain.StatusPending,
		CreatedAt:      h.Now(),
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, h.Now(), n.CreatedAt.UTC(),
		"created_at must come from the caller's clock, not from the database")

	require.NoError(t, repo.SetStatus(h.Ctx, fx.scope, n.ID,
		domain.StatusDelivered, h.Now().Add(-lag)),
		"a dispatch worker whose clock lags the evaluator must not 500 on notifications_time_ck")

	stored, err := repo.Get(h.Ctx, fx.scope, n.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusDelivered, stored.Status,
		"the status change itself still happened; only the timestamp was clamped")
	require.Equal(t, stored.CreatedAt.UTC(), stored.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
}

// TestDeliveryWritesSurviveAWorkerBehindTheFanOut walks the whole dispatch
// sequence from a lagging worker, because every statement in the file is a
// separate chance to trip the same CHECK and a fix applied to one of them proves
// nothing about the rest.
func TestDeliveryWritesSurviveAWorkerBehindTheFanOut(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	notifications := repository.NewNotificationRepository(h.Pool)
	deliveries := repository.NewDeliveryRepository(h.Pool)
	threads := repository.NewThreadRepository(h.Pool)

	n, _, err := notifications.Insert(h.Ctx, fx.scope, domain.Notification{
		ID:             id.New(),
		OrgID:          fx.scope.OrgID(),
		SubjectKind:    domain.SubjectAlertGroup,
		SubjectID:      fx.groupID,
		GroupID:        fx.groupID,
		Reason:         domain.ReasonFired,
		StateVersion:   1,
		IdempotencyKey: idem("b"),
		Status:         domain.StatusPending,
		CreatedAt:      h.Now(),
	})
	require.NoError(t, err)

	th, err := threads.Ensure(h.Ctx, fx.scope, fx.channel,
		domain.SubjectAlertGroup, fx.groupID, h.Now())
	require.NoError(t, err)

	d, madeNew, err := deliveries.Create(h.Ctx, fx.scope, repository.NewDelivery{
		ID:             id.New(),
		NotificationID: n.ID,
		ChannelID:      fx.channel,
		ThreadID:       &th.ID,
		Mode:           domain.ModePostRoot,
		CreatedAt:      h.Now(),
	})
	require.NoError(t, err)
	require.True(t, madeNew)
	require.Equal(t, h.Now(), d.CreatedAt.UTC(),
		"created_at must come from the caller's clock, not from the database")

	// Everything below runs on a worker two seconds behind the pod that fanned
	// this delivery out.
	behind := h.Now().Add(-lag)

	require.NoError(t, deliveries.SetThreadSeq(h.Ctx, fx.scope, d.ID, 1, behind),
		"a worker whose clock lags the fan-out must not 500 on deliveries_time_ck")

	claimed, ok, err := deliveries.Claim(h.Ctx, fx.scope, d.ID, behind.Add(-time.Minute), behind)
	require.NoError(t, err,
		"a worker whose clock lags the fan-out must not 500 on deliveries_time_ck")
	require.True(t, ok, "the claim must still succeed; only the timestamp was clamped")
	require.False(t, claimed.UpdatedAt.Before(claimed.CreatedAt),
		"deliveries_time_ck, restated in Go so the failure names the invariant")

	require.NoError(t, deliveries.PersistRendered(h.Ctx, fx.scope, d.ID,
		json.RawMessage(`{"text":"hi"}`), strings.Repeat("0", 64), "hi", behind),
		"a worker whose clock lags the fan-out must not 500 on deliveries_time_ck")

	recorded, err := deliveries.MarkSent(h.Ctx, fx.scope, d.ID,
		"1712345678.000100", "C0123", json.RawMessage(`{}`), behind)
	require.NoError(t, err,
		"a worker whose clock lags the fan-out must not 500 on deliveries_time_ck")
	require.True(t, recorded, "the send must be recorded; losing it duplicates the message")

	sent, err := deliveries.Get(h.Ctx, fx.scope, d.ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliverySent, sent.Status)
	require.Equal(t, sent.CreatedAt.UTC(), sent.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging write may not drag the row backwards")
	require.NotNil(t, sent.SentAt)
	require.Equal(t, behind, sent.SentAt.UTC(),
		"sent_at is the sending pod's OWN instant and is recorded verbatim")
}

// TestClaimIsNotExpiredAtTheMomentItIsTaken is the second thing GREATEST buys on
// `notification_deliveries`, and it is not about a CHECK at all.
//
// `updated_at` IS the claim lease: `Claim`'s reclaim disjunct is `status =
// 'sending' AND provider_message_id IS NULL AND updated_at < $cutoff`. A lagging
// worker writing a plain `updated_at = $now` stamps its own fresh claim as
// already older than the lease, and the next worker along reclaims a delivery
// that is being sent RIGHT NOW — a duplicate message, from the mechanism that
// exists to prevent them.
func TestClaimIsNotExpiredAtTheMomentItIsTaken(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	notifications := repository.NewNotificationRepository(h.Pool)
	deliveries := repository.NewDeliveryRepository(h.Pool)

	n, _, err := notifications.Insert(h.Ctx, fx.scope, domain.Notification{
		ID:             id.New(),
		OrgID:          fx.scope.OrgID(),
		SubjectKind:    domain.SubjectAlertGroup,
		SubjectID:      fx.groupID,
		GroupID:        fx.groupID,
		Reason:         domain.ReasonFired,
		StateVersion:   1,
		IdempotencyKey: idem("c"),
		Status:         domain.StatusPending,
		CreatedAt:      h.Now(),
	})
	require.NoError(t, err)

	d, _, err := deliveries.Create(h.Ctx, fx.scope, repository.NewDelivery{
		ID:             id.New(),
		NotificationID: n.ID,
		ChannelID:      fx.channel,
		Mode:           domain.ModePostRoot,
		CreatedAt:      h.Now(),
	})
	require.NoError(t, err)

	// A worker a full minute behind takes the claim.
	behind := h.Now().Add(-time.Minute)
	_, ok, err := deliveries.Claim(h.Ctx, fx.scope, d.ID, behind.Add(-30*time.Second), behind)
	require.NoError(t, err)
	require.True(t, ok)

	// A second worker, on time, applies a 30-second lease. The first worker's
	// claim is 60 seconds old BY ITS OWN CLOCK and would be reclaimable if the
	// write had been allowed to land there.
	_, stolen, err := deliveries.Claim(h.Ctx, fx.scope, d.ID,
		h.Now().Add(-30*time.Second), h.Now())
	require.NoError(t, err)
	require.False(t, stolen,
		"a claim taken by a lagging worker must not be expired at the moment it is taken; "+
			"reclaiming it sends the same message twice")
}

// TestThreadWritesSurviveAWorkerBehindTheOneThatOpenedIt covers the table that
// IS oto's memory of the destination. A constraint failure on these statements
// leaves oto's record of where a message went behind the destination itself.
func TestThreadWritesSurviveAWorkerBehindTheOneThatOpenedIt(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	threads := repository.NewThreadRepository(h.Pool)

	th, err := threads.Ensure(h.Ctx, fx.scope, fx.channel,
		domain.SubjectAlertGroup, fx.groupID, h.Now())
	require.NoError(t, err)
	require.Equal(t, h.Now(), th.CreatedAt.UTC(),
		"created_at must come from the caller's clock, not from the database")

	behind := h.Now().Add(-lag)

	seq, err := threads.AllocateSeq(h.Ctx, fx.scope, th.ID, behind)
	require.NoError(t, err,
		"a worker whose clock lags the pod that opened the thread must not 500 on threads_time_ck")
	require.Equal(t, 1, seq)

	require.NoError(t, threads.RecordRoot(h.Ctx, fx.scope, th.ID,
		"C0123", "1712345678.000100", id.New(), seq, behind),
		"the statement that records where the root landed must not fail for a clock reason")

	seq2, err := threads.AllocateSeq(h.Ctx, fx.scope, th.ID, behind)
	require.NoError(t, err)
	require.NoError(t, threads.RecordReply(h.Ctx, fx.scope, th.ID, seq2, behind))
	require.NoError(t, threads.AdvanceSent(h.Ctx, fx.scope, th.ID, seq2, behind))
	require.NoError(t, threads.Freeze(h.Ctx, fx.scope, th.ID, behind))

	stored, err := threads.Get(h.Ctx, fx.scope, th.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ThreadFrozen, stored.State,
		"every state change still happened; only the timestamps were clamped")
	require.Equal(t, 1, stored.ReplyCount)
	require.Equal(t, stored.CreatedAt.UTC(), stored.UpdatedAt.UTC(),
		"updated_at is monotonic: the lagging writes may not drag the row backwards")
}

// TestTheFourTablesHaveNoClockOfTheirOwn pins the migration itself.
//
// A `DEFAULT now()` here is not inert, it is a trap with a delayed fuse: a
// writer that omits the columns succeeds, and the row it plants fails LATER, on
// a dispatch worker's UPDATE, as a 500 blaming a CHECK constraint. Without the
// default the same omission fails here, immediately, as a not-null violation.
func TestTheFourTablesHaveNoClockOfTheirOwn(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	h := fx.h
	orgID := fx.scope.OrgID()

	refused := func(what, sql string, args ...any) {
		t.Helper()
		_, err := h.Pool.Exec(h.Ctx, sql, args...)
		require.Errorf(t, err, "an INSERT into %s that omits created_at must be refused", what)

		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		require.Equal(t, "23502", pgErr.Code,
			"not_null_violation, at the statement that got it wrong")
		require.Equal(t, "created_at", pgErr.ColumnName)
	}

	refused("notification_policies",
		`INSERT INTO notification_policies (id, org_id, name, reasons, channel_ids)
		 VALUES ($1, $2, 'forgetful', ARRAY['fired'], ARRAY[$3::uuid])`,
		id.New(), orgID, fx.channel)

	refused("notifications",
		`INSERT INTO notifications (id, org_id, subject_kind, subject_id, group_id, reason,
		     state_version, idempotency_key)
		 VALUES ($1, $2, 'alert_group', $3, $3, 'fired', 1, $4)`,
		id.New(), orgID, fx.groupID, idem("d"))

	threadID := id.New()
	h.Exec(`INSERT INTO channel_threads (id, org_id, channel_id, subject_kind, subject_id,
	           created_at, updated_at)
	        VALUES ($1, $2, $3, 'alert_group', $4, $5, $5)`,
		threadID, orgID, fx.channel, fx.groupID, h.Now())

	notificationID := id.New()
	h.Exec(`INSERT INTO notifications (id, org_id, subject_kind, subject_id, group_id, reason,
	           state_version, idempotency_key, created_at, updated_at)
	        VALUES ($1, $2, 'alert_group', $3, $3, 'fired', 1, $4, $5, $5)`,
		notificationID, orgID, fx.groupID, idem("e"), h.Now())

	refused("notification_deliveries",
		`INSERT INTO notification_deliveries (id, org_id, notification_id, channel_id, mode)
		 VALUES ($1, $2, $3, $4, 'post_root')`,
		id.New(), orgID, notificationID, fx.channel)

	refused("channel_threads",
		`INSERT INTO channel_threads (id, org_id, channel_id, subject_kind, subject_id)
		 VALUES ($1, $2, $3, 'alert_group', $4)`,
		id.New(), orgID, fx.channel, id.New())
}
