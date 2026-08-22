package template

import (
	"strings"
	"time"
)

// Compiling the IR to one provider's inline syntax.
//
// ⛔ THIS IS THE ONLY PLACE TEXT IS ESCAPED, AND THAT IS THE WHOLE POINT OF
// HAVING AN IR. The old design escaped runs of a finished string while walking
// past markup it had itself just emitted, which is how `<!date^…>` once became
// `&lt;!date^…&gt;`. A SpanText's Text is words and nothing else, so escaping
// it is unambiguous and happens exactly once.

// InlineTo writes spans into b in d's syntax.
func InlineTo(b *strings.Builder, d Dialect, spans []Span) {
	for _, s := range spans {
		switch s.Kind {
		case SpanText:
			// ⛔ NEUTRALISED HERE, PER SPAN, NOT OVER THE FINISHED STRING. Running
			// it at the end defuses oto's OWN links: `<https://…|text>` has a bare
			// address inside it, defuseLinks cannot tell it from one a label
			// smuggled in, and the button-shaped link came out wrapped in a code
			// span. The IR is what makes the distinction available — a SpanText is
			// prose by construction and oto's markup is never inside one.
			b.WriteString(neutralise(d, d.EscapeText(s.Text)))
		case SpanCode:
			open, shut := d.Emphasis(MarkCode)
			// A code span's content is literal in every provider, but a
			// provider's own parser still has to be able to find the closing
			// delimiter — so the escape runs, and only the emphasis is skipped.
			b.WriteString(open)
			// Audience spellings are stripped even here. A code span suppresses a
			// mention in Slack, but "some provider suppresses it" is not the same
			// claim as "no provider notifies", and the dialect layer exists
			// precisely because providers differ. Links are NOT defused: the span
			// is already inert, and wrapping it again would nest delimiters.
			b.WriteString(d.StripAudience(d.EscapeText(s.Text)))
			b.WriteString(shut)
		case SpanEmphasis:
			open, shut := d.Emphasis(s.Mark)
			// An empty pair means this provider cannot show this emphasis. The
			// words survive; only the styling is dropped. A webhook consumer
			// receiving `*firing*` where the value was `firing` would be
			// corrupted data, not degraded presentation.
			b.WriteString(open)
			InlineTo(b, d, s.Children)
			b.WriteString(shut)
		case SpanLink:
			writeLink(b, d, s)
		case SpanTime:
			b.WriteString(d.Timestamp(unixTime(s.Unix), s.Fallback))
		}
	}
}

// Inline renders spans to a string in d's syntax.
func Inline(d Dialect, spans []Span) string {
	var b strings.Builder
	InlineTo(&b, d, spans)
	return b.String()
}

// spansToPlain renders spans with no emphasis at all, for a fallback line.
func spansToPlain(d Dialect, spans []Span) string {
	var b strings.Builder
	plainTo(&b, d, spans)
	return b.String()
}

func plainTo(b *strings.Builder, d Dialect, spans []Span) {
	for _, s := range spans {
		switch s.Kind {
		case SpanText, SpanCode:
			b.WriteString(neutralise(d, d.EscapeText(s.Text)))
		case SpanEmphasis:
			plainTo(b, d, s.Children)
		case SpanLink:
			plainTo(b, d, s.Children)
		case SpanTime:
			// The fallback, never the provider's token: a fallback line is read
			// by a push notification and a screen reader, and `<!date^…>` is
			// neither of those things.
			b.WriteString(neutralise(d, d.EscapeText(s.Fallback)))
		}
	}
}

// lineWriter joins non-empty lines, so that a block which renders to nothing
// leaves no blank line behind it.
type lineWriter struct{ b strings.Builder }

func (w *lineWriter) line(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if w.b.Len() > 0 {
		w.b.WriteByte('\n')
	}
	w.b.WriteString(s)
}

func (w *lineWriter) String() string { return w.b.String() }

// writeLink spells an oto-issued link.
//
// ⛔ THE ADDRESS IS NEVER ESCAPED AND THE TEXT ALWAYS IS. An address that
// reached a SpanLink was minted by oto (the parser refuses any other), so
// escaping it would corrupt a URL oto itself built. The text is prose and may
// have come from an alert label, so it is escaped like any other prose — and
// escaped for the provider, which is what stops a `|` in a label from closing
// Slack's `<url|text>` early and spilling the rest into the address.
func writeLink(b *strings.Builder, d Dialect, s Span) {
	var t strings.Builder
	InlineTo(&t, d, s.Children)
	text := strings.TrimSpace(t.String())
	if text == "" {
		text = d.EscapeText(s.Addr)
	}
	b.WriteString(d.LinkTo(s.Addr, text))
}

func unixTime(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
