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
// Note what is NOT here: no token, no signing secret. Credentials live in
// channel_credentials, sealed, and reach a provider only as a domain.Credential.
type Config struct {
	// TeamID is the workspace. It is carried so a multi-workspace install can tell
	// two identically named channels apart in the UI.
	TeamID string `json:"team_id"`
	// ConversationID is a Slack channel ID, never a #name. A name is ambiguous,
	// mutable, and resolves differently for different tokens.
	ConversationID string `json:"conversation_id"`
	// ConversationName is display only and may be stale by the time it is read.
	ConversationName string `json:"conversation_name"`
	// Transport selects Socket Mode or HTTP for interactivity (§H.8).
	Transport string `json:"transport"`
	// MaxInstances is how many member instances the card renders inline.
	MaxInstances int `json:"max_instances"`
	// ⛔ THERE IS NO `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX.
	// It existed, was schema-validated, was rendered into the settings form — and
	// was NEVER READ: the registry builds one shared renderer and nothing ever
	// called the option that would have carried this list into it. An operator
	// could set it and nothing could ever happen. The unacked-reminder audience is
	// now ONE org-level setting (`unacked_reminder_mention`, ADR 0020), resolved
	// once, gated on severity, and rendered into the top-level `text` where a
	// broadcast can actually carry it.
	// LinkNames makes Slack linkify bare @names in the message text.
	LinkNames bool `json:"link_names"`
}

// Transports.
const (
	// TransportSocketMode is the default for self-hosted: no public ingress and
	// no signature verification, because the socket is pre-authenticated (§H.8).
	TransportSocketMode = "socket_mode"
	// TransportHTTP is for the install that ALREADY has public ingress and would
	// rather oto used it than hold a second outbound WebSocket open.
	//
	// ⚠️ It is NOT here for a Slack Marketplace listing. ADR 0018 forecloses the
	// Marketplace outright — a listing needs a stable client ID, client secret and
	// redirect URL, all of which are properties of an operator oto does not have,
	// and a listing would additionally FORBID Socket Mode, which is the transport
	// the majority of self-hosted installs actually want. The real justification is
	// the mundane one: an install with an ingress controller in front of oto can
	// point Slack's interactivity request URL at it and be done, with no extra
	// egress path to approve.
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

	if c.Transport == "" {
		c.Transport = TransportSocketMode
	}
	if c.MaxInstances == 0 {
		c.MaxInstances = 10
	}
	return c, nil
}
