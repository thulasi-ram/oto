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

// reservedPrefix is the namespace oto's own request framing lives in.
//
// ⛔ EVERY `X-Oto-*` NAME IS OTO'S, INCLUDING ONES THAT DO NOT EXIST YET (ADR 0055
// §1, git-bug 2765f74). The map above named six headers and none of oto's, while
// Channel.send's comment claimed this check "already refused the reserved names" —
// so a channel configured with its own `X-Oto-Delivery-Id` replaced oto's
// idempotency handle with a constant, and every retry of every delivery looked to
// the receiver like the same one. Reserving the prefix rather than listing today's
// four names means a header oto adds next release cannot already be configured to
// something else in somebody's channel.
const reservedPrefix = "x-oto-"

// reservedHeader reports whether name is in oto's namespace.
func reservedHeader(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), reservedPrefix)
}

// CheckHeaders rejects the headers a config may not carry. It is the SAVE-TIME
// rule: every refusal here is a 422 an operator reads while the form is open.
func CheckHeaders(headers map[string]string) error {
	return checkHeaders(headers, true)
}

// checkStoredHeaders is CheckHeaders for a config that is ALREADY STORED, at Open.
//
// ⭐ IT DOES NOT REFUSE THE `X-Oto-*` PREFIX, AND THAT IS NOT LENIENCY. A channel
// saved before the prefix was reserved may carry one, and refusing to open it would
// turn a hardening change into silent non-delivery — the argument `skipVerify`
// makes for a stored `insecure_skip_verify`. Such a header is not refused here; it
// is DROPPED at send (Channel.send skips every reserved name), so oto's framing
// wins without the channel going dark. Every other rule — credentials in headers,
// a newline — was refused at save from the start and is refused here too.
func checkStoredHeaders(headers map[string]string) error {
	return checkHeaders(headers, false)
}

func checkHeaders(headers map[string]string, reserve bool) error {
	var violations []errs.Violation
	for name := range headers {
		if reason, bad := forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))]; bad {
			violations = append(violations, errs.Violation{
				Field:   "headers/" + name,
				Code:    "forbidden",
				Message: reason,
			})
		}
		if reserve && reservedHeader(name) {
			violations = append(violations, errs.Violation{
				Field:   "headers/" + name,
				Code:    "forbidden",
				Message: "X-Oto-* headers are oto's own request framing (delivery id, signature, timestamp) and cannot be configured",
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
