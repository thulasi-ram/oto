package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// `oto remedy-rules apply` / `show` (ADR 0054 §3; owner ruling 2026-10-05 on git-bug eb4f21b)
// are the ONLY writer and the shell's reader of an org's Remedy risk rules. What these tests
// pin: a file applies whole, in order, with its risk model and the CLI as its writer; a
// malformed file, an unknown risk model and an unknown org each exit non-zero naming the
// problem and change nothing; `rules: []` makes every Remedy need two again; and `show` prints
// the YAML that would apply what stands.

type rulesWorld struct {
	h     *harness.H
	scope db.TenantScope
	model investigatordomain.ProviderConfig
	dir   string
}

func newRulesWorld(t *testing.T) rulesWorld {
	t.Helper()
	h := harness.New(t)
	seedUser(t, h, "acme", "operator@example.test", "correct-horse-battery-staple")
	var orgID uuid.UUID
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT id FROM orgs WHERE slug = 'acme'`).Scan(&orgID))
	scope, err := db.NewTenantScope(orgID)
	require.NoError(t, err)
	model, err := investigatorrepo.NewProviderRepository(h.Pool).Insert(h.Ctx, scope,
		investigatordomain.ProviderDraft{Name: "risk", BaseURL: "http://risk.models.test/v1", Model: "risk-m"},
		uuid.Nil, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return rulesWorld{h: h, scope: scope, model: model, dir: t.TempDir()}
}

func (w rulesWorld) file(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(w.dir, uuid.NewString()+".yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func (w rulesWorld) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return capture(t, func() error { return remedyRulesCommand(w.h.Ctx, w.h.DSN, args) })
}

func (w rulesWorld) stored(t *testing.T) investigatordomain.RemedyRiskSettings {
	t.Helper()
	set, err := investigatorrepo.NewRemedyRiskRepository(w.h.Pool).RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	return set
}

const twoRules = `
risk_model: risk
rules:
  - name: restart-payments
    verbs: [rollout restart]
    kinds: [deploy]
    namespaces: [payments]
    approvals: 1
  - name: secrets-need-two
    tool: k8s-write__kubectl
    kinds: [secret]
    reversibility: irreversible
    approvals: 2
`

func TestARulesFileAppliesWholeFromTheHostShell(t *testing.T) {
	w := newRulesWorld(t)
	require.Empty(t, w.stored(t).Rules.Rules(), "oto ships no rule")

	out, err := w.run(t, "apply", "--org", "acme", "-f", w.file(t, twoRules))
	require.NoError(t, err)
	require.Contains(t, out, "2 (1 say one approval, 1 say two); replaced 0")
	require.Contains(t, out, "risk_model risk")

	set := w.stored(t)
	rules := set.Rules.Rules()
	require.Len(t, rules, 2)
	require.Equal(t, "restart-payments", rules[0].Name)
	require.Equal(t, []string{"deployment"}, rules[0].Kinds)
	require.Equal(t, w.model.ID, set.RiskModelProviderID)
	require.Equal(t, "oto remedy-rules apply", set.WrittenByLabel)
	var writer *uuid.UUID
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT written_by FROM remedy_risk_settings WHERE org_id = $1`, w.scope.OrgID()).Scan(&writer))
	require.Nil(t, writer, "the host's shell is not a member, and no member is recorded as the writer")

	// `show` prints the YAML that would apply what stands — and it applies.
	shown, err := w.run(t, "show", "--org", "acme")
	require.NoError(t, err)
	require.Contains(t, shown, "risk_model: risk")
	require.Contains(t, shown, "restart-payments")
	out, err = w.run(t, "apply", "--org", "acme", "-f", w.file(t, shown))
	require.NoError(t, err)
	require.Contains(t, out, "replaced 2")
	require.Len(t, w.stored(t).Rules.Rules(), 2)

	// `rules: []` with no model: every Remedy needs two again.
	out, err = w.run(t, "apply", "--org", "acme", "-f", w.file(t, "rules: []\n"))
	require.NoError(t, err)
	require.Contains(t, out, "every Remedy needs two approvals")
	set = w.stored(t)
	require.Empty(t, set.Rules.Rules())
	require.Equal(t, uuid.Nil, set.RiskModelProviderID)
}

func TestAMalformedRulesFileChangesNothingAndNamesTheProblem(t *testing.T) {
	w := newRulesWorld(t)
	_, err := w.run(t, "apply", "--org", "acme", "-f", w.file(t, twoRules))
	require.NoError(t, err)
	before := w.stored(t)

	for name, tc := range map[string]struct{ args []string }{
		"a misspelt key":   {[]string{"apply", "--org", "acme", "-f", w.file(t, "rules:\n  - name: x\n    namespace: [payments]\n    approvals: 1\n")}},
		"no condition":     {[]string{"apply", "--org", "acme", "-f", w.file(t, "rules:\n  - name: everything\n    approvals: 1\n")}},
		"not yaml":         {[]string{"apply", "--org", "acme", "-f", w.file(t, "rules: [\n")}},
		"an unknown model": {[]string{"apply", "--org", "acme", "-f", w.file(t, "risk_model: nobody\nrules: []\n")}},
		"an unknown org":   {[]string{"apply", "--org", "globex", "-f", w.file(t, "rules: []\n")}},
		"no file":          {[]string{"apply", "--org", "acme"}},
		"a missing file":   {[]string{"apply", "--org", "acme", "-f", filepath.Join(w.dir, "absent.yaml")}},
		"no org":           {[]string{"apply", "-f", w.file(t, "rules: []\n")}},
		"an unknown verb":  {[]string{"replace", "--org", "acme"}},
	} {
		_, err := w.run(t, tc.args...)
		require.Error(t, err, name)
		after := w.stored(t)
		require.Equal(t, before.Rules.Rules(), after.Rules.Rules(), "%s changed the rules", name)
		require.Equal(t, before.RiskModelProviderID, after.RiskModelProviderID, "%s changed the risk model", name)
		require.Equal(t, before.WrittenAt, after.WrittenAt, "%s rewrote the settings row", name)
	}

	_, err = w.run(t, "apply", "--org", "acme", "-f", w.file(t, "risk_model: nobody\nrules: []\n"))
	require.ErrorContains(t, err, `risk_model "nobody"`)
	_, err = w.run(t, "apply", "--org", "globex", "-f", w.file(t, "rules: []\n"))
	require.ErrorContains(t, err, `no org with slug "globex"`)
	_, err = w.run(t, "apply", "--org", "acme", "-f", w.file(t, "rules:\n  - name: everything\n    approvals: 1\n"))
	require.ErrorContains(t, err, "rules/0")
}
