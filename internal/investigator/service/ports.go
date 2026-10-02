package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports this module declares for itself (ADR 0002, CONTEXT.md §5.4).
// `internal/app` satisfies each one; nothing here names a concrete.

// ProviderStore is where model endpoints are kept, satisfied by
// `investigator/repository.ProviderRepository`.
type ProviderStore interface {
	Insert(ctx context.Context, s db.TenantScope, draft domain.ProviderDraft, credentialID uuid.UUID, at time.Time) (domain.ProviderConfig, error)
	Get(ctx context.Context, s db.TenantScope, providerID uuid.UUID) (domain.ProviderConfig, error)
	List(ctx context.Context, s db.TenantScope) ([]domain.ProviderConfig, error)
}

// CredentialWriter seals a secret into the one sealed-secret store and returns its id.
//
// ⭐ IT IS `channels/repository.CredentialRepository`'S PLAIN-TYPED FACE, the same one
// `sources/service` and `channels/api` declare: `uuid.UUID` and `map[string]string`,
// so one concrete satisfies every consumer with no adapter. It has no read method on
// purpose — the only reader of a model key is KeyResolver, at the moment of dialling.
type CredentialWriter interface {
	CreateCredential(ctx context.Context, s db.TenantScope, kind string, values map[string]string) (uuid.UUID, error)
}

// KeyResolver unseals a model endpoint's key, satisfied by
// `investigator/repository.KeyStore`. The string it returns is a secret.
type KeyResolver interface {
	ResolveKey(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (string, error)
}

// ModelDialer builds a domain.ModelProvider for one stored endpoint and its unsealed
// key, satisfied by `investigator/models/openaicompat.Dialer`. It is a port so this
// service names no adapter: the one that speaks a wire protocol is chosen in
// `internal/app`, with the guarded HTTP client it must dial through.
type ModelDialer interface {
	Dial(cfg domain.ProviderConfig, apiKey string) (domain.ModelProvider, error)
}

// TxRunner is the unit of work: an endpoint's row and its sealed key commit together.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}
