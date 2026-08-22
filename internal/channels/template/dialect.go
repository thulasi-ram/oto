package template

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
	markCodeOpen    = '\uE000'
	markCodeClose   = '\uE001'
	markStrikeOpen  = '\uE002'
	markStrikeClose = '\uE003'
	markBoldOpen    = '\uE004'
	markBoldClose   = '\uE005'
	markItalicOpen  = '\uE006'
	markItalicClose = '\uE007'

	// A timestamp mark wraps `<unix>|<fallback>` because a provider that can render
	// a viewer-local time needs the epoch and a provider that cannot needs a string
	// oto already formatted. Slack's <!date> token takes both; a plain-text sink
	// takes only the second.
	markTimeOpen  = '\uE010'
	markTimeClose = '\uE011'
	markTimeSep   = '\uE012'
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
	// LinkTo spells an OTO-ISSUED link: an address plus the words that stand for
	// it. It is not the author's escape hatch — the parser refuses any link whose
	// target did not come from the binding's Links namespace, so every Addr that
	// reaches here was minted by oto. An author-typed URL is prose, and prose goes
	// through DefuseLink instead.
	LinkTo(addr, text string) string
	// DefuseLink makes an address unclickable without hiding it, in whatever way
	// THIS provider allows. An empty return means the provider does not linkify at
	// all and the address is data rather than markup.
	DefuseLink(addr string) string
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

// DefuseLink wraps an address in a code span, where Slack does not linkify.
//
// ⭐ WRAPPING RATHER THAN REMOVING: the reader still sees the exact address the
// alert carried and simply cannot click it. Deleting it would tell them a smaller
// truth than the one that exists, which is truncateAt's doctrine applied to a URL.
func (SlackDialect) DefuseLink(addr string) string { return "`" + addr + "`" }

// LinkTo spells Slack's `<url|text>`. The text half is escaped by the caller;
// the pipe and angle brackets are oto's own punctuation and must survive.
func (SlackDialect) LinkTo(addr, text string) string {
	if text == "" {
		return "<" + addr + ">"
	}
	return "<" + addr + "|" + text + ">"
}

// StripAudience removes Slack's broadcast and subteam spellings.
//
// ⚠️ IT RUNS EVEN THOUGH escape() ALREADY NEUTRALISES THE BRACKETS, and the
// redundancy is deliberate. escape() defends the mrkdwn parser and happens to
// defeat these tokens as a side effect of turning "<" into "&lt;" — a property of
// an unrelated function that a future refactor could remove without knowing it was
// load-bearing here. ADR 0037 promises a Wording "can never emit a mention"; a
// promise that rests on a side effect is not a promise.
func (SlackDialect) StripAudience(s string) string { return stripCommonAudience(s) }

// stripCommonAudience removes the audience spellings NO provider may carry out of
// a Wording, whoever is going to read them.
//
// ⛔ IT IS SHARED BECAUSE THE THREAT IS SHARED. It used to live only on
// SlackDialect, and PlainDialect refused four bare words and nothing else — so a
// Wording emitting `<!channel>` or `<@U0123>` reached `envelope.rendered` verbatim,
// and a webhook consumer forwarding oto's text into a chat product delivered the
// ping. That is the exact laundering step PlainDialect's own comment claimed to
// prevent. A dialect adds its provider's extra spellings; it does not get to skip
// these.
func stripCommonAudience(s string) string {
	// ⛔ THE WHOLE BODY IS A FIXPOINT, NOT JUST THE WORD PASS. An earlier fix looped
	// `replaceFold` and stopped there, which closed one door and left the other
	// open: the bracketed-span pass runs AFTER the word pass, so the halves IT joins
	// are never re-examined by the word pass that already ran. A label of
	// `@ch<@U024BE7LH>annel` strips its user mention and leaves `@ch`+`annel` =
	// `@channel`, on both dialects. Every pass strictly shortens the string, so this
	// terminates.
	for i := 0; i < 32; i++ {
		before := s
		for _, tok := range []string{
			"<!channel>", "<!here>", "<!everyone>",
			"&lt;!channel&gt;", "&lt;!here&gt;", "&lt;!everyone&gt;",
			"@channel", "@here", "@everyone", "@room", "@all",
		} {
			s = replaceFold(s, tok, "")
		}
		for _, pair := range [][2]string{
			{"<@", ">"}, {"&lt;@", "&gt;"},
			{"<!subteam^", ">"}, {"&lt;!subteam^", "&gt;"},
		} {
			s = stripBracketed(s, pair[0], pair[1])
		}
		if s == before {
			return s
		}
	}
	return s
}

// neutralise is the LAST thing that touches a Wording's output: it refuses this
// provider's audience spellings and defuses this provider's links, repeatedly,
// until neither pass changes anything.
//
// ⛔ THE ORDER BETWEEN THEM IS NOT FIXABLE BY CHOOSING ONE, WHICH IS WHY IT IS A
// LOOP. Defusing first misses every address the audience strip goes on to CREATE —
// a label of `htt@channelps://evil.example/phish` loses `@channel` and becomes a
// live, clickable link on the card. Stripping first misses every audience token an
// address-defusing pass would expose. Each pass can feed the other, so the only
// correct answer is to run both until the string stops changing.
func neutralise(d Dialect, s string) string {
	for i := 0; i < 16; i++ {
		before := s
		s = d.StripAudience(s)
		s = defuseLinks(d, s)
		if s == before {
			return s
		}
	}
	return s
}

// defuseLinks makes a URL in a Wording's output unclickable without hiding it.
//
// ⛔ THE BRACKETED FORM WAS DEFENDED AND THE BARE FORM WAS NOT, WHICH IS THE WHOLE
// BUG. `<https://x|label>` never survives escaping, so it looked handled — but
// Slack auto-links a bare `https://…` in mrkdwn, so a Wording of
// `{{ annotations.summary }}` over an annotation containing a URL put a live,
// customer-controlled link on the card. ADR 0037 refuses user-authored URLs
// outright ("Links come only from the fixed `Links` set"), and it refuses them
// because `runbook_url: "<!channel>"` once put a channel-wide ping in every push
// notification.
//
// ⭐ IT WRAPS RATHER THAN REMOVES, AND THAT IS THE POINT. Slack does not linkify
// inside a code span, so the reader still sees the exact URL the alert carried —
// they simply cannot click it, and it is visibly marked as a literal. Deleting it
// would tell them a smaller truth than the one that exists, which is the doctrine
// truncateAt already states for a cut sentence.
func defuseLinks(d Dialect, s string) string {
	if !strings.Contains(s, "://") && !strings.Contains(s, "www.") {
		return s
	}
	if d.DefuseLink("x") == "x" {
		return s // this provider does not linkify; the address is data
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		start, n := urlAt(s, i)
		if n == 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(s[i:start])
		if alreadyDefused(d, s, start, n) {
			// ⚠️ IDEMPOTENCE IS REQUIRED, NOT NICE. neutralise runs this pass until
			// the string stops changing, so a defuser that re-wraps its own output
			// would add a delimiter per iteration and stop only at the loop's cap —
			// which is what a lone `code`-filtered URL did, arriving as seventeen
			// backticks.
			b.WriteString(s[start : start+n])
		} else {
			b.WriteString(d.DefuseLink(s[start : start+n]))
		}
		i = start + n
	}
	return b.String()
}

// alreadyDefused reports whether the address at [start, start+n) is already sitting
// inside this provider's defusing wrapper.
func alreadyDefused(d Dialect, s string, start, n int) bool {
	wrapped := d.DefuseLink("\x00")
	i := strings.Index(wrapped, "\x00")
	if i < 0 {
		return false
	}
	open, shut := wrapped[:i], wrapped[i+1:]
	if open == "" && shut == "" {
		return false
	}
	return start >= len(open) && s[start-len(open):start] == open &&
		start+n+len(shut) <= len(s) && s[start+n:start+n+len(shut)] == shut
}

// urlAt reports a URL-looking run beginning at or after i, and its length.
func urlAt(s string, i int) (int, int) {
	for _, prefix := range []string{"http://", "https://", "www."} {
		if idx := indexFoldFrom(s, prefix, i); idx >= 0 {
			end := idx
			for end < len(s) && !isURLBreak(s[end]) {
				end++
			}
			// Trailing punctuation belongs to the sentence, not the address.
			for end > idx && strings.IndexByte(".,;:!?)]}'\"", s[end-1]) >= 0 {
				end--
			}
			if end > idx {
				return idx, end - idx
			}
		}
	}
	return 0, 0
}

func isURLBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '<' || c == '>' || c == '|' || c == '`'
}

func indexFoldFrom(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	i := strings.Index(strings.ToLower(s[from:]), sub)
	if i < 0 {
		return -1
	}
	return from + i
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

// DefuseLink returns the address UNCHANGED, and that is a decision rather than an
// omission.
//
// ⛔ A WEBHOOK CONSUMER IS A PROGRAM AND A URL IS DATA TO IT. Nothing linkifies a
// JSON string, so there is no link to defuse — and a runbook address is the single
// most common thing an alert annotation carries, so mangling it would corrupt the
// field a consumer is most likely to want. This is the same reasoning as
// EscapeText's identity: handing a program another product's defensive markup is
// not safety.
//
// ⚠️ IT IS NOT AN EXEMPTION FOR AUDIENCE TOKENS, WHICH ARE STILL STRIPPED HERE. A
// ping is never legitimate content; an address usually is.
func (PlainDialect) DefuseLink(addr string) string { return addr }

// LinkTo gives a plain consumer both halves. It cannot render a link, and
// dropping the address would lose the only actionable thing in the line.
func (PlainDialect) LinkTo(addr, text string) string {
	if text == "" || text == addr {
		return addr
	}
	return text + " (" + addr + ")"
}

// StripAudience removes the bare spellings that read as a broadcast in almost every
// chat product, so a webhook consumer that forwards oto's text into one cannot be
// used as a laundering step for a ping a Wording was not allowed to send.
func (PlainDialect) StripAudience(s string) string { return stripCommonAudience(s) }

// ---------------------------------------------------------------------------
// Spelling the marks
// ---------------------------------------------------------------------------

// Spell converts oto's neutral marks into d's syntax and then refuses d's audience
// spellings. It is the last thing that touches a Wording's output before the
// renderer's own escape-and-truncate sink.
func Spell(d Dialect, s string, links map[string]string) string {
	if s == "" {
		return ""
	}
	var b, run strings.Builder
	b.Grow(len(s))
	// flush escapes the words accumulated so far. Markup is written straight to b,
	// bypassing this, which is the point.
	//
	// ⚠️ DEFUSING IS NOT DONE HERE. It used to be, and that was the bug: an address
	// the audience strip CREATES at the end of Spell had already passed this point.
	// Both now run together, to a fixpoint, in neutralise.
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
		case linkOpenRune:
			// ⭐ A `text` TEMPLATE RESOLVES LINK HANDLES HERE, for the same reason
			// a card resolves them in the parser: after Liquid has flattened
			// everything into one string, oto's own link and an address an alert
			// label smuggled in are the same kind of thing, and only a handle
			// minted after sanitise() proves which is which.
			end := indexRune(runes, i+1, linkShutRune)
			if end < 0 {
				continue // unterminated: drop the mark, keep the words
			}
			flush()
			if addr := links[string(runes[i+1:end])]; addr != "" {
				b.WriteString(d.LinkTo(addr, addr))
			}
			i = end
		case linkShutRune, actionsRune:
			// An orphaned handle, or an actions token in a format that has no
			// buttons to place. Neither has a spelling; drop it.
		default:
			run.WriteRune(r)
		}
	}
	flush()
	return neutralise(d, b.String())
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
		case r >= '\uE000' && r <= '\uF8FF': // BMP private-use area
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

// replaceFold removes every case-insensitive occurrence of tok, REPEATEDLY, until
// the string stops changing.
//
// ⛔ ONE PASS IS NOT ENOUGH, AND THE COUNTEREXAMPLE IS SHORT. A single left-to-right
// pass writes the text before each hit and resumes AFTER it, so the two halves it
// just joined are never re-examined — and joining them can create the very token
// being removed. An alert label of `@ch@channelannel` contains one `@channel`; strip
// it and the remainder is `@ch` + `annel` = `@channel`, which reaches the card. The
// value comes from upstream alert data, so this is not an admin typing something
// odd, it is anything that can set a label.
//
// Each pass strictly shortens the string, so the loop terminates; the bound is
// belt-and-braces against a future `with` that is not empty.
func replaceFold(s, tok, with string) string {
	if tok == "" || len(with) >= len(tok) {
		return replaceFoldOnce(s, tok, with)
	}
	for i := 0; i < 16; i++ {
		next := replaceFoldOnce(s, tok, with)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

func replaceFoldOnce(s, tok, with string) string {
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
	for n := 0; n < 64; n++ {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+len(open):], shut)
		if j < 0 {
			// ⛔ AN UNCLOSED OPENER REMOVES THE OPENER, NOT THE REST OF THE STANZA.
			// This used to `return s[:i]`, which silently deleted everything after
			// a label value containing a bare "<@" — an operator would have read a
			// truncated sentence with no ellipsis, no link and no way to know, which
			// is precisely the failure text.go's truncation doctrine exists to
			// refuse. There is no mention here to strip: an unterminated token is
			// not a mention, it is a stray bracket.
			return s[:i] + s[i+len(open):]
		}
		s = s[:i] + s[i+len(open)+j+len(shut):]
	}
	return s
}
