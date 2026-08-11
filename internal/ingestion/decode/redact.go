package decode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
)

// RedactedValue is what replaces a matched value. It is a constant, not the
// original length or a hash: an operator must be able to see THAT a value was
// redacted without being able to infer anything about it.
const RedactedValue = "[redacted]"

// Redactor applies `alert_sources.redact_labels` and `redact_annotations` glob
// patterns to VALUES.
//
// ⭐ REDACTION PRECEDES PERSISTENCE (§L.3.3 step 7, §C.9.2). The patterns are
// applied to the in-memory payload before `ingest_batches.payload` is written, so
// a secret in an annotation never lands on disk, never reaches a log line, and
// never appears in the evidence column of a rejection. Redacting after the insert
// would be theatre.
//
// The patterns match LABEL NAMES and the value is what is replaced — a redaction
// list names the fields that are sensitive, not the secrets themselves.
type Redactor struct {
	labels      []string
	annotations []string
}

// NewRedactor builds a Redactor over one source's two pattern lists. Empty lists
// produce a Redactor that is a no-op but is never nil, so no call site needs a
// branch.
func NewRedactor(labelPatterns, annotationPatterns []string) *Redactor {
	return &Redactor{labels: clean(labelPatterns), annotations: clean(annotationPatterns)}
}

func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Enabled reports whether this Redactor would change anything.
func (r *Redactor) Enabled() bool { return len(r.labels) > 0 || len(r.annotations) > 0 }

// Envelope redacts every label and annotation map an envelope carries, in place.
//
// In place is deliberate. The alternative is a deep copy of a payload that can be
// eight megabytes, on the path whose p99 budget is 250 ms, to protect a value
// that the caller is about to throw away anyway. The caller owns the decoded
// envelope exclusively.
func (r *Redactor) Envelope(env *Envelope) {
	if !r.Enabled() || env == nil {
		return
	}
	r.mapLabels(env.GroupLabels)
	r.mapLabels(env.CommonLabels)
	r.mapLabels(env.RouteLabels)
	r.mapAnnotations(env.CommonAnnotations)
	for i := range env.Alerts {
		r.mapLabels(env.Alerts[i].Labels)
		r.mapAnnotations(env.Alerts[i].Annotations)
	}
}

// The JSON keys the persisted payload is walked by. They are Alertmanager's own
// spelling, and they are listed rather than derived because the walk must not
// touch anything else: a payload from Grafana or a custom `payload:` template can
// contain arbitrary nested objects, and a generic "redact every map" would rename
// fields oto has never heard of.
const (
	keyAlerts            = "alerts"
	keyLabels            = "labels"
	keyAnnotations       = "annotations"
	keyGroupLabels       = "groupLabels"
	keyCommonLabels      = "commonLabels"
	keyCommonAnnotations = "commonAnnotations"
	keyRouteLabels       = "routeLabels"
)

// PersistedPayload builds the bytes written to `ingest_batches.payload`: the RAW
// body, redacted, with B2's truncation reflected.
//
// ⭐ IT DOES NOT ROUND-TRIP THROUGH Envelope, and that is the point. Envelope
// models the fields oto reads; a Grafana payload carries `orgId`, `title`,
// `state`, `silenceURL`, `dashboardURL` and more, and a custom `payload:` template
// carries whatever its author wrote. Re-encoding the struct would silently delete
// all of it from the one artefact that exists to answer "what actually arrived".
// The column's documented contract is "the raw body AFTER redaction", so the walk
// below edits the decoded body in place and changes nothing else.
//
// When there is nothing to redact, nothing to truncate and nothing UNSTORABLE —
// the overwhelmingly common case — the original bytes are returned VERBATIM, byte
// for byte.
//
// ⛔ THE STORABILITY PASS IS NOT OPTIONAL, and it is here rather than at a caller
// because this function's output goes straight into `ingest_batches.payload
// JSONB NOT NULL` in the SAME transaction that would record a rejection. Bytes
// Postgres refuses do not produce a recorded failure; they produce a failed
// INSERT, a 503, and an Alertmanager that retries the identical body forever with
// nothing written down. `ApplyBatchBounds` already sanitises the decoded
// ENVELOPE via B18/B19 — but the envelope is not what is stored, so that pass
// cleans a copy this one throws away. See `storability_test.go`.
func PersistedPayload(body []byte, r *Redactor, truncateAlertsTo int) (json.RawMessage, error) {
	needsTruncate := truncateAlertsTo > 0
	needsStorable := hasUnstorableBytes(body)
	if (r == nil || !r.Enabled()) && !needsTruncate && !needsStorable {
		return body, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode: persisted payload: %w", err)
	}

	if needsTruncate {
		if list, ok := doc[keyAlerts].([]any); ok && len(list) > truncateAlertsTo {
			doc[keyAlerts] = list[:truncateAlertsTo]
		}
	}

	if r != nil && r.Enabled() {
		r.walk(doc)
	}

	// AFTER redaction, so a redacted value is never scanned, and so the constant
	// `RedactedValue` cannot itself be rewritten.
	if needsStorable {
		doc = storable(doc)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("decode: persisted payload: %w", err)
	}
	return out, nil
}

// nulEscapeBytes is the six characters a JSON encoder writes for U+0000.
//
// ⛔ IT IS BUILT FROM BYTES ON PURPOSE. Written literally it is one editor, one
// formatter or one copy-paste away from becoming the actual NUL it merely
// denotes — a different input, on a different path, already handled. Conflating
// the two is exactly why this defect was reported twice and reproduced never.
var nulEscapeBytes = []byte{'\\', 'u', '0', '0', '0', '0'}

// hasUnstorableBytes is the cheap pre-check that preserves the verbatim fast path
// for every clean body. It scans the RAW bytes, so it sees the escape sequence
// that survives decoding as well as the raw byte that does not.
//
// ⭐ THE ESCAPE IS THE ONE THAT MATTERS. A raw 0x00 inside a JSON string is
// invalid JSON: `json.Unmarshal` refuses it at the door, which is a clean
// rejection with a row to show for it. The escape is VALID JSON — it decodes to a
// NUL, it survives a verbatim passthrough as six innocent ASCII characters, and
// Postgres then refuses it with SQLSTATE 22P05 at the moment of the INSERT.
func hasUnstorableBytes(body []byte) bool {
	return bytes.Contains(body, nulEscapeBytes) ||
		bytes.IndexByte(body, 0x00) >= 0 ||
		!utf8.Valid(body)
}

// storable rewrites a decoded document so that every part of it can be stored.
//
// The rule is B19's, for B19's reason: a VALUE is sanitised to U+FFFD, and a KEY
// is DROPPED. Sanitising keys would let two distinct unstorable names collapse
// onto one and the second silently overwrite the first — losing data with no
// trace, which is the failure mode this function exists to prevent.
func storable(doc map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		if alerts.UnstorableReason(k) != "" {
			continue
		}
		out[k] = storableAny(v)
	}
	return out
}

func storableAny(v any) any {
	switch t := v.(type) {
	case string:
		s, _ := alerts.SanitiseText(t)
		return s
	case map[string]any:
		return storable(t)
	case []any:
		for i, item := range t {
			t[i] = storableAny(item)
		}
		return t
	default:
		return v
	}
}

// walk redacts the seven label and annotation containers of a decoded body.
func (r *Redactor) walk(doc map[string]any) {
	redactAny(doc[keyGroupLabels], r.labels)
	redactAny(doc[keyCommonLabels], r.labels)
	redactAny(doc[keyRouteLabels], r.labels)
	redactAny(doc[keyCommonAnnotations], r.annotations)

	list, ok := doc[keyAlerts].([]any)
	if !ok {
		return
	}
	for _, item := range list {
		alert, ok := item.(map[string]any)
		if !ok {
			continue
		}
		redactAny(alert[keyLabels], r.labels)
		redactAny(alert[keyAnnotations], r.annotations)
	}
}

// redactAny replaces matched values inside a decoded JSON object. A non-object
// (a hostile payload sending `"labels": 7`) is left alone: it carries no value to
// leak, and rewriting it would corrupt the evidence.
func redactAny(v any, patterns []string) {
	m, ok := v.(map[string]any)
	if !ok || len(patterns) == 0 {
		return
	}
	for name := range m {
		if matchAny(name, patterns) {
			m[name] = RedactedValue
		}
	}
}

// Labels reports a redacted copy of a label map, leaving the input untouched.
func (r *Redactor) Labels(in map[string]string) map[string]string {
	return redactCopy(in, r.labels)
}

// Annotations reports a redacted copy of an annotation map.
func (r *Redactor) Annotations(in map[string]string) map[string]string {
	return redactCopy(in, r.annotations)
}

func (r *Redactor) mapLabels(m map[string]string)      { redactInPlace(m, r.labels) }
func (r *Redactor) mapAnnotations(m map[string]string) { redactInPlace(m, r.annotations) }

func redactInPlace(m map[string]string, patterns []string) {
	if len(m) == 0 || len(patterns) == 0 {
		return
	}
	for name := range m {
		if matchAny(name, patterns) {
			m[name] = RedactedValue
		}
	}
}

func redactCopy(in map[string]string, patterns []string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for name, value := range in {
		if matchAny(name, patterns) {
			out[name] = RedactedValue
			continue
		}
		out[name] = value
	}
	return out
}

// matchAny reports whether name matches any glob.
//
// `path.Match` is the glob dialect: `*`, `?` and `[…]`, with `*` not crossing a
// `/`. Label names contain no `/`, so that restriction is inert here and the
// dialect is exactly "shell globs" as an operator expects. A malformed pattern
// (`[`) is treated as no match rather than an error — a typo in a redaction list
// must not be able to fail an ingest, and the safe direction on a typo is
// "redacted nothing", which is visible, rather than "rejected the batch", which
// loses alerts.
func matchAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == name {
			return true
		}
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}
