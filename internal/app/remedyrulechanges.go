package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⭐⭐ THE SECOND WAY AN ORG'S REMEDY RISK RULES ARE WRITTEN: A CHANGE A DIFFERENT MEMBER CONFIRMED
// (ADR 0054 §3, owner ruling O3, 2026-10-06; migration 00111). The first is `oto remedy-rules apply`
// (remedyrules.go), and these two are the only writers of `remedy_risk_rules` and
// `remedy_risk_settings`: they share writeRemedyRules, and a repository the investigator module holds
// still only reads.
//
// ⛔⛔ WHY CONFIRMING IS HERE AND NOT IN THE SERVICE. A rule saying ONE lets one grant holder approve
// a Remedy alone, so writing one has the authority of granting a second approver. A member who
// proposes a change writes nothing; this applier is what a second member's confirmation reaches, and
// it takes nothing on trust from its caller. Inside its own transaction, under the org's risk-rules
// advisory lock, it re-reads the change `FOR UPDATE` and re-checks that it is still pending, that the
// confirmer is not its proposer, and that its rules are valid by today's domain — and the schema
// then refuses a self-confirmed row (`remedy_risk_changes_two_people_ck`) even if all of that were wrong.
//
// ⭐ WHAT THE CONFIRMER READ IS WHAT IS WRITTEN: the change's rules are frozen by a trigger, and the
// confirmation names the change's id, so a change superseded meanwhile is refused and not replaced
// by the newer one.
//
// ⛔ NO REMEDY ALREADY PROPOSED IS RE-TIERED. A Remedy's tier is set once, at its proposal, and frozen
// with it; the new rules decide the next one's.

// RemedyRiskApplier satisfies investigator/service.RiskChangeApplier.
type RemedyRiskApplier struct {
	pool *pgxpool.Pool
}

// NewRemedyRiskApplier builds the applier over the pool.
func NewRemedyRiskApplier(pool *pgxpool.Pool) *RemedyRiskApplier {
	return &RemedyRiskApplier{pool: pool}
}

const (
	selectRemedyRiskChangeForUpdateSQL = `
SELECT ` + investigatorrepo.ChangeColumns + `
  FROM remedy_risk_changes
 WHERE org_id = $1 AND id = $2
   FOR UPDATE`

	markRemedyRiskChangeAppliedSQL = `
UPDATE remedy_risk_changes
   SET status = 'applied', decided_by = $3, decided_by_label = $4, decided_at = $5
 WHERE org_id = $1 AND id = $2 AND status = 'pending'`
)

// maxWriterLabel is `remedy_risk_settings_label_ck`'s upper bound.
const maxWriterLabel = 200

// Confirm writes a pending change as the org's rules and risk model, recording the confirmer as who
// wrote them and the proposer in the label, and marks the change applied. It returns the rules as
// they now stand.
func (a *RemedyRiskApplier) Confirm(
	ctx context.Context, scope db.TenantScope, changeID uuid.UUID, by investigatordomain.Requester, at time.Time,
) (investigatordomain.RemedyRiskSettings, error) {
	if a == nil || a.pool == nil {
		return investigatordomain.RemedyRiskSettings{}, errors.New("a database pool is required")
	}
	if err := db.RequireScope(scope); err != nil {
		return investigatordomain.RemedyRiskSettings{}, err
	}
	var out investigatordomain.RemedyRiskSettings
	err := db.Tx(ctx, a.pool, func(ctx context.Context) error {
		q := db.FromContext(ctx, a.pool)
		key := db.AdvisoryKey(db.LockNamespaceInvestigations, "remedy-risk/"+scope.OrgID().String())
		if err := db.AdvisoryXactLock(ctx, q, key); err != nil {
			return fmt.Errorf("lock the org's risk rules: %w", err)
		}
		change, err := investigatorrepo.ScanChange(q.QueryRow(ctx, selectRemedyRiskChangeForUpdateSQL, scope.OrgID(), changeID))
		if errors.Is(err, pgx.ErrNoRows) {
			return investigatordomain.RiskChangeNotFound()
		}
		if err != nil {
			return fmt.Errorf("read the rule change: %w", err)
		}
		if err := change.ConfirmableBy(by); err != nil {
			return err
		}
		// ⭐ Re-validated by today's domain, not trusted from when it was proposed: a rule the domain
		// has since tightened is refused here and not written.
		raw, err := investigatorrepo.EncodeRiskRules(change.Rules)
		if err != nil {
			return err
		}
		list, err := investigatorrepo.DecodeRiskRules(raw)
		if err != nil {
			return err
		}
		rules, err := investigatordomain.NewRiskRules(list)
		if err != nil {
			return err
		}
		var model *uuid.UUID
		if change.RiskModelProviderID != uuid.Nil {
			id := change.RiskModelProviderID
			model = &id
		}
		var confirmer *uuid.UUID
		if by.UserID != uuid.Nil {
			id := by.UserID
			confirmer = &id
		}
		label := fmt.Sprintf("%s, confirming %s's change", by.Label, change.ProposedBy.Label)
		if r := []rune(label); len(r) > maxWriterLabel {
			label = string(r[:maxWriterLabel])
		}
		if _, err := writeRemedyRules(ctx, q, scope.OrgID(), rules, model, confirmer, label, at.UTC()); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, markRemedyRiskChangeAppliedSQL, scope.OrgID(), changeID, confirmer, by.Label, at.UTC())
		if err != nil {
			return fmt.Errorf("mark the rule change applied: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return investigatordomain.RiskChangeNotPending(change.Status)
		}
		if out, err = investigatorrepo.NewRemedyRiskRepository(q).RemedyRisk(ctx, scope); err != nil {
			return fmt.Errorf("read the rules back: %w", err)
		}
		return nil
	})
	return out, err
}
