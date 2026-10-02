package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/osteele/liquid"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// ADR 0055 §2: A PAYLOAD MAPPING RENDERS ON THIS ENGINE, AND IT IS NOT A TEMPLATE.
//
// A webhook Connection's payload mapping (domain.PayloadMapping) is Liquid over the
// `oto.notification.v1` envelope, and it runs on the same curated engine — the same
// tags, the same filters, the same iteration budget — a NotificationTemplate does.
// That is the whole of what the two share. A template binds a NotificationView and
// falls back to oto's own card on any failure, because it is wording; a mapping binds
// the ENVELOPE, the thing it "renders from" per §2, and a failure is a failed
// delivery, because it decides what an incident tool does (§6).
//
// ⛔ EVERY INTERPOLATED VALUE IS JSON-ESCAPED, WITH NO OPT-OUT, AND ONLY JSON-ESCAPED.
// It goes through jsonStringContent ALONE. `newBinder`'s `raw` case escapes for Slack
// first and JSON second, which would put `&lt;` into an incident tool's title: a
// vendor is not Slack, and a value it receives must read as the value. A label
// holding `"`, `\`, a newline or `</script>` therefore lands as a JSON string's
// content and can never become structure — the property that lets an alert label,
// which anyone who can fire a metric can write, sit inside an operator's JSON.
//
// ⛔ A SECRET IS A REFERENCE UNTIL THE MOMENT OF SENDING. `{{ secrets.routing_key }}`
// renders to a SENTINEL — the name between U+2028 and U+2029 — and the webhook
// provider swaps the sealed value in (FillSecrets) after the delivery row has
// recorded the body. No interpolated value can forge one: jsonStringContent always
// escapes both separators (encoding/json does so unconditionally), and a mapping's own
// source may not contain them. So the stored request names the secret and never
// holds it, and a receiver's label cannot make oto send a secret somewhere it was not
// written.

// Sentinel delimiters for a secret reference. Both are escaped by every JSON encoder
// oto runs, so the raw rune can only come from a `secrets.<name>` binding.
const (
	secretOpen = '\u2028'
	secretShut = '\u2029'
)

// MaxMappingBodyBytes is a mapped body's ceiling — the envelope's own (§L.6), because
// a mapping is the same request to the same receiver in another shape.
const MaxMappingBodyBytes = 1 << 20

// maxMappingHeaderBytes bounds one rendered header value.
const maxMappingHeaderBytes = 4096

// A MappingProblem is one reason a mapping cannot be saved, placed on the source that
// caused it: `body`, `facts/<fact>` or `headers/<name>`.
type MappingProblem struct {
	Field   string
	Message string
}

// Mapping is one compiled payload mapping.
type Mapping struct {
	body    *liquid.Template
	facts   map[string]*liquid.Template
	headers []mappingHeader
	// secrets is every name the mapping reads as `secrets.<name>`, sorted. The
	// Connection must hold each of them (checked at save) and the provider fills
	// exactly these at send.
	secrets []string
}

type mappingHeader struct {
	name  string
	value *liquid.Template
}

// Secrets is every secret name the mapping references, sorted.
func (m *Mapping) Secrets() []string { return append([]string(nil), m.secrets...) }

// CompileMapping parses every source in a mapping. It does NOT prove the mapping
// renders — Liquid reports an unknown filter at render time — which is why the save
// gate renders it against an envelope for every fact.
func CompileMapping(doc domain.PayloadMapping) (*Mapping, []MappingProblem) {
	var probs []MappingProblem
	refs := map[string]bool{}
	compile := func(field, src string) *liquid.Template {
		t, names, err := compileMappingSource(src)
		if err != nil {
			probs = append(probs, MappingProblem{Field: field, Message: err.Error()})
			return nil
		}
		for _, n := range names {
			refs[n] = true
		}
		return t
	}

	m := &Mapping{facts: map[string]*liquid.Template{}}
	if strings.TrimSpace(doc.Body) == "" {
		probs = append(probs, MappingProblem{Field: "body",
			Message: "a mapping needs a body: it is what every fact without its own body renders"})
	} else {
		m.body = compile("body", doc.Body)
	}
	for _, fact := range sortedKeys(doc.Facts) {
		if !domain.IsMappingFact(fact) {
			probs = append(probs, MappingProblem{Field: "facts/" + fact,
				Message: fmt.Sprintf("%q is not a fact an envelope carries; the facts are %s",
					fact, strings.Join(domain.MappingFacts(), ", "))})
			continue
		}
		if t := compile("facts/"+fact, doc.Facts[fact]); t != nil {
			m.facts[fact] = t
		}
	}
	for _, name := range sortedKeys(doc.Headers) {
		src := doc.Headers[name]
		if strings.ContainsAny(src, "\r\n") {
			probs = append(probs, MappingProblem{Field: "headers/" + name,
				Message: "a header value may not contain a newline"})
			continue
		}
		if t := compile("headers/"+name, src); t != nil {
			m.headers = append(m.headers, mappingHeader{name: name, value: t})
		}
	}
	for n := range refs {
		m.secrets = append(m.secrets, n)
	}
	sort.Strings(m.secrets)
	if len(probs) > 0 {
		return nil, probs
	}
	return m, nil
}

// secretRef finds `secrets` read inside a Liquid expression or tag: group 2 is the
// `.name` that must follow it, group 3 the name. A `secrets` preceded by a dot or a
// bracket is somebody else's key (`alert.labels.secrets`), not the namespace.
var secretRef = regexp.MustCompile(`(^|[^A-Za-z0-9_.\]])secrets\b(\s*\.\s*([A-Za-z0-9_]+))?`)

// compileMappingSource parses one source and returns the secret names it reads.
func compileMappingSource(src string) (*liquid.Template, []string, error) {
	if len(src) > MaxSourceBytes {
		return nil, nil, fmt.Errorf("this source is %d bytes and the limit is %d", len(src), MaxSourceBytes)
	}
	if d := forDepth(src); d > MaxForDepth {
		return nil, nil, fmt.Errorf("`{%% for %%}` is nested %d deep and the limit is %d", d, MaxForDepth)
	}
	// A stray `}}` is JSON closing two objects, exactly as it is in a `raw` template.
	if msg := unbalanced(src, FormatRaw); msg != "" {
		return nil, nil, errors.New(msg)
	}
	if strings.ContainsRune(src, secretOpen) || strings.ContainsRune(src, secretShut) {
		return nil, nil, errors.New("a mapping may not contain U+2028 or U+2029 literally; " +
			"oto uses them to mark a secret reference")
	}
	var names []string
	for _, seg := range liquidSegments(src) {
		for _, m := range secretRef.FindAllStringSubmatch(seg, -1) {
			if m[2] == "" {
				return nil, nil, errors.New("`secrets` is read one secret at a time, as `secrets.<name>`")
			}
			if !domain.ValidSecretName(m[3]) {
				return nil, nil, fmt.Errorf("`secrets.%s` is not a secret name: a name is lower-case "+
					"letters, digits and underscores, starting with a letter", m[3])
			}
			names = append(names, m[3])
		}
	}
	// The SOURCE is sanitised, as Compile does for a template: a private-use
	// codepoint typed into a mapping would otherwise reach the vendor raw.
	t, err := mappingEngineOf().ParseString(sanitise(src))
	if err != nil {
		return nil, nil, fmt.Errorf("this source does not parse: %s", liquidMessage(err))
	}
	return t, names, nil
}

// mappingEngine is the engine a payload mapping renders on, built once.
var (
	mappingOnce   sync.Once
	mappingEngine *liquid.Engine
)

// mappingEngineOf is engineOf with the four TEXT filters taught that a mapping's
// values are JSON string CONTENT.
//
// ⛔ A BOUND VALUE IS ALREADY ESCAPED, AND A FILTER THAT CUTS OR CASE-MAPS THE ESCAPED
// FORM CAN BREAK IT. `truncate_runes: 3` over `ab\"cd` stops after the backslash and
// leaves a lone `\` that escapes the mapping's own closing quote — the body is no
// longer JSON, or worse, is different JSON. So each filter unescapes the value,
// applies the template engine's own transform to the TEXT, and escapes the result
// again. A value that does not unescape (a literal the mapping typed with a bare
// quote in it) gets the transform as it stands, which is what the author wrote.
//
// ⛔ A SECRET SENTINEL IS NOT ROUND-TRIPPED. Its separators would come back escaped
// as `\u2028`, the reference would silently become text and no secret would be sent,
// so a value carrying one gets the transform raw — which bends the name, and
// checkSentinels then refuses `{{ secrets.key | upper }}` as it always has.
func mappingEngineOf() *liquid.Engine {
	mappingOnce.Do(func() {
		e := liquid.NewBasicEngine()
		registerFilters(e)
		registerTags(e)
		e.RegisterFilter("upper", func(v any) any { return onJSONText(str(v), upperText) })
		e.RegisterFilter("lower", func(v any) any { return onJSONText(str(v), lowerText) })
		e.RegisterFilter("capitalise", func(v any) any { return onJSONText(str(v), capitaliseText) })
		e.RegisterFilter("truncate_runes", func(v any, n int) any {
			return onJSONText(str(v), func(s string) string { return truncateRunes(s, n) })
		})
		mappingEngine = e
	})
	return mappingEngine
}

// onJSONText applies f to the text a piece of JSON string content spells.
func onJSONText(s string, f func(string) string) string {
	if strings.ContainsRune(s, secretOpen) || strings.ContainsRune(s, secretShut) {
		return f(s)
	}
	var text string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &text); err != nil {
		return f(s)
	}
	return jsonStringContent(f(text))
}

// liquidSegments returns the inside of every `{{ }}` and `{% %}` in a source that
// `unbalanced` has already accepted. A JSON key spelled "secrets" in the literal text
// is the operator's own JSON, not a read of the namespace.
func liquidSegments(src string) []string {
	var out []string
	for i := 0; i+1 < len(src); i++ {
		two := src[i : i+2]
		if two != "{{" && two != "{%" {
			continue
		}
		closer := "}}"
		if two == "{%" {
			closer = "%}"
		}
		j := strings.Index(src[i+2:], closer)
		if j < 0 {
			break
		}
		out = append(out, src[i+2:i+2+j])
		i += 2 + j + 1
	}
	return out
}

// MappingOutput is one rendered request.
type MappingOutput struct {
	// Fact is the envelope's `reason`, "" when it carried none.
	Fact string
	// Field names the source that rendered the body: `body`, or `facts/<fact>`.
	Field string
	// Body is what that source rendered, trimmed. It is set even when Render
	// refuses it, so a dead delivery can carry the attempt that failed.
	Body string
	// Headers are the rendered headers, by name; one that rendered empty is absent.
	Headers map[string]string
}

// Render renders the request for one envelope.
//
// The body must be ONE JSON OBJECT of at most MaxMappingBodyBytes, and every header
// value must be one line. Anything else is an error, and the caller's answer to an
// error is a failed delivery — never the envelope instead.
func (m *Mapping) Render(envelope json.RawMessage) (MappingOutput, error) {
	env, fact, err := bindEnvelope(envelope)
	if err != nil {
		return MappingOutput{}, err
	}
	secrets := make(map[string]any, len(m.secrets))
	for _, n := range m.secrets {
		secrets[n] = string(secretOpen) + n + string(secretShut)
	}
	// ⚠️ `secrets` SHADOWS an envelope key of the same name. v1 has none, and a v2
	// that added one would be read as `envelope.secrets` in a mapping written for it.
	env["secrets"] = secrets

	out := MappingOutput{Fact: fact, Field: "body"}
	tpl := m.body
	if t, ok := m.facts[fact]; ok && fact != "" {
		tpl, out.Field = t, "facts/"+fact
	}
	body, err := renderMapping(tpl, env)
	out.Body = body
	if err != nil {
		return out, err
	}
	if len(body) > MaxMappingBodyBytes {
		return out, fmt.Errorf("the mapping rendered %d bytes and the limit is %d", len(body), MaxMappingBodyBytes)
	}
	if !strings.HasPrefix(body, "{") || !json.Valid([]byte(body)) {
		return out, errors.New("the mapping did not render one JSON object; interpolated values " +
			"are escaped for you, so look for a missing comma, quote or bracket in the mapping itself")
	}
	if err := checkSentinels(body); err != nil {
		return out, err
	}
	if err := m.ownRefs(SecretRefs(body)); err != nil {
		return out, err
	}

	for _, h := range m.headers {
		v, err := renderMapping(h.value, env)
		if err != nil {
			return out, fmt.Errorf("header %s: %w", h.name, err)
		}
		switch {
		case v == "":
			continue
		case strings.ContainsAny(v, "\r\n"):
			return out, fmt.Errorf("header %s rendered a newline", h.name)
		case len(v) > maxMappingHeaderBytes:
			return out, fmt.Errorf("header %s rendered %d bytes and the limit is %d", h.name, len(v), maxMappingHeaderBytes)
		}
		if err := checkSentinels(v); err != nil {
			return out, fmt.Errorf("header %s: %w", h.name, err)
		}
		if err := m.ownRefs(SecretRefs(v)); err != nil {
			return out, fmt.Errorf("header %s: %w", h.name, err)
		}
		if out.Headers == nil {
			out.Headers = map[string]string{}
		}
		out.Headers[h.name] = v
	}
	return out, nil
}

// ownRefs refuses a rendered reference the mapping's SOURCE never wrote. The escape
// already makes a value-forged sentinel impossible; this is the second wall, so that a
// reference reaching FillSecrets is always one the operator typed as
// `secrets.<name>`, whatever a future binder or filter lets through.
func (m *Mapping) ownRefs(refs []string) error {
	for _, n := range refs {
		i := sort.SearchStrings(m.secrets, n)
		if i >= len(m.secrets) || m.secrets[i] != n {
			return fmt.Errorf("the rendered request references secrets.%s, which the mapping does not read; "+
				"a secret reference can only come from the mapping itself", n)
		}
	}
	return nil
}

// renderMapping runs one source and strips oto's private-use marks from the output:
// `| bold` and friends mean something to a Dialect and nothing to a vendor's JSON.
func renderMapping(t *liquid.Template, env map[string]any) (string, error) {
	if t == nil {
		return "", errors.New("the mapping is not compiled")
	}
	raw, err := t.Render(withBudget(Input(env)))
	if err != nil {
		return "", fmt.Errorf("the mapping did not render: %s", liquidMessage(err))
	}
	return strings.TrimSpace(stripMarks(string(raw))), nil
}

func stripMarks(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= '\uE000' && r <= '\uF8FF') || r >= 0xF0000 {
			return -1
		}
		return r
	}, s)
}

// bindEnvelope decodes the envelope into Liquid bindings, every string leaf
// JSON-escaped, and returns its `reason` unescaped.
func bindEnvelope(envelope json.RawMessage) (map[string]any, string, error) {
	dec := json.NewDecoder(bytes.NewReader(envelope))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "", fmt.Errorf("the envelope is not JSON: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, "", errors.New("the envelope is not a JSON object")
	}
	fact, _ := obj["reason"].(string)
	bound, _ := bindValue(obj).(map[string]any)
	return bound, fact, nil
}

// bindValue is the mapping's binder: strings through jsonStringContent and nothing
// else, numbers as numbers, objects and arrays walked.
func bindValue(v any) any {
	switch t := v.(type) {
	case string:
		return mappingText(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		f, _ := t.Float64()
		return f
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = bindValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		// ⛔ KEYS ARE ESCAPED TOO. `{{ alert.annotations }}` renders a map's keys
		// into the body as well as its values, and an annotation's NAME is as
		// writable as its value: an unescaped key holding `","event_action":"x` would
		// be structure, and one spelling the sentinel separators a forged reference.
		for k, e := range t {
			out[mappingText(k)] = bindValue(e)
		}
		return out
	default:
		return t
	}
}

// mappingText is one envelope string as it may appear between a JSON string's quotes.
// The two sentinel runes are dropped before escaping as well as escaped by it: the
// escape alone already makes a forged reference impossible, and this keeps that true
// if the encoder underneath ever changed its mind.
func mappingText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == secretOpen || r == secretShut {
			return ' '
		}
		return r
	}, sanitise(s))
	return jsonStringContent(s)
}

// checkSentinels refuses a rendered value whose secret references a filter has bent
// out of shape (`{{ secrets.key | upper }}`), which FillSecrets could not resolve.
func checkSentinels(s string) error {
	_, err := fillSentinels(s, func(string) (string, bool) { return "", true })
	return err
}

// SecretRefs is every secret name a rendered body or header value references.
func SecretRefs(s string) []string {
	var names []string
	_, _ = fillSentinels(s, func(n string) (string, bool) {
		names = append(names, n)
		return "", true
	})
	return names
}

// fillSentinels replaces every reference in s with what fill answers for its name.
func fillSentinels(s string, fill func(name string) (string, bool)) (string, error) {
	if !strings.ContainsRune(s, secretOpen) && !strings.ContainsRune(s, secretShut) {
		return s, nil
	}
	var b strings.Builder
	for {
		i := strings.IndexRune(s, secretOpen)
		if i < 0 {
			if strings.ContainsRune(s, secretShut) {
				return "", errors.New("a secret reference is malformed; read a secret as `{{ secrets.<name> }}` with no filter")
			}
			b.WriteString(s)
			return b.String(), nil
		}
		rest := s[i+len(string(secretOpen)):]
		j := strings.IndexRune(rest, secretShut)
		if j < 0 {
			return "", errors.New("a secret reference is malformed; read a secret as `{{ secrets.<name> }}` with no filter")
		}
		name := rest[:j]
		if !domain.ValidSecretName(name) || strings.ContainsRune(s[:i], secretShut) {
			return "", errors.New("a secret reference was altered; read a secret as `{{ secrets.<name> }}` with no filter")
		}
		v, ok := fill(name)
		if !ok {
			return "", fmt.Errorf("the mapping references secrets.%s, which this connection does not hold", name)
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = rest[j+len(string(secretShut)):]
	}
}

// FillSecrets swaps every secret reference in a mapped request for the sealed value
// the Connection holds — in the body as JSON string content, in a header verbatim.
//
// ⛔ IT RUNS IN THE PROVIDER, AT THE MOMENT OF SENDING, AND NOWHERE ELSE. The delivery
// row has already recorded the body with the references in it, so the stored request
// never holds a secret. A reference to a secret the Connection no longer holds is an
// error, and the delivery fails rather than sending an empty key.
func FillSecrets(
	body []byte, headers map[string]string, secrets map[string]string,
) ([]byte, map[string]string, error) {
	filled, err := fillSentinels(string(body), func(n string) (string, bool) {
		v, ok := secrets[n]
		return jsonStringContent(v), ok && v != ""
	})
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]string, len(headers))
	for k, h := range headers {
		v, err := fillSentinels(h, func(n string) (string, bool) {
			v, ok := secrets[n]
			return v, ok && v != ""
		})
		if err != nil {
			return nil, nil, fmt.Errorf("header %s: %w", k, err)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, nil, fmt.Errorf("header %s: a secret it references holds a line break", k)
		}
		out[k] = v
	}
	return []byte(filled), out, nil
}

// compiledMappings keeps parsed mappings across deliveries, keyed by the stored
// bytes.
//
// ⚠️ IT IS CAPPED, NOT "BOUNDED BY WHAT AN ORG HAS SAVED". Nothing evicts an entry:
// every edit leaves its predecessor's bytes behind, and a parse that failed is cached
// as its error, so the key space only grows. Past maxCompiledMappings entries the
// whole cache is dropped and refilled on demand — recompiling is cheap, and an
// unbounded map keyed by bytes is not.
var (
	compiledMappings     sync.Map // string(raw) -> *Mapping or error
	compiledMappingCount atomic.Int64
)

const maxCompiledMappings = 1024

// storeCompiledMapping caches v under key, emptying the cache first when it is full.
func storeCompiledMapping(key string, v any) {
	if compiledMappingCount.Add(1) > maxCompiledMappings {
		compiledMappings.Range(func(k, _ any) bool {
			compiledMappings.Delete(k)
			return true
		})
		compiledMappingCount.Store(1)
	}
	compiledMappings.Store(key, v)
}

// CompiledMapping parses and compiles a stored mapping, reusing the work across
// deliveries.
func CompiledMapping(raw json.RawMessage) (*Mapping, error) {
	key := string(raw)
	if hit, ok := compiledMappings.Load(key); ok {
		switch v := hit.(type) {
		case *Mapping:
			return v, nil
		case error:
			return nil, v
		}
	}
	doc, err := domain.ParsePayloadMapping(raw)
	if err != nil {
		storeCompiledMapping(key, err)
		return nil, err
	}
	m, probs := CompileMapping(doc)
	if len(probs) > 0 {
		msgs := make([]string, 0, len(probs))
		for _, p := range probs {
			msgs = append(msgs, p.Field+": "+p.Message)
		}
		err := errors.New("the payload mapping does not compile: " + strings.Join(msgs, "; "))
		storeCompiledMapping(key, err)
		return nil, err
	}
	storeCompiledMapping(key, m)
	return m, nil
}
