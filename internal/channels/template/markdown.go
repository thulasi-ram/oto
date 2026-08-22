package template

import (
	"strconv"
	"strings"
)

// Parsing a rendered `card` template into the IR.
//
// ⭐ THE INPUT IS MARKDOWN BECAUSE THAT IS ALREADY WHAT A SLACK OR DISCORD USER
// TYPES. It is not a new markup to learn; it is the one they use every day in
// the very product this message is going to. HTML was the alternative and it
// loses on surface area: we would support ten tags and users would expect all
// of them.
//
// ⛔ HANDLES ARE THE SECURITY BOUNDARY, NOT THE SYNTAX. An oto-issued link and
// an author-typed URL are indistinguishable once Liquid has rendered them into
// one flat string, so oto does not put URLs into the binding at all — it puts
// unforgeable private-use handles, and resolves them here. sanitise() strips the
// whole private-use area from the template source AND from every interpolated
// value before any of this runs, so the only handles that can reach the parser
// are ones oto minted after sanitising. `[text](https://evil.example)` therefore
// has no handle, and is treated as the prose it is.
const (
	// ⛔ WRITTEN AS ESCAPES, NEVER AS LITERAL CHARACTERS. A literal private-use
	// codepoint in a Go source file is invisible in every editor and diff, and
	// gosec's G116 flags the class for good reason. An escape is greppable and
	// reviewable; the byte it produces is identical.
	//
	// ⭐ THE RUNE AND STRING FORMS ARE THE SAME CODEPOINTS ON PURPOSE. The card
	// path scans bytes and the `text` path scans runes, and a handle that meant
	// two different things to the two of them would be a hole exactly where the
	// two formats disagree.
	actionsRune  = '\uE100'
	linkOpenRune = '\uE101'
	linkShutRune = '\uE102'

	sentinelActions = string(actionsRune)
	linkOpen        = string(linkOpenRune)
	linkShut        = string(linkShutRune)

	// Time reuses the marks the Dialect layer already spells, so `{{ x | datetime }}`
	// means one thing in a card and the same thing in a one-liner.
	timeOpen  = string(markTimeOpen)
	timeShut  = string(markTimeClose)
	handleSep = string(markTimeSep)
)

// maxNesting bounds inline emphasis depth. Unbounded nesting is a stack the
// author controls, and `Parse` is reached by a save-time preview before it is
// ever reached by a delivery.
const maxNesting = 8

// Parse turns a rendered card into a Document, or returns the problems that
// stopped it. links resolves a handle key to the address oto minted for it.
func Parse(src string, links map[string]string) (*Document, []Problem) {
	p := &parser{lines: strings.Split(src, "\n"), links: links}
	p.blocks()
	if len(p.problems) > 0 {
		return nil, p.problems
	}
	return &Document{Blocks: p.out, HasActions: p.hasActions}, nil
}

type parser struct {
	lines      []string
	links      map[string]string
	out        []Block
	problems   []Problem
	hasActions bool
	i          int
}

func (p *parser) fail(kind ProblemKind, msg string) {
	if len(p.problems) >= 10 {
		return
	}
	p.problems = append(p.problems, Problem{Kind: kind, Message: msg})
}

func (p *parser) blocks() {
	for p.i < len(p.lines) {
		raw := p.lines[p.i]
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			p.i++
		case line == sentinelActions:
			// The actions row is a block, not prose, and it may appear once.
			if p.hasActions {
				p.fail(ProblemParse, "`{{ actions }}` appears more than once; a card has one row of buttons")
				p.i++
				continue
			}
			p.hasActions = true
			p.out = append(p.out, Block{Kind: BlockActions})
			p.i++
		case isDivider(line):
			p.out = append(p.out, Block{Kind: BlockDivider})
			p.i++
		case strings.HasPrefix(line, ":::"):
			p.fields()
		case strings.HasPrefix(line, "|"):
			p.fail(ProblemUnsupported, "tables are not supported: Slack has no table block and a table degrades to unreadable prose. Use a `:::fields` grid instead.")
			p.i++
		case strings.HasPrefix(line, "#"):
			p.heading(line)
		case strings.HasPrefix(line, ">"):
			p.quote()
		case isListMarker(line) && indentOf(raw) == 0:
			p.list()
		case isListMarker(line) && indentOf(raw) > 0:
			p.fail(ProblemUnsupported, "nested lists are not supported: no provider renders a second level the same way. Use one flat list, or a `:::fields` grid.")
			p.i++
		default:
			p.paragraph()
		}
	}
}

func (p *parser) heading(line string) {
	depth := 0
	for depth < len(line) && line[depth] == '#' {
		depth++
	}
	text := strings.TrimSpace(line[depth:])
	p.i++
	if text == "" {
		return
	}
	spans := p.inline(text)
	if depth == 1 {
		p.out = append(p.out, Block{Kind: BlockHeading, Inline: spans})
		return
	}
	// ⚠️ `##` DEGRADES RATHER THAN REFUSING. Slack has exactly one heading
	// level, so a second one has nowhere to go — but `##` is muscle memory and
	// refusing it would teach nothing. Bold prose is the honest approximation.
	p.out = append(p.out, Block{Kind: BlockParagraph, Inline: []Span{{
		Kind: SpanEmphasis, Mark: MarkBold, Children: spans,
	}}})
}

func (p *parser) quote() {
	var body []string
	for p.i < len(p.lines) {
		line := strings.TrimSpace(p.lines[p.i])
		if !strings.HasPrefix(line, ">") {
			break
		}
		body = append(body, strings.TrimSpace(strings.TrimPrefix(line, ">")))
		p.i++
	}
	text := strings.TrimSpace(strings.Join(body, " "))
	if text == "" {
		return
	}
	p.out = append(p.out, Block{Kind: BlockQuote, Inline: p.inline(text)})
}

func (p *parser) list() {
	var items [][]Span
	for p.i < len(p.lines) {
		raw := p.lines[p.i]
		line := strings.TrimSpace(raw)
		if line == "" || !isListMarker(line) || indentOf(raw) > 0 {
			break
		}
		text := strings.TrimSpace(line[1:])
		p.i++
		if text == "" {
			continue
		}
		items = append(items, p.inline(text))
	}
	if len(items) > 0 {
		p.out = append(p.out, Block{Kind: BlockList, Items: items})
	}
}

// fields parses the `:::fields` extension.
//
// ⭐ IT IS THE ONE BLOCK MARKDOWN HAS NO SYNTAX FOR AND BLOCK KIT DOES NATIVELY.
// Expressing it as a Markdown table would have meant supporting tables, which
// no provider renders; a fenced block borrows a syntax people already know from
// admonitions and costs one branch.
func (p *parser) fields() {
	open := strings.TrimSpace(p.lines[p.i])
	if open != ":::fields" {
		p.fail(ProblemUnsupported, "the only fenced block is `:::fields`; `"+open+"` is not a block oto knows")
		p.i++
		return
	}
	p.i++
	var rows []Field
	closed := false
	for p.i < len(p.lines) {
		line := strings.TrimSpace(p.lines[p.i])
		p.i++
		if line == ":::" {
			closed = true
			break
		}
		if line == "" {
			continue
		}
		label, value, ok := splitRow(line)
		if !ok {
			p.fail(ProblemParse, "a `:::fields` row is `label | value`, and this row has no `|`: "+quoteShort(line))
			continue
		}
		rows = append(rows, Field{Label: p.inline(label), Value: p.inline(value)})
	}
	if !closed {
		p.fail(ProblemParse, "a `:::fields` block was opened and never closed with `:::`")
		return
	}
	if len(rows) > 0 {
		p.out = append(p.out, Block{Kind: BlockFields, Fields: rows})
	}
}

func (p *parser) paragraph() {
	var body []string
	for p.i < len(p.lines) {
		raw := p.lines[p.i]
		line := strings.TrimSpace(raw)
		if line == "" || startsBlock(raw, line) {
			break
		}
		body = append(body, line)
		p.i++
	}
	text := strings.TrimSpace(strings.Join(body, " "))
	if text == "" {
		return
	}
	p.out = append(p.out, Block{Kind: BlockParagraph, Inline: p.inline(text)})
}

func startsBlock(raw, line string) bool {
	switch {
	case line == sentinelActions, isDivider(line):
		return true
	case strings.HasPrefix(line, ":::"), strings.HasPrefix(line, "#"), strings.HasPrefix(line, ">"), strings.HasPrefix(line, "|"):
		return true
	case isListMarker(line) && indentOf(raw) == 0:
		return true
	}
	return false
}

func isDivider(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(s, string(c)) == ""
}

func isListMarker(s string) bool {
	if len(s) < 2 {
		return false
	}
	return (s[0] == '-' || s[0] == '*' || s[0] == '+') && s[1] == ' '
}

func indentOf(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

func splitRow(s string) (label, value string, ok bool) {
	i := strings.Index(s, "|")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
}

func quoteShort(s string) string {
	const n = 60
	if len(s) > n {
		s = s[:n] + "…"
	}
	return strconv.Quote(s)
}
