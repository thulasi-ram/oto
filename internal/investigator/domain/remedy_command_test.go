package domain_test

// WHAT THE RULES READ, AND EVERY WAY A COMMAND WAS ONCE READ ON TRUST (ADR 0054 §3; judgment 2
// on the Remedy review — C1+C3, C2, C4, C5). Each case below is a bypass a reviewer wrote down:
// a command whose arguments the ToolServer would act on differently from what the rules read.
// Every one is unparseable — two approvals whatever the rules say — and the commands an
// operator actually lowers still parse, so the guard is not "refuse everything".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
)

var k8sKubectl = domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s", Tool: "kubectl"}

// restartInPayments is the Done-when rule: it names its Tool, so it may say one.
var restartInPayments = domain.RiskRule{Name: "restart-payments", Tool: "k8s__kubectl",
	Verbs: []string{"rollout restart"}, Namespaces: []string{"payments"}, Approvals: 1}

func argvArgs(member string, words ...string) string {
	b, _ := json.Marshal(map[string][]string{member: words})
	return string(b)
}

// TestEveryWordIsOnTheAllowlist — C2. A separator a ToolServer's split might honour joins two
// words, in a `command` string, a `command` array, `args`, and inside a value flag's value. Each
// is refused, never split.
func TestEveryWordIsOnTheAllowlist(t *testing.T) {
	separators := map[string]string{
		"NBSP": " ", "em space": " ", "ideographic space": "　", "vertical tab": "\v",
		"form feed": "\f", "NEL": "\u0085", "unit separator": "\u001f",
	}
	for name, sep := range separators {
		cases := map[string]string{
			"string":            cmdArgs("kubectl rollout restart" + sep + "deployment/api -n payments"),
			"command array":     argvArgs("command", "kubectl", "rollout", "restart"+sep+"deployment/api", "-n", "payments"),
			"args array":        argvArgs("args", "rollout", "restart", "deployment/api", "-n", "payments"+sep+"--context=prod"),
			"a timeout's value": cmdArgs("kubectl rollout restart deployment/api -n payments --timeout 30s" + sep + "--context=prod"),
		}
		for form, args := range cases {
			t.Run(name+"/"+form, func(t *testing.T) {
				c := domain.ParseRemedyCommand(k8sKubectl, args)
				if c.Parsed() {
					t.Fatalf("%q read as %+v", args, c)
				}
				if v := mustRules(t, restartInPayments).Evaluate(c); v.Approvals != 2 || v.Basis != domain.BasisUnparseable {
					t.Fatalf("verdict %+v", v)
				}
			})
		}
	}

	// Unicode lookalikes: a Cyrillic а in the namespace, a fullwidth hyphen on the flag.
	for _, args := range []string{
		cmdArgs("kubectl rollout restart deployment/api -n pаyments"),
		cmdArgs("kubectl rollout restart deployment/api －n payments"),
		cmdArgs("kubectl rollout restart deployment/api -n payments​"),
	} {
		if c := domain.ParseRemedyCommand(k8sKubectl, args); c.Parsed() || !strings.Contains(c.Unparseable, "U+") {
			t.Fatalf("%q read as %+v", args, c)
		}
	}

	// ⭐ What an operator actually lowers still parses: the allowlist is not "refuse everything".
	for _, line := range []string{
		"kubectl rollout restart deployment/api -n payments",
		"kubectl scale deploy/api --replicas=3 -n payments",
		"kubectl delete secret db-creds -n payments",
		"kubectl delete pods -l app=api,tier=web -n payments",
		"kubectl set image deployment/api api=reg.io/a/b:1.2@sha256:ab -n payments",
		"kubectl rollout restart deployment/api -n payments --request-timeout=30s",
		"kubectl scale deploy/api --replicas=0 -n payments",
		"kubectl delete pod api-1 -n payments --dry-run=server",
	} {
		if c := domain.ParseRemedyCommand(k8sKubectl, cmdArgs(line)); !c.Parsed() {
			t.Fatalf("%q: %s", line, c.Unparseable)
		}
	}

	// -n is held to a namespace name.
	for _, line := range []string{"kubectl delete pod x -n Payments", "kubectl delete pod x -n a,b", "kubectl delete pod x --namespace=a.b"} {
		if c := domain.ParseRemedyCommand(k8sKubectl, cmdArgs(line)); c.Parsed() {
			t.Fatalf("%q read as %+v", line, c)
		}
	}
}

// TestTheArgumentsAreAClosedShapeReadTokenByToken — C1+C3. A duplicate key, a case variant, a
// non-ASCII key, a member beside the command, or a structured member the rules do not read: a
// decoder (or a ToolServer) could read each differently from the rules, so each is unparseable.
func TestTheArgumentsAreAClosedShapeReadTokenByToken(t *testing.T) {
	const restart = `"kubectl rollout restart deployment/api -n payments"`
	cases := map[string]struct{ args, says string }{
		"two commands":           {`{"command":` + restart + `,"command":"kubectl delete ns payments"}`, "twice"},
		"a case variant":         {`{"command":` + restart + `,"COMMAND":"kubectl delete ns payments"}`, "twice"},
		"a case variant alone":   {`{"COMMAND":` + restart + `}`, "cannot read"},
		"a nested duplicate":     {`{"name":"api","kind":"secret","x":{"a":"1","a":"2"}}`, "twice"},
		"two namespaces":         {`{"verb":"delete","kind":"secret","namespace":"staging","namespace":"payments"}`, "twice"},
		"a Kelvin-sign key":      {`{"verb":"delete","Kind":"secret","namespace":"payments"}`, "the rules do not read"},
		"an escaped duplicate":   {`{"command":` + restart + `,"command":"kubectl delete ns payments"}`, "twice"},
		"a context beside it":    {`{"command":` + restart + `,"context":"prod"}`, "beside the command"},
		"a kubeconfig beside it": {`{"args":["rollout","restart","deployment/api","-n","payments"],"kubeconfig":"/root/.kube/config"}`, "beside the command"},
		"a manifest":             {`{"verb":"apply","kind":"deployment","namespace":"payments","manifest":"kind: Secret"}`, "cannot read"},
		"a patch":                {`{"verb":"patch","kind":"deployment","namespace":"payments","patch":"{}"}`, "cannot read"},
		"a nested object":        {`{"verb":"delete","kind":{"name":"secret"}}`, "not a string"},
		"a number":               {`{"verb":"scale","kind":"deployment","name":"api","namespace":"payments","replicas":0}`, "cannot read"},
		"a null":                 {`{"verb":"delete","namespace":null}`, "not a string"},
		"a bool verb":            {`{"verb":true}`, "not a string"},
		"a non-ASCII value":      {`{"verb":"delete","namespace":"pаyments"}`, "U+0430"},
		"two kinds":              {`{"verb":"delete","kind":"deployment","resourceType":"secret","namespace":"payments"}`, "more than one kind"},
		"a bad name":             {`{"verb":"delete","kind":"secret","name":"Db Creds","namespace":"payments"}`, "not an object name"},
		"two documents":          {`{"command":` + restart + `} {"command":"kubectl delete ns payments"}`, "not one JSON object"},
		"a command string array": {`{"command":"x","args":"y"}`, "both"},
		"args as a string":       {`{"args":"rollout restart deployment/api -n payments"}`, "not an array"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := domain.ParseRemedyCommand(k8sKubectl, tc.args)
			if c.Parsed() || !strings.Contains(c.Unparseable, tc.says) {
				t.Fatalf("Unparseable = %q, want it to say %q (read %+v)", c.Unparseable, tc.says, c)
			}
			rules := mustRules(t, restartInPayments, domain.RiskRule{Name: "anything-kubectl", Tool: "k8s__kubectl", Approvals: 1})
			if v := rules.Evaluate(c); v.Approvals != 2 || v.Basis != domain.BasisUnparseable {
				t.Fatalf("verdict %+v", v)
			}
		})
	}
}

// TestAStructuredVerbIsNeverEvidence — C1+C3: the review's scenario. A model writes
// `{"verb":"rollout restart"}` to a Tool that deletes. The rule as the reviewer wrote it is
// refused (it lowers to one and names no Tool); with its Tool named it still never matches,
// because a structured verb is never reversible.
func TestAStructuredVerbIsNeverEvidence(t *testing.T) {
	deleteTool := domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s", Tool: "kubectl_delete"}
	args := `{"verb":"rollout restart","resourceType":"secret","namespace":"payments","name":"db-creds"}`
	reviewers := domain.RiskRule{Name: "reversible-payments", Namespaces: []string{"payments"},
		Reversibility: domain.Reversible, Approvals: 1}
	if _, err := domain.NewRiskRules([]domain.RiskRule{reviewers}); err == nil {
		t.Fatalf("a rule lowering to one with no Tool was accepted")
	}
	withTool := reviewers
	withTool.Tool = "k8s__kubectl_delete"
	c := domain.ParseRemedyCommand(deleteTool, args)
	if !c.Parsed() || c.Verb != "rollout restart" || c.Kind != "secret" || c.Reversible() {
		t.Fatalf("read %+v reversible=%v", c, c.Reversible())
	}
	if v := mustRules(t, withTool).Evaluate(c); v.Approvals != 2 || v.Basis != domain.BasisNoRule {
		t.Fatalf("a model-written verb lowered the Remedy: %+v", v)
	}

	// ⭐ Done-when kept: the command line in payments, under a rule naming its Tool, is one.
	v := mustRules(t, restartInPayments).Evaluate(domain.ParseRemedyCommand(k8sKubectl,
		`{"command":"kubectl rollout restart deployment/api -n payments"}`))
	if v.Approvals != 1 || v.Rule != "restart-payments" {
		t.Fatalf("the Done-when restart: %+v", v)
	}
}

// TestAFlagThatPointsElsewhereIsUnparseable — C4: `--context` and `--cluster` point the same
// command at another cluster, and an unknown flag is unparseable in every form.
func TestAFlagThatPointsElsewhereIsUnparseable(t *testing.T) {
	for _, line := range []string{
		"kubectl rollout restart deployment/api -n payments --context=prod",
		"kubectl rollout restart deployment/api -n payments --context prod",
		"kubectl rollout restart deployment/api -n payments --cluster x",
		"kubectl rollout restart deployment/api -n payments --profile=cpu",
		"kubectl rollout restart deployment/api -n payments --tls-server-name=x",
		"kubectl rollout restart deployment/api -n payments --as-user-extra=a=b",
		"kubectl rollout restart deployment/api -n payments --cache-dir=/tmp",
		"kubectl rollout restart deployment/api -n payments --some-new-flag=x",
	} {
		if c := domain.ParseRemedyCommand(k8sKubectl, cmdArgs(line)); c.Parsed() {
			t.Fatalf("%q read as %+v", line, c)
		}
	}
}

// TestAKindOtoCannotFoldIsUnparseable — C5: a kind outside the table was compared as written,
// so a rule saying TWO about its singular silently missed its plural.
func TestAKindOtoCannotFoldIsUnparseable(t *testing.T) {
	rules := mustRules(t,
		domain.RiskRule{Name: "infra-one", Tool: "k8s__kubectl", Namespaces: []string{"infra"}, Approvals: 1},
		domain.RiskRule{Name: "webhooks-need-two", Kinds: []string{"validatingwebhookconfiguration"}, Approvals: 2})
	v := rules.Evaluate(domain.ParseRemedyCommand(k8sKubectl, cmdArgs("kubectl delete validatingwebhookconfigurations/x -n infra")))
	if v.Approvals != 2 || v.Rule != "webhooks-need-two" {
		t.Fatalf("a webhook delete: %+v", v)
	}
	c := domain.ParseRemedyCommand(k8sKubectl, cmdArgs("kubectl delete certificates x -n infra"))
	if c.Parsed() || !strings.Contains(c.Unparseable, "other spellings") {
		t.Fatalf("a custom kind read as %+v", c)
	}
	if v := rules.Evaluate(c); v.Approvals != 2 || v.Basis != domain.BasisUnparseable {
		t.Fatalf("a custom kind: %+v", v)
	}
	if _, err := domain.NewRiskRules([]domain.RiskRule{{Name: "x", Kinds: []string{"certificate"}, Approvals: 2}}); err == nil {
		t.Fatalf("a rule naming a kind oto cannot fold was accepted")
	}
	for _, k := range []string{"deploy", "deployments.apps", "Deployments"} {
		if got := domain.CanonicalKind(k); got != "deployment" {
			t.Fatalf("%s folds to %q", k, got)
		}
	}
}
