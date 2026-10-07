package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐⭐ THE HOST-SHELL WAY AN ORG'S REMEDY RISK RULES ARE WRITTEN (ADR 0054 §3; owner ruling
// 2026-10-05 on git-bug eb4f21b). `oto remedy-rules apply` calls ApplyRemedyRules. The only other
// writer of `remedy_risk_rules` and `remedy_risk_settings` is RemedyRiskApplier
// (remedyrulechanges.go), which writes a change that a DIFFERENT member confirmed (owner ruling O3,
// 2026-10-06). Both go through writeRemedyRules below; nothing else in the product writes them.
//
// ⛔ A SUBCOMMAND AND NOT A ROUTE, for the approval grant's reason (remedyapprover.go). A rule
// saying ONE lets one grant holder approve a Remedy alone. 5ace8f3 made the rules writable by
// any org member over `PUT /api/v1/remedy-risk-rules`, so a grant holder could write a
// one-approval rule and then approve alone — the loophole ADR 0054 §4 closed for grants. Writing
// a rule is the same authority as granting a second approver, so it takes the same thing: a
// shell on the host and the database credentials. test/scope/remedy_risk_rules_routes_test.go
// walks the mounted router to hold that no route writes one.
//
// ⚠️ RAW WRITES IN `internal/app`, beside the grant's, for the same reason as the grant's: a
// repository method that replaced the rules would be one call away from every service holding
// that repository. `investigator/repository.RemedyRiskRepository` only reads.
//
// ⭐ ONE FILE, APPLIED WHOLE, IN ONE TRANSACTION. The rules are one list whose order names the
// rule that set a tier; a partial edit of it is a list nobody wrote. A file that does not parse,
// names an unknown key, breaks a rule, or names a risk model the org does not have changes
// NOTHING: it is refused before the transaction opens, or inside it before the first write.
//
// ⛔ NO REMEDY ALREADY PROPOSED IS RE-TIERED. A Remedy's tier is set once, at its proposal, and
// frozen with it (`remedies_refuse_rewrite`); new rules decide the next Remedy's.

// RemedyRulesWriter is the label every applied rule set is recorded under. ⭐ Not a person:
// whoever ran it had the host's shell, which oto cannot name; the label says how it was written.
const RemedyRulesWriter = "oto remedy-rules apply"

// RemedyRulesFile is the YAML an operator applies. ⭐ Every key is known: a misspelt one —
// `namespace:` for `namespaces:` — would silently drop a condition and BROADEN the rule, so the
// decoder refuses unknown keys rather than ignore them.
type RemedyRulesFile struct {
	// RiskModel names one of the org's model endpoints, asked whether a Remedy the rules say
	// needs one approval should need two. Absent or null for none.
	RiskModel *string `yaml:"risk_model"`
	// Rules is the whole list, in order. ⭐ Required, and `rules: []` is legal: it is how an
	// operator says "every Remedy needs two", and an absent key is more likely a broken file.
	Rules *[]RemedyRuleYAML `yaml:"rules"`
}

// RemedyRuleYAML is one rule as the operator writes it: the names of `RemedyRiskRuleDTO`.
type RemedyRuleYAML struct {
	Name          string   `yaml:"name"`
	Tool          string   `yaml:"tool,omitempty"`
	Verbs         []string `yaml:"verbs,omitempty,flow"`
	Kinds         []string `yaml:"kinds,omitempty,flow"`
	Namespaces    []string `yaml:"namespaces,omitempty,flow"`
	Reversibility string   `yaml:"reversibility,omitempty"`
	Approvals     int      `yaml:"approvals"`
}

// RemedyRules is a parsed, valid rules file: the rules in order and the risk model's name, ""
// for none. Build it with ParseRemedyRules.
type RemedyRules struct {
	Rules     investigatordomain.RiskRules
	RiskModel string
}

// RemedyRulesResult is what the operator reads back: the rules as stored, and what they replaced.
type RemedyRulesResult struct {
	OrgID uuid.UUID
	// Rules are the rules as stored, normalised (`deploy` is `deployment`).
	Rules []investigatordomain.RiskRule
	// RiskModel is the risk model's endpoint, or the zero config for none.
	RiskModel investigatordomain.ProviderConfig
	// Replaced is how many rules were there before.
	Replaced int
	// WrittenByLabel and WrittenAt are who applied the rules last, and when; zero when nobody has.
	WrittenByLabel string
	WrittenAt      time.Time
}

// ErrRiskModelNotFound means the file names a risk model that is not one of the org's endpoints.
var ErrRiskModelNotFound = errors.New("no model endpoint with that name in this org " +
	"(risk_model names one of the org's model endpoints by its name; omit it for none)")

// ParseRemedyRules reads a rules file strictly — one YAML document, no unknown key, `rules:`
// present — and validates every rule as the domain does (investigatordomain.NewRiskRules). Each
// problem is named by where it is: `rules/2/namespaces/0: …`.
func ParseRemedyRules(data []byte) (RemedyRules, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return RemedyRules{}, errors.New("the file is empty; it needs at least `rules: []` (which makes every Remedy need two approvals)")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f RemedyRulesFile
	if err := dec.Decode(&f); err != nil {
		return RemedyRules{}, fmt.Errorf("the file is not a rules file: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return RemedyRules{}, errors.New("the file holds more than one YAML document; a rules file is one")
	}
	if f.Rules == nil {
		return RemedyRules{}, errors.New("the file has no `rules:` key; write `rules: []` to make every Remedy need two approvals")
	}
	rules := make([]investigatordomain.RiskRule, 0, len(*f.Rules))
	for _, r := range *f.Rules {
		rules = append(rules, investigatordomain.RiskRule{Name: r.Name, Tool: r.Tool, Verbs: r.Verbs, Kinds: r.Kinds,
			Namespaces: r.Namespaces, Reversibility: investigatordomain.Reversibility(r.Reversibility), Approvals: r.Approvals})
	}
	rs, err := investigatordomain.NewRiskRules(rules)
	if err != nil {
		return RemedyRules{}, violationsOf(err)
	}
	out := RemedyRules{Rules: rs}
	if f.RiskModel != nil {
		out.RiskModel = strings.TrimSpace(*f.RiskModel)
		if out.RiskModel == "" {
			return RemedyRules{}, errors.New("risk_model: is blank; name one of the org's model endpoints, or omit it for none")
		}
	}
	return out, nil
}

// violationsOf is a domain refusal as the lines an operator reads: one per violation, each
// prefixed by the place in the file it is about.
func violationsOf(err error) error {
	e, ok := errs.As(err)
	if !ok || len(e.Violations) == 0 {
		return fmt.Errorf("the rules are not rules oto can apply: %s", messageOf(err))
	}
	lines := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		lines = append(lines, fmt.Sprintf("  %s: %s", v.Field, v.Message))
	}
	return fmt.Errorf("the rules are not rules oto can apply, and nothing was changed:\n%s", strings.Join(lines, "\n"))
}

// ApplyRemedyRules replaces the org's whole rule set and its risk model with the file's, in ONE
// transaction under the org's risk-rules advisory lock, stamped with `now` and RemedyRulesWriter.
// A file that does not parse or names an unknown risk model changes nothing.
func ApplyRemedyRules(ctx context.Context, pool *pgxpool.Pool, orgSlug string, data []byte, now time.Time) (RemedyRulesResult, error) {
	if pool == nil {
		return RemedyRulesResult{}, errors.New("a database pool is required")
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" {
		return RemedyRulesResult{}, errors.New("--org is required")
	}
	parsed, err := ParseRemedyRules(data)
	if err != nil {
		return RemedyRulesResult{}, err
	}
	at := now.UTC()
	var out RemedyRulesResult
	err = db.Tx(ctx, pool, func(ctx context.Context) error {
		q := db.FromContext(ctx, pool)
		scope, err := orgScopeBySlug(ctx, q, slug)
		if err != nil {
			return err
		}
		// ⭐ The lock 5ace8f3's replace took, for its reason: two applies at once cannot both
		// delete the old rules and collide on the primary key.
		key := db.AdvisoryKey(db.LockNamespaceInvestigations, "remedy-risk/"+scope.OrgID().String())
		if err := db.AdvisoryXactLock(ctx, q, key); err != nil {
			return fmt.Errorf("lock the org's risk rules: %w", err)
		}
		var model *uuid.UUID
		if parsed.RiskModel != "" {
			var id uuid.UUID
			err := q.QueryRow(ctx, selectModelProviderByNameSQL, scope.OrgID(), parsed.RiskModel).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("risk_model %q: %w", parsed.RiskModel, ErrRiskModelNotFound)
			}
			if err != nil {
				return fmt.Errorf("find the risk model: %w", err)
			}
			model = &id
		}
		replaced, err := writeRemedyRules(ctx, q, scope.OrgID(), parsed.Rules, model, nil, RemedyRulesWriter, at)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, supersedePendingRemedyRiskChangeSQL, scope.OrgID(), RemedyRulesWriter, at); err != nil {
			return fmt.Errorf("supersede the pending rule change: %w", err)
		}
		read, err := readRemedyRules(ctx, pool, scope)
		if err != nil {
			return err
		}
		read.Replaced = replaced
		out = read
		return nil
	})
	return out, err
}

// writeRemedyRules replaces the org's whole rule set and its risk model inside the caller's
// transaction, and returns how many rules it replaced. ⛔ The caller holds the org's risk-rules
// advisory lock, has validated `rules` with investigatordomain.NewRiskRules, and decides who is
// recorded as the writer: nil for the host's shell, a member for a confirmed change.
func writeRemedyRules(
	ctx context.Context, q db.Querier, orgID uuid.UUID, rules investigatordomain.RiskRules,
	model *uuid.UUID, writtenBy *uuid.UUID, label string, at time.Time,
) (int, error) {
	var replaced int
	if err := q.QueryRow(ctx, countRemedyRiskRulesSQL, orgID).Scan(&replaced); err != nil {
		return 0, fmt.Errorf("count the old rules: %w", err)
	}
	if _, err := q.Exec(ctx, upsertRemedyRiskSettingsSQL, orgID, model, writtenBy, label, at); err != nil {
		return 0, fmt.Errorf("store the risk model: %w", err)
	}
	if _, err := q.Exec(ctx, deleteRemedyRiskRulesSQL, orgID); err != nil {
		return 0, fmt.Errorf("remove the old rules: %w", err)
	}
	for i, r := range rules.Rules() {
		if _, err := q.Exec(ctx, insertRemedyRiskRuleSQL, orgID, r.Name, i, optText(r.Tool),
			r.Verbs, r.Kinds, r.Namespaces, optText(string(r.Reversibility)), r.Approvals, at); err != nil {
			return 0, fmt.Errorf("store rule %q: %w", r.Name, err)
		}
	}
	return replaced, nil
}

// ShowRemedyRules reads the org's rules and risk model as they stand.
func ShowRemedyRules(ctx context.Context, pool *pgxpool.Pool, orgSlug string) (RemedyRulesResult, error) {
	if pool == nil {
		return RemedyRulesResult{}, errors.New("a database pool is required")
	}
	slug := strings.ToLower(strings.TrimSpace(orgSlug))
	if slug == "" {
		return RemedyRulesResult{}, errors.New("--org is required")
	}
	scope, err := orgScopeBySlug(ctx, pool, slug)
	if err != nil {
		return RemedyRulesResult{}, err
	}
	return readRemedyRules(ctx, pool, scope)
}

// RemedyRulesYAML renders rules as the file that would apply them — what `show` prints, so an
// operator can start from what is there: `oto remedy-rules show --org acme > rules.yaml`.
func RemedyRulesYAML(r RemedyRulesResult) ([]byte, error) {
	f := struct {
		RiskModel *string          `yaml:"risk_model,omitempty"`
		Rules     []RemedyRuleYAML `yaml:"rules"`
	}{Rules: make([]RemedyRuleYAML, 0, len(r.Rules))}
	if r.RiskModel.ID != uuid.Nil {
		name := r.RiskModel.Name
		f.RiskModel = &name
	}
	for _, rule := range r.Rules {
		f.Rules = append(f.Rules, RemedyRuleYAML{Name: rule.Name, Tool: rule.Tool, Verbs: rule.Verbs, Kinds: rule.Kinds,
			Namespaces: rule.Namespaces, Reversibility: string(rule.Reversibility), Approvals: rule.Approvals})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readRemedyRules(ctx context.Context, q db.Querier, scope db.TenantScope) (RemedyRulesResult, error) {
	set, err := investigatorrepo.NewRemedyRiskRepository(q).RemedyRisk(ctx, scope)
	if err != nil {
		return RemedyRulesResult{}, fmt.Errorf("read the rules back: %w", err)
	}
	out := RemedyRulesResult{OrgID: scope.OrgID(), Rules: set.Rules.Rules(),
		WrittenByLabel: set.WrittenByLabel, WrittenAt: set.WrittenAt}
	if set.RiskModelProviderID != uuid.Nil {
		if out.RiskModel, err = investigatorrepo.NewProviderRepository(q).Get(ctx, scope, set.RiskModelProviderID); err != nil {
			return RemedyRulesResult{}, fmt.Errorf("read the risk model back: %w", err)
		}
	}
	return out, nil
}

func orgScopeBySlug(ctx context.Context, q db.Querier, slug string) (db.TenantScope, error) {
	var orgID uuid.UUID
	err := q.QueryRow(ctx, selectLiveOrgIDBySlugSQL, slug).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.TenantScope{}, ErrOrgNotFound
	}
	if err != nil {
		return db.TenantScope{}, fmt.Errorf("find org: %w", err)
	}
	return db.NewTenantScope(orgID)
}

func optText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const selectModelProviderByNameSQL = `
SELECT id FROM model_providers WHERE org_id = $1 AND name = $2`

const countRemedyRiskRulesSQL = `
SELECT count(*)::int FROM remedy_risk_rules WHERE org_id = $1`

// ⭐ written_by is NULL for the host's shell, which is not a member and says so in the label; for a
// confirmed change it is the member who confirmed it.
const upsertRemedyRiskSettingsSQL = `
INSERT INTO remedy_risk_settings (org_id, risk_model_provider_id, written_by, written_by_label, written_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id) DO UPDATE
   SET risk_model_provider_id = EXCLUDED.risk_model_provider_id,
       written_by = EXCLUDED.written_by, written_by_label = EXCLUDED.written_by_label,
       written_at = EXCLUDED.written_at`

// ⭐ A host-shell apply is the later word, so a proposal still waiting is overtaken by it and can no
// longer be confirmed over it (migration 00111).
const supersedePendingRemedyRiskChangeSQL = `
UPDATE remedy_risk_changes
   SET status = 'superseded', decided_by = NULL, decided_by_label = $2, decided_at = $3
 WHERE org_id = $1 AND status = 'pending'`

const deleteRemedyRiskRulesSQL = `
DELETE FROM remedy_risk_rules WHERE org_id = $1`

const insertRemedyRiskRuleSQL = `
INSERT INTO remedy_risk_rules
  (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
