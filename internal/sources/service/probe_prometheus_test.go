package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// THE PAIRED-RULE-SOURCE HALF OF THE PROBE, AGAINST oto's REAL PROMETHEUS CLIENT.
//
// The factory is a fake and the client is not: what is under test is how the
// probe reads a 404 on buildinfo next to a valid rules envelope, and the client
// is where "valid envelope" is decided. A fake client answering a canned
// RuleGroup would be asking the test what the server said.

// ruleSourceFactory builds the real client against whatever URL the source
// names. Alertmanager is never called by probePrometheus.
type ruleSourceFactory struct{}

func (ruleSourceFactory) Alertmanager(domain.Source, domain.Credential) (AlertmanagerClient, error) {
	panic("probePrometheus does not build an Alertmanager client")
}

func (ruleSourceFactory) Prometheus(src domain.Source, _ domain.Credential, override string) (PrometheusClient, error) {
	base := override
	if base == "" {
		base = src.PrometheusURL
	}
	return prometheus.New(prometheus.Config{
		BaseURL:   base,
		Clock:     clock.NewFake(probeNow),
		UserAgent: "oto-test",
	})
}

var probeNow = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

// ruleServer records the paths it was asked for and answers each from routes;
// a path with no route is a 404, which is what vmalert answers for buildinfo.
type ruleServer struct {
	mu    sync.Mutex
	paths []string
}

func (r *ruleServer) start(t *testing.T, routes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.mu.Unlock()
		body, ok := routes[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (r *ruleServer) asked(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if p == path {
			return true
		}
	}
	return false
}

// vmalertEmptyRules is what vmalert answers for a rule_name[] that matches
// nothing: a success envelope, empty groups, and its own paging keys oto ignores.
const vmalertEmptyRules = `{"status":"success","page":1,"total_pages":1,"total_groups":0,"total_rules":0,"data":{"groups":[]}}`

func probeRuleSource(t *testing.T, url string) ProbeResult {
	t.Helper()
	s := &Service{clients: ruleSourceFactory{}, clk: clock.NewFake(probeNow)}
	var res ProbeResult
	s.probePrometheus(context.Background(), domain.Source{PrometheusURL: url}, domain.Credential{}, probeNow, &res)
	return res
}

func hasWarning(res ProbeResult, code string) bool {
	for _, w := range res.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestAVmalertWithNoBuildinfoProbesAsReachable is git-bug d8fedb8: vmalert
// serves the rules API and 404s buildinfo, and it used to be reported as a
// Prometheus that "did not answer" while RuleSnapshots resolved through it.
func TestAVmalertWithNoBuildinfoProbesAsReachable(t *testing.T) {
	var srv ruleServer
	url := srv.start(t, map[string]string{prometheus.PathRules: vmalertEmptyRules})

	res := probeRuleSource(t, url)

	if !res.PrometheusReachable {
		t.Fatalf("a server answering the rules API probed as unreachable; warnings: %+v", res.Warnings)
	}
	if hasWarning(res, domain.WarnPrometheusUnreachable) {
		t.Errorf("a reachable rule source still carries %s: %+v", domain.WarnPrometheusUnreachable, res.Warnings)
	}
	if res.PrometheusVersion != "" {
		t.Errorf("version = %q; a server with no buildinfo has said nothing about what it is", res.PrometheusVersion)
	}
	if !srv.asked(prometheus.PathBuildInfo) {
		t.Error("buildinfo was not tried first; a real Prometheus must still report its version")
	}
}

// TestAServerFailingBothStillWarns is the guard on the fallback: it widens what
// counts as answering to the rules API, not to anything with a socket.
func TestAServerFailingBothStillWarns(t *testing.T) {
	var srv ruleServer
	url := srv.start(t, nil)

	res := probeRuleSource(t, url)

	if res.PrometheusReachable {
		t.Fatal("a server that 404s both buildinfo and rules probed as reachable")
	}
	if !hasWarning(res, domain.WarnPrometheusUnreachable) {
		t.Fatalf("no %s warning: %+v", domain.WarnPrometheusUnreachable, res.Warnings)
	}
	if !srv.asked(prometheus.PathRules) {
		t.Error("a 404 on buildinfo is an answer, not an outage; the rules API should have been tried")
	}
}

// TestAnUnreachableServerIsNotAskedTwice pins the one place the fallback is
// skipped: a server that never answered buildinfo is down, and asking it again
// would double the probe's wait at the worst moment.
func TestAnUnreachableServerIsNotAskedTwice(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	res := probeRuleSource(t, url)

	if res.PrometheusReachable || !hasWarning(res, domain.WarnPrometheusUnreachable) {
		t.Fatalf("a closed port probed as reachable or without a warning: %+v", res)
	}
}

// TestAPrometheusProbesExactlyAsBefore: buildinfo answers, the version is
// reported, and the rules API is never asked.
func TestAPrometheusProbesExactlyAsBefore(t *testing.T) {
	var srv ruleServer
	url := srv.start(t, map[string]string{
		prometheus.PathBuildInfo: `{"status":"success","data":{"version":"3.5.0","revision":"abc","branch":"HEAD"}}`,
		prometheus.PathRules:     vmalertEmptyRules,
	})

	res := probeRuleSource(t, url)

	if !res.PrometheusReachable || res.PrometheusVersion != "3.5.0" {
		t.Fatalf("reachable=%v version=%q; want true, 3.5.0", res.PrometheusReachable, res.PrometheusVersion)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a healthy Prometheus produced warnings: %+v", res.Warnings)
	}
	if srv.asked(prometheus.PathRules) {
		t.Error("the rules API was asked although buildinfo answered")
	}
}
