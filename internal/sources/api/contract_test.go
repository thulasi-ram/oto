package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// THE SOURCES TAG, ASSERTED AGAINST THE CONTRACT RATHER THAN AGAINST A SECOND
// COPY OF IT.
//
// Every finding of the conformance audit lived in this layer, and every one of
// them was invisible for the same reason: the tests that existed described the
// shape they expected in Go, so a DTO and its test drifted together and agreed
// with each other while disagreeing with `api/openapi/openapi.yaml`. The
// assertions below therefore never spell a body out. They hand the bytes the
// handler wrote to the schema the contract declares for that operationId and
// that status, which is the only assertion that cannot drift with the code it
// checks.
//
// The three promises each operation owes, in the order they appear here:
//
//  1. a request that succeeds answers with a body its own schema accepts;
//  2. another tenant's id is INDISTINGUISHABLE FROM NOTHING AT ALL — 404, never
//     403, never a row, never a 500;
//  3. the refusal a caller can actually provoke is the refusal the contract
//     declares, with `violations[]` naming the field a form must highlight.

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

// The two ids this package owns. They are constants rather than freshly
// generated so a failure message names the exact value that leaked, and so the
// "somebody else's id" probe has something stable to contrast with.
var (
	contractSourceID  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77")
	contractClusterID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b78")
)

// contractStamp is the instant every fixture timestamp is derived from. A fixed
// clock reading keeps `format: date-time` an honest assertion: a handler that
// started emitting unix seconds would fail here rather than pass as "a number".
var contractStamp = time.Date(2026, 8, 9, 12, 0, 0, 114_000_000, time.UTC)

// contractSource is a fully populated source: both URLs, every label list
// non-empty, a real reconcile interval. Sparse fixtures are how a required
// member goes missing without anything noticing.
func contractSource() domain.Source {
	return domain.Source{
		ID:                contractSourceID,
		OrgID:             apitest.OrgID,
		ClusterID:         contractClusterID,
		Name:              "alertmanager-prod-eu",
		Kind:              domain.KindAlertmanager,
		BaseURL:           "https://alertmanager.example.com",
		PrometheusURL:     "https://prometheus.example.com",
		TLSSkipVerify:     false,
		InjectLabels:      map[string]string{"team": "payments"},
		IgnoreLabels:      []string{"prometheus_replica", "__replica__"},
		RedactLabels:      []string{"*password*"},
		RedactAnnotations: []string{"*token*"},
		PushEnabled:       true,
		ReconcileInterval: 30 * time.Second,
		CreatedAt:         contractStamp.Add(-72 * time.Hour),
		UpdatedAt:         contractStamp.Add(-time.Hour),
	}
}

// contractHealth is a source oto HAS probed, with its route timings read off the
// running configuration and one standing warning.
//
// ⭐ The route-timing half is the newest part of this payload and the part a
// fixture is most likely to under-exercise: `route_timings` is required, always
// present, and carries a provenance per field plus the whole delivering-route
// tree. A fixture that left it zero-valued would still satisfy the schema and
// would prove nothing about the numbers a tuning screen does arithmetic on.
func contractHealth() domain.SourceHealth {
	observed := contractStamp.Add(-5 * time.Minute)
	pushed := contractStamp.Add(-90 * time.Second)
	sendResolved := true

	return domain.SourceHealth{
		SourceID:            contractSourceID,
		OrgID:               apitest.OrgID,
		Status:              domain.HealthHealthy,
		LastPushAt:          &pushed,
		LastReconcileAt:     &observed,
		LastReconcileStatus: "ok",
		LastError:           "",
		ConsecutiveFailures: 0,
		AMVersion:           "0.28.1",
		SendResolved:        &sendResolved,
		ClockSkew:           412 * time.Millisecond,
		DivergenceCount:     0,
		RouteTimings: domain.RouteTimings{
			GroupWait:           dur(30 * time.Second),
			GroupInterval:       dur(5 * time.Minute),
			ChildRoutes:         2,
			ChildrenWithTimings: 1,
		},
		Routes: domain.RouteResolution{
			Receiver:         "oto",
			Basis:            domain.ReceiverSoleWebhook,
			WebhookReceivers: []string{"oto"},
			Routes: []domain.ReceiverRoute{
				route("fallback", []domain.RouteStep{step()},
					dur(30*time.Second), dur(5*time.Minute), nil),
				route("oto", []domain.RouteStep{step(), step(`team="sre"`)},
					nil, dur(time.Minute), nil),
			},
		},
		RouteTimingsAt: &observed,
		Warnings: []domain.HealthWarning{{
			Code:    domain.WarnClockSkew,
			Message: "this source's clock is 412ms behind oto's",
			Subject: "https://alertmanager.example.com",
			At:      observed,
		}},
		UpdatedAt: contractStamp,
	}
}

// contractProbe is a probe that reached the upstream and found one receiver with
// `send_resolved` disabled — the observation C15 exists for, and the reason the
// receiver map is on this DTO at all.
func contractProbe() service.ProbeResult {
	sendResolved := true
	return service.ProbeResult{
		SourceID:             contractSourceID,
		CheckedAt:            contractStamp,
		Latency:              42 * time.Millisecond,
		Reachable:            true,
		AlertmanagerVersion:  "0.28.1",
		ClusterStatus:        "ready",
		ClusterPeers:         3,
		SendResolved:         &sendResolved,
		MutedReceivers:       []string{"pager-only"},
		ClockSkew:            412 * time.Millisecond,
		PrometheusConfigured: true,
		PrometheusReachable:  true,
	}
}

// contractReconcile is one completed pass, including the two counts nothing else
// in the system can produce: `suppressed_observed` and `divergence_count`.
func contractReconcile() domain.ReconcileResult {
	return domain.ReconcileResult{
		SourceID:           contractSourceID,
		OK:                 true,
		StartedAt:          contractStamp.Add(-3 * time.Second),
		FinishedAt:         contractStamp,
		Observed:           214,
		SuppressedObserved: 12,
		Recovered:          1,
		MissingUpstream:    3,
		DivergenceCount:    0,
	}
}

func contractCluster() domain.Cluster {
	return domain.Cluster{
		ID:          contractClusterID,
		OrgID:       apitest.OrgID,
		Key:         "prod-eu",
		DisplayName: "Production EU",
		SourceCount: 2,
		CreatedAt:   contractStamp.Add(-96 * time.Hour),
		UpdatedAt:   contractStamp.Add(-time.Hour),
	}
}

// notMine is the ONE refusal a tenant boundary is allowed to make. It is a 404
// and not a 403 because a 403 confirms the row exists, which is the whole of
// what a cross-tenant prober wanted to learn.
func notMine() error { return errs.NotFound("not_found", "no such resource") }

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// contractSources is a SourceReader that owns EXACTLY ONE source. Every id it
// does not own answers not-found, which is what makes the tenant probe a fact
// about the handler rather than a fact about the fake.
type contractSources struct {
	src    domain.Source
	health domain.SourceHealth
	probe  service.ProbeResult
}

func (f *contractSources) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.Source, error) {
	if id != f.src.ID {
		return domain.Source{}, notMine()
	}
	return f.src, nil
}

func (f *contractSources) List(
	context.Context, db.TenantScope, domain.SourceFilter, db.Keyset,
) ([]domain.Source, db.Cursor, error) {
	return []domain.Source{f.src}, db.Cursor{}, nil
}

func (f *contractSources) Health(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.SourceHealth, error) {
	if id != f.src.ID {
		return domain.SourceHealth{}, notMine()
	}
	return f.health, nil
}

func (f *contractSources) Probe(_ context.Context, _ db.TenantScope, id uuid.UUID) (service.ProbeResult, error) {
	if id != f.src.ID {
		return service.ProbeResult{}, notMine()
	}
	return f.probe, nil
}

// contractRegistry is the write side, scoped the same way.
type contractRegistry struct {
	src     domain.Source
	health  domain.SourceHealth
	created int
	updated int
	deleted int
	// deletedInTx records whether the soft delete joined the caller's unit of work.
	deletedInTx bool
}

func (f *contractRegistry) Create(
	_ context.Context, s db.TenantScope, in domain.SourceDraft,
) (domain.Source, error) {
	f.created++
	out := f.src
	out.OrgID = s.OrgID()
	out.ClusterID = in.ClusterID
	out.Name = in.Name
	out.Kind = in.Kind
	out.BaseURL = in.BaseURL
	out.PrometheusURL = in.PrometheusURL
	out.TLSSkipVerify = in.TLSSkipVerify
	out.InjectLabels = in.InjectLabels
	out.IgnoreLabels = in.IgnoreLabels
	out.RedactLabels = in.RedactLabels
	out.RedactAnnotations = in.RedactAnnotations
	out.PushEnabled = in.PushEnabled
	out.ReconcileInterval = in.ReconcileInterval
	return out, nil
}

func (f *contractRegistry) Update(
	_ context.Context, _ db.TenantScope, id uuid.UUID, p domain.SourcePatch,
) (domain.Source, error) {
	if id != f.src.ID {
		return domain.Source{}, notMine()
	}
	f.updated++
	out := f.src
	if p.Name != nil {
		out.Name = *p.Name
	}
	return out, nil
}

func (f *contractRegistry) SoftDelete(ctx context.Context, _ db.TenantScope, id uuid.UUID) error {
	if id != f.src.ID {
		return notMine()
	}
	f.deletedInTx = joinedUnitOfWork(ctx)
	f.deleted++
	return nil
}

func (f *contractRegistry) HealthFor(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]domain.SourceHealth, error) {
	out := map[uuid.UUID]domain.SourceHealth{}
	for _, id := range ids {
		if id == f.src.ID {
			out[id] = f.health
		}
	}
	return out, nil
}

// contractClusters owns exactly one cluster, on the same rule.
type contractClusters struct {
	cluster domain.Cluster
	created int
}

func (f *contractClusters) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.Cluster, error) {
	if id != f.cluster.ID {
		return domain.Cluster{}, notMine()
	}
	return f.cluster, nil
}

func (f *contractClusters) List(
	context.Context, db.TenantScope, bool, db.Keyset,
) ([]domain.Cluster, db.Cursor, error) {
	return []domain.Cluster{f.cluster}, db.Cursor{}, nil
}

func (f *contractClusters) Create(
	_ context.Context, s db.TenantScope, key, displayName string,
) (domain.Cluster, error) {
	f.created++
	out := f.cluster
	out.OrgID = s.OrgID()
	out.Key = key
	out.DisplayName = displayName
	out.SourceCount = 0
	return out, nil
}

func (f *contractClusters) UpdateDisplayName(
	_ context.Context, _ db.TenantScope, id uuid.UUID, displayName string,
) (domain.Cluster, error) {
	if id != f.cluster.ID {
		return domain.Cluster{}, notMine()
	}
	out := f.cluster
	out.DisplayName = displayName
	return out, nil
}

func (f *contractClusters) ClusterKeysFor(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if id == f.cluster.ID {
			out[id] = f.cluster.Key
		}
	}
	return out, nil
}

// contractReconciler stands in for the ingestion module's reconciler.
type contractReconciler struct {
	res   domain.ReconcileResult
	calls int
}

func (f *contractReconciler) Reconcile(
	_ context.Context, _ db.TenantScope, _ uuid.UUID,
) (domain.ReconcileResult, error) {
	f.calls++
	return f.res, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// contractStack is one fully-wired router plus the fakes behind it. The four
// `drop*` switches remove a collaborator, because "this deployment is not wired
// for that" is a declared 503 on several of these operations and a nil-pointer
// panic is the alternative.
type contractStack struct {
	sources   *contractSources
	registry  *contractRegistry
	clusters  *contractClusters
	tokens    *fakeTokens
	tx        *fakeTx
	reconcile *contractReconciler
	feeds     *contractFeeds

	dropRegistry  bool
	dropClusters  bool
	dropTokens    bool
	dropReconcile bool
	dropFeeds     bool
}

func newContractStack() *contractStack {
	src, health := contractSource(), contractHealth()
	return &contractStack{
		sources:   &contractSources{src: src, health: health, probe: contractProbe()},
		registry:  &contractRegistry{src: src, health: health},
		clusters:  &contractClusters{cluster: contractCluster()},
		tokens:    &fakeTokens{},
		tx:        &fakeTx{},
		reconcile: &contractReconciler{res: contractReconcile()},
		feeds:     newContractFeeds(),
	}
}

// router builds the real Router over the stack.
//
// There is deliberately NO AddressGuard: these tests are about the wire shape,
// and a guard would make every create depend on the DNS of whatever machine is
// running them. `sources_guard_test.go` owns that half.
func (s *contractStack) router() *Router {
	o := Options{
		Sources: s.sources,
		Tx:      s.tx,
		Clock:   clock.New(),
		BaseURL: "https://oto.example.com",
	}
	if !s.dropRegistry {
		o.Registry = s.registry
	}
	if !s.dropClusters {
		o.Clusters = s.clusters
	}
	if !s.dropTokens {
		o.Tokens = s.tokens
	}
	if !s.dropReconcile {
		o.Reconcile = s.reconcile
	}
	if !s.dropFeeds {
		o.Feeds = s.feeds
	}
	return NewRouter(o)
}

// client drives the router as the least-privileged caller every one of these
// endpoints accepts: a human session in apitest.OrgID.
func (s *contractStack) client() *apitest.Client { return apitest.New(s.router()) }

// sourcePath renders a router-relative path. apitest mounts the domain router at
// the chi ROOT, so requests are `/sources/{id}` while the operationIds still name
// the full `/api/v1/...` operation.
func sourcePath(id uuid.UUID, suffix string) string { return "/sources/" + id.String() + suffix }

// jsonBody marshals a fixture and returns both the bytes (so the REQUEST can be
// asserted against its own schema) and a body a client can send.
func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request fixture: %v", err)
	}
	return raw
}

/* -------------------------------------------------------------------------- */
/* 1. Every operation answers with a body its own schema accepts              */
/* -------------------------------------------------------------------------- */

// TestListSourcesAnswersTheShapeTheContractDeclares.
//
// The list is the settings screen, and it carries each row's health inline
// because that is what tells an operator that the silence from prod-eu is oto
// failing to reach it (§B.4). A page whose envelope or whose row shape drifted
// would break every client at once, which is precisely what went unnoticed
// before this assertion existed.
func TestListSourcesAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().GET("/sources").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSources", http.StatusOK, resp.Body())

	// The join the endpoint exists for: health beside the row, not a second call.
	body := resp.JSON(t)
	rows, ok := body["data"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("data = %#v, want the one source the reader owns", body["data"])
	}
	row, _ := rows[0].(map[string]any)
	if row["health"] == nil {
		t.Fatal("the row carries no health; an operator cannot see why a source has gone quiet")
	}
	if row["cluster_key"] != "prod-eu" {
		t.Fatalf("cluster_key = %v, want the joined key", row["cluster_key"])
	}
}

// TestCreateSourceAnswersTheShapeTheContractDeclares.
//
// The request fixture is validated against the CONTRACT'S request schema first:
// a handler test that passes on a body no client would be allowed to send is a
// test of nothing.
func TestCreateSourceAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	raw := jsonBody(t, map[string]any{
		"name":                       "alertmanager-prod-eu",
		"cluster_id":                 contractClusterID.String(),
		"kind":                       "alertmanager",
		"base_url":                   "https://alertmanager.example.com",
		"prometheus_url":             "https://prometheus.example.com",
		"inject_labels":              map[string]string{"team": "payments"},
		"ignore_labels":              []string{"prometheus_replica", "__replica__"},
		"redact_labels":              []string{"*password*"},
		"redact_annotations":         []string{"*token*"},
		"push_enabled":               true,
		"reconcile_interval_seconds": 60,
	})
	schema.AssertRequest(t, "createSource", raw)

	s := newContractStack()
	resp := s.client().
		Raw(http.MethodPost, "/sources", "application/json", string(raw)).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createSource", http.StatusCreated, resp.Body())

	if s.registry.created != 1 || s.tokens.issued != 1 {
		t.Fatalf("writes = %d source, %d token; the source and its credential are one fact",
			s.registry.created, s.tokens.issued)
	}
}

// TestGetSourceAnswersTheShapeTheContractDeclares.
func TestGetSourceAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		GET(sourcePath(contractSourceID, "")).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getSource", http.StatusOK, resp.Body())
}

// TestUpdateSourceAnswersTheShapeTheContractDeclares.
func TestUpdateSourceAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	raw := jsonBody(t, map[string]any{"name": "alertmanager-prod-eu-b"})
	schema.AssertRequest(t, "updateSource", raw)

	s := newContractStack()
	resp := s.client().
		Raw(http.MethodPatch, sourcePath(contractSourceID, ""), "application/json", string(raw)).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateSource", http.StatusOK, resp.Body())

	if s.registry.updated != 1 {
		t.Fatalf("the registry saw %d updates, want 1", s.registry.updated)
	}
}

// TestDeleteSourceAnswersNoBodyAtAll.
//
// ⛔ A 204 carries no body and no Content-Type. The delete endpoints once shipped
// `204 + application/json`, which is a trap for any client literal enough to try
// to parse zero bytes as JSON.
func TestDeleteSourceAnswersNoBodyAtAll(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().DELETE(sourcePath(contractSourceID, "")).MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteSource", http.StatusNoContent, resp.Body())

	// Deleting revokes: a source that has been deleted must not still be pushable.
	if s.registry.deleted != 1 || s.tokens.revoked != 1 {
		t.Fatalf("delete = %d rows, %d revocations; a soft delete that leaves a live "+
			"ingest token is a soft delete in name only", s.registry.deleted, s.tokens.revoked)
	}

	// ⭐ AND IT REVOKES IN THE SAME BREATH. Both writes must have run on the
	// transaction the unit of work handed down; two commits mean a failure in the
	// second leaves a deleted source whose ingest token still authenticates.
	if !s.tx.committed {
		t.Fatal("the delete did not run inside a unit of work")
	}
	if !s.registry.deletedInTx || !s.tokens.revokedInTx {
		t.Fatalf("joined the unit of work: soft delete = %t, revocation = %t; both writes "+
			"must commit together or the deleted source keeps a live credential",
			s.registry.deletedInTx, s.tokens.revokedInTx)
	}
}

// ⭐ TestDeleteSourceRollsBackWhenRevocationFails is the negative half.
//
// A revocation that fails must take the soft delete down with it. The alternative
// is the state this endpoint exists to prevent: `alert_sources.deleted_at` set,
// the row gone from every screen, and a live `api_tokens` row still answering for
// a source no operator can find in order to revoke it.
func TestDeleteSourceRollsBackWhenRevocationFails(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	s.tokens.revokeErr = errors.New("the identity store is having a bad day")

	resp := s.client().DELETE(sourcePath(contractSourceID, ""))
	if resp.Code() < 400 {
		t.Fatalf("status = %d, want a failure", resp.Code())
	}
	if !s.tx.rolledBack || s.tx.committed {
		t.Fatalf("rolled back = %t, committed = %t; a failed revocation must undo the "+
			"soft delete", s.tx.rolledBack, s.tx.committed)
	}
}

// TestTestSourceAnswersTheShapeTheContractDeclares.
//
// The probe reports a receiver with `send_resolved: false`, because that map is
// the payload that matters most here: a receiver with it disabled can never tell
// oto an alert ended, so every alert routed through it EXPIRES rather than
// resolves.
func TestTestSourceAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		POST(t, sourcePath(contractSourceID, "/test"), nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "testSource", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	muted, _ := data["send_resolved"].(map[string]any)
	if muted["pager-only"] != false {
		t.Fatalf("send_resolved = %#v, want the muted receiver reported as false", data["send_resolved"])
	}
}

// TestRotateTokenAnswersTheShapeTheContractDeclares.
//
// The rotation response is one of exactly two bodies in this API that ever
// contain a secret, and its `token_prefix` pattern is load bearing: an ingest
// prefix is fifteen characters, not the twelve a PAT's is.
func TestRotateTokenAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().
		POST(t, sourcePath(contractSourceID, "/rotate-token"), nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "rotateSourceIngestToken", http.StatusOK, resp.Body())

	if s.tokens.issued != 1 {
		t.Fatalf("the issuer minted %d tokens, want 1", s.tokens.issued)
	}
}

// TestReconcileSourceAnswersTheShapeTheContractDeclares.
func TestReconcileSourceAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().
		POST(t, sourcePath(contractSourceID, "/reconcile"), nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "reconcileSource", http.StatusOK, resp.Body())

	if s.reconcile.calls != 1 {
		t.Fatalf("the reconciler ran %d times, want 1", s.reconcile.calls)
	}
	// `suppressed_observed` travels: it is the ONLY way suppression can be
	// observed at all, because Alertmanager's mute stage drops silenced alerts
	// before any webhook fires.
	data, _ := resp.JSON(t)["data"].(map[string]any)
	if data["suppressed_observed"] != float64(12) {
		t.Fatalf("suppressed_observed = %v, want 12", data["suppressed_observed"])
	}
}

// TestGetSourceHealthAnswersTheShapeTheContractDeclares.
//
// ⭐ This is the payload the route-timing fields were added to. `route_timings`
// is REQUIRED and `additionalProperties: false`, so both an omission and an
// invented member fail here — and each of the three durations must carry the
// provenance of its number, because `observed` and `default_applies` call for
// different actions from the operator reading them.
func TestGetSourceHealthAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		GET(sourcePath(contractSourceID, "/health")).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getSourceHealth", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	tm, ok := data["route_timings"].(map[string]any)
	if !ok {
		t.Fatalf("route_timings is %T; it is required and always present", data["route_timings"])
	}
	// The headline three describe oto's OWN route when one could be identified —
	// the top-level route is the fallback, and it frequently governs nothing oto
	// is sent.
	if tm["route"] != RouteOtoReceiver {
		t.Fatalf("route = %v, want %s", tm["route"], RouteOtoReceiver)
	}
	gi, _ := tm["group_interval"].(map[string]any)
	if gi["value_ms"] != float64(60_000) {
		t.Fatalf("group_interval = %v ms, want the 60000 of the route oto hangs off, "+
			"not the top-level route's 300000", gi["value_ms"])
	}
}

// TestListClustersAnswersTheShapeTheContractDeclares.
func TestListClustersAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().GET("/clusters").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listClusters", http.StatusOK, resp.Body())
}

// TestCreateClusterAnswersTheShapeTheContractDeclares.
func TestCreateClusterAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	raw := jsonBody(t, map[string]any{"cluster_key": "prod-us", "display_name": "Production US"})
	schema.AssertRequest(t, "createCluster", raw)

	s := newContractStack()
	resp := s.client().
		Raw(http.MethodPost, "/clusters", "application/json", string(raw)).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createCluster", http.StatusCreated, resp.Body())

	if s.clusters.created != 1 {
		t.Fatalf("the registry created %d clusters, want 1", s.clusters.created)
	}
}

// TestUpdateClusterAnswersTheShapeTheContractDeclares.
func TestUpdateClusterAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	raw := jsonBody(t, map[string]any{"display_name": "Production EU (west)"})
	schema.AssertRequest(t, "updateCluster", raw)

	resp := newContractStack().client().
		Raw(http.MethodPatch, "/clusters/"+contractClusterID.String(), "application/json", string(raw)).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateCluster", http.StatusOK, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* 2. The tenant boundary                                                     */
/* -------------------------------------------------------------------------- */

// ⭐⭐ TestAnotherTenantsIdIsIndistinguishableFromNothingAtAll is the highest-value
// assertion in this file, and it is table-driven because the boundary is a
// property of EVERY id-addressed operation rather than a feature of any one of
// them.
//
// v1 has no roles, so the tenant boundary is the only boundary there is. The
// failure it forbids has three shapes and all three are answered by the same
// status:
//
//   - 200 with somebody else's row — a data leak;
//   - 403 — a leak of one bit, which is the bit a prober wanted: the row EXISTS;
//   - 500 — a leak of the same bit, dressed as a bug.
//
// Every one of them must be a 404 whose body is a well-formed problem document,
// because "no such resource in the caller's org" is the only true statement oto
// can make about an id it does not own.
func TestAnotherTenantsIdIsIndistinguishableFromNothingAtAll(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID
	cases := []struct {
		op     string
		method string
		target string
		body   any
	}{
		{"getSource", http.MethodGet, sourcePath(stranger, ""), nil},
		{"updateSource", http.MethodPatch, sourcePath(stranger, ""), map[string]any{"name": "borrowed"}},
		{"deleteSource", http.MethodDelete, sourcePath(stranger, ""), nil},
		{"testSource", http.MethodPost, sourcePath(stranger, "/test"), nil},
		{"rotateSourceIngestToken", http.MethodPost, sourcePath(stranger, "/rotate-token"), nil},
		{"reconcileSource", http.MethodPost, sourcePath(stranger, "/reconcile"), nil},
		{"getSourceHealth", http.MethodGet, sourcePath(stranger, "/health"), nil},
		{"updateCluster", http.MethodPatch, "/clusters/" + stranger.String(),
			map[string]any{"display_name": "Borrowed"}},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			s := newContractStack()
			c := s.client()

			var resp *apitest.Response
			switch tc.method {
			case http.MethodGet:
				resp = c.GET(tc.target)
			case http.MethodDelete:
				resp = c.DELETE(tc.target)
			case http.MethodPatch:
				resp = c.PATCH(t, tc.target, tc.body)
			default:
				resp = c.POST(t, tc.target, tc.body)
			}

			resp.MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())

			// ⛔ AND NOTHING HAPPENED. A refused write that still reached the
			// registry would be a leak the status line hides.
			if s.registry.updated != 0 || s.registry.deleted != 0 || s.tokens.issued != 0 {
				t.Fatalf("a stranger's id still caused %d updates, %d deletes and %d mints",
					s.registry.updated, s.registry.deleted, s.tokens.issued)
			}
		})
	}
}

// TestTheOwnersOfARowCanStillSeeIt is the other half of the boundary, and it is
// not a formality: a scoping bug that answered 404 to EVERYBODY would satisfy
// the probe above completely.
func TestTheOwnersOfARowCanStillSeeIt(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		As(apitest.MemberOf(apitest.OrgID)).
		GET(sourcePath(contractSourceID, "")).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getSource", http.StatusOK, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* 3. The refusals a caller can provoke                                       */
/* -------------------------------------------------------------------------- */

// ⛔ TestATypoedFilterOnTheSourceListIsRefusedRatherThanIgnored is §E.3.
//
// A silently ignored `?serverity=critical` returns a page of the wrong sources
// and looks exactly like a page of the right ones. The refusal names the
// parameter so the caller can find its own typo.
func TestATypoedFilterOnTheSourceListIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		GET("/sources?serverity=critical").
		MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listSources", http.StatusBadRequest, resp.Body())
	resp.MustViolate(t, "serverity")
}

// TestTheClusterListRefusesTheParameterItDoesNotDeclare.
//
// `since_seq` is legal on `listSources` and is NOT declared on `listClusters`.
// Accepting it here would mean a generic client's polling parameter silently
// changed nothing while appearing to work.
func TestTheClusterListRefusesTheParameterItDoesNotDeclare(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		GET("/clusters?since_seq=42").
		MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listClusters", http.StatusBadRequest, resp.Body())
	resp.MustViolate(t, "since_seq")
}

// TestCreatingASourceWithoutANameNamesTheField.
//
// §L.2.2: a refusal a form cannot act on is a refusal that becomes a support
// ticket. The violation carries the JSON name the caller sent and a machine code
// beside it.
func TestCreatingASourceWithoutANameNamesTheField(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().POST(t, "/sources", map[string]any{
		"cluster_id": contractClusterID.String(),
		"kind":       "alertmanager",
		"base_url":   "https://alertmanager.example.com",
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createSource", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "name")

	if s.registry.created != 0 || s.tokens.issued != 0 {
		t.Fatal("a refused create still wrote a source or minted a token")
	}
}

// TestPatchingASourceWithABadPrometheusURLNamesTheField.
//
// `prometheus_url` carries its own bound because a custom unmarshaller has no
// field for a validator tag to hang on — which is exactly the kind of place a
// bound goes missing. A trailing slash is the commonest way to get it wrong,
// because it is what a browser bar hands you.
func TestPatchingASourceWithABadPrometheusURLNamesTheField(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().
		PATCH(t, sourcePath(contractSourceID, ""),
			map[string]any{"prometheus_url": "https://prometheus.example.com/"}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "updateSource", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "prometheus_url")

	if s.registry.updated != 0 {
		t.Fatal("a refused patch still reached the registry")
	}
}

// TestCreatingAClusterWithAnUnusableKeyNamesTheField.
//
// `cluster_key` participates in ALERT IDENTITY, so its charset is load bearing
// rather than cosmetic: the same label set in two clusters is two different
// alerts, and the key cannot be changed afterwards.
func TestCreatingAClusterWithAnUnusableKeyNamesTheField(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().POST(t, "/clusters", map[string]any{
		"cluster_key":  "Production EU",
		"display_name": "Production EU",
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createCluster", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "cluster_key")

	if s.clusters.created != 0 {
		t.Fatal("a refused create still wrote a cluster")
	}
}

// TestRenamingAClusterToNothingNamesTheField. A blank display name is not a
// rename, and a 200 for it is a 200 whose author will believe something changed.
func TestRenamingAClusterToNothingNamesTheField(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().
		PATCH(t, "/clusters/"+contractClusterID.String(), map[string]any{"display_name": "   "}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "updateCluster", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "display_name")
}

// ⛔ TestAMissingCollaboratorIsAnHonest503 covers the three operations that can
// be reached in a deployment which is not wired for them.
//
// The alternative is a nil-pointer panic rendered as a 500, which invites the
// caller to retry a request that can never succeed. A 503 says "not here" and
// carries a Retry-After, and it is what the contract declares for all three.
func TestAMissingCollaboratorIsAnHonest503(t *testing.T) {
	t.Parallel()

	cases := []struct {
		op     string
		method string
		target string
		drop   func(*contractStack)
	}{
		{"deleteSource", http.MethodDelete, sourcePath(contractSourceID, ""),
			func(s *contractStack) { s.dropRegistry = true }},
		{"rotateSourceIngestToken", http.MethodPost, sourcePath(contractSourceID, "/rotate-token"),
			func(s *contractStack) { s.dropTokens = true }},
		{"reconcileSource", http.MethodPost, sourcePath(contractSourceID, "/reconcile"),
			func(s *contractStack) { s.dropReconcile = true }},
		{"listClusters", http.MethodGet, "/clusters",
			func(s *contractStack) { s.dropClusters = true }},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			s := newContractStack()
			tc.drop(s)
			c := s.client()

			var resp *apitest.Response
			switch tc.method {
			case http.MethodGet:
				resp = c.GET(tc.target)
			case http.MethodDelete:
				resp = c.DELETE(tc.target)
			default:
				resp = c.POST(t, tc.target, nil)
			}

			resp.MustStatus(t, http.StatusServiceUnavailable)
			schema.AssertProblem(t, tc.op, http.StatusServiceUnavailable, resp.Body())
			if resp.Header("Retry-After") == "" {
				t.Fatal("a 503 with no Retry-After tells a client to guess when to come back")
			}
		})
	}
}

// TestAnUnauthenticatedCallerIsRefusedBeforeAnythingIsRead. Every one of these
// endpoints resolves its tenant from a principal, and there is no other
// sanctioned path to a db.TenantScope — so no principal is a 401 and never an
// empty page.
func TestAnUnauthenticatedCallerIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()

	resp := newContractStack().client().Anonymous().
		GET("/sources").
		MustStatus(t, http.StatusUnauthorized)
	schema.AssertProblem(t, "listSources", http.StatusUnauthorized, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* Divergences between the handlers and the contract                          */
/* -------------------------------------------------------------------------- */

// ⚠️ TestBUG_UnknownQueryParameterOnIdAddressedReadsIsUndeclared.
//
// WHAT THE SERVER DOES. `subject()` runs `httpx.NewParams(r).Err()` for every
// operation addressed by `{id}`, so `GET /api/v1/sources/{id}?foo=1` is refused
// with `400 unknown_parameter`. That behaviour is right and is §E.3: a silently
// ignored parameter is how the UI and the API stop agreeing.
//
// WHAT THE CONTRACT SAYS. `getSource`, `deleteSource`, `testSource`,
// `rotateSourceIngestToken`, `reconcileSource` and `getSourceHealth` declare no
// `400` at all — only 401/403/404/409/429/500/502/503/504 between them. A
// generated client for any of these has no branch for the status they can
// actually return, and a strict contract test would call the 400 a violation.
//
// WHICH IS WRONG: THE CONTRACT. The refusal is the documented policy; the
// response list is simply missing an entry that six sibling operations
// (`listSources`, `updateSource`, `updateCluster`, `listClusters`,
// `getStatsOverview`, `getAlertQualityStats`) all declare. The fix is to add
// `'400': $ref: '#/components/responses/BadRequest'` to those six operations,
// not to start ignoring parameters.
func TestBUG_UnknownQueryParameterOnIdAddressedReadsIsUndeclared(t *testing.T) {
	t.Skip("live conformance defect: the contract declares no 400 for the id-addressed " +
		"sources operations, although §E.3 requires the server to refuse an unknown parameter")

	for _, op := range []string{"getSource", "deleteSource", "testSource",
		"rotateSourceIngestToken", "reconcileSource", "getSourceHealth"} {
		t.Run(op, func(t *testing.T) {
			if !schema.Op(t, op).Declares(http.StatusBadRequest) {
				t.Fatalf("%s declares no 400, but §E.3 makes one reachable with any unknown "+
					"query parameter", op)
			}
		})
	}

	resp := newContractStack().client().
		GET(sourcePath(contractSourceID, "")+"?foo=1").
		MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "getSource", http.StatusBadRequest, resp.Body())
}
