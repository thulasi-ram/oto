package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Every interface in this file is a PORT DECLARED BY THE CONSUMER. This service
// says what it needs; `internal/app/container.go` decides what satisfies it.

// InstanceStore reads a configured destination and records what a send taught us
// about it.
type InstanceStore interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Instance, error)
	SetHealth(ctx context.Context, s db.TenantScope, id uuid.UUID,
		status domain.InstanceHealth, detail string, at time.Time) error
}

// CredentialResolver unseals a destination's secret.
//
// ⛔ The returned values are plaintext and exist only for the duration of one
// provider construction. This service never logs them and never puts one in an
// errs.Message.
type CredentialResolver interface {
	Resolve(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (kind string, values map[string]string, err error)
}

// Registry is the subset of `channels/registry` this service uses.
type Registry interface {
	Renderer(t domain.Type, id domain.RendererID) (domain.Renderer, error)
	Open(ctx context.Context, t domain.Type, cfg domain.ChannelConfig, cred domain.Credential) (domain.Channel, error)
	ValidateConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error
}

// TesterOptions are the Tester's dependencies.
type TesterOptions struct {
	Store    InstanceStore
	Creds    CredentialResolver
	Registry Registry
	Clock    clock.Clock
	// BaseURL is oto's public root, so the synthetic card's deep links point
	// somewhere real rather than at a placeholder.
	BaseURL string
}

// Tester sends ONE synthetic alert card through a channel.
//
// ⛔ THE POINT OF THIS TYPE IS THAT IT TAKES NO SHORTCUTS. It renders through the
// SAME renderer, the same outbound validator and the same transport a real
// notification uses. A "test" that posted a plain "hello from oto" would prove
// that the token works and nothing else — not that the Block Kit payload is
// legal, not that the conversation accepts a card of this size, not that the
// renderer's output survives validation. A passing test has to mean the real path
// works, or it is worse than no test at all.
type Tester struct {
	store    InstanceStore
	creds    CredentialResolver
	registry Registry
	clk      clock.Clock
	baseURL  string
}

// NewTester builds the tester.
func NewTester(o TesterOptions) (*Tester, error) {
	if o.Store == nil || o.Registry == nil {
		return nil, errs.New(errs.KindInternal, "channel_tester_deps",
			"a channel store and a registry are required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Tester{store: o.Store, creds: o.Creds, registry: o.Registry, clk: clk, baseURL: o.BaseURL}, nil
}

// Test renders and sends one synthetic card.
//
// The returned TestResult reports a provider REJECTION as `ok: false` with a
// classified `error_class`, not as an error: the test ran, and what it found out
// is the answer. An error is returned only when the test could not be attempted
// at all — no such channel, an unusable credential, a config that no longer
// validates.
//
// If the rendered payload fails outbound validation it is NEVER SENT. The
// delivery is reported as failed with `config_invalid`, because oto does not
// silently truncate a message to make it fit (§L.6).
func (t *Tester) Test(ctx context.Context, scope db.TenantScope, channelID uuid.UUID) (domain.TestResult, error) {
	now := t.clk.Now().UTC()

	inst, err := t.store.Get(ctx, scope, channelID)
	if err != nil {
		return domain.TestResult{}, err
	}
	if inst.Deleted() {
		return domain.TestResult{}, errs.NotFound("channel_deleted", "this channel has been deleted")
	}
	if !inst.Enabled {
		// A disabled destination is a PRECONDITION failure, not a bad request: the
		// channel is real and the operator can fix it by enabling it.
		return domain.TestResult{}, errs.Precondition("channel_disabled",
			"this channel is disabled; enable it before testing")
	}

	// Re-validate the stored config against the provider schema first. A channel
	// whose config stopped validating (a provider tightened a rule between
	// releases) must say so here rather than fail opaquely inside Open.
	if err := t.registry.ValidateConfig(ctx, inst.Type, inst.Config); err != nil {
		t.recordHealth(ctx, scope, inst.ID, domain.InstanceConfigInvalid, errs.CodeOf(err), now)
		return domain.TestResult{}, err
	}

	cred, err := t.credential(ctx, scope, inst)
	if err != nil {
		return domain.TestResult{}, err
	}

	renderer, err := t.registry.Renderer(inst.Type, inst.Renderer)
	if err != nil {
		return domain.TestResult{}, err
	}

	view := SyntheticView(inst, now, t.baseURL)
	msg, err := renderer.Render(ctx, view, domain.RenderOptions{
		Mode:           domain.ModePostRoot,
		Verbosity:      inst.Verbosity.Normalise(),
		ShowFieldEmoji: inst.ShowFieldEmoji,
		BaseURL:        t.baseURL,
	})
	if err != nil {
		// Outbound validation rejected the payload. It is NOT sent, and the
		// failure is reported as `config_invalid` with the reason preserved.
		t.recordHealth(ctx, scope, inst.ID, domain.InstanceConfigInvalid, errs.CodeOf(err), now)
		return domain.TestResult{
			OK:         false,
			Error:      messageOf(err, "the test card failed outbound validation and was not sent"),
			ErrorClass: domain.ClassConfigInvalid,
			CheckedAt:  now,
		}, nil
	}

	ch, err := t.registry.Open(ctx, inst.Type, inst.ToChannelConfig(), cred)
	if err != nil {
		return domain.TestResult{}, err
	}
	defer func() { _ = ch.Close() }()

	res, err := ch.Deliver(ctx, domain.DeliverRequest{
		Message:    msg,
		Mode:       domain.ModePostRoot,
		DeliveryID: uuid.Nil,
	})
	if err != nil {
		out := domain.TestResult{
			OK:         false,
			Error:      providerMessage(err),
			ErrorClass: classOf(err),
			CheckedAt:  now,
		}
		if h := destinationHealth(out.ErrorClass); h != "" {
			t.recordHealth(ctx, scope, inst.ID, h, out.Error, now)
		}
		return out, nil
	}

	t.recordHealth(ctx, scope, inst.ID, domain.InstanceHealthy, "", now)
	return domain.TestResult{
		OK:                     true,
		ProviderConversationID: res.Ref.ConversationID,
		ProviderMessageID:      res.Ref.MessageID,
		// ⛔ `Permalink` IS NOT `Ref.ProviderKey`, and used to be. ProviderKey is
		// documented in domain/ports.go as "opaque, provider-defined": Slack's is
		// `channel:ts` and the webhook provider's is the delivery UUID. Neither is
		// a URL, and `ChannelTestDTO.permalink` is `format: uri` — so every
		// successful channel test answered 200 with a body its own contract
		// rejects. Gate G2 is what found it, by validating the bytes a REAL
		// handler wrote. No provider currently returns a permalink, so the honest
		// answer is null; the field stays because Slack's `chat.getPermalink` is
		// the obvious way to fill it.
		CheckedAt: now,
	}, nil
}

// credential unseals the destination's secret, or returns the empty credential.
func (t *Tester) credential(ctx context.Context, scope db.TenantScope, inst domain.Instance) (domain.Credential, error) {
	if inst.CredentialID == nil {
		return domain.Credential{}, nil
	}
	if t.creds == nil {
		return domain.Credential{}, errs.New(errs.KindInternal, "credential_resolver_missing",
			"this deployment cannot unseal channel credentials")
	}
	kind, values, err := t.creds.Resolve(ctx, scope, *inst.CredentialID)
	if err != nil {
		return domain.Credential{}, err
	}
	return domain.Credential{Kind: kind, Values: values}, nil
}

// recordHealth writes what this attempt learned about the destination.
//
// A failure to record health never fails the test: the operator asked whether the
// message lands, and answering that is more useful than a 500 about bookkeeping.
func (t *Tester) recordHealth(
	ctx context.Context, scope db.TenantScope, id uuid.UUID,
	status domain.InstanceHealth, detail string, at time.Time,
) {
	if status.NeedsDetail() && detail == "" {
		detail = "the destination rejected a test message"
	}
	_ = t.store.SetHealth(ctx, scope, id, status, detail, at)
}

// classOf extracts oto's own classification from a provider error.
//
// THE CLASSIFICATION DRIVES BEHAVIOUR, never the provider's raw string: an
// unclassified failure is treated as retryable, which is the conservative
// reading — it does not raise a permanent banner on a destination that may simply
// be having a bad minute.
func classOf(err error) domain.ErrorClass {
	var pe *domain.Error
	if errors.As(err, &pe) && pe.Class != "" {
		return pe.Class
	}
	return domain.ClassRetryable
}

// providerMessage renders a provider failure safely.
//
// It reports the provider's own error CODE ("channel_not_found",
// "invalid_auth") because that is what a support question is answered with, and
// never the underlying error string, which can contain a request URL and
// therefore a token.
func providerMessage(err error) string {
	var pe *domain.Error
	if errors.As(err, &pe) {
		if pe.Code != "" {
			return pe.Provider + ": " + pe.Code
		}
		return pe.Provider + ": the destination rejected the message"
	}
	return "the destination could not be reached"
}

func messageOf(err error, fallback string) string {
	if e, ok := errs.As(err); ok && e.Message != "" {
		return e.Message
	}
	return fallback
}

// destinationHealth maps an error class onto the banner it raises on the
// destination, or "" when the class says nothing about it.
//
// The distinction it preserves is a PRODUCT FEATURE (§G.6): `auth_expired` and
// `config_invalid` are the difference between "the provider is flaky" and "your
// token was revoked three days ago and nobody noticed". A retryable or
// rate-limited failure deliberately maps to nothing — one bad minute must not
// paint a healthy destination red.
func destinationHealth(c domain.ErrorClass) domain.InstanceHealth {
	switch c {
	case domain.ClassAuthExpired:
		return domain.InstanceAuthFailed
	case domain.ClassConfigInvalid:
		return domain.InstanceConfigInvalid
	case domain.ClassPermanent:
		return domain.InstanceDegraded
	default:
		return ""
	}
}
