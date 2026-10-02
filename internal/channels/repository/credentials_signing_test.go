package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// Migration 00088 (ADR 0055 §1, git-bug 2765f74): a rotated signing secret keeps
// signing beside its successor for domain.SigningSecretOverlap, and the outgoing
// ciphertext is moved by the rotating UPDATE itself. These tests read the secrets
// BACK, through a sealer that round-trips, because "the previous secret is kept" is
// a claim about what unseals — a row with three non-null columns could hold the
// wrong bytes.

// TestARotatedSigningSecretKeepsSigningForTheOverlap is the rotation as a receiver
// lives it: both secrets for 24 hours, then only the new one.
func TestARotatedSigningSecretKeepsSigningForTheOverlap(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCredentialRepository(h.Pool, jsonSealer{}, jsonSealer{}, h.Clock)

	meta, err := repo.Create(h.Ctx, org.Scope, "webhook_signing_secret", map[string]string{"secret": "first"})
	require.NoError(t, err)

	before, err := repo.ResolveSigning(h.Ctx, org.Scope, meta.ID)
	require.NoError(t, err)
	require.Equal(t, domain.SigningSecret{Current: "first"}, before,
		"a secret that has never been rotated has no predecessor")

	rotatedAt := h.Now()
	_, err = repo.Rotate(h.Ctx, org.Scope, meta.ID, "webhook_signing_secret", map[string]string{"secret": "second"})
	require.NoError(t, err)

	during, err := repo.ResolveSigning(h.Ctx, org.Scope, meta.ID)
	require.NoError(t, err)
	require.Equal(t, "second", during.Current)
	require.Equal(t, "first", during.Previous,
		"the outgoing secret must still unseal: it is the one every receiver holds until told otherwise")
	require.Equal(t, rotatedAt.Add(domain.SigningSecretOverlap), during.PreviousUntil.UTC())
	require.Equal(t, []string{"second", "first"}, during.Secrets(h.Now()))

	// Past the overlap the predecessor is not even unsealed.
	h.Advance(domain.SigningSecretOverlap)
	after, err := repo.ResolveSigning(h.Ctx, org.Scope, meta.ID)
	require.NoError(t, err)
	require.Equal(t, domain.SigningSecret{Current: "second"}, after,
		"a predecessor whose overlap has ended must not be unsealed at all")
}

// TestASecondRotationRetiresTheOldestSecret: never more than two signatures, so a
// rotation inside the overlap keeps the secret it replaces and drops the one before.
func TestASecondRotationRetiresTheOldestSecret(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCredentialRepository(h.Pool, jsonSealer{}, jsonSealer{}, h.Clock)

	meta, err := repo.Create(h.Ctx, org.Scope, "webhook_signing_secret", map[string]string{"secret": "first"})
	require.NoError(t, err)
	_, err = repo.Rotate(h.Ctx, org.Scope, meta.ID, "webhook_signing_secret", map[string]string{"secret": "second"})
	require.NoError(t, err)

	h.Advance(time.Hour)
	_, err = repo.Rotate(h.Ctx, org.Scope, meta.ID, "webhook_signing_secret", map[string]string{"secret": "third"})
	require.NoError(t, err)

	got, err := repo.ResolveSigning(h.Ctx, org.Scope, meta.ID)
	require.NoError(t, err)
	require.Equal(t, "third", got.Current)
	require.Equal(t, "second", got.Previous, "the predecessor is the secret this rotation replaced")
	require.Equal(t, h.Now().Add(domain.SigningSecretOverlap), got.PreviousUntil.UTC(),
		"the overlap restarts at the rotation that began it")
}

// TestOnlyASigningSecretGetsAnOverlap: a rotated bearer token is revoked at once. An
// overlap on it would be a leaked token that keeps working for a day.
func TestOnlyASigningSecretGetsAnOverlap(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCredentialRepository(h.Pool, jsonSealer{}, jsonSealer{}, h.Clock)

	meta, err := repo.Create(h.Ctx, org.Scope, "bearer", map[string]string{"token": "old"})
	require.NoError(t, err)
	_, err = repo.Rotate(h.Ctx, org.Scope, meta.ID, "bearer", map[string]string{"token": "new"})
	require.NoError(t, err)

	var previous int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM channel_credentials WHERE id = $1 AND previous_sealed IS NOT NULL`,
		meta.ID).Scan(&previous))
	require.Zero(t, previous, "a rotated bearer token kept its predecessor")

	// And the constraint says so independently of the repository.
	_, err = h.Pool.Exec(h.Ctx,
		`UPDATE channel_credentials
		    SET previous_sealed = sealed, previous_key_version = 1, previous_until = $2
		  WHERE id = $1`, meta.ID, h.Now().Add(time.Hour))
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "channel_credentials_previous_ck admitted an overlap on a bearer token")
	require.Equal(t, "channel_credentials_previous_ck", pgErr.ConstraintName)

	// A signing slot must hold a signing secret: anything else would become an HMAC key.
	_, err = repo.ResolveSigning(h.Ctx, org.Scope, meta.ID)
	require.Error(t, err, "ResolveSigning unsealed a bearer token as a signing secret")
}

// TestAConnectionHoldsACredentialAndASigningSecretAtOnce is the second slot itself.
func TestAConnectionHoldsACredentialAndASigningSecretAtOnce(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	creds := repository.NewCredentialRepository(h.Pool, jsonSealer{}, jsonSealer{}, h.Clock)
	conns := repository.NewConnectionRepository(h.Pool, h.Clock)

	bearer, err := creds.Create(h.Ctx, org.Scope, "bearer", map[string]string{"token": "tok"})
	require.NoError(t, err)
	signing, err := creds.Create(h.Ctx, org.Scope, "webhook_signing_secret", map[string]string{"secret": "s"})
	require.NoError(t, err)

	conn, err := conns.Create(h.Ctx, org.Scope, domain.NewConnection{
		Type: domain.TypeWebhook, Name: "incident tool", Config: json.RawMessage(`{}`),
		CredentialID: &bearer.ID, SigningCredentialID: &signing.ID,
	})
	require.NoError(t, err)
	require.Equal(t, bearer.ID, *conn.CredentialID)
	require.Equal(t, "bearer", conn.CredentialKind)
	require.NotNil(t, conn.SigningCredentialID)
	require.Equal(t, signing.ID, *conn.SigningCredentialID)
	require.Nil(t, conn.SigningPreviousUntil, "a never-rotated signing secret has no overlap to report")

	_, err = creds.Rotate(h.Ctx, org.Scope, signing.ID, "webhook_signing_secret", map[string]string{"secret": "s2"})
	require.NoError(t, err)
	conn, err = conns.Get(h.Ctx, org.Scope, conn.ID)
	require.NoError(t, err)
	require.NotNil(t, conn.SigningPreviousUntil)
	require.Equal(t, h.Now().Add(domain.SigningSecretOverlap), conn.SigningPreviousUntil.UTC())
	require.Equal(t, "bearer", conn.CredentialKind, "rotating the signing slot touched the credential slot")

	// The two slots never name one row.
	_, err = h.Pool.Exec(h.Ctx,
		`UPDATE channel_connections SET signing_credential_id = credential_id WHERE id = $1`, conn.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "channel_connections_signing_distinct_ck", pgErr.ConstraintName)

	// And only a webhook signs.
	slack := h.SlackConnection(org, "T"+id.New().String()[:8])
	_, err = h.Pool.Exec(h.Ctx,
		`UPDATE channel_connections SET signing_credential_id = $2 WHERE id = $1`, slack.ID, signing.ID)
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "channel_connections_signing_ck", pgErr.ConstraintName)
}

// jsonSealer round-trips values so a test can read back WHICH secret is where. The
// 32-byte pad clears channel_credentials_seal_ck's 29-byte floor.
type jsonSealer struct{}

var sealPad = make([]byte, 32)

func (jsonSealer) Seal(_ context.Context, _ string, values map[string]string) ([]byte, int, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, 0, err
	}
	return append(append([]byte(nil), sealPad...), raw...), 1, nil
}

func (jsonSealer) Unseal(_ context.Context, _ string, sealed []byte, _ int) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal(sealed[len(sealPad):], &out); err != nil {
		return nil, err
	}
	return out, nil
}
