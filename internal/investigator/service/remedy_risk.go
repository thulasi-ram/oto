package service

// HOW MANY APPROVALS A REMEDY NEEDS (ADR 0054 §3; git-bug eb4f21b).
//
// An operator writes the org's risk rules and, optionally, names a model endpoint as the risk
// model, in one YAML file applied from the host shell (`oto remedy-rules apply`), replaced
// whole. When a run's
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
// ⭐⭐ A MODEL'S TOKENS ARE RECORDED ON THE REMEDY AND COUNT AGAINST THE ORG'S DAILY BUDGET
// (owner ruling 2026-10-05 on git-bug eb4f21b, superseding 5ace8f3's "not budgeted"). The day's
// spend (InvestigationStore.SpentSince) sums them beside every Investigation's model turns, so a
// risk question delays the next run like any other spend; and when the day is already spent the
// question is NOT asked and the Remedy needs two, recorded `budget` (domain.RiskVerdict.BudgetSpent)
// — fail closed: an unpaid check never lets one approval stand.
//
// ⛔ AND THE RULES ARE NOT WRITTEN HERE. They are read. They are written from the host shell by
// `oto remedy-rules apply`, or by a CONFIRMED change (remedy_risk_changes.go): a rule saying one lets
// one grant holder approve alone, so a change from the app takes a second person. Both writers are in
// internal/app (remedyrules.go, remedyrulechanges.go).

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// RemedyRisk reads the org's risk rules, its risk model and who last wrote them.
//
// ⛔⛔ THERE IS NO WRITE HERE, AND NONE ON THE PORT (owner ruling 2026-10-05 on git-bug
// eb4f21b). The rules are written in internal/app, by `oto remedy-rules apply` from the host shell
// or by a change a DIFFERENT member confirmed (ADR 0054 §3, owner ruling O3): a rule saying one lets
// one grant holder approve alone, so writing one is the same authority as granting a second approver
// (ADR 0054 §4), and it lives where the grant does.
func (s *Service) RemedyRisk(ctx context.Context, scope db.TenantScope) (domain.RemedyRiskSettings, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return s.remedyRisk.RemedyRisk(ctx, scope)
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
		day      *dayBudget
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
			// ⭐ THE QUESTION IS PAID FOR FROM THE DAY'S BUDGET, OR IT IS NOT ASKED (owner ruling
			// 2026-10-05). Read once, at the first question, and carried forward by what each
			// answer cost: those tokens are recorded on the Remedy only when the Finding's
			// transaction commits, so a re-read would not yet see them.
			if day == nil {
				if day, err = s.readDayBudget(ctx, scope); err != nil {
					return nil, err
				}
			}
			if why := day.controls.BudgetSpent(day.spent, day.at); why != "" {
				out[i] = verdict.BudgetSpent(why)
				continue
			}
			if !opened {
				model, modelErr = s.OpenProvider(ctx, scope, settings.RiskModelProviderID)
				opened = true
			}
			out[i] = assessRemedy(ctx, model, modelErr, d, verdict)
			day.spent += out[i].ModelTokens
		}
	}
	return out, nil
}

// dayBudget is what the risk model's question is checked against: the org's controls and what
// it has spent today — every Investigation Step's model turn and every Remedy's risk question
// (InvestigationStore.SpentSince).
type dayBudget struct {
	controls domain.OrgControls
	spent    int64
	at       time.Time
}

func (s *Service) readDayBudget(ctx context.Context, scope db.TenantScope) (*dayBudget, error) {
	controls, err := s.orgControls.InvestigationControls(ctx, scope)
	if err != nil {
		return nil, err
	}
	at := s.now()
	spent, err := s.investigations.SpentSince(ctx, scope, domain.DayStart(at))
	if err != nil {
		return nil, err
	}
	return &dayBudget{controls: controls, spent: spent, at: at}, nil
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
