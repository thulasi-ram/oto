package harness

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	sources "github.com/thulasiram/oto/internal/sources/domain"
)

// Prometheus is a fake Prometheus v1 HTTP API.
//
// Like the Alertmanager fake it is an httptest.Server and `Client()` returns
// oto's REAL client, so the v1 envelope — a 200 that carries
// `{"status":"error"}` is a REFUSAL, not a failure — is decoded by the code that
// ships rather than by the test.
//
// It serves exactly two paths, because oto reads exactly two: oto never queries
// Prometheus for data, only for the definition of a rule (R6) and for the
// version.
type Prometheus struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	buildInfo PromBuildInfo
	groups    []PromRuleGroup
	apiError  string
	failWith  int
	requests  []string
}

// PromBuildInfo is what GET /api/v1/status/buildinfo reports.
type PromBuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	Branch    string `json:"branch,omitempty"`
	BuildUser string `json:"buildUser,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
}

// PromRuleGroup is one rule group on the v1 wire.
type PromRuleGroup struct {
	Name     string     `json:"name"`
	File     string     `json:"file"`
	Interval float64    `json:"interval"`
	Rules    []PromRule `json:"rules"`
}

// PromRule is one rule on the v1 wire.
//
// ⚠️ Duration and KeepFiringFor are FLOAT SECONDS: 600 means `for: 10m`. Not
// milliseconds, not a Go duration string, not a Prometheus duration string.
type PromRule struct {
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Query         string            `json:"query"`
	Duration      float64           `json:"duration"`
	KeepFiringFor float64           `json:"keepFiringFor,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	State         string            `json:"state,omitempty"`
	Health        string            `json:"health,omitempty"`
	LastError     string            `json:"lastError,omitempty"`
}

// NewPrometheus starts a fake Prometheus and stops it when the test ends.
func NewPrometheus(t *testing.T) *Prometheus {
	t.Helper()
	p := &Prometheus{t: t, buildInfo: PromBuildInfo{Version: "3.1.0"}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the base URL to set as alert_sources.prometheus_url. No trailing
// slash, as alert_sources_prom_ck requires.
func (p *Prometheus) URL() string { return strings.TrimSuffix(p.srv.URL, "/") }

// Client returns oto's REAL Prometheus client pointed at this fake. It
// satisfies `sources/domain.PrometheusClient` and `sources/service`'s wider
// port.
func (p *Prometheus) Client(clk clock.Clock) *prometheus.Client {
	p.t.Helper()
	c, err := prometheus.New(prometheus.Config{
		BaseURL:   p.URL(),
		Clock:     clk,
		UserAgent: "oto-test",
	})
	if err != nil {
		p.t.Fatalf("harness: build prometheus client: %v", err)
	}
	return c
}

// SetBuildInfo replaces what the version probe reports.
func (p *Prometheus) SetBuildInfo(bi PromBuildInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buildInfo = bi
}

// SetRuleGroups replaces what GET /api/v1/rules returns.
func (p *Prometheus) SetRuleGroups(groups ...PromRuleGroup) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groups = groups
}

// RefuseWith makes the API answer 200 with `{"status":"error"}` — Prometheus
// REFUSING rather than Prometheus being down. The distinction is carried in
// oto's error taxonomy and this is how a test reaches the refusal branch.
func (p *Prometheus) RefuseWith(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apiError = reason
}

// FailWith makes every subsequent request answer with an HTTP status. 0 restores
// normal service.
func (p *Prometheus) FailWith(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failWith = status
}

// Requests returns the "METHOD path?query" of every request served, in order.
// It is how a test asserts that `exclude_alerts=true` and `rule_name[]` were
// actually sent.
func (p *Prometheus) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

// AlertingRule is one `type: alerting` rule with a `for:` duration in seconds.
func AlertingRule(name, query string, forSeconds float64) PromRule {
	return PromRule{
		Type: "alerting", Name: name, Query: query,
		Duration: forSeconds, State: "firing", Health: "ok",
	}
}

func (p *Prometheus) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.URL.RequestURI())
	fail, apiErr := p.failWith, p.apiError
	p.mu.Unlock()

	if fail != 0 {
		http.Error(w, http.StatusText(fail), fail)
		return
	}
	if apiErr != "" {
		// A 200 carrying status="error". Prometheus really does answer this way.
		writeJSON(w, map[string]any{
			"status": "error", "errorType": "bad_data", "error": apiErr,
		})
		return
	}

	switch r.URL.Path {
	case prometheus.PathRules:
		p.mu.Lock()
		groups := p.groups
		p.mu.Unlock()
		writeJSON(w, map[string]any{
			"status": "success",
			"data":   map[string]any{"groups": orEmpty(groups)},
		})
	case prometheus.PathBuildInfo:
		p.mu.Lock()
		bi := p.buildInfo
		p.mu.Unlock()
		writeJSON(w, map[string]any{"status": "success", "data": bi})
	default:
		http.NotFound(w, r)
	}
}

// Compile-time proof that the real client, driven at this fake, IS the port.
var _ sources.PrometheusClient = (*prometheus.Client)(nil)
