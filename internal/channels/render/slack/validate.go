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

// allowedBlocks is the V4 whitelist. `header` is forbidden because the title must
// be a linkable section (S1); `alert` is forbidden because, despite its name, it
// is modals-only and silently fails in a channel message (S2).
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
// Check names the counter label (oto_render_invalid_total{check}) that oto alerts
// itself on. This is always an oto bug.
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

	// V3: fifty blocks is Slack's hard ceiling; oto's own budget is seven.
	if len(att.Blocks) > maxBlocks {
		return fail(payload, "V3", "%d blocks, limit %d", len(att.Blocks), maxBlocks)
	}

	seenBlockIDs := make(map[string]struct{}, len(att.Blocks))
	for i, b := range att.Blocks {
		if err := validateBlock(payload, i, b, seenBlockIDs); err != nil {
			return err
		}
	}

	// V17: Slack rejects oversized metadata with metadata_too_large, which would
	// kill the whole delivery for a debugging convenience.
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
		if b.Text != nil && len(b.Text.Text) > maxSectionText {
			return fail(payload, "V5", "block %d section text is %d chars, limit %d",
				idx, len(b.Text.Text), maxSectionText)
		}
		// V6.
		if len(b.Fields) > maxFields {
			return fail(payload, "V6", "block %d has %d fields, limit %d", idx, len(b.Fields), maxFields)
		}
		for j, f := range b.Fields {
			if len(f.Text) > maxFieldText {
				return fail(payload, "V6", "block %d field %d is %d chars, limit %d",
					idx, j, len(f.Text), maxFieldText)
			}
		}
		if b.Text == nil && len(b.Fields) == 0 {
			return fail(payload, "V5", "block %d is a section with neither text nor fields", idx)
		}

	case BlockContext:
		// V7.
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
		if err := checkURL(payload, "V10", "image_url", b.ImageURL); err != nil {
			return err
		}
	}

	return nil
}

func validateActions(payload json.RawMessage, idx int, b Block) error {
	// V8.
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
			for k, opt := range el.Options {
				if strings.TrimSpace(opt.Text.Text) == "" {
					return fail(payload, "V9", "overflow option %d in block %d has no label", k, idx)
				}
				if len([]rune(opt.Text.Text)) > maxButtonText {
					return fail(payload, "V9", "overflow option %d label is %d chars, limit %d",
						k, len([]rune(opt.Text.Text)), maxButtonText)
				}
				if err := checkURL(payload, "V10", "overflow option url", opt.URL); err != nil {
					return err
				}
				if len(opt.Value) > maxButtonValue {
					return fail(payload, "V11", "overflow option value is %d chars, limit %d",
						len(opt.Value), maxButtonValue)
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
