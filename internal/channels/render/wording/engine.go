package wording

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/osteele/liquid"
)

// MaxTemplateBytes bounds a Wording's SOURCE, not its output.
//
// It is not a safety limit — the sink bounds output regardless — it is a shape
// limit. ADR 0037's ceiling is "one line of prose per Stanza"; a template that does
// not fit in 2 KiB is not one line of prose, and refusing it at save time is kinder
// than letting somebody build a document inside a field that will truncate it.
const MaxTemplateBytes = 2048

var (
	strictOnce, laxOnce     sync.Once
	strictEngine, laxEngine *liquid.Engine
)

// strictly is the READ-SET PROBE engine. An unknown variable is an error, which is
// how unknownFields discovers which names a template reaches.
//
// ⚠️ IT IS NOT THE AUTHORING ENGINE, THOUGH ADR 0037 SAYS IT SHOULD BE. Strict
// fires before filters run, so `{{ alert.nmae | default: "-" }}` fails and
// `default` never gets the chance the ADR relies on for totality. Used directly as
// the save-time gate it would refuse every honest wording that names a field some
// card legitimately lacks. It is used against a MAXIMAL view instead, where absence
// means the name does not exist.
func strictly() *liquid.Engine {
	strictOnce.Do(func() {
		strictEngine = liquid.NewBasicEngine()
		registerFilters(strictEngine)
		strictEngine.StrictVariables()
	})
	return strictEngine
}

// laxly is the DELIVERY engine: an unknown variable renders empty, because a
// missing field must degrade one Stanza rather than kill a delivery at 03:00.
func laxly() *liquid.Engine {
	laxOnce.Do(func() {
		laxEngine = liquid.NewBasicEngine()
		registerFilters(laxEngine)
	})
	return laxEngine
}

// A Wording is one compiled, matcher-selected template for one Stanza.
type Wording struct {
	Stanza   StanzaID
	Source   string
	compiled *liquid.Template
}

// ErrRefusedStanza reports a Wording aimed at a Stanza that takes none.
var ErrRefusedStanza = errors.New("stanza takes no wording")

// Compile parses src for delivery. It does NOT prove the template renders — an
// unknown filter is a render-time error in Liquid, not a parse-time one, which is
// exactly why Validate exists and why saving must execute rather than merely parse.
func Compile(stanza StanzaID, src string) (*Wording, error) {
	if !stanza.Wordable() {
		return nil, fmt.Errorf("%w: %s — %s", ErrRefusedStanza, stanza, stanza.RefusalReason())
	}
	if len(src) > MaxTemplateBytes {
		return nil, fmt.Errorf("wording is %d bytes, and the limit is %d: a stanza is one line of prose",
			len(src), MaxTemplateBytes)
	}
	if msg := unbalanced(src); msg != "" {
		return nil, errors.New(msg)
	}
	// The template SOURCE is sanitised too, not only the values interpolated into
	// it. ADR 0037 says the sink strips a mention "from interpolated values *and*
	// from literals", and a literal private-use codepoint typed into a template body
	// is precisely how an author would otherwise forge one of oto's marks.
	src = sanitise(src)
	t, err := laxly().ParseString(src)
	if err != nil {
		return nil, fmt.Errorf("wording does not parse: %w", err)
	}
	return &Wording{Stanza: stanza, Source: src, compiled: t}, nil
}

// Render produces this Wording's text for one delivery, already spelled in d's
// syntax and with d's audience spellings refused.
//
// ⛔ IT NEVER RETURNS BOTH AN ERROR AND TEXT A CALLER SHOULD USE. On any failure
// the caller must fall back to oto's built-in Go string for the Stanza, which is
// what keeps ADR 0037's promise that a Wording can never mark a delivery dead.
// An empty render is a failure for the same reason: a Stanza that vanishes is a
// card telling a smaller truth with nothing to say it did.
func (w *Wording) Render(in StanzaInput, d Dialect) (string, error) {
	if w == nil || w.compiled == nil {
		return "", errors.New("wording is not compiled")
	}
	out, err := w.compiled.Render(liquid.Bindings(in))
	if err != nil {
		return "", fmt.Errorf("wording for %s did not render: %w", w.Stanza, err)
	}
	spelled := strings.TrimSpace(Spell(d, string(out)))
	if spelled == "" {
		return "", fmt.Errorf("wording for %s rendered empty", w.Stanza)
	}
	return spelled, nil
}

// Validate is the SAVE-TIME gate. It parses strictly and then RENDERS against a
// fixture corpus including the hostile cases, because parsing proves almost
// nothing: `{{ x | no_such_filter }}` parses cleanly and fails at render.
//
// It returns one Problem per distinct failure, quoting the offending expression as
// Liquid reports it. Liquid gives no line or column — accepted, since a Stanza is
// one line.
func Validate(stanza StanzaID, src string) []Problem {
	if !stanza.Wordable() {
		return []Problem{{Kind: ProblemStanza, Message: stanza.RefusalReason()}}
	}
	if len(src) > MaxTemplateBytes {
		return []Problem{{Kind: ProblemTooLong, Message: fmt.Sprintf(
			"a wording is one line of prose; this is %d bytes and the limit is %d",
			len(src), MaxTemplateBytes)}}
	}
	if msg := unbalanced(src); msg != "" {
		return []Problem{{Kind: ProblemParse, Message: msg}}
	}
	clean := sanitise(src)
	t, err := laxly().ParseString(clean)
	if err != nil {
		return []Problem{{Kind: ProblemParse, Message: liquidMessage(err)}}
	}

	seen := map[string]bool{}
	var problems []Problem

	// The typo pass. See unknownFields for why this is not StrictVariables.
	for _, path := range unknownFields(clean) {
		if seen[path] {
			continue
		}
		seen[path] = true
		problems = append(problems, Problem{
			Kind:    ProblemUnknownField,
			Message: path + " is not a field of an oto notification",
		})
	}
	for _, f := range Fixtures() {
		out, err := t.Render(liquid.Bindings(f.Input))
		if err != nil {
			msg := liquidMessage(err)
			if !seen[msg] {
				seen[msg] = true
				problems = append(problems, Problem{
					Kind: ProblemRender, Fixture: f.Name, Message: msg,
				})
			}
			continue
		}
		// A Wording that renders empty on the ORDINARY fixture is a mistake worth
		// refusing while somebody is present to fix it. On a hostile fixture it is
		// expected and is handled at delivery by falling back.
		if f.Representative && strings.TrimSpace(Spell(PlainDialect{}, string(out))) == "" {
			const msg = "renders nothing on an ordinary notification — give every field a `| default:` so the stanza always says something"
			if !seen[msg] {
				seen[msg] = true
				problems = append(problems, Problem{
					Kind: ProblemEmpty, Fixture: f.Name, Message: msg,
				})
			}
		}
	}
	return problems
}

// ProblemKind classifies a save-time refusal.
type ProblemKind string

const (
	ProblemStanza       ProblemKind = "stanza"
	ProblemUnknownField ProblemKind = "unknown_field"
	ProblemParse        ProblemKind = "parse"
	ProblemRender       ProblemKind = "render"
	ProblemEmpty        ProblemKind = "empty"
	ProblemTooLong      ProblemKind = "too_long"
)

// Problem is one reason a Wording was refused, in words meant for the person who
// typed it.
type Problem struct {
	Kind    ProblemKind
	Fixture string
	Message string
}

func (p Problem) Error() string { return string(p.Kind) + ": " + p.Message }

// liquidMessage strips the library's "Liquid error: " prefix, which tells an oto
// operator nothing they can act on.
func liquidMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "Liquid error: ")
}

// unbalanced reports an unclosed Liquid delimiter, in words, or "" when the
// template is balanced.
//
// ⛔ LIQUID DOES NOT DO THIS FOR US, AND THE FAILURE IS SILENT. `{{ alert.name`
// with no closing braces does NOT fail to parse: the library treats the unclosed
// run as ordinary literal text, so the template "succeeds" and renders the string
// `{{ alert.name` onto the card. Every other malformation this package can produce
// surfaces as an error and falls back to oto's own text; this one produces a
// plausible-looking card with template syntax printed on it, which is worse than a
// failure because nothing anywhere reports it. Found by
// TestAFailingWordingFallsBackRatherThanKillingTheCard.
//
// ⚠️ IT CHECKS THE SOURCE, NOT THE OUTPUT, ON PURPOSE. Scanning rendered text for
// "{{" would also fire on an alert whose annotation legitimately contains a
// PromQL or Go-template snippet, and punish the data for the template's mistake.
func unbalanced(src string) string {
	for _, d := range []struct{ open, close, what string }{
		{"{{", "}}", "an expression"},
		{"{%", "%}", "a tag"},
	} {
		if n := strings.Count(src, d.open) - strings.Count(src, d.close); n != 0 {
			return "unclosed " + d.what + ": " + strconv.Itoa(abs(n)) + "\u00d7 " + d.open +
				" with no matching " + d.close +
				" — liquid would print this on the card as literal text rather than fail"
		}
	}
	return ""
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
