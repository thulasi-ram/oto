package wording

import (
	"strconv"
	"strings"
	"time"
	"unicode"
)

// A mark is a neutral emphasis instruction a Wording's filters emit, which a
// Dialect spells in one provider's syntax on the way out.
//
// ⛔ THE MARKS ARE PRIVATE-USE CODEPOINTS AND THAT IS THE MECHANISM, NOT A STYLE.
// A Wording must not be able to forge one, because forging a mark is how a
// customer would get raw markup — and eventually a mention — past the sink. So
// every interpolated value and every literal has the whole private-use area
// stripped from it (see sanitise) BEFORE any filter runs, which means the only
// codepoints in this range that can reach a Dialect are ones oto's own filters
// put there.
//
// ⛔ AND THEY MUST NEVER APPEAR IN OUTPUT. A Dialect that does not recognise a
// mark drops it; markStripper removes any that survive. A private-use codepoint
// reaching Slack would render as a replacement glyph on some clients and nothing
// on others, which is the worst kind of bug: invisible to the author, visible to
// exactly one reader.
const (
	markCodeOpen    = ''
	markCodeClose   = ''
	markStrikeOpen  = ''
	markStrikeClose = ''
	markBoldOpen    = ''
	markBoldClose   = ''
	markItalicOpen  = ''
	markItalicClose = ''

	// A timestamp mark wraps `<unix>|<fallback>` because a provider that can render
	// a viewer-local time needs the epoch and a provider that cannot needs a string
	// oto already formatted. Slack's <!date> token takes both; a plain-text sink
	// takes only the second.
	markTimeOpen  = ''
	markTimeClose = ''
	markTimeSep   = ''
)

// Dialect spells oto's neutral marks in one provider's syntax, and refuses that
// provider's mention spellings.
//
// ⭐ IT IS THE WHOLE ANSWER TO "each channel has their own quirks". ADR 0037 said
// a Wording is text and text is portable; that is true only once the text stops
// carrying Slack's punctuation. A provider is added by writing one of these, not by
// touching a template, a filter, or the sink.
type Dialect interface {
	// Name is the provider this dialect speaks for, for diagnostics.
	Name() string
	// Emphasis returns the opening and closing literal for one kind of mark. An
	// empty pair means "this provider cannot show it" and the emphasis is dropped
	// while the text it wrapped is kept.
	Emphasis(kind MarkKind) (open, shut string)
	// Timestamp renders an instant. fallback is oto's own pre-formatted UTC string.
	Timestamp(at time.Time, fallback string) string
	// StripAudience removes every spelling by which THIS provider addresses a
	// group of humans. It is called on the finished string, after marks are spelled.
	StripAudience(s string) string
	// EscapeText neutralises the characters THIS provider's message parser treats
	// as markup, and it is applied to the WORDS only — never to the markup oto's
	// own marks just produced.
	//
	// ⛔ THE ORDER IS THE WHOLE REASON THIS IS ON THE INTERFACE. Escaping the
	// finished string would destroy oto's own output: Slack's <!date^…> token
	// would become &lt;!date^…&gt; and render as literal garbage. Escaping before
	// rendering would be worse — the marks are inserted BY filters during the
	// render, so there is no "before" that sees them. Spell therefore escapes each
	// run of text as it walks, and writes markup through untouched.
	//
	// ⚠️ AND IT IS PER-PROVIDER BECAUSE ESCAPING IS A QUIRK LIKE ANY OTHER. A
	// webhook consumer receives a JSON string: giving it "&amp;" where the alert
	// said "&" is not safety, it is corruption of the value it will go on to
	// process.
	EscapeText(s string) string
}

// MarkKind is one emphasis a Wording can ask for.
type MarkKind int

// The emphases a Wording can ask for. Each Dialect spells them in its own syntax.
const (
	MarkCode MarkKind = iota
	MarkStrike
	MarkBold
	MarkItalic
)

func (k MarkKind) String() string {
	switch k {
	case MarkCode:
		return "code"
	case MarkStrike:
		return "strike"
	case MarkBold:
		return "bold"
	case MarkItalic:
		return "italic"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Slack
// ---------------------------------------------------------------------------

// SlackDialect spells marks as Slack mrkdwn.
//
// The spellings match what internal/channels/render/slack/text.go already emits by
// hand — `code()` uses backticks, `strike()` uses a single tilde — so a Wording and
// a built-in string are typographically indistinguishable on the card.
type SlackDialect struct{}

// Name is the provider this dialect speaks for.
func (SlackDialect) Name() string { return "slack" }

// EscapeText neutralises the three characters Slack's mrkdwn parser treats as
// control characters, exactly as slack.escape() does for upstream annotation text.
// A label value containing "<!channel>" must not become a channel-wide ping.
func (SlackDialect) EscapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Emphasis spells one mark as Slack mrkdwn.
func (SlackDialect) Emphasis(k MarkKind) (string, string) {
	switch k {
	case MarkCode:
		return "`", "`"
	case MarkStrike:
		return "~", "~"
	case MarkBold:
		// ⚠️ SLACK'S BOLD IS ONE ASTERISK. Discord's is two, and Discord reads one
		// as italic. This line is the reason marks exist.
		return "*", "*"
	case MarkItalic:
		return "_", "_"
	}
	return "", ""
}

// Timestamp emits Slack's <!date> token so every viewer sees their own timezone,
// with oto's UTC string as the fallback for clients that cannot render it (S13).
func (SlackDialect) Timestamp(at time.Time, fallback string) string {
	if at.IsZero() {
		return fallback
	}
	return "<!date^" + strconv.FormatInt(at.UTC().Unix(), 10) + "^{date_short_pretty} {time}|" + fallback + ">"
}

// StripAudience removes Slack's broadcast and subteam spellings.
//
// ⚠️ IT RUNS EVEN THOUGH escape() ALREADY NEUTRALISES THE BRACKETS, and the
// redundancy is deliberate. escape() defends the mrkdwn parser and happens to
// defeat these tokens as a side effect of turning "<" into "&lt;" — a property of
// an unrelated function that a future refactor could remove without knowing it was
// load-bearing here. ADR 0037 promises a Wording "can never emit a mention"; a
// promise that rests on a side effect is not a promise.
func (SlackDialect) StripAudience(s string) string {
	for _, tok := range []string{
		"<!channel>", "<!here>", "<!everyone>",
		"&lt;!channel&gt;", "&lt;!here&gt;", "&lt;!everyone&gt;",
		"@channel", "@here", "@everyone",
	} {
		s = replaceFold(s, tok, "")
	}
	s = stripBracketed(s, "<@", ">")
	s = stripBracketed(s, "&lt;@", "&gt;")
	s = stripBracketed(s, "<!subteam^", ">")
	s = stripBracketed(s, "&lt;!subteam^", "&gt;")
	return s
}

// ---------------------------------------------------------------------------
// Plain
// ---------------------------------------------------------------------------

// PlainDialect drops every mark and keeps the words.
//
// It is what the `webhook` provider receives, because a webhook consumer is NOT a
// degraded Slack: it is a program, and handing it another product's punctuation to
// parse is a worse contract than handing it clean text.
type PlainDialect struct{}

// Name is the provider this dialect speaks for.
func (PlainDialect) Name() string { return "plain" }

// Emphasis drops every mark: a webhook consumer is a program, not a reader.
func (PlainDialect) Emphasis(MarkKind) (string, string) { return "", "" }

// EscapeText is the identity: a webhook consumer receives a JSON string and is
// going to process the value, so handing it "&amp;" where the alert said "&" is
// not safety, it is corruption.
func (PlainDialect) EscapeText(s string) string { return s }

// Timestamp hands over oto's own UTC rendering, which a program can parse.
func (PlainDialect) Timestamp(_ time.Time, fallback string) string { return fallback }

// StripAudience removes the bare spellings that read as a broadcast in almost every
// chat product, so a webhook consumer that forwards oto's text into one cannot be
// used as a laundering step for a ping a Wording was not allowed to send.
func (PlainDialect) StripAudience(s string) string {
	for _, tok := range []string{"@channel", "@here", "@everyone", "@room"} {
		s = replaceFold(s, tok, "")
	}
	return s
}

// ---------------------------------------------------------------------------
// Spelling the marks
// ---------------------------------------------------------------------------

// Spell converts oto's neutral marks into d's syntax and then refuses d's audience
// spellings. It is the last thing that touches a Wording's output before the
// renderer's own escape-and-truncate sink.
func Spell(d Dialect, s string) string {
	if s == "" {
		return ""
	}
	var b, run strings.Builder
	b.Grow(len(s))
	// flush escapes the words accumulated so far. Markup is written straight to b,
	// bypassing this, which is the point.
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(d.EscapeText(run.String()))
			run.Reset()
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case markCodeOpen, markCodeClose, markStrikeOpen, markStrikeClose,
			markBoldOpen, markBoldClose, markItalicOpen, markItalicClose:
			flush()
			kind, opening := markMeta(r)
			open, shut := d.Emphasis(kind)
			if opening {
				b.WriteString(open)
			} else {
				b.WriteString(shut)
			}
		case markTimeOpen:
			end := indexRune(runes, i+1, markTimeClose)
			if end < 0 {
				continue // unterminated: drop the mark, keep the words
			}
			flush()
			b.WriteString(spellTime(d, string(runes[i+1:end])))
			i = end
		case markTimeClose, markTimeSep:
			// Orphaned separator. Drop it rather than print a private-use glyph.
		default:
			run.WriteRune(r)
		}
	}
	flush()
	return d.StripAudience(b.String())
}

func spellTime(d Dialect, payload string) string {
	sep := strings.IndexRune(payload, markTimeSep)
	if sep < 0 {
		return payload
	}
	unix, err := strconv.ParseInt(payload[:sep], 10, 64)
	// The fallback is oto's own formatted UTC string, but it is escaped anyway:
	// it is the one part of a time mark that becomes visible prose, and escaping
	// it costs nothing on a string oto controls.
	fallback := d.EscapeText(payload[sep+len(string(markTimeSep)):])
	if err != nil {
		return fallback
	}
	return d.Timestamp(time.Unix(unix, 0).UTC(), fallback)
}

func markMeta(r rune) (MarkKind, bool) {
	switch r {
	case markCodeOpen:
		return MarkCode, true
	case markCodeClose:
		return MarkCode, false
	case markStrikeOpen:
		return MarkStrike, true
	case markStrikeClose:
		return MarkStrike, false
	case markBoldOpen:
		return MarkBold, true
	case markBoldClose:
		return MarkBold, false
	case markItalicOpen:
		return MarkItalic, true
	}
	return MarkItalic, false
}

func indexRune(rs []rune, from int, target rune) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == target {
			return i
		}
	}
	return -1
}

// sanitise strips every codepoint a Wording is not allowed to contribute, and it
// runs on EVERY interpolated value and EVERY literal before a filter sees it.
//
// Three classes go:
//   - the private-use area, so no template can forge one of oto's marks;
//   - other-format codepoints (Cf), which is where the bidi overrides live: a
//     right-to-left override can make a rendered sentence read in an order the
//     author did not write and the reviewer did not see;
//   - control characters other than the tab and newline a card may legitimately
//     carry.
func sanitise(s string) string {
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r >= '' && r <= '':
			return -1
		case r >= 0xF0000: // supplementary private-use planes A and B
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// replaceFold removes every case-insensitive occurrence of tok.
func replaceFold(s, tok, with string) string {
	if tok == "" {
		return s
	}
	var b strings.Builder
	lower, ltok := strings.ToLower(s), strings.ToLower(tok)
	for {
		i := strings.Index(lower, ltok)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(with)
		s, lower = s[i+len(tok):], lower[i+len(ltok):]
	}
}

// stripBracketed removes open…close spans, used for the id-carrying mention forms
// whose payload is a user or group id rather than a fixed word.
func stripBracketed(s, open, shut string) string {
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+len(open):], shut)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+len(open)+j+len(shut):]
	}
}
