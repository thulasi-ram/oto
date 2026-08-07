package decode

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
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
// When there is nothing to redact and nothing to truncate — the overwhelmingly
// common case — the original bytes are returned VERBATIM, byte for byte.
func PersistedPayload(body []byte, r *Redactor, truncateAlertsTo int) (json.RawMessage, error) {
	needsTruncate := truncateAlertsTo > 0
	if (r == nil || !r.Enabled()) && !needsTruncate {
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

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("decode: persisted payload: %w", err)
	}
	return out, nil
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
