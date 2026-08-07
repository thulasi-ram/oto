package slack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// The Block Kit vocabulary oto is allowed to emit.
//
// These are oto's own types, not the SDK's, for three reasons. The renderer is a
// pure function that must produce byte-identical output for the same view, so it
// owns its field order and its omitempty decisions. Validate (§L.6) checks the
// bytes we are about to send rather than a struct that might marshal differently.
// And the SDK is confined to the provider by depguard, which keeps the abstraction
// honest.
const (
	// BlockSection is the workhorse. The title is a section, never a header: a
	// header is plain_text only and would cost the deep link for no gain (S1).
	BlockSection = "section"
	// BlockContext is the quiet line: rule expression, footer provenance.
	BlockContext = "context"
	// BlockActions holds the buttons and the overflow.
	BlockActions = "actions"
	// BlockDivider separates. Used sparingly; it costs a block for no information.
	BlockDivider = "divider"
	// BlockImage is permitted by the whitelist for Grafana panel renders.
	BlockImage = "image"
	// BlockRichText is permitted by the whitelist but unused in v1.
	BlockRichText = "rich_text"
)

// Text object types.
const (
	// TypeMrkdwn is Slack's markdown dialect. Not CommonMark.
	TypeMrkdwn = "mrkdwn"
	// TypePlainText is literal text. Button labels must use it.
	TypePlainText = "plain_text"
)

// Interactive element types.
const (
	// ElementButton is a button. At most one may carry style "primary" (S10).
	ElementButton = "button"
	// ElementOverflow is the "…" menu that keeps the action row to four elements.
	ElementOverflow = "overflow"
)

// Payload is the chat.postMessage / chat.update body oto renders.
//
// channel, ts and thread_ts are deliberately absent: those are the provider's
// business, taken from the DeliverRequest and from the API RESPONSE (S7). A
// renderer that knew a conversation id could not be a pure function of the view.
type Payload struct {
	// Text is the highest-leverage string in the product. It is the push
	// notification, the sidebar preview, the search snippet, and the only thing a
	// screen reader reads (S5). It is always a complete sentence.
	Text string `json:"text"`
	// UnfurlLinks and UnfurlMedia are always false and are always present, so
	// that V15 checks a value rather than an absence (S6).
	UnfurlLinks bool `json:"unfurl_links"`
	UnfurlMedia bool `json:"unfurl_media"`
	// LinkNames is opt-in per channel; it makes Slack linkify bare @names.
	LinkNames bool `json:"link_names,omitempty"`
	// Metadata rides along so an interaction payload can be traced back to the
	// delivery that produced the message without a database round trip.
	Metadata *Metadata `json:"metadata,omitempty"`
	// Attachments is EXACTLY ONE attachment wrapping ALL blocks (S3). Attachments
	// are legacy but remain the only way to get a colour bar, and the colour bar
	// is the peripheral-vision answer to "do I need to act?".
	Attachments []Attachment `json:"attachments"`
}

// Metadata is Slack message metadata. It is bounded at 8 KiB (V17).
type Metadata struct {
	EventType    string         `json:"event_type"`
	EventPayload map[string]any `json:"event_payload"`
}

// Attachment carries the state colour and wraps the whole card.
type Attachment struct {
	// Color encodes STATE, never severity (S4). Severity is the leading emoji.
	Color string `json:"color"`
	// Fallback is the legacy plain-text summary of the attachment.
	Fallback string  `json:"fallback"`
	Blocks   []Block `json:"blocks"`
}

// Block is one Block Kit block. It is a union: only the fields legal for Type are
// ever populated, and the constructors below are the only way it is built.
type Block struct {
	Type string `json:"type"`
	// BlockID is regenerated on every render (S12): Slack advises a new block_id
	// per message iteration, and update-in-place means many iterations.
	BlockID  string `json:"block_id,omitempty"`
	Text     *Text  `json:"text,omitempty"`
	Fields   []Text `json:"fields,omitempty"`
	Elements []any  `json:"elements,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	AltText  string `json:"alt_text,omitempty"`
}

// Text is a Block Kit text object.
type Text struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

// Action is a button or an overflow menu inside an actions block.
type Action struct {
	Type     string           `json:"type"`
	Text     *Text            `json:"text,omitempty"`
	ActionID string           `json:"action_id,omitempty"`
	URL      string           `json:"url,omitempty"`
	Value    string           `json:"value,omitempty"`
	Style    string           `json:"style,omitempty"`
	Options  []OverflowOption `json:"options,omitempty"`
}

// OverflowOption is one entry in an overflow menu. Every option, url-bearing or not,
// still delivers an interaction payload oto must ack with a 200 (S9).
type OverflowOption struct {
	Text  Text   `json:"text"`
	URL   string `json:"url,omitempty"`
	Value string `json:"value,omitempty"`
}

func mrkdwn(s string) *Text { return &Text{Type: TypeMrkdwn, Text: s} }

func plain(s string) *Text { return &Text{Type: TypePlainText, Text: s, Emoji: true} }

func sectionBlock(id, text string) Block {
	return Block{Type: BlockSection, BlockID: id, Text: mrkdwn(text)}
}

func fieldsBlock(id string, fields []Text) Block {
	return Block{Type: BlockSection, BlockID: id, Fields: fields}
}

func contextBlock(id string, elements ...Text) Block {
	els := make([]any, 0, len(elements))
	for _, e := range elements {
		els = append(els, e)
	}
	return Block{Type: BlockContext, BlockID: id, Elements: els}
}

func actionsBlock(id string, elements ...Action) Block {
	els := make([]any, 0, len(elements))
	for _, e := range elements {
		els = append(els, e)
	}
	return Block{Type: BlockActions, BlockID: id, Elements: els}
}

// hashOf is the sha256 of the marshalled payload. The notification module uses it
// to skip a chat.update that would change nothing, which is the difference
// between a card that is current and a card that is noisy.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// marshal renders the payload. json.Marshal escapes <, > and & by default, which
// would turn every mrkdwn link into <…>, so encoding is done with HTML
// escaping disabled.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
