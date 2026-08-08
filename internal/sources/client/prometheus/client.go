package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpc"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// ErrPrefix namespaces every error code this client produces.
const ErrPrefix = "prometheus"

// The v1 HTTP API paths oto uses. It uses exactly two: oto never queries
// Prometheus for data, only for the definition of a rule (R6) and for the
// version, so there is no /query, no /query_range and no /series here.
const (
	// PathRules is GET /api/v1/rules.
	PathRules = "/api/v1/rules"
	// PathBuildInfo is GET /api/v1/status/buildinfo.
	PathBuildInfo = "/api/v1/status/buildinfo"
)

// Request bounds.
const (
	// MaxRuleNames is the number of rule_name[] filters one request may carry.
	// The docs warn that paging through all rules is not consistent while rule
	// groups are being modified, so oto always filters and never pages.
	MaxRuleNames = 128
	// MaxRuleNameBytes mirrors rule_snapshots_name_ck.
	MaxRuleNameBytes = 1024
)

// Config builds a Client for one AlertSource's paired Prometheus.
type Config struct {
	// BaseURL is alert_sources.prometheus_url: absolute http(s), no trailing
	// slash. In a federated setup this may be per-alert rather than per-source,
	// recovered from generatorURL (research A7 pitfall 5).
	BaseURL string
	// Auth is the resolved credential.
	Auth httpc.Auth
	// TLS carries alert_sources.tls_skip_verify and any custom CA bundle.
	TLS httpc.TLSOptions
	// Timeout bounds one attempt.
	Timeout time.Duration
	// MaxResponseBytes caps a response body.
	MaxResponseBytes int64
	// Retry is the in-band retry policy; 5xx and network only.
	Retry httpc.Retry
	// UserAgent identifies oto upstream.
	UserAgent string
	// Clock is the time source.
	Clock clock.Clock
	// Transport lets a test point the client at an httptest.Server.
	Transport http.RoundTripper
	// DialContext installs the SSRF guard's dialer.
	//
	// ⭐ It is the control, not the URL validation at configuration time: it sees
	// the address the socket actually connects to, so a DNS record re-pointed
	// between the check and the dial has nothing left to win.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Sleep lets a test drive the retry backoff.
	Sleep httpc.Sleeper
}

// Client is a read-only Prometheus v1 API client.
type Client struct {
	http *httpc.Client
	clk  clock.Clock
}

// Client implements the port exactly as SPEC §F.4 declares it.
var _ domain.PrometheusClient = (*Client)(nil)

// New builds a Client.
func New(cfg Config) (*Client, error) {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.New()
	}
	hc, err := httpc.New(httpc.Config{
		BaseURL:          cfg.BaseURL,
		Auth:             cfg.Auth,
		TLS:              cfg.TLS,
		Timeout:          cfg.Timeout,
		MaxResponseBytes: cfg.MaxResponseBytes,
		Retry:            cfg.Retry,
		UserAgent:        cfg.UserAgent,
		ErrPrefix:        ErrPrefix,
		Clock:            clk,
		Transport:        cfg.Transport,
		DialContext:      cfg.DialContext,
		Sleep:            cfg.Sleep,
	})
	if err != nil {
		return nil, err
	}
	return &Client{http: hc, clk: clk}, nil
}

// BaseURL is the canonical Prometheus root this client talks to.
func (c *Client) BaseURL() string { return c.http.BaseURL() }

// envelope is the v1 API's uniform response wrapper. Prometheus returns it on
// success AND on its own 4xx, so a decoded envelope with status="error" is a
// refusal that carries a usable reason.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

// wireRulesData is the `data` of GET /api/v1/rules.
type wireRulesData struct {
	Groups []wireRuleGroup `json:"groups"`
}

type wireRuleGroup struct {
	Name     string     `json:"name"`
	File     string     `json:"file"`
	Interval float64    `json:"interval"`
	Rules    []wireRule `json:"rules"`
}

// wireRule covers both rule types; recording rules are filtered out by Type.
//
// Duration and KeepFiringFor are FLOAT SECONDS. 600 means `for: 10m`. Not
// milliseconds, not a Go duration string, not a Prometheus duration string
// (research A7 pitfall 6).
type wireRule struct {
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Query         string            `json:"query"`
	Duration      float64           `json:"duration"`
	KeepFiringFor float64           `json:"keepFiringFor"`
	Labels        map[string]string `json:"labels"`
	Annotations   map[string]string `json:"annotations"`
	State         string            `json:"state"`
	Health        string            `json:"health"`
	LastError     string            `json:"lastError"`
}

// wireBuildInfo is the `data` of GET /api/v1/status/buildinfo.
type wireBuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// BuildInfo is what a Prometheus reports about itself.
type BuildInfo struct {
	Version   string
	Revision  string
	Branch    string
	BuildUser string
	BuildDate string
	GoVersion string
	// Latency is how long the probe took, on oto's clock.
	Latency time.Duration
}

// Rules reads GET /api/v1/rules?type=alert&exclude_alerts=true, optionally
// filtered by rule_name[].
//
// `exclude_alerts=true` is not an optimisation detail: without it the response
// carries every active alert instance of every matched rule, which on a busy
// Prometheus is orders of magnitude larger than the rule definitions oto wants.
//
// Passing names is strongly preferred over fetching everything: the docs
// explicitly disclaim consistency while paging, so oto filters instead of
// paging (research A7 pitfall 7).
func (c *Client) Rules(ctx context.Context, names []string) ([]domain.RuleGroup, error) {
	q := url.Values{}
	q.Set("type", "alert")
	q.Set("exclude_alerts", "true")

	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if len(n) > MaxRuleNameBytes {
			return nil, errs.Newf(errs.KindValidation, ErrPrefix+"_rule_name_too_large",
				"a rule name must be at most %d bytes", MaxRuleNameBytes)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		if len(seen) > MaxRuleNames {
			return nil, errs.Newf(errs.KindValidation, ErrPrefix+"_too_many_rule_names",
				"at most %d rule names may be requested at once", MaxRuleNames)
		}
		q.Add("rule_name[]", n)
	}

	var data wireRulesData
	if err := c.getEnvelope(ctx, PathRules, q, &data); err != nil {
		return nil, err
	}

	out := make([]domain.RuleGroup, 0, len(data.Groups))
	for _, g := range data.Groups {
		rg := domain.RuleGroup{
			Name:     g.Name,
			File:     g.File,
			Interval: g.Interval,
			Rules:    make([]domain.AlertingRule, 0, len(g.Rules)),
		}
		for _, r := range g.Rules {
			// A recording rule has type "recording" and no alertname. Older
			// servers have been seen to omit `type` entirely, so an untyped
			// rule with a name is kept rather than dropped.
			if r.Type != "" && !strings.EqualFold(r.Type, "alerting") {
				continue
			}
			if r.Name == "" {
				continue
			}
			rg.Rules = append(rg.Rules, domain.AlertingRule{
				Name:          r.Name,
				Query:         r.Query,
				Duration:      r.Duration,
				KeepFiringFor: r.KeepFiringFor,
				Labels:        copyMap(r.Labels),
				Annotations:   copyMap(r.Annotations),
				State:         r.State,
				Health:        r.Health,
				LastError:     r.LastError,
			})
		}
		out = append(out, rg)
	}
	return out, nil
}

// BuildInfo reads GET /api/v1/status/buildinfo. It is the cheapest liveness and
// identity probe there is, and it is what tells an operator whether the
// prometheus_url they typed points at a Prometheus at all.
func (c *Client) BuildInfo(ctx context.Context) (BuildInfo, error) {
	started := c.clk.Now()

	var data wireBuildInfo
	if err := c.getEnvelope(ctx, PathBuildInfo, nil, &data); err != nil {
		return BuildInfo{}, err
	}
	out := BuildInfo{
		Version:   strings.TrimSpace(data.Version),
		Revision:  data.Revision,
		Branch:    data.Branch,
		BuildUser: data.BuildUser,
		BuildDate: data.BuildDate,
		GoVersion: data.GoVersion,
		Latency:   c.clk.Since(started),
	}
	if out.Version == "" {
		return out, c.http.Errorf(httpc.CodeMalformedResponse,
			errors.New("buildinfo carried no version"))
	}
	return out, nil
}

// getEnvelope performs the request, unwraps the v1 envelope and decodes `data`.
func (c *Client) getEnvelope(ctx context.Context, path string, q url.Values, out any) error {
	var env envelope
	if _, err := c.http.GetJSON(ctx, path, q, &env); err != nil {
		return err
	}

	// A 200 carrying status="error" is Prometheus refusing, not Prometheus
	// failing. It is still an upstream failure from oto's point of view, but the
	// distinct code keeps "your rule_name filter was rejected" separable from
	// "the process is down".
	if env.Status != "" && env.Status != "success" {
		reason := env.Error
		if env.ErrorType != "" {
			reason = env.ErrorType + ": " + reason
		}
		return &errs.Error{
			Kind:    errs.KindUpstreamDown,
			Code:    ErrPrefix + "_api_error",
			Message: "the Prometheus API refused the request",
			Cause:   errors.New(truncate(reason, 512)),
		}
	}
	if len(env.Data) == 0 {
		return c.http.Errorf(httpc.CodeMalformedResponse, errors.New("no data in the API envelope"))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return c.http.Errorf(httpc.CodeMalformedResponse, err)
	}
	return nil
}

// copyMap returns a defensive copy, or nil for an empty input.
func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// truncate bounds a cause string.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
