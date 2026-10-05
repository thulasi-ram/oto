package app

import (
	"strings"
	"testing"

	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
)

// `oto remedy-rules apply` reads its file strictly (owner ruling 2026-10-05 on git-bug
// eb4f21b): a malformed file is refused, naming the problem, before any transaction opens — so
// it changes nothing. No Docker: ParseRemedyRules is pure.

const goodRules = `
risk_model: risk
rules:
  - name: restart-payments
    verbs: [rollout restart]
    kinds: [deploy]
    namespaces: [payments]
    approvals: 1
  - name: secrets-need-two
    tool: k8s-write__kubectl
    kinds: [secrets]
    reversibility: irreversible
    approvals: 2
`

func TestARulesFileIsReadInOrderAndNormalised(t *testing.T) {
	got, err := ParseRemedyRules([]byte(goodRules))
	if err != nil {
		t.Fatal(err)
	}
	rules := got.Rules.Rules()
	if len(rules) != 2 || rules[0].Name != "restart-payments" || rules[0].Kinds[0] != "deployment" ||
		rules[1].Kinds[0] != "secret" || rules[1].Reversibility != investigatordomain.Irreversible || got.RiskModel != "risk" {
		t.Fatalf("parsed %+v", got)
	}

	// `rules: []` is legal: every Remedy needs two. No risk model is none.
	empty, err := ParseRemedyRules([]byte("rules: []\n"))
	if err != nil || len(empty.Rules.Rules()) != 0 || empty.RiskModel != "" {
		t.Fatalf("an empty list = %+v, %v", empty, err)
	}

	// What `show` prints applies back to the same rules.
	yml, err := RemedyRulesYAML(RemedyRulesResult{Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseRemedyRules(yml)
	if err != nil {
		t.Fatalf("show's output does not apply: %v\n%s", err, yml)
	}
	if len(again.Rules.Rules()) != 2 || again.Rules.Rules()[0].Verbs[0] != "rollout restart" {
		t.Fatalf("show's output round-tripped to %+v\n%s", again.Rules.Rules(), yml)
	}
}

func TestAMalformedRulesFileIsRefusedNamingTheProblem(t *testing.T) {
	for name, tc := range map[string]struct{ file, says string }{
		"empty":           {"", "empty"},
		"not yaml":        {"rules: [\n", "not a rules file"},
		"no rules key":    {"risk_model: risk\n", "no `rules:` key"},
		"a misspelt key":  {"rules:\n  - name: x\n    namespace: [payments]\n    approvals: 1\n", "namespace"},
		"an unknown top":  {"rulez: []\nrules: []\n", "rulez"},
		"two documents":   {"rules: []\n---\nrules: []\n", "more than one YAML document"},
		"a string tier":   {"rules:\n  - name: x\n    verbs: [delete]\n    approvals: one\n", "not a rules file"},
		"no condition":    {"rules:\n  - name: everything\n    approvals: 1\n", "rules/0"},
		"three approvals": {"rules:\n  - name: x\n    verbs: [delete]\n    approvals: 3\n", "rules/0/approvals"},
		"a capital name":  {"rules:\n  - name: Restart\n    verbs: [delete]\n    approvals: 1\n", "rules/0/name"},
		"a pipe verb":     {"rules:\n  - name: x\n    verbs: [get | sh]\n    approvals: 1\n", "rules/0/verbs/0"},
		"a wildcard ns":   {"rules:\n  - name: x\n    namespaces: ['*']\n    approvals: 1\n", "rules/0/namespaces/0"},
		"a bare tool":     {"rules:\n  - name: x\n    tool: kubectl\n    approvals: 1\n", "rules/0/tool"},
		"twice named":     {"rules:\n  - name: x\n    verbs: [delete]\n    approvals: 2\n  - name: x\n    verbs: [scale]\n    approvals: 1\n", "rules/1/name"},
		"a blank model":   {"risk_model: ''\nrules: []\n", "risk_model"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRemedyRules([]byte(tc.file))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("%q: err = %v, want it to say %q", tc.file, err, tc.says)
			}
		})
	}
}
