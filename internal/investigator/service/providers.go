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
	"strings"
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
	incidents      IncidentReader
	digests        DigestReader
	declarer       FindingDeclarer
	timeline       TimelineReader
	rules          RuleReader
	findings       FindingPublisher
	orgControls    OrgControls
	classes        ClassStore
	queue          JobQueue
	limits         Limits
	tools          []Tool

	toolServers ToolServerStore
	tokens      TokenResolver
	toolDialer  ToolServerDialer
	redaction   RedactionRules

	suggestions SuggestionStore
	policies    PolicyEditor
	memberships MembershipEditor
	approvers   RemedyApprovers

	remedies       RemedyStore
	remedyDeclarer RemedyDeclarer
	remedyRisk     RemedyRiskStore
	riskChanges    RiskChangeStore
	riskApplier    RiskChangeApplier
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
	// Incidents reads an Incident and the one a Case is in; Declarer declares an
	// Incident's new Finding outbound (git-bug 74ea849).
	Incidents IncidentReader
	Declarer  FindingDeclarer
	// Digests reads the digest windows a policy asked to have summarised, and their
	// Cases (git-bug 3e96f5a).
	Digests     DigestReader
	Timeline    TimelineReader
	Rules       RuleReader
	Findings    FindingPublisher
	OrgControls OrgControls
	// Classes is the org's Classification set (ADR 0053 §5, git-bug 4298aa0): what a
	// run offers the model to classify its Finding in, and the settings API's store.
	Classes ClassStore
	Queue   JobQueue
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

	// Suggestions are a Finding's proposals (git-bug 8327c00); Policies and Memberships
	// read what a proposal names and make the ORDINARY edit when a human applies one.
	Suggestions SuggestionStore
	Policies    PolicyEditor
	Memberships MembershipEditor

	// Approvers reads who holds the Remedy approval grant on a write ToolServer (ADR 0054
	// §4, git-bug 47f67c8). Read-only: a grant is written only from the host shell.
	Approvers RemedyApprovers

	// Remedies are a Finding's proposed cluster changes, their approvals and transitions
	// (ADR 0054, git-bug 4148256); RemedyDeclarer declares each transition outbound.
	Remedies       RemedyStore
	RemedyDeclarer RemedyDeclarer
	// RemedyRisk is the org's risk rules and risk model (ADR 0054 §3, git-bug eb4f21b): what
	// sets how many approvals a Remedy needs at its proposal.
	RemedyRisk RemedyRiskStore
	// RiskChanges holds proposed changes to those rules, and RiskApplier writes a confirmed one
	// (ADR 0054 §3, owner ruling O3): a rule change from the app takes a different member to confirm.
	RiskChanges RiskChangeStore
	RiskApplier RiskChangeApplier
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
	case d.Incidents == nil:
		return nil, errors.New("investigator: an Incident reader is required; an Incident is investigated as a whole, " +
			"and a Case in one starts nothing of its own automatically")
	case d.Digests == nil:
		return nil, errors.New("investigator: a digest reader is required; a digest window a policy asked to have " +
			"summarised is read through it")
	case d.Declarer == nil:
		return nil, errors.New("investigator: a Finding declarer is required; an Incident's new Finding goes outbound as a fact")
	case d.Findings == nil:
		return nil, errors.New("investigator: a Finding publisher is required")
	case d.OrgControls == nil:
		return nil, errors.New("investigator: the org's controls are required; a run must be stoppable and budgeted")
	case d.Classes == nil:
		return nil, errors.New("investigator: the org's class store is required; a Finding is classified only in the operator's words")
	case d.Queue == nil:
		return nil, errors.New("investigator: a job queue is required; an Investigation runs asynchronously")
	case d.ToolServers == nil || d.Tokens == nil || d.ToolDialer == nil:
		return nil, errors.New("investigator: the ToolServer store, token resolver and dialer are required")
	case d.Redaction == nil:
		return nil, errors.New("investigator: the org's redaction rules are required; a Tool result is never recorded unredacted")
	case d.Suggestions == nil || d.Policies == nil || d.Memberships == nil:
		return nil, errors.New("investigator: the Suggestion store and the policy and membership editors are required; " +
			"a Finding's Suggestion is applied only through the ordinary edit")
	case d.Approvers == nil:
		return nil, errors.New("investigator: the Remedy approver grants are required; who may approve a Remedy " +
			"is read through them")
	case d.Remedies == nil || d.RemedyDeclarer == nil:
		return nil, errors.New("investigator: the Remedy store and declarer are required; a Remedy's every transition " +
			"is recorded and goes outbound as a fact")
	case d.RiskChanges == nil || d.RiskApplier == nil:
		return nil, errors.New("investigator: the risk-rule change store and applier are required; a rule change " +
			"from the app is proposed, and written only when a different member confirms it")
	case d.RemedyRisk == nil:
		return nil, errors.New("investigator: the Remedy risk rules are required; how many approvals a Remedy " +
			"needs is set from them when it is proposed")
	}
	if d.Clock == nil {
		d.Clock = clock.New()
	}
	s := &Service{
		providers: d.Providers, creds: d.Credentials, keys: d.Keys,
		dial: d.Dialer, tx: d.Tx, clock: d.Clock,
		investigators: d.Investigators, investigations: d.Investigations,
		cases: d.Cases, incidents: d.Incidents, digests: d.Digests, declarer: d.Declarer,
		timeline: d.Timeline, rules: d.Rules, findings: d.Findings,
		orgControls: d.OrgControls, classes: d.Classes, queue: d.Queue, limits: d.Limits.orDefault(),
		toolServers: d.ToolServers, tokens: d.Tokens, toolDialer: d.ToolDialer, redaction: d.Redaction,
		suggestions: d.Suggestions, policies: d.Policies, memberships: d.Memberships,
		approvers: d.Approvers,
		remedies:  d.Remedies, remedyDeclarer: d.RemedyDeclarer, remedyRisk: d.RemedyRisk,
		riskChanges: d.RiskChanges, riskApplier: d.RiskApplier,
	}
	// The proposing Tools are built in too, and held only when an allowlist names them
	// (git-bug 8327c00).
	s.tools = append(builtinTools(s), proposingTools(s)...)
	// So are the two Remedy Tools (git-bug 4148256): one that lists the write Tools a Remedy
	// may name, and one that proposes. ⛔ Neither calls a write Tool.
	s.tools = append(s.tools, remedyTools(s)...)
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

// RotateProviderKey replaces an endpoint's API key and returns the endpoint without it.
//
// ⛔ ONLY THE KEY MOVES. `base_url` and `model` are the identity an Investigator version
// pins, so a rotation cannot trip `model_changed` and cannot rewrite what a past Finding
// says produced it. A stored key is re-sealed in place, so nothing pointing at it moves; an
// endpoint that took none gets a new row. The key is checked against the endpoint's scheme
// as at create: a key is only ever sent over https.
//
// A key travels only here and into the sealer: no error below renders it.
func (s *Service) RotateProviderKey(ctx context.Context, scope db.TenantScope, providerID uuid.UUID, apiKey string) (domain.ProviderConfig, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.ProviderConfig{}, err
	}
	if apiKey == "" || strings.TrimSpace(apiKey) == "" {
		return domain.ProviderConfig{}, errs.Validation("model_api_key_required",
			"an API key is required",
			errs.Violation{Field: "api_key", Code: "required", Message: "an API key is required"})
	}
	at := s.now()
	var out domain.ProviderConfig
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		cfg, err := s.providers.Get(ctx, scope, providerID)
		if err != nil {
			return err
		}
		if err := domain.KeyNeedsHTTPS(cfg.BaseURL, true); err != nil {
			return err
		}
		values := map[string]string{domain.CredentialValueKey: apiKey}
		if cfg.HasKey() {
			if err := s.creds.RotateCredential(ctx, scope, cfg.CredentialID, domain.CredentialKind, values); err != nil {
				return err
			}
			out, err = s.providers.Touch(ctx, scope, providerID, at)
			return err
		}
		id, err := s.creds.CreateCredential(ctx, scope, domain.CredentialKind, values)
		if err != nil {
			return err
		}
		out, err = s.providers.SetCredential(ctx, scope, providerID, id, at)
		return err
	})
	if err != nil {
		return domain.ProviderConfig{}, err
	}
	return out, nil
}

// DeleteProvider removes an endpoint and its sealed key.
//
// ⛔ TWO THINGS HOLD IT, BOTH REFUSED AS A 409 THAT SAYS WHICH. An Investigator version that
// dials it (the row refuses, `model_provider_in_use`), and the Remedy risk model naming it —
// whose column would silently go NULL on delete, leaving every Remedy needing the second
// approval for a reason nobody can see.
func (s *Service) DeleteProvider(ctx context.Context, scope db.TenantScope, providerID uuid.UUID) error {
	if err := db.RequireScope(scope); err != nil {
		return err
	}
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		cfg, err := s.providers.Get(ctx, scope, providerID)
		if err != nil {
			return err
		}
		risk, err := s.remedyRisk.RemedyRisk(ctx, scope)
		if err != nil {
			return err
		}
		if risk.RiskModelProviderID == providerID {
			return errs.Conflict("model_provider_is_risk_model",
				"the Remedy risk model is this endpoint; choose another model for it before deleting this one")
		}
		if err := s.providers.Delete(ctx, scope, providerID); err != nil {
			return err
		}
		if cfg.HasKey() {
			return s.creds.DeleteCredential(ctx, scope, cfg.CredentialID)
		}
		return nil
	})
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
