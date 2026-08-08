package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/migrate"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/repository"
)

// ⭐ THIS FILE IS THE PROOF THAT 00028 ROUND-TRIPS FROM A FRESH DATABASE.
//
// The container is created, `goose up` runs every migration from nothing, and the
// real repository writes and re-reads the three Alertmanager timings through the
// real SQL. A unit test over the mapper would agree with whatever the mapper does
// and would not notice a column name that never made it into the INSERT, a CHECK
// that rejects a legal value, or a NULL that comes back as zero — which is the
// one failure mode that matters here, because zero and unknown mean opposite
// things for these three.

// seedSource writes the minimum an org/cluster/source needs for a health row.
func seedSource(t *testing.T, e *env) (db.TenantScope, *repository.SourceRepository, domain.SourceHealth) {
	t.Helper()

	orgID, clusterID, sourceID := id.New(), id.New(), id.New()
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		orgID, "t"+orgID.String()[:8], "timings org"); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO clusters (id, org_id, cluster_key, display_name) VALUES ($1, $2, $3, $4)`,
		clusterID, orgID, "prod", "prod"); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url)
		 VALUES ($1, $2, $3, $4, 'alertmanager', 'http://am.test')`,
		sourceID, orgID, clusterID, "am-"+sourceID.String()[:8]); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return scope, repository.NewSourceRepository(e.pool, e.container.Clock), domain.SourceHealth{
		SourceID: sourceID,
		OrgID:    orgID,
		Status:   domain.HealthHealthy,
	}
}

func dur(d time.Duration) *time.Duration { return &d }

// TestRouteTimingsRoundTripFromAFreshDatabase.
func TestRouteTimingsRoundTripFromAFreshDatabase(t *testing.T) {
	env := newEnv(t)
	scope, repo, health := seedSource(t, env)

	// 1. A source nobody has probed: every timing unknown, and the observed-at
	//    NULL. This is the state 00028 leaves every existing row in.
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save an unprobed source: %v", err)
	}
	got, err := repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.RouteTimingsAt != nil {
		t.Fatalf("observed_at = %v on a source nobody has probed", got.RouteTimingsAt)
	}
	if got.RouteTimings.Known() {
		t.Fatalf("timings %+v on a source nobody has probed", got.RouteTimings)
	}

	// 2. A probe that read the config. All three observed, plus the per-route
	//    caveat count.
	observedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health.RouteTimings = domain.RouteTimings{
		GroupWait:           dur(10 * time.Second),
		GroupInterval:       dur(30 * time.Second),
		RepeatInterval:      dur(4 * time.Hour),
		ChildRoutes:         4,
		ChildrenWithTimings: 2,
	}
	health.RouteTimingsAt = &observedAt
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save observed timings: %v", err)
	}

	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.GroupWait == nil || *got.RouteTimings.GroupWait != 10*time.Second {
		t.Fatalf("group_wait = %v", got.RouteTimings.GroupWait)
	}
	if got.RouteTimings.GroupInterval == nil || *got.RouteTimings.GroupInterval != 30*time.Second {
		t.Fatalf("group_interval = %v", got.RouteTimings.GroupInterval)
	}
	if got.RouteTimings.RepeatInterval == nil || *got.RouteTimings.RepeatInterval != 4*time.Hour {
		t.Fatalf("repeat_interval = %v", got.RouteTimings.RepeatInterval)
	}
	if got.RouteTimings.ChildRoutes != 4 || got.RouteTimings.ChildrenWithTimings != 2 {
		t.Fatalf("child routes %d/%d, want 2/4",
			got.RouteTimings.ChildrenWithTimings, got.RouteTimings.ChildRoutes)
	}
	if got.RouteTimingsAt == nil || !got.RouteTimingsAt.Equal(observedAt) {
		t.Fatalf("observed_at = %v, want %v", got.RouteTimingsAt, observedAt)
	}

	// 3. ⛔ THE ONE THAT WOULD LIE. A source whose config states NONE of the three
	//    is read back with three NULLs, not three zeros. Zero means `group_wait:
	//    0s` — "notify immediately" — and is a completely different statement about
	//    the upstream from "oto has not been told".
	health.RouteTimings = domain.RouteTimings{ChildRoutes: 1, ChildrenWithTimings: 1}
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save an unstated config: %v", err)
	}
	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.Known() {
		t.Fatalf("a config stating none of the three came back as %+v; NULL must stay NULL, "+
			"because 0 is a real Alertmanager setting", got.RouteTimings)
	}
	if got.RouteTimingsAt == nil {
		t.Fatal("observed_at was cleared: oto DID read this config and found nothing stated, " +
			"which is an observation")
	}

	// 4. A zero timing is a value, and survives as one.
	health.RouteTimings = domain.RouteTimings{GroupWait: dur(0)}
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save a zero group_wait: %v", err)
	}
	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.GroupWait == nil {
		t.Fatal("group_wait: 0s round-tripped as unknown")
	}
	if *got.RouteTimings.GroupWait != 0 {
		t.Fatalf("group_wait = %v, want 0", *got.RouteTimings.GroupWait)
	}
}

// TestTheHealthListReadsTheTimingsToo. The sources screen renders health for a
// page of sources through HealthFor, which has its own column list; a column
// added to one query and not the other is a field that is present on the detail
// page and mysteriously absent on the list.
func TestTheHealthListReadsTheTimingsToo(t *testing.T) {
	env := newEnv(t)
	scope, repo, health := seedSource(t, env)

	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health.RouteTimings = domain.RouteTimings{GroupInterval: dur(5 * time.Minute)}
	health.RouteTimingsAt = &at
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save: %v", err)
	}

	byID, err := repo.HealthFor(env.ctx, scope, []uuid.UUID{health.SourceID})
	if err != nil {
		t.Fatalf("list health: %v", err)
	}
	got, ok := byID[health.SourceID]
	if !ok {
		t.Fatal("the source is absent from the list read")
	}
	if got.RouteTimings.GroupInterval == nil || *got.RouteTimings.GroupInterval != 5*time.Minute {
		t.Fatalf("group_interval = %v on the list read", got.RouteTimings.GroupInterval)
	}
	if got.RouteTimingsAt == nil || !got.RouteTimingsAt.Equal(at) {
		t.Fatalf("observed_at = %v on the list read", got.RouteTimingsAt)
	}
}

// TestTheTopTwoMigrationsAreReversible. Expand/contract (CONTEXT.md §6) is only a
// property if the contract half actually runs: a migration nobody has rolled back
// is a migration nobody can deploy on a Friday.
//
// It rolls back BOTH of the top two — 00029's delivery-roll-up index and 00028's
// timing columns — because `migrate.Down` reverts exactly one, and a test that
// pinned the count would have silently stopped testing 00028 the day 00029
// landed.
func TestTheTopTwoMigrationsAreReversible(t *testing.T) {
	env := newEnv(t)
	dsn := env.cfg.DB.URL

	latest, err := migrate.Latest()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest != 29 {
		t.Fatalf("latest migration is %d, want 29 — this test pins the number so that a "+
			"second migration claiming the same version is caught here", latest)
	}

	// 00029 down: the partial index behind the occurrence delivery roll-up goes.
	if err := migrate.Down(env.ctx, dsn); err != nil {
		t.Fatalf("goose down 00029: %v", err)
	}
	var indexes int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'notif_occurrence_idx'`).Scan(&indexes); err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}
	if indexes != 0 {
		t.Fatal("notif_occurrence_idx survived the down migration")
	}

	// 00028 down: the six route-timing columns go with it.
	if err := migrate.Down(env.ctx, dsn); err != nil {
		t.Fatalf("goose down 00028: %v", err)
	}
	var n int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'source_health' AND column_name LIKE 'am_%route%'`).Scan(&n); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d route-timing columns survived the down migration", n)
	}

	if err := migrate.Up(env.ctx, dsn); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'notif_occurrence_idx'`).Scan(&indexes); err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}
	if indexes != 1 {
		t.Fatal("notif_occurrence_idx did not come back on the way up")
	}
}

// TestDeclarativeTuningOverTheWire is the contract the UI is about to be built
// against, asserted end to end: real config loading, real container, real router,
// real Postgres.
//
// ⭐ THE THREE THINGS A UI NEEDS AND CANNOT INVENT: the origin says `config`, the
// config key says WHERE, and the shadowed override is still there to show beside
// the value in force. Plus the 409, which is what stops the screen from offering
// an edit that would silently revert on the next deploy.
func TestDeclarativeTuningOverTheWire(t *testing.T) {
	t.Setenv("OTO_TUNING_REFIRE_GRACE_S", "600")

	env := newEnvWith(t, func(c *config.Config) {
		loaded, err := config.Load("")
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		// Keep the container's own plumbing; take only the tuning layer.
		loaded.DB, loaded.HTTP, loaded.Telemetry, loaded.Jobs = c.DB, c.HTTP, c.Telemetry, c.Jobs
		*c = loaded
	})

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "acme", OrgName: "Acme", Email: "ops@acme.example",
		DisplayName: "Ops", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The org already had an override of 900 — written before the deployment
	// started stating a value, which is the whole scenario.
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE orgs SET settings = '{"refire_grace_s": 900}'::jsonb WHERE slug = 'acme'`); err != nil {
		t.Fatalf("seed the override: %v", err)
	}

	var view struct {
		Data struct {
			Settings   map[string]any    `json:"settings"`
			Origins    map[string]string `json:"origins"`
			ConfigKeys map[string]string `json:"config_keys"`
			Shadowed   map[string]any    `json:"shadowed"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/org/settings", boot.Token, nil, http.StatusOK, &view)

	if got := view.Data.Settings["refire_grace_s"]; got != float64(600) {
		t.Fatalf("effective refire_grace_s = %v, want 600: configuration must beat the override", got)
	}
	if got := view.Data.Origins["refire_grace_s"]; got != "config" {
		t.Fatalf("origin %q, want config", got)
	}
	if got := view.Data.ConfigKeys["refire_grace_s"]; got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config_keys[refire_grace_s] = %q, want the env var an operator can edit", got)
	}
	if got := view.Data.Shadowed["refire_grace_s"]; got != float64(900) {
		t.Fatalf("shadowed = %v, want the org's own 900 — hiding it is how somebody spends "+
			"an afternoon on a number they can see in the database and never in force", got)
	}
	// A key the deployment does not manage is not shadowed and carries no config key.
	if _, present := view.Data.Shadowed["storm_threshold"]; present {
		t.Fatal("an unmanaged key was reported as shadowed")
	}
	if _, present := view.Data.ConfigKeys["storm_threshold"]; present {
		t.Fatal("an unmanaged key was given a config key")
	}

	// Writing the tuning is SESSION-ONLY (a leaked token must not be able to make
	// oto go quiet), so the write half of this test logs in.
	session := login(t, env, "ops@acme.example", "correct-horse-battery-staple")

	// ⛔ The write is REFUSED, and the refusal names the key to edit.
	status, raw := session.patch(t, map[string]any{"refire_grace_s": 1200})
	if status != http.StatusConflict {
		t.Fatalf("PATCH on a config-managed key → %d, want 409: %s", status, raw)
	}
	if !strings.Contains(string(raw), "OTO_TUNING_REFIRE_GRACE_S") {
		t.Fatalf("the 409 does not name the config key: %s", raw)
	}

	// A key the deployment does not manage is still writable, and the write does
	// not drop the declarative overlay from the response.
	status, raw = session.patch(t, map[string]any{"storm_threshold": 40})
	if status != http.StatusOK {
		t.Fatalf("PATCH on an unmanaged key → %d: %s", status, raw)
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := view.Data.Settings["storm_threshold"]; got != float64(40) {
		t.Fatalf("storm_threshold = %v after a legal write", got)
	}
	if got := view.Data.Settings["refire_grace_s"]; got != float64(600) {
		t.Fatalf("the write dropped the declarative overlay: refire_grace_s = %v", got)
	}
	if got := view.Data.Origins["storm_threshold"]; got != "org" {
		t.Fatalf("origin of a freshly written unmanaged key = %q, want org", got)
	}
}

// sessionEnv is a cookie-carrying client for the session-only settings write.
type sessionEnv struct {
	base   string
	client *http.Client
}

// login exchanges a password for a session cookie, which is the only credential
// `PATCH /api/v1/org/settings` accepts.
func login(t *testing.T, e *env, email, password string) sessionEnv {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	body, err := json.Marshal(map[string]any{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.server.URL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login → %d: %s", resp.StatusCode, raw)
	}
	return sessionEnv{base: e.server.URL, client: client}
}

func (s sessionEnv) patch(t *testing.T, body any) (int, []byte) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch,
		s.base+"/api/v1/org/settings", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}
