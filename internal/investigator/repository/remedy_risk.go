package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// RemedyRiskRepository is every statement against `remedy_risk_rules` and
// `remedy_risk_settings` (migration 00103): an org's Remedy risk rules, its risk model, and who
// last wrote them (ADR 0054 §3, git-bug eb4f21b).
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

// ReplaceRemedyRisk writes the org's whole rule set and its risk model, and who wrote them,
// in the caller's transaction: the old rules go and the new ones are written in order.
//
// ⭐ SERIALISED ON THE ORG'S ADVISORY LOCK, for ReplaceClassSet's reason: two operators saving
// at once cannot both delete the old rules and collide on the primary key.
func (r *RemedyRiskRepository) ReplaceRemedyRisk(
	ctx context.Context, s db.TenantScope, set domain.RemedyRiskSettings, by domain.Requester, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	key := db.AdvisoryKey(db.LockNamespaceInvestigations, "remedy-risk/"+s.OrgID().String())
	if err := db.AdvisoryXactLock(ctx, r.db(ctx), key); err != nil {
		return errs.Internal("remedy_risk_lock", err)
	}
	if _, err := r.db(ctx).Exec(ctx, `
INSERT INTO remedy_risk_settings (org_id, risk_model_provider_id, written_by, written_by_label, written_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id) DO UPDATE
   SET risk_model_provider_id = EXCLUDED.risk_model_provider_id,
       written_by = EXCLUDED.written_by, written_by_label = EXCLUDED.written_by_label,
       written_at = EXCLUDED.written_at`,
		s.OrgID(), nullableID(set.RiskModelProviderID), nullableID(by.UserID), by.Label, at.UTC()); err != nil {
		return mapRiskErr(err, "store the risk settings")
	}
	if _, err := r.db(ctx).Exec(ctx, `DELETE FROM remedy_risk_rules WHERE org_id = $1`, s.OrgID()); err != nil {
		return mapRiskErr(err, "replace the risk rules")
	}
	rules := set.Rules.Rules()
	if len(rules) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for i, rule := range rules {
		batch.Queue(`INSERT INTO remedy_risk_rules
  (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			s.OrgID(), rule.Name, i, nullable(rule.Tool), rule.Verbs, rule.Kinds, rule.Namespaces,
			nullable(string(rule.Reversibility)), rule.Approvals, at.UTC())
	}
	if err := r.db(ctx).SendBatch(ctx, batch).Close(); err != nil {
		return mapRiskErr(err, "store the risk rules")
	}
	return nil
}
