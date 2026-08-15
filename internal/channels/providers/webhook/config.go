package webhook

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"slices"
	"time"

	"github.com/thulasiram/oto/internal/channels/configschema"
	"github.com/thulasiram/oto/internal/platform/errs"
)

//go:embed schema.json
var schemaBytes []byte

// schemaID is the published identity of a webhook channel's config schema. Both
// compiled forms below carry it: they are the same schema, and the gated one is
// the same schema minus a control this deployment will not honour.
const schemaID = "https://oto.dev/schemas/channel/webhook/v1.json"

// insecureSkipVerifyProperty is the one property whose presence in the SERVED
// schema depends on a deployment-level switch.
const insecureSkipVerifyProperty = "insecure_skip_verify"

// Schema is the ONE source of truth for a webhook channel's configuration (§L.5).
//
// It always declares `insecure_skip_verify`, because it must be able to READ a
// row that already carries it: a config stored before the deployment gate existed
// must still parse, so the channel keeps delivering (verified) rather than
// failing to open. Whether the flag is HONOURED, and whether it is OFFERED, is
// Provider's decision — see Provider.checkInsecureSkipVerify and skipVerify.
var Schema = configschema.MustCompile(schemaID, schemaBytes)

// GatedSchema is what a deployment with `security.allow_insecure_tls` OFF serves
// from GET /api/v1/channel-types: the same schema with `insecure_skip_verify`
// removed.
//
// ⛔ A FORM MUST NOT OFFER A CONTROL THE SERVER WILL REFUSE. Serving the full
// schema there rendered a "skip TLS verification" checkbox that a create would
// then reject, which reads as a bug and teaches an operator to distrust the
// form. Pruning is strictly a NARROWING — every config valid under this schema is
// valid under `Schema` — so the §L.5 promise that the form and the server agree
// about what is acceptable is strengthened, not broken.
var GatedSchema = configschema.MustCompile(schemaID, schemaWithout(schemaBytes, insecureSkipVerifyProperty))

// schemaWithout returns raw with one property deleted from `properties`.
//
// It PANICS when the property is not there, for the same reason MustCompile
// panics: this is a boot-time derivation from an embedded file, so a failure is a
// developer's edit and never a runtime condition. A silent no-op here would be a
// deployment quietly advertising a control it refuses.
//
// ⚠️ IT PRESERVES MEMBER ORDER, which is why it is not four lines of
// map[string]json.RawMessage. JSON Schema attaches no meaning to the order of
// `properties`, but the settings form is generated from these bytes in the order
// they arrive — and a Go map would re-emit them alphabetically, quietly moving
// "Endpoint URL" below "Additional headers" for every deployment running the
// default configuration.
func schemaWithout(raw []byte, property string) []byte {
	doc := decodeObject(raw, "schema.json")
	props, ok := doc.get("properties")
	if !ok {
		panic("webhook: schema.json declares no `properties`")
	}

	pruned := decodeObject(props, "schema.json `properties`")
	if _, ok := pruned.get(property); !ok {
		panic("webhook: schema.json no longer declares " + property +
			"; the deployment gate in provider.go is now dead code")
	}
	pruned.delete(property)
	doc.set("properties", pruned.encode())
	return doc.encode()
}

// object is a JSON object that remembers the order its members were written in.
type object struct {
	keys   []string
	values map[string]json.RawMessage
}

// decodeObject reads a JSON object, keeping member order. what names the input in
// the panic message.
func decodeObject(raw json.RawMessage, what string) *object {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		panic("webhook: " + what + " is not a JSON object")
	}

	o := &object{values: make(map[string]json.RawMessage)}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			panic("webhook: " + what + " is malformed: " + err.Error())
		}
		key, ok := keyTok.(string)
		if !ok {
			panic("webhook: " + what + " has a non-string member name")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			panic("webhook: " + what + " member " + key + " is malformed: " + err.Error())
		}
		o.set(key, value)
	}
	return o
}

func (o *object) get(key string) (json.RawMessage, bool) {
	v, ok := o.values[key]
	return v, ok
}

func (o *object) set(key string, value json.RawMessage) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *object) delete(key string) {
	delete(o.values, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

func (o *object) encode() json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(k)
		if err != nil {
			panic("webhook: cannot encode member name " + k + ": " + err.Error())
		}
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(o.values[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

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
	// InsecureSkipVerify asks for TLS verification to be turned off for an
	// internal receiver behind a private CA.
	//
	// ⛔ IT IS A REQUEST, NOT A DECISION. `channels.config` is tenant-writable
	// through POST /api/v1/channels, and honouring this field unconditionally let
	// any org member disable certificate verification on a connection oto's own
	// process makes — turning alert labels, annotations and the rule expression
	// into interceptable traffic. It takes effect only where the DEPLOYMENT has
	// set `security.allow_insecure_tls`, exactly as `alert_sources.tls_skip_verify`
	// does (§M2).
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
