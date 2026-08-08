package authn

import (
	"strings"

	"github.com/google/uuid"
)

// Redacted is what a secret looks like in a log record.
const Redacted = "[redacted]"

// Redact renders a presented secret safely for a log line or an error string.
//
// It returns the DISPLAY PREFIX and nothing else — `oto_pat_AbCd…` — which is
// exactly what the `api_tokens.prefix` column exists to make loggable: enough to
// tell two credentials apart in an incident, never enough to use one. Anything
// shorter than a prefix is redacted entirely, because a short string is more
// likely to be a whole secret than a truncated one.
//
// ⚠️ THIS IS THE ONLY SANCTIONED WAY A CREDENTIAL REACHES A LOG. A token, a
// session id and a password hash are never logged; if a diagnostic needs to name
// one, it names it through here.
func Redact(secret string) string {
	const displayLen = 12
	secret = strings.TrimSpace(secret)
	if len(secret) < displayLen {
		return Redacted
	}
	return secret[:displayLen] + "…"
}

// RedactID renders an identifier that is a bearer handle in everything but name
// — a session id, a token id — for a log record.
//
// A session id is not the cookie value, but it selects exactly one live session
// and it correlates a shipped log stream with a specific human. The last six
// characters are enough to match two records to each other during an incident
// and are useless on their own.
func RedactID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	s := id.String()
	return "…" + s[len(s)-6:]
}
