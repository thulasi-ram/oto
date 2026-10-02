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
// `payload_mapping`, through the same save-time gate every mapping passes.
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
	SecretFields []string         `yaml:"secret_fields"`
	Mapping      any              `yaml:"mapping"`
}

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
//     reach. Rendering proves only the branches the corpus reaches; a mapping that
//     cannot write the word cannot send it, and nobody has to argue whether a
//     branch is reachable.
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
//     envelope's data, and an alert the corpus does not contain could set it.
func CheckCatalogMapping(c domain.CatalogMapping) []string {
	var probs []string
	add := func(format string, args ...any) {
		if len(probs) < maxCatalogProblems {
			probs = append(probs, fmt.Sprintf(format, args...))
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

	// 2. The spelling.
	for _, src := range mappingSources(doc) {
		for _, cmd := range c.Commands {
			for _, v := range cmd.Forbidden {
				if strings.TrimSpace(v) != "" && wholeWord(v).MatchString(src.text) {
					add("%s spells %q, a value of %s the catalog never sends", src.field, v, cmd.Field)
				}
			}
		}
	}

	// 4. A connection would accept it.
	if err := ValidateMapping(c.Mapping, c.Secrets); err != nil {
		if e, ok := errs.As(err); ok && len(e.Violations) > 0 {
			for _, v := range e.Violations {
				add("a connection would refuse it: %s: %s", v.Field, v.Message)
			}
		} else {
			add("a connection would refuse it: %s", err.Error())
		}
		return probs
	}

	// 3 and 5. Every fact, rendered.
	m, mprobs := template.CompileMapping(doc)
	if len(mprobs) > 0 {
		add("the mapping does not compile")
		return probs
	}
	sources := mappingSources(doc)
	fromData := map[string]bool{}
	canaries := make(map[string]string, len(c.Secrets))
	for _, n := range c.Secrets {
		canaries[n] = catalogCanary(n)
	}
	for _, fx := range template.MappingFixtures() {
		envelope, err := sampleEnvelope(fx)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		out, err := m.Render(envelope)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		body, _, err := template.FillSecrets([]byte(out.Body), out.Headers, canaries)
		if err != nil {
			add("fixture %q: %s", fx.Name, err.Error())
			continue
		}
		// A repeated key is two answers to one question, and gjson reads the first
		// while a tool may read the last: the check would pass a `trigger` the tool
		// never sees.
		if k, err := repeatedKey(body); err != nil {
			add("fixture %q (%s): the body does not decode: %s", fx.Name, out.Field, err.Error())
		} else if k != "" {
			add("fixture %q (%s): the body repeats the key %q; which of the two a tool reads is its choice, "+
				"not the catalog's", fx.Name, out.Field, k)
		}
		for _, cmd := range c.Commands {
			got := gjson.GetBytes(body, cmd.Field)
			if !got.Exists() {
				add("fixture %q (%s): the body carries no %s, so the tool's default decides what the fact does",
					fx.Name, out.Field, cmd.Field)
				continue
			}
			for _, v := range cmd.Forbidden {
				if strings.EqualFold(strings.TrimSpace(got.String()), strings.TrimSpace(v)) {
					add("fixture %q (%s): %s is %q — the fact %q became a command",
						fx.Name, out.Field, cmd.Field, got.String(), out.Fact)
				}
			}
			// The corpus is finite and an alert is not: a command field whose value
			// is interpolated passes every fixture and sends whatever a label says.
			// So the value must be one the mapping itself writes, as a literal.
			// Reported once per field: twenty copies would crowd out the rest.
			if !fromData[cmd.Field] && !writtenLiterally(got, sources) {
				fromData[cmd.Field] = true
				add("fixture %q (%s): %s is %q, which no mapping source writes as a literal; "+
					"it comes from the envelope's data, so an alert would decide the command",
					fx.Name, out.Field, cmd.Field, got.String())
			}
		}
		for _, sf := range c.SecretFields {
			got := gjson.GetBytes(body, sf)
			if !isCanary(got, c.Secrets) {
				add("fixture %q (%s): %s is not a `{{ secrets.<name> }}` reference and nothing else",
					fx.Name, out.Field, sf)
			}
		}
	}
	return probs
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
