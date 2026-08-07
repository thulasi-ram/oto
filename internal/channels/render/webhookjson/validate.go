package webhookjson

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// maxPayloadBytes is the webhook envelope's ceiling (§L.6). It is far larger than
// the Slack one because there is no third party imposing block limits — but it is
// not unbounded, because an operator's endpoint is not oto's to overwhelm.
const maxPayloadBytes = 1 << 20 // 1 MiB

// Error is a failed outbound check on the webhook envelope. Like its Slack
// counterpart it carries the offending payload for the dead-letter (§L.6).
type Error struct {
	Check   string
	Detail  string
	Payload json.RawMessage
}

// Error implements the error interface.
func (e *Error) Error() string {
	return "webhook render invalid (" + e.Check + "): " + e.Detail
}

// ChannelError maps the failure onto the port's terminal class.
func (e *Error) ChannelError() *domain.Error {
	return &domain.Error{
		Class:    domain.ClassConfigInvalid,
		Provider: "webhook",
		Code:     "invalid_envelope",
		Cause:    e,
	}
}

func fail(payload json.RawMessage, check, format string, args ...any) *Error {
	return &Error{Check: check, Detail: fmt.Sprintf(format, args...), Payload: payload}
}

// Validate checks the envelope before delivery (§L.6): the schema tag matches,
// the payload is within bounds, and every timestamp is RFC 3339 UTC.
//
// The timestamp rule is not pedantry. A consumer that receives a local-time
// timestamp will silently mis-order oto's events against its own, and the whole
// value of oto is an honest ordering.
func Validate(payload json.RawMessage) error {
	if len(payload) > maxPayloadBytes {
		return fail(payload, "W3", "payload is %d bytes, limit %d", len(payload), maxPayloadBytes)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return fail(payload, "W0", "payload is not a JSON object: %v", err)
	}

	var schema string
	if err := json.Unmarshal(probe["schema"], &schema); err != nil || schema != Schema {
		return fail(payload, "W1", "schema must be %q, got %q", Schema, schema)
	}

	if err := checkTimestamps(payload, "", probe); err != nil {
		return err
	}
	return nil
}

// checkTimestamps walks the envelope for anything that parses as an RFC 3339
// instant and insists it is UTC.
func checkTimestamps(payload json.RawMessage, path string, obj map[string]json.RawMessage) error {
	for k, raw := range obj {
		here := k
		if path != "" {
			here = path + "/" + k
		}
		if err := checkNode(payload, here, raw); err != nil {
			return err
		}
	}
	return nil
}

func checkNode(payload json.RawMessage, path string, raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return nil
	case trimmed[0] == '{':
		var child map[string]json.RawMessage
		if err := json.Unmarshal(raw, &child); err != nil {
			return nil
		}
		return checkTimestamps(payload, path, child)
	case trimmed[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		for i, item := range items {
			if err := checkNode(payload, fmt.Sprintf("%s/%d", path, i), item); err != nil {
				return err
			}
		}
		return nil
	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		return checkInstant(payload, path, s)
	default:
		return nil
	}
}

// checkInstant only fires on strings that already look like an RFC 3339 instant,
// so a label value that merely contains digits is never mistaken for a timestamp.
func checkInstant(payload json.RawMessage, path, s string) error {
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || (s[10] != 'T' && s[10] != 't') {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fail(payload, "W2", "%s is not a valid RFC 3339 timestamp: %q", path, s)
	}
	if _, offset := t.Zone(); offset != 0 {
		return fail(payload, "W2", "%s must be UTC, got %q", path, s)
	}
	return nil
}
