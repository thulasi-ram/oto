package slack

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// Never send a message we have not proved is legal.
//
// A truncated-by-accident alert card is a CORRECTNESS failure, not a cosmetic
// one: it tells an operator a smaller truth than the one that exists, and they
// have no way to know. So every rendered payload is checked against Slack's
// documented limits before the API call and before it is persisted, and a payload
// that fails is DEAD — never silently truncated, never retried (§L.6).

var (
	// V2: the three legacy keywords plus a six-digit hex.
	colourRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	// V12: every action id oto emits lives in its own namespace, which is what
	// lets the interaction handler have an exhaustive switch with a no-op branch.
	actionIDRe = regexp.MustCompile(`^oto\.[a-z0-9._]+$`)
)

// allowedBlocks is the V4 whitelist.
//
// `header` is forbidden because the title must be a linkable section: a header is
// plain_text only, so it cannot carry the deep link, and that link is the point
// (S1). Slack accepts headers in messages perfectly well — this is oto's taste,
// not Slack's rule.
//
// ⚠️ S2 SAYS THE `alert` BLOCK IS FORBIDDEN "BECAUSE IT IS MODALS-ONLY DESPITE
// THE NAME", AND THERE IS NO `alert` BLOCK. Slack's block list is actions,
// context, divider, file, header, image, input, markdown, rich_text, section,
// video. The block that is surface-restricted is `input`, which is modals and
// App Home only and is rejected in a message — so the rule S2 wanted is real and
// this whitelist enforces it, by omission, along with `file`, `video` and
// `markdown`. Only the NAME in S2 and in ADR 0008 is wrong, and it is wrong in
// the two places a reader would go to check.
var allowedBlocks = map[string]bool{
	BlockSection:  true,
	BlockContext:  true,
	BlockActions:  true,
	BlockDivider:  true,
	BlockImage:    true,
	BlockRichText: true,
}

// Error is a failed outbound check.
//
// It carries the offending payload deliberately: §L.6 requires the bytes to land
// in notification_deliveries.rendered so the dead delivery can be debugged, and
// Check names which rule refused it. This is always an oto bug.
//
// ⛔ `Check` IS NOT A METRIC LABEL. It reads like one, and this comment used to
// say so — `oto_render_invalid_total{check}` was promised by an early draft and
// never built (5bc341a). The check name reaches an operator through the dead
// delivery and the log line, not through a series.
type Error struct {
	Check   string
	Detail  string
	Payload json.RawMessage
}

// Error implements the error interface.
func (e *Error) Error() string {
	return "slack render invalid (" + e.Check + "): " + e.Detail
}

// ChannelError maps the failure onto the channels port's terminal class. It is
// config_invalid rather than permanent so the Channel is flagged in the UI: a
// renderer emitting illegal blocks is a bug someone must see.
func (e *Error) ChannelError() *domain.Error {
	return &domain.Error{
		Class:    domain.ClassConfigInvalid,
		Provider: "slack",
		Code:     "invalid_blocks",
		Cause:    e,
	}
}

func fail(payload json.RawMessage, check, format string, args ...any) *Error {
	return &Error{Check: check, Detail: fmt.Sprintf(format, args...), Payload: payload}
}

// Validate runs the eighteen outbound checks of §L.6 over the exact bytes that
// are about to be sent.
//
// It validates the marshalled payload rather than the struct that produced it, so
// a marshalling bug is caught too. The checks run in order and the first failure
// wins, because a payload with two problems still only has one story to tell.
func Validate(payload json.RawMessage) error {
	// V18 first: an oversized payload is cheap to detect and expensive to walk.
	if len(payload) > maxPayloadBytes {
		return fail(payload, "V18", "payload is %d bytes, limit %d", len(payload), maxPayloadBytes)
	}

	var msg Payload
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fail(payload, "V0", "payload is not a slack message: %v", err)
	}

	// V14: the top-level text is the push notification, the search snippet and
	// the only thing a screen reader reads. An empty one is a silent alert.
	//
	// ⚠️ Slack does NOT require `text` when `blocks` or `attachments` are present
	// — "the text field is not enforced as required when using blocks or
	// attachments" — so this is oto's rule, and it is the right one: without it a
	// card is invisible on a locked phone and silent to a screen reader. The
	// LENGTH bound is oto's too; Slack's own numbers are 4 000 (chat.update's hard
	// `msg_too_long`) and 40 000 (where a message is silently truncated), and
	// maxTopLevelText is neither. See the constant.
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return fail(payload, "V14", "top-level text is empty")
	}
	if len(msg.Text) > maxTopLevelText {
		return fail(payload, "V14", "top-level text is %d chars, limit %d", len(msg.Text), maxTopLevelText)
	}

	// V15: unfurling turns a runbook link into an unpredictable preview card and
	// blows the block budget from outside oto's control.
	if msg.UnfurlLinks || msg.UnfurlMedia {
		return fail(payload, "V15", "unfurl_links and unfurl_media must both be false")
	}

	// V1: exactly one attachment, always. Two attachments means two colour bars,
	// which means the card no longer answers "do I need to act?" with one glance.
	if len(msg.Attachments) != 1 {
		return fail(payload, "V1", "expected exactly 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]

	// V2: a colour Slack cannot parse is silently dropped, taking the state cue
	// with it.
	switch att.Color {
	case "good", "warning", "danger":
	default:
		if !colourRe.MatchString(att.Color) {
			return fail(payload, "V2", "attachment colour %q is not good/warning/danger or #rrggbb", att.Color)
		}
	}

	// V3. ⚠️ Fifty is Slack's ceiling for a MESSAGE's blocks — "you can include up
	// to 50 blocks in each message". Slack publishes no ceiling for an
	// ATTACHMENT's blocks, which is where every one of oto's live (S3). The number
	// is applied to a position the documentation does not cover; it is
	// conservative and oto's own budget is seven, so nothing rides on it, but it
	// is an assumption and not a citation.
	if len(att.Blocks) > maxBlocks {
		return fail(payload, "V3", "%d blocks, limit %d", len(att.Blocks), maxBlocks)
	}

	seenBlockIDs := make(map[string]struct{}, len(att.Blocks))
	for i, b := range att.Blocks {
		if err := validateBlock(payload, i, b, seenBlockIDs); err != nil {
			return err
		}
	}

	// V17: Slack rejects oversized metadata with `metadata_too_large`, which would
	// kill the whole delivery for a debugging convenience.
	//
	// ⚠️ THE ERROR IS DOCUMENTED; THE SIZE IS NOT. "Metadata exceeds size limit"
	// appears in the error tables of both write methods and Slack states no figure
	// anywhere — not on the method pages, not in the message-metadata guide.
	// maxMetadata is oto's guess. oto's own payload is three short scalars, so the
	// guess has never been near the truth in either direction and only a live
	// workspace can find where the real edge is.
	if msg.Metadata != nil {
		raw, err := json.Marshal(msg.Metadata.EventPayload)
		if err != nil {
			return fail(payload, "V17", "metadata event_payload is not serialisable: %v", err)
		}
		if len(raw) > maxMetadata {
			return fail(payload, "V17", "metadata event_payload is %d bytes, limit %d", len(raw), maxMetadata)
		}
	}

	return nil
}

func validateBlock(payload json.RawMessage, idx int, b Block, seen map[string]struct{}) error {
	// V4: the whitelist. header and alert are the two that matter (S1, S2).
	if !allowedBlocks[b.Type] {
		return fail(payload, "V4", "block %d has forbidden type %q", idx, b.Type)
	}

	// V12 (block_id half) and V16.
	if b.BlockID != "" {
		if len(b.BlockID) > maxID {
			return fail(payload, "V12", "block %d block_id is %d chars, limit %d", idx, len(b.BlockID), maxID)
		}
		if _, dup := seen[b.BlockID]; dup {
			return fail(payload, "V16", "block_id %q is used more than once", b.BlockID)
		}
		seen[b.BlockID] = struct{}{}
	}

	switch b.Type {
	case BlockSection:
		// V5.
		if b.Text != nil {
			// ⚠️ SLACK'S MINIMUM IS ONE CHARACTER, NOT ZERO: "the minimum length is
			// 1 and maximum length is 3000". An empty text object is `invalid_blocks`
			// and this check did not exist — the only emptiness V5 caught was a
			// section with no text object AT ALL, so `{"type":"mrkdwn","text":""}`
			// passed every one of oto's eighteen rules and would have been refused by
			// Slack.
			if strings.TrimSpace(b.Text.Text) == "" {
				return fail(payload, "V5", "block %d section text object is empty", idx)
			}
			if len(b.Text.Text) > maxSectionText {
				return fail(payload, "V5", "block %d section text is %d chars, limit %d",
					idx, len(b.Text.Text), maxSectionText)
			}
		}
		// V6.
		if len(b.Fields) > maxFields {
			return fail(payload, "V6", "block %d has %d fields, limit %d", idx, len(b.Fields), maxFields)
		}
		for j, f := range b.Fields {
			if strings.TrimSpace(f.Text) == "" {
				return fail(payload, "V6", "block %d field %d is an empty text object", idx, j)
			}
			if len(f.Text) > maxFieldText {
				return fail(payload, "V6", "block %d field %d is %d chars, limit %d",
					idx, j, len(f.Text), maxFieldText)
			}
		}
		if b.Text == nil && len(b.Fields) == 0 {
			return fail(payload, "V5", "block %d is a section with neither text nor fields", idx)
		}

	case BlockContext:
		// V7. Slack documents the maximum ("an array of image elements and text
		// objects. Maximum number of items is 10") and no minimum, but an empty
		// `elements` array is not a context block — it is a block with nothing in it,
		// and it costs the same as one that says something.
		if len(b.Elements) == 0 {
			return fail(payload, "V7", "block %d is a context block with no elements", idx)
		}
		if len(b.Elements) > maxContextItems {
			return fail(payload, "V7", "block %d has %d context elements, limit %d",
				idx, len(b.Elements), maxContextItems)
		}

	case BlockActions:
		if err := validateActions(payload, idx, b); err != nil {
			return err
		}

	case BlockImage:
		// V10.
		//
		// ⚠️ `alt_text` IS REQUIRED AND WAS NOT CHECKED. Slack's image block
		// reference lists it as a required field — "a plain-text summary of the
		// image … maximum length for this field is 2000 characters" — and an image
		// block without one is `invalid_blocks`. So was an image block with no
		// `image_url`: `checkURL` returns nil for an empty string, which is right for
		// an OPTIONAL url and wrong for a required one. oto emits no image blocks
		// today (§H.3's budget is seven blocks and none is an image) — which is
		// exactly why the gap survived: the whitelist permits the block, so the first
		// person to render a Grafana panel would have hit a dead delivery.
		if strings.TrimSpace(b.AltText) == "" {
			return fail(payload, "V10", "block %d is an image with no alt_text", idx)
		}
		if len(b.AltText) > maxAltText {
			return fail(payload, "V10", "block %d alt_text is %d chars, limit %d",
				idx, len(b.AltText), maxAltText)
		}
		if b.ImageURL == "" {
			return fail(payload, "V10", "block %d is an image with no image_url", idx)
		}
		if err := checkURL(payload, "V10", "image_url", b.ImageURL); err != nil {
			return err
		}
	}

	return nil
}

func validateActions(payload json.RawMessage, idx int, b Block) error {
	// V8. An actions block with no elements is an empty row: it costs a block and
	// offers nothing to press.
	if len(b.Elements) == 0 {
		return fail(payload, "V8", "block %d is an actions block with no elements", idx)
	}
	if len(b.Elements) > maxActionItems {
		return fail(payload, "V8", "block %d has %d action elements, limit %d",
			idx, len(b.Elements), maxActionItems)
	}

	primaries := 0
	for j, raw := range b.Elements {
		el, err := asAction(raw)
		if err != nil {
			return fail(payload, "V8", "block %d element %d is not an interactive element: %v", idx, j, err)
		}

		// V12 (action_id half). The oto.* namespace is what lets the interaction
		// handler ack every unknown callback with a 200 instead of a 4xx — Slack
		// disables event subscriptions for apps that fail deliveries (§H.8).
		if el.ActionID != "" {
			if len(el.ActionID) > maxID {
				return fail(payload, "V12", "action_id %q is %d chars, limit %d",
					el.ActionID, len(el.ActionID), maxID)
			}
			if !actionIDRe.MatchString(el.ActionID) {
				return fail(payload, "V12", "action_id %q does not match ^oto\\.[a-z0-9._]+$", el.ActionID)
			}
		}

		switch el.Type {
		case ElementButton:
			// V9.
			if el.Text == nil || strings.TrimSpace(el.Text.Text) == "" {
				return fail(payload, "V9", "button %d in block %d has no label", j, idx)
			}
			if el.Text.Type != TypePlainText {
				return fail(payload, "V9", "button %d in block %d label must be plain_text, got %q",
					j, idx, el.Text.Type)
			}
			if len([]rune(el.Text.Text)) > maxButtonText {
				return fail(payload, "V9", "button %d in block %d label is %d chars, limit %d",
					j, idx, len([]rune(el.Text.Text)), maxButtonText)
			}
			// V10.
			if err := checkURL(payload, "V10", "button.url", el.URL); err != nil {
				return err
			}
			// V11: a button value is an OPAQUE ID and nothing else (S8). State is
			// looked up in oto's database; a value that carries a payload is a
			// value an attacker can forge.
			if el.Value != "" {
				if len(el.Value) > maxButtonValue {
					return fail(payload, "V11", "button value is %d chars, limit %d",
						len(el.Value), maxButtonValue)
				}
				if _, err := uuid.Parse(el.Value); err != nil {
					return fail(payload, "V11", "button value %q is not a bare UUID", el.Value)
				}
			}
			// V13.
			switch el.Style {
			case "primary":
				primaries++
			case "danger":
				return fail(payload, "V13", "inline style \"danger\" is not permitted; use the overflow with a confirm")
			case "":
			default:
				return fail(payload, "V13", "unknown button style %q", el.Style)
			}

		case ElementOverflow:
			// ⛔⛔ AN OVERFLOW MENU IS NOT A ROW OF BUTTONS, AND THIS BRANCH USED TO
			// TREAT IT AS ONE. Slack's overflow element and its option objects have
			// their OWN limits, and two of them were being checked against a button's:
			//
			//	option count  Slack: "up to five option objects"    oto: not checked
			//	option value  Slack: 150 characters                 oto: 2000
			//
			// The count was enforced only by `overflowMenu` refusing to add a sixth —
			// a renderer-side convention, not a rule. Anything that built an overflow
			// another way, and every future caller, had nothing standing between it
			// and `invalid_blocks`. That is precisely the job V0–V18 exist to do: the
			// renderer's own discipline is not a check, because the check has to hold
			// when the renderer changes.
			if len(el.Options) == 0 {
				return fail(payload, "V9", "overflow %d in block %d has no options", j, idx)
			}
			if len(el.Options) > maxOverflowOptions {
				return fail(payload, "V9", "overflow %d in block %d has %d options, limit %d",
					j, idx, len(el.Options), maxOverflowOptions)
			}
			for k, opt := range el.Options {
				if strings.TrimSpace(opt.Text.Text) == "" {
					return fail(payload, "V9", "overflow option %d in block %d has no label", k, idx)
				}
				// An overflow's option label is plain_text, always. mrkdwn in an
				// option renders as its own source text.
				if opt.Text.Type != TypePlainText {
					return fail(payload, "V9", "overflow option %d in block %d label must be plain_text, got %q",
						k, idx, opt.Text.Type)
				}
				if len([]rune(opt.Text.Text)) > maxOptionText {
					return fail(payload, "V9", "overflow option %d label is %d chars, limit %d",
						k, len([]rune(opt.Text.Text)), maxOptionText)
				}
				if err := checkURL(payload, "V10", "overflow option url", opt.URL); err != nil {
					return err
				}
				if len(opt.Value) > maxOptionValue {
					return fail(payload, "V11", "overflow option value is %d chars, limit %d "+
						"(an OPTION's limit, not a button's %d)",
						len(opt.Value), maxOptionValue, maxButtonValue)
				}
			}

		default:
			return fail(payload, "V8", "block %d element %d has unsupported type %q", idx, j, el.Type)
		}
	}

	// V13: exactly one call to action. Two primaries is two answers to "what do I
	// do now", which is none.
	if primaries > 1 {
		return fail(payload, "V13", "%d buttons carry style \"primary\", at most 1 is permitted", primaries)
	}
	return nil
}

// checkURL implements V10: bounded, absolute, http(s). A relative URL renders as
// a dead button and a javascript: URL is a security hole.
func checkURL(payload json.RawMessage, check, what, u string) error {
	if u == "" {
		return nil
	}
	if len(u) > maxURL {
		return fail(payload, check, "%s is %d chars, limit %d", what, len(u), maxURL)
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fail(payload, check, "%s %q is not an absolute http(s) URL", what, u)
	}
	return nil
}

// asAction re-decodes one element of an actions block. Elements are `any` in the
// Block union because a context element is a text object while an actions element
// is a button, and Slack uses the same key for both.
func asAction(v any) (Action, error) {
	switch t := v.(type) {
	case Action:
		return t, nil
	case *Action:
		return *t, nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return Action{}, err
		}
		var a Action
		if err := json.Unmarshal(raw, &a); err != nil {
			return Action{}, err
		}
		return a, nil
	}
}
