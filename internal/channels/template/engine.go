package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/osteele/liquid"
)

// Limits. Every one of them exists because the pivot to whole-message
// templating made a class of abuse reachable that per-slot templating could not
// express.
const (
	// MaxSourceBytes bounds a template's source. A whole card is a document,
	// not the one line the old design allowed, so this is far larger — but it
	// is still a bound, and it is the first thing that keeps a pathological
	// template from ever being parsed.
	MaxSourceBytes = 16384
	// MaxOutputBytes bounds what a render may produce. Slack's own ceiling is
	// lower and the renderer truncates against it; this catches the case where
	// a template tries to build something enormous before anyone truncates it.
	MaxOutputBytes = 64000
	// MaxForDepth bounds `{% for %}` nesting.
	//
	// ⛔ THIS IS THE DoS GATE, AND IT IS A SOURCE SCAN BECAUSE THERE IS NOWHERE
	// ELSE TO PUT IT. osteele/liquid renders into a buffer with no context, no
	// cancellation and no iteration budget, so an output cap is checked only
	// after the memory is already spent. Depth is the one thing measurable
	// before execution, and combined with bounded collections (BuildInput caps
	// every list it exposes) it makes the iteration count knowable in advance:
	// worst case is cap² , which is thousands, not billions.
	MaxForDepth = 2
	// MaxBlocks bounds a card's block count, ahead of Slack's own limit of 50.
	MaxBlocks = 50
)

// Format is the shape an author writes in. It is the second half of the
// capability key — the first is the provider — and it decides which editor,
// which validator, which preview and which compiler a template gets.
type Format string

const (
	// FormatCard is Markdown+, compiled by oto to each provider's own shape. It
	// is the default and the only PORTABLE format.
	FormatCard Format = "card"
	// FormatText is one flat string. Portable, and the right answer for a busy
	// channel that wants a one-liner.
	FormatText Format = "text"
	// FormatRaw is literal Block Kit JSON with interpolation.
	//
	// ⚠️ IT IS PINNED TO SLACK AND IT IS THE OPERATOR'S RISK. A malformed payload
	// is REJECTED BY SLACK, which turns a rendering mistake into an undelivered
	// alert — so saving requires a preview that actually rendered, and delivery
	// falls back to oto's built-in card if Slack refuses the payload. Those two
	// gates are what "heavily gated" means here.
	FormatRaw Format = "raw"
)

// Valid reports whether f is a format oto knows.
func (f Format) Valid() bool {
	switch f {
	case FormatCard, FormatText, FormatRaw:
		return true
	}
	return false
}

// Portable reports whether a template in this format can be sent to a channel
// of any provider.
//
// ⭐ A NON-PORTABLE TEMPLATE POINTED AT THE WRONG PROVIDER IS NOT AN ERROR oto
// PREVENTS. A policy fans out to as many as sixteen channels and they need not
// share a provider; matching them up is the operator's job and oto only warns. What
// oto does guarantee is that the mismatch degrades to the built-in card rather
// than dropping the alert.
func (f Format) Portable() bool { return f == FormatCard || f == FormatText }

// laxOnce and laxEngine build the one engine, once.
var (
	laxOnce   sync.Once
	laxEngine *liquid.Engine
)

// engine is the DELIVERY and AUTHORING engine both.
//
// ⛔ NewBasicEngine SHIPS ZERO TAGS AND ZERO FILTERS AND THAT IS WHY IT WAS
// CHOSEN. Every construct a template can use is one oto registered by name, so
// the curated set is the WHOLE surface rather than a subset of somebody else's
// that has to be re-audited on each upgrade.
//
// ⚠️ AN UNKNOWN VARIABLE RENDERS EMPTY, DELIBERATELY. A missing field must
// degrade one line rather than kill a delivery at 03:00. Strict mode cannot be
// the authoring gate either — it fires BEFORE filters, so `{{ alert.nmae |
// default: "-" }}` errors and `default` never runs, and it cannot tell a typo
// from a field a digest legitimately lacks. The read-set probe in readset.go is
// the typo gate instead.
func engineOf() *liquid.Engine {
	laxOnce.Do(func() {
		laxEngine = liquid.NewBasicEngine()
		registerFilters(laxEngine)
		registerTags(laxEngine)
	})
	return laxEngine
}

// strictOnce and strictEngine build the PROBE engine, once.
var (
	strictOnce   sync.Once
	strictEngine *liquid.Engine
)

// strictly is the READ-SET PROBE engine, and it is used for nothing else.
//
// ⛔ IT IS NOT THE AUTHORING GATE AND IT CANNOT BE. StrictVariables fires BEFORE
// filters run, so `{{ alert.nmae | default: "-" }}` errors and `default` never
// gets its chance — and strict cannot tell a misspelling from a field a digest
// legitimately lacks. readset.go uses it to enumerate which names a template
// reads, by rendering against a MAXIMAL view and planting whatever it complains
// about. Absence from that view means the name does not exist; absence from one
// card does not.
func strictly() *liquid.Engine {
	strictOnce.Do(func() {
		strictEngine = liquid.NewBasicEngine()
		strictEngine.StrictVariables()
		registerFilters(strictEngine)
		registerTags(strictEngine)
	})
	return strictEngine
}

// A Template is one compiled notification template.
type Template struct {
	Format   Format
	Source   string
	compiled *liquid.Template
}

// Compile parses src for delivery. It does NOT prove the template renders — an
// unknown filter is a render-time error in Liquid, not a parse-time one, which
// is exactly why Validate exists and why saving must execute rather than parse.
func Compile(f Format, src string) (*Template, error) {
	if !f.Valid() {
		return nil, fmt.Errorf("%q is not a template format", string(f))
	}
	if len(src) > MaxSourceBytes {
		return nil, fmt.Errorf("template is %d bytes and the limit is %d", len(src), MaxSourceBytes)
	}
	if d := forDepth(src); d > MaxForDepth {
		return nil, fmt.Errorf("`{%% for %%}` is nested %d deep and the limit is %d", d, MaxForDepth)
	}
	if msg := unbalanced(src); msg != "" {
		return nil, errors.New(msg)
	}
	// The SOURCE is sanitised, not only the values interpolated into it: a
	// literal private-use codepoint typed into a template body is precisely how
	// an author would otherwise forge one of oto's handles.
	src = sanitise(src)
	t, err := engineOf().ParseString(src)
	if err != nil {
		return nil, fmt.Errorf("template does not parse: %w", err)
	}
	return &Template{Format: f, Source: src, compiled: t}, nil
}

// expand runs Liquid and nothing else.
func (t *Template) expand(in Input) (string, error) {
	if t == nil || t.compiled == nil {
		return "", errors.New("template is not compiled")
	}
	out, err := t.compiled.Render(withBudget(in))
	if err != nil {
		return "", fmt.Errorf("template did not render: %s", liquidMessage(err))
	}
	if len(out) > MaxOutputBytes {
		return "", fmt.Errorf("template rendered %d bytes and the limit is %d", len(out), MaxOutputBytes)
	}
	return string(out), nil
}

// RenderCard renders a `card` template to the IR. links resolves oto's link
// handles; BuildInput and this call must be given the same table.
func (t *Template) RenderCard(in Input, links map[string]string) (*Document, []Problem) {
	// ⛔ THE FORMAT IS CHECKED, NOT ASSUMED, AND THE FIELD EXISTS FOR THIS. A
	// caller holds a compiled template and a format from two different places —
	// the row and the request — and rendering `raw` Block Kit as if it were
	// Markdown would parse a JSON document into prose and send it. Cheap guard,
	// and the alternative is a field nothing reads.
	if t.Format != FormatCard {
		return nil, []Problem{{Kind: ProblemRender, Message: fmt.Sprintf(
			"this template is %q and cannot be rendered as a card", string(t.Format))}}
	}
	raw, err := t.expand(in)
	if err != nil {
		return nil, []Problem{{Kind: ProblemRender, Message: err.Error()}}
	}
	doc, probs := Parse(raw, links)
	if len(probs) > 0 {
		return nil, probs
	}
	if len(doc.Blocks) == 0 {
		return nil, []Problem{{Kind: ProblemEmpty, Message: "the template rendered no blocks at all"}}
	}
	if len(doc.Blocks) > MaxBlocks {
		return nil, []Problem{{Kind: ProblemTooLong, Message: fmt.Sprintf(
			"the template rendered %d blocks and the limit is %d", len(doc.Blocks), MaxBlocks)}}
	}
	return doc, nil
}

// RenderText renders a `text` template to one string, spelled by d.
func (t *Template) RenderText(in Input, d Dialect, links map[string]string) (string, error) {
	if t.Format != FormatText {
		return "", fmt.Errorf("this template is %q and cannot be rendered as one line", string(t.Format))
	}
	raw, err := t.expand(in)
	if err != nil {
		return "", err
	}
	// A text template has no IR to escape through, so it takes the same walk the
	// old design used: markup written straight through, text runs escaped.
	out := strings.TrimSpace(Spell(d, raw, links))
	if out == "" {
		return "", errors.New("the template rendered nothing at all")
	}
	return out, nil
}

// RenderRaw renders a `raw` template and proves the result is JSON.
//
// ⛔ IT PROVES SHAPE, NOT VALIDITY. Whether these are blocks Slack will accept
// is `render/slack`'s question, answered by the same Validate every built-in
// card passes. This only refuses the case where interpolation broke the JSON
// itself — an unescaped quote in a label, which is the overwhelmingly common way
// a raw template fails.
func (t *Template) RenderRaw(in Input) (json.RawMessage, error) {
	if t.Format != FormatRaw {
		return nil, fmt.Errorf("this template is %q and carries no payload of its own", string(t.Format))
	}
	raw, err := t.expand(in)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(raw)
	if !json.Valid([]byte(trimmed)) {
		return nil, errors.New("the template rendered text that is not valid JSON; " +
			"a value interpolated into a JSON string almost certainly needs `| json`")
	}
	return json.RawMessage(trimmed), nil
}

// compiledCache keeps parsed templates across deliveries.
//
// ⛔ BOUNDED BY WHAT A SAVED TEMPLATE CAN BE, NOT BY A SIZE LIMIT, and that is
// only sound because nothing reaches it before the save-time gate: the key space
// is the set of templates an org has persisted, which is small and administrator
// -controlled. Preview does NOT use this path.
var compiledCache sync.Map // format + "\x00" + source -> *Template or error

// Compiled returns a parsed Template, reusing the parse across deliveries.
func Compiled(f Format, src string) (*Template, error) {
	key := string(f) + "\x00" + src
	if hit, ok := compiledCache.Load(key); ok {
		switch v := hit.(type) {
		case *Template:
			return v, nil
		case error:
			return nil, v
		}
		return nil, errors.New("compiled template cache holds an unexpected type")
	}
	t, err := Compile(f, src)
	if err != nil {
		compiledCache.Store(key, err)
		return nil, err
	}
	compiledCache.Store(key, t)
	return t, nil
}

// forDepth returns the deepest `{% for %}` nesting in src.
func forDepth(src string) int {
	depth, deepest := 0, 0
	for i := 0; i+1 < len(src); i++ {
		if src[i] != '{' || src[i+1] != '%' {
			continue
		}
		j := strings.Index(src[i:], "%}")
		if j < 0 {
			break
		}
		tag := strings.TrimSpace(src[i+2 : i+j])
		tag = strings.TrimPrefix(tag, "-")
		switch {
		case strings.HasPrefix(tag, "for "), tag == "for":
			depth++
			if depth > deepest {
				deepest = depth
			}
		case strings.HasPrefix(tag, "endfor"):
			if depth > 0 {
				depth--
			}
		}
		i += j
	}
	return deepest
}

// Validate is the SAVE-TIME gate.
//
// ⛔ IT RENDERS. Parsing proves almost nothing — `{{ x | no_such_filter }}`
// parses cleanly and fails at render — so saving executes the template against a
// fixture corpus that includes the hostile cases, and reports what actually
// happened rather than what the source looked like.
//
// ⚠️ IT RETURNS WARNINGS ALONGSIDE REFUSALS. Call Blocking() to tell them apart:
// a missing action row is reported and does not stop the save, because the operator
// is allowed to choose that.
func Validate(f Format, src string) []Problem {
	if !f.Valid() {
		return []Problem{{Kind: ProblemParse, Message: fmt.Sprintf("%q is not a template format", string(f))}}
	}
	if len(src) > MaxSourceBytes {
		return []Problem{{Kind: ProblemTooLong, Message: fmt.Sprintf(
			"this template is %d bytes and the limit is %d", len(src), MaxSourceBytes)}}
	}
	if msg := unbalanced(src); msg != "" {
		return []Problem{{Kind: ProblemParse, Message: msg}}
	}
	clean := sanitise(src)
	t, err := Compile(f, clean)
	if err != nil {
		return []Problem{{Kind: ProblemParse, Message: liquidMessage(err)}}
	}

	var out []Problem
	// The typo gate. It runs before the fixtures because "you misspelled
	// alert.nmae" is a better sentence than three fixtures each rendering a
	// blank line.
	for _, name := range unknownFields(clean) {
		out = append(out, Problem{
			Kind:    ProblemUnknownField,
			Message: name + " is not a field oto can give you",
		})
	}
	if len(out) > 0 {
		return out
	}

	sawActions := false
	for _, fx := range Fixtures() {
		in, links := fx.Bind(f)
		switch f {
		case FormatCard:
			doc, probs := t.RenderCard(in, links)
			for _, p := range probs {
				p.Fixture = fx.Name
				out = append(out, p)
			}
			if doc != nil && doc.HasActions {
				sawActions = true
			}
		case FormatText:
			if _, err := t.RenderText(in, SlackDialect{}, links); err != nil {
				out = append(out, Problem{Kind: ProblemRender, Fixture: fx.Name, Message: liquidMessage(err)})
			}
		case FormatRaw:
			if _, err := t.RenderRaw(in); err != nil {
				out = append(out, Problem{Kind: ProblemRender, Fixture: fx.Name, Message: liquidMessage(err)})
			}
		}
		if len(out) >= 10 {
			break
		}
	}

	// ⭐ THE WARNING THE OPERATOR ASKED FOR. A card template may leave out the
	// action row — that is a deliberate choice and oto does not overrule it —
	// but the alert then carries no acknowledge button, and the only remaining
	// way to acknowledge is `POST /api/v1/cases/{id}/ack` or the console. That
	// is a degraded card, not a lost alert, and it is worth exactly one sentence
	// at the moment somebody can still change their mind.
	if f == FormatCard && !sawActions && !Blocking(out) {
		out = append(out, Problem{
			Kind: ProblemWarning,
			Message: "this template has no `{{ actions }}`, so its cards carry no Acknowledge or Snooze " +
				"button. Alerts can still be acknowledged from the console or the API. Add `{{ actions }}` " +
				"on its own line to put the buttons back.",
		})
	}
	return out
}

// ProblemKind classifies a save-time refusal.
type ProblemKind string

// The reasons a template is refused at save time.
const (
	ProblemUnknownField ProblemKind = "unknown_field"
	ProblemParse        ProblemKind = "parse"
	ProblemRender       ProblemKind = "render"
	ProblemEmpty        ProblemKind = "empty"
	ProblemTooLong      ProblemKind = "too_long"
	// ProblemUnsupported is Markdown oto deliberately does not carry — a table,
	// a nested list, an image. It is separate from ProblemParse because the
	// author did not make a mistake: they wrote valid Markdown that no provider
	// can render, and the message has to say so rather than imply a typo.
	ProblemUnsupported ProblemKind = "unsupported"
	// ProblemWarning is the one kind that does NOT refuse a save.
	//
	// ⭐ IT EXISTS BECAUSE THE OPERATOR MAY OMIT THE ACTION ROW. That is their
	// decision to make and oto does not overrule it, but a card with no
	// acknowledge button is a real loss and nobody should discover it in
	// production. So it is said, loudly, at save and in preview, and the save
	// proceeds.
	ProblemWarning ProblemKind = "warning"
)

// Problem is one reason a template was refused, in words meant for the person who
// typed it.
type Problem struct {
	Kind    ProblemKind
	Fixture string
	Message string
}

func (p Problem) Error() string { return string(p.Kind) + ": " + p.Message }

// Blocking reports whether this problem stops a save.
func (p Problem) Blocking() bool { return p.Kind != ProblemWarning }

// Blocking reports whether any problem in ps stops a save.
func Blocking(ps []Problem) bool {
	for _, p := range ps {
		if p.Blocking() {
			return true
		}
	}
	return false
}

// liquidMessage strips the library's "Liquid error: " prefix, which tells an oto
// operator nothing they can act on.
func liquidMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "Liquid error: ")
}

// unbalanced reports a malformed Liquid delimiter structure, in words, or "" when
// the template is well formed.
//
// ⛔ LIQUID DOES NOT DO THIS FOR US, AND THE FAILURE IS SILENT. `{{ alert.name`
// with no closing braces does NOT fail to parse: the library treats the unclosed
// run as ordinary literal text, so the template "succeeds" and renders
// `{{ alert.name` onto the card. Every other malformation surfaces as an error and
// falls back to oto's own text; this one produces a plausible-looking card with
// template syntax printed on it, and nothing anywhere reports it.
//
// ⛔ IT IS A SCAN AND NOT A COUNT, AND THE COUNTEREXAMPLE IS WHY. This started as
// `strings.Count("{{") - strings.Count("}}")`, which reads `}}{{ alert.name }}{{`
// as perfectly balanced — two of each — while the card renders `}}OtoSmokeTest{{`.
// Counting cannot see ORDER, and order is the whole property. `%}{%` and `}}{{`
// are the same trick with fewer characters.
//
// ⚠️ IT CHECKS THE SOURCE, NOT THE OUTPUT, ON PURPOSE. Scanning rendered text for
// "{{" would also fire on an alert whose annotation legitimately contains a PromQL
// or Go-template snippet, and punish the data for the template's mistake.
func unbalanced(src string) string {
	type opener struct {
		close string
		what  string
	}
	var open *opener
	for i := 0; i < len(src); i++ {
		two := ""
		if i+2 <= len(src) {
			two = src[i : i+2]
		}
		switch {
		case open == nil && (two == "{{" || two == "{%"):
			if two == "{{" {
				open = &opener{"}}", "an expression"}
			} else {
				open = &opener{"%}", "a tag"}
			}
			i++
		case open == nil && (two == "}}" || two == "%}"):
			return "a stray " + two + " with no " + openerFor(two) + " before it — " +
				"liquid would print it on the card as literal text rather than fail"
		case open != nil && two == open.close:
			open = nil
			i++
		case open != nil && (two == "{{" || two == "{%"):
			return "a " + two + " inside " + open.what + " that is still open — " +
				"liquid does not nest delimiters and would print this as literal text"
		}
	}
	if open != nil {
		return "unclosed " + open.what + ": no matching " + open.close +
			" — liquid would print this on the card as literal text rather than fail"
	}
	return ""
}

func openerFor(closer string) string {
	if closer == "%}" {
		return "{%"
	}
	return "{{"
}
