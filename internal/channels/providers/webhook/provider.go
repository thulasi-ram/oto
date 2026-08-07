package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// capabilities is CapRichLayout and NOTHING ELSE (§H.10).
//
// This one constant is the abstraction proof. A webhook cannot thread, cannot
// amend and cannot be clicked, and every one of those absences is handled by the
// dispatch service reading this bitset — not by a webhook-shaped branch in the
// notification module.
const capabilities = domain.CapRichLayout

// CredNone is the credential kind for an unauthenticated receiver. A webhook that
// needs auth uses `basic` or `bearer`; the secret is sealed in
// channel_credentials and is never in the config (§L.5).
const (
	CredNone   = "none"
	CredBasic  = "basic"
	CredBearer = "bearer"
)

// Provider mints generic webhook Channels.
type Provider struct {
	guard     *Guard
	clock     clock.Clock
	transport http.RoundTripper
}

// Options configures the webhook provider.
type Options struct {
	Clock clock.Clock
	// AllowPrivateTargets comes from OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS. It is off
	// by default: oto runs inside the operator's network, so an unguarded
	// operator-supplied URL is a Server-Side Request Forgery primitive (§L.5).
	AllowPrivateTargets bool
	// Transport overrides the HTTP transport, for tests.
	Transport http.RoundTripper
}

// NewProvider builds the webhook provider.
func NewProvider(o Options) *Provider {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Provider{
		guard:     NewGuard(o.AllowPrivateTargets),
		clock:     clk,
		transport: o.Transport,
	}
}

// Descriptor is static and is served verbatim by GET /api/v1/channel-types.
func (p *Provider) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:            domain.TypeWebhook,
		DisplayName:     "Webhook",
		ConfigSchema:    Schema.Raw(),
		CredentialKinds: []string{CredNone, CredBasic, CredBearer},
		Capabilities:    capabilities,
		Renderers:       []domain.RendererID{domain.RendererWebhookJSON},
		RateLimitClass:  "none",
	}
}

// ValidateConfig checks a stored config against the schema, then applies the two
// rules a JSON Schema cannot express (§L.5): no Authorization header, and no
// loopback, link-local or private target unless the operator opted in.
func (p *Provider) ValidateConfig(ctx context.Context, raw json.RawMessage) error {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	if err := CheckHeaders(cfg.Headers); err != nil {
		return err
	}
	return p.guard.CheckURL(ctx, cfg.URL)
}

// Open mints a Channel.
//
// The credential is applied here as a header on a per-channel transport, so the
// secret never touches Config and therefore never reaches the settings UI, the
// API response or a log line.
func (p *Provider) Open(
	ctx context.Context, cfg domain.ChannelConfig, cred domain.Credential,
) (domain.Channel, error) {
	parsed, err := ParseConfig(cfg.Raw)
	if err != nil {
		return nil, err
	}
	if err := CheckHeaders(parsed.Headers); err != nil {
		return nil, err
	}
	if err := p.guard.CheckURL(ctx, parsed.URL); err != nil {
		return nil, err
	}

	return &Channel{
		cfg:    parsed,
		client: p.httpClient(parsed, cred),
		guard:  p.guard,
		clock:  p.clock,
	}, nil
}

func (p *Provider) httpClient(cfg Config, cred domain.Credential) *http.Client {
	base := p.transport
	if base == nil {
		t, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			clone := t.Clone()
			if cfg.InsecureSkipVerify {
				// Opt-in, per channel, for a receiver behind a private CA. It is
				// never the default and never global.
				clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in, documented in the schema
			}
			base = clone
		} else {
			base = http.DefaultTransport
		}
	}

	return &http.Client{
		Transport: &authTransport{base: base, cred: cred},
		Timeout:   cfg.Timeout() + time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is how an allowed target becomes a forbidden one after
			// the guard has already passed. Refuse them all.
			return http.ErrUseLastResponse
		},
	}
}

// authTransport applies the sealed credential to every request.
type authTransport struct {
	base http.RoundTripper
	cred domain.Credential
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.cred.Kind {
	case CredBasic:
		if user := t.cred.Values["username"]; user != "" {
			req.SetBasicAuth(user, t.cred.Values["password"])
		}
	case CredBearer:
		if token := firstValue(t.cred.Values, "token", "bearer", "value"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return t.base.RoundTrip(req)
}

func firstValue(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

// Compile-time proof that the provider satisfies the port.
var _ domain.Provider = (*Provider)(nil)
var _ domain.Channel = (*Channel)(nil)
