package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/slack-go/slack"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// API is the slice of the Slack SDK oto uses.
//
// It is an interface for two reasons. It makes the Channel testable without a
// workspace, and it makes the surface auditable: everything oto can do to Slack
// is these FIVE methods, and the manifest in deploy/slack requests exactly the
// scopes they need between them.
//
// ⛔ THIS USED TO BE THREE METHODS, AND THE COMMENT USED TO SAY "NOTHING HERE
// READS SLACK". `GetConversationInfoContext` and `channels:read`/`groups:read`
// were removed for having ZERO CALLERS — a `Probe` nothing invoked. They are
// back, on purpose, with a real caller this time: `Provider.ResolveConversation`
// (resolve.go), which lets the settings UI turn a typed channel NAME into its
// ID and back, at CONFIGURATION time. This is still not "oto reads Slack to
// reconstruct its own state" (C9, ADR 0008) — oto's database remains the memory
// of what it has DELIVERED. It is metadata lookup for a human naming a
// destination, and the ADR that reintroduces the scopes says so explicitly
// rather than letting this comment silently disagree with the manifest again.
//
// `GetConversationsContext` (`conversations.list`) is how a NAME resolves to an
// ID — Slack has no "look up a channel by name" method, so a name lookup walks
// the list. `GetConversationInfoContext` (`conversations.info`) is how an ID
// resolves to a name; it is the cheaper, single-item call.
type API interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
}

// Channel is one configured Slack destination.
//
// It knows NOTHING about alerts (§F.1). It moves rendered bytes to a conversation
// and reports what Slack said. Deciding what to send, and remembering what was
// sent, both belong to the notification module: this type returns a MessageRef and
// forgets it.
type Channel struct {
	api     API
	cfg     Config
	limiter *limiter
	clock   clock.Clock
}

// Capabilities reports what this Channel can do. The dispatch service negotiates
// against this centrally — a provider never opts itself out of a rule (§F.1).
func (c *Channel) Capabilities() domain.Capability {
	return capabilities
}

// Deliver sends one rendered message.
//
// The MessageRef it returns is built from the API RESPONSE, never from config:
// Slack returns the conversation id it actually posted to, and the configured one
// can be stale or ambiguous (S7). The ts is carried as a STRING and is never
// parsed as a float — float round-tripping silently corrupts the six-digit
// sequence counter and is the single most common Slack integration bug.
func (c *Channel) Deliver(ctx context.Context, req domain.DeliverRequest) (domain.DeliverResult, error) {
	opts, err := c.messageOptions(req.Message)
	if err != nil {
		return domain.DeliverResult{}, err
	}

	conversation := c.cfg.ConversationID

	switch req.Mode {
	case domain.ModePostRoot:
		// Nothing else to add: a root message is a plain post.

	case domain.ModeThreadReply:
		if req.ReplyTo == nil || req.ReplyTo.MessageID == "" {
			return domain.DeliverResult{}, &domain.Error{
				Class: domain.ClassPermanent, Provider: providerName,
				Code:  "missing_thread_ts",
				Cause: errors.New("a threaded reply needs the root message ref"),
			}
		}
		if req.ReplyTo.ConversationID != "" {
			conversation = req.ReplyTo.ConversationID
		}
		// ALWAYS the root ts, never a reply's ts: Slack's own guidance, and
		// threading off a reply silently flattens the thread.
		opts = append(opts, slack.MsgOptionTS(threadRoot(req.ReplyTo)))

		// ⛔⛔ `slack.MsgOptionBroadcast()` WAS HERE AND IS DELETED (git-bug
		// 7570090). This was the SINGLE CALL SITE of Slack's `reply_broadcast`
		// parameter in the whole product, gated on `domain.ModeBroadcastReply`.
		//
		// WHY IT WENT RATHER THAN BEING TURNED OFF. The gate above it was
		// `BroadcastPolicy.Warrants`, which returned true for exactly two reasons:
		// `refired`, which ADR 0040 retired T8 and left with no producer at all, and
		// `all_resolved`, which was opt-in and default-off. So this line had never
		// run in a default deployment and could not be reached without an operator
		// asking for a thing the owner has now ruled oto does not do. A mechanism
		// with no producer is not a feature that is off; it is dead code with a
		// settings page.
		//
		// ⭐ THE REASONING THAT OUTLIVED IT, because the next irreversible act will
		// need it: broadcasting was IRREVERSIBLE — Slack documents nothing that
		// un-broadcasts — and that is why the decision was made ONCE, upstream in
		// `notification/domain/mode.go`, and obeyed blindly here. A provider that
		// re-derives a policy is a second opinion on an act nobody can take back.
		// Any future flag of that shape belongs on the mode, not in this switch.

	case domain.ModeUpdateRoot:
		if req.Target == nil || req.Target.MessageID == "" {
			return domain.DeliverResult{}, &domain.Error{
				Class: domain.ClassPermanent, Provider: providerName,
				Code:  "missing_target_ts",
				Cause: errors.New("an update needs the target message ref"),
			}
		}
		return c.amend(ctx, *req.Target, req.Message)

	default:
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassPermanent, Provider: providerName,
			Code:  "unsupported_mode",
			Cause: fmt.Errorf("slack channel cannot deliver mode %q", req.Mode),
		}
	}

	if err := c.limiter.waitPost(ctx, conversation); err != nil {
		return domain.DeliverResult{}, retryable(err, "context_cancelled")
	}

	respChannel, ts, err := c.api.PostMessageContext(ctx, conversation, opts...)
	if err != nil {
		return domain.DeliverResult{}, classify(err)
	}

	ref := domain.MessageRef{
		ConversationID: respChannel,
		MessageID:      ts,
		ThreadID:       ts,
		ProviderKey:    respChannel + ":" + ts,
	}
	if req.Mode == domain.ModeThreadReply {
		ref.ThreadID = threadRoot(req.ReplyTo)
	}

	return domain.DeliverResult{
		Ref:         ref,
		DeliveredAt: c.clock.Now().UTC(),
		Raw:         rawResult(respChannel, ts),
	}, nil
}

// Amend edits a message in place. This is the PRIMARY mechanism (ADR 0008):
// chat.update is Tier 3 at 50/min while chat.postMessage is ~1/s/channel, so
// amending is roughly fifty times cheaper — and it is also simply better, because
// the operator reads one card that is current instead of six that are not.
func (c *Channel) Amend(
	ctx context.Context, ref domain.MessageRef, msg domain.RenderedMessage,
) (domain.DeliverResult, error) {
	return c.amend(ctx, ref, msg)
}

func (c *Channel) amend(
	ctx context.Context, ref domain.MessageRef, msg domain.RenderedMessage,
) (domain.DeliverResult, error) {
	if ref.MessageID == "" {
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassPermanent, Provider: providerName,
			Code:  "missing_target_ts",
			Cause: errors.New("an update needs a message ts"),
		}
	}

	conversation := ref.ConversationID
	if conversation == "" {
		conversation = c.cfg.ConversationID
	}

	opts, err := c.messageOptions(msg)
	if err != nil {
		return domain.DeliverResult{}, err
	}

	if err := c.limiter.waitUpdate(ctx, conversation); err != nil {
		return domain.DeliverResult{}, retryable(err, "context_cancelled")
	}

	respChannel, ts, _, err := c.api.UpdateMessageContext(ctx, conversation, ref.MessageID, opts...)
	if err != nil {
		return domain.DeliverResult{}, classify(err)
	}

	out := ref
	if respChannel != "" {
		out.ConversationID = respChannel
	}
	if ts != "" {
		out.MessageID = ts
	}
	if out.ThreadID == "" {
		out.ThreadID = out.MessageID
	}
	out.ProviderKey = out.ConversationID + ":" + out.MessageID

	return domain.DeliverResult{
		Ref:         out,
		DeliveredAt: c.clock.Now().UTC(),
		Raw:         rawResult(out.ConversationID, out.MessageID),
	}, nil
}

// Probe verifies the CREDENTIAL without sending anything. It is one call:
// `auth.test`, which Slack documents as requiring NO SCOPE AT ALL.
//
// ⛔ IT NO LONGER READS `conversations.info`, AND THAT IS A SCOPE DECISION, NOT
// AN OVERSIGHT. Confirming the destination exists cost `channels:read` and
// `groups:read` — two workspace-wide metadata read scopes on an app whose entire
// job is to write two message types. The first live run proved the runtime needs
// exactly `chat.postMessage`, `chat.update` and `auth.test`, and the probe was
// the only thing keeping the read scopes in the manifest.
//
// What is lost is learning "the bot was removed from this channel" at
// configuration time rather than at delivery time. It is a small loss and it is
// already covered: §H.9 classifies `channel_not_found`, `is_archived` and
// `not_in_channel` as TERMINAL, marks the thread dead, sets the channel
// `degraded` and surfaces it in the UI with the fix. Paying two read scopes to
// learn the same fact a few minutes earlier is not a trade worth making for an
// app a security reviewer has to approve.
func (c *Channel) Probe(ctx context.Context) error {
	if _, err := c.api.AuthTestContext(ctx); err != nil {
		return classify(err)
	}
	return nil
}

// Close releases the Channel. The SDK client holds no pooled state oto owns, so
// there is nothing to tear down — but the port has the method because a provider
// that does hold a socket will need it.
func (c *Channel) Close() error { return nil }

// messageOptions turns oto's rendered bytes into SDK call options.
//
// The blocks are passed through VERBATIM rather than being re-marshalled from the
// SDK's own block types. That is deliberate: the payload oto validated (§L.6),
// persisted and hashed is the payload Slack receives, byte for byte. Round-tripping
// through another library's structs would mean validating one thing and sending
// another.
func (c *Channel) messageOptions(msg domain.RenderedMessage) ([]slack.MsgOption, error) {
	var p slackrender.Payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return nil, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "invalid_blocks", Cause: fmt.Errorf("rendered slack payload is unreadable: %w", err),
		}
	}

	atts := make([]slack.Attachment, 0, len(p.Attachments))
	for _, a := range p.Attachments {
		blocks := make([]slack.Block, 0, len(a.Blocks))
		for _, b := range a.Blocks {
			raw, err := json.Marshal(b)
			if err != nil {
				return nil, &domain.Error{
					Class: domain.ClassConfigInvalid, Provider: providerName,
					Code: "invalid_blocks", Cause: err,
				}
			}
			blocks = append(blocks, rawBlock{blockID: b.BlockID, kind: b.Type, raw: raw})
		}
		atts = append(atts, slack.Attachment{
			Color:    a.Color,
			Fallback: a.Fallback,
			Blocks:   slack.Blocks{BlockSet: blocks},
		})
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(p.Text, false),
		slack.MsgOptionAttachments(atts...),
		// S6: unfurling turns a runbook link into an unpredictable preview and
		// spends block budget oto does not control.
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	}
	if p.Metadata != nil {
		opts = append(opts, slack.MsgOptionMetadata(slack.SlackMetadata{
			EventType:    p.Metadata.EventType,
			EventPayload: p.Metadata.EventPayload,
		}))
	}
	if c.cfg.LinkNames {
		opts = append(opts, slack.MsgOptionLinkNames(true))
	}
	return opts, nil
}

// rawBlock carries a rendered block through the SDK untouched.
type rawBlock struct {
	blockID string
	kind    string
	raw     json.RawMessage
}

// BlockType implements slack.Block.
func (b rawBlock) BlockType() slack.MessageBlockType { return slack.MessageBlockType(b.kind) }

// ID implements slack.Block.
func (b rawBlock) ID() string { return b.blockID }

// MarshalJSON emits the renderer's exact bytes.
func (b rawBlock) MarshalJSON() ([]byte, error) { return b.raw, nil }

// threadRoot returns the ROOT ts of a thread. Slack is explicit: never thread off
// a reply's ts, always the parent's.
func threadRoot(ref *domain.MessageRef) string {
	if ref == nil {
		return ""
	}
	if ref.ThreadID != "" {
		return ref.ThreadID
	}
	return ref.MessageID
}

// rawResult records what Slack said, for debugging a provider that misbehaves.
func rawResult(conversation, ts string) json.RawMessage {
	raw, err := json.Marshal(map[string]string{"channel": conversation, "ts": ts})
	if err != nil {
		return nil
	}
	return raw
}
