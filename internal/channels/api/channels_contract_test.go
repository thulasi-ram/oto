package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/service"
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

// ValidateConnectionConfig shares configErr/configCalls with ValidateConfig:
// no test in this file exercises both a channel and a connection create in the
// same call, so one counter is enough to prove "validated on the server" either
// way.
func (f *chanRegistry) ValidateConnectionConfig(context.Context, domain.Type, json.RawMessage) error {
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
	return domain.Instance{
		ID: uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104a"), OrgID: s.OrgID(),
		Type: in.Type, Name: in.Name, Config: in.Config,
		ConnectionID: in.ConnectionID,
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

// chanConnStore is a ConnectionStore keyed by id, with the same tenant
// isolation chanStore proves for channels one hop over.
type chanConnStore struct {
	byID map[uuid.UUID]domain.Connection
	// referencing is what ReferencingChannels answers for a live id.
	referencing []string
	created     []domain.NewConnection
	patched     []domain.ConnectionPatch
	deleted     []uuid.UUID
	listPage    []domain.Connection
}

func (f *chanConnStore) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Connection, error) {
	conn, ok := f.byID[id]
	if !ok || conn.OrgID != s.OrgID() {
		return domain.Connection{}, errs.NotFound("connection_not_found", "no such connection")
	}
	return conn, nil
}

func (f *chanConnStore) List(
	_ context.Context, s db.TenantScope, _ bool, _ db.Keyset,
) ([]domain.Connection, db.Cursor, error) {
	out := make([]domain.Connection, 0, len(f.listPage))
	for _, c := range f.listPage {
		if c.OrgID == s.OrgID() {
			out = append(out, c)
		}
	}
	return out, db.Cursor{}, nil
}

func (f *chanConnStore) Create(
	_ context.Context, s db.TenantScope, in domain.NewConnection,
) (domain.Connection, error) {
	f.created = append(f.created, in)
	now := chanNow
	id := in.ID
	if id == uuid.Nil {
		id = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317493333")
	}
	return domain.Connection{
		ID: id, OrgID: s.OrgID(), Type: in.Type, Name: in.Name, Config: in.Config,
		CredentialID: in.CredentialID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (f *chanConnStore) Update(
	ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.ConnectionPatch,
) (domain.Connection, error) {
	conn, err := f.Get(ctx, s, id)
	if err != nil {
		return domain.Connection{}, err
	}
	f.patched = append(f.patched, p)
	if p.Name != nil {
		conn.Name = *p.Name
	}
	return conn, nil
}

func (f *chanConnStore) SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error {
	if _, err := f.Get(ctx, s, id); err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *chanConnStore) ReferencingChannels(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) ([]string, error) {
	if _, err := f.Get(ctx, s, id); err != nil {
		return nil, err
	}
	return f.referencing, nil
}

// chanResolver is a ConversationResolver that answers from a table instead of
// from Slack. It records the scope it was given, because the whole tenancy
// question for this operation is whether the connection id was resolved under
// the caller's org before a credential was ever opened.
type chanResolver struct {
	// answer is what a live connection resolves to, whichever half was asked.
	answer domain.ConversationResult
	err    error
	// queries is every lookup that reached this far, in order.
	queries []domain.ConversationQuery
}

func (f *chanResolver) ResolveConversation(
	_ context.Context, s db.TenantScope, connectionID uuid.UUID, q domain.ConversationQuery,
) (domain.ConversationResult, error) {
	// The resolver holds the tenant boundary itself: it opens a connection's
	// credential, so a request naming somebody else's connection must die here
	// rather than reach Slack with the wrong workspace's token.
	if connectionID != chanConnMine || s.OrgID() != apitest.OrgID {
		return domain.ConversationResult{}, errs.NotFound("connection_not_found", "no such connection")
	}
	f.queries = append(f.queries, q)
	if f.err != nil {
		return domain.ConversationResult{}, f.err
	}
	return f.answer, nil
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

// chanConnMine is the slack connection chanMine references, owned by the same
// tenant.
var chanConnMine = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317494444")

// chanConnStranger is a slack connection owned by apitest.OtherOrgID, for the
// cross-tenant type-check test.
var chanConnStranger = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317495555")

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
		CredentialKinds: []string{},
		ConnectionConfigSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema",` +
			`"type":"object","required":["team_id"],` +
			`"properties":{"team_id":{"type":"string","pattern":"^T[A-Z0-9]{2,}$"}}}`),
		ConnectionCredentialKinds: []string{"slack_bot_token"},
		Capabilities: domain.CapThreading | domain.CapAmend | domain.CapRichLayout |
			domain.CapInteractive,
		Renderers:      []domain.RendererID{domain.RendererSlackDefault},
		RateLimitClass: "slack",
	}
}

func webhookDescriptor() domain.Descriptor {
	return domain.Descriptor{
		Type:                      domain.TypeWebhook,
		DisplayName:               "Webhook",
		ConfigSchema:              json.RawMessage(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}`),
		CredentialKinds:           []string{},
		ConnectionConfigSchema:    json.RawMessage(`{"type":"object","properties":{}}`),
		ConnectionCredentialKinds: []string{"bearer", "basic", "webhook_signing_secret", "none"},
		Capabilities:              domain.CapDedupeKey,
		Renderers:                 []domain.RendererID{domain.RendererWebhookJSON},
		RateLimitClass:            "none",
	}
}

// channelFixture is one stored destination, healthy and fully populated, so that
// a schema failure is about the mapping rather than about an absent field. It
// references chanConnMine — the credential lives there now, not here.
func channelFixture(id, org uuid.UUID) domain.Instance {
	checked := chanNow.Add(-time.Minute)
	return domain.Instance{
		ID:              id,
		OrgID:           org,
		Type:            domain.TypeSlack,
		Name:            "#sre-alerts",
		Config:          json.RawMessage(`{"conversation_id":"C7F2X9QLM"}`),
		ConnectionID:    chanConnMine,
		Capabilities:    domain.CapThreading | domain.CapAmend,
		Renderer:        domain.RendererSlackDefault,
		Verbosity:       domain.VerbosityStatusChanges,
		ThreadUpdates:   true,
		ShowFieldEmoji:  true,
		Enabled:         true,
		Health:          domain.InstanceHealthy,
		HealthCheckedAt: &checked,
		CreatedAt:       chanNow.Add(-30 * 24 * time.Hour),
		UpdatedAt:       chanNow.Add(-time.Hour),
	}
}

// connectionFixture is one stored connection, fully populated, owned by org.
func connectionFixture(id, org uuid.UUID) domain.Connection {
	kind := "slack_bot_token"
	rotated := chanNow.Add(-72 * time.Hour)
	credential := uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317492222")
	return domain.Connection{
		ID:                  id,
		OrgID:               org,
		Type:                domain.TypeSlack,
		Name:                "Acme Slack workspace",
		Config:              json.RawMessage(`{"team_id":"T9TK3CUKW"}`),
		CredentialID:        &credential,
		CredentialKind:      kind,
		CredentialRotatedAt: &rotated,
		CreatedAt:           chanNow.Add(-30 * 24 * time.Hour),
		UpdatedAt:           chanNow.Add(-time.Hour),
	}
}

// chanWorld is one wired router plus the fakes behind it.
type chanWorld struct {
	registry    *chanRegistry
	store       *chanStore
	connections *chanConnStore
	creds       *chanCreds
	resolver    *chanResolver
	tester      *chanTester
	writer      *service.Writer
	client      *apitest.Client
}

// newChanWorld wires the Channels router with two providers, one destination
// owned by apitest.OrgID and one owned by apitest.OtherOrgID, each referencing
// a connection owned by the same tenant as its channel.
//
// The stranger row EXISTS in the store. That matters: a probe against an id that
// is simply absent proves nothing about tenancy, because "no such row" and "not
// yours" would be the same answer for the wrong reason.
func newChanWorld(t *testing.T) *chanWorld {
	t.Helper()

	mine := channelFixture(chanMine, apitest.OrgID)
	stranger := channelFixture(apitest.StrangerID, apitest.OtherOrgID)
	stranger.ConnectionID = chanConnStranger

	connMine := connectionFixture(chanConnMine, apitest.OrgID)
	connStranger := connectionFixture(chanConnStranger, apitest.OtherOrgID)

	store := &chanStore{
		byID: map[uuid.UUID]domain.Instance{
			chanMine:           mine,
			apitest.StrangerID: stranger,
		},
		listPage: []domain.Instance{mine, stranger},
	}
	connStore := &chanConnStore{
		byID: map[uuid.UUID]domain.Connection{
			chanConnMine:     connMine,
			chanConnStranger: connStranger,
		},
		listPage: []domain.Connection{connMine, connStranger},
	}
	w := &chanWorld{
		registry:    &chanRegistry{descriptors: []domain.Descriptor{slackDescriptor(), webhookDescriptor()}},
		store:       store,
		connections: connStore,
		creds:       &chanCreds{},
		resolver: &chanResolver{
			answer: domain.ConversationResult{ID: "C7F2X9QLM", Name: "sre-alerts"},
		},
		tester: &chanTester{
			store:  store,
			result: domain.TestResult{OK: true, ProviderConversationID: "C7F2X9QLM", ProviderMessageID: "1723023262.114300", CheckedAt: chanNow},
		},
	}
	// The REAL write facade over the fake store and the fake tester: the claim
	// path is the thing under test on two of these operations, and a fake writer
	// would prove nothing about it. With no claim store wired, an UNKEYED request
	// behaves exactly as it always did and a KEYED one is the declared `503`.
	writer, err := service.NewWriter(service.WriterOptions{
		Store:  w.store,
		Tester: w.tester,
		Clock:  clock.NewFake(chanNow),
	})
	require.NoError(t, err)
	w.writer = writer

	rt := NewRouter(Options{
		Registry:    w.registry,
		Channels:    w.store,
		Connections: w.connections,
		Creds:       w.creds,
		Resolver:    w.resolver,
		Writes:      w.writer,
		Clock:       clock.NewFake(chanNow),
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

// TestListConnectionsAnswersAPageOfThisTenantsSetups.
//
// The admin half of ADR 0047's split: Settings lists CONNECTIONS, and one
// connection stands for a whole workspace rather than for a destination. The
// stranger's connection exists in the same store, so a handler that forgot its
// scope would serve two.
func TestListConnectionsAnswersAPageOfThisTenantsSetups(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channel-connections").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listChannelConnections", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d connections, want 1 — the other belongs to %s",
			len(data), apitest.OtherOrgID)
	}
	// A list is the widest surface a secret could escape through: one row per
	// workspace, and every one of them owns a sealed bot token.
	first, _ := data[0].(map[string]any)
	for _, forbidden := range []string{"credential", "values", "token", "credential_id"} {
		if _, present := first[forbidden]; present {
			t.Fatalf("⛔ the connection list carries %q: %#v", forbidden, first[forbidden])
		}
	}
}

// ⭐ TestCreatingAConnectionSealsTheSecretAndNeverEchoesIt.
//
// This is the one assertion in the file whose failure is a security incident
// rather than a bug. `credential.values` is write-only: it travels from the
// decoded DTO into the sealer and is referenced nowhere else, and no endpoint in
// this API has a way to read it back. The response is checked for the literal
// token bytes because a leak would not announce itself as a named field — it
// would arrive inside some helpfully echoed request object.
//
// This used to be about creating a CHANNEL. It is a connection's credential now
// — a channel no longer carries one at all — so the assertion moved with it.
func TestCreatingAConnectionSealsTheSecretAndNeverEchoesIt(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	const botToken = "xoxb-0000000000-1111111111-notarealtoken" //nolint:gosec // a fixture, not a credential

	body := map[string]any{
		"type":   "slack",
		"name":   "Acme Slack workspace 2",
		"config": map[string]any{"team_id": "T9TK3CUKW"},
		"credential": map[string]any{
			"kind":   "slack_bot_token",
			"values": map[string]string{"token": botToken},
		},
	}
	// The fixture is proved to be one a real client could have sent, so this test
	// cannot pass with a request the contract forbids.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "createChannelConnection", raw)

	resp := w.client.POST(t, "/channel-connections", body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createChannelConnection", http.StatusCreated, resp.Body())

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
		t.Fatalf("ValidateConnectionConfig ran %d times; the provider schema must be applied on every write",
			w.registry.configCalls)
	}
}

// TestCreatingAChannelCarriesNoCredentialAndReferencesAConnection.
//
// A channel no longer has anywhere to put a secret — `connection_id` is the
// whole of what it says about where its credential lives.
func TestCreatingAChannelCarriesNoCredentialAndReferencesAConnection(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	body := map[string]any{
		"type":          "slack",
		"name":          "#sre-alerts-2",
		"config":        map[string]any{"conversation_id": "C7F2X9QLM"},
		"connection_id": chanConnMine.String(),
		"renderer":      "slack.default",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "createChannel", raw)

	resp := w.client.POST(t, "/channels", body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createChannel", http.StatusCreated, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if got := data["connection_id"]; got != chanConnMine.String() {
		t.Fatalf("connection_id = %v, want %s", got, chanConnMine)
	}
	for _, forbidden := range []string{"credential", "credential_kind", "credential_rotated_at"} {
		if _, present := data[forbidden]; present {
			t.Fatalf("⛔ the channel response carries %q: %#v", forbidden, data[forbidden])
		}
	}
}

// TestGetConnectionServesOneSetupAndOnlyTheSafeHalfOfItsSecret.
//
// `credential_kind` and `credential_rotated_at` are the ONLY things this API ever
// says about a secret: which kind is attached, and when it last moved.
func TestGetConnectionServesOneSetupAndOnlyTheSafeHalfOfItsSecret(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channel-connections/"+chanConnMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getChannelConnection", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if got := data["credential_kind"]; got != "slack_bot_token" {
		t.Fatalf("credential_kind = %v, want slack_bot_token", got)
	}
	for _, forbidden := range []string{"credential", "values", "token", "credential_id"} {
		if _, present := data[forbidden]; present {
			t.Fatalf("⛔ the connection response carries %q: %#v", forbidden, data[forbidden])
		}
	}
}

// TestGetChannelCarriesConnectionIdAndNoCredentialFields.
//
// A channel's response points at its connection and says nothing about a
// secret directly — there is no field left on this DTO that could.
func TestGetChannelCarriesConnectionIdAndNoCredentialFields(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.GET("/channels/"+chanMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getChannel", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if got := data["connection_id"]; got != chanConnMine.String() {
		t.Fatalf("connection_id = %v, want %s", got, chanConnMine)
	}
	for _, forbidden := range []string{"credential", "credential_kind", "credential_rotated_at"} {
		if _, present := data[forbidden]; present {
			t.Fatalf("⛔ the channel response carries %q: %#v", forbidden, data[forbidden])
		}
	}
}

// TestUpdatingAConnectionRotatesTheSecretInPlace.
//
// A supplied credential ROTATES rather than detaching and re-attaching, so
// every channel referencing the connection never spends a moment pointing at
// nothing — and the new plaintext is no more echoed than the old one was.
func TestUpdatingAConnectionRotatesTheSecretInPlace(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	const rotatedToken = "xoxb-2222222222-3333333333-alsonotreal" //nolint:gosec // a fixture, not a credential

	body := map[string]any{
		"name":       "Acme Slack workspace v2",
		"credential": map[string]any{"kind": "slack_bot_token", "values": map[string]string{"token": rotatedToken}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "updateChannelConnection", raw)

	resp := w.client.PATCH(t, "/channel-connections/"+chanConnMine.String(), body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateChannelConnection", http.StatusOK, resp.Body())

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

// TestDeletingAConnectionIsA204WithNothingInIt.
//
// The same RFC 9110 §15.3.5 shape `deleteChannel` holds one hop over. A
// connection nothing references is the only one that gets here — the 409 for one
// that is still referenced has its own test below.
func TestDeletingAConnectionIsA204WithNothingInIt(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.DELETE("/channel-connections/"+chanConnMine.String()).
		MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteChannelConnection", http.StatusNoContent, resp.Body())

	if resp.Header("Content-Type") != "" {
		t.Fatalf("the 204 advertises Content-Type %q for a body that cannot exist", resp.Header("Content-Type"))
	}
	if len(w.connections.deleted) != 1 {
		t.Fatalf("SoftDelete ran %d times, want 1", len(w.connections.deleted))
	}
}

// ⭐ TestResolvingAConversationFillsInTheHalfTheOperatorDidNotType.
//
// This is the operation ADR 0047 reopened `channels:read`/`groups:read` for, and
// the reason it is allowed to exist is entirely about WHEN it runs: a human is
// typing into a settings form, once, naming a destination. It is not on the
// delivery path, so C9 — "oto never reads Slack to reconstruct its own state" —
// is untouched.
//
// Both directions go through one endpoint because the operator only ever knows
// one half: type `#sre-alerts` and get the id, or paste the id out of Slack's
// own "Copy link" menu and get the name back to check it against what you meant.
func TestResolvingAConversationFillsInTheHalfTheOperatorDidNotType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body map[string]any
		want domain.ConversationQuery
	}{
		{"a name resolves to an id", map[string]any{"name": "sre-alerts"},
			domain.ConversationQuery{Name: "sre-alerts"}},
		{"an id resolves to a name", map[string]any{"conversation_id": "C7F2X9QLM"},
			domain.ConversationQuery{ID: "C7F2X9QLM"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newChanWorld(t)
			raw, err := json.Marshal(tc.body)
			require.NoError(t, err)
			schema.AssertRequest(t, "resolveSlackConversation", raw)

			resp := w.client.POST(t, "/channel-connections/"+chanConnMine.String()+"/slack/resolve", tc.body).
				MustStatus(t, http.StatusOK)
			schema.Assert(t, "resolveSlackConversation", http.StatusOK, resp.Body())

			data, _ := resp.JSON(t)["data"].(map[string]any)
			if data["conversation_id"] != "C7F2X9QLM" || data["conversation_name"] != "sre-alerts" {
				t.Fatalf("the answer is missing a half: %#v", data)
			}
			// The half the operator typed is the half that goes to Slack. Sending
			// both would let a mismatched pair resolve to whichever one Slack
			// happened to look at first.
			if len(w.resolver.queries) != 1 || w.resolver.queries[0] != tc.want {
				t.Fatalf("the resolver was asked %#v, want exactly one %#v", w.resolver.queries, tc.want)
			}
		})
	}
}

// ⛔ TestResolvingWithNeitherHalfNamesTheFieldThatIsEmpty.
//
// An empty body is the one request this endpoint cannot answer — there is no
// half to fill the other in from — and it is a 422 naming a control rather than
// a round trip to Slack that asks for nothing.
func TestResolvingWithNeitherHalfNamesTheFieldThatIsEmpty(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.POST(t, "/channel-connections/"+chanConnMine.String()+"/slack/resolve",
		map[string]any{}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "resolveSlackConversation", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "name")

	if len(w.resolver.queries) != 0 {
		t.Fatal("⛔ an empty query still reached the resolver, and so would have reached Slack")
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
	// ⭐ THE CONNECTION ROUTES ARE IN THE SAME TABLE, NOT A SECOND TEST. ADR 0047
	// gave the module a second addressable resource, and a connection is the one
	// that HOLDS THE CREDENTIAL — the id in the path decides which workspace's bot
	// token gets opened. A cross-tenant miss here is worse than a leaked channel
	// name: it is oto posting into somebody else's Slack. `chanConnStranger` is a
	// real row owned by `apitest.OtherOrgID`, so a handler that forgot its scope
	// would find it.
	conn := chanConnStranger.String()
	routes := []apitest.Route{
		{Op: "getChannel", Method: http.MethodGet, Path: "/channels/" + stranger},
		{Op: "updateChannel", Method: http.MethodPatch, Path: "/channels/" + stranger, Body: `{"name":"mine now"}`},
		{Op: "deleteChannel", Method: http.MethodDelete, Path: "/channels/" + stranger},
		{Op: "testChannel", Method: http.MethodPost, Path: "/channels/" + stranger + "/test"},
		{Op: "getChannelConnection", Method: http.MethodGet, Path: "/channel-connections/" + conn},
		{
			Op: "updateChannelConnection", Method: http.MethodPatch,
			Path: "/channel-connections/" + conn, Body: `{"name":"mine now"}`,
		},
		{Op: "deleteChannelConnection", Method: http.MethodDelete, Path: "/channel-connections/" + conn},
		{
			Op: "resolveSlackConversation", Method: http.MethodPost,
			Path: "/channel-connections/" + conn + "/slack/resolve", Body: `{"name":"sre-alerts"}`,
		},
	}

	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		w := newChanWorld(t)
		return w.client, func(t *testing.T, _ apitest.Route, resp *apitest.Response) {
			if strings.Contains(string(resp.Body()), "#sre-alerts") {
				t.Fatalf("⛔ the refusal leaked the other tenant's channel name:\n%s", resp.Body())
			}
			if strings.Contains(string(resp.Body()), "Acme Slack workspace") {
				t.Fatalf("⛔ the refusal leaked the other tenant's connection name:\n%s", resp.Body())
			}
			if len(w.store.deleted) != 0 || len(w.store.patched) != 0 {
				t.Fatal("⛔ a cross-tenant request still reached a write")
			}
			if len(w.connections.deleted) != 0 || len(w.connections.patched) != 0 {
				t.Fatal("⛔ a cross-tenant request still reached a connection write")
			}
			if len(w.resolver.queries) != 0 {
				t.Fatal("⛔ a cross-tenant resolve reached the resolver, and so would have opened a token")
			}
		}
	}, routes)
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

// TestASlackConnectionWithoutABotTokenNamesTheControlThatIsEmpty.
//
// `channel_connections_cred_ck` would catch this in the database, but as a
// 23514 — a 500 that tells the operator nothing. Saying it here turns it into
// a field violation pointing at the control they left blank. It used to be a
// channel that could be created without a credential; only a connection can be
// now.
func TestASlackConnectionWithoutABotTokenNamesTheControlThatIsEmpty(t *testing.T) {
	t.Parallel()

	w := newChanWorld(t)
	resp := w.client.POST(t, "/channel-connections", map[string]any{
		"type":   "slack",
		"name":   "Acme Slack workspace 2",
		"config": map[string]any{"team_id": "T9TK3CUKW"},
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createChannelConnection", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "credential")

	if len(w.connections.created) != 0 {
		t.Fatal("a refused create still wrote a connection row")
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
		"type":          "slack",
		"name":          "#sre-alerts",
		"config":        map[string]any{"conversation_id": "C7F2X9QLM"},
		"connection_id": chanConnMine.String(),
		"renderer":      "webhook.json",
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
		"type":          "slack",
		"name":          "#sre-alerts",
		"config":        map[string]any{"conversation_id": "#sre-alerts"},
		"connection_id": chanConnMine.String(),
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

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newChanWorld(t).client, nil
	}, []apitest.Route{
		{Op: "listChannels", Method: http.MethodGet, Path: "/channels?limitt=200"},
	})
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
	// ⛔ `domain.CapBroadcast` WAS A TERM HERE AND IS DELETED (git-bug 7570090). The
	// bit is gone from the bitset and its wire name is gone from `capabilityNames`,
	// so it can neither be set nor served.
	//
	// ⚠️ THE RETIRED BIT POSITION IS DELIBERATELY NOT SET, AND "EVERY BIT" NOW MEANS
	// "every NAMED bit". `channels.capabilities` is a persisted BIGINT and the iota
	// positions are a stored wire contract, so `channels/domain` holds bit 4 open
	// with a `_` placeholder rather than letting `CapDedupeKey` slide from 32 to 16.
	// A held-open bit has no wire name on purpose — giving one to a retired
	// capability would put something on the contract no provider can advertise — and
	// the assertion below compares against `len(capabilityNames)`, which is the list
	// that actually has to stay complete.
	all := domain.CapThreading | domain.CapAmend | domain.CapRichLayout |
		domain.CapInteractive | domain.CapDedupeKey

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
/* §E.3: the parameter nobody asked for                                       */
/* -------------------------------------------------------------------------- */

// TestAnUnknownQueryParameterOnAChannelEndpointIsADeclared400.
//
// `Router.subject` runs `httpx.NewParams(r).Err()` on every `{id}` endpoint and
// `listChannelTypes` does the same, so `?foo=bar` anywhere on this surface is
// `400 unknown_parameter`. §E.3 is binding and deliberate: a silently ignored
// parameter returns the wrong answer and looks right.
//
// ⭐ THE POINT OF THIS TEST IS THE `schema.AssertProblem` CALL. The refusal was
// never in doubt; what was missing until git-bug ee3ae9c was the
// `'400': $ref BadRequest` entry, so the assertion below failed at the LOOKUP —
// a generated client had no branch for a status these handlers really emit, and
// `?_=<timestamp>` from a browser is all it takes to reach one.
func TestAnUnknownQueryParameterOnAChannelEndpointIsADeclared400(t *testing.T) {
	t.Parallel()

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newChanWorld(t).client, nil
	}, []apitest.Route{
		{Op: "listChannelTypes", Method: http.MethodGet, Path: "/channel-types?foo=bar"},
		{Op: "getChannel", Method: http.MethodGet, Path: "/channels/" + chanMine.String() + "?foo=bar"},
		{Op: "deleteChannel", Method: http.MethodDelete, Path: "/channels/" + chanMine.String() + "?foo=bar"},
		{Op: "testChannel", Method: http.MethodPost, Path: "/channels/" + chanMine.String() + "/test?foo=bar"},
	})
}
