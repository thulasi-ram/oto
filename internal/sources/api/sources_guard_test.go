package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/netguard"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/service"
)

// ⭐ TestCreateSourceRefusesSSRFTargets is the create-time half of C1.
//
// Before the guard existed, `base_url` was validated by a scheme/host regex and
// nothing else, so any org member could register a source at 169.254.169.254 and
// then read the cloud metadata service out of `POST /sources/{id}/test`, which
// reflects upstream content back to the caller.
func TestCreateSourceRefusesSSRFTargets(t *testing.T) {
	t.Parallel()

	forbidden := []struct{ url, why string }{
		{"http://169.254.169.254", "the cloud instance metadata service"},
		{"http://169.254.169.254:80/latest/meta-data", "metadata with a path"},
		{"http://127.0.0.1:9093", "oto's own loopback surfaces"},
		{"http://[::1]:9093", "loopback over IPv6"},
		{"http://10.0.0.5:9093", "an RFC1918 address"},
		{"http://172.16.0.5:9093", "an RFC1918 address"},
		{"http://192.168.1.5:9093", "an RFC1918 address"},
		{"http://100.100.100.200", "the Alibaba metadata service"},
		{"http://2130706433:9093", "loopback spelled in decimal"},
		{"http://0.0.0.0:9093", "the unspecified address"},
	}
	for _, tc := range forbidden {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			rt, deps := newTestRouter(t)

			rec := doCreate(t, rt, map[string]any{
				"name":       "probe",
				"cluster_id": uuid.New().String(),
				"kind":       "alertmanager",
				"base_url":   tc.url,
			})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; %s is %s", rec.Code, tc.url, tc.why)
			}
			if body := rec.Body.String(); !strings.Contains(body, "base_url") {
				t.Fatalf("the violation should name the field: %s", body)
			}
			// ⛔ NOTHING was written. A refused target must not leave a source row.
			if deps.registry.created != 0 {
				t.Fatalf("a refused source was still created (%d writes)", deps.registry.created)
			}
			if deps.tokens.issued != 0 {
				t.Fatalf("a refused source still minted a token")
			}
		})
	}
}

// TestCreateSourceRefusesSSRFInPrometheusURL proves the second URL field is
// guarded too — it is dialled by the rule-snapshot path, which is no less an
// outbound connection than the Alertmanager one.
func TestCreateSourceRefusesSSRFInPrometheusURL(t *testing.T) {
	t.Parallel()
	rt, _ := newTestRouter(t)

	rec := doCreate(t, rt, map[string]any{
		"name":           "probe",
		"cluster_id":     uuid.New().String(),
		"kind":           "alertmanager",
		"base_url":       "https://am.example.com",
		"prometheus_url": "http://169.254.169.254",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "prometheus_url") {
		t.Fatalf("the violation should name prometheus_url: %s", rec.Body.String())
	}
}

// ⭐ TestCreateSourceIsAtomic is the second half of C2.
//
// The source row and its ingest credential are ONE fact. They used to be
// independent commits, so a failing mint left an `alert_sources` row with no
// token: a source the settings screen shows as configured, whose webhook URL an
// operator has already pasted into `webhook_config`, and which answers 401 to
// every alert forever. Alertmanager never retries a 4xx, so those alerts are gone.
func TestCreateSourceIsAtomic(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)
	deps.tokens.err = errors.New("the identity store is having a bad day")

	rec := doCreate(t, rt, map[string]any{
		"name":       "prod-eu",
		"cluster_id": uuid.New().String(),
		"kind":       "alertmanager",
		"base_url":   "https://am.example.com",
	})
	if rec.Code < 400 {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	if !deps.tx.rolledBack {
		t.Fatal("the unit of work committed despite a failed token mint; the source row would be an orphan")
	}
	if deps.tx.committed {
		t.Fatal("the unit of work committed a source with no ingest credential")
	}
}

// TestCreateSourceCommitsSourceAndTokenTogether is the positive half: on the
// happy path everything happens inside ONE unit of work.
func TestCreateSourceCommitsSourceAndTokenTogether(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)
	rec := doCreate(t, rt, map[string]any{
		"name":       "prod-eu",
		"cluster_id": uuid.New().String(),
		"kind":       "alertmanager",
		"base_url":   "https://am.example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if !deps.tx.committed {
		t.Fatal("the create did not run inside a unit of work")
	}
	if deps.registry.created != 1 || deps.tokens.issued != 1 {
		t.Fatalf("writes = %d source, %d token; want 1 and 1", deps.registry.created, deps.tokens.issued)
	}

	var body struct {
		Data struct {
			IngestToken string `json:"ingest_token"`
			TokenPrefix string `json:"token_prefix"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !strings.HasPrefix(body.Data.IngestToken, "oto_ingest_") {
		t.Fatalf("no usable ingest token in the response: %q", body.Data.IngestToken)
	}
	if len(body.Data.TokenPrefix) != 15 {
		t.Fatalf("token_prefix = %q (%d chars); an ingest prefix is fifteen",
			body.Data.TokenPrefix, len(body.Data.TokenPrefix))
	}
}

// ⛔ TestTLSSkipVerifyIsNotTenantControlled is §M2. Disabling certificate
// verification is a statement about the operator's network, not about one org's
// source, and the request body used to be able to make it.
func TestTLSSkipVerifyIsNotTenantControlled(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRouter(t)
	rec := doCreate(t, rt, map[string]any{
		"name":            "prod-eu",
		"cluster_id":      uuid.New().String(),
		"kind":            "alertmanager",
		"base_url":        "https://am.example.com",
		"tls_skip_verify": true,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tls_skip_verify") {
		t.Fatalf("the violation should name the field: %s", rec.Body.String())
	}

	// The deployment-level switch is what grants it.
	open, _ := newTestRouterWith(t, routerOverrides{allowInsecureTLS: true})
	rec = doCreate(t, open, map[string]any{
		"name":            "prod-eu",
		"cluster_id":      uuid.New().String(),
		"kind":            "alertmanager",
		"base_url":        "https://am.example.com",
		"tls_skip_verify": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("the deployment-level switch did not grant it: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPrivateTargetsAllowedWhenDeploymentOptsIn proves the self-hosted escape
// hatch reaches the create path too — an operator whose Alertmanager really is on
// 10.0.0.0/8 must still be able to configure it.
func TestPrivateTargetsAllowedWhenDeploymentOptsIn(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRouterWith(t, routerOverrides{allowPrivate: true})
	rec := doCreate(t, rt, map[string]any{
		"name":       "in-cluster",
		"cluster_id": uuid.New().String(),
		"kind":       "alertmanager",
		"base_url":   "http://alertmanager.monitoring.svc:9093",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// ⭐ TestRotateTokenMintsBeforeItRevokes is the destructive-rotation regression.
//
// The issuer revoked first and minted second. A mint that failed therefore
// revoked the source's only credential and left nothing behind it — and because
// Alertmanager treats 401 as permanent, the alerts sent afterwards were destroyed
// rather than delayed. A rotation must never leave a source with zero working
// tokens.
func TestRotateTokenMintsBeforeItRevokes(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)
	deps.tokens.err = errors.New("the mint failed")

	id := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/sources/"+id.String()+"/rotate-token", nil)
	rec := serve(rt, req)

	if rec.Code < 400 {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	if deps.tokens.revoked != 0 {
		t.Fatalf("a failed rotation revoked %d token(s); the source is now unreachable", deps.tokens.revoked)
	}
	if deps.tx.committed {
		t.Fatal("a failed rotation committed")
	}
}

// ------------------------------------------------------------------- harness

// stubLookup answers `*.example.com` with a public address and everything else
// with NXDOMAIN, so the create-path tests exercise the deny list rather than
// whatever DNS the machine running them happens to have.
func stubLookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	if strings.HasSuffix(host, ".example.com") {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	if host == "alertmanager.monitoring.svc" {
		return []netip.Addr{netip.MustParseAddr("10.42.0.9")}, nil
	}
	return nil, errors.New("no such host")
}

type routerOverrides struct {
	allowPrivate     bool
	allowInsecureTLS bool
}

type testDeps struct {
	registry *fakeRegistry
	tokens   *fakeTokens
	tx       *fakeTx
}

func newTestRouter(t *testing.T) (*Router, *testDeps) {
	t.Helper()
	return newTestRouterWith(t, routerOverrides{})
}

func newTestRouterWith(t *testing.T, o routerOverrides) (*Router, *testDeps) {
	t.Helper()
	deps := &testDeps{registry: &fakeRegistry{}, tokens: &fakeTokens{}, tx: &fakeTx{}}
	rt := NewRouter(Options{
		Sources:  &fakeReader{},
		Registry: deps.registry,
		Tokens:   deps.tokens,
		Tx:       deps.tx,
		Guard: netguard.New(netguard.Options{
			AllowPrivate: o.allowPrivate,
			Field:        "url",
			// A stub resolver, so these tests assert the RULE rather than the
			// sandbox's DNS. `*.example.com` is a public address; nothing else
			// resolves, which is itself a refusal the guard is entitled to make.
			Lookup: stubLookup,
		}),
		AllowInsecureTLS: o.allowInsecureTLS,
		Clock:            clock.New(),
		BaseURL:          "https://oto.example.com",
	})
	return rt, deps
}

func doCreate(t *testing.T, rt *Router, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sources/", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	return serve(rt, req)
}

// serve mounts the router on a chi tree and runs one request as an authenticated
// org member, which is the least-privileged caller every one of these endpoints
// accepts. That is the point of C1: no special role is needed.
func serve(rt *Router, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	rt.Mount(r)

	ctx := authn.Into(req.Context(), authn.Principal{
		Kind:   authn.KindSession,
		OrgID:  uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		UserID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// fakeTx records whether the unit of work committed or rolled back, which is the
// only thing the atomicity tests need to observe.
type fakeTx struct {
	committed  bool
	rolledBack bool
}

func (f *fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := fn(ctx); err != nil {
		f.rolledBack = true
		return err
	}
	f.committed = true
	return nil
}

type fakeRegistry struct{ created int }

func (f *fakeRegistry) Create(_ context.Context, s db.TenantScope, in domain.SourceDraft) (domain.Source, error) {
	f.created++
	return domain.Source{
		ID: uuid.New(), OrgID: s.OrgID(), ClusterID: in.ClusterID,
		Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL,
		PrometheusURL: in.PrometheusURL, TLSSkipVerify: in.TLSSkipVerify,
		PushEnabled: in.PushEnabled, ReconcileEnabled: in.ReconcileEnabled,
		ReconcileInterval: in.ReconcileInterval,
		CreatedAt:         time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, nil
}

func (f *fakeRegistry) Update(_ context.Context, _ db.TenantScope, _ uuid.UUID, _ domain.SourcePatch) (domain.Source, error) {
	return domain.Source{}, nil
}
func (f *fakeRegistry) SoftDelete(context.Context, db.TenantScope, uuid.UUID) error { return nil }
func (f *fakeRegistry) HealthFor(context.Context, db.TenantScope, []uuid.UUID) (map[uuid.UUID]domain.SourceHealth, error) {
	return map[uuid.UUID]domain.SourceHealth{}, nil
}

// fakeTokens mints a REAL ingest secret, so the shape assertions in these tests
// exercise the same derivation the production path does.
type fakeTokens struct {
	issued  int
	revoked int
	err     error
}

func (f *fakeTokens) IssueIngestToken(context.Context, db.TenantScope, uuid.UUID) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.issued++
	secret := "oto_ingest_" + "Ab3D" + strings.Repeat("k", 39)
	return secret, secret[:15], nil
}

func (f *fakeTokens) RevokeIngestTokens(context.Context, db.TenantScope, uuid.UUID) error {
	f.revoked++
	return nil
}

type fakeReader struct{}

func (f *fakeReader) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Source, error) {
	return domain.Source{ID: id, OrgID: s.OrgID(), Name: "prod", Kind: domain.KindAlertmanager}, nil
}

func (f *fakeReader) List(context.Context, db.TenantScope, domain.SourceFilter, db.Keyset) ([]domain.Source, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

func (f *fakeReader) Health(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.SourceHealth, error) {
	return domain.SourceHealth{SourceID: id, Status: domain.HealthUnknown}, nil
}

// Probe is never reached by these tests: every one of them is refused before any
// upstream is dialled, which is the property under test.
func (f *fakeReader) Probe(context.Context, db.TenantScope, uuid.UUID) (service.ProbeResult, error) {
	return service.ProbeResult{}, errors.New("no upstream in a unit test")
}
