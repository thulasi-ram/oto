package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Sealer seals plaintext credential values for storage.
//
// ⭐ It is declared HERE, BY THE CONSUMER, and there is no implementation of it in
// this module. `internal/platform/secrets.Keyring` is the one keyring in the
// process (SPEC §D.8) and `internal/app` injects it; a self-contained copy used to
// live in `seal.go` while `platform/secrets` was empty, and deleting that copy cost
// nothing precisely because everything here depends on this interface and not on a
// type.
type Sealer interface {
	// Seal returns the ciphertext and the keyring generation that produced it.
	Seal(ctx context.Context, kind string, values map[string]string) ([]byte, int, error)
}

// Unsealer recovers plaintext credential values.
//
// The method set is byte-identical to `notification/service.CredentialUnsealer`
// and `sources/repository.Unsealer` so that one concrete satisfies all three
// without an adapter.
type Unsealer interface {
	Unseal(ctx context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error)
}

// CredentialKinds is the closed set of `channel_credentials.kind`
// (channel_credentials_kind_ck). It is exported because the API layer validates
// against it and a second copy would drift.
var CredentialKinds = []string{
	"slack_bot_token", "slack_app_token", "slack_signing_secret", "basic", "bearer",
	"webhook_signing_secret",
	// A webhook Connection's payload-mapping secrets, name → value, in its own slot
	// (migration 00090). channels/api seals it from `mapping_secrets`, never from a
	// `credential` input, whose `kind` enum does not list it.
	domain.MappingSecretsKind,
	"none",
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

// ⭐ `created_at` IS PASSED, never left to a column default, and 00033 removed
// that default so this cannot regress quietly.
//
// The application owns time here (`platform/clock`), and the reason is
// `channel_credentials_rot_ck`: `rotated_at IS NULL OR rotated_at >= created_at`.
// If `created_at` came from `DEFAULT now()` — the DATABASE's clock — while
// Rotate below stamps the GO process's, an app server a few milliseconds behind
// its database would fail the first rotation of a fresh credential with a 23514.
// It is the same defect 00032 fixed on `channels`; only production's Create
// already naming the column kept it off the live paths.
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
	if err := db.RequireScope(s); err != nil {
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

// ⭐ `rotated_at` IS ADVANCED MONOTONICALLY, and on this table it is the ordering
// column as well as the observation: `channel_credentials_rot_ck` is what makes
// it one. Every timestamp here comes from the application (00033), but "the
// application" is N pods with N clocks, and the pod rotating a secret is rarely
// the pod that sealed it. A pod a few milliseconds behind the creator would
// otherwise write a `rotated_at` below `created_at` and fail the CHECK with a
// 23514 — a 500 on an ordinary credential rotation.
//
// GREATEST is given THREE arguments, not two, because two invariants have to
// hold and each covers a different one. `created_at` is the floor the CHECK
// names, which the FIRST rotation can fall below; `rotated_at` is the floor a
// LATER rotation from a lagging pod can fall below, which would report the
// secret as older than it is. GREATEST ignores NULLs, so a never-rotated row —
// where `rotated_at` is NULL — takes the max of the other two, exactly as it
// should.
//
// ⭐⭐ A SIGNING SECRET KEEPS ITS PREDECESSOR FOR THE OVERLAP, AND THE SQL MOVES IT
// (migration 00088, ADR 0055 §1). When the row was a `webhook_signing_secret` and
// stays one, the three `previous_*` columns take the row's CURRENT ciphertext and
// key version and `$7`, the end of the overlap. Every SET expression reads the
// OLD row, so `previous_sealed = sealed` is the outgoing secret, still sealed: it
// is never unsealed to be carried forward, and no plaintext of it exists anywhere
// during a rotation. Any other kind — or a signing secret re-sealed as something
// else — clears all three, which is what `channel_credentials_previous_ck`
// demands: an overlap on a bearer token is a revoked token that still works.
const rotateCredentialSQL = `
UPDATE channel_credentials
   SET previous_sealed      = CASE WHEN kind = 'webhook_signing_secret' AND $3 = 'webhook_signing_secret'
                                   THEN sealed END,
       previous_key_version = CASE WHEN kind = 'webhook_signing_secret' AND $3 = 'webhook_signing_secret'
                                   THEN key_version END,
       previous_until       = CASE WHEN kind = 'webhook_signing_secret' AND $3 = 'webhook_signing_secret'
                                   THEN $7::timestamptz END,
       kind = $3, sealed = $4, key_version = $5,
       rotated_at = GREATEST(created_at, rotated_at, $6)
 WHERE org_id = $1 AND id = $2
RETURNING id, kind, created_at, rotated_at`

// signingKind is the one credential kind that rotates with an overlap.
const signingKind = "webhook_signing_secret"

// Rotate re-seals an existing credential in place and stamps `rotated_at`.
//
// Rotating rather than replacing keeps `alert_sources.auth_credential_id` and
// `channels.credential_id` pointing at the same row, so a rotation is one UPDATE
// and cannot leave a channel briefly credential-less.
//
// A `webhook_signing_secret` rotated into another one keeps signing with its
// predecessor until `domain.SigningSecretOverlap` from now — see
// rotateCredentialSQL. A second rotation inside that window retires the oldest
// secret at once: there are never more than two.
func (r *CredentialRepository) Rotate(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID, kind string, values map[string]string,
) (CredentialMeta, error) {
	if err := db.RequireScope(s); err != nil {
		return CredentialMeta{}, err
	}
	if err := db.RequireID("credential_id", credentialID); err != nil {
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

	now := r.clock.Now().UTC()
	var out CredentialMeta
	row := r.db(ctx).QueryRow(ctx, rotateCredentialSQL,
		s.OrgID(), credentialID, kind, sealed, version, now, now.Add(domain.SigningSecretOverlap))
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
	if err := db.RequireScope(s); err != nil {
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
	if err := db.RequireScope(s); err != nil {
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

const getSigningSQL = `
SELECT kind, sealed, key_version, previous_sealed, previous_key_version, previous_until
  FROM channel_credentials
 WHERE org_id = $1 AND id = $2`

// ResolveSigning unseals a connection's outbound signing secret and, while its
// overlap lasts, the one the last rotation replaced.
//
// ⛔ THE KIND IS CHECKED HERE, NOT TRUSTED. `signing_credential_id` is a column
// any row id fits in; a bearer token behind it would otherwise become an HMAC key
// — and an HMAC over a body is not a secret, so every receiver would be signing
// with a token oto also hands out as `Authorization`. A non-signing row is
// refused as an internal error, because only a bug in channels/api can put one
// there.
//
// A predecessor whose overlap has already ended is not unsealed at all: there is
// no reason for that plaintext to exist, even briefly.
func (r *CredentialRepository) ResolveSigning(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID,
) (domain.SigningSecret, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.SigningSecret{}, err
	}
	if r.open == nil {
		return domain.SigningSecret{}, errs.New(errs.KindInternal, "credential_unsealer_missing",
			"this deployment has no credential keyring configured")
	}

	var (
		kind            string
		sealed          []byte
		keyVersion      int
		previousSealed  []byte
		previousVersion *int
		previousUntil   *time.Time
	)
	err := r.db(ctx).QueryRow(ctx, getSigningSQL, s.OrgID(), credentialID).Scan(
		&kind, &sealed, &keyVersion, &previousSealed, &previousVersion, &previousUntil)
	if err != nil {
		if isNoRows(err) {
			return domain.SigningSecret{}, errs.NotFound("credential_not_found", "no such credential")
		}
		return domain.SigningSecret{}, mapErr(err, "credential_not_found", "read a signing secret")
	}
	if kind != signingKind {
		return domain.SigningSecret{}, errs.Newf(errs.KindInternal, "signing_credential_kind",
			"the connection's signing slot holds a %q credential, not a %s", kind, signingKind)
	}

	values, err := r.open.Unseal(ctx, kind, sealed, keyVersion)
	if err != nil {
		return domain.SigningSecret{}, err
	}
	out := domain.SigningSecret{Current: domain.SigningValue(values)}

	if previousSealed != nil && previousVersion != nil && previousUntil != nil &&
		r.clock.Now().Before(*previousUntil) {
		prev, err := r.open.Unseal(ctx, kind, previousSealed, *previousVersion)
		if err != nil {
			return domain.SigningSecret{}, err
		}
		out.Previous, out.PreviousUntil = domain.SigningValue(prev), previousUntil.UTC()
	}
	return out, nil
}

// Delete removes a credential row.
//
// Both referencing columns are ON DELETE SET NULL, so deleting a credential
// leaves the channel or source pointing at nothing rather than cascading a
// destination out of existence. That is the correct blast radius: losing a token
// must not lose the record of where messages went.
func (r *CredentialRepository) Delete(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) error {
	if err := db.RequireScope(s); err != nil {
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
// They exist because `sources/service` and `channels/api` both need to seal a
// credential, and neither may name `CredentialMeta`: `api` must not import
// `repository` (CONTEXT.md §5.1), a service declares its own ports (§5.3), and
// `sources` must not import `channels` internals at all (depguard). Expressing the port in `uuid.UUID` and
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
