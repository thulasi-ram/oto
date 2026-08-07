package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/slack-go/slack"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Credential kinds this provider consumes. They are the literal values of the
// channel_credentials.kind CHECK (§D.8).
const (
	// CredBotToken is the xoxb- token every delivery uses.
	CredBotToken = "slack_bot_token"
	// CredAppToken is the xapp- token Socket Mode needs.
	CredAppToken = "slack_app_token"
	// CredSigningSecret verifies HTTP-mode interaction requests. slack-go below
	// v0.23.1 accepts an EMPTY signing secret and therefore forged requests, which
	// is why the dependency has a version floor (CONTEXT.md §6).
	CredSigningSecret = "slack_signing_secret"
)

// capabilities is what a Slack Channel can do.
//
// CapDedupeKey is absent deliberately: Slack does no dedupe of its own, and
// claiming it would let the dispatch service skip oto's idempotency key — which is
// the only thing standing between an at-least-once queue and a duplicated page.
const capabilities = domain.CapThreading |
	domain.CapAmend |
	domain.CapRichLayout |
	domain.CapInteractive |
	domain.CapBroadcast

// Provider mints Slack Channels from stored config and a sealed credential.
//
// One Provider is registered at boot for the whole process; a Channel is minted
// per delivery target. The rate limiter lives on the Provider, not the Channel,
// because Slack's posting budget is per conversation per app — two Channels
// pointed at the same conversation must share one bucket or oto will be throttled
// while believing it is within budget.
type Provider struct {
	limiter    *limiter
	clock      clock.Clock
	httpClient *http.Client
	newAPI     func(token string, httpClient *http.Client) API
}

// Options configures the Slack provider.
type Options struct {
	Clock      clock.Clock
	HTTPClient *http.Client
	// NewAPI overrides SDK construction. It exists so the provider can be driven
	// against a fake workspace without a network.
	NewAPI func(token string, httpClient *http.Client) API
}

// NewProvider builds the Slack provider.
func NewProvider(o Options) *Provider {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	newAPI := o.NewAPI
	if newAPI == nil {
		newAPI = defaultAPI
	}
	return &Provider{
		limiter:    newLimiter(clk),
		clock:      clk,
		httpClient: o.HTTPClient,
		newAPI:     newAPI,
	}
}

func defaultAPI(token string, httpClient *http.Client) API {
	opts := []slack.Option{}
	if httpClient != nil {
		opts = append(opts, slack.OptionHTTPClient(httpClient))
	}
	return slack.New(token, opts...)
}

// Descriptor is static and is served verbatim by GET /api/v1/channel-types.
func (p *Provider) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:            domain.TypeSlack,
		DisplayName:     "Slack",
		ConfigSchema:    Schema.Raw(),
		CredentialKinds: []string{CredBotToken, CredAppToken, CredSigningSecret},
		Capabilities:    capabilities,
		Renderers:       []domain.RendererID{domain.RendererSlackDefault},
		RateLimitClass:  "slack",
	}
}

// ValidateConfig checks a stored config against the schema.
//
// Everything a JSON Schema can express lives in schema.json and nowhere else
// (§L.5). This method exists for the rules a schema cannot state — and for Slack,
// there are none beyond the schema, which is the point of having one.
func (p *Provider) ValidateConfig(_ context.Context, raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

// Open mints a Channel from validated config and an unsealed credential.
func (p *Provider) Open(
	_ context.Context, cfg domain.ChannelConfig, cred domain.Credential,
) (domain.Channel, error) {
	parsed, err := ParseConfig(cfg.Raw)
	if err != nil {
		return nil, err
	}

	token := botToken(cred)
	if token == "" {
		// A Slack channel without a bot token cannot do anything, and the DDL
		// already forbids one (channels_cred_ck). Reaching here means the sealed
		// credential is the wrong kind.
		return nil, errs.Validation("credential_missing",
			"a slack channel needs a "+CredBotToken+" credential",
			errs.Violation{
				Field: "credential_id", Code: "required",
				Message: "select a Slack bot token credential",
			})
	}

	return &Channel{
		api:     p.newAPI(token, p.httpClient),
		cfg:     parsed,
		limiter: p.limiter,
		clock:   p.clock,
	}, nil
}

// VerifyCredential proves a token is alive and reports the workspace it belongs
// to, so the settings UI can show "connected to Acme Corp as @oto" rather than a
// bare green tick.
func (p *Provider) VerifyCredential(ctx context.Context, cred domain.Credential) (Identity, error) {
	token := botToken(cred)
	if token == "" {
		return Identity{}, errs.Validation("credential_missing",
			"a slack bot token is required")
	}
	resp, err := p.newAPI(token, p.httpClient).AuthTestContext(ctx)
	if err != nil {
		return Identity{}, classify(err)
	}
	if resp == nil {
		return Identity{}, classify(errors.New("invalid_auth"))
	}
	return Identity{
		TeamID:   resp.TeamID,
		TeamName: resp.Team,
		UserID:   resp.UserID,
		UserName: resp.User,
		BotID:    resp.BotID,
	}, nil
}

// Identity is who a bot token says it is.
type Identity struct {
	TeamID   string
	TeamName string
	UserID   string
	UserName string
	BotID    string
}

// botToken pulls the delivery token out of an unsealed credential. It accepts a
// couple of key spellings because the sealed blob's shape is the secret store's
// business, not this package's.
func botToken(cred domain.Credential) string {
	if cred.Kind != "" && cred.Kind != CredBotToken {
		// A credential of another kind may still carry the bot token alongside
		// the app token, so fall through to the value lookup rather than refusing.
		_ = cred.Kind
	}
	for _, k := range []string{"bot_token", CredBotToken, "token", "value"} {
		if v := cred.Values[k]; v != "" {
			return v
		}
	}
	return ""
}

// Compile-time proof that the provider satisfies the port.
var _ domain.Provider = (*Provider)(nil)
var _ domain.Channel = (*Channel)(nil)
