package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// ADR 0055 §2: A WEBHOOK CONNECTION MAY CARRY A PAYLOAD MAPPING (git-bug 2205620).
//
// A payload mapping is a document an operator writes onto a webhook Connection that
// turns the `oto.notification.v1` envelope into the request a particular incident
// tool expects: a body per fact, optional headers, and optionally where in a 2xx
// response the tool names the incident it opened. A Connection without one sends
// the plain envelope, byte for byte as before.
//
// ⛔ IT IS DESTINATION SETUP, NOT WORDING, AND IT IS NOT A NotificationTemplate
// (ADR 0055 §6). A template falls back to oto's own card on any failure because a
// template only decides how a message READS; a mapping decides what an incident
// tool DOES, so a mapping that fails is a failed delivery — dead, `config_invalid`,
// visible and retryable — and NEVER the plain envelope instead, which the vendor
// could not parse. That would turn a broken mapping into a missing incident.
//
// ⛔ IT NEVER HOLDS A SECRET. A vendor key that has to travel in the body (PagerDuty's
// `routing_key`) is a sealed credential on the Connection, which the mapping names
// as `{{ secrets.routing_key }}`; oto fills it in at the moment of sending, after the
// delivery row has recorded the body. A mapping is therefore safe to export to the
// catalog and to show in Settings, and the stored request never carries the key.
//
// ⭐ oto'S OWN CODE SENDS NO COMMAND (ADR 0055 §4). Nothing here knows what `quiet`
// means to a vendor; a mapping that turns it into a resolve is a rule the operator
// wrote, keyed on a fact oto stated.

// PayloadMapping is the stored document, `channel_connections.payload_mapping`.
//
// Every source in it is Liquid over the envelope, on the engine NotificationTemplates
// use, with every interpolated value JSON-escaped and no way to opt out.
type PayloadMapping struct {
	// Body renders the request body for every fact Facts does not name. It is
	// REQUIRED: ADR 0055 grants a mapping no way to decline a fact, so every one of
	// the MappingFacts must render to something.
	Body string `json:"body"`
	// Facts overrides Body for the named facts — the envelope's `reason`.
	Facts map[string]string `json:"facts,omitempty"`
	// Headers are request headers, name to a Liquid value. The names are held to the
	// webhook config's header rules: no `Authorization` (the token is the Connection's
	// sealed credential) and nothing under `X-Oto-*`.
	Headers map[string]string `json:"headers,omitempty"`
	// Response names where in a 2xx JSON response the external incident's link and id
	// are found. When it is present the default top-level `external_url` /
	// `external_id` keys are NOT read (git-bug 506ff21's governing comment).
	Response *MappingResponse `json:"response,omitempty"`
}

// MappingResponse is a pair of path expressions over a 2xx JSON response body
// (gjson syntax: `data.incident.url`, `incidents.0.id`). An empty one reads nothing.
type MappingResponse struct {
	ExternalURL string `json:"external_url,omitempty"`
	ExternalID  string `json:"external_id,omitempty"`
}

// Bounds on a mapping. Each is checked when the mapping is saved, and the source
// limits again by the engine on every compile.
const (
	// MaxPayloadMappingBytes bounds the stored document.
	MaxPayloadMappingBytes = 64 << 10
	// MaxMappingHeaders bounds how many headers a mapping sets.
	MaxMappingHeaders = 16
	// MaxMappingPathLength bounds a response path expression.
	MaxMappingPathLength = 256
	// MaxMappingSecrets bounds how many named secrets a Connection seals for its
	// mapping.
	MaxMappingSecrets = 16
	// MaxMappingSecretBytes bounds one secret's value.
	MaxMappingSecretBytes = 4096
)

// MappingSecretsKind is the `channel_credentials.kind` of a Connection's mapping
// secrets: ONE sealed row whose values are name → secret, in the Connection's
// `mapping_credential_id` slot (migration 00090).
const MappingSecretsKind = "webhook_mapping_secrets"

// mappingFacts is every fact an envelope can carry, by its `reason`, in the order
// `notification/domain.allReasons` declares them. A mapping must render every one.
//
// ⛔ IT IS A COPY, AND A TEST HOLDS IT TO THE ORIGINAL. This package cannot import
// `notification/domain` (channels never reaches into notification), and the
// envelope's `reason` is that vocabulary verbatim — so
// `notification/service.TestEveryReasonIsAMappingFact` fails the moment a Reason is
// added there and not here, which is the moment a mapping would otherwise meet a
// fact it was never checked against.
var mappingFacts = []string{
	"fired", "all_resolved",
	"repeat", "suppressed", "unsuppressed", "expired",
	"refired", "acked", "unacked", "snoozed", "unsnoozed",
	"enriched", "rule_changed", "comment",
	"digest",
	"drawn", "case_added", "case_removed", "quiet", "active_again", "finding",
	"remedy_proposed", "remedy_approved", "remedy_declined", "remedy_expired", "remedy_executed", "remedy_failed",
}

// MappingFacts returns every fact a mapping is rendered for, freshly copied.
func MappingFacts() []string { return slices.Clone(mappingFacts) }

// IsMappingFact reports whether fact is one an envelope can carry.
func IsMappingFact(fact string) bool { return slices.Contains(mappingFacts, fact) }

// IncidentFact reports whether fact is one of the twelve Incident facts, whose
// envelope carries `incident` and no `group`.
func IncidentFact(fact string) bool {
	switch fact {
	case "drawn", "case_added", "case_removed", "quiet", "active_again", "finding":
		return true
	}
	return RemedyFact(fact)
}

// RemedyFact reports whether fact is one of the six Remedy transitions (ADR 0054 §2), whose
// envelope's `incident` also carries `remedy`.
func RemedyFact(fact string) bool {
	switch fact {
	case "remedy_proposed", "remedy_approved", "remedy_declined", "remedy_expired", "remedy_executed", "remedy_failed":
		return true
	}
	return false
}

// secretNamePattern is what a mapping secret may be called: a Liquid identifier a
// mapping can write as `secrets.<name>` with nothing to escape.
var secretNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidSecretName reports whether name may name a mapping secret.
func ValidSecretName(name string) bool { return secretNamePattern.MatchString(name) }

// ParsePayloadMapping reads a stored or submitted mapping, refusing an unknown key —
// a misspelt `fatcs` that silently did nothing would send the default body for a
// fact the operator believes they wrote a body for.
//
// It checks SHAPE only. Whether every source renders against every fact is the
// save-time gate's question (`channels/service.ValidateMapping`), and whether the
// headers are permitted is the webhook provider's.
func ParsePayloadMapping(raw json.RawMessage) (PayloadMapping, error) {
	if len(raw) > MaxPayloadMappingBytes {
		return PayloadMapping{}, fmt.Errorf("the payload mapping is %d bytes and the limit is %d",
			len(raw), MaxPayloadMappingBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m PayloadMapping
	if err := dec.Decode(&m); err != nil {
		return PayloadMapping{}, fmt.Errorf("the payload mapping is not a mapping document: %w", err)
	}
	if dec.More() {
		return PayloadMapping{}, errors.New("the payload mapping is followed by more JSON")
	}
	return m, nil
}

// IsNullMapping reports whether raw says "no mapping": absent, or JSON null.
func IsNullMapping(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}
