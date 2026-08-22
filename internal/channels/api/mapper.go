package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/id"
)

// capabilityNames maps the Capability bitset onto the contract's string enum.
//
// The bits are oto's internal negotiation currency; the strings are the wire. The
// mapping lives in exactly one place so a new capability cannot appear on the
// wire without appearing here.
//
// ⛔ `{domain.CapBroadcast, "broadcast"}` WAS HERE AND IS DELETED (git-bug
// 7570090). The bit is gone from the bitset, so nothing can set it and no
// descriptor can serve the name. The RETIRED BIT POSITION is still held open in
// `channels/domain/ports.go` — the bitmask is persisted — but a retired bit has no
// wire name, and giving it one would put a capability on the contract that no
// provider can advertise. `broadcast` may linger in the contract's capability enum
// until the OpenAPI catches up; an enum admits more than any descriptor serves,
// which is why that direction is safe and this one is not.
var capabilityNames = []struct {
	bit  domain.Capability
	name string
}{
	{domain.CapThreading, "threading"},
	{domain.CapAmend, "amend"},
	{domain.CapRichLayout, "rich_layout"},
	{domain.CapInteractive, "interactive"},
	{domain.CapDedupeKey, "dedupe_key"},
}

func capabilityList(c domain.Capability) []string {
	out := make([]string, 0, len(capabilityNames))
	for _, cn := range capabilityNames {
		if c&cn.bit != 0 {
			out = append(out, cn.name)
		}
	}
	return out
}

// descriptorDTO maps one provider descriptor onto the wire.
//
// ⛔ `ConfigSchema` is copied through UNPARSED. Round-tripping it through a Go
// map would reorder keys and normalise numbers, and the settings form is
// generated from these exact bytes — the whole point of §L.5 is that there is no
// second copy of the rules.
func descriptorDTO(d domain.Descriptor) ChannelTypeDTO {
	renderers := make([]string, 0, len(d.Renderers))
	for _, r := range d.Renderers {
		renderers = append(renderers, string(r))
	}
	// ⛔ `make` + `append`, NOT `append([]string(nil), ...)`. That idiom returns
	// `nil` when the source is empty — nothing grows the underlying array — and
	// `credential_kinds` is legitimately empty now that every credential lives on
	// the connection: a nil slice marshals to JSON `null`, which the contract's
	// `type: array` rejects. `make(…, 0, …)` is non-nil even at length zero.
	kinds := make([]string, 0, len(d.CredentialKinds))
	kinds = append(kinds, d.CredentialKinds...)
	connKinds := make([]string, 0, len(d.ConnectionCredentialKinds))
	connKinds = append(connKinds, d.ConnectionCredentialKinds...)

	schema := d.ConfigSchema
	if len(schema) == 0 {
		// The registry refuses to register a schemaless provider, so this cannot
		// happen. `{}` rather than null keeps the response shape honest if it ever
		// does: a form generated from `{}` renders nothing, which is visibly wrong
		// rather than silently permissive.
		schema = json.RawMessage(`{}`)
	}
	connSchema := d.ConnectionConfigSchema
	if len(connSchema) == 0 {
		connSchema = json.RawMessage(`{}`)
	}

	return ChannelTypeDTO{
		Type:                      string(d.Type),
		DisplayName:               d.DisplayName,
		ConfigSchema:              schema,
		CredentialKinds:           kinds,
		ConnectionConfigSchema:    connSchema,
		ConnectionCredentialKinds: connKinds,
		Capabilities:              capabilityList(d.Capabilities),
		Renderers:                 renderers,
		RateLimitClass:            d.RateLimitClass,
	}
}

// channelDTO maps a stored destination onto the wire.
func channelDTO(i domain.Instance) ChannelDTO {
	cfg := i.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	renderer := string(i.Renderer)
	if renderer == "" {
		renderer = "default"
	}
	return ChannelDTO{
		ID:              i.ID,
		Type:            string(i.Type),
		Name:            i.Name,
		Config:          cfg,
		ConnectionID:    i.ConnectionID,
		Renderer:        renderer,
		Verbosity:       string(i.Verbosity.Normalise()),
		ThreadUpdates:   i.ThreadUpdates,
		ShowFieldEmoji:  i.ShowFieldEmoji,
		Enabled:         i.Enabled,
		HealthStatus:    string(healthOr(i.Health)),
		HealthError:     optionalString(i.HealthError),
		HealthCheckedAt: utcPtr(i.HealthCheckedAt),
		CreatedAt:       i.CreatedAt.UTC(),
		UpdatedAt:       i.UpdatedAt.UTC(),
	}
}

// connectionDTO maps a stored connection onto the wire.
//
// ⛔ It reads `CredentialKind` and `CredentialRotatedAt` and NOTHING ELSE about
// the secret, for the same structural reason channelDTO used to say about
// `domain.Instance`: `domain.Connection` has no field that could carry the
// material.
func connectionDTO(c domain.Connection) ChannelConnectionDTO {
	cfg := c.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	return ChannelConnectionDTO{
		ID:                  c.ID,
		Type:                string(c.Type),
		Name:                c.Name,
		Config:              cfg,
		CredentialKind:      optionalString(c.CredentialKind),
		CredentialRotatedAt: utcPtr(c.CredentialRotatedAt),
		CreatedAt:           c.CreatedAt.UTC(),
		UpdatedAt:           c.UpdatedAt.UTC(),
	}
}

// resolveConversationDTO maps a provider's resolved conversation onto the wire.
func resolveConversationDTO(r domain.ConversationResult) ResolveConversationDTO {
	return ResolveConversationDTO{
		ConversationID:   r.ID,
		ConversationName: r.Name,
	}
}

func healthOr(h domain.InstanceHealth) domain.InstanceHealth {
	if h == "" {
		return domain.InstanceHealthUnknown
	}
	return h
}

// testDTO maps a synthetic-card result onto the wire.
func testDTO(r domain.TestResult) ChannelTestDTO {
	out := ChannelTestDTO{
		OK:                     r.OK,
		ProviderConversationID: optionalString(r.ProviderConversationID),
		ProviderMessageID:      optionalString(r.ProviderMessageID),
		Permalink:              optionalString(r.Permalink),
		Error:                  optionalString(r.Error),
		ErrorClass:             optionalString(string(r.ErrorClass)),
		CheckedAt:              r.CheckedAt.UTC(),
	}
	return out
}

// ------------------------------------------------------------- request → domain

// toNewInstance maps a create request onto the domain command.
//
// The defaults it applies are the DDL's own, restated because the contract
// publishes them: a client that omits `enabled` expects the documented `true`
// rather than Go's zero value, and a channel created silently disabled is a
// channel whose first missed alert is blamed on oto.
func (r CreateChannelRequest) toNewInstance(caps domain.Capability) domain.NewInstance {
	return domain.NewInstance{
		Type:           domain.Type(r.Type),
		Name:           r.Name,
		Config:         r.Config,
		ConnectionID:   r.ConnectionID,
		Capabilities:   caps,
		Renderer:       domain.RendererID(stringOr(r.Renderer, "default")),
		Verbosity:      domain.Verbosity(stringOr(r.Verbosity, string(domain.VerbosityStatusChanges))),
		ThreadUpdates:  boolOr(r.ThreadUpdates, true),
		ShowFieldEmoji: boolOr(r.ShowFieldEmoji, true),
		Enabled:        boolOr(r.Enabled, true),
	}
}

// toPatch maps an update request onto the domain command.
func (r UpdateChannelRequest) toPatch() domain.InstancePatch {
	p := domain.InstancePatch{
		Name:           r.Name,
		Config:         r.Config,
		ConnectionID:   r.ConnectionID,
		ThreadUpdates:  r.ThreadUpdates,
		ShowFieldEmoji: r.ShowFieldEmoji,
		Enabled:        r.Enabled,
	}
	if r.Renderer != nil {
		v := domain.RendererID(*r.Renderer)
		p.Renderer = &v
	}
	if r.Verbosity != nil {
		v := domain.Verbosity(*r.Verbosity)
		p.Verbosity = &v
	}
	return p
}

// ------------------------------------------------------------- connections

// toNewConnection maps a create request onto the domain command.
func (r CreateChannelConnectionRequest) toNewConnection(credentialID *uuid.UUID) domain.NewConnection {
	return domain.NewConnection{
		Type:         domain.Type(r.Type),
		Name:         r.Name,
		Config:       r.Config,
		CredentialID: credentialID,
	}
}

// toPatch maps an update request onto the domain command.
func (r UpdateChannelConnectionRequest) toPatch(credential **uuid.UUID) domain.ConnectionPatch {
	return domain.ConnectionPatch{
		Name:         r.Name,
		Config:       r.Config,
		CredentialID: credential,
	}
}

// ------------------------------------------------------------------- helpers

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func stringOr(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

// -------------------------------------------------------------- templates

// notificationTemplateDTO maps a stored template onto the wire.
func notificationTemplateDTO(t domain.NotificationTemplate) NotificationTemplateDTO {
	return NotificationTemplateDTO{
		ID:        t.ID,
		Name:      t.Name,
		Provider:  t.Provider,
		Format:    t.Format,
		Source:    t.Source,
		Version:   t.Version,
		Enabled:   t.Enabled,
		CreatedAt: t.CreatedAt.UTC(),
		UpdatedAt: t.UpdatedAt.UTC(),
		DeletedAt: utcPtr(t.DeletedAt),
	}
}

// toNewNotificationTemplate maps a create request onto the domain command.
//
// `enabled` defaults to TRUE when omitted: a template somebody just wrote and
// saved is one they meant to use, and a picker full of disabled entries teaches
// nothing.
func (r CreateNotificationTemplateRequest) toNewNotificationTemplate() domain.NewNotificationTemplate {
	return domain.NewNotificationTemplate{
		// ⭐ THE ID IS MINTED HERE, not left to the repository. It is the rule
		// `platform/id` states for the whole codebase — a row's id is known before
		// the INSERT, and oto never asks Postgres for one.
		ID:       id.New(),
		Name:     r.Name,
		Provider: r.Provider,
		Format:   r.Format,
		Source:   r.Source,
		Enabled:  boolOr(r.Enabled, true),
	}
}

// toPatch maps an update request onto the domain command. Every field stays a
// pointer all the way down, because "set this to empty" and "leave this alone"
// are different requests.
func (r UpdateNotificationTemplateRequest) toPatch() domain.NotificationTemplatePatch {
	return domain.NotificationTemplatePatch{
		Name:     r.Name,
		Provider: r.Provider,
		Format:   r.Format,
		Source:   r.Source,
		Enabled:  r.Enabled,
	}
}

// patchedTemplate is the row an update WOULD produce, for the gate to judge.
//
// ⛔ THE GATE JUDGES THE WHOLE ROW, NOT THE DELTA. A source is refused or accepted
// against the FORMAT it will be parsed as, so validating only the fields a patch
// happens to carry would let two individually-legal patches build an illegal row —
// change the format today, change the source tomorrow, and neither request ever
// saw the combination it produced.
func patchedTemplate(existing domain.NotificationTemplate, p UpdateNotificationTemplateRequest) domain.NotificationTemplate {
	out := existing
	if p.Name != nil {
		out.Name = *p.Name
	}
	if p.Provider != nil {
		out.Provider = *p.Provider
	}
	if p.Format != nil {
		out.Format = *p.Format
	}
	if p.Source != nil {
		out.Source = *p.Source
	}
	return out
}
