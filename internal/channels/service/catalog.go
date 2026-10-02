package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0055 §2 AND §4: THE CATALOG OF PAYLOAD MAPPINGS, READ AND CHECKED (git-bug
// 2b5eecc).
//
// The catalog is `mappings/*.yaml` at the repository root, embedded in the binary.
// LoadCatalog reads it at boot so Settings → Connections can list it; importing an
// entry is the UI copying the entry's mapping into the Connection's own
// `payload_mapping`, through the same save-time gate every mapping passes — with
// the operator's pick written in for each of the entry's `choices` (owner ruling
// of 2026-10-02: a value only the operator may decide is asked at import, never
// answered by the file).
//
// ⛔ "NO MAPPING IN oto'S CATALOG TURNS A FACT INTO A RESOLVE, CLOSE OR STATUS
// CHANGE; A CATALOG REVIEW REFUSES ONE" (§4). A reviewer reading a Liquid branch on
// `reason == "quiet"` can miss a `"resolve"` three lines down, so the review is
// mechanical: CheckCatalogMapping, run over every catalog file by
// catalog_test.go. It knows no vendor. Each file DECLARES its tool's command fields
// and the values of them that would be a command, and the check holds the file to
// its own declaration — see CheckCatalogMapping for what that means and why each
// part is there.
//
// ⚠️ IMPORT DOES NOT RE-RUN THE CHECK, ON PURPOSE. Once copied onto a Connection a
// mapping is the operator's, and editing it into a command is a rule the operator
// wrote, keyed on a fact oto stated (§4, ADR 0044's test). The catalog itself can
// never ship one.

// catalogIDPattern is what a catalog file may be called: the id the API lists it by.
var catalogIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// catalogFile is one catalog file as written. `mapping` is decoded generically and
// re-encoded as JSON, then parsed as the stored document with unknown keys refused,
// so a file can hold nothing a Connection could not.
type catalogFile struct {
	Vendor       string           `yaml:"vendor"`
	Title        string           `yaml:"title"`
	Summary      string           `yaml:"summary"`
	Docs         []string         `yaml:"docs"`
	CheckedOn    string           `yaml:"checked_on"`
	Setup        []string         `yaml:"setup"`
	Commands     []catalogCommand `yaml:"commands"`
	Choices      []catalogChoice  `yaml:"choices"`
	SecretFields []string         `yaml:"secret_fields"`
	Mapping      any              `yaml:"mapping"`
}

type catalogChoice struct {
	Name     string   `yaml:"name"`
	Question string   `yaml:"question"`
	Field    string   `yaml:"field"`
	Options  []string `yaml:"options"`
}

// choiceNamePattern is what a choice may be called, the same shape as a secret's
// name.
var choiceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// choiceOptionPattern is what a choice's option may be: a plain token. It is
// written into the mapping's JSON strings and Liquid as text, so it holds nothing
// that could close a string or open a tag.
var choiceOptionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type catalogCommand struct {
	Field     string   `yaml:"field"`
	Forbidden []string `yaml:"forbidden"`
}

// LoadCatalog reads every `*.yaml` at the root of fsys, sorted by name.
//
// A file that does not parse fails the whole load: the catalog is embedded, so a
// broken file is a broken build, and booting with a silently shorter catalog would
// hide it.
func LoadCatalog(fsys fs.FS) ([]domain.CatalogMapping, error) {
	names, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, fmt.Errorf("mapping catalog: %w", err)
	}
	out := make([]domain.CatalogMapping, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("mapping catalog: %w", err)
		}
		entry, err := ParseCatalogFile(strings.TrimSuffix(path.Base(name), ".yaml"), raw)
		if err != nil {
			return nil, fmt.Errorf("mapping catalog: %s: %w", name, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// ParseCatalogFile reads one catalog file and checks its SHAPE: every key known,
// the descriptive fields present, the docs `https://`, the date a date, and the
// mapping a mapping document that compiles.
//
// It does not judge the commands — a file that declares none still parses, so the
// check, not the parser, is what refuses it, and says why.
func ParseCatalogFile(id string, raw []byte) (domain.CatalogMapping, error) {
	if !catalogIDPattern.MatchString(id) {
		return domain.CatalogMapping{}, fmt.Errorf("%q is not a catalog id: lower-case letters, digits and dashes", id)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f catalogFile
	if err := dec.Decode(&f); err != nil {
		return domain.CatalogMapping{}, fmt.Errorf("not a catalog file: %w", err)
	}

	var probs []string
	for _, req := range [][2]string{{"vendor", f.Vendor}, {"title", f.Title}, {"summary", f.Summary}} {
		if strings.TrimSpace(req[1]) == "" {
			probs = append(probs, "`"+req[0]+"` is required")
		}
	}
	if len(f.Docs) == 0 {
		probs = append(probs, "`docs` names at least one page of the vendor's public docs")
	}
	for _, d := range f.Docs {
		if u, err := url.Parse(d); err != nil || u.Scheme != "https" || u.Host == "" {
			probs = append(probs, fmt.Sprintf("`docs`: %q is not an https:// URL", d))
		}
	}
	if _, err := time.Parse(time.DateOnly, f.CheckedOn); err != nil {
		probs = append(probs, "`checked_on` is the date the field names were checked, YYYY-MM-DD")
	}
	if len(f.Setup) == 0 {
		probs = append(probs, "`setup` says what the operator does, in at least one step")
	}
	if f.Mapping == nil {
		probs = append(probs, "`mapping` is required")
	}
	probs = append(probs, checkChoiceShapes(f.Choices)...)
	if len(probs) > 0 {
		return domain.CatalogMapping{}, errors.New(strings.Join(probs, "; "))
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f.Mapping); err != nil {
		return domain.CatalogMapping{}, fmt.Errorf("`mapping` is not a mapping document: %w", err)
	}
	mapping := json.RawMessage(bytes.TrimSpace(buf.Bytes()))
	doc, err := domain.ParsePayloadMapping(mapping)
	if err != nil {
		return domain.CatalogMapping{}, err
	}
	m, mprobs := template.CompileMapping(doc)
	if len(mprobs) > 0 {
		msgs := make([]string, 0, len(mprobs))
		for _, p := range mprobs {
			msgs = append(msgs, "mapping/"+p.Field+": "+p.Message)
		}
		return domain.CatalogMapping{}, errors.New(strings.Join(msgs, "; "))
	}

	// Every placeholder names a declared choice, and every declared choice is
	// written somewhere: a choice nobody reads is a question with no effect.
	written := map[string]bool{}
	for _, src := range mappingSources(doc) {
		for _, n := range domain.ChoicesIn(src.text) {
			written[n] = true
		}
	}
	declared := map[string]bool{}
	choices := make([]domain.CatalogChoice, 0, len(f.Choices))
	for _, c := range f.Choices {
		declared[c.Name] = true
		if !written[c.Name] {
			probs = append(probs, fmt.Sprintf("choice %s is declared and the mapping never writes %s",
				c.Name, domain.ChoicePlaceholder(c.Name)))
		}
		choices = append(choices, domain.CatalogChoice{
			Name: c.Name, Question: strings.TrimSpace(c.Question), Field: c.Field, Options: c.Options,
		})
	}
	for _, n := range slices.Sorted(maps.Keys(written)) {
		if !declared[n] {
			probs = append(probs, fmt.Sprintf("the mapping writes %s and `choices` declares no %q",
				domain.ChoicePlaceholder(n), n))
		}
	}
	if len(probs) > 0 {
		return domain.CatalogMapping{}, errors.New(strings.Join(probs, "; "))
	}

	commands := make([]domain.CatalogCommand, 0, len(f.Commands))
	for _, c := range f.Commands {
		commands = append(commands, domain.CatalogCommand{Field: c.Field, Forbidden: c.Forbidden})
	}
	return domain.CatalogMapping{
		ID:           id,
		Vendor:       f.Vendor,
		Title:        f.Title,
		Summary:      strings.TrimSpace(f.Summary),
		Docs:         f.Docs,
		CheckedOn:    f.CheckedOn,
		Setup:        f.Setup,
		Commands:     commands,
		Choices:      choices,
		SecretFields: f.SecretFields,
		Secrets:      m.Secrets(),
		Mapping:      mapping,
	}, nil
}

// maxCatalogProblems caps what one check reports: twenty-five fixtures times one
// mistake is twenty-five copies of one sentence.
const maxCatalogProblems = 20

// catalogCanary is the value a secret is filled with while the check reads a
// rendered body, so a secret field can be told apart from a literal that happens
// to sit where the key goes.
func catalogCanary(name string) string { return "oto-catalog-canary-" + name }

// CheckCatalogMapping is THE CATALOG REVIEW of ADR 0055 §4, mechanised. It returns
// every reason the entry may not ship in the catalog; none means it may.
//
// It is vendor-agnostic: all it knows of a tool is what the entry declares.
//
//  1. THE ENTRY DECLARES ITS COMMAND FIELDS. At least one, each with at least one
//     forbidden value. An entry that declares none is REFUSED rather than passed,
//     so a contributor has to name the field and a reviewer has to agree it is the
//     right one — the one judgement no test can make.
//  2. NO FORBIDDEN VALUE IS SPELLED ANYWHERE IN THE MAPPING, as a whole word,
//     ignoring case — in a JSON value, a Liquid comparison or a branch nobody can
//     reach — nor offered as a choice's option. Rendering proves only the branches
//     the corpus reaches; a mapping that cannot write the word cannot send it, and
//     nobody has to argue whether a branch is reachable.
//  3. EVERY FACT RENDERS AND NONE RENDERS A COMMAND. The mapping is rendered
//     against the save-time corpus — every one of the 20 facts plus the hostile and
//     zero-value shapes (template.MappingFixtures) — and in every body each command
//     field must be PRESENT and must not equal a forbidden value. Present, because
//     a mapping that leaves the field out has handed the choice to the tool's
//     default, which is not a choice the catalog may make blind.
//  4. A CONNECTION WOULD ACCEPT IT: ValidateMapping, given the secrets it names.
//  5. A KEY IS A REFERENCE. Each secret field renders as exactly one secret the
//     mapping names, on every fact — never a literal key, never empty.
//  6. ONE ANSWER PER KEY. No object in a rendered body repeats a key: the check
//     reads the first and a tool may read the last.
//  7. A COMMAND IS WRITTEN, NOT INTERPOLATED. Each command field's rendered value
//     appears as a literal in some mapping source; otherwise it comes from the
//     envelope's data (or from a choice), and an alert the corpus does not contain
//     could set it.
//  8. A CHOICE IS THE OPERATOR'S, AND THE FILE DOES NOT ANSWER IT. A file with
//     choices is checked once per option of each — 3 to 7 run over every filled
//     copy an import could store — and then, fact by fact, the choice's field must
//     be present, must be one of its options, and must either FOLLOW the pick (the
//     fact carries no usable value, so the operator's answer goes out) or PASS
//     THROUGH a value the fact itself carries, the same whatever is picked. A value
//     that is the same whatever is picked and that the fact does not carry is a
//     fallback the FILE chose — PagerDuty's old `critical` — and is refused. At
//     least one fact must follow the pick, or the question changes nothing.
func CheckCatalogMapping(c domain.CatalogMapping) []string {
	var probs []string
	seen := map[string]bool{}
	// Each filled copy is checked in turn, and a mistake that does not depend on
	// the pick would otherwise be reported once per option.
	add := func(format string, args ...any) {
		p := fmt.Sprintf(format, args...)
		if len(probs) < maxCatalogProblems && !seen[p] {
			seen[p] = true
			probs = append(probs, p)
		}
	}

	// 1. The declaration.
	if len(c.Commands) == 0 {
		add("it declares no command field: name the field the tool reads as an instruction " +
			"and the values that would resolve, close, acknowledge or change an incident's status")
	}
	for i, cmd := range c.Commands {
		if strings.TrimSpace(cmd.Field) == "" {
			add("commands[%d] names no field", i)
		}
		if len(cmd.Forbidden) == 0 {
			add("commands[%d] (%s) forbids no value", i, cmd.Field)
		}
		for _, v := range cmd.Forbidden {
			if strings.TrimSpace(v) == "" {
				add("commands[%d] (%s) forbids an empty value", i, cmd.Field)
			}
		}
	}

	doc, err := domain.ParsePayloadMapping(c.Mapping)
	if err != nil {
		add("the mapping is not a mapping document: %s", err.Error())
		return probs
	}

	// 2. The spelling — in the mapping as the catalog writes it, and in what a
	// choice could write into it.
	sources := mappingSources(doc)
	for _, src := range sources {
		for _, cmd := range c.Commands {
			for _, v := range cmd.Forbidden {
				if strings.TrimSpace(v) != "" && wholeWord(v).MatchString(src.text) {
					add("%s spells %q, a value of %s the catalog never sends", src.field, v, cmd.Field)
				}
			}
		}
	}
	for _, ch := range c.Choices {
		for _, cmd := range c.Commands {
			if ch.Field == cmd.Field {
				add("choice %s decides %s, a command field: the catalog writes a command, it never asks for one",
					ch.Name, cmd.Field)
			}
			for _, v := range cmd.Forbidden {
				if containsFoldTrim(ch.Options, v) {
					add("choice %s offers %q, a value of %s the catalog never sends", ch.Name, v, cmd.Field)
				}
			}
		}
	}

	// 3 to 7, over every copy an import could store.
	variants, err := catalogVariants(c)
	if err != nil {
		add("%s", err.Error())
		return probs
	}
	for i := range variants {
		v := &variants[i]
		// 4. A connection would accept it.
		if err := ValidateMapping(v.mapping, c.Secrets); err != nil {
			if e, ok := errs.As(err); ok && len(e.Violations) > 0 {
				for _, viol := range e.Violations {
					add("a connection would refuse it: %s: %s", viol.Field, viol.Message)
				}
			} else {
				add("a connection would refuse it: %s", err.Error())
			}
			return probs
		}
		v.rendered = renderCatalogVariant(c, v.mapping, sources, add)
		if v.rendered == nil {
			return probs
		}
	}

	// 8. The choices.
	for _, ch := range c.Choices {
		checkChoice(ch, variants, add)
	}
	return probs
}

// catalogVariant is one copy of a catalog mapping an import could store: every
// choice filled, the one under test with each of its options in turn.
type catalogVariant struct {
	// varies is the choice this copy fills with each of its options in turn; ""
	// for a file that asks nothing.
	varies   string
	picks    map[string]string
	mapping  json.RawMessage
	rendered []renderedFixture
}

// renderedFixture is one fact of the corpus, rendered through one copy.
type renderedFixture struct {
	fixture  string
	field    string
	body     []byte
	envelope []byte
}

// catalogVariants is the mapping itself when the file asks nothing, and otherwise
// one filled copy per option of each choice, the other choices at their first.
func catalogVariants(c domain.CatalogMapping) ([]catalogVariant, error) {
	if len(c.Choices) == 0 {
		return []catalogVariant{{mapping: c.Mapping}}, nil
	}
	var out []catalogVariant
	for _, ch := range c.Choices {
		for _, opt := range ch.Options {
			picks := make(map[string]string, len(c.Choices))
			for _, other := range c.Choices {
				if len(other.Options) > 0 {
					picks[other.Name] = other.Options[0]
				}
			}
			picks[ch.Name] = opt
			filled, err := c.Fill(picks)
			if err != nil {
				return nil, fmt.Errorf("the mapping cannot be filled: %w", err)
			}
			out = append(out, catalogVariant{varies: ch.Name, picks: picks, mapping: filled})
		}
	}
	return out, nil
}

// renderCatalogVariant runs checks 3, 5, 6 and 7 over one filled copy and returns
// what every fixture rendered to, or nil when the copy does not compile.
func renderCatalogVariant(
	c domain.CatalogMapping, mapping json.RawMessage, sources []mappingSource, add func(string, ...any),
) []renderedFixture {
	doc, err := domain.ParsePayloadMapping(mapping)
	if err != nil {
		add("the mapping is not a mapping document: %s", err.Error())
		return nil
	}
	m, mprobs := template.CompileMapping(doc)
	if len(mprobs) > 0 {
		add("the mapping does not compile")
		return nil
	}
	fromData := map[string]bool{}
	canaries := make(map[string]string, len(c.Secrets))
	for _, n := range c.Secrets {
		canaries[n] = catalogCanary(n)
	}
	out := []renderedFixture{}
	for _, fx := range template.MappingFixtures() {
		envelope, err := sampleEnvelope(fx)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		rendered, err := m.Render(envelope)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		body, _, err := template.FillSecrets([]byte(rendered.Body), rendered.Headers, canaries)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		out = append(out, renderedFixture{fixture: fx.Name, field: rendered.Field, body: body, envelope: envelope})
		// A repeated key is two answers to one question, and gjson reads the first
		// while a tool may read the last: the check would pass a `trigger` the tool
		// never sees.
		if k, err := repeatedKey(body); err != nil {
			add("fixture %q (%s): the body does not decode: %s", fx.Name, rendered.Field, err.Error())
		} else if k != "" {
			add("fixture %q (%s): the body repeats the key %q; which of the two a tool reads is its choice, "+
				"not the catalog's", fx.Name, rendered.Field, k)
		}
		for _, cmd := range c.Commands {
			got := gjson.GetBytes(body, cmd.Field)
			if !got.Exists() {
				add("fixture %q (%s): the body carries no %s, so the tool's default decides what the fact does",
					fx.Name, rendered.Field, cmd.Field)
				continue
			}
			for _, v := range cmd.Forbidden {
				if strings.EqualFold(strings.TrimSpace(got.String()), strings.TrimSpace(v)) {
					add("fixture %q (%s): %s is %q — the fact %q became a command",
						fx.Name, rendered.Field, cmd.Field, got.String(), rendered.Fact)
				}
			}
			// The corpus is finite and an alert is not: a command field whose value
			// is interpolated passes every fixture and sends whatever a label says.
			// So the value must be one the mapping itself writes, as a literal — in
			// the file as written, not in a copy a choice filled. Reported once per
			// field: twenty copies would crowd out the rest.
			if !fromData[cmd.Field] && !writtenLiterally(got, sources) {
				fromData[cmd.Field] = true
				add("fixture %q (%s): %s is %q, which no mapping source writes as a literal; "+
					"it comes from the envelope's data, so an alert would decide the command",
					fx.Name, rendered.Field, cmd.Field, got.String())
			}
		}
		for _, sf := range c.SecretFields {
			got := gjson.GetBytes(body, sf)
			if !isCanary(got, c.Secrets) {
				add("fixture %q (%s): %s is not a `{{ secrets.<name> }}` reference and nothing else",
					fx.Name, rendered.Field, sf)
			}
		}
	}
	return out
}

// checkChoice is check 8 for one choice: fact by fact, across the copies filled
// with each of its options.
func checkChoice(ch domain.CatalogChoice, variants []catalogVariant, add func(string, ...any)) {
	// The copies that vary this choice, one per option, in the options' order.
	var mine []catalogVariant
	for _, v := range variants {
		if v.varies == ch.Name {
			mine = append(mine, v)
		}
	}
	if len(mine) == 0 {
		return
	}
	n := len(mine[0].rendered)
	for _, v := range mine {
		if len(v.rendered) != n {
			return // a fixture failed to render; that is already reported
		}
	}
	followed := false
	for j := 0; j < n; j++ {
		fx := mine[0].rendered[j]
		vals := make([]string, 0, len(mine))
		present := true
		for _, v := range mine {
			got := gjson.GetBytes(v.rendered[j].body, ch.Field)
			if !got.Exists() || got.Type != gjson.String {
				add("fixture %q (%s): the body carries no %s as a string, which choice %s decides",
					fx.fixture, fx.field, ch.Field, ch.Name)
				present = false
				break
			}
			if !slices.Contains(ch.Options, got.Str) {
				add("fixture %q (%s): %s is %q, which is not one of choice %s's options (%s)",
					fx.fixture, fx.field, ch.Field, got.Str, ch.Name, strings.Join(ch.Options, ", "))
			}
			vals = append(vals, got.Str)
		}
		if !present {
			continue
		}
		follows, same := true, true
		for i, v := range mine {
			if vals[i] != v.picks[ch.Name] {
				follows = false
			}
			if vals[i] != vals[0] {
				same = false
			}
		}
		switch {
		case follows:
			followed = true
		case same && carries(fx.envelope, vals[0]):
			// Passed through from the fact, whatever was picked.
		case same:
			add("fixture %q (%s): %s is %q whatever the operator picks for %s, and the fact carries no "+
				"such value: the file hard-codes the fallback that %s asks the operator to choose; "+
				"write %s there instead", fx.fixture, fx.field, ch.Field, vals[0], ch.Name, ch.Name,
				domain.ChoicePlaceholder(ch.Name))
		default:
			add("fixture %q (%s): %s is %s across the picks %s: it neither follows choice %s nor "+
				"passes one value of the fact through", fx.fixture, fx.field, ch.Field,
				strings.Join(vals, "/"), strings.Join(ch.Options, "/"), ch.Name)
		}
	}
	if !followed {
		add("choice %s never reaches %s: no fact sends the operator's pick, so the question changes nothing",
			ch.Name, ch.Field)
	}
}

// carries reports whether some string anywhere in the envelope is exactly v — a
// label, a state, a name — so a value the body sends can be told to come from the
// fact rather than from the file.
func carries(envelope []byte, v string) bool {
	var walk func(r gjson.Result) bool
	walk = func(r gjson.Result) bool {
		switch {
		case r.Type == gjson.String:
			return r.Str == v
		case r.IsObject() || r.IsArray():
			found := false
			r.ForEach(func(_, x gjson.Result) bool {
				found = walk(x)
				return !found
			})
			return found
		default:
			return false
		}
	}
	return walk(gjson.ParseBytes(envelope))
}

// checkChoiceShapes is the shape of `choices`, checked at parse.
func checkChoiceShapes(choices []catalogChoice) []string {
	var probs []string
	names := map[string]bool{}
	for i, c := range choices {
		if !choiceNamePattern.MatchString(c.Name) {
			probs = append(probs, fmt.Sprintf("choices[%d]: %q is not a choice name: lower-case letters, digits "+
				"and underscores, starting with a letter", i, c.Name))
		}
		if names[c.Name] {
			probs = append(probs, fmt.Sprintf("choices[%d]: %s is declared twice", i, c.Name))
		}
		names[c.Name] = true
		if strings.TrimSpace(c.Question) == "" {
			probs = append(probs, fmt.Sprintf("choices[%d] (%s): `question` says what the import asks", i, c.Name))
		}
		if strings.TrimSpace(c.Field) == "" {
			probs = append(probs, fmt.Sprintf("choices[%d] (%s): `field` names the body field it decides", i, c.Name))
		}
		if len(c.Options) < 2 {
			probs = append(probs, fmt.Sprintf("choices[%d] (%s): `options` offers at least two values; "+
				"one is not a choice", i, c.Name))
		}
		opts := map[string]bool{}
		for _, o := range c.Options {
			if !choiceOptionPattern.MatchString(o) {
				probs = append(probs, fmt.Sprintf("choices[%d] (%s): option %q is not a plain token: letters, "+
					"digits, `_`, `.` and `-`", i, c.Name, o))
			}
			if opts[o] {
				probs = append(probs, fmt.Sprintf("choices[%d] (%s): option %q is offered twice", i, c.Name, o))
			}
			opts[o] = true
		}
	}
	return probs
}

func containsFoldTrim(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// writtenLiterally reports whether a rendered command value appears in some mapping
// source as the JSON literal it is — `"trigger"` for a string, the bare token for a
// number or a boolean.
func writtenLiterally(got gjson.Result, sources []mappingSource) bool {
	literal := got.Raw
	if got.Type == gjson.String {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(got.Str); err != nil {
			return false
		}
		literal = strings.TrimSuffix(b.String(), "\n")
	}
	for _, src := range sources {
		if strings.Contains(src.text, literal) {
			return true
		}
	}
	return false
}

// repeatedKey walks a JSON document token by token and returns the first key any
// one object holds twice, or "".
func repeatedKey(body []byte) (string, error) {
	type frame struct {
		keys    map[string]bool // nil for an array
		wantKey bool
	}
	var stack []*frame
	// valueDone marks the enclosing object as ready for its next key.
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].keys != nil {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if n := len(stack); n > 0 && stack[n-1].keys != nil && stack[n-1].wantKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				valueDone()
				continue
			}
			k, _ := tok.(string)
			if stack[n-1].keys[k] {
				return k, nil
			}
			stack[n-1].keys[k] = true
			stack[n-1].wantKey = false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{keys: map[string]bool{}, wantKey: true})
		case json.Delim('['):
			stack = append(stack, &frame{})
		case json.Delim(']'), json.Delim('}'):
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// isCanary reports whether a rendered field is exactly one secret's canary.
func isCanary(got gjson.Result, secrets []string) bool {
	if got.Type != gjson.String {
		return false
	}
	for _, n := range secrets {
		if got.Str == catalogCanary(n) {
			return true
		}
	}
	return false
}

// wholeWord matches v as a whole word, ignoring case: `resolve` but not
// `all_resolved`, which is a fact's name and not a command.
func wholeWord(v string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(strings.TrimSpace(v)) + `($|[^A-Za-z0-9_])`)
}

type mappingSource struct {
	field string
	text  string
}

// mappingSources is every Liquid source in a mapping, named the way a save-time
// refusal names it.
func mappingSources(doc domain.PayloadMapping) []mappingSource {
	out := []mappingSource{{field: "body", text: doc.Body}}
	for _, fact := range slices.Sorted(maps.Keys(doc.Facts)) {
		out = append(out, mappingSource{field: "facts/" + fact, text: doc.Facts[fact]})
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Headers)) {
		out = append(out, mappingSource{field: "headers/" + name, text: doc.Headers[name]})
	}
	return out
}
