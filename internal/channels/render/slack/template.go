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
// ⭐ THE TEMPLATE OWNS THE DOCUMENT AND OTO OWNS THE ENVELOPE. Everything outside
// `Attachments[0].Blocks` — the colour that encodes state, the metadata the
// interaction handler reads back, the fallback line a push notification shows —
// stays oto's, because none of it is text and none of it is the author's to
// choose. What changes is the blocks.
//
// ⛔ EVERY FAILURE PATH RETURNS `false` AND NOTHING ELSE. The caller then builds
// oto's own card and the alert goes out. That is the single most important
// property in this feature: a template can render badly, render nothing, name a
// field that does not exist, or be written for another provider entirely, and the
// alert still arrives.
func (r *Renderer) templatePayload(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce string,
) (Payload, string, bool) {
	if o.Template == nil || o.Template.Source == "" {
		return Payload{}, "", false
	}
	format := template.Format(o.Template.Format)
	compiled, err := template.Compiled(format, o.Template.Source)
	if err != nil {
		return Payload{}, "", false
	}
	in, links := template.BuildInput(v, r.renderedAt(v), format)

	var (
		blocks   []Block
		fallback string
	)
	switch format {
	case template.FormatCard:
		doc, probs := compiled.RenderCard(in, links)
		if doc == nil || template.Blocking(probs) {
			return Payload{}, "", false
		}
		blocks = r.blocksOf(doc, v, state, nonce)
		// ⭐ THE FALLBACK IS THE DOCUMENT WITHOUT ITS EMPHASIS, NOT oto's OWN LINE.
		// It is the push notification, the search snippet, and the only thing a
		// screen reader reads — so it has to say what the card says. Using oto's
		// built-in sentence here would make the notification and the message
		// disagree, which is worse than either alone.
		fallback = doc.PlainText(template.PlainDialect{})
	case template.FormatText:
		text, err := compiled.RenderText(in, template.SlackDialect{}, links)
		if err != nil {
			return Payload{}, "", false
		}
		blocks = []Block{sectionBlock(blockID("body", nonce), truncateSection(text, o.BaseURL))}
		fallback = oneLine(text)
	case template.FormatRaw:
		blocks, err = rawBlocks(compiled, in, nonce)
		if err != nil {
			return Payload{}, "", false
		}
		fallback = shortFallback(v, state)
	default:
		return Payload{}, "", false
	}

	if len(blocks) == 0 {
		return Payload{}, "", false
	}
	fallback = strings.TrimSpace(oneLine(fallback))
	if fallback == "" {
		fallback = shortFallback(v, state)
	}

	return Payload{
		Text:        fallback,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			// Colour encodes STATE and is never the author's. A template that could
			// paint a firing card green would be a template that could lie about the
			// one fact the eye reads before any word.
			Color:    state.Colour(),
			Fallback: shortFallback(v, state),
			Blocks:   capBlocks(blocks),
		}},
	}, fallback, true
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

// rawBlocks unmarshals a `raw` template's JSON into Block Kit.
//
// ⚠️ IT ACCEPTS EITHER A BARE ARRAY OR `{"blocks": [...]}`, because both are what
// Slack's own Block Kit Builder copies to the clipboard and an author will paste
// whichever they were given.
//
// ⛔ THE BLOCK IDS ARE OVERWRITTEN, NOT ACCEPTED. `block_id` is regenerated per
// render (S12) and oto reads its own back off an interaction payload; an
// author-supplied one would either collide with oto's namespace or be silently
// wrong. The author owns the content of the blocks, not their identity.
func rawBlocks(compiled *template.Template, in template.Input, nonce string) ([]Block, error) {
	raw, err := compiled.RenderRaw(in)
	if err != nil {
		return nil, err
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var wrapper struct {
			Blocks []Block `json:"blocks"`
		}
		if err2 := json.Unmarshal(raw, &wrapper); err2 != nil {
			return nil, err
		}
		blocks = wrapper.Blocks
	}
	for i := range blocks {
		blocks[i].BlockID = blockID("raw"+strconv.Itoa(i), nonce)
	}
	return blocks, nil
}
