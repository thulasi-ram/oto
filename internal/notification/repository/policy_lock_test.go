package repository_test

// judgment 2, E6: an applied count Suggestion compares the policy and writes it under the row
// lock LockPolicy takes, so a hand edit cannot commit between its stale check and its edit. The
// lock is held as Postgres holds it: an edit from another connection WAITS for the locking
// transaction, and lands only once it ends.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

func TestLockPolicyHoldsAHandEditUntilTheApplyEnds(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	h := fx.h
	repo := repository.NewConfigRepository(h.Pool, h.Clock)
	pol, err := repo.CreatePolicy(h.Ctx, fx.scope, domain.PolicyDraft{
		Name: "page-sre", Reasons: []domain.Reason{domain.ReasonFired}, ChannelIDs: []uuid.UUID{fx.channel},
	})
	require.NoError(t, err)

	locked := make(chan struct{})
	release := make(chan struct{})
	applied := make(chan error, 1)
	go func() {
		applied <- db.Tx(h.Ctx, h.Pool, func(ctx context.Context) error {
			got, err := repo.LockPolicy(ctx, fx.scope, pol.ID)
			if err != nil {
				return err
			}
			if got.ID != pol.ID {
				t.Errorf("LockPolicy read %s, want %s", got.ID, pol.ID)
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	// ⛔ While the apply holds the lock, a hand edit from another connection cannot land.
	name := "page-sre-eu"
	short, cancel := context.WithTimeout(h.Ctx, 300*time.Millisecond)
	_, err = repo.UpdatePolicy(short, fx.scope, pol.ID, domain.PolicyPatch{Name: &name})
	cancel()
	require.Error(t, err, "a hand edit committed while an applied Suggestion held the policy's lock")

	close(release)
	require.NoError(t, <-applied)
	edited, err := repo.UpdatePolicy(h.Ctx, fx.scope, pol.ID, domain.PolicyPatch{Name: &name})
	require.NoError(t, err, "once the apply ended, the hand edit lands")
	require.Equal(t, name, edited.Name)
}
