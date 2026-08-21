package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"time"

	"github.com/thulasiram/oto/internal/channels/configschema"
	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/netguard"
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
//
// CredSigningSecret is a different kind of credential from the other three: it
// authenticates oto TO the receiver in the OTHER direction — every one of the
// other kinds gets oto INTO the receiver, while this one lets the receiver
// PROVE a payload came from oto. It signs the outbound JSON body with
// HMAC-SHA256 and sets the result on `X-Oto-Signature` (see Channel.send in
// channel.go). A connection may carry both a signing secret and a basic/bearer
// credential — they answer different questions — but `channel_credentials` has
// one row per kind, so a connection needing both seals two credentials and this
// provider is only ever handed one Credential at a time; v1 does not need that
// combination and does not implement it.
const (
	CredNone          = "none"
	CredBasic         = "basic"
	CredBearer        = "bearer"
	CredSigningSecret = "webhook_signing_secret"
)

// Provider mints generic webhook Channels.
type Provider struct {
	guard            *netguard.Guard
	clock            clock.Clock
	transport        http.RoundTripper
	allowInsecureTLS bool
}

// Options configures the webhook provider.
type Options struct {
	Clock clock.Clock
	// AllowPrivateTargets comes from OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS. It is off
	// by default: oto runs inside the operator's network, so an unguarded
	// operator-supplied URL is a Server-Side Request Forgery primitive (§L.5).
	AllowPrivateTargets bool
	// AllowInsecureTLS comes from `security.allow_insecure_tls`, the SAME
	// deployment-level switch that gates `alert_sources.tls_skip_verify`. It is off
	// by default: whether an unverified certificate is acceptable is a statement
	// about the operator's network, so `config.insecure_skip_verify` — which any
	// org member can write — is refused at validation and ignored at open unless
	// this is on (§M2).
	AllowInsecureTLS bool
	// Transport overrides the HTTP transport, for tests.
	Transport http.RoundTripper
	// Guard overrides the SSRF guard, which is how a test models an attacker's
	// DNS — a TTL-0 name that alternates public → link-local — without a DNS
	// server. Nil builds the production guard from AllowPrivateTargets.
	Guard *netguard.Guard
}

// NewProvider builds the webhook provider.
func NewProvider(o Options) *Provider {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	guard := o.Guard
	if guard == nil {
		// ⭐ ONE SSRF CONTROL FOR THE PROCESS. `Code` and `Field` place a refusal
		// where the settings form can render it: the webhook URL lives at
		// `config/url`, and a blocked target is a config error, not an outage.
		guard = netguard.New(netguard.Options{
			AllowPrivate: o.AllowPrivateTargets,
			Code:         "config_invalid",
			Field:        "url",
		})
	}
	return &Provider{
		guard:            guard,
		clock:            clk,
		transport:        o.Transport,
		allowInsecureTLS: o.AllowInsecureTLS,
	}
}

// Descriptor is served verbatim by GET /api/v1/channel-types.
//
// ⛔ IT IS STATIC PER PROCESS, NOT PER TENANT. The one thing that varies is
// whether the config schema declares `insecure_skip_verify`, and that varies with
// a DEPLOYMENT switch read once at boot: a deployment that will refuse the flag
// does not offer the control. See GatedSchema.
func (p *Provider) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:                      domain.TypeWebhook,
		DisplayName:               "Webhook",
		ConfigSchema:              p.schema().Raw(),
		CredentialKinds:           []string{CredNone, CredBasic, CredBearer},
		ConnectionConfigSchema:    ConnectionSchema.Raw(),
		ConnectionCredentialKinds: []string{CredNone, CredBasic, CredBearer, CredSigningSecret},
		Capabilities:              capabilities,
		Renderers:                 []domain.RendererID{domain.RendererWebhookJSON},
		RateLimitClass:            "none",
	}
}

// ValidateConfig checks a stored config against the schema, then applies the
// three rules a JSON Schema cannot express (§L.5): no Authorization header, no
// loopback, link-local or private target unless the operator opted in, and no
// tenant-set `insecure_skip_verify` unless the deployment allows it.
//
// ⚠️ The URL check here is FAST FEEDBACK, not the control. The control is the
// guard's dialer, installed on every channel's transport by httpClient. That is
// why an UNDECIDED answer — the host did not resolve from this machine, right
// now — is accepted: refusing to save `receiver.monitoring.svc` because a laptop
// cannot resolve it would make oto unconfigurable, and the dialer refuses the
// address it actually reaches regardless.
func (p *Provider) ValidateConfig(ctx context.Context, raw json.RawMessage) error {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	if err := CheckHeaders(cfg.Headers); err != nil {
		return err
	}
	if err := p.checkInsecureSkipVerify(cfg.InsecureSkipVerify); err != nil {
		return err
	}
	return p.checkTarget(ctx, cfg.URL)
}

// checkInsecureSkipVerify refuses a tenant's attempt to turn off certificate
// verification.
//
// ⛔ IT IS REFUSED, NOT IGNORED. Silently dropping the flag would leave an
// operator believing their receiver is reached without verification when it is
// not — and, worse, leave the org member who set it believing they had disabled
// something they had not. The 422 names the field, so a settings form points at
// the control and says who owns the decision. It is `schema()` that stops the
// form offering it in the first place; this is the server-side truth, because a
// client is not a trust boundary and `curl` is a client.
func (p *Provider) checkInsecureSkipVerify(requested bool) error {
	if !requested || p.allowInsecureTLS {
		return nil
	}
	return errs.Validation("insecure_skip_verify_not_permitted",
		"certificate verification is enforced by this deployment",
		errs.Violation{
			Field: insecureSkipVerifyProperty, Code: "forbidden",
			Message: "this is a deployment-level setting and cannot be changed per channel",
		})
}

// ValidateConnectionConfig checks a stored connection config against
// ConnectionSchema — currently empty, so any object with no properties passes.
func (p *Provider) ValidateConnectionConfig(_ context.Context, raw json.RawMessage) error {
	return ConnectionSchema.Validate(raw)
}

// schema is the config schema this deployment will actually honour.
func (p *Provider) schema() *configschema.Schema {
	if p.allowInsecureTLS {
		return Schema
	}
	return GatedSchema
}

// skipVerify resolves the effective TLS posture for one channel.
//
// ⛔ A TENANT CANNOT TURN OFF CERTIFICATE VERIFICATION. ValidateConfig refuses
// the flag on the way in; this is the layer that decides what the socket does, so
// a row written BEFORE the gate existed cannot keep working either. Such a row is
// ignored here rather than migrated or rejected at Open: refusing to open the
// channel would turn a hardening change into silent non-delivery, and oto's
// silence must never be indistinguishable from "no alert".
func (p *Provider) skipVerify(cfg Config) bool {
	return p.allowInsecureTLS && cfg.InsecureSkipVerify
}

// checkTarget runs the configuration-time URL check, tolerating an undecided
// answer. See ValidateConfig.
func (p *Provider) checkTarget(ctx context.Context, raw string) error {
	err := p.guard.CheckURL(ctx, raw)
	if err == nil || netguard.Undecided(err) {
		return nil
	}
	return err
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
	if err := p.checkTarget(ctx, parsed.URL); err != nil {
		return nil, err
	}

	return &Channel{
		cfg:    parsed,
		cred:   cred,
		client: p.httpClient(parsed, cred),
		guard:  p.guard,
		clock:  p.clock,
	}, nil
}

// httpClient builds the per-channel client.
//
// ⭐ THIS IS WHERE THE SSRF CONTROL LIVES. `guard.DialContext` resolves the host
// itself, checks EVERY candidate address, and dials a checked IP LITERAL — so
// there is no second resolution for a TTL-0 rebind to poison, and the guard
// covers redirects and reused connections that a pre-flight check never saw.
func (p *Provider) httpClient(cfg Config, cred domain.Credential) *http.Client {
	base := p.transport
	if base == nil {
		t, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			clone := t.Clone()
			if p.skipVerify(cfg) {
				// Per channel, for a receiver behind a private CA, and ONLY where
				// the deployment has opted in. Never the default.
				clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // gated on security.allow_insecure_tls; see skipVerify
			}
			clone.DialContext = p.guard.DialContext
			// A proxy would dial the proxy rather than the target, which takes the
			// guard out of the path silently. oto never proxies a webhook.
			clone.Proxy = nil
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
