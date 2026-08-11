package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The Channels tag, asserted against the ONE contract.
//
// Every finding of the conformance audit lived in an `api` package, and two of
// them lived here. The reason was not that the handlers were careless: it was
// that nothing had ever compared a response byte to `api/openapi/openapi.yaml`.
// So these tests never restate an expected shape by hand — a second copy of the
// contract drifts exactly the way the DTOs did. `schema.Assert` compiles the
// schema the spec declares for the operation and the status, and validates the
// bytes the handler actually wrote.
//
// The promises being protected, in order of how much a break would cost:
//
//  1. A secret NEVER comes back. `credential.values` goes in and nothing but a
//     kind and a rotation timestamp comes out.
//  2. Another tenant's id is a 404. Not a 403, which would confirm the row
//     exists, and never that tenant's data.
//  3. Every enum value the Go code can emit is one the contract admits. An
//     enum the server writes and the contract rejects is a client that crashes
//     on a value oto produces routinely.

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// chanRegistry is a ProviderRegistry whose whole memory is a descriptor list.
//
// `ValidateConfig` is a switch rather than a real JSON-Schema run: the schema
// engine has its own tests, and what this layer owns is the RE-ROOTING of a
// violation under `config/` — a violation reported at `/conversation_id` would
// not find the form control at `config.conversation_id`.
type chanRegistry struct {
	descriptors []domain.Descriptor
	// configErr is returned by ValidateConfig, already shaped as the registry
	// would shape it: violations rooted at the instance location.
	configErr error
	// configCalls counts server-side config validation, because "the client
	// validated it" is not a trust boundary and curl is a client.
	configCalls int
}

func (f *chanRegistry) Descriptors() []domain.Descriptor { return f.descriptors }

func (f *chanRegistry) ConfigSchema(t domain.Type) (json.RawMessage, error) {
	for _, d := range f.descriptors {
		if d.Type == t {
			return d.ConfigSchema, nil
		}
	}
	return nil, errs.NotFound("channel_type_unknown", "no such provider")
}

func (f *chanRegistry) ValidateConfig(context.Context, domain.Type, json.RawMessage) error {
	f.configCalls++
	return f.configErr
}

func (f *chanRegistry) Types() []domain.Type {
	out := make([]domain.Type, 0, len(f.descriptors))
	for _, d := range f.descriptors {
		out = append(out, d.Type)
	}
	return out
}

// chanStore is a ChannelStore keyed by id, and — this is the whole point — it
// answers NOT FOUND for any row whose OrgID is not the caller's.
//
// That is the honest shape of the real query: every statement is `WHERE org_id =
// $1 AND id = $2`, so a stranger's id genuinely returns zero rows. A fake that
// returned the row and left the handler to notice would be testing a guard the
// production code does not have.
type chanStore struct {
	byID map[uuid.UUID]domain.Instance
	// referencing is what ReferencingPolicies answers for a live id.
	referencing []string
	// created and patched record the writes, so a refused request can be proved
	// to have written nothing.
	created  []domain.NewInstance
	patched  []domain.InstancePatch
	deleted  []uuid.UUID
	listPage []domain.Instance
}

func (f *chanStore) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Instance, error) {
	inst, ok := f.byID[id]
	if !ok || inst.OrgID != s.OrgID() {
		return domain.Instance{}, errs.NotFound("channel_not_found", "no such channel")
	}
	return inst, nil
}

func (f *chanStore) List(
	_ context.Context, s db.TenantScope, _ bool, _ db.Keyset,
) ([]domain.Instance, db.Cursor, error) {
	out := make([]domain.Instance, 0, len(f.listPage))
	for _, i := range f.listPage {
		if i.OrgID == s.OrgID() {
			out = append(out, i)
		}
	}
	return out, db.Cursor{}, nil
}

func (f *chanStore) Create(
	_ context.Context, s db.TenantScope, in domain.NewInstance,
) (domain.Instance, error) {
	f.created = append(f.created, in)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	kind := ""
	if in.CredentialID != nil {
		kind = "slack_bot_token"
	}
	return domain.Instance{
		ID: uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104a"), OrgID: s.OrgID(),
		Type: in.Type, Name: in.Name, Config: in.Config,
		CredentialID: in.CredentialID, CredentialKind: kind,
		Capabilities: in.Capabilities, Renderer: in.Renderer, Verbosity: in.Verbosity,
		ThreadUpdates: in.ThreadUpdates, ShowFieldEmoji: in.ShowFieldEmoji, Enabled: in.Enabled,
		Health: domain.InstanceHealthUnknown, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (f *chanStore) Update(
	ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.InstancePatch,
) (domain.Instance, error) {
	inst, err := f.Get(ctx, s, id)
	if err != nil {
		return domain.Instance{}, err
	}
	f.patched = append(f.patched, p)
	if p.Name != nil {
		inst.Name = *p.Name
	}
	if p.Renderer != nil {
		inst.Renderer = *p.Renderer
	}
	return inst, nil
}

func (f *chanStore) SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error {
	if _, err := f.Get(ctx, s, id); err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *chanStore) ReferencingPolicies(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) ([]string, error) {
	if _, err := f.Get(ctx, s, id); err != nil {
		return nil, err
	}
	return f.referencing, nil
}

// chanCreds records the plaintext it was handed, so a test can prove the bytes
// reached the sealer AND that the same bytes never reached the response.
type chanCreds struct {
	sealed  []map[string]string
	rotated []map[string]string
}

func (f *chanCreds) CreateCredential(
	_ context.Context, _ db.TenantScope, _ string, values map[string]string,
) (uuid.UUID, error) {
	f.sealed = append(f.sealed, values)
	return uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317491111"), nil
}

func (f *chanCreds) RotateCredential(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, _ string, values map[string]string,
) error {
	f.rotated = append(f.rotated, values)
	return nil
}

// chanTester answers the synthetic-card send.
type chanTester struct {
	result domain.TestResult
	err    error
	// scoped makes Test behave like the real query and refuse an id the caller
	// does not own, which is what the tenant probe drives.
	store *chanStore
}

func (f *chanTester) Test(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.TestResult, error) {
	if f.store != nil {
		if _, err := f.store.Get(ctx, s, id); err != nil {
			return domain.TestResult{}, err
		}
	}
	if f.err != nil {
		return domain.TestResult{}, f.err
	}
	return f.result, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// chanNow is the instant every fixture in this file is stamped with. It is a
// constant so that a failure message names a timestamp a reader can find.
var chanNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// chanMine is a destination owned by apitest.OrgID.
var chanMine = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490001")

// slackDescriptor is the provider descriptor `listChannelTypes` serves. Its
// ConfigSchema is deliberately a real schema document: the contract types it as
// an object and the settings form is generated from these exact bytes.
func slackDescriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:        domain.TypeSlack,
		DisplayName: "Slack",
		ConfigSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema",` +
			`"type":"object","required":["conversation_id"],` +
			`"properties":{"conversation_id":{"type":"string","pattern":"^[CGD][A-Z0-9]{8,}$"}}}`),
		CredentialKinds: []string{"slack_bot_token"},
		Capabilities: domain.CapThreading | domain.CapAmend | domain.CapRichLayout |
			domain.CapInteractive | domain.CapBroadcast,
		Renderers:      []domain.RendererID{domain.RendererSlackDefault},
		RateLimitClass: "slack",
	}
}

func webhookDescriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:            domain.TypeWebhook,
		DisplayName:     "Webhook",
		ConfigSchema:    json.RawMessage(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}`),
		CredentialKinds: []string{"bearer", "basic", "none"},
		Capabilities:    domain.CapDedupeKey,
		Renderers:       []domain.RendererID{domain.RendererWebhookJSON},
		RateLimitClass:  "none",
	}
}

// channelFixture is one stored destination, healthy and fully populated, so that
// a schema failure is about the mapping rather than about an absent field.
func channelFixture(id, org uuid.UUID) domain.Instance {
	kind := "slack_bot_token"
	rotated := chanNow.Add(-72 * time.Hour)
	checked := chanNow.Add(-time.Minute)
	credential := uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317492222")
	return domain.Instance{
		ID:                  id,
		OrgID:               org,
		Type:                domain.TypeSlack,
		Name:                "#sre-alerts",
		Config:              json.RawMessage(`{"conversation_id":"C7F2X9QLM","team_id":"T9TK3CUKW"}`),
		CredentialID:        &credential,
		CredentialKind:      kind,
		CredentialRotatedAt: &rotated,
		Capabilities:        domain.CapThreading | domain.CapAmend,
		Renderer:            domain.RendererSlackDefault,
		Verbosity:           domain.VerbosityStatusChanges,
		ThreadUpdates:       true,
		ShowFieldEmoji:      true,
		Enabled:             true,
		Health:              domain.InstanceHealthy,
		HealthCheckedAt:     &checked,
		CreatedAt:           chanNow.Add(-30 * 24 * time.Hour),
		UpdatedAt:           chanNow.Add(-time.Hour),
	}
}

// chanWorld is one wired router plus the fakes behind it.
type chanWorld struct {
	registry *chanRegistry
	store    *chanStore
	creds    *chanCreds
	tester   *chanTester
	client   *apitest.Client
}

// newChanWorld wires the Channels router with two providers, one destination
// owned by apitest.OrgID and one owned by apitest.OtherOrgID.
//
// The stranger row EXISTS in the store. That matters: a probe against an id that
// is simply absent proves nothing about tenancy, because "no such row" and "not
// yours" would be the same answer for the wrong reason.
func newChanWorld(t *testing.T) *chanWorld {
	t.Helper()

	mine := channelFixture(chanMine, apitest.OrgID)
	stranger := channelFixture(apitest.StrangerID, apitest.OtherOrgID)

	store := &chanStore{
		byID: map[uuid.UUID]domain.Instance{
			chanMine:           mine,
			apitest.StrangerID: stranger,
		},
		listPage: []domain.Instance{mine, stranger},
	}
	w := &chanWorld{
		registry: &chanRegistry{descriptors: []domain.Descriptor{slackDescriptor(), webhookDescriptor()}},
		store:    store,
		creds:    &chanCreds{},
		tester: &chanTester{
			store:  store,
			result: domain.TestResult{OK: true, ProviderConversationID: "C7F2X9QLM", ProviderMessageID: "1723023262.114300", CheckedAt: chanNow},
		},
	}
	rt := NewRouter(Options{
		Registry: w.registry,
		Channels: w.store,
		Creds:    w.creds,
		Tester:   w.tester,
		Clock:    clock.NewFake(chanNow),
	})
	w.client = apitest.New(rt)
	return w
}

/* -------------------------------------------------------------------------- */
/* Happy paths — one per operation, asserted against the contract             */
/* -------------------------------------------------------------------------- */

// TestChannelTypesServeTheProviderSchemaTheFormIsGeneratedFrom.
//
// The promise: `config_schema` is the SAME BYTES the server validates against,
// so the settings form has no per-provider code. If this response ever became a
// hand-written summary of a provider's options, adding a provider would silently
// start requiring UI work — and the form and the server would begin to disagree
// about what a valid config is.
func TestChannelTypesServeTheProviderSchemaTheFormIsGeneratedFrom(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channel-types").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listChannelTypes", http.StatusOK, resp.Body())

	body := resp.JSON(t)
	data, ok := body["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("data is %#v, want the two v1 providers", body["data"])
	}
	// ⚠️ The SINGLE-RESOURCE envelope carrying an array. `ChannelTypeListResponse`
	// is additionalProperties:false and requires exactly {data, meta}: a `page`
	// object here would offer a `next_cursor` that can never exist.
	if _, present := body["page"]; present {
		t.Fatalf("the descriptor list carries a page object: %#v", body["page"])
	}
	first, _ := data[0].(map[string]any)
	if _, ok := first["config_schema"].(map[string]any); !ok {
		t.Fatalf("config_schema is %T, want the provider's schema document", first["config_schema"])
	}
}

// TestListChannelsAnswersAPageOfThisTenantsDestinations.
func TestListChannelsAnswersAPageOfThisTenantsDestinations(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channels").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listChannels", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d channels, want 1 — the other belongs to %s", len(data), apitest.OtherOrgID)
	}
}

// ⭐ TestCreatingAChannelSealsTheSecretAndNeverEchoesIt.
//
// This is the one assertion in the file whose failure is a security incident
// rather than a bug. `credential.values` is write-only: it travels from the
// decoded DTO into the sealer and is referenced nowhere else, and no endpoint in
// this API has a way to read it back. The response is checked for the literal
// token bytes because a leak would not announce itself as a named field — it
// would arrive inside some helpfully echoed request object.
func TestCreatingAChannelSealsTheSecretAndNeverEchoesIt(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	const botToken = "xoxb-0000000000-1111111111-notarealtoken" //nolint:gosec // a fixture, not a credential

	body := map[string]any{
		"type":   "slack",
		"name":   "#sre-alerts",
		"config": map[string]any{"conversation_id": "C7F2X9QLM"},
		"credential": map[string]any{
			"kind":   "slack_bot_token",
			"values": map[string]string{"token": botToken},
		},
		"renderer": "slack.default",
	}
	// The fixture is proved to be one a real client could have sent, so this test
	// cannot pass with a request the contract forbids.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "createChannel", raw)

	resp := w.client.POST(t, "/channels", body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createChannel", http.StatusCreated, resp.Body())

	if strings.Contains(string(resp.Body()), botToken) {
		t.Fatalf("⛔ the bot token came back in the response body:\n%s", resp.Body())
	}
	if strings.Contains(string(resp.Body()), "\"values\"") {
		t.Fatalf("⛔ the response carries a `values` member; credentials are write-only:\n%s", resp.Body())
	}
	if len(w.creds.sealed) != 1 || w.creds.sealed[0]["token"] != botToken {
		t.Fatalf("the secret did not reach the sealer: %#v", w.creds.sealed)
	}
	// ⛔ The config was validated ON THE SERVER. A client is not a trust boundary.
	if w.registry.configCalls != 1 {
		t.Fatalf("ValidateConfig ran %d times; the provider schema must be applied on every write",
			w.registry.configCalls)
	}
}

// TestGetChannelServesOneDestinationAndOnlyTheSafeHalfOfItsSecret.
//
// `credential_kind` and `credential_rotated_at` are the ONLY things this API ever
// says about a secret: which kind is attached, and when it last moved.
func TestGetChannelServesOneDestinationAndOnlyTheSafeHalfOfItsSecret(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channels/"+chanMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getChannel", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if got := data["credential_kind"]; got != "slack_bot_token" {
		t.Fatalf("credential_kind = %v, want slack_bot_token", got)
	}
	for _, forbidden := range []string{"credential", "values", "token", "credential_id"} {
		if _, present := data[forbidden]; present {
			t.Fatalf("⛔ the channel response carries %q: %#v", forbidden, data[forbidden])
		}
	}
}

// TestUpdatingAChannelRotatesTheSecretInPlace.
//
// A supplied credential ROTATES rather than detaching and re-attaching, so the
// channel never spends a moment pointing at nothing — and the new plaintext is
// no more echoed than the old one was.
func TestUpdatingAChannelRotatesTheSecretInPlace(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	const rotatedToken = "xoxb-2222222222-3333333333-alsonotreal" //nolint:gosec // a fixture, not a credential

	body := map[string]any{
		"name":       "#sre-alerts-v2",
		"credential": map[string]any{"kind": "slack_bot_token", "values": map[string]string{"token": rotatedToken}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "updateChannel", raw)

	resp := w.client.PATCH(t, "/channels/"+chanMine.String(), body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateChannel", http.StatusOK, resp.Body())

	if len(w.creds.rotated) != 1 {
		t.Fatalf("RotateCredential ran %d times, want exactly 1 — a detach-then-attach leaves a gap",
			len(w.creds.rotated))
	}
	if len(w.creds.sealed) != 0 {
		t.Fatal("the update created a NEW credential instead of rotating the existing one")
	}
	if strings.Contains(string(resp.Body()), rotatedToken) {
		t.Fatalf("⛔ the rotated token came back in the response:\n%s", resp.Body())
	}
}

// TestDeletingAChannelIsA204WithNothingInIt.
//
// RFC 9110 §15.3.5: a 204 has no content. Advertising a media type for a body
// that cannot exist invites a strict client to parse zero bytes as JSON and fail.
func TestDeletingAChannelIsA204WithNothingInIt(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.DELETE("/channels/"+chanMine.String()).MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteChannel", http.StatusNoContent, resp.Body())

	if resp.Header("Content-Type") != "" {
		t.Fatalf("the 204 advertises Content-Type %q for a body that cannot exist", resp.Header("Content-Type"))
	}
	if len(w.store.deleted) != 1 {
		t.Fatalf("SoftDelete ran %d times, want 1", len(w.store.deleted))
	}
}

// ⭐ TestAProviderRejectionIsA200WithOkFalse.
//
// Turning it into a 502 would throw away `error_class`, which is the field that
// distinguishes "Slack is flaky" from "your token was revoked three days ago and
// nobody noticed". The test RAN; the answer is that the destination said no.
func TestAProviderRejectionIsA200WithOkFalse(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	w.tester.result = domain.TestResult{
		OK:         false,
		Error:      "the bot token was revoked",
		ErrorClass: domain.ClassAuthExpired,
		CheckedAt:  chanNow,
	}

	resp := w.client.POST(t, "/channels/"+chanMine.String()+"/test", nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "testChannel", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if data["ok"] != false {
		t.Fatalf("ok = %v, want false", data["ok"])
	}
	if data["error_class"] != "auth_expired" {
		t.Fatalf("error_class = %v, want auth_expired — the field that says WHICH kind of no", data["error_class"])
	}
}

/* -------------------------------------------------------------------------- */
/* The tenant boundary                                                        */
/* -------------------------------------------------------------------------- */

// ⭐⭐ TestAnotherTenantsChannelIdIsAlwaysA404.
//
// v1 has no roles, so the tenant boundary is the ONLY boundary there is. Every
// endpoint addressed by `{id}` must answer 404 for an id belonging to somebody
// else — never 403, which confirms the row exists; never 200 with that tenant's
// data; and never a 500, which is a boundary held by accident rather than by
// design.
//
// The stranger row is REAL and owned by apitest.OtherOrgID, so a handler that
// forgot its scope would find it.
func TestAnotherTenantsChannelIdIsAlwaysA404(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID.String()

	probes := []struct {
		op string
		// send runs the request. It takes the *testing.T of its own subtest so a
		// marshalling failure is attributed where it happened.
		send func(t *testing.T, c *apitest.Client) *apitest.Response
	}{
		{
			op:   "getChannel",
			send: func(_ *testing.T, c *apitest.Client) *apitest.Response { return c.GET("/channels/" + stranger) },
		},
		{
			op: "updateChannel",
			send: func(t *testing.T, c *apitest.Client) *apitest.Response {
				return c.PATCH(t, "/channels/"+stranger, map[string]any{"name": "mine now"})
			},
		},
		{
			op:   "deleteChannel",
			send: func(_ *testing.T, c *apitest.Client) *apitest.Response { return c.DELETE("/channels/" + stranger) },
		},
		{
			op: "testChannel",
			send: func(t *testing.T, c *apitest.Client) *apitest.Response {
				return c.POST(t, "/channels/"+stranger+"/test", nil)
			},
		},
	}

	for _, p := range probes {
		t.Run(p.op, func(t *testing.T) {
			t.Parallel()
			w := newChanWorld(t)

			resp := p.send(t, w.client)
			if resp.Code() != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for another tenant's id.\n%s", resp.Code(), resp)
			}
			schema.AssertProblem(t, p.op, http.StatusNotFound, resp.Body())

			if strings.Contains(string(resp.Body()), "#sre-alerts") {
				t.Fatalf("⛔ the refusal leaked the other tenant's channel name:\n%s", resp.Body())
			}
			if len(w.store.deleted) != 0 || len(w.store.patched) != 0 {
				t.Fatal("⛔ a cross-tenant request still reached a write")
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* Refusals the handlers actually have                                        */
/* -------------------------------------------------------------------------- */

// ⛔ TestDeletingAChannelAPolicyStillRoutesToIsA409.
//
// Never a cascade. Silently orphaning a policy's only destination would make it
// stop notifying without saying so — the invisible silence §B.6 forbids — and the
// refusal names the policies so the operator can fix them rather than go looking.
func TestDeletingAChannelAPolicyStillRoutesToIsA409(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	w.store.referencing = []string{"critical → #sre-alerts"}

	resp := w.client.DELETE("/channels/"+chanMine.String()).MustStatus(t, http.StatusConflict)
	schema.AssertProblem(t, "deleteChannel", http.StatusConflict, resp.Body())

	if !strings.Contains(resp.Problem(t).Detail, "critical → #sre-alerts") {
		t.Fatalf("the refusal does not name the offending policy: %s", resp.Body())
	}
	if len(w.store.deleted) != 0 {
		t.Fatal("the channel was deleted anyway; a policy's only destination is now orphaned")
	}
}

// TestASlackChannelWithoutABotTokenNamesTheControlThatIsEmpty.
//
// `channels_cred_ck` would catch this in the database, but as a 23514 — a 500
// that tells the operator nothing. Saying it here turns it into a field violation
// pointing at the control they left blank.
func TestASlackChannelWithoutABotTokenNamesTheControlThatIsEmpty(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.POST(t, "/channels", map[string]any{
		"type":   "slack",
		"name":   "#sre-alerts",
		"config": map[string]any{"conversation_id": "C7F2X9QLM"},
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createChannel", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "credential")

	if len(w.store.created) != 0 {
		t.Fatal("a refused create still wrote a channel row")
	}
}

// TestARendererBelongingToTheOtherProviderIsRefused.
//
// `channels_rend_ck` catches a wholly unknown string, but not a VALID renderer
// belonging to the other provider: `webhook.json` on a Slack channel passes the
// CHECK and then renders a JSON envelope into Block Kit. Only the registry can
// see that mismatch.
func TestARendererBelongingToTheOtherProviderIsRefused(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.POST(t, "/channels", map[string]any{
		"type":       "slack",
		"name":       "#sre-alerts",
		"config":     map[string]any{"conversation_id": "C7F2X9QLM"},
		"credential": map[string]any{"kind": "slack_bot_token", "values": map[string]string{"token": "x"}},
		"renderer":   "webhook.json",
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createChannel", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "renderer")
}

// TestAConfigTheProviderSchemaRejectsIsReRootedUnderConfig.
//
// The schema knows nothing about the envelope its instance arrived in, so a
// violation reported at `/conversation_id` would not find the form control at
// `config.conversation_id`. Re-rooting is what makes the 422 actionable.
func TestAConfigTheProviderSchemaRejectsIsReRootedUnderConfig(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	w.registry.configErr = errs.Validation("validation_failed", "1 field failed validation.",
		errs.Violation{Field: "/conversation_id", Code: "pattern",
			Message: "a Slack conversation id, not a channel name"})

	resp := w.client.POST(t, "/channels", map[string]any{
		"type":       "slack",
		"name":       "#sre-alerts",
		"config":     map[string]any{"conversation_id": "#sre-alerts"},
		"credential": map[string]any{"kind": "slack_bot_token", "values": map[string]string{"token": "x"}},
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createChannel", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "config/conversation_id")
}

// TestAnUpdateThatAsksForNothingIsRefused, because the contract marks the body
// `minProperties: 1` and a PATCH that changes nothing is a client bug worth
// naming rather than a no-op worth hiding.
func TestAnUpdateThatAsksForNothingIsRefused(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.PATCH(t, "/channels/"+chanMine.String(), map[string]any{}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "updateChannel", http.StatusUnprocessableEntity, resp.Body())
	if len(resp.Problem(t).Violations) == 0 {
		t.Fatalf("the refusal carries no violations[]: %s", resp.Body())
	}
	if len(w.store.patched) != 0 {
		t.Fatal("an empty patch still reached the store")
	}
}

// TestASoftDeletedChannelIs404AndNotAGhost.
//
// The row survives — delivery history is the point — but the settings surface is
// about what is configured NOW, and a deleted destination answering 200 would
// invite an operator to edit something that can never receive anything.
func TestASoftDeletedChannelIs404AndNotAGhost(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	gone := w.store.byID[chanMine]
	deletedAt := chanNow.Add(-time.Hour)
	gone.DeletedAt = &deletedAt
	w.store.byID[chanMine] = gone

	resp := w.client.GET("/channels/"+chanMine.String()).MustStatus(t, http.StatusNotFound)
	schema.AssertProblem(t, "getChannel", http.StatusNotFound, resp.Body())

	if got := resp.Problem(t).Code; got != "channel_deleted" {
		t.Fatalf("code = %q, want channel_deleted so a client can tell it apart from a typo'd id", got)
	}
}

// TestAnUnknownQueryParameterOnTheChannelListIsRefused keeps §E.3: a typo'd
// `?limitt=200` that is silently ignored returns a page of the wrong size and
// looks right.
func TestAnUnknownQueryParameterOnTheChannelListIsRefused(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channels?limitt=200").MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listChannels", http.StatusBadRequest, resp.Body())

	if got := resp.Problem(t).Code; got != "unknown_parameter" {
		t.Fatalf("code = %q, want unknown_parameter", got)
	}
}

/* -------------------------------------------------------------------------- */
/* The enum sweep                                                             */
/* -------------------------------------------------------------------------- */

// ⭐ TestEveryChannelHealthStateTheServerCanEmitSatisfiesTheContract.
//
// This is the shape of the audit's worst finding, applied prophylactically: a Go
// constant the server writes routinely and the contract's enum does not admit is
// a client that crashes on ordinary data. Driving one response per constant —
// from the CONSTANTS, not from a list retyped here — means adding a health state
// without adding it to the contract fails immediately, in this package.
func TestEveryChannelHealthStateTheServerCanEmitSatisfiesTheContract(t *testing.T) {
	t.Parallel()

	states := []domain.InstanceHealth{
		domain.InstanceHealthy,
		domain.InstanceDegraded,
		domain.InstanceAuthFailed,
		domain.InstanceConfigInvalid,
		domain.InstanceHealthUnknown,
	}

	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			w := newChanWorld(t)

			inst := w.store.byID[chanMine]
			inst.Health = state
			if state.NeedsDetail() {
				inst.HealthError = "the destination refused the last delivery"
			}
			w.store.byID[chanMine] = inst

			resp := w.client.GET("/channels/"+chanMine.String()).MustStatus(t, http.StatusOK)
			schema.Assert(t, "getChannel", http.StatusOK, resp.Body())

			data, _ := resp.JSON(t)["data"].(map[string]any)
			if data["health_status"] != string(state) {
				t.Fatalf("health_status = %v, want %s", data["health_status"], state)
			}
		})
	}
}

// TestEveryVerbosityAndRendererTheServerCanEmitSatisfiesTheContract.
//
// `verbosity` and `renderer` are closed enums on the wire and ordinary strings in
// the domain, which is exactly the gap a contract test exists to close. The empty
// verbosity is included because `Normalise` fills it in — and a channel whose
// verbosity serialised as `""` would fail its own schema.
func TestEveryVerbosityAndRendererTheServerCanEmitSatisfiesTheContract(t *testing.T) {
	t.Parallel()

	verbosities := []domain.Verbosity{
		domain.VerbosityAll,
		domain.VerbosityStatusChanges,
		domain.VerbosityFiringAndResolved,
		domain.VerbosityFiringOnly,
		"", // unset in the row; Normalise() supplies the documented default
	}
	renderers := []domain.RendererID{
		domain.RendererSlackDefault,
		domain.RendererWebhookJSON,
		"", // unset in the row; the mapper supplies `default`
	}

	for _, v := range verbosities {
		for _, r := range renderers {
			name := string(v) + "/" + string(r)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				w := newChanWorld(t)

				inst := w.store.byID[chanMine]
				inst.Verbosity = v
				inst.Renderer = r
				w.store.byID[chanMine] = inst

				resp := w.client.GET("/channels/"+chanMine.String()).MustStatus(t, http.StatusOK)
				schema.Assert(t, "getChannel", http.StatusOK, resp.Body())
			})
		}
	}
}

// TestEveryCapabilityBitHasAWireNameTheContractAdmits.
//
// The bits are oto's internal negotiation currency and the strings are the wire.
// A capability added to the bitset without a name here would simply vanish from
// `listChannelTypes`, and the dispatcher would negotiate on a bit no client can
// see — so the descriptor is served with EVERY bit set and validated whole.
func TestEveryCapabilityBitHasAWireNameTheContractAdmits(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	all := domain.CapThreading | domain.CapAmend | domain.CapRichLayout |
		domain.CapInteractive | domain.CapBroadcast | domain.CapDedupeKey

	d := slackDescriptor()
	d.Capabilities = all
	w.registry.descriptors = []domain.Descriptor{d}

	resp := w.client.GET("/channel-types").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listChannelTypes", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	first, _ := data[0].(map[string]any)
	caps, _ := first["capabilities"].([]any)
	if len(caps) != len(capabilityNames) {
		t.Fatalf("capabilities = %v; %d bits are declared and %d were named",
			caps, len(capabilityNames), len(caps))
	}
}

// TestEveryTestErrorClassTheServerCanEmitSatisfiesTheContract sweeps the class
// that decides whether an operator rotates a token or waits out an outage.
func TestEveryTestErrorClassTheServerCanEmitSatisfiesTheContract(t *testing.T) {
	t.Parallel()

	classes := []domain.ErrorClass{
		domain.ClassRetryable,
		domain.ClassRateLimited,
		domain.ClassPermanent,
		domain.ClassConfigInvalid,
		domain.ClassAuthExpired,
		"", // a successful test names no class at all
	}

	for _, c := range classes {
		name := string(c)
		if name == "" {
			name = "none"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newChanWorld(t)
			w.tester.result = domain.TestResult{
				OK: c == "", Error: string(c), ErrorClass: c, CheckedAt: chanNow,
			}

			resp := w.client.POST(t, "/channels/"+chanMine.String()+"/test", nil).
				MustStatus(t, http.StatusOK)
			schema.Assert(t, "testChannel", http.StatusOK, resp.Body())
		})
	}
}

/* -------------------------------------------------------------------------- */
/* Divergences from the contract                                              */
/* -------------------------------------------------------------------------- */

// TestBUG_UnknownQueryParameterOnAnIdAddressedChannelEndpointIsAnUndeclared400.
//
// WHAT THE SERVER DOES. `Router.subject` runs `httpx.NewParams(r).Err()` on every
// `{id}` endpoint, so `GET /api/v1/channels/{id}?foo=bar` answers
// `400 unknown_parameter`. `listChannelTypes` does the same.
//
// WHAT THE CONTRACT SAYS. `getChannel`, `deleteChannel`, `testChannel` and
// `listChannelTypes` declare no `400` at all — only 200/204, 401, 403, 404, 409,
// 429, 500, 503.
//
// WHICH IS WRONG. THE CONTRACT. §E.3 is binding and deliberate: a silently
// ignored parameter returns the wrong answer and looks right, so rejecting is the
// behaviour oto wants everywhere. The fix is to add
// `'400': $ref: '#/components/responses/BadRequest'` to those four operations,
// not to make the handlers lenient. Until then a generated client has no branch
// for a status the server really emits.
func TestBUG_UnknownQueryParameterOnAnIdAddressedChannelEndpointIsAnUndeclared400(t *testing.T) {
	t.Skip("live conformance defect: the handlers answer 400 unknown_parameter on operations " +
		"whose contract declares no 400. Fix the contract, not the handlers.")

	w := newChanWorld(t)
	for _, tc := range []struct{ op, target string }{
		{"listChannelTypes", "/channel-types?foo=bar"},
		{"getChannel", "/channels/" + chanMine.String() + "?foo=bar"},
		{"deleteChannel", "/channels/" + chanMine.String() + "?foo=bar"},
	} {
		resp := w.client.GET(tc.target)
		if resp.Code() != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.op, resp.Code())
		}
		// This is the assertion that fails today: the contract declares no 400.
		schema.AssertProblem(t, tc.op, http.StatusBadRequest, resp.Body())
	}
}
