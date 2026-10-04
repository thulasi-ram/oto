package domain

// A TOOL'S RESULT IS REDACTED WITH THE INGEST REDACTION RULES (git-bug 2e9a086) before
// it is recorded as a Step and before the model reads it.
//
// ⭐⭐ THE RULES ARE THE ORG'S OWN, NOT A SECOND LIST. An operator already told oto which
// names are sensitive — `alert_sources.redact_labels` and `redact_annotations`, glob
// patterns over a NAME whose VALUE is replaced (SPEC §C.9.2) — and a ToolServer reads
// the same cluster those alerts came from. A pod's `DB_PASSWORD` env var, a log line's
// `token=…`, a ConfigMap's `api_key` are the same names in a different envelope. So the
// matcher is ingest's own (`ingestion/decode.Redactor`, handed in by `internal/app`
// over this module's RedactionRules port), and so is the replacement constant: a value
// redacted here reads exactly like one redacted at ingest.
//
// What a NAME is depends on the shape of the result, and both shapes are walked:
//
//   - JSON (the usual MCP shape — a pod, a query result, a list of log entries): every
//     object key that matches has its value replaced, at any depth, whatever the value
//     is — a matched key holding an object loses the whole object.
//   - Text, and every string inside JSON (a log line is a string): `name=value`,
//     `name: value` and `"name": "value"` pairs whose name matches have the value
//     replaced.
//
// ⚠️ THE TEXT PASS IS A PATTERN OVER PROSE, AND IT ERRS TOWARDS HIDING. `token_count=5`
// is redacted under `*token*`; a value with no `name=` before it is not seen at all.
// That is the stated limit of name-based redaction — the same one ingest has — and the
// reason the trust boundary is the ToolServer's RBAC, not this pass.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// ResultRedactor applies an org's redaction rules to a Tool's result. Its zero value
// redacts nothing — an org that configured no rules has none to apply — and never
// fails.
type ResultRedactor struct {
	match func(name string) bool
	mask  string
}

// NewResultRedactor builds a redactor over ingest's own name matcher and replacement
// value. A nil matcher is the zero value.
func NewResultRedactor(match func(name string) bool, mask string) ResultRedactor {
	if match == nil {
		return ResultRedactor{}
	}
	if mask == "" {
		mask = "[redacted]"
	}
	return ResultRedactor{match: match, mask: mask}
}

// Enabled reports whether this redactor can change anything.
func (r ResultRedactor) Enabled() bool { return r.match != nil }

// pairPattern finds `name=value`, `name: value` and `"name": "value"` in prose. Group 2
// is the name; group 5 the value — quoted, bracketed (so the mask itself is one value
// and redacting twice changes nothing), or bare.
var pairPattern = regexp.MustCompile(
	`("?)([A-Za-z_][A-Za-z0-9_.\-]*)("?)([ \t]*[:=][ \t]*)("(?:[^"\\\n]|\\.)*"|'[^'\n]*'|\[[^\]\s]*\]|[^\s,;&}\]"']+)`)

// Redact returns text with every matched value replaced, and how many were.
func (r ResultRedactor) Redact(text string) (string, int) {
	if !r.Enabled() || text == "" {
		return text, 0
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var doc any
		if dec.Decode(&doc) == nil && !dec.More() {
			n := 0
			doc = r.walk(doc, &n)
			if n == 0 {
				return text, 0
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if enc.Encode(doc) == nil {
				return strings.TrimSuffix(buf.String(), "\n"), n
			}
		}
	}
	return r.prose(text)
}

func (r ResultRedactor) walk(v any, n *int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if r.match(k) {
				t[k] = r.mask
				*n++
				continue
			}
			t[k] = r.walk(child, n)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = r.walk(child, n)
		}
		return t
	case string:
		out, k := r.prose(t)
		*n += k
		return out
	default:
		return v
	}
}

func (r ResultRedactor) prose(text string) (string, int) {
	n := 0
	out := pairPattern.ReplaceAllStringFunc(text, func(m string) string {
		g := pairPattern.FindStringSubmatch(m)
		if g == nil || !r.match(g[2]) || g[5] == r.mask || g[5] == `"`+r.mask+`"` {
			return m
		}
		n++
		value := r.mask
		if strings.HasPrefix(g[5], `"`) {
			value = `"` + r.mask + `"`
		} else if strings.HasPrefix(g[5], `'`) {
			value = `'` + r.mask + `'`
		}
		return g[1] + g[2] + g[3] + g[4] + value
	})
	return out, n
}
