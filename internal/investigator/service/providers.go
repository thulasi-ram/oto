package service

// MODEL ENDPOINTS IN SETTINGS (git-bug 8f1f071 comment #1: "settings can store a
// provider config with a sealed key"). Create seals the key and stores the row in one
// transaction; Get and List return the row and never the key; Open is the one path on
// which a key is unsealed, and it is unsealed straight into an adapter.
//
// ⚠️ THERE IS NO HTTP SURFACE YET. The settings API and its page are a follow-up (they
// belong with the Investigator settings in git-bug 180a525's area); this is the service
// they will call, and the shape a DTO must keep — a `has_key` boolean, never the key.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Service is the investigator module's service. Today it holds model endpoints; the
// Investigation loop (git-bug 180a525) joins it.
type Service struct {
	providers ProviderStore
	creds     CredentialWriter
	keys      KeyResolver
	dial      ModelDialer
	tx        TxRunner
	clock     clock.Clock
}

// Deps are the Service's collaborators. Every one but Clock is required: an endpoint
// stored with no unit of work can leave a sealed key nothing points at, and one with no
// way to unseal its key is an endpoint every Investigation fails against.
type Deps struct {
	Providers   ProviderStore
	Credentials CredentialWriter
	Keys        KeyResolver
	Dialer      ModelDialer
	Tx          TxRunner
	Clock       clock.Clock
}

// New builds the Service.
func New(d Deps) (*Service, error) {
	switch {
	case d.Providers == nil:
		return nil, errors.New("investigator: a model endpoint store is required")
	case d.Credentials == nil:
		return nil, errors.New("investigator: a credential writer is required; a model key is only ever stored sealed")
	case d.Keys == nil:
		return nil, errors.New("investigator: a key resolver is required")
	case d.Dialer == nil:
		return nil, errors.New("investigator: a model dialer is required")
	case d.Tx == nil:
		return nil, errors.New("investigator: a unit of work is required; an endpoint and its key commit together")
	}
	if d.Clock == nil {
		d.Clock = clock.New()
	}
	return &Service{
		providers: d.Providers, creds: d.Credentials, keys: d.Keys,
		dial: d.Dialer, tx: d.Tx, clock: d.Clock,
	}, nil
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// CreateProvider validates a draft, seals its key and stores the endpoint.
//
// ⛔ THE KEY IS NEVER RETURNED, LOGGED OR ECHOED. The returned config carries only the
// sealed row's id, and every error below comes from validation or storage — none of
// which ever renders the draft.
func (s *Service) CreateProvider(ctx context.Context, scope db.TenantScope, draft domain.ProviderDraft) (domain.ProviderConfig, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.ProviderConfig{}, err
	}
	clean, err := draft.Normalize()
	if err != nil {
		return domain.ProviderConfig{}, err
	}
	at := s.now()

	var out domain.ProviderConfig
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		credentialID := uuid.Nil
		if clean.APIKey != "" {
			id, err := s.creds.CreateCredential(ctx, scope, domain.CredentialKind,
				map[string]string{domain.CredentialValueKey: clean.APIKey})
			if err != nil {
				return err
			}
			credentialID = id
		}
		stored, err := s.providers.Insert(ctx, scope, clean, credentialID, at)
		if err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil {
		return domain.ProviderConfig{}, err
	}
	return out, nil
}

// GetProvider reads one endpoint, without its key.
func (s *Service) GetProvider(ctx context.Context, scope db.TenantScope, providerID uuid.UUID) (domain.ProviderConfig, error) {
	return s.providers.Get(ctx, scope, providerID)
}

// ListProviders reads an org's endpoints by name, without their keys.
func (s *Service) ListProviders(ctx context.Context, scope db.TenantScope) ([]domain.ProviderConfig, error) {
	return s.providers.List(ctx, scope)
}

// OpenProvider builds the ModelProvider an Investigation talks to for one stored
// endpoint. The key is unsealed here and handed straight to the adapter; nothing
// keeps it. The returned provider's Identity is the config's, which is what the
// Investigator version pins.
func (s *Service) OpenProvider(ctx context.Context, scope db.TenantScope, providerID uuid.UUID) (domain.ModelProvider, error) {
	cfg, err := s.providers.Get(ctx, scope, providerID)
	if err != nil {
		return nil, err
	}
	key := ""
	if cfg.HasKey() {
		if key, err = s.keys.ResolveKey(ctx, scope, cfg.CredentialID); err != nil {
			return nil, err
		}
	}
	p, err := s.dial.Dial(cfg, key)
	if err != nil {
		return nil, err
	}
	if p.Identity() != cfg.Identity() {
		// A dialer that normalised differently would pin Findings to an endpoint the
		// settings page does not show. Refuse rather than record the wrong identity.
		return nil, errs.Newf(errs.KindInternal, "model_identity_mismatch",
			"the model adapter reports %s for the endpoint stored as %s", p.Identity(), cfg.Identity())
	}
	return p, nil
}
