package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// CredentialKinds is the closed set of `channel_credentials.kind`
// (channel_credentials_kind_ck). It is exported because the API layer validates
// against it and a second copy would drift.
var CredentialKinds = []string{
	"slack_bot_token", "slack_app_token", "slack_signing_secret", "basic", "bearer", "none",
}

// ValidCredentialKind reports whether kind is in the closed set.
func ValidCredentialKind(kind string) bool {
	for _, k := range CredentialKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// credentialRow is the row model of `channel_credentials`. Unexported, per the
// three-model rule: no DTO and no domain type may embed it.
//
// ⛔ `sealed` is CIPHERTEXT and this struct is the only place it exists in Go.
// It is never returned from this package, never logged, and never carried into a
// DTO — the only thing that leaves is the already-decrypted value map, and only
// to the one caller that asked for it.
type credentialRow struct {
	id         uuid.UUID
	orgID      uuid.UUID
	kind       string
	sealed     []byte
	keyVersion int
	createdAt  time.Time
	rotatedAt  *time.Time
}

// CredentialMeta is everything about a stored credential that is SAFE TO SHOW.
//
// There is deliberately no field that could hold secret material. "The secret is
// never returned" is therefore a property of this type rather than a habit of
// the code that builds it.
type CredentialMeta struct {
	ID        uuid.UUID
	Kind      string
	CreatedAt time.Time
	RotatedAt *time.Time
}

// CredentialRepository is the SQL over `channel_credentials`, the GENERIC sealed
// secret store: `alert_sources.auth_credential_id` reuses it rather than growing
// a second one (SPEC §D.8).
type CredentialRepository struct {
	q     db.Querier
	seal  Sealer
	open  Unsealer
	clock clock.Clock
}

// NewCredentialRepository builds the repository. The sealer and unsealer are
// ports rather than a concrete keyring so that a test can exercise the SQL with a
// fake and never needs a key.
func NewCredentialRepository(q db.Querier, seal Sealer, open Unsealer, clk clock.Clock) *CredentialRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &CredentialRepository{q: q, seal: seal, open: open, clock: clk}
}

func (r *CredentialRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const insertCredentialSQL = `
INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, kind, created_at, rotated_at`

// Create seals and stores a new credential.
//
// The plaintext exists in this process for the duration of this call and is not
// referenced afterwards. The caller supplies it straight off a write-only DTO
// field and must not retain it either.
func (r *CredentialRepository) Create(
	ctx context.Context, s db.TenantScope, kind string, values map[string]string,
) (CredentialMeta, error) {
	if err := requireScope(s); err != nil {
		return CredentialMeta{}, err
	}
	if !ValidCredentialKind(kind) {
		return CredentialMeta{}, errs.Validation("credential_kind_invalid",
			"unsupported credential kind",
			errs.Violation{Field: "credential/kind", Code: "enum", Message: "unsupported credential kind"})
	}
	if r.seal == nil {
		return CredentialMeta{}, errs.New(errs.KindInternal, "credential_sealer_missing",
			"this deployment has no credential keyring configured")
	}

	sealed, version, err := r.seal.Seal(ctx, kind, values)
	if err != nil {
		return CredentialMeta{}, err
	}

	var out CredentialMeta
	row := r.db(ctx).QueryRow(ctx, insertCredentialSQL,
		id.New(), s.OrgID(), kind, sealed, version, r.clock.Now().UTC())
	if err := row.Scan(&out.ID, &out.Kind, &out.CreatedAt, &out.RotatedAt); err != nil {
		return CredentialMeta{}, mapErr(err, "credential_not_found", "store a credential")
	}
	return out, nil
}

const rotateCredentialSQL = `
UPDATE channel_credentials
   SET kind = $3, sealed = $4, key_version = $5, rotated_at = $6
 WHERE org_id = $1 AND id = $2
RETURNING id, kind, created_at, rotated_at`

// Rotate re-seals an existing credential in place and stamps `rotated_at`.
//
// Rotating rather than replacing keeps `alert_sources.auth_credential_id` and
// `channels.credential_id` pointing at the same row, so a rotation is one UPDATE
// and cannot leave a channel briefly credential-less.
func (r *CredentialRepository) Rotate(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID, kind string, values map[string]string,
) (CredentialMeta, error) {
	if err := requireScope(s); err != nil {
		return CredentialMeta{}, err
	}
	if err := requireID("credential_id", credentialID); err != nil {
		return CredentialMeta{}, err
	}
	if !ValidCredentialKind(kind) {
		return CredentialMeta{}, errs.Validation("credential_kind_invalid",
			"unsupported credential kind",
			errs.Violation{Field: "credential/kind", Code: "enum", Message: "unsupported credential kind"})
	}
	if r.seal == nil {
		return CredentialMeta{}, errs.New(errs.KindInternal, "credential_sealer_missing",
			"this deployment has no credential keyring configured")
	}

	sealed, version, err := r.seal.Seal(ctx, kind, values)
	if err != nil {
		return CredentialMeta{}, err
	}

	var out CredentialMeta
	row := r.db(ctx).QueryRow(ctx, rotateCredentialSQL,
		s.OrgID(), credentialID, kind, sealed, version, r.clock.Now().UTC())
	if err := row.Scan(&out.ID, &out.Kind, &out.CreatedAt, &out.RotatedAt); err != nil {
		if isNoRows(err) {
			return CredentialMeta{}, errs.NotFound("credential_not_found", "no such credential")
		}
		return CredentialMeta{}, mapErr(err, "credential_not_found", "rotate a credential")
	}
	return out, nil
}

const getCredentialMetaSQL = `
SELECT id, kind, created_at, rotated_at
  FROM channel_credentials
 WHERE org_id = $1 AND id = $2`

// Meta reads the SAFE half of a credential: what kind it is and when it was last
// rotated. This is what the channel DTO carries.
func (r *CredentialRepository) Meta(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID,
) (CredentialMeta, error) {
	if err := requireScope(s); err != nil {
		return CredentialMeta{}, err
	}
	var out CredentialMeta
	row := r.db(ctx).QueryRow(ctx, getCredentialMetaSQL, s.OrgID(), credentialID)
	if err := row.Scan(&out.ID, &out.Kind, &out.CreatedAt, &out.RotatedAt); err != nil {
		if isNoRows(err) {
			return CredentialMeta{}, errs.NotFound("credential_not_found", "no such credential")
		}
		return CredentialMeta{}, mapErr(err, "credential_not_found", "read a credential")
	}
	return out, nil
}

const getSealedSQL = `
SELECT id, org_id, kind, sealed, key_version, created_at, rotated_at
  FROM channel_credentials
 WHERE org_id = $1 AND id = $2`

// Resolve unseals one credential.
//
// ⛔ The returned map is plaintext secret material. It exists only for the
// duration of one provider construction. Nothing in this package logs it, and
// nothing may persist it in this shape.
func (r *CredentialRepository) Resolve(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID,
) (string, map[string]string, error) {
	if err := requireScope(s); err != nil {
		return "", nil, err
	}
	if r.open == nil {
		return "", nil, errs.New(errs.KindInternal, "credential_unsealer_missing",
			"this deployment has no credential keyring configured")
	}

	var row credentialRow
	err := r.db(ctx).QueryRow(ctx, getSealedSQL, s.OrgID(), credentialID).Scan(
		&row.id, &row.orgID, &row.kind, &row.sealed, &row.keyVersion, &row.createdAt, &row.rotatedAt)
	if err != nil {
		if isNoRows(err) {
			return "", nil, errs.NotFound("credential_not_found", "no such credential")
		}
		return "", nil, mapErr(err, "credential_not_found", "read a credential")
	}

	values, err := r.open.Unseal(ctx, row.kind, row.sealed, row.keyVersion)
	if err != nil {
		return "", nil, err
	}
	return row.kind, values, nil
}

// Delete removes a credential row.
//
// Both referencing columns are ON DELETE SET NULL, so deleting a credential
// leaves the channel or source pointing at nothing rather than cascading a
// destination out of existence. That is the correct blast radius: losing a token
// must not lose the record of where messages went.
func (r *CredentialRepository) Delete(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) error {
	if err := requireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx,
		`DELETE FROM channel_credentials WHERE org_id = $1 AND id = $2`, s.OrgID(), credentialID)
	if err != nil {
		return mapErr(err, "credential_not_found", "delete a credential")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("credential_not_found", "no such credential")
	}
	return nil
}

// The two methods below are the PLAIN-TYPED face of Create and Rotate.
//
// They exist because `sources/api` and `channels/api` both need to seal a
// credential, and neither may name `CredentialMeta`: `api` must not import
// `repository` (CONTEXT.md §5.1), and `sources` must not import `channels`
// internals at all (depguard). Expressing the port in `uuid.UUID` and
// `map[string]string` lets ONE concrete satisfy both consumer-declared ports
// with no adapter anywhere — which is the difference between a composition root
// that wires and a composition root that translates.

// CreateCredential seals a new secret and returns its id.
func (r *CredentialRepository) CreateCredential(
	ctx context.Context, s db.TenantScope, kind string, values map[string]string,
) (uuid.UUID, error) {
	meta, err := r.Create(ctx, s, kind, values)
	if err != nil {
		return uuid.Nil, err
	}
	return meta.ID, nil
}

// RotateCredential re-seals an existing secret in place.
func (r *CredentialRepository) RotateCredential(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID, kind string, values map[string]string,
) error {
	_, err := r.Rotate(ctx, s, credentialID, kind, values)
	return err
}
