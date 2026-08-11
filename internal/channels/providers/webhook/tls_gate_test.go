package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// This file is §M2 on the CHANNEL side. `alert_sources.tls_skip_verify` and a
// webhook channel's `config.insecure_skip_verify` ask the same question — may
// this deployment skip certificate verification — and both are written through a
// public, tenant-facing endpoint. The sources path answers it with the
// deployment switch (`internal/sources/api.checkTLSSkipVerify` and
// `internal/sources/service.skipVerify`); these tests hold the webhook provider
// to the same answer, at both layers, plus the one the sources path does not
// have: the schema the settings form is generated from.

// insecureConfig is a valid webhook config that asks for verification to be
// skipped.
func insecureConfig(t *testing.T, url string, skip bool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"url": url, "insecure_skip_verify": skip})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestInsecureSkipVerifyNeedsTheDeploymentSwitch is the refusal itself.
//
// ⛔ It is a 422 naming the field, NOT a silent drop. An operator who set the
// flag must be told it will not be honoured; a flag that vanishes quietly leaves
// somebody believing they turned something off that is still on.
func TestInsecureSkipVerifyNeedsTheDeploymentSwitch(t *testing.T) {
	t.Parallel()
	const url = "https://receiver.example.com/hook"

	closed := NewProvider(Options{Clock: clock.New()})
	err := closed.ValidateConfig(context.Background(), insecureConfig(t, url, true))
	if err == nil {
		t.Fatal("a tenant turned off certificate verification without the deployment switch")
	}

	e, ok := errs.As(err)
	if !ok {
		t.Fatalf("want an *errs.Error, got %T: %v", err, err)
	}
	if e.Kind != errs.KindValidation {
		t.Errorf("Kind = %v, want %v: this is a 422, not a 500", e.Kind, errs.KindValidation)
	}
	if e.Code != "insecure_skip_verify_not_permitted" {
		t.Errorf("Code = %q, want %q", e.Code, "insecure_skip_verify_not_permitted")
	}
	if e.Message != "certificate verification is enforced by this deployment" {
		t.Errorf("Message = %q, want the deployment-enforcement sentence", e.Message)
	}
	if len(e.Violations) != 1 {
		t.Fatalf("Violations = %d, want exactly 1 naming the control", len(e.Violations))
	}
	v := e.Violations[0]
	// The field is the JSON name of the property inside `config`; the channels
	// router re-roots it to `config/insecure_skip_verify` so a settings form can
	// find the control.
	if v.Field != "insecure_skip_verify" {
		t.Errorf("Violation.Field = %q, want %q", v.Field, "insecure_skip_verify")
	}
	if v.Code != "forbidden" {
		t.Errorf("Violation.Code = %q, want %q", v.Code, "forbidden")
	}
	if !strings.Contains(v.Message, "deployment-level") {
		t.Errorf("Violation.Message = %q, want it to say whose decision this is", v.Message)
	}

	// The same config with the flag off is ordinary and must still be accepted:
	// the gate refuses a REQUEST to skip verification, not the property's
	// existence.
	if err := closed.ValidateConfig(context.Background(), insecureConfig(t, url, false)); err != nil {
		t.Errorf("insecure_skip_verify:false was refused: %v", err)
	}

	// And with the deployment switch on, the operator's decision stands.
	open := NewProvider(Options{Clock: clock.New(), AllowInsecureTLS: true})
	if err := open.ValidateConfig(context.Background(), insecureConfig(t, url, true)); err != nil {
		t.Errorf("the deployment switch did not grant insecure_skip_verify: %v", err)
	}
}

// TestServedSchemaOffersTheFlagOnlyWhenItIsHonoured is the half of this the
// sources path has no equivalent of: GET /api/v1/channel-types generates the
// settings form, so a deployment that refuses the flag must not render a
// checkbox for it.
func TestServedSchemaOffersTheFlagOnlyWhenItIsHonoured(t *testing.T) {
	t.Parallel()

	properties := func(t *testing.T, p *Provider) map[string]json.RawMessage {
		t.Helper()
		var doc struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(p.Descriptor().ConfigSchema, &doc); err != nil {
			t.Fatalf("the served config schema is not a JSON Schema object: %v", err)
		}
		return doc.Properties
	}

	closed := properties(t, NewProvider(Options{Clock: clock.New()}))
	if _, offered := closed["insecure_skip_verify"]; offered {
		t.Error("the settings form offers a control this deployment will refuse with a 422")
	}
	// Pruning must remove ONE property and nothing else — the form is generated
	// from these bytes, so a lost `url` would be an unusable form.
	for _, want := range []string{"url", "method", "headers", "timeout_ms"} {
		if _, ok := closed[want]; !ok {
			t.Errorf("pruning removed %q as well: the settings form is now wrong", want)
		}
	}

	open := properties(t, NewProvider(Options{Clock: clock.New(), AllowInsecureTLS: true}))
	if _, offered := open["insecure_skip_verify"]; !offered {
		t.Error("the deployment honours the flag but does not offer it")
	}
}

// TestStoredInsecureFlagIsIgnoredWithoutTheSwitch is the layer that actually
// builds the TLS config, and it is where a row written BEFORE the gate existed
// is dealt with.
//
// The receiver's certificate is self-signed, so a verified connection fails and
// an unverified one succeeds. That is the discriminator: nothing about this test
// asks the provider what it intends, it asks the socket what it did.
func TestStoredInsecureFlagIsIgnoredWithoutTheSwitch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	deliver := func(t *testing.T, p *Provider) error {
		t.Helper()
		// Open must SUCCEED either way: refusing to open a channel whose stored
		// config predates the gate would turn a hardening change into silent
		// non-delivery, and oto's silence must never look like "no alert".
		ch, err := p.Open(context.Background(),
			domain.ChannelConfig{Raw: insecureConfig(t, srv.URL, true)},
			domain.Credential{Kind: CredNone})
		if err != nil {
			t.Fatalf("Open refused a stored config instead of ignoring the flag: %v", err)
		}
		t.Cleanup(func() { _ = ch.Close() })

		_, err = ch.Deliver(context.Background(), domain.DeliverRequest{
			Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: uuid.New(),
		})
		return err
	}

	// AllowPrivateTargets, because httptest binds loopback. The SSRF guard is
	// tested in ssrf_test.go; what is under test here is the TLS posture.
	closed := NewProvider(Options{Clock: clock.New(), AllowPrivateTargets: true})
	err := deliver(t, closed)
	if err == nil {
		t.Fatal("the stored flag disabled certificate verification without the deployment switch")
	}
	// `domain.Error.Error()` is "webhook: <code>" by design — it must not carry
	// upstream bytes — so the reason lives on Cause.
	var de *domain.Error
	if !asDomainError(err, &de) {
		t.Fatalf("want a *domain.Error, got %T: %v", err, err)
	}
	if de.Cause == nil ||
		(!strings.Contains(de.Cause.Error(), "certificate") && !strings.Contains(de.Cause.Error(), "x509")) {
		t.Fatalf("the delivery failed, but not on certificate verification: %v", de.Cause)
	}

	// With the switch on it is the operator's own decision, and it is honoured.
	open := NewProvider(Options{
		Clock: clock.New(), AllowPrivateTargets: true, AllowInsecureTLS: true,
	})
	if err := deliver(t, open); err != nil {
		t.Fatalf("the deployment switch did not reach the transport: %v", err)
	}
}
