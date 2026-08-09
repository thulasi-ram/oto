package repository_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// `channel_credentials` has the same shape as `channels` did: a DB-defaultable
// `created_at` against a `rotated_at` that the Go clock stamps, with
// `channel_credentials_rot_ck` — `rotated_at IS NULL OR rotated_at >= created_at`
// — comparing the two. Production Create already named `created_at`, so the
// default was reachable only from non-repository inserts; that is exactly the
// "unreachable" the `channels` default had before it cost half a day. 00033
// takes the default away and makes `rotated_at` monotonic.

// TestCredentialTimestampsComeFromTheApplicationClock pins what Create already
// did, and states why it must keep doing it.
func TestCredentialTimestampsComeFromTheApplicationClock(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCredentialRepository(h.Pool, fakeSealer{}, nil, h.Clock)

	meta, err := repo.Create(h.Ctx, org.Scope, "slack_bot_token", map[string]string{"token": "xoxb-1"})
	require.NoError(t, err)

	// If the row had taken the database's clock, `created_at` would be the
	// container's wall clock — months ahead of Epoch — and every rotation
	// assertion below would be about a different hazard.
	require.Equal(t, h.Now(), meta.CreatedAt.UTC(),
		"created_at must come from the injected clock, not from the database")
	require.Nil(t, meta.RotatedAt, "a freshly sealed secret has never been rotated")
}

// TestCredentialRotationSurvivesAPodBehindTheOneThatSealedIt is the half the
// migration alone does not fix.
//
// Every timestamp on this row comes from the application, but "the application"
// is N pods and N clocks, and the pod rotating a secret is rarely the pod that
// sealed it. A plain `rotated_at = $6` from a lagging pod lands BELOW
// `created_at` and fails channel_credentials_rot_ck with a 23514 — a 500 on an
// ordinary rotation. GREATEST(created_at, rotated_at, $6) makes the check
// unfalsifiable while leaving the value app-owned.
func TestCredentialRotationSurvivesAPodBehindTheOneThatSealedIt(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCredentialRepository(h.Pool, fakeSealer{}, nil, h.Clock)

	meta, err := repo.Create(h.Ctx, org.Scope, "slack_bot_token", map[string]string{"token": "xoxb-1"})
	require.NoError(t, err)

	// A second pod, two seconds behind the one that sealed the secret, rotates it.
	lagging := repository.NewCredentialRepository(h.Pool, fakeSealer{}, nil,
		clock.NewFake(h.Now().Add(-2*time.Second)))
	rotated, err := lagging.Rotate(h.Ctx, org.Scope, meta.ID, "slack_bot_token",
		map[string]string{"token": "xoxb-2"})
	require.NoError(t, err,
		"a pod whose clock lags the row's creator must not 500 on channel_credentials_rot_ck")

	require.NotNil(t, rotated.RotatedAt)
	require.Equal(t, meta.CreatedAt.UTC(), rotated.RotatedAt.UTC(),
		"rotated_at is floored at created_at: a lagging pod may not report the secret as sealed after it was rotated")

	// The SECOND floor: a later rotation from a lagging pod must not drag
	// `rotated_at` back below where an earlier one already put it.
	h.Advance(time.Hour)
	ahead := repository.NewCredentialRepository(h.Pool, fakeSealer{}, nil, h.Clock)
	_, err = ahead.Rotate(h.Ctx, org.Scope, meta.ID, "slack_bot_token", map[string]string{"token": "xoxb-3"})
	require.NoError(t, err)

	backwards, err := lagging.Rotate(h.Ctx, org.Scope, meta.ID, "slack_bot_token",
		map[string]string{"token": "xoxb-4"})
	require.NoError(t, err)
	require.Equal(t, h.Now(), backwards.RotatedAt.UTC(),
		"rotated_at is monotonic: the lagging pod's rotation may not make the secret look older than it is")
}

// TestChannelCredentialsTableHasNoClockOfItsOwn pins the migration itself, for
// the reason 00033 gives: a default nobody exercises is not inert, it is a trap
// that lets a future writer plant a row from the wrong clock and fail somewhere
// else, much later.
func TestChannelCredentialsTableHasNoClockOfItsOwn(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	_, err := h.Pool.Exec(h.Ctx,
		`INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version)
		 VALUES ($1, $2, 'slack_bot_token', decode(repeat('00', 32), 'hex'), 1)`,
		id.New(), org.ID)
	require.Error(t, err, "an INSERT that omits created_at must be refused, not defaulted")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code, "not_null_violation, at the statement that got it wrong")
	require.Equal(t, "created_at", pgErr.ColumnName)
}

// fakeSealer stands in for the keyring. These tests are about WHEN a row is
// written, not about what is in it, and a real key would make them depend on
// `platform/secrets` for nothing. The blob clears
// channel_credentials_seal_ck's 29-byte floor.
type fakeSealer struct{}

func (fakeSealer) Seal(context.Context, string, map[string]string) ([]byte, int, error) {
	return bytes.Repeat([]byte{0x01}, 32), 1, nil
}
