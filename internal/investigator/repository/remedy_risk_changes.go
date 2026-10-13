package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// RiskChangeRepository is every statement against `remedy_risk_changes` that a MEMBER can cause:
// proposing a change, reading the pending one, and discarding it (migration 00111; ADR 0054 §3,
// owner ruling O3).
//
// ⛔⛔ NOTHING HERE WRITES `remedy_risk_rules` OR `remedy_risk_settings`, and nothing here marks a
// change APPLIED. That transition is the same act as writing the rules, so it is the same raw SQL in
// internal/app, behind a different member's confirmation. A change this repository stores changes no
// Remedy's tier. test/scope/remedy_risk_rules_routes_test.go holds the mounted routes to match.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE: another org's change is the
// same answer as one that never existed.
type RiskChangeRepository struct {
	q db.Querier
}

// NewRiskChangeRepository builds the repository over a fallback querier; a transaction travelling
// in the context wins over it.
func NewRiskChangeRepository(q db.Querier) *RiskChangeRepository { return &RiskChangeRepository{q: q} }

func (r *RiskChangeRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapChangeErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "remedy_risk_change_not_found",
		NotFoundMessage:    "no such rule change",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// riskRuleJSON is a rule as `remedy_risk_changes.rules` holds it.
type riskRuleJSON struct {
	Name          string   `json:"name"`
	Tool          string   `json:"tool,omitempty"`
	Verbs         []string `json:"verbs"`
	Kinds         []string `json:"kinds"`
	Namespaces    []string `json:"namespaces"`
	Reversibility string   `json:"reversibility,omitempty"`
	Approvals     int      `json:"approvals"`
}

// EncodeRiskRules is the JSON `remedy_risk_changes.rules` stores for validated rules. Exported for
// internal/app, which re-reads the same column when it confirms.
func EncodeRiskRules(rules domain.RiskRules) ([]byte, error) {
	list := rules.Rules()
	out := make([]riskRuleJSON, 0, len(list))
	for _, r := range list {
		out = append(out, riskRuleJSON{Name: r.Name, Tool: r.Tool, Verbs: nonNil(r.Verbs), Kinds: nonNil(r.Kinds),
			Namespaces: nonNil(r.Namespaces), Reversibility: string(r.Reversibility), Approvals: r.Approvals})
	}
	return json.Marshal(out)
}

// DecodeRiskRules reads that JSON back as rules, UNVALIDATED: the caller that writes them
// re-validates with domain.NewRiskRules, so a rule the domain has since tightened is refused at
// confirmation and not written.
func DecodeRiskRules(raw []byte) ([]domain.RiskRule, error) {
	var in []riskRuleJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errs.Internal("remedy_risk_change_corrupt", err)
	}
	out := make([]domain.RiskRule, 0, len(in))
	for _, r := range in {
		out = append(out, domain.RiskRule{Name: r.Name, Tool: r.Tool, Verbs: r.Verbs, Kinds: r.Kinds,
			Namespaces: r.Namespaces, Reversibility: domain.Reversibility(r.Reversibility), Approvals: r.Approvals})
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ChangeColumns is the column list every read of a change selects, for internal/app's locked read.
const ChangeColumns = `id, org_id, rules, risk_model_provider_id, status, proposed_by, proposed_by_label,
       proposed_at, decided_by, decided_by_label`

// ScanChange reads one row selected with ChangeColumns.
func ScanChange(row pgx.Row) (domain.RiskChange, error) {
	var (
		c                          domain.RiskChange
		raw                        []byte
		model, proposer, decidedBy *uuid.UUID
		status                     string
		decidedLabel               *string
	)
	if err := row.Scan(&c.ID, &c.OrgID, &raw, &model, &status, &proposer, &c.ProposedBy.Label,
		&c.ProposedAt, &decidedBy, &decidedLabel); err != nil {
		return domain.RiskChange{}, err
	}
	rules, err := DecodeRiskRules(raw)
	if err != nil {
		return domain.RiskChange{}, err
	}
	if c.Rules, err = domain.RestoreRiskRules(rules); err != nil {
		return domain.RiskChange{}, err
	}
	if model != nil {
		c.RiskModelProviderID = *model
	}
	if proposer != nil {
		c.ProposedBy.UserID = *proposer
	}
	c.Status = domain.RiskChangeStatus(status)
	c.ProposedAt = c.ProposedAt.UTC()
	if decidedBy != nil {
		c.DecidedBy.UserID = *decidedBy
	}
	if decidedLabel != nil {
		c.DecidedBy.Label = *decidedLabel
	}
	return c, nil
}

const selectChangeSQL = `SELECT ` + ChangeColumns + ` FROM remedy_risk_changes WHERE org_id = $1 AND `

// Propose stores a validated change as the org's one pending change, superseding the pending one
// if there is one, under the org's risk-rules advisory lock (the lock `oto remedy-rules apply`
// takes), so a proposal and an apply cannot interleave.
//
// ⛔ IT STORES A PROPOSAL AND NOTHING ELSE: no rule, no setting, no Remedy is touched.
func (r *RiskChangeRepository) Propose(
	ctx context.Context, s db.TenantScope, by domain.Requester, rules domain.RiskRules, model uuid.UUID, at time.Time,
) (domain.RiskChange, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RiskChange{}, err
	}
	raw, err := EncodeRiskRules(rules)
	if err != nil {
		return domain.RiskChange{}, errs.Internal("remedy_risk_change_encode", err)
	}
	q := r.db(ctx)
	key := db.AdvisoryKey(db.LockNamespaceInvestigations, "remedy-risk/"+s.OrgID().String())
	if err := db.AdvisoryXactLock(ctx, q, key); err != nil {
		return domain.RiskChange{}, mapChangeErr(err, "lock the org's risk rules")
	}
	// The one that was pending is overtaken by this proposal, whoever made it.
	if _, err := q.Exec(ctx, `
UPDATE remedy_risk_changes
   SET status = 'superseded', decided_by = $2, decided_by_label = $3, decided_at = $4
 WHERE org_id = $1 AND status = 'pending'`, s.OrgID(), nilIfZero(by.UserID), by.Label, at.UTC()); err != nil {
		return domain.RiskChange{}, mapChangeErr(err, "supersede the pending rule change")
	}
	modelID := nilIfZero(model)
	row := q.QueryRow(ctx, `
INSERT INTO remedy_risk_changes
  (id, org_id, rules, risk_model_provider_id, status, proposed_by, proposed_by_label, proposed_at)
VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
RETURNING `+ChangeColumns, id.New(), s.OrgID(), raw, modelID, nilIfZero(by.UserID), by.Label, at.UTC())
	out, err := ScanChange(row)
	if err != nil {
		return domain.RiskChange{}, mapChangeErr(err, "store a rule change")
	}
	return out, nil
}

// Pending reads the org's pending change; ok is false when there is none.
func (r *RiskChangeRepository) Pending(ctx context.Context, s db.TenantScope) (domain.RiskChange, bool, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RiskChange{}, false, err
	}
	out, err := ScanChange(r.db(ctx).QueryRow(ctx, selectChangeSQL+`status = 'pending'`, s.OrgID()))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.RiskChange{}, false, nil
	case err != nil:
		return domain.RiskChange{}, false, mapChangeErr(err, "read the pending rule change")
	}
	return out, true, nil
}

// Get reads one change of any status.
func (r *RiskChangeRepository) Get(ctx context.Context, s db.TenantScope, changeID uuid.UUID) (domain.RiskChange, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RiskChange{}, err
	}
	if err := db.RequireID("change_id", changeID); err != nil {
		return domain.RiskChange{}, domain.RiskChangeNotFound()
	}
	out, err := ScanChange(r.db(ctx).QueryRow(ctx, selectChangeSQL+`id = $2`, s.OrgID(), changeID))
	if err != nil {
		return domain.RiskChange{}, mapChangeErr(err, "read a rule change")
	}
	return out, nil
}

// Discard marks a pending change discarded. It is refused (`remedy_risk_change_not_pending`) when
// the change was decided meanwhile, so a person who read a change cannot discard a newer one.
func (r *RiskChangeRepository) Discard(
	ctx context.Context, s db.TenantScope, changeID uuid.UUID, by domain.Requester, at time.Time,
) (domain.RiskChange, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RiskChange{}, err
	}
	if err := db.RequireID("change_id", changeID); err != nil {
		return domain.RiskChange{}, domain.RiskChangeNotFound()
	}
	out, err := ScanChange(r.db(ctx).QueryRow(ctx, `
UPDATE remedy_risk_changes
   SET status = 'discarded', decided_by = $3, decided_by_label = $4, decided_at = $5
 WHERE org_id = $1 AND id = $2 AND status = 'pending'
RETURNING `+ChangeColumns, s.OrgID(), changeID, nilIfZero(by.UserID), by.Label, at.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		// Either it never existed in this org, or it was decided: say which.
		cur, gerr := r.Get(ctx, s, changeID)
		if gerr != nil {
			return domain.RiskChange{}, gerr
		}
		return domain.RiskChange{}, domain.RiskChangeNotPending(cur.Status)
	}
	if err != nil {
		return domain.RiskChange{}, mapChangeErr(err, "discard a rule change")
	}
	return out, nil
}

func nilIfZero(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}
