package template

// The intermediate document a `card` template renders into, and the reason a
// card is portable at all.
//
// ⭐ THE IR IS THE ANSWER TO "each channel has their own quirks". An author
// writes one Markdown document; this is what it parses to; and each provider
// compiles THIS, not the author's text. Adding Discord is writing one compiler
// over these seven node kinds — it is not touching a template, a filter, or a
// parser.
//
// ⛔ IT IS DELIBERATELY SMALLER THAN MARKDOWN. Every node here has a defensible
// spelling in Block Kit, in a Discord embed, and in plain text. Markdown
// constructs with no such spelling (tables, images, nested lists, HTML) are
// refused at parse time with a sentence, rather than silently degraded — a
// table that renders as mangled prose at 03:00 is worse than one that never
// saved.

// BlockKind names a block-level node.
type BlockKind string

const (
	// BlockHeading is `# text`. Slack has one heading block and it is plain
	// text only; Discord embeds carry a title. Depth beyond 1 degrades to bold
	// prose rather than being refused, because `##` is muscle memory.
	BlockHeading BlockKind = "heading"
	// BlockParagraph is a run of prose.
	BlockParagraph BlockKind = "paragraph"
	// BlockDivider is `---`.
	BlockDivider BlockKind = "divider"
	// BlockQuote is `> text`, oto's callout.
	BlockQuote BlockKind = "quote"
	// BlockList is `- item`, flat only.
	BlockList BlockKind = "list"
	// BlockFields is the `:::fields` extension: a two-column key/value grid.
	// It exists because it is the one shape Block Kit does natively and
	// Markdown has no syntax for, and expressing it as a table would have
	// meant supporting tables.
	BlockFields BlockKind = "fields"
	// BlockActions is the `{{ actions }}` token: oto's interactive row.
	//
	// ⚠️ A TEMPLATE MAY OMIT IT, BY THE OWNER'S EXPLICIT DECISION. The card
	// then carries no acknowledge button. That is a degraded card and not a
	// lost alert — `POST /api/v1/cases/{id}/ack` reaches the same service
	// method the button does — but it IS a real loss, so Validate warns and
	// the editor says so. It never blocks.
	BlockActions BlockKind = "actions"
)

// A Block is one block-level node. Only the fields its Kind uses are set.
type Block struct {
	Kind BlockKind
	// Inline is the content of a heading, paragraph or quote.
	Inline []Span
	// Items is the content of a list: one Inline run per item.
	Items [][]Span
	// Fields is the content of a fields grid, in author order.
	Fields []Field
}

// A Field is one key/value row of a fields grid.
type Field struct {
	Label []Span
	Value []Span
}

// SpanKind names an inline node.
type SpanKind string

const (
	// SpanText is literal words. Its Text is the ONLY place author or alert
	// prose lives, which is what lets a compiler escape exactly once, in
	// exactly one place.
	SpanText SpanKind = "text"
	// SpanEmphasis wraps children in one emphasis.
	SpanEmphasis SpanKind = "emphasis"
	// SpanCode is a code span. Its Text is never escaped as markup because a
	// code span is already literal in every provider.
	SpanCode SpanKind = "code"
	// SpanLink is an oto-issued link. Its Addr is NOT author-controlled — see
	// the parser, which refuses a link whose target did not come from the
	// binding's Links namespace.
	SpanLink SpanKind = "link"
	// SpanTime is an instant, carried as a node because Slack's `<!date^…>`
	// token renders in each viewer's own timezone and no Markdown spelling can
	// express that. It is the one construct that survived from the neutral-mark
	// design, and it survived because it earns its keep.
	SpanTime SpanKind = "time"
)

// A Span is one inline node.
type Span struct {
	Kind SpanKind
	// Text is set on SpanText and SpanCode.
	Text string
	// Children is set on SpanEmphasis and SpanLink.
	Children []Span
	// Mark is set on SpanEmphasis.
	Mark MarkKind
	// Addr is set on SpanLink.
	Addr string
	// Unix and Fallback are set on SpanTime.
	Unix     int64
	Fallback string
}

// A Document is a rendered card, ready for one provider's compiler.
type Document struct {
	Blocks []Block
	// HasActions records whether the author placed `{{ actions }}`. A compiler
	// that finds it false emits no action row at all — it does not append one,
	// because the owner chose omission and a compiler second-guessing that
	// would make the choice unavailable.
	HasActions bool
}

// PlainText renders a Document as prose, for the fallback text of a Slack
// message (the push notification and the screen-reader line) and for any
// provider with no structured shape at all.
func (d *Document) PlainText(dl Dialect) string {
	if d == nil {
		return ""
	}
	var b lineWriter
	for _, blk := range d.Blocks {
		switch blk.Kind {
		case BlockHeading, BlockParagraph:
			b.line(spansToPlain(dl, blk.Inline))
		case BlockQuote:
			b.line(spansToPlain(dl, blk.Inline))
		case BlockDivider, BlockActions:
			// Neither has a spoken form. A divider is punctuation for the eye
			// and an action row is not text at all.
		case BlockList:
			for _, it := range blk.Items {
				b.line("• " + spansToPlain(dl, it))
			}
		case BlockFields:
			for _, f := range blk.Fields {
				b.line(spansToPlain(dl, f.Label) + ": " + spansToPlain(dl, f.Value))
			}
		}
	}
	return b.String()
}

// Spelled renders a Document the way one provider will read it, keeping the
// emphasis. It is what a preview shows.
//
// ⭐ SHOWING TWO SPELLINGS SIDE BY SIDE IS THE PREVIEW'S WHOLE TEACHING JOB. An
// author shown one concludes that markup is theirs to write; an author shown
// `*bold*` beside `bold` cannot. It is also the only way the portability claim is
// visible rather than asserted.
//
// ⚠️ IT IS A FAITHFUL TEXT RENDERING, NOT A PIXEL PREVIEW. Slack draws a header
// block larger than a section and neither this nor any string can show that. What
// it does show exactly is the thing an author gets wrong: which characters are
// markup and which are words.
func (d *Document) Spelled(dl Dialect) string {
	if d == nil {
		return ""
	}
	var b lineWriter
	for _, blk := range d.Blocks {
		switch blk.Kind {
		case BlockHeading:
			b.line(Inline(dl, blk.Inline))
		case BlockParagraph:
			b.line(Inline(dl, blk.Inline))
		case BlockQuote:
			b.line("> " + Inline(dl, blk.Inline))
		case BlockDivider:
			b.line("───")
		case BlockActions:
			// Not text, and saying so is better than drawing nothing: an author
			// who cannot see where the buttons land cannot tell whether the token
			// took effect.
			b.line("[ Acknowledge ] [ Snooze ]")
		case BlockList:
			for _, it := range blk.Items {
				b.line("• " + Inline(dl, it))
			}
		case BlockFields:
			for _, f := range blk.Fields {
				b.line(Inline(dl, f.Label) + ": " + Inline(dl, f.Value))
			}
		}
	}
	return b.String()
}
