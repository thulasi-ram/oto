package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// policyStore serves one hard-coded policy. `notification_policies` has its own
// repository and its own tests; what this test is about is the fan-out.
type policyStore struct{ policy domain.Policy }

func (p policyStore) ListLive(context.Context, db.TenantScope) ([]domain.Policy, error) {
	return []domain.Policy{p.policy}, nil
}

func (p policyStore) Get(context.Context, db.TenantScope, uuid.UUID) (domain.Policy, error) {
	return p.policy, nil
}

func (p policyStore) ListWithUnackedReminder(
	context.Context, db.TenantScope, *int,
) ([]domain.Policy, error) {
	return nil, nil
}

// TestEvaluateTwiceAllocatesOneSequence pins the fan-out's most expensive bug.
//
// `notify.evaluate` runs on an at-least-once queue and its re-runs are documented
// as normal — the fan-out deliberately runs again even when the notification
// already exists, because `Create` is `ON CONFLICT DO NOTHING` and converges.
// Allocating the thread sequence in FRONT of that insert did not converge: the
// second run committed `next_seq++` with no delivery behind it, and every
// delivery queued behind the empty slot then waited the full ordering MaxWait —
// fifteen minutes of complete silence on that thread — before gap recovery
// stepped past it. One duplicated job was enough to cause it.
func TestEvaluateTwiceAllocatesOneSequence(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, domain.CapThreading|domain.CapAmend)
	ctx := t.Context()

	channels := repository.NewChannelRepository(fx.pool)
	policies, err := service.NewPolicyService(policyStore{policy: domain.Policy{
		ID: fx.policyID, OrgID: fx.orgID, Name: "all", Priority: 1, Enabled: true,
		Reasons:    []domain.Reason{domain.ReasonFired},
		ChannelIDs: []uuid.UUID{fx.channel.ID},
	}}, channels)
	require.NoError(t, err)

	jobs := &enqueuer{}
	deliveries := repository.NewDeliveryRepository(fx.pool)
	threads := repository.NewThreadRepository(fx.pool)
	clk := clock.New()

	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            txRunner{pool: fx.pool},
		Policies:      policies,
		Notifications: repository.NewNotificationRepository(fx.pool),
		Deliveries:    deliveries,
		Threads:       threads,
		Snapshots:     snapshots{fx: fx},
		Events:        repository.NewEventRepository(fx.pool, clk),
		Enqueuer:      jobs,
		Channels:      channels,
		Clock:         clk,
	})
	require.NoError(t, err)

	intent := service.Intent{
		GroupID:      fx.groupID,
		Reason:       domain.ReasonFired,
		StateVersion: 1,
	}

	first, err := notifier.Evaluate(ctx, fx.scope, intent)
	require.NoError(t, err)
	require.True(t, first.Created)
	require.Equal(t, 1, first.Deliveries)

	// The SAME intent again: a redelivered job, which the queue guarantees will
	// happen and the service documents as normal.
	second, err := notifier.Evaluate(ctx, fx.scope, intent)
	require.NoError(t, err)
	require.False(t, second.Created, "the §C.7 idempotency key must swallow the second intent")
	require.Zero(t, second.Deliveries, "a re-run creates no delivery, so it must count none")

	th, err := threads.Ensure(ctx, fx.scope, fx.channel.ID,
		domain.SubjectAlertGroup, fx.groupID, clk.Now().UTC())
	require.NoError(t, err)

	next, lastSent := nextSeqOf(t, fx.pool, th.ID)
	require.Equal(t, 2, next,
		"next_seq must have advanced exactly once: one delivery, one sequence")
	require.Zero(t, lastSent)

	// And the number that was handed out is the one the row actually holds — no
	// slot exists that nothing will ever fill.
	var (
		rows int
		seq  int
	)
	require.NoError(t, fx.pool.QueryRow(ctx,
		`SELECT count(*), coalesce(min(thread_seq), 0)
		   FROM notification_deliveries WHERE org_id = $1 AND thread_id = $2`,
		fx.orgID, th.ID).Scan(&rows, &seq))
	require.Equal(t, 1, rows)
	require.Equal(t, 1, seq)

	require.Len(t, jobs.jobs, 1, "the second run must not enqueue a job for a row it did not create")
}
