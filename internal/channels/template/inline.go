package template

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Inline parsing: emphasis, code, links and time handles.
//
// ⚠️ THIS IS A DELIBERATELY SMALL SUBSET OF MARKDOWN'S INLINE GRAMMAR. CommonMark
// spends pages on emphasis flanking rules; oto spends one function, and where the
// two disagree oto keeps the text and drops the emphasis. A message about a
// production signal has no stake in whether `a*b*c` is italic.

// inline parses one line of already-rendered card text into spans.
func (p *parser) inline(s string) []Span { return p.inlineAt(s, 0) }

func (p *parser) inlineAt(s string, depth int) []Span {
	if depth > maxNesting {
		// The words survive; only the styling stops. Refusing here would
		// discard content over a formatting detail.
		return []Span{{Kind: SpanText, Text: stripDelims(s)}}
	}
	var out []Span
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, Span{Kind: SpanText, Text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			// An escape is how an interpolated value carries a character that
			// would otherwise be syntax. escapeMarkdown wrote it; this consumes
			// it. The pair collapses to the literal, which is why a label
			// containing `*` can never open emphasis.
			lit.WriteByte(s[i+1])
			i += 2

		case strings.HasPrefix(s[i:], linkOpen):
			flush()
			i = p.linkHandle(s, i, depth, &out)

		case isMarkByte(s, i):
			// ⭐ A FILTER'S EMPHASIS ARRIVES AS A MARK, NOT AS MARKDOWN, AND THE
			// PARSER TAKES BOTH. `{{ x | bold }}` has to mean the same thing in a
			// card and in a one-liner, and the two paths do not share a syntax:
			// the card is parsed as Markdown and the one-liner is walked by Spell.
			// A neutral mark is the one spelling both understand, so the filters
			// emit that and each path resolves it — here, into a real span.
			flush()
			i = p.markSpan(s, i, depth, &out)

		case strings.HasPrefix(s[i:], timeOpen):
			flush()
			i = p.timeHandle(s, i, &out)

		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				out = append(out, Span{Kind: SpanCode, Text: s[i+1 : i+1+j]})
				i += j + 2
				continue
			}
			lit.WriteByte(s[i])
			i++

		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "~~"):
			d := s[i : i+2]
			mark := MarkBold
			if d == "~~" {
				mark = MarkStrike
			}
			if j := strings.Index(s[i+2:], d); j > 0 {
				flush()
				out = append(out, Span{Kind: SpanEmphasis, Mark: mark,
					Children: p.inlineAt(s[i+2:i+2+j], depth+1)})
				i += j + 4
				continue
			}
			lit.WriteString(d)
			i += 2

		case s[i] == '*' || s[i] == '_':
			d := s[i]
			if j := strings.IndexByte(s[i+1:], d); j > 0 {
				flush()
				out = append(out, Span{Kind: SpanEmphasis, Mark: MarkItalic,
					Children: p.inlineAt(s[i+1:i+1+j], depth+1)})
				i += j + 2
				continue
			}
			lit.WriteByte(d)
			i++

		case s[i] == '[':
			if n, ok := p.bracketLink(s, i, depth, &out, &lit, flush); ok {
				i = n
				continue
			}
			lit.WriteByte(s[i])
			i++

		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return out
}

// isMarkByte reports whether an emphasis mark begins at s[i].
func isMarkByte(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	_, ok := markMeta(r)
	return ok && isMarkOpener(r)
}

func isMarkOpener(r rune) bool {
	switch r {
	case markCodeOpen, markStrikeOpen, markBoldOpen, markItalicOpen:
		return true
	}
	return false
}

func markCloser(r rune) rune {
	switch r {
	case markCodeOpen:
		return markCodeClose
	case markStrikeOpen:
		return markStrikeClose
	case markBoldOpen:
		return markBoldClose
	}
	return markItalicClose
}

// markSpan consumes one filter-emitted emphasis run.
func (p *parser) markSpan(s string, i, depth int, out *[]Span) int {
	open, w := utf8.DecodeRuneInString(s[i:])
	kind, _ := markMeta(open)
	shut := string(markCloser(open))
	rest := s[i+w:]
	j := strings.Index(rest, shut)
	if j < 0 {
		// Truncation cut the run in half. Keep the words, drop the styling.
		return i + w
	}
	body := rest[:j]
	next := i + w + j + len(shut)
	if kind == MarkCode {
		// A code span is literal, so its body is not parsed further — which is
		// also what stops `{{ x | code }}` from being a way to smuggle syntax.
		*out = append(*out, Span{Kind: SpanCode, Text: stripDelims(body)})
		return next
	}
	*out = append(*out, Span{Kind: SpanEmphasis, Mark: kind, Children: p.inlineAt(body, depth+1)})
	return next
}

// linkHandle resolves an oto-minted link handle to a real address.
func (p *parser) linkHandle(s string, i, depth int, out *[]Span) int {
	rest := s[i+len(linkOpen):]
	j := strings.Index(rest, linkShut)
	if j < 0 {
		// An unterminated handle cannot happen from oto's own binding, so it
		// means a truncation cut one in half. Dropping the marker and keeping
		// the rest is the only non-destructive answer — the old design deleted
		// the tail of the line here, which lost real content.
		return i + len(linkOpen)
	}
	key := rest[:j]
	next := i + len(linkOpen) + j + len(linkShut)
	addr, ok := p.links[key]
	if !ok || addr == "" {
		// oto minted a handle for a link this view does not have. The words
		// that stood for it, if any, are somewhere after; nothing to emit.
		return next
	}
	// `[text](handle)` is handled by bracketLink; a bare handle stands for
	// itself and takes the address as its own text.
	*out = append(*out, Span{Kind: SpanLink, Addr: addr,
		Children: []Span{{Kind: SpanText, Text: addr}}})
	_ = depth
	return next
}

func (p *parser) timeHandle(s string, i int, out *[]Span) int {
	rest := s[i+len(timeOpen):]
	j := strings.Index(rest, timeShut)
	if j < 0 {
		return i + len(timeOpen)
	}
	body := rest[:j]
	next := i + len(timeOpen) + j + len(timeShut)
	k := strings.Index(body, handleSep)
	if k < 0 {
		return next
	}
	sec, err := strconv.ParseInt(body[:k], 10, 64)
	if err != nil {
		*out = append(*out, Span{Kind: SpanText, Text: body[k+len(handleSep):]})
		return next
	}
	*out = append(*out, Span{Kind: SpanTime, Unix: sec, Fallback: body[k+len(handleSep):]})
	return next
}

// bracketLink parses `[text](target)`.
//
// ⛔ THE TARGET MUST BE A HANDLE. An author who writes a literal URL there gets
// their text and their URL as PROSE, not as a link — and the URL then meets
// DefuseLink like any other bare address. This is the whole reason links are
// bound as handles: after Liquid has flattened everything into one string,
// `https://oto.internal/case/1` and `https://evil.example/phish` are the same
// kind of thing, and only a handle proves oto minted it.
func (p *parser) bracketLink(s string, i, depth int, out *[]Span, lit *strings.Builder, flush func()) (int, bool) {
	close := strings.IndexByte(s[i:], ']')
	if close < 0 || i+close+1 >= len(s) || s[i+close+1] != '(' {
		return 0, false
	}
	open := i + close + 1
	shut := strings.IndexByte(s[open:], ')')
	if shut < 0 {
		return 0, false
	}
	text := s[i+1 : i+close]
	target := s[open+1 : open+shut]
	next := open + shut + 1

	// ⚠️ AN EMPTY TARGET IS NOT AN ERROR. `{{ links.runbook }}` renders to
	// nothing on a view that has no runbook, and refusing the template for that
	// would refuse it for a card it renders perfectly well. Keep the words, drop
	// the link — the same degradation an absent handle gets below.
	if strings.TrimSpace(target) == "" {
		flush()
		*out = append(*out, p.inlineAt(text, depth+1)...)
		return next, true
	}

	key, ok := handleKey(target)
	if !ok {
		p.fail(ProblemUnsupported, "a link target must be one of oto's own links, and "+
			quoteShort(target)+" is not. Write `[text]({{ links.case }})`; a literal address is shown as text.")
		return 0, false
	}
	addr, found := p.links[key]
	if !found || addr == "" {
		// The link does not exist for this view — a digest has no single case.
		// Keep the words, drop the link.
		flush()
		*out = append(*out, p.inlineAt(text, depth+1)...)
		return next, true
	}
	flush()
	*out = append(*out, Span{Kind: SpanLink, Addr: addr, Children: p.inlineAt(text, depth+1)})
	_ = lit
	return next, true
}

// handleKey reports whether target is exactly one link handle, and its key.
func handleKey(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, linkOpen) || !strings.HasSuffix(target, linkShut) {
		return "", false
	}
	key := target[len(linkOpen) : len(target)-len(linkShut)]
	if key == "" || strings.Contains(key, linkOpen) || strings.Contains(key, linkShut) {
		return "", false
	}
	return key, true
}

// stripDelims removes emphasis punctuation from text whose styling was given up.
func stripDelims(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		switch s[i] {
		case '*', '_', '~', '`':
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// escapeMarkdown neutralises every character this parser treats as syntax.
//
// ⭐ IT IS APPLIED TO EVERY INTERPOLATED VALUE, UNCONDITIONALLY, AND THERE IS NO
// WAY TO OPT OUT. That is the whole reason the `card` format needs no raw-output
// mechanism and no taint tracking: an alert label is attacker-influenced —
// anyone who can fire a metric can write one — and if a value cannot produce
// syntax, it cannot produce structure, a link, a mention, or a forged handle.
// An author who genuinely needs raw markup uses `format=raw`, which is gated.
func escapeMarkdown(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for _, r := range s {
		switch r {
		case '\\', '`', '*', '_', '~', '[', ']', '(', ')', '#', '>', '|', ':', '-', '+':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
