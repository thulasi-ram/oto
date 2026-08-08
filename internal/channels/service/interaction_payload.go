package service

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The Slack interaction envelope, decoded into exactly the fields oto reads and
// no others.
//
// ⛔ THE SLACK SDK IS NOT USED HERE, AND MUST NOT BE: it lives only in
// `channels/providers/slack`. `slack.InteractionCallback` is a 60-field struct
// whose shape changes with the SDK; what oto needs is nine strings, and decoding
// exactly those nine makes the parse total — an unrecognised extra field cannot
// break it, and a field oto does not read cannot be smuggled into a decision.
//
// ⛔ NOTHING DECODED HERE IS AUTHORITY (§H.8, S8). The envelope was proved to
// come from Slack by the HMAC over the raw body; that proves the SENDER, not the
// permission. `Value` is an opaque identifier resolved against oto's own tables,
// and the tenant is resolved from the workspace and conversation, never from
// anything the button carries.

// interactionTypeBlockActions is the only interaction type oto's cards produce.
// A view submission, a shortcut or a slash command is not something oto has
// registered, and answering one would be answering a question nobody asked.
const interactionTypeBlockActions = "block_actions"

// slackEnvelope is the decoded interaction. Fields are unexported types on
// purpose: this is a wire model, it never leaves the package, and nothing in the
// service layer takes one as a parameter.
type slackEnvelope struct {
	Type        string          `json:"type"`
	ResponseURL string          `json:"response_url"`
	Team        slackTeamRef    `json:"team"`
	Channel     slackChannelRef `json:"channel"`
	User        slackUserRef    `json:"user"`
	Container   slackContainer  `json:"container"`
	Message     slackMessageRef `json:"message"`
	Actions     []slackAction   `json:"actions"`
}

type slackTeamRef struct {
	ID string `json:"id"`
}

type slackChannelRef struct {
	ID string `json:"id"`
}

// slackUserRef carries three spellings of a display name because Slack sends
// different ones depending on the surface. `username` is the classic handle,
// `name` the modern one, and `profile.display_name` what the person actually set.
// oto prefers the most human of the three it was given and keeps a copy — the
// timeline label must survive a rename (§D.4), so it is denormalised at press
// time and never read back (C9).
type slackUserRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Profile  struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
	} `json:"profile"`
}

type slackContainer struct {
	MessageTS string `json:"message_ts"`
}

type slackMessageRef struct {
	TS string `json:"ts"`
}

type slackAction struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// handle is the display name to record, in preference order.
func (u slackUserRef) handle() string {
	for _, c := range []string{u.Profile.DisplayName, u.Username, u.Name, u.Profile.RealName} {
		if c = strings.TrimSpace(c); c != "" {
			return strings.TrimPrefix(c, "@")
		}
	}
	return ""
}

// messageTS is the root message the button sits on. `container.message_ts` is
// what Slack sends for a block action on a posted message; `message.ts` is the
// same value on some surfaces, and is the fallback rather than a second source
// of truth.
func (e slackEnvelope) messageTS() string {
	if ts := strings.TrimSpace(e.Container.MessageTS); ts != "" {
		return ts
	}
	return strings.TrimSpace(e.Message.TS)
}

// parseSlackEnvelope decodes one verified interaction payload.
//
// It is DELIBERATELY LENIENT about unknown fields — this is untrusted upstream
// input under CONTEXT.md §5b's second trust model, where a strict decoder turns
// "Slack added a field" into "acknowledgement stopped working" — and STRICT about
// the handful of fields it will act on: an envelope missing a team, a channel or
// a user is refused, because each of those is an input to a tenancy decision.
func parseSlackEnvelope(raw json.RawMessage) (slackEnvelope, error) {
	var e slackEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return slackEnvelope{}, errs.Malformed("slack_payload_malformed",
			"the interaction payload could not be decoded")
	}

	e.Type = strings.TrimSpace(e.Type)
	e.Team.ID = strings.TrimSpace(e.Team.ID)
	e.Channel.ID = strings.TrimSpace(e.Channel.ID)
	e.User.ID = strings.TrimSpace(e.User.ID)
	e.ResponseURL = strings.TrimSpace(e.ResponseURL)

	if e.Type != interactionTypeBlockActions {
		// Not a defect and not an attack: oto registers no other interaction type,
		// so anything else is a surface somebody enabled in the Slack app that oto
		// does not serve. The caller logs and acknowledges.
		return e, errUnsupportedInteraction
	}
	if e.Team.ID == "" || e.Channel.ID == "" || e.User.ID == "" {
		return slackEnvelope{}, errs.Malformed("slack_payload_incomplete",
			"the interaction named no workspace, conversation or member")
	}
	return e, nil
}

// errUnsupportedInteraction marks an envelope oto has no handler for.
//
// It is a PLAIN SENTINEL rather than an errs.Error on purpose: every errs.Kind
// maps to an HTTP status, and this outcome has none — the request was answered
// 200 before it was ever decoded. Borrowing 415 for "oto registered no shortcut"
// would put a lie in the taxonomy for the sake of reusing a constructor.
var errUnsupportedInteraction = errors.New("channels: unsupported slack interaction type")
