package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/migrate"
)

// ⭐ THIS FILE IS THE PROOF THAT A FRESH INSTALL WORKS.
//
// Two defects met here and made the product unusable out of the box:
//
//  1. `POST /api/v1/sources` returned 422 for EVERY request ever sent to it,
//     because the ingest token's prefix was sliced at a fixed twelve characters
//     and `oto_ingest_` is eleven — so the derived prefix carried one random
//     character where api_tokens_prefix_ck wants four. Nothing could configure a
//     source, which is the product's primary path.
//  2. There was no bootstrap of any kind: no org-creation API, no signup, so
//     `POST /auth/login` had no row to authenticate against. Every test of this
//     system so far seeded Postgres with hand-written SQL.
//
// So this test does the thing a user does, against a real Postgres, with no SQL
// of its own: bootstrap → create a cluster → create a source → take the ingest
// token out of the response → fire a webhook at it → 202.

func TestFreshInstallCanConfigureASourceAndReceiveAWebhook(t *testing.T) {
	env := newEnv(t)

	// 1. The install path. This is `oto bootstrap`, which is the only way a
	//    migrated database acquires a credential that can reach the API.
	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug:     "acme",
		OrgName:     "Acme",
		Email:       "ops@acme.example",
		DisplayName: "Ops",
		Password:    "correct-horse-battery-staple",
		TokenName:   "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !strings.HasPrefix(boot.Token, "oto_pat_") {
		t.Fatalf("bootstrap token is not a PAT: %q", boot.Token)
	}
	if len(boot.TokenPrefix) != 12 {
		t.Fatalf("a PAT prefix is twelve characters, got %q", boot.TokenPrefix)
	}

	// Bootstrap refuses to run twice. Idempotence here would mean either silently
	// doing nothing or resetting a password, and the second is a takeover.
	if _, rerr := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "acme2", Email: "other@acme.example", Password: "another-long-password",
	}, time.Now()); rerr == nil {
		t.Fatal("bootstrap ran a second time")
	}

	// 2. The bootstrap token authenticates.
	var me struct {
		Data struct {
			Org struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			} `json:"org"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/me", boot.Token, nil, http.StatusOK, &me)
	if me.Data.Org.Slug != "acme" {
		t.Fatalf("GET /me resolved org %q, want acme", me.Data.Org.Slug)
	}

	// 3. A cluster, then ⭐ THE SOURCE — the request that used to be a guaranteed
	//    422 on every install of this product.
	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key":  "prod-eu",
		"display_name": "Production EU",
	}, http.StatusCreated, &cluster)

	var created struct {
		Data struct {
			Source struct {
				ID         string `json:"id"`
				IngestPath string `json:"ingest_path"`
			} `json:"source"`
			IngestToken string `json:"ingest_token"`
			TokenPrefix string `json:"token_prefix"`
			WebhookURL  string `json:"webhook_url"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name":       "am-eu-1",
		"cluster_id": cluster.Data.ID,
		"kind":       "alertmanager",
		"base_url":   "https://alertmanager.acme.example",
	}, http.StatusCreated, &created)

	if !strings.HasPrefix(created.Data.IngestToken, "oto_ingest_") {
		t.Fatalf("no usable ingest token: %q", created.Data.IngestToken)
	}
	// FIFTEEN, not twelve. This single number is the whole of the first defect.
	if len(created.Data.TokenPrefix) != 15 {
		t.Fatalf("token_prefix = %q (%d chars); `oto_ingest_` + 4 is fifteen",
			created.Data.TokenPrefix, len(created.Data.TokenPrefix))
	}

	// 4. ⭐ THE TOKEN THE API HANDED BACK ACTUALLY WORKS. This is the assertion
	//    that makes the whole path real rather than merely non-erroring.
	var accepted struct {
		Data struct {
			BatchID    string `json:"batch_id"`
			AlertCount int    `json:"alert_count"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, created.Data.Source.IngestPath, created.Data.IngestToken,
		alertmanagerWebhook("HighErrorRate"), http.StatusAccepted, &accepted)
	if accepted.Data.AlertCount != 1 {
		t.Fatalf("alert_count = %d, want 1", accepted.Data.AlertCount)
	}

	// 5. The credential is real in the other direction too: a wrong token is 401,
	//    so the 202 above was authentication and not an open door.
	env.do(t, http.MethodPost, created.Data.Source.IngestPath,
		"oto_ingest_"+strings.Repeat("z", 43), alertmanagerWebhook("Other"),
		http.StatusUnauthorized, nil)

	// 6. And the source row is not an orphan: exactly one live ingest token exists
	//    for it, which is what "the source and its credential are one fact" means.
	if n := env.countLiveIngestTokens(t, created.Data.Source.ID); n != 1 {
		t.Fatalf("live ingest tokens for the source = %d, want 1", n)
	}
}

// ⭐ TestRotateTokenNeverLeavesASourceWithoutACredential is the destructive
// rotation, against a real database.
//
// The issuer revoked first and minted second. One probe of this endpoint against
// the prefix bug therefore revoked every live ingest token in the org and left
// nothing behind — and Alertmanager never retries a 401, so the alerts sent
// afterwards were destroyed rather than delayed.
func TestRotateTokenNeverLeavesASourceWithoutACredential(t *testing.T) {
	env := newEnv(t)

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "rotate", Email: "ops@rotate.example", Password: "correct-horse-battery-staple",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key": "prod", "display_name": "Production",
	}, http.StatusCreated, &cluster)

	var created struct {
		Data struct {
			Source struct {
				ID         string `json:"id"`
				IngestPath string `json:"ingest_path"`
			} `json:"source"`
			IngestToken string `json:"ingest_token"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "am-1", "cluster_id": cluster.Data.ID,
		"kind": "alertmanager", "base_url": "https://am.rotate.example",
	}, http.StatusCreated, &created)

	var rotated struct {
		Data struct {
			IngestToken string `json:"ingest_token"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/sources/"+created.Data.Source.ID+"/rotate-token",
		boot.Token, nil, http.StatusOK, &rotated)

	if rotated.Data.IngestToken == created.Data.IngestToken {
		t.Fatal("rotation returned the same secret")
	}
	// Exactly one live token: the new one replaced the old, and did not join it.
	if n := env.countLiveIngestTokens(t, created.Data.Source.ID); n != 1 {
		t.Fatalf("live ingest tokens after rotation = %d, want exactly 1", n)
	}
	// The new one works...
	env.do(t, http.MethodPost, created.Data.Source.IngestPath, rotated.Data.IngestToken,
		alertmanagerWebhook("AfterRotation"), http.StatusAccepted, nil)
	// ...and the old one does not. Rotation revokes, it does not accumulate.
	env.do(t, http.MethodPost, created.Data.Source.IngestPath, created.Data.IngestToken,
		alertmanagerWebhook("BeforeRotation"), http.StatusUnauthorized, nil)
}

// ⭐ TestSourceCreationRefusesSSRFTargetsEndToEnd is C1 through the real stack: an
// ordinary org member, the real router, the real validation.
func TestSourceCreationRefusesSSRFTargetsEndToEnd(t *testing.T) {
	env := newEnv(t)

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "ssrf", Email: "ops@ssrf.example", Password: "correct-horse-battery-staple",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key": "prod", "display_name": "Production",
	}, http.StatusCreated, &cluster)

	for _, target := range []string{
		"http://169.254.169.254",
		"http://127.0.0.1:9093",
		"http://10.0.0.5:9093",
		"http://192.168.1.5:9093",
		"http://172.16.0.5:9093",
		"http://100.100.100.200",
		"http://[::1]:9093",
		"http://2130706433:9093",
	} {
		env.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
			"name": "probe", "cluster_id": cluster.Data.ID,
			"kind": "alertmanager", "base_url": target,
		}, http.StatusUnprocessableEntity, nil)
	}

	// ⛔ And nothing was written. A refused target must not leave a source row
	// behind, or the guard has merely moved the problem to `PATCH`.
	var sources struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/sources", boot.Token, nil, http.StatusOK, &sources)
	if len(sources.Data) != 0 {
		t.Fatalf("%d source rows survived a refused create", len(sources.Data))
	}

	// The deployment-level TLS switch is off, so a tenant cannot disable
	// certificate verification either (§M2).
	env.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "insecure", "cluster_id": cluster.Data.ID, "kind": "alertmanager",
		"base_url": "https://am.ssrf.example", "tls_skip_verify": true,
	}, http.StatusUnprocessableEntity, nil)
}

// TestLoginIsRateLimited is the HIGH the audit called both an auth and a DoS
// control: argon2id at 19 MiB runs on every path, including for addresses that do
// not exist, so an unbounded login endpoint is a memory amplifier.
func TestLoginIsRateLimited(t *testing.T) {
	env := newEnv(t)

	if _, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "rl", Email: "ops@rl.example", Password: "correct-horse-battery-staple",
	}, time.Now()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	limited := false
	for range env.cfg.Security.LoginRateBurst + 3 {
		status := env.status(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
			"email": "nobody@rl.example", "password": "wrong-password-entirely",
		})
		if status == http.StatusTooManyRequests {
			limited = true
			break
		}
		if status != http.StatusUnauthorized {
			t.Fatalf("unexpected login status %d", status)
		}
	}
	if !limited {
		t.Fatal("the login endpoint never answered 429; it is an unbounded argon2id amplifier")
	}

	// The real credential still works from a fresh address, because the limiter is
	// keyed per client and a successful login clears its bucket.
	env.limiterReset()
	if s := env.status(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "ops@rl.example", "password": "correct-horse-battery-staple",
	}); s != http.StatusOK {
		t.Fatalf("a correct login after the limit cleared returned %d", s)
	}
}

// ------------------------------------------------------------------ harness

type env struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	server    *httptest.Server
	container *app.Container
	cfg       config.Config
}

// newEnv brings up a real Postgres, migrates it, and wires the real container
// and the real route table over it. Nothing here writes SQL of its own — that is
// the point.
func newEnv(t *testing.T) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test needs Docker")
	}

	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("oto"),
		tcpostgres.WithUsername("oto"),
		tcpostgres.WithPassword("oto"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		t.Skipf("could not start Postgres (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if err := migrate.Up(ctx, dsn); err != nil {
		t.Fatalf("goose: %v", err)
	}
	if err := riverMigrate(ctx, dsn); err != nil {
		t.Fatalf("river: %v", err)
	}

	cfg := config.Default()
	cfg.DB.URL = dsn
	cfg.HTTP.BaseURL = "http://oto.test"
	cfg.Telemetry.MetricsEnabled = false
	// Jobs are ENQUEUED but not worked: the ingest path must enqueue to answer
	// 202, and a worker pool would make this test about River rather than about
	// the accept contract.
	cfg.Jobs.Enabled = true

	pools, err := db.Open(ctx, cfg.DB)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}

	c, err := app.New(ctx, app.Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pools:  pools,
	})
	if err != nil {
		t.Fatalf("container: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	srv := httptest.NewServer(c.Router())
	t.Cleanup(srv.Close)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return &env{ctx: ctx, pool: pool, server: srv, container: c, cfg: cfg}
}

// do performs one request and asserts the status, decoding into out when given.
func (e *env) do(t *testing.T, method, path, token string, body any, wantStatus int, out any) {
	t.Helper()
	resp, raw := e.request(t, method, path, token, body)
	if resp != wantStatus {
		t.Fatalf("%s %s → %d, want %d: %s", method, path, resp, wantStatus, raw)
	}
	if out == nil || len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: decode: %v (%s)", method, path, err, raw)
	}
}

func (e *env) status(t *testing.T, method, path, token string, body any) int {
	t.Helper()
	status, _ := e.request(t, method, path, token, body)
	return status
}

func (e *env) request(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.server.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// countLiveIngestTokens is the one read this file makes that the API cannot
// answer: "how many credentials could actually authenticate for this source".
func (e *env) countLiveIngestTokens(t *testing.T, sourceID string) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM api_tokens
		  WHERE kind = 'ingest' AND source_id = $1 AND revoked_at IS NULL`, sourceID).Scan(&n)
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

// limiterReset clears the login limiter, standing in for "a different client
// address" — httptest always connects from loopback, so a real second address is
// not available inside one process.
func (e *env) limiterReset() {
	if e.container.LoginLimiter != nil {
		e.container.LoginLimiter.Reset("127.0.0.1")
		e.container.LoginLimiter.Reset("::1")
	}
}

// alertmanagerWebhook is a minimal, valid Alertmanager v4 payload.
func alertmanagerWebhook(alertname string) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"version":           "4",
		"groupKey":          fmt.Sprintf(`{}:{alertname="%s"}`, alertname),
		"truncatedAlerts":   0,
		"status":            "firing",
		"receiver":          "oto",
		"groupLabels":       map[string]string{"alertname": alertname},
		"commonLabels":      map[string]string{"alertname": alertname, "severity": "critical"},
		"commonAnnotations": map[string]string{},
		"externalURL":       "https://alertmanager.example.com",
		"alerts": []map[string]any{{
			"status":       "firing",
			"labels":       map[string]string{"alertname": alertname, "severity": "critical", "instance": "web-1"},
			"annotations":  map[string]string{"summary": "error rate is high"},
			"startsAt":     now,
			"endsAt":       "0001-01-01T00:00:00Z",
			"generatorURL": "https://prometheus.example.com/graph?g0.expr=up",
			"fingerprint":  "0123456789abcdef",
		}},
	}
}

func riverMigrate(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
