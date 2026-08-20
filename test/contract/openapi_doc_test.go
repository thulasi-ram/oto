package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/thulasiram/oto/api/openapi"
)

// This file holds the OpenAPI document reader shared by gate G1
// (dto_schema_test.go) and gate G2 (server_conformance_test.go).
//
// It deliberately does NOT use a third-party OpenAPI library. Two reasons:
// the contract is OpenAPI 3.1, whose schemas ARE JSON Schema 2020-12, so the
// only machinery actually needed is `$ref` resolution plus `allOf` merging;
// and every 3.1-capable Go library available today either drops `examples`,
// rewrites `type: ['string','null']` into `nullable: true`, or normalises the
// document on load — all three of which would make a gate that compares the
// contract to something else compare against a rewritten contract instead.

// doc is the parsed `api/openapi/openapi.yaml`.
type doc struct {
	root    map[string]any
	schemas map[string]any
	paths   map[string]any
}

// loadDoc parses the EMBEDDED contract — the same bytes the binary serves at
// `GET /openapi.json` — so the gate can never pass against a copy on disk that
// the server does not use.
func loadDoc(t *testing.T) *doc {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal(openapi.YAML, &root); err != nil {
		t.Fatalf("parse api/openapi/openapi.yaml: %v", err)
	}
	comps, _ := root["components"].(map[string]any)
	if comps == nil {
		t.Fatal("contract has no `components`")
	}
	schemas, _ := comps["schemas"].(map[string]any)
	if len(schemas) == 0 {
		t.Fatal("contract has no `components.schemas`")
	}
	paths, _ := root["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("contract has no `paths`")
	}
	return &doc{root: root, schemas: schemas, paths: paths}
}

// schema returns one component schema by name.
func (d *doc) schema(name string) (map[string]any, bool) {
	s, ok := d.schemas[name].(map[string]any)
	return s, ok
}

// refName returns the component name a schema node points at, or "".
func refName(s map[string]any) string {
	ref, _ := s["$ref"].(string)
	n := strings.TrimPrefix(ref, "#/components/schemas/")
	if n == ref {
		return ""
	}
	return n
}

// flat is one schema node with `$ref`, `allOf` and the `oneOf`-with-null idiom
// resolved away. It is what both gates actually compare against.
type flat struct {
	// types is the set of OpenAPI types the node permits, with "null" removed
	// into nullable. Empty means the node constrains no type at all.
	types    map[string]bool
	nullable bool
	props    map[string]map[string]any
	required map[string]bool
	// hasProps distinguishes "an object with no declared properties" (a free
	// map) from "not an object".
	hasProps bool
	items    map[string]any
	addl     map[string]any
	// addlFalse records `additionalProperties: false`.
	addlFalse bool
	// maxItems is the array bound, nil when the node declares none. It is read
	// through the same `$ref`/`allOf` resolution as everything else so that an
	// array declared once in `components.schemas` and referenced from a
	// parameter is bounded by what it really resolves to.
	maxItems *int
	enum     []any
	// variants is a non-null oneOf/anyOf with more than one real branch: the
	// node is a union and a field-by-field comparison is not meaningful.
	variants []map[string]any
	// origin names the component this came from, for error messages.
	origin string
}

// flatten resolves one schema node into a comparable shape.
func (d *doc) flatten(s map[string]any) flat {
	f := flat{types: map[string]bool{}, props: map[string]map[string]any{}, required: map[string]bool{}}
	f.origin = refName(s)
	d.mergeInto(&f, s, 0)
	return f
}

func (d *doc) mergeInto(f *flat, s map[string]any, depth int) {
	if depth > 32 || s == nil {
		return
	}
	if ref := refName(s); ref != "" {
		// `origin` is set ONLY from a `$ref` on the node itself, never from one
		// inside an `allOf`. `AlertDetailDTO` is `allOf: [AlertDTO, {…}]`, and
		// taking the origin from the first branch would file every divergence in
		// its own branch under `AlertDTO`, pointing the fix at the wrong schema.
		if depth == 0 {
			f.origin = ref
		}
		if target, ok := d.schema(ref); ok {
			d.mergeInto(f, target, depth+1)
		}
		if len(s) == 1 {
			return
		}
	}

	switch t := s["type"].(type) {
	case string:
		if t == "null" {
			f.nullable = true
		} else {
			f.types[t] = true
		}
	case []any:
		for _, v := range t {
			name, _ := v.(string)
			if name == "null" {
				f.nullable = true
			} else if name != "" {
				f.types[name] = true
			}
		}
	}

	if props, ok := s["properties"].(map[string]any); ok {
		f.hasProps = true
		for k, v := range props {
			if node, ok := v.(map[string]any); ok {
				f.props[k] = node
			}
		}
	}
	if req, ok := s["required"].([]any); ok {
		for _, v := range req {
			if name, _ := v.(string); name != "" {
				f.required[name] = true
			}
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		f.items = items
	}
	switch ap := s["additionalProperties"].(type) {
	case map[string]any:
		f.addl = ap
	case bool:
		if !ap {
			f.addlFalse = true
		}
	}
	if e, ok := s["enum"].([]any); ok {
		f.enum = e
	}
	if m, ok := s["maxItems"].(int); ok {
		f.maxItems = &m
	}

	if all, ok := s["allOf"].([]any); ok {
		for _, v := range all {
			if node, ok := v.(map[string]any); ok {
				d.mergeInto(f, node, depth+1)
			}
		}
	}

	// `oneOf: [X, {type: null}]` is how this contract spells "nullable X". A
	// oneOf/anyOf with more than one non-null branch is a genuine union and is
	// recorded as such rather than merged, because merging a union produces a
	// shape neither branch has.
	for _, key := range []string{"oneOf", "anyOf"} {
		list, ok := s[key].([]any)
		if !ok {
			continue
		}
		var branches []map[string]any
		for _, v := range list {
			node, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if isNullOnly(node) {
				f.nullable = true
				continue
			}
			branches = append(branches, node)
		}
		switch len(branches) {
		case 0:
		case 1:
			// `oneOf: [$ref X, null]` at the top of a node still IDENTIFIES the
			// node as X, so a divergence inside it is filed against X rather
			// than against the route that reached it.
			if depth == 0 && f.origin == "" {
				f.origin = refName(branches[0])
			}
			d.mergeInto(f, branches[0], depth+1)
		default:
			f.variants = append(f.variants, branches...)
		}
	}
}

func isNullOnly(n map[string]any) bool {
	if len(n) != 1 {
		return false
	}
	t, ok := n["type"].(string)
	return ok && t == "null"
}

// typeNames renders the permitted types for an error message.
func (f flat) typeNames() string {
	if len(f.types) == 0 {
		if f.hasProps {
			return "object (implied by properties)"
		}
		return "<untyped>"
	}
	out := make([]string, 0, len(f.types))
	for k := range f.types {
		out = append(out, k)
	}
	sort.Strings(out)
	s := strings.Join(out, "|")
	if f.nullable {
		s += "|null"
	}
	return s
}

// isObject reports whether the node describes an object, including the case
// where `type` is omitted but `properties` is present.
func (f flat) isObject() bool { return f.types["object"] || (len(f.types) == 0 && f.hasProps) }

// moduleRoot walks up from the test's working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 12 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod above the test's working directory")
	return ""
}

// sortedKeys is used everywhere a failure message must be stable, because a
// gate whose output reorders itself between runs cannot be diffed.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// failures accumulates every divergence so that one run reports the whole
// truth. A gate that stops at the first difference turns a ten-minute triage
// into ten CI runs.
//
// It DEDUPLICATES. One property of one component schema is reached by many
// paths — `OrgSettingsDTO.resolve_grace_s` is inside `MeDTO`, `OrgDTO` and
// `OrgSettingsViewDTO` as well as on its own — and reporting the same fact four
// times, once per route, buries the eighteen distinct facts under a hundred
// lines. The message names the SCHEMA the property belongs to, because that is
// where the fix goes.
type failures struct {
	seen map[string]bool
	list []string
}

func (f *failures) addf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[msg] {
		return
	}
	f.seen[msg] = true
	f.list = append(f.list, msg)
}

func (f *failures) items() []string {
	out := append([]string(nil), f.list...)
	sort.Strings(out)
	return out
}

func (f *failures) report(t *testing.T, headline string) {
	t.Helper()
	items := f.items()
	if len(items) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d divergence(s):\n", headline, len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "  • %s\n", it)
	}
	t.Fatal(b.String())
}
