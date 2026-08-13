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
			// ⛔ NOTHING was written. The refusal lands BEFORE the write facade is
			// called at all, so a refused target cannot leave a source row and
			// cannot mint a token, whatever the facade would otherwise have done.
			if deps.writes.created != 0 {
				t.Fatalf("a refused source still reached the write path (%d calls)", deps.writes.created)
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

// TestCreateSourceReturnsTheSecretTheFacadeMinted.
//
// ⭐ WHAT THIS LAYER OWES THE CALLER is that the plaintext ingest token comes back
// on the 201 and comes back in a shape a client can use — it is returned exactly
// once, and a response that dropped or mangled it costs the operator the only copy
// there will ever be. THAT THE SOURCE ROW AND THE TOKEN COMMIT TOGETHER is a
// different promise, made where the transaction is:
// `internal/sources/service/write_test.go`.
func TestCreateSourceReturnsTheSecretTheFacadeMinted(t *testing.T) {
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
	if deps.writes.created != 1 {
		t.Fatalf("the write facade saw %d creates, want 1", deps.writes.created)
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

// TestRotateTokenSurfacesAFailedRotationAsAFailure.
//
// The destructive half of this — that a rotation mints before it revokes, and
// that a failed mint leaves the source's existing token alone — is asserted where
// the transaction is (`internal/sources/service/write_test.go`). What must hold
// HERE is that the handler does not render a 200 with an empty `ingest_token`
// over a rotation that did not happen, which would tell an operator to go and
// reconfigure Alertmanager with nothing.
func TestRotateTokenSurfacesAFailedRotationAsAFailure(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)
	deps.writes.rotateErr = errors.New("the mint failed")

	id := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/sources/"+id.String()+"/rotate-token", nil)
	rec := serve(rt, req)

	if rec.Code < 400 {
		t.Fatalf("status = %d, want a failure: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ingest_token") {
		t.Fatalf("a failed rotation still rendered a token field: %s", rec.Body.String())
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
	writes *fakeWrites
}

func newTestRouter(t *testing.T) (*Router, *testDeps) {
	t.Helper()
	return newTestRouterWith(t, routerOverrides{})
}

func newTestRouterWith(t *testing.T, o routerOverrides) (*Router, *testDeps) {
	t.Helper()
	deps := &testDeps{writes: &fakeWrites{}}
	rt := NewRouter(Options{
		Sources:  &fakeReader{},
		Registry: deps.writes,
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

// fakeWrites is the WRITE FACADE these tests stand in front of.
//
// ⭐ IT IS ONE FAKE, AND THAT IS THE SHAPE OF THE THING NOW. The transaction, the
// credential sealing, the ingest-token mint and the `Idempotency-Key` claim live
// behind these methods (`sources/service`), so the questions this file can still
// ask are the transport's own: was the write path reached at all, and with what.
// It mints a REAL-SHAPED ingest secret so the response assertions exercise the
// same fifteen-character prefix a client will actually receive.
type fakeWrites struct {
	created int
	updated int
	deleted int
	rotated int
	// The commands the handler built, so a test can assert that what the caller
	// sent survived binding.
	lastCreate service.CreateCommand
	lastUpdate service.UpdateCommand

	rotateErr error
}

func (f *fakeWrites) Create(
	_ context.Context, s db.TenantScope, cmd service.CreateCommand,
) (service.IssuedIngest, error) {
	f.created++
	f.lastCreate = cmd
	in := cmd.Draft
	secret := "oto_ingest_" + "Ab3D" + strings.Repeat("k", 39)
	return service.IssuedIngest{
		Source: domain.Source{
			ID: uuid.New(), OrgID: s.OrgID(), ClusterID: in.ClusterID,
			Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL,
			PrometheusURL: in.PrometheusURL, TLSSkipVerify: in.TLSSkipVerify,
			PushEnabled:       in.PushEnabled,
			ReconcileInterval: in.ReconcileInterval,
			CreatedAt:         time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		},
		Secret: secret,
		Prefix: secret[:15],
	}, nil
}

func (f *fakeWrites) Update(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, cmd service.UpdateCommand,
) (domain.Source, error) {
	f.updated++
	f.lastUpdate = cmd
	return domain.Source{}, nil
}

func (f *fakeWrites) SoftDelete(context.Context, db.TenantScope, uuid.UUID) error {
	f.deleted++
	return nil
}

func (f *fakeWrites) RotateIngestToken(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _ service.Idempotency,
) (service.IssuedIngest, error) {
	if f.rotateErr != nil {
		return service.IssuedIngest{}, f.rotateErr
	}
	f.rotated++
	secret := "oto_ingest_" + "Ab3D" + strings.Repeat("k", 39)
	return service.IssuedIngest{
		Source: domain.Source{ID: id, Kind: domain.KindAlertmanager},
		Secret: secret,
		Prefix: secret[:15],
	}, nil
}

func (f *fakeWrites) HealthFor(context.Context, db.TenantScope, []uuid.UUID) (map[uuid.UUID]domain.SourceHealth, error) {
	return map[uuid.UUID]domain.SourceHealth{}, nil
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
