package rulematch_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/rulematch"
)

// The vmalert docs' own example of its default generatorURL, and the vmui form
// its docs recommend through `-external.alert.source`.
const (
	vmalertDefaultLink = "http://vmalert:8880/vmalert/alert?group_id=1036955090143761274&alert_id=1074584496268461589"
	vmuiLink           = "http://vmui:8428/vmui/#/?g0.expr=up%20%3D%3D%200"
)

// TestGeneratorURLForms is git-bug 766709c's table, beside the Prometheus forms
// it must leave exactly as they were.
func TestGeneratorURLForms(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expr     string
		variant  rulematch.Variant
		external string
	}{
		{
			name:     "prometheus graph link",
			raw:      "http://prom:9090/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
			expr:     "up == 0",
			variant:  rulematch.VariantGraph,
			external: "http://prom:9090",
		},
		{
			name:     "prometheus behind a routing prefix",
			raw:      "https://ops.example.com/prometheus/graph?g0.expr=rate%28x%5B5m%5D%29+%3E+1",
			expr:     "rate(x[5m]) > 1",
			variant:  rulematch.VariantGraph,
			external: "https://ops.example.com/prometheus",
		},
		{
			name:     "plain fragment, no router",
			raw:      "http://prom:9090/#g0.expr=up%20%3D%3D%200",
			expr:     "up == 0",
			variant:  rulematch.VariantFragment,
			external: "http://prom:9090",
		},
		{
			name:     "vmui hash-router form",
			raw:      vmuiLink,
			expr:     "up == 0",
			variant:  rulematch.VariantFragment,
			external: "http://vmui:8428",
		},
		{
			// queryEscape is url.QueryEscape: a space is `+` and a `+` is %2B.
			// Read through the DECODED fragment, %2B came back `+` and was then
			// parsed as a space — a different expression.
			name:     "vmui form keeps a plus",
			raw:      "http://vmui:8428/vmui/#/?g0.expr=a+%2B+b+%3E+1&g0.range_input=1h",
			expr:     "a + b > 1",
			variant:  rulematch.VariantFragment,
			external: "http://vmui:8428",
		},
		{
			// A `?` inside the expression of a router-less fragment is the
			// expression's, not a route's.
			name:     "a question mark after the key is not a route",
			raw:      "http://prom:9090/#g0.expr=up%7Bjob%3D%22a?b%22%7D",
			expr:     `up{job="a?b"}`,
			variant:  rulematch.VariantFragment,
			external: "http://prom:9090",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := rulematch.ParseGeneratorURL(c.raw)
			if err != nil {
				t.Fatalf("ParseGeneratorURL: %v", err)
			}
			if got.Expr != c.expr {
				t.Errorf("Expr = %q, want %q", got.Expr, c.expr)
			}
			if got.Variant != c.variant {
				t.Errorf("Variant = %q, want %q", got.Variant, c.variant)
			}
			if got.ExternalURL != c.external {
				t.Errorf("ExternalURL = %q, want %q", got.ExternalURL, c.external)
			}
		})
	}
}

// TestVmalertDefaultLinkStillCarriesNoExpr pins the line between the parser and
// the helper: success from ParseGeneratorURL means an expression was recovered,
// and vmalert's default link has none.
func TestVmalertDefaultLinkStillCarriesNoExpr(t *testing.T) {
	_, err := rulematch.ParseGeneratorURL(vmalertDefaultLink)
	if got := errs.CodeOf(err); got != rulematch.CodeNoExpr {
		t.Fatalf("code = %q, want %q", got, rulematch.CodeNoExpr)
	}
}

func TestVmalertRoot(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		root string
		ok   bool
	}{
		{"the vmalert docs' default link", vmalertDefaultLink, "http://vmalert:8880", true},
		{"behind a routing prefix", "https://ops.example.com/vm/vmalert/alert?group_id=1&alert_id=2", "https://ops.example.com/vm", true},
		{"no alert_id", "http://vmalert:8880/vmalert/alert?group_id=1", "", false},
		{"no group_id", "http://vmalert:8880/vmalert/alert?alert_id=2", "", false},
		{"a prometheus graph link", "http://prom:9090/graph?g0.expr=up", "", false},
		{"the vmui form", vmuiLink, "", false},
		{"not absolute", "/vmalert/alert?group_id=1&alert_id=2", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, ok := rulematch.VmalertRoot(c.raw)
			if root != c.root || ok != c.ok {
				t.Errorf("VmalertRoot = (%q, %v), want (%q, %v)", root, ok, c.root, c.ok)
			}
		})
	}
}
