package service_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/id"
)

// snoozeRig is a notifier wired to a policy that routes the two snooze Reasons.
//
// It is not `newDispatchRig`: that one's policy routes `fired` and
// `all_resolved`, and what is under test here is a Reason it does not carry. No
// dispatcher either — every claim these tests make is about the rows the fan-out
// writes, and the root is landed directly on the thread rather than by sending
// anything.
type snoozeRig struct {
	fx       fixture
	notifier *service.NotificationService
	threads  *repository.ThreadRepository
	clk      clock.Clock
}

func newSnoozeRig(t *testing.T) snoozeRig {
	t.Helper()

	fx := newFixture(t, domain.CapThreading|domain.CapAmend)
	clk := clock.New()

	channels := repository.NewChannelRepository(fx.pool)
	deliveries := repository.NewDeliveryRepository(fx.pool)
	threads := repository.NewThreadRepository(fx.pool)

	policies, err := service.NewPolicyService(policyStore{policy: domain.Policy{
		ID: fx.policyID, OrgID: fx.orgID, Name: "all", Priority: 1, Enabled: true,
		Reasons: []domain.Reason{
			domain.ReasonFired, domain.ReasonSnoozed, domain.ReasonUnsnoozed,
		},
		ChannelIDs: []uuid.UUID{fx.channel.ID},
	}}, channels)
	require.NoError(t, err)

	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            txRunner{pool: fx.pool},
		Policies:      policies,
		Notifications: repository.NewNotificationRepository(fx.pool),
		Deliveries:    deliveries,
		Threads:       threads,
		Snapshots:     snapshots{fx: fx},
		Events:        repository.NewEventRepository(fx.pool, clk),
		Enqueuer:      &enqueuer{},
		Channels:      channels,
		Clock:         clk,
	})
	require.NoError(t, err)

	return snoozeRig{fx: fx, notifier: notifier, threads: threads, clk: clk}
}

// snooze evaluates one `snoozed` fact about one snooze row.
//
// `AlertID` is set because `snoozed` allocates `SubjectAlert` (reason.go) and
// notifications_subject_ck wants the typed column its kind names — which is also
// what production sends: `notifySnoozeChange` names the alert.
func (h snoozeRig) snooze(t *testing.T, snoozeID uuid.UUID) service.Result {
	t.Helper()
	alertID := h.fx.alertID
	res, err := h.notifier.Evaluate(t.Context(), h.fx.scope, service.Intent{
		CaseID:       h.fx.caseID,
		Reason:       domain.ReasonSnoozed,
		StateVersion: 1,
		OccasionID:   snoozeID,
		AlertID:      &alertID,
		Actor:        "someone@example.com",
	})
	require.NoError(t, err)
	return res
}

// modesOf is every delivery mode this notification fanned out to, in thread order.
func (h snoozeRig) modesOf(t *testing.T, notificationID uuid.UUID) []domain.Mode {
	t.Helper()
	rows, err := h.fx.pool.Query(t.Context(),
		`SELECT mode FROM notification_deliveries
		  WHERE org_id = $1 AND notification_id = $2
		  ORDER BY thread_seq NULLS FIRST`, h.fx.orgID, notificationID)
	require.NoError(t, err)
	defer rows.Close()

	var out []domain.Mode
	for rows.Next() {
		var mode string
		require.NoError(t, rows.Scan(&mode))
		out = append(out, domain.Mode(mode))
	}
	require.NoError(t, rows.Err())
	return out
}

// landRoot makes the destination's card exist without sending anything, which is
// what `update_root` turns on (§H.6's root column reads `RootLanded`).
func (h snoozeRig) landRoot(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	now := h.clk.Now().UTC()

	th, err := h.threads.Ensure(ctx, h.fx.scope, h.fx.channel.ID,
		domain.SubjectCase, h.fx.caseID, now)
	require.NoError(t, err)
	require.False(t, th.RootLanded(), "a fresh thread has no card yet")

	seq, err := h.threads.AllocateSeq(ctx, h.fx.scope, th.ID, now)
	require.NoError(t, err)
	require.NoError(t, h.threads.RecordRoot(ctx, h.fx.scope, th.ID,
		"C123", "1700000000.000200", id.New(), seq, now))

	th, err = h.threads.Get(ctx, h.fx.scope, th.ID)
	require.NoError(t, err)
	require.True(t, th.RootLanded())
}

// TestASecondSnoozeAmendsTheCardInsteadOfGoingSilent is the whole of the defect.
//
// A human snoozes for 1h and then, thinking better of it, for 4h. The DATA was
// always right — `Service.Snooze` closes the incumbent as `superseded`, writes the
// new row and appends both events — and the ANNOUNCEMENT was what went missing.
// The §C.7 key is (org, subject_kind, subject_id, reason, state_version), all five
// of which are IDENTICAL across the two presses: same alert, same reason, and
// `state_version` is `alert_cases.state_version`, which a snooze cannot move
// because `StartSnooze` takes an Alert and a snooze is not a Case state
// transition. So the second intent was byte-identical, `notifications_idem_uniq`
// dropped it, and the channel went on saying "quiet until 17:00" forever.
//
// The occasion — the `alert_snoozes.id` — is what tells the two apart, and the
// card is AMENDED rather than re-posted, which is ADR 0008's rule (migration
// 00069: "chat.update in place is PRIMARY; thread replies are the exception").
func TestASecondSnoozeAmendsTheCardInsteadOfGoingSilent(t *testing.T) {
	t.Parallel()

	h := newSnoozeRig(t)
	h.landRoot(t)

	first := h.snooze(t, id.New())
	require.True(t, first.Created)
	require.Contains(t, h.modesOf(t, first.Notification.ID), domain.ModeUpdateRoot,
		"a snooze on a card that exists amends it")

	// The second press, on a DIFFERENT snooze row. Everything else about the fact is
	// the same, which is precisely why it used to vanish.
	second := h.snooze(t, id.New())
	require.True(t, second.Created,
		"the second snooze must be a NEW notification: the operator changed the quiet period")
	require.NotEqual(t, first.Notification.IdempotencyKey, second.Notification.IdempotencyKey,
		"the occasion is the only §C.7 component that can differ here, so it must")
	require.Equal(t, first.Notification.StateVersion, second.Notification.StateVersion,
		"and it is NOT the state version doing the work: a snooze never moves the Case lock")

	require.Equal(t, h.modesOf(t, first.Notification.ID), h.modesOf(t, second.Notification.ID),
		"the second announcement is delivered the same way as the first — an amend, not a second card")
	require.Contains(t, h.modesOf(t, second.Notification.ID), domain.ModeUpdateRoot)
	require.NotContains(t, h.modesOf(t, second.Notification.ID), domain.ModePostRoot,
		"a second root card would read as a second incident to everybody in the channel")
}

// TestARetriedPressProducesOneCard is the other half, and it is the reason the
// occasion is the SNOOZE ID rather than a nonce.
//
// Two mechanisms guard a snooze and they are not the same mechanism. The Slack
// idempotency key (sha256 of the interaction's `response_url`) stops ONE PRESS
// being APPLIED TWICE when River rescues a job whose transaction had already
// committed; that press's replay never reaches this layer at all, because the
// transaction that would have enqueued the evaluation is rolled back. This one
// stops a duplicate CARD: a `notify.evaluate` job that is redelivered — which the
// queue guarantees will happen — carries the same occasion, mints the same key,
// and is swallowed by `notifications_idem_uniq` exactly as it always was.
//
// A nonce here would have broken that: every redelivery would have been a fresh
// key and a fresh card.
func TestARetriedPressProducesOneCard(t *testing.T) {
	t.Parallel()

	h := newSnoozeRig(t)
	h.landRoot(t)

	snoozeID := id.New()
	first := h.snooze(t, snoozeID)
	require.True(t, first.Created)
	require.NotZero(t, first.Deliveries)

	again := h.snooze(t, snoozeID)
	require.False(t, again.Created,
		"the same snooze evaluated twice is one announcement: §C.7 working, not an error")
	require.Zero(t, again.Deliveries, "and it creates no second delivery row")
	require.Equal(t, first.Notification.ID, again.Notification.ID)
	require.Equal(t, first.Notification.IdempotencyKey, again.Notification.IdempotencyKey)
	require.Contains(t, h.modesOf(t, first.Notification.ID), domain.ModeUpdateRoot,
		"and the one announcement that DID land is still an amend of the card")
}

// TestAFirstSnoozeWithNoCardYetStillPostsOne pins the no-root path, which the
// occasion must not disturb.
//
// A snooze taken before any card has landed — the alert fired and the operator
// silenced it inside the pre-notification budget — has nothing to amend, and §H.6
// answers `post_root` for exactly this case. Silence would be the wrong
// degradation: the fresh card carries every field the amend would have changed,
// the snooze badge among them.
func TestAFirstSnoozeWithNoCardYetStillPostsOne(t *testing.T) {
	t.Parallel()

	h := newSnoozeRig(t)

	res := h.snooze(t, id.New())
	require.True(t, res.Created)
	require.Equal(t, []domain.Mode{domain.ModePostRoot}, h.modesOf(t, res.Notification.ID),
		"there is no card to amend, so the fact posts one")
}

// TestTheOccasionIsInertForEveryOtherReason is the additive claim, asserted rather
// than argued.
//
// A `fired` notification names no occasion, so its stored key must be the one
// `IdempotencyKey` computes with `uuid.Nil` — which the domain's golden pre-image
// test pins to the exact bytes §C.7 hashed before the field existed. If this goes
// red, adding the occasion re-keyed a Reason that never asked for one.
func TestTheOccasionIsInertForEveryOtherReason(t *testing.T) {
	t.Parallel()

	h := newSnoozeRig(t)

	res, err := h.notifier.Evaluate(t.Context(), h.fx.scope, service.Intent{
		CaseID: h.fx.caseID, Reason: domain.ReasonFired, StateVersion: 7,
	})
	require.NoError(t, err)
	require.True(t, res.Created)
	require.Equal(t,
		domain.IdempotencyKey(h.fx.orgID, domain.SubjectCase, h.fx.caseID,
			domain.ReasonFired, 7, uuid.Nil),
		res.Notification.IdempotencyKey,
		"a Reason that names no occasion is keyed over exactly the bytes it always was")
}
