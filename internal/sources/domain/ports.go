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
