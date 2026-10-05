package domain

import (
	"encoding/json"
	"path"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestAToolServerNameKeepsTheQualifiedSeparatorUnambiguous(t *testing.T) {
	for _, ok := range []string{"k8s", "victoria-logs", "a", "vm1"} {
		if _, err := NewToolServerName(ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "K8s", "k8s_prod", "-k8s", "k8s-", "1k8s", "oto", strings.Repeat("a", 25), "k8s.prod"} {
		if _, err := NewToolServerName(bad); !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAQualifiedNameSplitsAtTheFirstSeparator(t *testing.T) {
	for name, want := range map[string][2]string{
		"k8s__pods_list":       {"k8s", "pods_list"},
		"k8s__pods__by__label": {"k8s", "pods__by__label"},
		"victoria-logs__query": {"victoria-logs", "query"},
	} {
		server, tool, ok := SplitQualifiedToolName(name)
		if !ok || server != want[0] || tool != want[1] {
			t.Fatalf("%s split as %q %q %v", name, server, tool, ok)
		}
	}
	// ⭐ oto's built-in Tools are never qualified, and neither is anything malformed.
	for _, name := range []string{"oto_case_timeline", "__x", "k8s__", "K8S__x", "k8s_x"} {
		if _, _, ok := SplitQualifiedToolName(name); ok {
			t.Fatalf("%q read as qualified", name)
		}
	}
	a, err := NewAllowlist([]string{"oto_case_timeline", "k8s__pods_list", "vm__query", "k8s__events"})
	if err != nil {
		t.Fatal(err)
	}
	got := a.ToolServerTools()
	if len(got) != 2 || strings.Join(got["k8s"], ",") != "events,pods_list" || got["vm"][0] != "query" {
		t.Fatalf("grouped %v", got)
	}
}

func TestAToolServerURLCarriesNoSecretAndATokenOnlyGoesOverTLS(t *testing.T) {
	limits, _ := NewCallLimits(0, 0)
	u, err := NormalizeToolServerURL(" HTTPS://K8s-MCP.tools.svc:8443/mcp/ ")
	if err != nil || u != "https://k8s-mcp.tools.svc:8443/mcp/" {
		t.Fatalf("normalised %q, %v (the path is the server's, trailing slash and all)", u, err)
	}
	for _, bad := range []string{"https://u:p@k8s/mcp", "https://k8s/mcp?api_key=x", "https://k8s/mcp#x", "ftp://k8s", "k8s/mcp", "https:///mcp"} {
		if _, err := NormalizeToolServerURL(bad); errs.CodeOf(err) != "tool_server_url_invalid" {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if _, err := NewToolServerDraft("k8s", "http://k8s-mcp:8080/mcp", "", "read", "tok", limits); errs.CodeOf(err) != "tool_server_token_needs_https" {
		t.Fatalf("a token over http: %v", err)
	}
	if _, err := NewToolServerDraft("k8s", "http://k8s-mcp:8080/mcp", "sse", "read", "", limits); err != nil {
		t.Fatalf("a token-less in-cluster ToolServer: %v", err)
	}
	if _, err := NewToolServerDraft("k8s", "https://k8s", "stdio", "read", "", limits); errs.CodeOf(err) != "tool_server_transport_invalid" {
		t.Fatalf("stdio accepted: %v", err)
	}
	if _, err := NewToolServerDraft("k8s", "https://k8s", "", "", "", limits); errs.CodeOf(err) != "tool_server_access_invalid" {
		t.Fatalf("an undeclared access accepted: %v", err)
	}
}

func TestCallLimitsDefaultAndBound(t *testing.T) {
	l, err := NewCallLimits(0, 0)
	if err != nil || l.TimeoutSeconds() != DefaultCallTimeoutSeconds || l.MaxResultBytes != DefaultResultBytes {
		t.Fatalf("defaults = %+v, %v", l, err)
	}
	if _, err := NewCallLimits(121, 512); len(errs.ViolationsOf(err)) != 2 {
		t.Fatalf("out of range: %v", err)
	}
}

func TestADiscoveredToolSaysWhyItCannotBeHeld(t *testing.T) {
	hint := true
	ok, err := NewDiscoveredTool("pods_list", "  List pods. ", json.RawMessage(`{"type":"object"}`), &hint)
	if err != nil || ok.Description != "List pods." {
		t.Fatal(err)
	}
	if q, why := ok.Usable("k8s"); q != "k8s__pods_list" || why != "" {
		t.Fatalf("%q %q", q, why)
	}
	dotted, _ := NewDiscoveredTool("logs.query", "", json.RawMessage(`{"type":"object"}`), nil)
	if q, why := dotted.Usable("k8s"); q != "" || !strings.Contains(why, "qualified name") {
		t.Fatalf("%q %q", q, why)
	}
	long, _ := NewDiscoveredTool(strings.Repeat("x", 62), "", json.RawMessage(`{}`), nil)
	if q, _ := long.Usable("k8s"); q != "" {
		t.Fatal("a qualified name past 64 was usable")
	}
	noSchema, _ := NewDiscoveredTool("pods_get", "", json.RawMessage(`"string"`), nil)
	if q, why := noSchema.Usable("k8s"); q != "" || !strings.Contains(why, "schema") || noSchema.InputSchema != nil {
		t.Fatalf("%q %q", q, why)
	}
	if _, err := NewDiscoveredTool("", "", nil, nil); err == nil {
		t.Fatal("a nameless Tool was recorded")
	}
}

func globs(patterns ...string) func(string) bool {
	return func(name string) bool {
		for _, p := range patterns {
			if ok, _ := path.Match(p, name); ok {
				return true
			}
		}
		return false
	}
}

func TestARedactorReplacesMatchedValuesInJSONAndProse(t *testing.T) {
	r := NewResultRedactor(globs("*password*", "*token*", "authorization"), "[redacted]")

	out, n := r.Redact(`{"user":"ada","db_password":"hunter2","nested":{"api_token":{"v":1}},"items":[{"authorization":"Bearer x"}],"msg":"login token=abc user=ada","n":12345678901234567890}`)
	if n != 4 {
		t.Fatalf("redacted %d: %s", n, out)
	}
	for _, leak := range []string{"hunter2", `"v":1`, "Bearer x", "abc"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q survived: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"user":"ada"`) || !strings.Contains(out, "user=ada") || !strings.Contains(out, "12345678901234567890") {
		t.Fatalf("redaction took too much, or lost a number's precision: %s", out)
	}

	out, n = r.Redact("time=09:00 level=info db_password: s3cret\n\"api_token\": \"tok-1\" next=ok")
	if n != 2 || strings.Contains(out, "s3cret") || strings.Contains(out, "tok-1") || !strings.Contains(out, "next=ok") ||
		!strings.Contains(out, `"api_token": "[redacted]"`) {
		t.Fatalf("prose: %d %q", n, out)
	}

	// Nothing to redact is the text as it came, byte for byte.
	in := `{ "b": 1,  "a": 2 }`
	if out, n := r.Redact(in); out != in || n != 0 {
		t.Fatalf("an untouched result was rewritten: %q", out)
	}
	// The zero value redacts nothing and never fails.
	if out, n := (ResultRedactor{}).Redact("password=x"); out != "password=x" || n != 0 {
		t.Fatal("the zero redactor changed something")
	}
	// Redacting twice changes nothing more.
	once, _ := r.Redact("password=x")
	if twice, n := r.Redact(once); twice != once || n != 0 {
		t.Fatalf("re-redacted %q", twice)
	}
}

// TestAToolItsServerMarksWritableIsNeverHeldWhileInvestigating — review D1: the hint
// never promotes a Tool, but `readOnlyHint: false` refuses one even on a read server;
// no annotations or `true` leave the declaration as the guard.
func TestAToolItsServerMarksWritableIsNeverHeldWhileInvestigating(t *testing.T) {
	no, yes := false, true
	for name, tc := range map[string]struct {
		hint *bool
		held bool
	}{
		"says it writes": {hint: &no},
		"says it reads":  {hint: &yes, held: true},
		"says nothing":   {hint: nil, held: true},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := NewDiscoveredTool("pods_delete", "", json.RawMessage(`{"type":"object"}`), tc.hint)
			if err != nil {
				t.Fatal(err)
			}
			q, why := d.HeldWhileInvestigating("k8s")
			if (why == "") != tc.held || (tc.held && q != "k8s__pods_delete") {
				t.Fatalf("held %q, why %q; want held=%v", q, why, tc.held)
			}
			if !tc.held && !strings.Contains(why, "readOnlyHint") {
				t.Fatalf("the refusal does not say why: %q", why)
			}
			// ⛔ Usable — what a Remedy binds a write Tool with — never reads the hint.
			if q, why := d.Usable("k8s"); q == "" || why != "" {
				t.Fatalf("Usable refused on the hint: %q", why)
			}
		})
	}
}
