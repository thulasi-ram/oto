package service

// INVESTIGATORS IN SETTINGS (ADR 0053 §1, §6; git-bug 180a525): create one, change one,
// list them.
//
// ⭐⭐ A CHANGE WRITES A NEW VERSION ONLY WHEN WHAT PRODUCES A FINDING CHANGED. §6:
// "Changing an Investigator's model, prompt or allowlist makes a new version; a Finding
// names the version that produced it." So a change carrying the versioned half is
// compared against the current version — the endpoint row, the identity that row reports
// NOW, the prompt, the allowlist — and only a difference mints version N+1. Re-sending the
// same prompt is not a new version; flipping the kill switch or a budget never is.
//
// ⛔ A TOOLSERVER TOOL ON THE ALLOWLIST MUST BE ONE A READ TOOLSERVER HAS LISTED (git-bug
// 2e9a086). `<toolserver>__<tool>` naming a write ToolServer, an unknown one, or a Tool
// it never listed is a 422 when the allowlist is written — and the run refuses the
// same call again, recorded, because a version outlives the moment it was written.
//
// ⭐ THE IDENTITY IS READ FROM THE ENDPOINT ROW AT WRITE TIME AND PINNED. A version names
// (base_url, model) as they stood when it was written, so a Finding names the model that
// produced it even if the row it was read from is later edited — and a run refuses to
// start (`model_changed`) rather than let the two disagree.

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// InvestigatorDetail is one Investigator with every version it has had, newest first.
type InvestigatorDetail struct {
	Investigator domain.Investigator
	Versions     []domain.Version
}

// CreateInvestigator stores an Investigator and its version 1.
func (s *Service) CreateInvestigator(ctx context.Context, scope db.TenantScope, d domain.InvestigatorDraft) (domain.Investigator, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigator{}, err
	}
	endpoint, err := s.providers.Get(ctx, scope, d.Spec.ProviderID)
	if err != nil {
		return domain.Investigator{}, err
	}
	if err := s.checkToolServerAllowlist(ctx, scope, d.Spec.Tools); err != nil {
		return domain.Investigator{}, err
	}
	var out domain.Investigator
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		inv, err := s.investigators.Create(ctx, scope, d, endpoint.Identity(), s.now())
		out = inv
		return err
	})
	return out, err
}

// UpdateInvestigator applies a change, writing a new version when the versioned half
// differs from the current one, and returns the Investigator as it now stands.
func (s *Service) UpdateInvestigator(
	ctx context.Context, scope db.TenantScope, id uuid.UUID, change domain.InvestigatorChange,
) (domain.Investigator, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigator{}, err
	}
	at := s.now()
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		cur, err := s.investigators.Lock(ctx, scope, id)
		if err != nil {
			return err
		}
		enabled, budgets := cur.Enabled, cur.Budgets
		if change.Enabled != nil {
			enabled = *change.Enabled
		}
		if change.Budgets != nil {
			budgets = *change.Budgets
		}
		if enabled != cur.Enabled || budgets != cur.Budgets {
			if err := s.investigators.Update(ctx, scope, id, enabled, budgets, at); err != nil {
				return err
			}
		}
		if !change.TouchesVersion() {
			return nil
		}
		spec, err := change.Apply(cur.Current)
		if err != nil {
			return err
		}
		if change.Tools != nil {
			if err := s.checkToolServerAllowlist(ctx, scope, spec.Tools); err != nil {
				return err
			}
		}
		endpoint, err := s.providers.Get(ctx, scope, spec.ProviderID)
		if err != nil {
			return err
		}
		if !cur.Current.NeedsNewVersion(spec, endpoint.Identity()) {
			return nil
		}
		_, err = s.investigators.AddVersion(ctx, scope, id, cur.Current.Number+1, spec, endpoint.Identity(), at)
		return err
	})
	if err != nil {
		return domain.Investigator{}, err
	}
	return s.investigators.Get(ctx, scope, id)
}

// ListInvestigators reads an org's Investigators by name, each with its current version.
func (s *Service) ListInvestigators(ctx context.Context, scope db.TenantScope) ([]domain.Investigator, error) {
	return s.investigators.List(ctx, scope)
}

// GetInvestigator reads one Investigator and its versions.
func (s *Service) GetInvestigator(ctx context.Context, scope db.TenantScope, id uuid.UUID) (InvestigatorDetail, error) {
	inv, err := s.investigators.Get(ctx, scope, id)
	if err != nil {
		return InvestigatorDetail{}, err
	}
	versions, err := s.investigators.Versions(ctx, scope, id)
	if err != nil {
		return InvestigatorDetail{}, err
	}
	return InvestigatorDetail{Investigator: inv, Versions: versions}, nil
}
