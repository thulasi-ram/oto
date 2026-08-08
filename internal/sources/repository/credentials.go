package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// Unsealer turns a sealed blob into plaintext credential values.
//
// It is a PORT DECLARED BY THE CONSUMER. This package holds ciphertext and never
// a key: a repository that could decrypt on its own would be a repository whose
// stack traces can contain a bearer token. The method set is byte-identical to
// `notification/service.CredentialUnsealer`, so one concrete satisfies every
// module that stores a secret and no adapter is needed anywhere.
//
// ⛔ `channel_credentials` is the GENERIC sealed-secret store (SPEC §D.8):
// `alert_sources.auth_credential_id` reuses it rather than growing a second one.
// This file therefore READS a table the channels module writes, which is the one
// sanctioned overlap and is why the read is narrowed to exactly one row by id.
type Unsealer interface {
	Unseal(ctx context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error)
}

// Credential value keys. They are the map keys the sealer stores and the
// unsealer returns, and they are shared with the channels providers, so they are
// spelled once here rather than typed out at each call site.
const (
	// CredValueToken carries a bearer token.
	CredValueToken = "token"
	// CredValueUsername carries the basic-auth user.
	CredValueUsername = "username"
	// CredValuePassword carries the basic-auth password.
	CredValuePassword = "password"
)

// CredentialStore resolves an AlertSource's outbound credential.
//
// It implements `sources/service.CredentialStore`.
type CredentialStore struct {
	q    db.Querier
	open Unsealer
}

// NewCredentialStore builds the store. A nil Unsealer is legal and makes every
// Resolve fail loudly: a deployment with no keyring must not silently fall back
// to talking to an authenticated Alertmanager without credentials.
func NewCredentialStore(q db.Querier, open Unsealer) *CredentialStore {
	return &CredentialStore{q: q, open: open}
}

func (r *CredentialStore) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const resolveCredentialSQL = `
SELECT kind, sealed, key_version
  FROM channel_credentials
 WHERE org_id = $1 AND id = $2`

// Resolve unseals one credential into the shape the outbound clients take.
//
// ⛔ THE RETURNED VALUE IS A SECRET. It exists in memory only for the duration of
// one client construction, and `domain.Credential` is deliberately not a type
// this package persists, logs or renders into an errs.Message. Every failure
// below reports WHAT failed and nothing about the material.
func (r *CredentialStore) Resolve(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID,
) (domain.Credential, error) {
	if err := requireScope(s); err != nil {
		return domain.Credential{}, err
	}
	if credentialID == uuid.Nil {
		// A nil id means the source has no credential, which is AuthNone rather
		// than a failure. The service already guards this; belt and braces.
		return domain.Credential{Kind: domain.AuthNone}, nil
	}
	if r.open == nil {
		return domain.Credential{}, errs.New(errs.KindInternal, "credential_unsealer_missing",
			"this deployment has no credential keyring configured")
	}

	var (
		kind       string
		sealed     []byte
		keyVersion int
	)
	err := r.db(ctx).QueryRow(ctx, resolveCredentialSQL, s.OrgID(), credentialID).
		Scan(&kind, &sealed, &keyVersion)
	if err != nil {
		if isNoRows(err) {
			return domain.Credential{}, errs.NotFound("credential_not_found", "no such credential")
		}
		return domain.Credential{}, mapErr(err, "credential_not_found", "read a credential")
	}

	values, err := r.open.Unseal(ctx, kind, sealed, keyVersion)
	if err != nil {
		return domain.Credential{}, err
	}

	switch domain.AuthKind(kind) {
	case domain.AuthNone:
		return domain.Credential{Kind: domain.AuthNone}, nil
	case domain.AuthBearer:
		return domain.Credential{Kind: domain.AuthBearer, Token: values[CredValueToken]}, nil
	case domain.AuthBasic:
		return domain.Credential{
			Kind:     domain.AuthBasic,
			Username: values[CredValueUsername],
			Password: values[CredValuePassword],
		}, nil
	default:
		// A Slack token on an AlertSource is a configuration mistake, not a
		// credential this module can use. Say so rather than sending a workspace
		// token to an Alertmanager.
		return domain.Credential{}, errs.Newf(errs.KindValidation, "credential_kind_unsupported",
			"credential kind %q cannot authenticate an alert source", kind)
	}
}

// CredentialMeta is the SAFE half of a stored credential — what kind it is and
// when it was last rotated. There is deliberately no field here that could hold
// secret material.
type CredentialMeta struct {
	ID        uuid.UUID
	Kind      string
	CreatedAt time.Time
	RotatedAt *time.Time
}

const credentialMetaSQL = `
SELECT id, kind, created_at, rotated_at
  FROM channel_credentials WHERE org_id = $1 AND id = $2`

// Meta reads what a source's credential IS, without reading what it says.
func (r *CredentialStore) Meta(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID,
) (CredentialMeta, error) {
	if err := requireScope(s); err != nil {
		return CredentialMeta{}, err
	}
	var out CredentialMeta
	err := r.db(ctx).QueryRow(ctx, credentialMetaSQL, s.OrgID(), credentialID).
		Scan(&out.ID, &out.Kind, &out.CreatedAt, &out.RotatedAt)
	if err != nil {
		if isNoRows(err) {
			return CredentialMeta{}, errs.NotFound("credential_not_found", "no such credential")
		}
		return CredentialMeta{}, mapErr(err, "credential_not_found", "read a credential")
	}
	return out, nil
}
