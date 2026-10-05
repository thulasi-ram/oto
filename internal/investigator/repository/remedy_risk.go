package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// RemedyRiskRepository reads `remedy_risk_rules` and `remedy_risk_settings` (migration 00103):
// an org's Remedy risk rules, its risk model, and who last wrote them (ADR 0054 §3, git-bug
// eb4f21b).
//
// ⛔⛔ IT ONLY READS (owner ruling 2026-10-05). The one writer is `oto remedy-rules apply`, as
// raw statements in internal/app/remedyrules.go beside the approval grant's, for the grant's
// reason: a rule saying one lets one grant holder approve alone, and a repository write would
// be one handler away from a route.
//
// ⛔ NOTHING HERE TOUCHES `remedies`. A Remedy copied the NAME of the rule that set its tier
// onto its own frozen row; replacing the rules re-tiers no Remedy — by construction.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE.
type RemedyRiskRepository struct {
	q db.Querier
}

// NewRemedyRiskRepository builds the repository over a fallback querier; a transaction
// travelling in the context wins over it.
func NewRemedyRiskRepository(q db.Querier) *RemedyRiskRepository { return &RemedyRiskRepository{q: q} }

func (r *RemedyRiskRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapRiskErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "remedy_risk_not_found",
		NotFoundMessage:    "no risk rules",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// RemedyRisk reads the org's rules in the operator's order, its risk model and who last
// wrote them. An org that never wrote any has no rules and no model.
func (r *RemedyRiskRepository) RemedyRisk(ctx context.Context, s db.TenantScope) (domain.RemedyRiskSettings, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	var out domain.RemedyRiskSettings
	var provider *uuid.UUID
	err := r.db(ctx).QueryRow(ctx, `
SELECT risk_model_provider_id, written_by_label, written_at
  FROM remedy_risk_settings
 WHERE org_id = $1`, s.OrgID()).Scan(&provider, &out.WrittenByLabel, &out.WrittenAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return domain.RemedyRiskSettings{}, mapRiskErr(err, "read the risk settings")
	default:
		if provider != nil {
			out.RiskModelProviderID = *provider
		}
		out.WrittenAt = out.WrittenAt.UTC()
	}
	rows, err := r.db(ctx).Query(ctx, `
SELECT name, tool, verbs, kinds, namespaces, reversibility, approvals
  FROM remedy_risk_rules
 WHERE org_id = $1
 ORDER BY position`, s.OrgID())
	if err != nil {
		return domain.RemedyRiskSettings{}, mapRiskErr(err, "read the risk rules")
	}
	defer rows.Close()
	rules := []domain.RiskRule{}
	for rows.Next() {
		var rule domain.RiskRule
		var tool, rev *string
		if err := rows.Scan(&rule.Name, &tool, &rule.Verbs, &rule.Kinds, &rule.Namespaces, &rev, &rule.Approvals); err != nil {
			return domain.RemedyRiskSettings{}, mapRiskErr(err, "read the risk rules")
		}
		rule.Tool, rule.Reversibility = derefS(tool), domain.Reversibility(derefS(rev))
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return domain.RemedyRiskSettings{}, mapRiskErr(err, "read the risk rules")
	}
	if out.Rules, err = domain.RestoreRiskRules(rules); err != nil {
		return domain.RemedyRiskSettings{}, err
	}
	return out, nil
}
