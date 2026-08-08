package integration

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
)

// ⭐ LIVE PROOF OF THE THREE PROVENANCE STATES, END TO END.
//
// The fixtures in `internal/sources/client/alertmanager/testdata` prove oto
// agrees with a string somebody once captured. This proves it agrees with the
// PROGRAM, through the real router, the real probe, the real `source_health`
// round trip and the real DTO — which is where a state can be lost, because the
// difference between `default_applies` and `unknown` is carried by nothing more
// than whether `am_route_timings_at` is NULL.
//
// Bring the two Alertmanagers up and point it at them:
//
//	docker compose up -d --wait alertmanager
//	docker run -d --name oto-am-stock -p 9094:9093 prom/alertmanager:v0.28.1
//	OTO_TEST_ALERTMANAGER_URL=http://<host-ip>:9093 \
//	OTO_TEST_ALERTMANAGER_STOCK_URL=http://<host-ip>:9094 \
//	  go test ./test/integration/ -run TestRouteTimingProvenanceAgainstLiveAlertmanagers -v
//
// It SKIPS rather than fails when the URLs are unset, because `go test ./...`
// must not require somebody's containers to be up.

// timingWire is `RouteTimingDTO` as the client sees it.
type timingWire struct {
	Provenance *string  `json:"provenance"`
	ValueMS    *float64 `json:"value_ms"`
}

// timingsWire is `RouteTimingsDTO`.
type timingsWire struct {
	GroupWait           timingWire `json:"group_wait"`
	GroupInterval       timingWire `json:"group_interval"`
	RepeatInterval      timingWire `json:"repeat_interval"`
	Route               string     `json:"route"`
	ChildRoutes         int        `json:"child_routes"`
	ChildRoutesWithTime int        `json:"child_routes_with_timings"`
	DefaultsFromVersion *string    `json:"defaults_from_version"`
	DefaultsVerified    bool       `json:"defaults_verified"`
	ObservedAt          *string    `json:"observed_at"`
}

func (t timingsWire) fields() map[string]timingWire {
	return map[string]timingWire{
		"group_wait": t.GroupWait, "group_interval": t.GroupInterval,
		"repeat_interval": t.RepeatInterval,
	}
}

// TestRouteTimingProvenanceAgainstLiveAlertmanagers.
func TestRouteTimingProvenanceAgainstLiveAlertmanagers(t *testing.T) {
	configured := os.Getenv("OTO_TEST_ALERTMANAGER_URL")
	stock := os.Getenv("OTO_TEST_ALERTMANAGER_STOCK_URL")
	if configured == "" || stock == "" {
		t.Skip("set OTO_TEST_ALERTMANAGER_URL and OTO_TEST_ALERTMANAGER_STOCK_URL " +
			"to run against live Alertmanagers")
	}

	// The SSRF guard refuses a private target by default, and both containers are
	// on a private address. Opening it is the documented operator opt-in, and it
	// is opened HERE rather than weakened in the guard.
	env := newEnvWith(t, func(c *config.Config) { c.Security.AllowPrivateTargets = true })

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "live", OrgName: "Live", Email: "ops@live.example",
		DisplayName: "Ops", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/clusters", boot.Token,
		map[string]any{"cluster_key": "prod", "display_name": "prod"},
		http.StatusCreated, &cluster)

	// ---- 1. The compose Alertmanager states all three: OBSERVED ---------
	observed := probeAndRead(t, env, boot.Token, cluster.Data.ID, "compose", configured)
	for name, f := range observed.fields() {
		if f.Provenance == nil || *f.Provenance != "observed" {
			t.Fatalf("compose Alertmanager: %s provenance = %v, want observed — "+
				"deploy/alertmanager/alertmanager.yml states all three", name, f.Provenance)
		}
		if f.ValueMS == nil {
			t.Fatalf("compose Alertmanager: %s was observed with no value", name)
		}
	}
	if observed.DefaultsFromVersion != nil {
		t.Fatalf("compose Alertmanager: defaults attributed to %v although nothing defaulted",
			*observed.DefaultsFromVersion)
	}
	if observed.ObservedAt == nil {
		t.Fatal("compose Alertmanager: observed_at is null after a successful probe")
	}

	// ---- 2. A stock Alertmanager states none: DEFAULT_APPLIES -----------
	//
	// ⛔ THIS IS THE CASE THE FEATURE EXISTS FOR, and the one oto used to render
	// as "unknown". The container is running the image's own default config,
	// which sets none of the three; Alertmanager applies 30s / 5m / 4h in
	// `dispatch.NewRoute`, where the status endpoint cannot see them.
	defaulted := probeAndRead(t, env, boot.Token, cluster.Data.ID, "stock", stock)
	want := map[string]float64{
		"group_wait": 30_000, "group_interval": 300_000, "repeat_interval": 14_400_000,
	}
	for name, f := range defaulted.fields() {
		if f.Provenance == nil || *f.Provenance != "default_applies" {
			t.Fatalf("stock Alertmanager: %s provenance = %v, want default_applies. "+
				"A stock install is the commonest deployment there is, and reporting it as "+
				"unknown makes every verdict on the tuning screen inert", name, f.Provenance)
		}
		if f.ValueMS == nil || *f.ValueMS != want[name] {
			t.Fatalf("stock Alertmanager: %s = %v, want %v ms", name, f.ValueMS, want[name])
		}
	}
	if defaulted.DefaultsFromVersion == nil {
		t.Fatal("stock Alertmanager: the defaults are attributed to no version, so nothing " +
			"tells a reader whose default is being quoted")
	}
	if defaulted.ObservedAt == nil {
		t.Fatal("stock Alertmanager: observed_at is null although oto DID read the config; " +
			"that null is the only thing separating default_applies from unknown")
	}
	t.Logf("stock Alertmanager: defaults attributed to %s (verified=%v)",
		*defaulted.DefaultsFromVersion, defaulted.DefaultsVerified)

	// ---- 3. A source nobody can reach: UNKNOWN --------------------------
	unreachable := probeAndRead(t, env, boot.Token, cluster.Data.ID, "dead",
		"http://192.0.2.1:9093")
	for name, f := range unreachable.fields() {
		if f.Provenance == nil || *f.Provenance != "unknown" {
			t.Fatalf("unreachable source: %s provenance = %v, want unknown", name, f.Provenance)
		}
		if f.ValueMS != nil {
			t.Fatalf("unreachable source: %s carries %v; oto has never read this config and "+
				"must not supply a default for one it cannot prove exists", name, *f.ValueMS)
		}
	}
	if unreachable.ObservedAt != nil {
		t.Fatalf("unreachable source: observed_at = %v", *unreachable.ObservedAt)
	}
}

// probeAndRead registers a source, probes it and returns the timings the SOURCES
// LIST serves — the exact bytes the settings screen reads.
func probeAndRead(t *testing.T, e *env, token, clusterID, name, baseURL string) timingsWire {
	t.Helper()

	var created struct {
		Data struct {
			Source struct {
				ID string `json:"id"`
			} `json:"source"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/sources", token, map[string]any{
		"name": name, "cluster_id": clusterID, "kind": "alertmanager", "base_url": baseURL,
	}, http.StatusCreated, &created)

	id := created.Data.Source.ID
	// A probe of a dead source answers 200 with `ok:false` — the probe SUCCEEDED
	// in discovering the source is down — so the status is 200 either way.
	e.do(t, http.MethodPost, "/api/v1/sources/"+id+"/test", token, nil, http.StatusOK, nil)

	var health struct {
		Data struct {
			RouteTimings *timingsWire `json:"route_timings"`
		} `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/sources/"+id+"/health", token, nil, http.StatusOK, &health)
	if health.Data.RouteTimings == nil {
		t.Fatalf("%s: route_timings is absent; it is required and always present, because "+
			"`unknown` is a state the object carries rather than a reason to omit it", name)
	}
	if health.Data.RouteTimings.Route != "top_level" {
		t.Fatalf("%s: route = %q, want top_level", name, health.Data.RouteTimings.Route)
	}
	return *health.Data.RouteTimings
}
