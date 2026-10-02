package webhook

import (
	"encoding/json"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// ADR 0052 §5: AN INCIDENT TOOL MAY ECHO ITS OWN INCIDENT (git-bug 506ff21).
//
// A receiver that opens an incident for an Incident fact may answer the POST with
// a 2xx JSON body naming that incident — its `external_url`, its `external_id`, or
// both. The dispatcher records the first such answer once per (Incident, channel)
// as ADR 0052 §5's outbound mapping and shows the link on the Incident's card and
// page. It is the RECEIPT of a delivery, the way a Slack `ts` is: nothing else in
// the response is read, and nothing oto decides depends on it.
//
// ⚠️ EXPECT IT TO BE EMPTY FOR THE TOOLS' OWN ALERT ENDPOINTS. PagerDuty Events v2
// and incident.io's HTTP alert source both answer 202 with a dedup key and a
// status, and open the incident asynchronously; neither returns an incident URL.
// The echo is populated by a bridge (ADR 0055 §3) or by a receiver that returns
// the keys. A response without them is the ordinary case, not an error.

// responseEcho reads a receiver's raw handle out of a 2xx response body. What it
// returns is UNVALIDATED; readEcho validates every source the same way.
type responseEcho interface {
	read(body []byte) (rawURL, rawID string)
}

// topLevelEcho is the default: the top-level string keys `external_url` and
// `external_id` of a JSON object. Anything else — not JSON, not an object, a key
// that is not a string, a body cut off at maxResponseBytes — reads as absent.
type topLevelEcho struct{}

func (topLevelEcho) read(body []byte) (string, string) {
	var v struct {
		URL json.RawMessage `json:"external_url"`
		ID  json.RawMessage `json:"external_id"`
	}
	if len(body) == 0 || json.Unmarshal(body, &v) != nil {
		return "", ""
	}
	return jsonString(v.URL), jsonString(v.ID)
}

// jsonString is a JSON string's value, or "" for anything that is not one. A
// number, a bool, an object or null is not an id oto can promise to show
// verbatim, so it is absent rather than coerced.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// readEcho is the echo this delivery carries, validated, or the zero value.
func (c *Channel) readEcho(body []byte) domain.ExternalIncident {
	if c.echo == nil {
		return domain.ExternalIncident{}
	}
	return domain.ValidExternalIncident(c.echo.read(body))
}
