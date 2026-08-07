package webhook

import (
	_ "embed"
	"encoding/json"
	"time"

	"github.com/thulasiram/oto/internal/channels/configschema"
	"github.com/thulasiram/oto/internal/platform/errs"
)

//go:embed schema.json
var schemaBytes []byte

// Schema is the ONE source of truth for a webhook channel's configuration (§L.5).
var Schema = configschema.MustCompile("https://oto.dev/schemas/channel/webhook/v1.json", schemaBytes)

// Config is the validated configuration of one generic webhook destination.
//
// Look at what is here: a URL, a verb, some headers, a timeout. No thread, no
// colour, no blocks, no mentions. That absence is the abstraction proof (R5): if
// this struct ever needs a Slack affordance, the Channel port is wrong and the
// SPEC changes before the code does.
type Config struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	// TimeoutMS bounds one delivery attempt. A receiver that hangs must not hold
	// a dispatch worker.
	TimeoutMS int `json:"timeout_ms"`
	// InsecureSkipVerify disables TLS verification for an internal receiver with
	// a private CA. It is opt-in, per channel, and never the default.
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// Timeout is the configured per-request budget.
func (c Config) Timeout() time.Duration {
	if c.TimeoutMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.TimeoutMS) * time.Millisecond
}

// ParseConfig validates raw against the schema and decodes it.
func ParseConfig(raw json.RawMessage) (Config, error) {
	if err := Schema.Validate(raw); err != nil {
		return Config{}, err
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, errs.Wrap(err, errs.KindValidation, "config_invalid",
			"the webhook channel config could not be decoded")
	}
	if c.Method == "" {
		c.Method = "POST"
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = 5000
	}
	return c, nil
}
