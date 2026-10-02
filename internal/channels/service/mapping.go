package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/providers/webhook"
	"github.com/thulasiram/oto/internal/channels/render/webhookjson"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0055 §2: A WEBHOOK CONNECTION'S PAYLOAD MAPPING, AT THE TWO PLACES IT IS JUDGED
// (git-bug 2205620) — when it is SAVED, against an envelope for every fact, and when a
// delivery is CLAIMED, against the envelope that delivery carries.
//
// The rendering itself is `template`'s (template/mapping.go); this file is what each
// caller does with the answer. At save a mapping that does not render cannot be
// stored, and the 422 names the fact. At claim a mapping that does not render is a
// failed delivery — dead, `config_invalid`, retryable from the audit once fixed — and
// the plain envelope is never sent instead.

// maxMappingViolations caps how many refusals one save reports. Twenty facts times
// three shapes of one typo is sixty copies of the same sentence.
const maxMappingViolations = 10

// credentialHeaderName is a header name that reads as a credential (provisional,
// pending an owner decision on the exact list).
var credentialHeaderName = regexp.MustCompile(`(?i)key|token|secret|auth|password`)

// headerSecretRef is a `secrets.<name>` read inside a Liquid output.
var headerSecretRef = regexp.MustCompile(`\{\{[^}]*\bsecrets\s*\.\s*[a-z]`)

// mappingField roots a mapping problem under the request field it came in on.
const mappingField = "payload_mapping"

// ValidateMapping is the SAVE-TIME GATE for a payload mapping (ADR 0055 §2: "checked
// before it is saved ... a mapping that does not render cannot be saved").
//
// ⛔ IT RENDERS. It builds the oto.notification.v1 envelope for EVERY fact — one
// representative view per fact plus the hostile and zero-value shapes
// (template.MappingFixtures) — through the real webhook renderer, renders the mapping
// against each, and refuses with the failing fact named. Parsing would accept
// `{{ x | no_such_filter }}`, which fails at render.
//
// secretNames is what the Connection will hold once this write lands; a mapping
// that references any other name is refused, because at send time it would fail
// every delivery.
func ValidateMapping(raw json.RawMessage, secretNames []string) error {
	doc, err := domain.ParsePayloadMapping(raw)
	if err != nil {
		return mappingInvalid(errs.Violation{Field: mappingField, Code: "invalid", Message: err.Error()})
	}

	var violations []errs.Violation
	add := func(field, code, message string) {
		if len(violations) < maxMappingViolations {
			violations = append(violations, errs.Violation{
				Field: mappingField + "/" + field, Code: code, Message: message,
			})
		}
	}

	if len(doc.Headers) > domain.MaxMappingHeaders {
		add("headers", "max_properties", fmt.Sprintf("a mapping sets at most %d headers", domain.MaxMappingHeaders))
	}
	// The same rules a webhook channel's static headers are held to — no
	// `Authorization` (a vendor's token is the Connection's sealed credential), nothing
	// under `X-Oto-*` — re-rooted under the mapping.
	if herr := webhook.CheckHeaders(doc.Headers); herr != nil {
		if e, ok := errs.As(herr); ok {
			for _, v := range e.Violations {
				add(v.Field, v.Code, v.Message)
			}
		}
	}
	// A header NAMED like a credential carries one, and a credential is sealed: one
	// written into the mapping as text is stored in the clear, returned by every read
	// of the Connection and copied into every delivery row.
	for _, name := range slices.Sorted(maps.Keys(doc.Headers)) {
		if credentialHeaderName.MatchString(name) && !headerSecretRef.MatchString(doc.Headers[name]) {
			add("headers/"+name, "secret_required", fmt.Sprintf(
				"%s reads as a credential, so its value must be a sealed secret: write "+
					"`{{ secrets.<name> }}` and add the value under mapping_secrets", name))
		}
	}
	if r := doc.Response; r != nil {
		for field, path := range map[string]string{"external_url": r.ExternalURL, "external_id": r.ExternalID} {
			if msg := checkPath(path); msg != "" {
				add("response/"+field, "pattern", msg)
			}
		}
		if r.ExternalURL == "" && r.ExternalID == "" {
			add("response", "min_properties",
				"name a path for external_url, external_id or both, or leave response out")
		}
	}

	m, probs := template.CompileMapping(doc)
	for _, p := range probs {
		add(p.Field, "invalid", p.Message)
	}
	if m != nil {
		held := map[string]bool{}
		for _, n := range secretNames {
			held[n] = true
		}
		for _, n := range m.Secrets() {
			if !held[n] {
				add("body", "missing_secret", fmt.Sprintf(
					"the mapping references secrets.%s, and this connection holds no secret of that name — "+
						"add it under mapping_secrets", n))
			}
		}
	}
	if len(violations) > 0 {
		return mappingInvalid(violations...)
	}

	for _, fx := range template.MappingFixtures() {
		envelope, err := sampleEnvelope(fx)
		if err != nil {
			return err
		}
		out, rerr := m.Render(envelope)
		if rerr != nil {
			add(out.Field, "render", fmt.Sprintf("%s (fixture %q): %s", factOf(out, fx), fx.Name, rerr.Error()))
		}
	}
	if len(violations) > 0 {
		return mappingInvalid(violations...)
	}
	return nil
}

func factOf(out template.MappingOutput, fx template.Fixture) string {
	if out.Fact != "" {
		return out.Fact
	}
	return fx.View().Reason
}

func mappingInvalid(violations ...errs.Violation) error {
	return errs.Validation("payload_mapping_invalid",
		fmt.Sprintf("the payload mapping cannot be saved: %d problem(s)", len(violations)), violations...)
}

// checkPath refuses a response path that is too long or carries a control
// character; its SYNTAX is gjson's, and a path that matches nothing reads as absent.
func checkPath(path string) string {
	if path == "" {
		return ""
	}
	if len(path) > domain.MaxMappingPathLength {
		return fmt.Sprintf("a response path is at most %d bytes", domain.MaxMappingPathLength)
	}
	if strings.IndexFunc(path, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return "a response path is printable text"
	}
	return ""
}

// sampleInstant renders a fixture that carries no instant of its own.
var sampleInstant = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

// sampleEnvelope is the envelope the webhook renderer builds for one fixture — the
// exact bytes a real delivery of that fact would carry, so a mapping that passes here
// has been rendered against the real shape and not a hand-written imitation.
func sampleEnvelope(fx template.Fixture) (json.RawMessage, error) {
	at := fx.At()
	if at.IsZero() {
		at = sampleInstant
	}
	msg, err := webhookjson.New(clock.NewFake(at)).Render(context.Background(), fx.View(), domain.RenderOptions{
		Mode: domain.ModePostRoot, MaxInstances: 10, BaseURL: "https://oto.example",
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.KindInternal, "mapping_sample_envelope",
			"oto could not build the sample envelope "+fx.Name)
	}
	return msg.Payload, nil
}

// MappingFailure is a payload mapping that did not render for one delivery. It is
// the configuration's fault and not oto's, so it is a `config_invalid` provider
// error — terminal, flagged on the channel, retryable from the audit once fixed —
// and its message names the fact and what went wrong, so the dead row explains
// itself.
//
// ⭐ IT IS THE OUTER ERROR AND THE CLASSIFIED ONE IS INSIDE IT. The dispatcher
// records `err.Error()` on the dead row, and a bare *domain.Error says only
// "webhook: payload_mapping_invalid"; the class is still found by errors.As.
type MappingFailure struct {
	Fact   string
	Field  string
	Detail string
}

// Unwrap is the classification: `config_invalid`, terminal, flagged on the channel.
func (e *MappingFailure) Unwrap() error {
	return &domain.Error{Class: domain.ClassConfigInvalid, Provider: "webhook", Code: "payload_mapping_invalid"}
}

// Error is the sentence the dead delivery row carries.
func (e *MappingFailure) Error() string {
	fact := e.Fact
	if fact == "" {
		fact = "a fact with no reason"
	}
	return fmt.Sprintf("the connection's payload mapping did not render for %s (%s): %s; "+
		"the plain envelope was not sent in its place", fact, e.Field, e.Detail)
}

// Mapper applies a Connection's payload mapping to a rendered envelope. It is the
// notification dispatcher's `PayloadMapper` and the channel tester's.
type Mapper struct{}

// NewMapper builds the mapper.
func NewMapper() *Mapper { return &Mapper{} }

// Map renders msg's envelope through the mapping.
//
// On success the message's Payload is the mapped body — with any `secrets.<name>`
// still a reference, filled by the provider at send — its Hash is that body's, its
// Headers are the mapping's, and Mapped is set. On failure it returns the ATTEMPT
// beside the error (a JSON string when the attempt was not JSON), so the dead
// delivery carries what the mapping produced; the error is a *MappingFailure, which
// unwraps to a `config_invalid` *domain.Error.
func (*Mapper) Map(
	_ context.Context, raw json.RawMessage, msg domain.RenderedMessage,
) (domain.RenderedMessage, error) {
	m, err := template.CompiledMapping(raw)
	if err != nil {
		return domain.RenderedMessage{}, mappingError(&MappingFailure{Field: "payload_mapping", Detail: err.Error()})
	}
	out, err := m.Render(msg.Payload)
	if err != nil {
		attempt := attemptOf(out.Body, msg)
		return attempt, mappingError(&MappingFailure{Fact: out.Fact, Field: out.Field, Detail: err.Error()})
	}
	return mapped(msg, json.RawMessage(out.Body), out.Headers), nil
}

func mappingError(f *MappingFailure) error { return f }

func mapped(msg domain.RenderedMessage, body json.RawMessage, headers map[string]string) domain.RenderedMessage {
	sum := sha256.Sum256(body)
	meta := maps.Clone(msg.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	meta["payload_mapping"] = "applied"
	return domain.RenderedMessage{
		Fallback: msg.Fallback,
		Summary:  msg.Summary,
		Payload:  body,
		Hash:     hex.EncodeToString(sum[:]),
		Metadata: meta,
		Mapped:   true,
		Headers:  headers,
	}
}

// attemptOf is a refused render as a delivery row can hold it: the body itself when
// it parsed, a JSON string of it when it did not, and nothing when nothing rendered.
func attemptOf(body string, msg domain.RenderedMessage) domain.RenderedMessage {
	if body == "" {
		return domain.RenderedMessage{}
	}
	payload := json.RawMessage(body)
	if !json.Valid(payload) {
		quoted, err := json.Marshal(truncate(body, 64<<10))
		if err != nil {
			return domain.RenderedMessage{}
		}
		payload = quoted
	}
	return mapped(msg, payload, nil)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// mapForChannel applies a Connection's mapping when it has one, and is the identity
// otherwise — the tester's half of what the dispatcher does at claim time.
func mapForChannel(ctx context.Context, conn domain.Connection, msg domain.RenderedMessage) (domain.RenderedMessage, error) {
	if domain.IsNullMapping(conn.PayloadMapping) {
		return msg, nil
	}
	return (&Mapper{}).Map(ctx, conn.PayloadMapping, msg)
}
