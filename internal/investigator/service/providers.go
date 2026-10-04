package service

// MODEL ENDPOINTS IN SETTINGS (git-bug 8f1f071 comment #1: "settings can store a
// provider config with a sealed key"). Create seals the key and stores the row in one
// transaction; Get and List return the row and never the key; Open is the one path on
// which a key is unsealed, and it is unsealed straight into an adapter.
//
// ⭐ THE HTTP SURFACE IS `investigator/api` (git-bug 180a525): a create and a list, and
// the DTO keeps the shape this service promises — a `has_key` boolean, never the key.

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

// Service is the investigator module's service: model endpoints, Investigators, and
// the Investigations that run them (git-bug 180a525).
type Service struct {
	providers ProviderStore
	creds     CredentialWriter
	keys      KeyResolver
	dial      ModelDialer
	tx        TxRunner
	clock     clock.Clock

	investigators  InvestigatorStore
	investigations InvestigationStore
	cases          CaseReader
	timeline       TimelineReader
	rules          RuleReader
	findings       FindingPublisher
	orgControls    OrgControls
	queue          JobQueue
	limits         Limits
	tools          []Tool

	toolServers ToolServerStore
	tokens      TokenResolver
	toolDialer  ToolServerDialer
	redaction   RedactionRules
}

// Deps are the Service's collaborators. Every one but Clock and Limits is required:
// an endpoint stored with no unit of work can leave a sealed key nothing points at,
// one with no way to unseal its key is an endpoint every Investigation fails against,
// and a run with no way to record its Steps is a run nobody can read back.
type Deps struct {
	Providers   ProviderStore
	Credentials CredentialWriter
	Keys        KeyResolver
	Dialer      ModelDialer
	Tx          TxRunner
	Clock       clock.Clock

	Investigators  InvestigatorStore
	Investigations InvestigationStore
	Cases          CaseReader
	Timeline       TimelineReader
	Rules          RuleReader
	Findings       FindingPublisher
	OrgControls    OrgControls
	Queue          JobQueue
	// Limits are the built-in Tools' per-call controls. Zero fields take
	// DefaultLimits. A ToolServer's Tools run under the ToolServer's own.
	Limits Limits

	// ToolServers, Tokens and ToolDialer reach an operator's MCP servers (git-bug
	// 2e9a086); Redaction is the org's ingest redaction rules every Tool result passes
	// through before it is recorded or read.
	ToolServers ToolServerStore
	Tokens      TokenResolver
	ToolDialer  ToolServerDialer
	Redaction   RedactionRules
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
	case d.Investigators == nil:
		return nil, errors.New("investigator: an Investigator store is required")
	case d.Investigations == nil:
		return nil, errors.New("investigator: an Investigation store is required; a run nobody can read back is not one")
	case d.Cases == nil || d.Timeline == nil || d.Rules == nil:
		return nil, errors.New("investigator: the Case, timeline and rule readers are required; they are the built-in Tools")
	case d.Findings == nil:
		return nil, errors.New("investigator: a Finding publisher is required")
	case d.OrgControls == nil:
		return nil, errors.New("investigator: the org's controls are required; a run must be stoppable and budgeted")
	case d.Queue == nil:
		return nil, errors.New("investigator: a job queue is required; an Investigation runs asynchronously")
	case d.ToolServers == nil || d.Tokens == nil || d.ToolDialer == nil:
		return nil, errors.New("investigator: the ToolServer store, token resolver and dialer are required")
	case d.Redaction == nil:
		return nil, errors.New("investigator: the org's redaction rules are required; a Tool result is never recorded unredacted")
	}
	if d.Clock == nil {
		d.Clock = clock.New()
	}
	s := &Service{
		providers: d.Providers, creds: d.Credentials, keys: d.Keys,
		dial: d.Dialer, tx: d.Tx, clock: d.Clock,
		investigators: d.Investigators, investigations: d.Investigations,
		cases: d.Cases, timeline: d.Timeline, rules: d.Rules, findings: d.Findings,
		orgControls: d.OrgControls, queue: d.Queue, limits: d.Limits.orDefault(),
		toolServers: d.ToolServers, tokens: d.Tokens, toolDialer: d.ToolDialer, redaction: d.Redaction,
	}
	s.tools = builtinTools(s)
	return s, nil
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
