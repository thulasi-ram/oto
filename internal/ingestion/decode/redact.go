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
//
// ⛔ AND THE PASS IS ONLY AS GOOD AS THE PRE-CHECK THAT SELECTS IT. The rewriting
// below repairs every unstorable input there is, because `json.Unmarshal` already
// folds both a NUL escape and a lone surrogate to something Postgres will take.
// Everything `hasUnstorableBytes` calls clean skips all of it and is returned
// verbatim, so that function's COMPLETENESS is what actually holds this line. The
// full rule, and why it is a contract rather than an optimisation, is documented
// on it.
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

// hasUnstorableBytes is the cheap pre-check that preserves the verbatim fast path
// for every clean body. It scans the RAW bytes, so it sees the escape sequences
// that survive decoding as well as the raw bytes that do not.
//
// ⭐ THE ESCAPES ARE THE ONES THAT MATTER. A raw 0x00, and a raw encoded
// surrogate, are both things `json.Unmarshal` either refuses at the door or
// folds to U+FFFD — either way there is a row to show for it. The ESCAPED
// spellings are VALID JSON by RFC 8259, decode without complaint, and survive a
// verbatim passthrough as six innocent ASCII characters each. Postgres is
// stricter than the RFC and refuses both at the moment of the INSERT.
//
// ⛔ THIS SCAN'S COMPLETENESS IS THE CONTRACT, not an optimisation. Every byte
// sequence it calls clean is returned to the caller VERBATIM and handed straight
// to `ingest_batches.payload JSONB NOT NULL`; nothing downstream re-examines it.
// The rewriting slow path below already repairs everything listed here — Go's
// decoder folds a lone surrogate to U+FFFD all by itself — so any defect of this
// kind is a defect of THIS FUNCTION ONLY, and the remedy is always to widen the
// scan, never to touch `storable`. A pre-scan that misses a case Postgres
// refuses is strictly worse than no pre-scan at all: the INSERT it poisons is
// the one whose job is to record the rejection, so the failure produces a 503,
// no `ingest_rejections` row, and an Alertmanager that retries the identical
// body until its budget runs out. If you close a ticket here, close it with a
// case in `storability_test.go` proved against a real Postgres.
//
// # The complete rule
//
// A body is unstorable when ANY of the following holds:
//
//  1. it contains a raw 0x00 byte;
//  2. it is not valid UTF-8 (which also covers a raw CESU-8-style encoded
//     surrogate half, since those are not valid UTF-8);
//  3. it contains the six-character escape for U+0000 — a backslash, a `u` and
//     four zeros — which Postgres refuses with SQLSTATE 22P05,
//     `unsupported Unicode escape sequence`;
//  4. it contains an UNPAIRED surrogate escape: a high surrogate `\uD800`
//     through `\uDBFF` not immediately followed by a low surrogate escape, or a
//     low surrogate `\uDC00` through `\uDFFF` not immediately preceded by one —
//     SQLSTATE 22P02, `invalid input syntax for type json`.
//
// A well-formed surrogate PAIR is storable and MUST stay on the verbatim path.
// Getting that wrong would silently re-encode every body carrying an emoji or a
// CJK supplementary character from any producer that escapes non-BMP text —
// ES5-era `JSON.stringify`, Jackson with `ESCAPE_NON_ASCII`, `json.dumps` with
// the default `ensure_ascii=True` — which is most of them.
//
// ⛔ ESCAPE-AWARENESS IS LOAD-BEARING IN BOTH DIRECTIONS. A backslash only
// escapes what follows it when it is the last of an ODD run of backslashes.
// So the U+0000 escape preceded by ANOTHER backslash is an escaped backslash
// followed by the plain letters `u0000`: six characters of literal text and no
// NUL anywhere, and flagging it would needlessly cost a clean body its verbatim
// guarantee. Symmetrically, a high surrogate escape followed by `\\uDC00` is NOT
// a pair — that low half is literal text, so the high half is alone.
func hasUnstorableBytes(body []byte) bool {
	if bytes.IndexByte(body, 0x00) >= 0 || !utf8.Valid(body) {
		return true
	}
	return hasUnstorableEscape(body)
}

// Surrogate code units, in the range JSON escapes them and UTF-8 forbids them.
const (
	highSurrogateMin = 0xD800
	highSurrogateMax = 0xDBFF
	lowSurrogateMin  = 0xDC00
	lowSurrogateMax  = 0xDFFF
)

// escapeLen is the length of a `\uXXXX` escape, in bytes.
const escapeLen = 6

// hasUnstorableEscape walks the raw bytes once looking for rules 3 and 4 above.
//
// It does NOT re-parse the document, and it does not need to: a `\uXXXX` escape
// is only meaningful inside a JSON string, and a backslash anywhere else is
// already invalid JSON that `json.Unmarshal` will reject on the slow path. So
// the scan can stay context-free and still be exact, provided it respects
// backslash parity — which is the only way `\uXXXX` and the literal text
// `\uXXXX` differ in the byte stream.
func hasUnstorableEscape(body []byte) bool {
	for i := 0; i < len(body); {
		at := bytes.IndexByte(body[i:], '\\')
		if at < 0 {
			return false
		}
		i += at

		// Consume the whole run of backslashes at once. Only an odd run ends in a
		// backslash that escapes the byte after it; an even run is that many
		// escaped backslashes and the byte after it is ordinary text.
		run := 1
		for i+run < len(body) && body[i+run] == '\\' {
			run++
		}
		if run%2 == 0 {
			i += run
			continue
		}

		esc := i + run - 1 // the backslash that actually escapes
		u, ok := escapedCodeUnitAt(body, esc)
		if !ok {
			// Some other escape (`\n`, `\"`, `\/`, …) or a truncated `\u`. The
			// escaped byte is not a backslash — the run above already ate those —
			// so skipping one byte past the run cannot desynchronise the parity.
			i += run + 1
			continue
		}

		switch {
		case u == 0:
			return true
		case u >= highSurrogateMin && u <= highSurrogateMax:
			// A pair must be ADJACENT in the byte stream, and the low half must be
			// a real escape. Anything else leaves this high surrogate alone.
			lo, ok := escapedCodeUnitAt(body, esc+escapeLen)
			if !ok || lo < lowSurrogateMin || lo > lowSurrogateMax {
				return true
			}
			i = esc + 2*escapeLen
		case u >= lowSurrogateMin && u <= lowSurrogateMax:
			// A low half that followed a high half was consumed by the case above,
			// so reaching here means this one has no high half before it.
			return true
		default:
			i = esc + escapeLen
		}
	}
	return false
}

// escapedCodeUnitAt reads the UTF-16 code unit of the `\uXXXX` escape starting
// at i, or reports false when there is no complete, well-formed one there.
//
// ⛔ ESCAPES ARE RECOGNISED BY PARSING BYTES, NEVER BY COMPARING AGAINST A
// SOURCE LITERAL, and that is deliberate for two separate reasons. The first is
// that the literal form is one editor, one formatter or one copy-paste away from
// becoming the code point it merely DENOTES — a different input, on a different
// path, already handled, and conflating the two is exactly why the NUL half of
// this defect was reported twice and reproduced never; it misfired again while
// this very comment was being written. The second is that there is no single
// literal to compare against anyway: the hazard is a RANGE of 2,048 code units
// in two hex spellings, and `bytes.Contains` does not do ranges.
//
// The caller guarantees body[i] is a backslash that escapes what follows it.
// Both hex cases are accepted: RFC 8259 allows `\uD800` and `\ud800` alike, and
// Postgres refuses both alike.
func escapedCodeUnitAt(body []byte, i int) (uint16, bool) {
	if i < 0 || i+escapeLen > len(body) || body[i] != '\\' || body[i+1] != 'u' {
		return 0, false
	}
	var u uint16
	for _, c := range body[i+2 : i+escapeLen] {
		d, ok := hexNibble(c)
		if !ok {
			return 0, false
		}
		u = u<<4 | uint16(d)
	}
	return u, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
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
