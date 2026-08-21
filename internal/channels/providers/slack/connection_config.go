package slack

import (
	_ "embed"
	"encoding/json"

	"github.com/thulasiram/oto/internal/channels/configschema"
	"github.com/thulasiram/oto/internal/platform/errs"
)

//go:embed connection_schema.json
var connectionSchemaBytes []byte

// ConnectionSchema is the ONE source of truth for a Slack connection's
// configuration — the org-wide setup, not one channel's. Same argument as
// Schema in config.go: the same bytes validate on the server and render the
// connection-creation form in the UI.
var ConnectionSchema = configschema.MustCompile(
	"https://oto.dev/schemas/channel-connection/slack/v1.json", connectionSchemaBytes)

// ConnectionConfig is the validated, non-secret configuration of one Slack
// connection: the workspace every channel referencing it posts into.
type ConnectionConfig struct {
	// TeamID identifies the workspace. Carried so a multi-workspace deployment
	// can tell two identically named connections apart in the UI; oto's own API
	// calls never need it, since the bot token is already scoped to one
	// workspace.
	TeamID string `json:"team_id"`
}

// ParseConnectionConfig validates raw against ConnectionSchema and decodes it.
func ParseConnectionConfig(raw json.RawMessage) (ConnectionConfig, error) {
	if err := ConnectionSchema.Validate(raw); err != nil {
		return ConnectionConfig{}, err
	}
	var c ConnectionConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return ConnectionConfig{}, errs.Wrap(err, errs.KindValidation, "config_invalid",
			"the slack connection config could not be decoded")
	}
	return c, nil
}
