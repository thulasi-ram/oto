package slack

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
)

// Compiling an operator's NotificationTemplate into Slack's own shape.
//
// ⭐ IN `card` AND `text` THE TEMPLATE OWNS THE DOCUMENT AND OTO OWNS THE ENVELOPE.
// The colour, the metadata the interaction handler reads back and the push
// notification line stay oto's, because a portable format has no spelling for them.
//
// ⭐ IN `raw` THE TEMPLATE OWNS THE MESSAGE (ADR 0051, amending ADR 0050). Raw is
// Slack-only Block Kit typed by hand, and an author who reached for it is asking
// for every element: the bar's colour, the top-level `text`, and where oto's
// buttons go. What stays oto's is what nobody sees and everything depends on —
// `block_id`s, message metadata, and the buttons' `action_id`s and values, without
// which an alert is one nobody can acknowledge from Slack.
//
// ⛔ EVERY FAILURE PATH RETURNS `false` AND NOTHING ELSE. The caller then builds
// oto's own message and the alert goes out. That is the single most important
// property in this feature: a template can render badly, render nothing, name a
// field that does not exist, be written for another provider entirely, or produce
// Block Kit that Slack would refuse, and the alert still arrives.
func (r *Renderer) templatePayload(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce string,
) (Payload, string, bool) {
	if o.Template == nil {
		return Payload{}, "", false
	}
	t, ok := r.renderTemplate(v, o, state, nonce, o.Template.Source, "")
	if !ok {
		return Payload{}, "", false
	}
	text := firstNonEmpty(t.text, shortFallback(v, state))
	p := Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			Color:    firstNonEmpty(t.colour, state.Colour()),
			Fallback: shortFallback(v, state),
			Blocks:   capBlocks(t.blocks),
		}},
	}
	if !deliverable(p) {
		return Payload{}, "", false
	}
	return p, text, true
}

// replyTemplatePayload is templatePayload for a thread reply, from the template's
// `reply_source`. A reply body that renders nothing — the usual way to say "I only
// restyle `all_resolved`" is a `{% if reason == 'all_resolved' %}` around the whole
// body — leaves oto's own reply in place for every other reason.
func (r *Renderer) replyTemplatePayload(
	v *domain.NotificationView, o domain.RenderOptions, nonce, colour, sentence string,
) (Payload, string, bool) {
	if o.Template == nil {
		return Payload{}, "", false
	}
	state := cardState(v)
	t, ok := r.renderTemplate(v, o, state, nonce, o.Template.ReplySource, "reply")
	if !ok {
		return Payload{}, "", false
	}
	text := firstNonEmpty(t.text, sentence)
	p := Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			Color:    firstNonEmpty(t.colour, colour),
			Fallback: truncateRunes(text, 200),
			Blocks:   capBlocks(t.blocks),
		}},
	}
	if !deliverable(p) {
		return Payload{}, "", false
	}
	return p, text, true
}

// templated is what a template decided: its blocks always, and in `raw` the top-
// level text and the bar colour too. An empty text or colour is "oto's own".
type templated struct {
	blocks []Block
	text   string
	colour string
}

// renderTemplate renders one template body. prefix keeps a reply's block ids
// apart from the root's, which matters only to a reader of the payload.
func (r *Renderer) renderTemplate(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce, src, prefix string,
) (templated, bool) {
	if strings.TrimSpace(src) == "" {
		return templated{}, false
	}
	format := template.Format(o.Template.Format)
	compiled, err := template.Compiled(format, src)
	if err != nil {
		return templated{}, false
	}
	in, links := template.BuildInput(v, r.renderedAt(v), format)

	var t templated
	switch format {
	case template.FormatCard:
		doc, probs := compiled.RenderCard(in, links)
		if doc == nil || template.Blocking(probs) {
			return templated{}, false
		}
		t.blocks = r.blocksOf(doc, v, state, nonce)
		// ⭐ THE FALLBACK IS THE DOCUMENT WITHOUT ITS EMPHASIS, NOT oto's OWN LINE.
		// It is the push notification, the search snippet, and the only thing a
		// screen reader reads — so it has to say what the card says. Using oto's
		// built-in sentence here would make the notification and the message
		// disagree, which is worse than either alone.
		t.text = doc.PlainText(template.PlainDialect{})
	case template.FormatText:
		text, err := compiled.RenderText(in, template.SlackDialect{}, links)
		if err != nil {
			return templated{}, false
		}
		t.blocks = []Block{sectionBlock(blockID(prefix+"body", nonce), truncateSection(text, o.BaseURL))}
		t.text = text
	case template.FormatRaw:
		msg, err := r.rawMessage(compiled, in, links, v, state, nonce, prefix)
		if err != nil {
			return templated{}, false
		}
		t = msg
	default:
		return templated{}, false
	}
	if len(t.blocks) == 0 {
		return templated{}, false
	}
	t.text = strings.TrimSpace(oneLine(t.text))
	return t, true
}

// deliverable runs the outbound gate on a templated payload BEFORE it is chosen.
//
// ⛔ A TEMPLATE THAT SLACK WOULD REFUSE MUST COST THE TEMPLATE, NOT THE ALERT. The
// same Validate runs on every payload after rendering, and a failure there marks
// the delivery dead — which is right for oto's own card and wrong for an author's:
// a hand-written button with a non-UUID value used to kill the alert outright,
// breaking ADR 0050's one unconditional promise. Asking here first turns it back
// into a fallback.
func deliverable(p Payload) bool {
	raw, err := json.Marshal(p)
	if err != nil {
		return false
	}
	return Validate(raw) == nil
}

// blocksOf compiles the document IR into Block Kit.
func (r *Renderer) blocksOf(
	doc *template.Document, v *domain.NotificationView, state CardState, nonce string,
) []Block {
	d := template.SlackDialect{}
	out := make([]Block, 0, len(doc.Blocks)+1)

	for i, blk := range doc.Blocks {
		id := blockID("tpl"+strconv.Itoa(i), nonce)
		switch blk.Kind {
		case template.BlockHeading:
			// A section, never a header block (S1): a header is plain_text only, so
			// it cannot carry a link or any emphasis, and a heading that silently
			// dropped the author's `**` would be the worst kind of degradation.
			text := template.Inline(d, blk.Inline)
			if text = strings.TrimSpace(text); text != "" {
				out = append(out, sectionBlock(id, truncateSection("*"+text+"*", v.Links.Group)))
			}
		case template.BlockParagraph:
			if text := strings.TrimSpace(template.Inline(d, blk.Inline)); text != "" {
				out = append(out, sectionBlock(id, truncateSection(text, v.Links.Group)))
			}
		case template.BlockQuote:
			// Slack's blockquote is a leading `>` on each line.
			if text := strings.TrimSpace(template.Inline(d, blk.Inline)); text != "" {
				out = append(out, sectionBlock(id, truncateSection("> "+text, v.Links.Group)))
			}
		case template.BlockDivider:
			out = append(out, Block{Type: BlockDivider, BlockID: id})
		case template.BlockList:
			var b strings.Builder
			for _, it := range blk.Items {
				line := strings.TrimSpace(template.Inline(d, it))
				if line == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("• " + line)
			}
			if b.Len() > 0 {
				out = append(out, sectionBlock(id, truncateSection(b.String(), v.Links.Group)))
			}
		case template.BlockFields:
			// ⛔ EACH CELL IS BUDGETED SEPARATELY, which is why a grid could never
			// have been one templated string. Slack caps a field at 2000 characters
			// and shows at most ten, and truncating the joined text would cut one
			// cell in half while leaving another empty.
			fields := make([]Text, 0, len(blk.Fields)*2)
			for _, f := range blk.Fields {
				if len(fields) >= maxFields {
					break
				}
				label := strings.TrimSpace(template.Inline(d, f.Label))
				value := strings.TrimSpace(template.Inline(d, f.Value))
				if label == "" && value == "" {
					continue
				}
				fields = append(fields, Text{Type: TypeMrkdwn, Text: truncateField("*"+label+"*\n"+value, v.Links.Group)})
			}
			if len(fields) > 0 {
				out = append(out, fieldsBlock(id, fields))
			}
		case template.BlockActions:
			// ⭐ THE BUTTONS ARE BUILT BY GO, ALWAYS, AND THE TEMPLATE ONLY SAYS
			// WHERE. Their `action_id`s are the dispatch keys interactions.go
			// switches on and validate.go pins to `^oto\\.[a-z0-9._]+$`; a label an
			// author could rewrite is a label, but an action_id they could rewrite
			// is an alert nobody can acknowledge.
			if b, ok := r.actionsBlock(v, state, nonce); ok {
				out = append(out, b)
			}
		}
	}
	return out
}

// rawMessage turns a `raw` template's JSON into Block Kit, a top-level text and a
// colour.
//
// ⚠️ IT ACCEPTS A BARE ARRAY OF BLOCKS, OR AN OBJECT WITH `blocks` AND OPTIONALLY
// `text` AND `color`. The first two are what Slack's own Block Kit Builder copies
// to the clipboard; the third is the message itself, and is how an author takes
// the bar and the notification line. A colour Slack cannot parse is oto's.
//
// ⭐ `{"type": "oto_actions"}` IS WHERE OTO'S BUTTONS GO. It is replaced by the same
// action row oto's own card draws, so Acknowledge, Snooze and the overflow keep
// their dispatch keys — a template places them, it never forges them.
//
// ⛔ THE BLOCK IDS ARE OVERWRITTEN, NOT ACCEPTED. `block_id` is regenerated per
// render (S12) and oto reads its own back off an interaction payload; an
// author-supplied one would either collide with oto's namespace or be silently
// wrong. The author owns the content of the blocks, not their identity.
func (r *Renderer) rawMessage(
	compiled *template.Template, in template.Input, links map[string]string,
	v *domain.NotificationView, state CardState, nonce, prefix string,
) (templated, error) {
	raw, err := compiled.RenderRaw(in)
	if err != nil {
		return templated{}, err
	}
	// Links are resolved only to addresses safeURL accepts, because in raw the
	// address lands inside an author's `<…|…>` and a `|` or `>` in it would end
	// their link early and print the rest.
	safe := make(map[string]string, len(links))
	for k, addr := range links {
		safe[k] = safeURL(addr)
	}
	spelled, err := template.SpellRawJSON(raw, safe)
	if err != nil {
		return templated{}, err
	}
	var msg struct {
		Text   string  `json:"text"`
		Color  string  `json:"color"`
		Blocks []Block `json:"blocks"`
	}
	if err := json.Unmarshal(spelled, &msg.Blocks); err != nil {
		if err2 := json.Unmarshal(spelled, &msg); err2 != nil {
			return templated{}, err
		}
	}

	out := make([]Block, 0, len(msg.Blocks))
	for _, b := range msg.Blocks {
		if b.Type == rawActionsBlock {
			if ab, ok := r.actionsBlock(v, state, nonce); ok {
				out = append(out, ab)
			}
			continue
		}
		b.BlockID = blockID(prefix+"raw"+strconv.Itoa(len(out)), nonce)
		out = append(out, b)
	}
	colour := ""
	if validColour(msg.Color) {
		colour = msg.Color
	}
	return templated{blocks: out, text: msg.Text, colour: colour}, nil
}

// rawActionsBlock is the placeholder a raw template uses for oto's action row.
const rawActionsBlock = "oto_actions"

// validColour is V2's rule, asked before the colour is chosen rather than after.
func validColour(c string) bool {
	switch c {
	case "good", "warning", "danger":
		return true
	}
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
