package webhook

import (
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The rules a webhook config must satisfy that its JSON Schema cannot express.
//
// The OTHER such rule — which network targets the receiver URL may reach — used
// to live beside this one, in a `Guard` local to this package that resolved the
// host and then let `client.Do` resolve it a SECOND time. Those two resolutions
// are a DNS-rebinding window, and closing it is not a webhook problem: it is the
// same problem the Alertmanager and Prometheus clients have. It is now solved
// once, in `internal/platform/netguard`, whose `DialContext` inspects the address
// it hands to the kernel. This file keeps only the header rule.

// forbiddenHeaders are headers a user-supplied config may not set.
//
// Authorization is the one that matters: a credential belongs in
// channel_credentials, sealed and rotatable, not in a config blob that is served
// to the settings UI and logged in an audit trail (§L.5). The rest would let a
// config override oto's own framing of the request.
var forbiddenHeaders = map[string]string{
	"authorization":       "credentials belong in the channel credential, not in headers",
	"proxy-authorization": "credentials belong in the channel credential, not in headers",
	"content-length":      "oto sets this header",
	"host":                "oto sets this header",
	"transfer-encoding":   "oto sets this header",
	"connection":          "oto sets this header",
}

// CheckHeaders rejects the headers a config may not carry.
func CheckHeaders(headers map[string]string) error {
	var violations []errs.Violation
	for name := range headers {
		if reason, bad := forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))]; bad {
			violations = append(violations, errs.Violation{
				Field:   "headers/" + name,
				Code:    "forbidden",
				Message: reason,
			})
		}
		if strings.ContainsAny(name, "\r\n") {
			violations = append(violations, errs.Violation{
				Field: "headers/" + name, Code: "pattern",
				Message: "a header name may not contain a newline",
			})
		}
	}
	for name, value := range headers {
		if strings.ContainsAny(value, "\r\n") {
			violations = append(violations, errs.Violation{
				Field: "headers/" + name, Code: "pattern",
				Message: "a header value may not contain a newline",
			})
		}
	}
	if len(violations) > 0 {
		return errs.Validation("config_invalid", "the webhook headers are not permitted", violations...)
	}
	return nil
}
