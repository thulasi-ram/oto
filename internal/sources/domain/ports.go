package domain

import (
	"context"
	"time"
)

// AlertmanagerClient targets API v2 ONLY. v1 has returned HTTP 410 since
// Alertmanager 0.27.0, and there is no v3.
type AlertmanagerClient interface {
	// Status reads GET /api/v2/status. Used to learn the version and whether a
	// receiver has send_resolved:false — a receiver that never sends resolutions
	// silently turns every alert into an expiry, so oto raises a persistent
	// source_health warning for it (C15).
	Status(ctx context.Context) (AMStatus, error)

	// Alerts reads GET /api/v2/alerts with active/silenced/inhibited/unprocessed.
	// THIS IS THE ONLY WAY TO OBSERVE SUPPRESSION (C1): Alertmanager's MuteStage
	// drops suppressed alerts before they ever reach a webhook.
	Alerts(ctx context.Context, f AlertFilter) ([]GettableAlert, error)

	// Silences reads GET /api/v2/silences. READ ONLY (R3): oto has no write path
	// into the cluster, because a silence-write bug suppresses a real incident.
	Silences(ctx context.Context, f SilenceFilter) ([]GettableSilence, error)
}

// AMStatus is what GET /api/v2/status told oto about an Alertmanager.
type AMStatus struct {
	Version        string
	Uptime         time.Time
	ClusterStatus  string
	ClusterPeers   int
	ResolveTimeout time.Duration
	// SendResolved maps a receiver name to its effective send_resolved (C15).
	SendResolved map[string]bool
	// ServerTime is the upstream clock, for skew measurement (C12).
	ServerTime time.Time
	// RouteTimings are the source's OWN group_wait, group_interval and
	// repeat_interval, read off its running configuration.
	RouteTimings RouteTimings
}

// RouteTimings is what an Alertmanager's OWN configuration says about how it
// batches, read off `route:` in the `config.original` that GET /api/v2/status
// already returns.
//
// ⭐⭐ WHY OTO READS THESE RATHER THAN ASKING FOR THEM. Every oto tuning knob is
// a function of these three numbers (docs/setup/tuning.md): a `refire_grace`
// below `group_interval` is unreachable and every re-fire opens a new thread; a
// `storm_window` below `group_wait` cannot see a burst; a `flap_threshold` above
// the observable ceiling is dead code that looks correctly configured. The tuning
// screen used to ask an operator to TYPE these in and kept the answer in one
// browser's localStorage — unshared, unvalidated, and silently wrong the moment
// somebody edited `alertmanager.yml`, so two operators could open the same page
// and be given contradictory guidance. The source already tells oto, on a call
// oto already makes.
//
// ⛔ EVERY DURATION IS A POINTER AND nil MEANS "THE CONFIGURATION STATED NOTHING".
// It is an OBSERVATION, and Alertmanager's documented defaults are never written
// into it. Alertmanager marshals these three as `omitempty` pointers, so a config
// that sets none of them — which is what a stock `alertmanager.yml` looks like —
// reports none of them; the defaults are applied later, in `dispatch.NewRoute`,
// where the status endpoint cannot see them.
//
// What that absence MEANS is decided at read time, by `Resolve`, which turns it
// into `default_applies` and carries the documented value beside a label saying
// where it came from (see timings.go). Keeping the derivation out of storage is
// what lets the whole estate be corrected by one deploy if Alertmanager ever
// moves a default, and is what keeps "stated 5m" distinguishable from "stated
// nothing, so 5m governs" forever.
type RouteTimings struct {
	// GroupWait is the delay before the FIRST notification for a new group. It is
	// a floor on alert→Slack latency that oto cannot improve, and any flap shorter
	// than it is invisible to oto entirely.
	GroupWait *time.Duration
	// GroupInterval is the minimum gap before an update for a group that changed.
	// It is the clock rate of oto's whole view of the world.
	GroupInterval *time.Duration
	// RepeatInterval is the gap before an unchanged group is re-sent. It is what
	// produces `notification_reason: "repeat interval elapsed"`.
	RepeatInterval *time.Duration

	// ChildRoutes is how many descendant routes sit below the top-level one.
	ChildRoutes int
	// ChildrenWithTimings is how many of those descendants state at least one of
	// the three for themselves.
	//
	// ⚠️ IT IS THE LIMITATION, MADE COUNTABLE. All three settings are per-route
	// and inherited, so the values that actually govern a given alert are the ones
	// on the route that MATCHED it — which depends on that alert's labels. oto
	// reports the TOP-LEVEL route, which is what governs everything matching no
	// more specific route and is exactly what the tuning guide tells an operator
	// to read; resolving per alert would mean re-implementing Alertmanager's
	// matcher tree, including `continue: true` and regex matchers, and being
	// wrong in a way nobody could see. This count is how a reader is TOLD that
	// the top-level number is not the whole story, rather than left to assume it
	// is.
	ChildrenWithTimings int
}

// Known reports whether any of the three durations was STATED by the source's
// own configuration. It is a question about the observation, not about what is in
// force — a config that states none of them still runs on Alertmanager's
// defaults, which is what `Resolve` reports.
func (t RouteTimings) Known() bool {
	return t.GroupWait != nil || t.GroupInterval != nil || t.RepeatInterval != nil
}

// AlertFilter selects which alerts the reconciler asks for. All four booleans
// default to TRUE in the Alertmanager API.
type AlertFilter struct {
	Active, Silenced, Inhibited, Unprocessed bool
	Filter                                   []string
	Receiver                                 string
}

// GettableAlert is one alert as API v2 returns it.
type GettableAlert struct {
	Fingerprint  string
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	EndsAt       time.Time
	UpdatedAt    time.Time
	GeneratorURL string
	Receivers    []string
	Status       AlertStatus
}

// AlertStatus carries the suppression truth. State is the ONLY source of it.
type AlertStatus struct {
	State       string // "unprocessed" | "active" | "suppressed"
	SilencedBy  []string
	InhibitedBy []string
	MutedBy     []string
}

// SilenceFilter selects which silences the mirror asks for.
type SilenceFilter struct {
	Active, Expired, Pending bool
	Filter                   []string
}

// GettableSilence is one Alertmanager silence, mirrored read-only.
type GettableSilence struct {
	ID          string
	Matchers    []Matcher
	StartsAt    time.Time
	EndsAt      time.Time
	UpdatedAt   time.Time
	CreatedBy   string
	Comment     string
	Annotations map[string]string
	State       string // "active" | "pending" | "expired"
}

// Matcher encodes all four operators via (IsRegex, IsEqual):
// "=" (false,true), "!=" (false,false), "=~" (true,true), "!~" (true,false).
type Matcher struct {
	Name    string
	Value   string
	IsRegex bool
	IsEqual bool
}

// PrometheusClient targets the Prometheus v1 HTTP API.
type PrometheusClient interface {
	// Rules reads GET /api/v1/rules?type=alert&rule_name[]=…&exclude_alerts=true.
	// This is the enrichment path for a RuleSnapshot; the primary path decodes
	// g0.expr out of the alert's generatorURL.
	Rules(ctx context.Context, names []string) ([]RuleGroup, error)
}

// RuleGroup is one Prometheus rule group as the v1 API returns it.
type RuleGroup struct {
	Name     string
	File     string
	Interval float64
	Rules    []AlertingRule
}

// AlertingRule mirrors Prometheus's wire shape. Duration fields are FLOAT
// SECONDS — 600 means `for: 10m`. Not milliseconds, not Go duration strings.
type AlertingRule struct {
	Name                     string
	Query                    string
	Duration                 float64 // the `for:` clause, SECONDS
	KeepFiringFor            float64 // the `keep_firing_for:` clause, SECONDS
	Labels, Annotations      map[string]string
	State, Health, LastError string
}
