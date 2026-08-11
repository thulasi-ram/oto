package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
)

// TestReRootedCardSaysItIsContinued covers the §H.9 transition end to end.
//
// oto's answer to a card it can no longer edit is to post a NEW one. That is the
// right answer — a visibly new root is a failure a human can SEE, and a silently
// stale card is one they cannot — but only if the new card says what it is.
// Without a marker, the recovery reads to everybody in the channel as a second
// incident, which is a different and equally wrong story.
func TestReRootedCardSaysItIsContinued(t *testing.T) {
	t.Parallel()

	tgt := &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)}
	h := newDispatchRig(t, tgt)
	ctx := t.Context()

	// The first root lands normally, and is NOT continued: it opens the thread.
	firstID := h.evaluate(t, domain.ReasonFired, 1)
	first := h.rowsFor(t, firstID)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, first[0].ID))

	h.renderer.mu.Lock()
	require.False(t, h.renderer.seen[0].Continued, "the first card of a generation continues nothing")
	h.renderer.mu.Unlock()

	// The workspace now refuses the edit. `edit_window_closed` is terminal for the
	// THREAD POINTER and fine for the destination (§H.9).
	tgt.mu.Lock()
	tgt.amendErr = &chdomain.Error{
		Class: chdomain.ClassPermanent, Provider: "webhook", Code: "edit_window_closed",
	}
	tgt.mu.Unlock()

	secondID := h.evaluate(t, domain.ReasonFired, 2)
	second := h.rowsFor(t, secondID)
	require.Len(t, second, 1)
	require.Equal(t, domain.ModeUpdateRoot, second[0].Mode)

	// Pass 1 kills the thread pointer and re-enqueues.
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[0].ID))
	th, err := h.threads.Ensure(ctx, h.fx.scope, h.fx.channel.ID,
		domain.SubjectAlertGroup, h.fx.groupID, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, domain.ThreadDead, th.State)
	require.Equal(t, domain.DeadEditWindowClosed, th.DeadReason)

	// Pass 2 sees the dead thread, clears the pointer and re-points the delivery.
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[0].ID))
	repointed, err := h.deliveries.Get(ctx, h.fx.scope, second[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.ModePostRoot, repointed.Mode)
	require.True(t, repointed.Ambiguous, "the previous card may still be sitting in the channel")

	// Pass 3 posts the fresh root — and it must announce itself as a continuation.
	tgt.mu.Lock()
	tgt.amendErr = nil
	tgt.mu.Unlock()
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[0].ID))

	sent, err := h.deliveries.Get(ctx, h.fx.scope, second[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliverySent, sent.Status)

	h.renderer.mu.Lock()
	last := h.renderer.seen[len(h.renderer.seen)-1]
	h.renderer.mu.Unlock()
	require.Equal(t, chdomain.ModePostRoot, last.Mode)
	require.True(t, last.Continued,
		"a root that replaces a card the channel already has must say so, or it reads as a second incident")
}

// TestMarkSentReportsALostClaim is the silent-zero-row case, stated directly.
//
// `MarkSent` is guarded by `status = 'sending'`, which is what stops a
// late-returning duplicate worker from overwriting a newer result. The guard
// matching zero rows used to be indistinguishable from success: the caller went
// on to record the thread's root handle from a claim it no longer held, and the
// send it had just made was recorded nowhere. A delivery that believes it was
// recorded but was not is exactly how a duplicate message reaches a human.
func TestMarkSentReportsALostClaim(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, domain.CapThreading|domain.CapAmend)
	ctx := t.Context()
	now := time.Now().UTC()

	deliveries := repository.NewDeliveryRepository(fx.pool)
	threads := repository.NewThreadRepository(fx.pool)

	th, err := threads.Ensure(ctx, fx.scope, fx.channel.ID,
		domain.SubjectAlertGroup, fx.groupID, now)
	require.NoError(t, err)

	notificationID := uuid.New()
	_, err = fx.pool.Exec(ctx, `
		INSERT INTO notifications
		  (id, org_id, subject_kind, subject_id, group_id, reason, policy_id,
		   state_version, idempotency_key, status, created_at, updated_at)
		VALUES ($1,$2,'alert_group',$3,$3,'fired',$4,1,$5,'dispatched',$6,$6)`,
		notificationID, fx.orgID, fx.groupID, fx.policyID, idemKey("c"), now)
	require.NoError(t, err)

	d, madeNew, err := deliveries.Create(ctx, fx.scope, repository.NewDelivery{
		ID:             uuid.New(),
		NotificationID: notificationID,
		ChannelID:      fx.channel.ID,
		ThreadID:       &th.ID,
		Mode:           domain.ModePostRoot,
		CreatedAt:      now,
	})
	require.NoError(t, err)
	require.True(t, madeNew)

	// A claimed row records normally.
	_, ok, err := deliveries.Claim(ctx, fx.scope, d.ID, now.Add(-2*time.Minute), now)
	require.NoError(t, err)
	require.True(t, ok)

	recorded, err := deliveries.MarkSent(ctx, fx.scope, d.ID,
		"1700000000.000300", "C123", nil, now)
	require.NoError(t, err)
	require.True(t, recorded)

	// The same call against a row that is no longer `sending` — which is precisely
	// what a lease expiring mid-call leaves behind — reports the loss instead of
	// pretending to have written.
	recorded, err = deliveries.MarkSent(ctx, fx.scope, d.ID,
		"1700000000.000400", "C123", nil, now)
	require.NoError(t, err)
	require.False(t, recorded, "a guard that matched nothing must never look like success")

	after, err := deliveries.Get(ctx, fx.scope, d.ID)
	require.NoError(t, err)
	require.Equal(t, "1700000000.000300", after.ProviderMessageID,
		"the first handle stands; the lost claim overwrote nothing")
}

// idemKey builds a 64-hex idempotency key, which notifications_idem_ck requires.
func idemKey(seed string) string { return hex(seed) }
