package slack

import (
	_ "embed"
	"encoding/json"

	"github.com/thulasiram/oto/internal/channels/configschema"
	"github.com/thulasiram/oto/internal/platform/errs"
)

//go:embed schema.json
var schemaBytes []byte

// Schema is the ONE source of truth for a Slack channel's configuration (§L.5).
// The same bytes validate on the server and render the settings form in the UI,
// which is why adding a provider changes no UI code. It is compiled at package
// init, so a malformed schema is a boot panic rather than a runtime surprise.
var Schema = configschema.MustCompile("https://oto.dev/schemas/channel/slack/v1.json", schemaBytes)

// Config is the validated, non-secret configuration of one Slack channel.
//
// Note what is NOT here: no token, no signing secret, and no longer a
// `team_id` — the workspace is a property of the Connection this channel
// references (connection_schema.json), not of one destination within it.
type Config struct {
	// ConversationID is a Slack channel ID, never a #name. A name is ambiguous,
	// mutable, and resolves differently for different tokens.
	ConversationID string `json:"conversation_id"`
	// ConversationName is display only and may be stale by the time it is read.
	ConversationName string `json:"conversation_name"`
	// Transport is DEPRECATED, IGNORED, and kept only so that a config written by
	// an earlier release still validates against `additionalProperties: false`.
	//
	// ⛔ IT NEVER MEANT ANYTHING, AND THAT IS THE BUG. It was schema-validated and
	// rendered into the settings form, so an operator could pick "Socket Mode",
	// save it, and believe they had chosen how button presses reach oto — while
	// nothing anywhere read the value and no socket client existed to honour it.
	// A setting that has no effect is worse than a missing one: it answers the
	// question the operator was asking, incorrectly.
	//
	// Interactivity is a DEPLOYMENT fact, not a per-channel one: `slack.mode`
	// plus a signing secret, because one process either has a public Request URL
	// or it does not. Removal is deliberately staged — this release stops reading
	// and defaulting it, a later one drops it from the schema and strips it from
	// stored rows — because N and N+1 run simultaneously and a property removed
	// from the schema in one step would fail validation on rows the previous
	// release is still writing.
	//
	// Deprecated: ignored; interactivity is configured on the deployment.
	Transport string `json:"transport"`
	// MaxInstances is how many member instances the card renders inline.
	MaxInstances int `json:"max_instances"`
	// ⛔ THERE IS NO `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX.
	// It existed, was schema-validated, was rendered into the settings form — and
	// was NEVER READ: the registry builds one shared renderer and nothing ever
	// called the option that would have carried this list into it. An operator
	// could set it and nothing could ever happen.
	//
	// ⛔ AND THE REPLACEMENT THIS COMMENT NAMED IS ALSO GONE. It said the audience is
	// "now ONE org-level setting (`unacked_reminder_mention`, ADR 0020) … rendered
	// into the top-level `text` where a broadcast can actually carry it". The
	// reminder went with git-bug `bd0fb1d`, the mention with it, and broadcast itself
	// with git-bug 7570090. What survives is the RULE this field broke: a
	// schema-validated setting nothing reads is worse than a missing feature,
	// because an operator can configure it and be sure they did.
	// LinkNames asks Slack to find and link USER GROUPS in the message text.
	//
	// ⚠️ IT NO LONGER DOES WHAT THIS COMMENT USED TO SAY. The description was
	// "makes Slack linkify bare @names", which is what the parameter did when it
	// was added and is not what it does now: chat.postMessage documents
	// `link_names` as "find and link user groups. NO LONGER SUPPORTS LINKING
	// INDIVIDUAL USERS." So an operator who turns this on expecting `@ram` in an
	// annotation to become a real mention gets nothing, silently.
	//
	// oto does not depend on it either way — every mention oto emits is already in
	// Slack's wire form (`<@U…>`, `<!subteam^S…>`) from the org's mention policy,
	// and those need no linkification. Nothing about the setting is wrong; the
	// sentence describing it was.
	LinkNames bool `json:"link_names"`
}

// The two values `transport` may still carry. They are the SCHEMA'S vocabulary,
// retained so that a stored config written by an earlier release keeps
// validating; nothing in oto branches on either of them.
//
// ⚠️ THERE IS NO SOCKET MODE CLIENT ANYWHERE IN THIS CODEBASE, and `slack.mode`
// nevertheless defaults to `socket`. A deployment that leaves both at their
// defaults renders an Acknowledge button that nothing is listening for — which
// is precisely the defect the interactions consumer exists to fix, arriving by a
// different road. docs/setup/slack.md says so in those words, and until a socket
// listener is built the honest configuration is `OTO_SLACK_MODE=http` plus a
// Request URL.
//
// ADR 0018 forecloses a Slack Marketplace listing outright — it needs a stable
// client ID, client secret and redirect URL, none of which is a property of an
// operator oto has — so the pressure a listing would apply here does not exist.
const (
	// TransportSocketMode is accepted and ignored. Deprecated.
	TransportSocketMode = "socket_mode"
	// TransportHTTP is accepted and ignored. Deprecated.
	TransportHTTP = "http"
)

// ParseConfig validates raw against the schema and decodes it.
//
// Schema validation happens FIRST and unconditionally. Decoding a config that has
// not been proved valid is how a typo becomes a channel that silently never
// delivers.
func ParseConfig(raw json.RawMessage) (Config, error) {
	if err := Schema.Validate(raw); err != nil {
		return Config{}, err
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, errs.Wrap(err, errs.KindValidation, "config_invalid",
			"the slack channel config could not be decoded")
	}

	// ⛔ `transport` IS NOT DEFAULTED. It used to be filled in with
	// `socket_mode` here, which manufactured a value nothing read and made a
	// config that had never mentioned interactivity look like one that had chosen
	// it. Leaving it empty is the truth: this channel expresses no opinion,
	// because the field never carried one.
	if c.MaxInstances == 0 {
		c.MaxInstances = 10
	}
	return c, nil
}
