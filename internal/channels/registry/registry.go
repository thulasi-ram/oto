package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackprovider "github.com/thulasiram/oto/internal/channels/providers/slack"
	webhookprovider "github.com/thulasiram/oto/internal/channels/providers/webhook"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/channels/render/webhookjson"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Registry is the lookup from a stored channel row to the code that serves it.
//
// It is populated once at boot and read concurrently thereafter. Registration
// after boot is permitted but is not the intended shape: "interfaces before
// implementations" (CONTEXT.md §2) means the set of providers is a deployment
// decision, made once, in one place.
type Registry struct {
	mu        sync.RWMutex
	providers map[domain.Type]domain.Provider
	renderers map[domain.RendererID]domain.Renderer
	// defaults maps a provider type to the renderer used when a channel row says
	// renderer='default' (the DDL's default value).
	defaults map[domain.Type]domain.RendererID
}

// Config configures the boot-time registry.
type Config struct {
	Clock clock.Clock
	// HTTPClient is handed to the Slack provider.
	HTTPClient *http.Client
	// AllowPrivateWebhookTargets relaxes the webhook SSRF guard. It is off by
	// default and comes from OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS (§L.5).
	AllowPrivateWebhookTargets bool
	// AllowInsecureWebhookTLS lets a webhook channel's `insecure_skip_verify` take
	// effect. It is off by default and comes from `security.allow_insecure_tls` —
	// the SAME switch that gates `alert_sources.tls_skip_verify`, because it is the
	// same question: which certificates does this deployment trust (§M2).
	AllowInsecureWebhookTLS bool
	// WebhookTransport overrides the webhook HTTP transport, for tests.
	WebhookTransport http.RoundTripper
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{
		providers: make(map[domain.Type]domain.Provider, 2),
		renderers: make(map[domain.RendererID]domain.Renderer, 2),
		defaults:  make(map[domain.Type]domain.RendererID, 2),
	}
}

// Default builds the registry oto boots with: Slack in full, and the generic
// webhook that proves the abstraction holds (R5).
//
// Adding a provider is this function plus a schema file. It is deliberately the
// only place that knows the v1 provider set, so "we ship exactly two" is a fact
// one file can be checked against.
func Default(cfg Config) *Registry {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.New()
	}

	r := New()
	r.MustRegisterProvider(slackprovider.NewProvider(slackprovider.Options{
		Clock:      clk,
		HTTPClient: cfg.HTTPClient,
	}))
	r.MustRegisterProvider(webhookprovider.NewProvider(webhookprovider.Options{
		Clock:               clk,
		AllowPrivateTargets: cfg.AllowPrivateWebhookTargets,
		AllowInsecureTLS:    cfg.AllowInsecureWebhookTLS,
		Transport:           cfg.WebhookTransport,
	}))

	r.MustRegisterRenderer(slackrender.New(clk))
	r.MustRegisterRenderer(webhookjson.New(clk))

	return r
}

// RegisterProvider adds a Provider. Registering a type twice is an error: two
// providers claiming `slack` means one of them is silently unreachable.
func (r *Registry) RegisterProvider(p domain.Provider) error {
	if p == nil {
		return errs.New(errs.KindInternal, "registry_nil_provider", "a provider is required")
	}
	d := p.Descriptor()
	if d.Type == "" {
		return errs.New(errs.KindInternal, "registry_no_type", "a provider must declare a type")
	}
	if len(d.ConfigSchema) == 0 {
		// A provider with no schema cannot validate its own config and cannot
		// render a settings form. There is no valid reason to have one.
		return errs.Newf(errs.KindInternal, "registry_no_schema",
			"provider %q must publish a config schema", d.Type)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[d.Type]; dup {
		return errs.Newf(errs.KindInternal, "registry_duplicate_provider",
			"provider %q is already registered", d.Type)
	}
	r.providers[d.Type] = p
	if len(d.Renderers) > 0 {
		r.defaults[d.Type] = d.Renderers[0]
	}
	return nil
}

// MustRegisterProvider registers a Provider or panics. Registration happens at
// boot from a fixed list, so a failure here is a programming error and a process
// that keeps running would be a process that silently cannot deliver.
func (r *Registry) MustRegisterProvider(p domain.Provider) {
	if err := r.RegisterProvider(p); err != nil {
		panic("channels/registry: " + err.Error())
	}
}

// RegisterRenderer adds a Renderer.
func (r *Registry) RegisterRenderer(rend domain.Renderer) error {
	if rend == nil {
		return errs.New(errs.KindInternal, "registry_nil_renderer", "a renderer is required")
	}
	id := rend.ID()
	if id == "" {
		return errs.New(errs.KindInternal, "registry_no_renderer_id", "a renderer must declare an id")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.renderers[id]; dup {
		return errs.Newf(errs.KindInternal, "registry_duplicate_renderer",
			"renderer %q is already registered", id)
	}
	r.renderers[id] = rend
	return nil
}

// MustRegisterRenderer registers a Renderer or panics.
func (r *Registry) MustRegisterRenderer(rend domain.Renderer) {
	if err := r.RegisterRenderer(rend); err != nil {
		panic("channels/registry: " + err.Error())
	}
}

// Provider returns the Provider for a channel type.
func (r *Registry) Provider(t domain.Type) (domain.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[t]
	if !ok {
		return nil, errs.Newf(errs.KindValidation, "unknown_channel_type",
			"there is no provider for channel type %q", t).WithViolations(errs.Violation{
			Field: "type", Code: "enum", Message: "unsupported channel type",
		})
	}
	return p, nil
}

// Renderer resolves a channel row's renderer column.
//
// The DDL's default value is the literal string "default", which resolves to the
// provider's first declared renderer. That indirection is what lets a provider
// change its default without a data migration.
func (r *Registry) Renderer(t domain.Type, id domain.RendererID) (domain.Renderer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if id == "" || id == "default" {
		def, ok := r.defaults[t]
		if !ok {
			return nil, errs.Newf(errs.KindInternal, "no_default_renderer",
				"channel type %q declares no renderer", t)
		}
		id = def
	}

	rend, ok := r.renderers[id]
	if !ok {
		return nil, errs.Newf(errs.KindValidation, "unknown_renderer",
			"there is no renderer %q", id).WithViolations(errs.Violation{
			Field: "renderer", Code: "enum", Message: "unsupported renderer",
		})
	}
	return rend, nil
}

// Descriptors returns every registered provider's descriptor, ordered by type so
// the API response and therefore the settings UI is stable.
func (r *Registry) Descriptors() []domain.Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]domain.Descriptor, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Descriptor())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// ConfigSchema returns the raw JSON Schema for a channel type.
//
// These are the SAME BYTES that validate on the server, so the settings form and
// the server can never disagree about what a valid config is. That is the whole
// argument for a schema over a hand-written DTO (§L.5).
func (r *Registry) ConfigSchema(t domain.Type) (json.RawMessage, error) {
	p, err := r.Provider(t)
	if err != nil {
		return nil, err
	}
	return p.Descriptor().ConfigSchema, nil
}

// ValidateConfig validates a stored config against its provider's schema plus the
// server-side rules a schema cannot express.
func (r *Registry) ValidateConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error {
	p, err := r.Provider(t)
	if err != nil {
		return err
	}
	return p.ValidateConfig(ctx, raw)
}

// ValidateConnectionConfig validates a stored connection config against its
// provider's ConnectionConfigSchema — the org-wide setup, not one channel's.
func (r *Registry) ValidateConnectionConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error {
	p, err := r.Provider(t)
	if err != nil {
		return err
	}
	return p.ValidateConnectionConfig(ctx, raw)
}

// Open mints a Channel for one configured destination.
func (r *Registry) Open(
	ctx context.Context, t domain.Type, cfg domain.ChannelConfig, cred domain.Credential,
) (domain.Channel, error) {
	p, err := r.Provider(t)
	if err != nil {
		return nil, err
	}
	return p.Open(ctx, cfg, cred)
}

// ResolveConversation answers "what is the id of the channel named X" (or the
// reverse), through a provider's OPTIONAL domain.ConversationResolver
// capability. A provider that does not implement it — the generic webhook has
// nothing to resolve — refuses with a validation error naming the type, rather
// than a panic on a failed type assertion.
func (r *Registry) ResolveConversation(
	ctx context.Context, t domain.Type, cred domain.Credential, query domain.ConversationQuery,
) (domain.ConversationResult, error) {
	p, err := r.Provider(t)
	if err != nil {
		return domain.ConversationResult{}, err
	}
	resolver, ok := p.(domain.ConversationResolver)
	if !ok {
		return domain.ConversationResult{}, errs.Newf(errs.KindValidation, "conversation_resolution_unsupported",
			"the %q provider cannot resolve a conversation name or id", t)
	}
	return resolver.ResolveConversation(ctx, cred, query)
}

// Types lists the registered channel types, sorted.
func (r *Registry) Types() []domain.Type {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]domain.Type, 0, len(r.providers))
	for t := range r.providers {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// TypeNames is Types rendered for a log line or an error message.
func (r *Registry) TypeNames() string {
	types := r.Types()
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}
