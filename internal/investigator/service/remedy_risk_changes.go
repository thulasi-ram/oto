package service

// A RULE CHANGE MADE FROM THE APP TAKES A SECOND PERSON (ADR 0054 §3, owner ruling O3,
// 2026-10-06; migration 00111).
//
// The rules say whether a Remedy needs one approval or two, and they only ever loosen. 5ace8f3
// let any member write them, so a grant holder could write a one-approval rule and approve alone;
// the 2026-10-05 ruling closed that by making them host-shell-only, and O3 reopens the Settings
// screen on THIS condition: a member PROPOSES, and a DIFFERENT member CONFIRMS. Between the two
// nothing is written, so a proposal changes no Remedy's tier and a proposal nobody confirms is inert.
//
// ⛔ THE SERVICE ONLY PROPOSES, READS AND DISCARDS. Confirming is `RiskChangeApplier`, which lives in
// internal/app beside the host-shell writer; this service never holds a port that can replace a rule.
//
// ⛔ ACTOR KIND IS THE ROUTER'S. A rule change must come from a human at a browser session — the
// same rule an approval has, for the same reason: a script holding two members' tokens would
// otherwise be the two people. The router refuses anything else before this is called.

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ProposeRemedyRiskChange validates a proposed rule set and risk model as `oto remedy-rules apply`
// would, and stores it as the org's pending change, superseding any pending one. It returns the
// change; no rule is written.
func (s *Service) ProposeRemedyRiskChange(
	ctx context.Context, scope db.TenantScope, by domain.Requester, draft domain.RiskChangeDraft,
) (domain.RiskChange, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.RiskChange{}, err
	}
	rules, err := domain.ValidateRiskChange(draft)
	if err != nil {
		return domain.RiskChange{}, err
	}
	if draft.RiskModelProviderID != uuid.Nil {
		if _, err := s.providers.Get(ctx, scope, draft.RiskModelProviderID); err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return domain.RiskChange{}, errs.Validation("remedy_risk_model_not_found",
					"the risk model is not one of this org's model endpoints",
					errs.Violation{Field: "risk_model_provider_id", Code: "not_found",
						Message: "not one of this org's model endpoints"})
			}
			return domain.RiskChange{}, err
		}
	}
	var out domain.RiskChange
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.riskChanges.Propose(ctx, scope, by, rules, draft.RiskModelProviderID, s.now())
		return err
	})
	return out, err
}

// PendingRemedyRiskChange reads the org's pending change; ok is false when there is none.
func (s *Service) PendingRemedyRiskChange(ctx context.Context, scope db.TenantScope) (domain.RiskChange, bool, error) {
	return s.riskChanges.Pending(ctx, scope)
}

// ConfirmRemedyRiskChange applies a pending change as the org's rules, when `by` is not the one who
// proposed it. The refusals — not pending, the proposer, invalid rules — are typed; the applier
// re-checks them inside its own transaction, and the schema refuses a self-confirmed row after that.
func (s *Service) ConfirmRemedyRiskChange(
	ctx context.Context, scope db.TenantScope, changeID uuid.UUID, by domain.Requester,
) (domain.RemedyRiskSettings, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	change, err := s.riskChanges.Get(ctx, scope, changeID)
	if err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	if err := change.ConfirmableBy(by); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return s.riskApplier.Confirm(ctx, scope, changeID, by, s.now())
}

// DiscardRemedyRiskChange withdraws or refuses a pending change. Anyone may: saying no is the safe
// direction, and the proposer withdrawing their own is the same act.
func (s *Service) DiscardRemedyRiskChange(
	ctx context.Context, scope db.TenantScope, changeID uuid.UUID, by domain.Requester,
) error {
	if err := db.RequireScope(scope); err != nil {
		return err
	}
	_, err := s.riskChanges.Discard(ctx, scope, changeID, by, s.now())
	return err
}
