package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ProviderRepository is every statement against `model_providers` (migration 00091).
//
// ⛔ IT NEVER SEES THE KEY. The row holds `credential_id`, and the only statement here
// that touches `channel_credentials` is KeyStore's narrowed read below; sealing goes
// through `channels/repository.CredentialRepository`, behind the CredentialWriter port
// `investigator/service` declares, inside the same transaction as the INSERT.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE, so another
// org's endpoint is the same answer as one that never existed: 404.
type ProviderRepository struct {
	q db.Querier
}

// NewProviderRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewProviderRepository(q db.Querier) *ProviderRepository { return &ProviderRepository{q: q} }

func (r *ProviderRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// TxRunner is this module's unit of work: an endpoint's row and its sealed key commit
// together or not at all, so a failure between them cannot leave a key nothing points
// at, or a row whose key was never stored.
type TxRunner = db.TxRunner

// NewTxRunner builds the unit of work over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }

// mapErr is the §L.9 translation under this module's codes. A name already taken in
// the org is a 23505 on `model_providers_org_name_uniq`, which MapError turns into a
// 409 whose code IS that index's name (CONTEXT.md §6: constraint names are a runtime
// contract) — so no code is mapped here for it.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "model_provider_not_found",
		NotFoundMessage:    "no such model endpoint",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

const providerColumns = `id, org_id, name, base_url, model, credential_id, created_at, updated_at`

// ⭐ BOTH TIMESTAMPS ARE PASSED, never defaulted — 00091 gave neither column a default
// — and they are the SAME instant on insert, so `model_providers_time_ck` holds by
// construction rather than by two clocks agreeing.
const insertProviderSQL = `
INSERT INTO model_providers (id, org_id, name, base_url, model, credential_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING ` + providerColumns

// Insert stores a normalised draft's public half. `draft.APIKey` is NOT read here: the
// caller has already sealed it and passes the row id (uuid.Nil for no key).
func (r *ProviderRepository) Insert(
	ctx context.Context, s db.TenantScope, draft domain.ProviderDraft, credentialID uuid.UUID, at time.Time,
) (domain.ProviderConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.ProviderConfig{}, err
	}
	var cred *uuid.UUID
	if credentialID != uuid.Nil {
		cred = &credentialID
	}
	row := r.db(ctx).QueryRow(ctx, insertProviderSQL,
		id.New(), s.OrgID(), draft.Name, draft.BaseURL, draft.Model, cred, at.UTC())
	out, err := scanProvider(row)
	if err != nil {
		return domain.ProviderConfig{}, mapErr(err, "store a model endpoint")
	}
	return out, nil
}

// Get reads one endpoint.
func (r *ProviderRepository) Get(ctx context.Context, s db.TenantScope, providerID uuid.UUID) (domain.ProviderConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.ProviderConfig{}, err
	}
	if err := db.RequireID("model_provider_id", providerID); err != nil {
		return domain.ProviderConfig{}, err
	}
	row := r.db(ctx).QueryRow(ctx,
		`SELECT `+providerColumns+` FROM model_providers WHERE org_id = $1 AND id = $2`, s.OrgID(), providerID)
	out, err := scanProvider(row)
	if err != nil {
		return domain.ProviderConfig{}, mapErr(err, "read a model endpoint")
	}
	return out, nil
}

// MaxListedProviders bounds List. An org configures a handful of endpoints; a list
// past this is a runaway script, and an unbounded read is not the way to find out.
const MaxListedProviders = 200

// List reads an org's endpoints by name, served by `model_providers_org_name_uniq`.
func (r *ProviderRepository) List(ctx context.Context, s db.TenantScope) ([]domain.ProviderConfig, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx,
		`SELECT `+providerColumns+` FROM model_providers WHERE org_id = $1 ORDER BY name LIMIT $2`,
		s.OrgID(), MaxListedProviders)
	if err != nil {
		return nil, mapErr(err, "list model endpoints")
	}
	defer rows.Close()
	out := []domain.ProviderConfig{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, mapErr(err, "list model endpoints")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "list model endpoints")
	}
	return out, nil
}

func scanProvider(row pgx.Row) (domain.ProviderConfig, error) {
	var (
		out  domain.ProviderConfig
		cred *uuid.UUID
	)
	if err := row.Scan(&out.ID, &out.OrgID, &out.Name, &out.BaseURL, &out.Model, &cred,
		&out.CreatedAt, &out.UpdatedAt); err != nil {
		return domain.ProviderConfig{}, err
	}
	if cred != nil {
		out.CredentialID = *cred
	}
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	return out, nil
}

// Unsealer turns a sealed blob into plaintext values. It is a PORT DECLARED BY THE
// CONSUMER, method-set identical to `sources/repository.Unsealer`, so the one keyring
// (`platform/secrets.Keyring`) satisfies it with no adapter.
type Unsealer interface {
	Unseal(ctx context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error)
}

// KeyStore unseals a model endpoint's API key.
//
// ⚠️ IT READS `channel_credentials`, A TABLE `channels` WRITES — the one sanctioned
// overlap SPEC §D.8 makes for the generic secret store, on the terms
// `sources/repository.CredentialStore` reads it: narrowed to exactly one row by id,
// in one org, and refused unless the row is a `model_api_key`.
type KeyStore struct {
	q    db.Querier
	open Unsealer
}

// NewKeyStore builds the store. A nil Unsealer is legal and makes every Resolve fail
// loudly: a deployment with no keyring must not quietly call a model with no key.
func NewKeyStore(q db.Querier, open Unsealer) *KeyStore { return &KeyStore{q: q, open: open} }

func (k *KeyStore) db(ctx context.Context) db.Querier { return db.FromContext(ctx, k.q) }

// ResolveKey unseals one key.
//
// ⛔ THE RETURNED STRING IS A SECRET. It exists for the construction of one adapter and
// is never logged, persisted, put in a Step or rendered into an errs.Message; every
// failure below names what failed and nothing about the material.
//
// ⛔ THE KIND IS CHECKED, NOT TRUSTED. `credential_id` is a column any row id fits in,
// and a Slack bot token behind it would otherwise be presented to a model endpoint as
// a bearer key — which is a token handed to a third party. The seal's AAD would refuse
// it too; this says why, before trying.
func (k *KeyStore) ResolveKey(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (string, error) {
	return k.resolve(ctx, s, credentialID, domain.CredentialKind, domain.CredentialValueKey,
		"model", "the model endpoint's key")
}

// ResolveToolServerToken unseals one ToolServer's access token (git-bug 2e9a086), on
// ResolveKey's terms: one row, one org, and refused unless it is a `tool_server_token`
// — a model key behind a ToolServer's slot would otherwise be presented to a
// cluster-facing server. ⛔ The returned string is a secret.
func (k *KeyStore) ResolveToolServerToken(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (string, error) {
	return k.resolve(ctx, s, credentialID, domain.ToolServerCredentialKind, domain.ToolServerCredentialValueKey,
		"tool_server", "the ToolServer's access token")
}

// resolve is both: `code` prefixes the error codes (`model_credential_kind`,
// `tool_server_credential_kind`), and `what` names the secret in a message without
// ever rendering it.
func (k *KeyStore) resolve(
	ctx context.Context, s db.TenantScope, credentialID uuid.UUID, wantKind, valueKey, code, what string,
) (string, error) {
	if err := db.RequireScope(s); err != nil {
		return "", err
	}
	if err := db.RequireID("credential_id", credentialID); err != nil {
		return "", err
	}
	if k.open == nil {
		return "", errs.New(errs.KindInternal, "credential_unsealer_missing",
			"this deployment has no credential keyring configured")
	}
	var (
		kind       string
		sealed     []byte
		keyVersion int
	)
	err := k.db(ctx).QueryRow(ctx,
		`SELECT kind, sealed, key_version FROM channel_credentials WHERE org_id = $1 AND id = $2`,
		s.OrgID(), credentialID).Scan(&kind, &sealed, &keyVersion)
	if err != nil {
		return "", db.MapError(err, db.ErrorPolicy{
			NotFound: "credential_not_found", NotFoundMessage: "no such credential",
			QueryFailed: "investigator_query_failed", QueryFailedMessage: "could not read " + what,
		})
	}
	if kind != wantKind {
		return "", errs.Newf(errs.KindInternal, code+"_credential_kind",
			"%s slot holds a %q credential, not a %s", what, kind, wantKind)
	}
	values, err := k.open.Unseal(ctx, kind, sealed, keyVersion)
	if err != nil {
		return "", err
	}
	secret := values[valueKey]
	if secret == "" {
		return "", errs.Newf(errs.KindInternal, code+"_credential_empty",
			"%s holds no %s value", what, valueKey)
	}
	return secret, nil
}
