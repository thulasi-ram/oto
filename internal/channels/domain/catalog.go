package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ADR 0055 §2: THE CATALOG OF PAYLOAD MAPPINGS (git-bug 2b5eecc).
//
// "A catalog of community mappings is a folder of files — contributed, reviewed and
// imported onto a Connection; no code ships with one." The folder is `mappings/` at
// the repository root, embedded in the binary; each file is one tool's mapping and
// what a reviewer needs to judge it.
//
// ⛔ IMPORT IS A COPY, NEVER A REFERENCE. A Connection that imports an entry stores
// the entry's Mapping as its own `payload_mapping`, and nothing on the Connection
// remembers where it came from, so a later catalog change cannot alter a live
// Connection. The copy is the operator's from then on, and so is any command they
// edit into it (§4).
//
// ⛔ AN ENTRY CARRIES NO SECRET. A vendor key in the body is `{{ secrets.<name> }}`
// and the operator seals the value on the Connection; Secrets lists the names an
// importer will need, and nothing else about them is in the file.

// CatalogMapping is one catalog file, read and shape-checked.
type CatalogMapping struct {
	// ID is the file's name without its extension: `pagerduty`, `incident-io`.
	ID string
	// Vendor is the tool, as its vendor writes it.
	Vendor string
	// Title names the API the mapping speaks to.
	Title string
	// Summary says what the mapping does with oto's facts.
	Summary string
	// Docs are the vendor's public docs the field names were checked against.
	Docs []string
	// CheckedOn is when they were checked, `YYYY-MM-DD`.
	CheckedOn string
	// Setup is what the operator does in the tool and on the Connection, in order.
	Setup []string
	// Commands are the tool's command fields and the values of them that would turn
	// a fact into a resolve, close, acknowledge or status change. The catalog check
	// refuses an entry that declares none, and fails one that renders any.
	Commands []CatalogCommand
	// Choices are the values the file leaves to the operator, asked at import and
	// written into the copy as literals (see CatalogChoice).
	Choices []CatalogChoice
	// SecretFields are body paths that carry a vendor key; each must render as a
	// `{{ secrets.<name> }}` reference and nothing else.
	SecretFields []string
	// Secrets is every secret name the mapping references, sorted — what the
	// operator must seal on the Connection before the import can be saved.
	Secrets []string
	// Mapping is the payload mapping document, exactly as a Connection stores it.
	Mapping json.RawMessage
}

// CatalogCommand is one command field of a tool's request body.
type CatalogCommand struct {
	// Field is a gjson path into the rendered body: `event_action`, `status`.
	Field string
	// Forbidden are the values of Field that would make a fact a command.
	Forbidden []string
}

// CatalogChoice is ONE VALUE THE CATALOG MAY NOT CHOOSE FOR THE OPERATOR (owner
// ruling of 2026-10-02 on PagerDuty's severity): the file declares it, its mapping
// writes `<<choose:<name>>>` where the value goes, and the import asks the operator
// to pick one of Options and writes the LITERAL pick into the Connection's copy.
//
// ⛔ THE COPY HOLDS THE PICK, NOT THE QUESTION. Nothing on the Connection remembers
// that a choice was asked, exactly as nothing remembers the catalog entry: a copy
// that still holds a placeholder is refused at save (ValidateMapping), so a raw
// API import that skipped the question fails loudly instead of sending the
// placeholder to the tool.
type CatalogChoice struct {
	// Name is how the mapping names it: `default_severity`.
	Name string
	// Question is what the import asks, in one sentence.
	Question string
	// Field is the gjson path into the rendered body that the choice decides:
	// `payload.severity`. The catalog check holds the file to it.
	Field string
	// Options are the values the operator picks from. Each is a plain token, so it
	// lands in the mapping's JSON and Liquid as text and never as structure.
	Options []string
}

// choicePlaceholder is a choice's mark in a catalog mapping. It is not Liquid, so a
// mapping that still holds one renders it as text — and the save gate refuses it.
var choicePlaceholder = regexp.MustCompile(`<<choose:([^<>]*)>>`)

// ChoicePlaceholder is how a catalog mapping writes the choice named name.
func ChoicePlaceholder(name string) string { return "<<choose:" + name + ">>" }

// ChoicesIn returns every choice name a mapping source writes as a placeholder, in
// the order written, repeats included.
func ChoicesIn(src string) []string {
	var out []string
	for _, m := range choicePlaceholder.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// Fill returns the entry's mapping with every choice placeholder replaced by the
// operator's pick — what an import stores. Every declared choice must be picked,
// each pick must be one of its Options, and no placeholder may be left over.
//
// It walks the decoded document and replaces inside its strings, rather than
// editing the JSON text, so an encoder's choice to escape `<` cannot hide a
// placeholder from it.
func (c CatalogMapping) Fill(picks map[string]string) (json.RawMessage, error) {
	declared := make(map[string]CatalogChoice, len(c.Choices))
	for _, ch := range c.Choices {
		declared[ch.Name] = ch
		v, ok := picks[ch.Name]
		if !ok {
			return nil, fmt.Errorf("choose a value for %s: one of %s", ch.Name, strings.Join(ch.Options, ", "))
		}
		if !slices.Contains(ch.Options, v) {
			return nil, fmt.Errorf("%q is not one of %s's options: %s", v, ch.Name, strings.Join(ch.Options, ", "))
		}
	}
	var doc any
	if err := json.Unmarshal(c.Mapping, &doc); err != nil {
		return nil, fmt.Errorf("the mapping is not JSON: %w", err)
	}
	var unknown []string
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			return choicePlaceholder.ReplaceAllStringFunc(t, func(m string) string {
				name := choicePlaceholder.FindStringSubmatch(m)[1]
				if _, ok := declared[name]; !ok {
					unknown = append(unknown, name)
					return m
				}
				return picks[name]
			})
		case map[string]any:
			for k, x := range t {
				t[k] = walk(x)
			}
			return t
		case []any:
			for i, x := range t {
				t[i] = walk(x)
			}
			return t
		default:
			return v
		}
	}
	doc = walk(doc)
	if len(unknown) > 0 {
		return nil, fmt.Errorf("the mapping writes %s, which the file does not declare as a choice",
			ChoicePlaceholder(unknown[0]))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes())), nil
}
