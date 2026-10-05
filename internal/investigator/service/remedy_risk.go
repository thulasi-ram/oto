package service

// HOW MANY APPROVALS A REMEDY NEEDS (ADR 0054 §3; git-bug eb4f21b).
//
// An operator writes the org's risk rules and, optionally, names a model endpoint as the risk
// model; both are read whole and replaced whole, like the Classification set. When a run's
// Finding is recorded, every Remedy it proposed that names a Tool is ASSESSED, before the
// Finding's transaction: its command is parsed (domain.ParseRemedyCommand), the rules give
// their verdict (domain.RiskRules.Evaluate), and — only when the verdict is ONE approval and
// the org names a risk model — the model is asked, once, through the model port, and its
// answer may only raise the tier (domain.RaiseOnly). The tier is `required_approvals`, and how
// it was set is recorded on the Remedy and shown under its exact command.
//
// ⛔⛔ THE RISK MODEL SEES ONLY THE COMMAND, ITS TARGET AND THE RULES' VERDICT. assessRemedy
// is handed the draft and the verdict — never the run, its Steps, its Finding or any Tool's
// answer — and builds the one request from them (domain.RiskModelRequest).
//
// ⛔ ASKING IS OUTSIDE ANY TRANSACTION, AND A MODEL THAT FAILS LEAVES TWO. The call is bounded
// by domain.RiskModelTimeout; a failure, an answer without usage, or an answer that is neither
// one nor two is recorded on the Remedy as `failed` and the Remedy needs two.
//
// ⚠️ A MODEL'S TOKENS ARE RECORDED ON THE REMEDY, NOT AGAINST THE ORG'S DAILY BUDGET. The
// daily budget bounds Investigations (ADR 0053 §6); one bounded question per proposed Remedy,
// at most three per run, is recorded where it was spent.

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// RemedyRisk reads the org's risk rules, its risk model and who last wrote them.
func (s *Service) RemedyRisk(ctx context.Context, scope db.TenantScope) (domain.RemedyRiskSettings, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return s.remedyRisk.RemedyRisk(ctx, scope)
}

// ReplaceRemedyRisk writes the org's whole rule set and its risk model — no rules, which is
// how an operator makes every Remedy need two again, and no model included — records who
// wrote them, and returns them as stored. A risk model must be an endpoint this org has.
//
// ⛔ IT RE-TIERS NOTHING ALREADY PROPOSED. A Remedy's tier is set once, at its proposal, and
// frozen with it; new rules decide the next Remedy's.
func (s *Service) ReplaceRemedyRisk(
	ctx context.Context, scope db.TenantScope, set domain.RemedyRiskSettings, by domain.Requester,
) (domain.RemedyRiskSettings, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	if by.UserID == uuid.Nil || by.Label == "" {
		return domain.RemedyRiskSettings{}, errs.Forbidden("forbidden", "the risk rules are written by a person, who is recorded")
	}
	if set.RiskModelProviderID != uuid.Nil {
		if _, err := s.providers.Get(ctx, scope, set.RiskModelProviderID); err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return domain.RemedyRiskSettings{}, errs.Validation("risk_model_not_found",
					"the risk model is one of this org's model endpoints",
					errs.Violation{Field: "risk_model_provider_id", Code: "not_found", Message: "no such model endpoint"})
			}
			return domain.RemedyRiskSettings{}, err
		}
	}
	at := s.now()
	var out domain.RemedyRiskSettings
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.remedyRisk.ReplaceRemedyRisk(ctx, scope, set, by, at); err != nil {
			return err
		}
		read, err := s.remedyRisk.RemedyRisk(ctx, scope)
		out = read
		return err
	})
	if err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return out, nil
}

// assessRemedies sets each draft's tier, in order: the zero RemedyRisk for a draft that names
// no Tool (it needs two and nobody can approve it), and otherwise the rules' verdict, raised
// by the risk model when one is configured and the verdict is one. The settings are read once
// and the model endpoint opened at most once.
func (s *Service) assessRemedies(ctx context.Context, scope db.TenantScope, drafts []domain.RemedyDraft) ([]domain.RemedyRisk, error) {
	out := make([]domain.RemedyRisk, len(drafts))
	named := false
	for _, d := range drafts {
		named = named || d.Tool.Named()
	}
	if !named {
		return out, nil
	}
	settings, err := s.remedyRisk.RemedyRisk(ctx, scope)
	if err != nil {
		return nil, err
	}
	var (
		model    domain.ModelProvider
		modelErr error
		opened   bool
	)
	for i, d := range drafts {
		if !d.Tool.Named() {
			continue
		}
		verdict := settings.Rules.Evaluate(domain.ParseRemedyCommand(d.Tool, d.Arguments))
		switch {
		case settings.RiskModelProviderID == uuid.Nil:
			out[i] = verdict.Settle(domain.ModelUnset)
		case verdict.Approvals != domain.SingleApproval:
			out[i] = verdict.Settle(domain.ModelNotAsked)
		default:
			if !opened {
				model, modelErr = s.OpenProvider(ctx, scope, settings.RiskModelProviderID)
				opened = true
			}
			out[i] = assessRemedy(ctx, model, modelErr, d, verdict)
		}
	}
	return out, nil
}

// assessRemedy asks the risk model about ONE draft the rules said needs one approval. ⛔ Its
// inputs are the draft — the Tool, its exact arguments, the target — and the verdict; there is
// nothing else here for a request to be built from.
func assessRemedy(ctx context.Context, model domain.ModelProvider, openErr error, d domain.RemedyDraft, v domain.RiskVerdict) domain.RemedyRisk {
	if openErr != nil {
		return domain.RaiseOnly(v, domain.RiskModelAnswer{Err: openErr})
	}
	req, err := domain.RiskModelRequest(domain.ParseRemedyCommand(d.Tool, d.Arguments), d.Arguments, d.Target, v)
	if err != nil {
		return domain.RaiseOnly(v, domain.RiskModelAnswer{Err: err, Identity: model.Identity()})
	}
	callCtx, cancel := context.WithTimeout(ctx, domain.RiskModelTimeout)
	defer cancel()
	turn, err := model.Complete(callCtx, req)
	return domain.RaiseOnly(v, domain.ReadRiskAnswer(model.Identity(), turn, err))
}
