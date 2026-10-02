package service

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/mappings"
)

// ADR 0055 §4, mechanised (git-bug 2b5eecc): "no mapping in oto's catalog turns a
// fact into a resolve, close or status change; a catalog review refuses one." The
// review is CheckCatalogMapping, and this file is where it runs over the catalog
// that ships — and over files built to fail it, so a check that quietly stopped
// checking would fail here too.

// TestTheCatalogShipsNoCommand runs the catalog review over every file in
// `mappings/`: every fact renders, each declared command field is present on every
// one, none of them renders a forbidden value, no forbidden value is spelled, a
// connection would accept the mapping, and every key is a secret reference.
func TestTheCatalogShipsNoCommand(t *testing.T) {
	catalog, err := LoadCatalog(mappings.FS)
	if err != nil {
		t.Fatalf("the embedded catalog does not load: %v", err)
	}
	if len(catalog) < 2 {
		t.Fatalf("the catalog holds %d mappings; it ships at least the two starter mappings", len(catalog))
	}
	for _, entry := range catalog {
		t.Run(entry.ID, func(t *testing.T) {
			for _, p := range CheckCatalogMapping(entry) {
				t.Error(p)
			}
		})
	}
}

// TestTheStarterMappingsDeclareTheirVendorsCommands pins the two declarations the
// ticket names, so a later edit cannot weaken one into passing: PagerDuty's
// `event_action` ∉ {resolve, acknowledge} and incident.io's `status` ≠ resolved.
func TestTheStarterMappingsDeclareTheirVendorsCommands(t *testing.T) {
	catalog, err := LoadCatalog(mappings.FS)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]domain.CatalogCommand{
		"pagerduty":   {Field: "event_action", Forbidden: []string{"resolve", "acknowledge"}},
		"incident-io": {Field: "status", Forbidden: []string{"resolved"}},
	}
	byID := map[string]domain.CatalogMapping{}
	for _, e := range catalog {
		byID[e.ID] = e
	}
	for id, cmd := range want {
		e, ok := byID[id]
		if !ok {
			t.Errorf("the catalog has no %s mapping", id)
			continue
		}
		found := false
		for _, got := range e.Commands {
			if got.Field != cmd.Field {
				continue
			}
			found = true
			for _, v := range cmd.Forbidden {
				if !containsFold(got.Forbidden, v) {
					t.Errorf("%s: %s does not forbid %q", id, cmd.Field, v)
				}
			}
		}
		if !found {
			t.Errorf("%s declares no %s command field", id, cmd.Field)
		}
	}
	// The PagerDuty routing key travels in the body, so it must be a sealed secret.
	if pd, ok := byID["pagerduty"]; ok {
		if !slices.Contains(pd.SecretFields, "routing_key") || !slices.Contains(pd.Secrets, "routing_key") {
			t.Errorf("pagerduty: routing_key must be declared a secret field and read as {{ secrets.routing_key }}; "+
				"secret_fields=%v secrets=%v", pd.SecretFields, pd.Secrets)
		}
	}
}

// TestTheCatalogCheckRefusesACommand proves the check bites. Each fixture in
// testdata/catalog is a file built to fail it one way, and must.
func TestTheCatalogCheckRefusesACommand(t *testing.T) {
	cases := []struct {
		file string
		// want are fragments that must each appear in some problem.
		want []string
	}{
		{
			// The one-line "helpful" mapping: `status: resolved` on quiet. Refused
			// twice over — it is spelled, and the quiet fact renders it.
			file: "resolves-on-quiet.yaml",
			want: []string{`facts/quiet spells "resolved"`, `fixture "quiet" (facts/quiet): status is "resolved"`},
		},
		{
			// PagerDuty's resolve assembled from two halves: no spelling to find, so
			// only the render over every fact can catch it — and does, on quiet.
			file: "assembles-a-resolve.yaml",
			want: []string{`fixture "quiet" (body): event_action is "resolve"`},
		},
		{
			// No command field declared: refused, not passed.
			file: "declares-no-command.yaml",
			want: []string{"it declares no command field"},
		},
		{
			// A declared command field the body never carries: the tool's default
			// would decide.
			file: "omits-the-command.yaml",
			want: []string{`fixture "drawn" (body): the body carries no status`},
		},
		{
			// A routing key written into the file rather than referenced.
			file: "holds-a-literal-key.yaml",
			want: []string{`routing_key is not a`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "catalog", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			entry, err := ParseCatalogFile(strings.TrimSuffix(tc.file, ".yaml"), raw)
			if err != nil {
				t.Fatalf("the fixture must PARSE, so the check is what refuses it: %v", err)
			}
			probs := CheckCatalogMapping(entry)
			if len(probs) == 0 {
				t.Fatalf("%s passed the catalog check; it must fail it", tc.file)
			}
			for _, w := range tc.want {
				hit := false
				for _, p := range probs {
					if strings.Contains(p, w) {
						hit = true
						break
					}
				}
				if !hit {
					t.Errorf("no problem says %q; got:\n  %s", w, strings.Join(probs, "\n  "))
				}
			}
		})
	}
}

// TestTheSpellingCheckIsAWholeWord pins what "spelled" means: the forbidden value
// as a whole word in any case, and never a fact's own name that contains it.
func TestTheSpellingCheckIsAWholeWord(t *testing.T) {
	cases := []struct {
		forbidden, src string
		want           bool
	}{
		{"resolve", `"event_action": "resolve"`, true},
		{"resolve", `{% if x == "RESOLVE" %}`, true},
		{"resolved", `"status":"resolved"}`, true},
		{"resolved", `{% if reason == "all_resolved" %}`, false},
		{"resolve", `"resolved"`, false},
		{"acknowledge", `"acknowledged by"`, false},
	}
	for _, tc := range cases {
		if got := wholeWord(tc.forbidden).MatchString(tc.src); got != tc.want {
			t.Errorf("wholeWord(%q) on %q = %v, want %v", tc.forbidden, tc.src, got, tc.want)
		}
	}
}

// keyShaped is a long unbroken run of letters and digits — the shape of a PagerDuty
// integration key (32 characters) or a bearer token. A catalog file is reviewed
// prose and Liquid; nothing in one has a reason to look like this.
var keyShaped = regexp.MustCompile(`[A-Za-z0-9]{24,}`)

// TestNoCatalogFileHoldsACredential: a mapping never holds a secret (ADR 0055 §2),
// and a catalog file is the thing that is shared. The secret-field check proves a
// key is a reference where the mapping declares one; this catches a key pasted
// anywhere else in the file.
func TestNoCatalogFileHoldsACredential(t *testing.T) {
	names, err := catalogFileNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("the catalog embeds no files")
	}
	for _, name := range names {
		raw, err := mappings.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := keyShaped.Find(raw); m != nil {
			t.Errorf("%s holds %q, which is shaped like a key; a vendor key is a mapping secret "+
				"sealed on the connection and named as {{ secrets.<name> }}", name, m)
		}
	}
}

// TestACatalogFileIsOnlyWhatAConnectionStores: an unknown key in a catalog file, or
// in its mapping, is refused — a misspelt `fatcs` must not ship as a body that is
// silently never used.
func TestACatalogFileIsOnlyWhatAConnectionStores(t *testing.T) {
	base := "vendor: x\ntitle: x\nsummary: x\ndocs: [https://example.com/docs]\nchecked_on: \"2026-10-02\"\n" +
		"setup: [x]\ncommands: [{field: status, forbidden: [resolved]}]\n"
	for name, doc := range map[string]string{
		"unknown file key":    base + "mapping: {body: '{\"status\": \"firing\"}'}\nnotes: x\n",
		"unknown mapping key": base + "mapping: {body: '{\"status\": \"firing\"}', fatcs: {}}\n",
		"plain-http docs":     strings.Replace(base, "https://", "http://", 1) + "mapping: {body: '{\"status\": \"firing\"}'}\n",
		"no checked_on date":  strings.Replace(base, `"2026-10-02"`, `"last week"`, 1) + "mapping: {body: '{\"status\": \"firing\"}'}\n",
	} {
		if _, err := ParseCatalogFile("bad", []byte(doc)); err == nil {
			t.Errorf("%s: the file parsed; it must be refused", name)
		}
	}
}

func catalogFileNames() ([]string, error) {
	entries, err := mappings.FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
