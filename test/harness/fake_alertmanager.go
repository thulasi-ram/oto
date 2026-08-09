package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/sources/client/alertmanager"
	sources "github.com/thulasiram/oto/internal/sources/domain"
)

// Alertmanager is a fake Alertmanager v2 HTTP API.
//
// It is an httptest.Server speaking the REAL v2 wire format, not a hand-written
// implementation of `sources/domain.AlertmanagerClient`. That distinction is the
// whole point: `Client()` hands back oto's REAL `alertmanager.Client`, so the
// lenient decoding, the singular/plural `/silence` asymmetry, the
// `isEqual`-defaults-to-true matcher rule and the Date-header clock skew are all
// exercised rather than bypassed. A Go-level fake of the port would agree with
// whatever the client does, which is the failure mode ADR 0021 exists to avoid.
//
// It serves the five paths oto reads and nothing else. oto has NO write path
// into an Alertmanager (R3), so neither does the fake.
type Alertmanager struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	config   string
	version  string
	alerts   []AMAlert
	groups   []AMGroup
	silences []AMSilence
	failWith int
	requests []string
}

// AMAlert is one alert as the fake will serve it. The field names are the v2
// wire names on purpose — this is an upstream's payload, not an oto model.
type AMAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
	Fingerprint  string            `json:"fingerprint,omitempty"`
	Receivers    []amReceiver      `json:"receivers,omitempty"`
	Status       AMAlertStatus     `json:"status"`
}

// AMAlertStatus carries the suppression truth. `state` is the ONLY source of it
// anywhere in the system (C1) — nothing on the webhook push path can produce it,
// because Alertmanager's MuteStage drops suppressed alerts first.
type AMAlertStatus struct {
	State       string   `json:"state"`
	SilencedBy  []string `json:"silencedBy,omitempty"`
	InhibitedBy []string `json:"inhibitedBy,omitempty"`
	MutedBy     []string `json:"mutedBy,omitempty"`
}

// AMGroup is one notification group as GET /api/v2/alerts/groups returns it.
type AMGroup struct {
	Labels      map[string]string `json:"labels"`
	RouteLabels map[string]string `json:"routeLabels,omitempty"`
	Receiver    amReceiver        `json:"receiver"`
	Alerts      []AMAlert         `json:"alerts"`
}

// AMSilence is one silence as GET /api/v2/silences returns it.
type AMSilence struct {
	ID        string          `json:"id"`
	Matchers  []AMMatcher     `json:"matchers"`
	StartsAt  time.Time       `json:"startsAt"`
	EndsAt    time.Time       `json:"endsAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	CreatedBy string          `json:"createdBy"`
	Comment   string          `json:"comment"`
	Status    AMSilenceStatus `json:"status"`
}

// AMSilenceStatus is `silenceStatus`: active | pending | expired. DELETE upstream
// EXPIRES a silence rather than removing it, so `expired` is a state a mirrored
// silence keeps reporting forever.
type AMSilenceStatus struct {
	State string `json:"state"`
}

// AMMatcher encodes all four operators through (isRegex, isEqual).
//
// ⚠️ IsEqual is a POINTER because a missing `isEqual` means TRUE upstream, and
// collapsing absent into `false` turns `=` into `!=`.
type AMMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`
}

// ActiveSilence is one live silence over an equality matcher per label.
func ActiveSilence(id string, from time.Time, until time.Time, match map[string]string) AMSilence {
	equal := true
	s := AMSilence{
		ID: id, StartsAt: from.UTC(), EndsAt: until.UTC(), UpdatedAt: from.UTC(),
		CreatedBy: "ops@example.test", Comment: "silenced for a test",
		Status: AMSilenceStatus{State: "active"},
	}
	for name, value := range match {
		s.Matchers = append(s.Matchers, AMMatcher{Name: name, Value: value, IsEqual: &equal})
	}
	return s
}

type amReceiver struct {
	Name string `json:"name"`
}

// DefaultAMConfig is a minimal but REALISTIC `alertmanager.yml`: it states all
// three route timings, which is what makes route-timing provenance observable
// (`stated` rather than `default_applies`).
const DefaultAMConfig = `route:
  receiver: oto
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: oto
    webhook_configs:
      - url: http://oto.test/webhook
        send_resolved: true
`

// NewAlertmanager starts a fake Alertmanager and stops it when the test ends.
func NewAlertmanager(t *testing.T) *Alertmanager {
	t.Helper()
	a := &Alertmanager{t: t, config: DefaultAMConfig, version: "0.28.1"}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

// URL is the base URL to configure an alert source with. It has no trailing
// slash, which is what `alert_sources_base_ck` requires.
func (a *Alertmanager) URL() string { return strings.TrimSuffix(a.srv.URL, "/") }

// Client returns oto's REAL Alertmanager client pointed at this fake. It
// satisfies both `sources/domain.AlertmanagerClient` and the wider
// `sources/service.AlertmanagerClient`.
func (a *Alertmanager) Client(clk clock.Clock) *alertmanager.Client {
	a.t.Helper()
	c, err := alertmanager.New(alertmanager.Config{
		BaseURL:   a.URL(),
		Clock:     clk,
		UserAgent: "oto-test",
	})
	if err != nil {
		a.t.Fatalf("harness: build alertmanager client: %v", err)
	}
	return c
}

// SetVersion sets what GET /api/v2/status reports as versionInfo.version.
func (a *Alertmanager) SetVersion(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.version = v
}

// SetConfig replaces the `config.original` YAML the status endpoint returns. It
// is the ONLY place resolve_timeout, send_resolved and the route timings are
// exposed by Alertmanager, so it is how a test drives all three.
func (a *Alertmanager) SetConfig(yaml string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config = yaml
}

// SetAlerts replaces what GET /api/v2/alerts returns.
func (a *Alertmanager) SetAlerts(alerts ...AMAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = alerts
}

// SetGroups replaces what GET /api/v2/alerts/groups returns.
func (a *Alertmanager) SetGroups(groups ...AMGroup) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.groups = groups
}

// SetSilences replaces what GET /api/v2/silences returns.
func (a *Alertmanager) SetSilences(silences ...AMSilence) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.silences = silences
}

// FailWith makes every subsequent request answer with an HTTP status. Pass 0 to
// go back to serving normally. It is how a test drives source_health from
// `healthy` to `degraded` without unplugging anything.
func (a *Alertmanager) FailWith(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failWith = status
}

// Requests returns the "METHOD path?query" of every request served, in order.
func (a *Alertmanager) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

// FiringAlert is one active, unsuppressed alert with the given labels.
func FiringAlert(labels map[string]string, startsAt time.Time) AMAlert {
	return AMAlert{
		Labels:    labels,
		StartsAt:  startsAt.UTC(),
		UpdatedAt: startsAt.UTC(),
		Status:    AMAlertStatus{State: "active"},
	}
}

// SuppressedAlert is one alert Alertmanager reports as silenced. ONLY the
// reconciler can ever see this (C1, ADR 0006), so this constructor is the only
// way a test can produce a `suppressed` occurrence.
func SuppressedAlert(labels map[string]string, startsAt time.Time, silenceIDs ...string) AMAlert {
	a := FiringAlert(labels, startsAt)
	a.Status = AMAlertStatus{State: "suppressed", SilencedBy: silenceIDs}
	return a
}

func (a *Alertmanager) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI())
	fail := a.failWith
	a.mu.Unlock()

	if fail != 0 {
		http.Error(w, http.StatusText(fail), fail)
		return
	}

	switch {
	case r.URL.Path == alertmanager.PathStatus:
		a.writeStatus(w)
	case r.URL.Path == alertmanager.PathAlertGroups:
		a.mu.Lock()
		groups := a.groups
		a.mu.Unlock()
		writeJSON(w, orEmpty(groups))
	case r.URL.Path == alertmanager.PathAlerts:
		a.mu.Lock()
		alerts := a.alerts
		a.mu.Unlock()
		writeJSON(w, orEmpty(alerts))
	case r.URL.Path == alertmanager.PathSilences:
		a.mu.Lock()
		silences := a.silences
		a.mu.Unlock()
		writeJSON(w, orEmpty(silences))
	case strings.HasPrefix(r.URL.Path, alertmanager.PathSilence):
		a.writeSilence(w, strings.TrimPrefix(r.URL.Path, alertmanager.PathSilence))
	case r.URL.Path == alertmanager.PathReceivers:
		writeJSON(w, []amReceiver{{Name: "oto"}})
	default:
		http.NotFound(w, r)
	}
}

func (a *Alertmanager) writeStatus(w http.ResponseWriter) {
	a.mu.Lock()
	version, config := a.version, a.config
	a.mu.Unlock()

	writeJSON(w, map[string]any{
		"cluster":     map[string]any{"name": "oto-test", "status": "ready", "peers": []any{}},
		"versionInfo": map[string]any{"version": version},
		"config":      map[string]any{"original": config},
		"uptime":      time.Now().UTC(),
	})
}

func (a *Alertmanager) writeSilence(w http.ResponseWriter, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.silences {
		if s.ID == id {
			writeJSON(w, s)
			return
		}
	}
	http.Error(w, `{"error":"silence not found"}`, http.StatusNotFound)
}

// Compile-time proof that the real client, driven at this fake, IS the port. It
// lives here so that a change to the port breaks the harness loudly.
var _ sources.AlertmanagerClient = (*alertmanager.Client)(nil)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// orEmpty makes a nil slice marshal as `[]` rather than `null`; the real API
// never sends a bare null for a collection.
func orEmpty[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
