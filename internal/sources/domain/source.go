package domain

import (
	"time"

	"github.com/google/uuid"
)

// Kind mirrors alert_sources.kind. Grafana Unified Alerting speaks an
// Alertmanager-compatible superset, which is why it is a kind of the same entity
// rather than a second one (SPEC §L.3.1).
type Kind string

// The configured source kinds (alert_sources_kind_ck).
const (
	// KindAlertmanager is a Prometheus Alertmanager.
	KindAlertmanager Kind = "alertmanager"
	// KindGrafana is Grafana's embedded Alertmanager.
	KindGrafana Kind = "grafana"
)

// AuthKind is the subset of channel_credentials.kind an AlertSource uses. The
// secret store is shared with Channels rather than duplicated (SPEC §D.8).
type AuthKind string

// The AlertSource auth kinds.
const (
	// AuthNone means the upstream is unauthenticated.
	AuthNone AuthKind = "none"
	// AuthBearer means a bearer token.
	AuthBearer AuthKind = "bearer"
	// AuthBasic means HTTP basic credentials.
	AuthBasic AuthKind = "basic"
)

// Bounds that mirror the alert_sources DDL. R9 binds each of these to three
// places — the DTO validate tag, this constructor-side constant, and the CHECK.
const (
	// MaxSourceNameBytes mirrors alert_sources_name_ck.
	MaxSourceNameBytes = 120
	// MaxURLBytes mirrors the `max=2048` on base_url and prometheus_url.
	MaxURLBytes = 2048
	// MinReconcileInterval mirrors alert_sources_ivl_min_ck.
	MinReconcileInterval = 10 * time.Second
	// MaxReconcileInterval mirrors alert_sources_ivl_ck.
	MaxReconcileInterval = 3600 * time.Second
	// MaxIgnoreLabels mirrors alert_sources_ignore_ck.
	MaxIgnoreLabels = 64
	// MaxRedactPatterns mirrors alert_sources_redactl_ck / _redacta_ck.
	MaxRedactPatterns = 64
)

// Credential is a resolved, UNSEALED outbound credential.
//
// It exists only in memory, only for the duration of one client construction,
// and is never logged, never rendered into an errs.Message and never persisted
// in this shape — the sealed form lives in channel_credentials.
type Credential struct {
	Kind     AuthKind
	Token    string
	Username string
	Password string
}

// IsZero reports whether the credential carries nothing, which is the same thing
// as AuthNone for every caller.
func (c Credential) IsZero() bool {
	return (c.Kind == "" || c.Kind == AuthNone) && c.Token == "" && c.Username == "" && c.Password == ""
}

// Source is one configured Alertmanager, optionally paired with the Prometheus
// that feeds it (alert_sources, SPEC §D.2).
//
// Alertmanager HA replicas are several Sources sharing one Cluster: the Cluster
// is the identity and failure domain, the Source is one endpoint.
type Source struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ClusterID uuid.UUID
	Name      string
	Kind      Kind

	// BaseURL is the Alertmanager API root with NO trailing slash
	// (alert_sources_base_ck).
	BaseURL string
	// PrometheusURL is optional. Its presence is what enables
	// origin='prometheus_api' RuleSnapshots (SPEC §D.6).
	PrometheusURL string

	// AuthCredentialID points into channel_credentials. Nil means AuthNone.
	AuthCredentialID *uuid.UUID
	// TLSSkipVerify is an explicit operator opt-in for a self-signed upstream.
	TLSSkipVerify bool

	InjectLabels      map[string]string
	IgnoreLabels      []string
	RedactLabels      []string
	RedactAnnotations []string

	PushEnabled       bool
	ReconcileEnabled  bool
	ReconcileInterval time.Duration

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// HasPrometheus reports whether rule snapshots can reach the Prometheus API for
// this source. Without it, the only recoverable rule detail is the expression
// decoded out of generatorURL.
func (s Source) HasPrometheus() bool { return s.PrometheusURL != "" }

// Deleted reports whether the source is soft deleted.
func (s Source) Deleted() bool { return s.DeletedAt != nil }

// SourceFilter selects sources for a list query.
type SourceFilter struct {
	ClusterID        *uuid.UUID
	Kind             Kind
	ReconcileEnabled *bool
	IncludeDeleted   bool
}

// HealthStatus mirrors source_health.status.
//
// Anything other than HealthHealthy BLOCKS the reaper (SPEC §B.4): losing sight
// of an alert is not the alert resolving.
type HealthStatus string

// The health statuses (source_health_status_ck).
const (
	// HealthUnknown means oto has never successfully probed the source.
	HealthUnknown HealthStatus = "unknown"
	// HealthHealthy means the last probe succeeded with no warnings.
	HealthHealthy HealthStatus = "healthy"
	// HealthDegraded means the source answers but something is wrong — a
	// send_resolved:false receiver, an unparseable config, one failed probe.
	HealthDegraded HealthStatus = "degraded"
	// HealthUnreachable means UnreachableAfterFailures consecutive failures.
	HealthUnreachable HealthStatus = "unreachable"
)

// UnreachableAfterFailures is the consecutive-failure count at which a source is
// declared unreachable and the reaper is blocked (SPEC §G.8.5).
const UnreachableAfterFailures = 3

// Warning codes recorded in source_health.warnings. They are stable strings: the
// UI keys off them and an operator greps for them.
const (
	// WarnSendResolvedFalse is C15: a receiver with send_resolved disabled turns
	// every alert into an expiry, because oto will never see the resolution.
	WarnSendResolvedFalse = "send_resolved_false"
	// WarnConfigUnparseable means GET /api/v2/status returned a config oto could
	// not read, so send_resolved and resolve_timeout are unknown.
	WarnConfigUnparseable = "alertmanager_config_unparseable"
	// WarnClockSkew means the upstream clock differs from oto's by more than
	// MaxTolerableSkew. Skew is measured and surfaced, never used to reject an
	// event (C12).
	WarnClockSkew = "clock_skew"
	// WarnReconcileDisabled means `reconcile_enabled` is false for this source.
	//
	// ⭐ ADR 0006 CALLS THE RECONCILER MANDATORY. The flag survives because oto
	// cannot always reach an Alertmanager's API — the ADR's own consequences
	// section names outbound reachability as a requirement, and a source behind a
	// one-way network path is a real deployment, not a misconfiguration. What the
	// flag must never be is SILENT: with it off, Alertmanager's MuteStage drops
	// silenced and inhibited alerts before any webhook fires, so oto can never
	// observe suppression for this source and will render an upstream-muted alert
	// as firing indefinitely. This warning is that fact, standing, on the source.
	WarnReconcileDisabled = "reconcile_disabled"
	// WarnPrometheusUnconfigured means no prometheus_url, so RuleSnapshots can
	// only ever carry the expression decoded from generatorURL.
	WarnPrometheusUnconfigured = "prometheus_not_configured"
	// WarnPrometheusUnreachable means prometheus_url is set but did not answer.
	WarnPrometheusUnreachable = "prometheus_unreachable"
	// WarnAlertmanagerUnreachable means the Alertmanager itself did not answer.
	WarnAlertmanagerUnreachable = "alertmanager_unreachable"
	// WarnAlertmanagerMalformed means the Alertmanager answered with something
	// oto cannot parse — almost always a base URL pointing at the wrong thing.
	WarnAlertmanagerMalformed = "alertmanager_malformed_response"
	// WarnClusterNotReady means the AM HA cluster is settling or disabled.
	WarnClusterNotReady = "alertmanager_cluster_not_ready"
)

// MaxTolerableSkew is the absolute clock difference above which a source earns a
// WarnClockSkew. Alertmanager timestamps drive occurred_at, which the UI shows.
const MaxTolerableSkew = 30 * time.Second

// HealthWarning is one structured operator warning inside
// source_health.warnings.
type HealthWarning struct {
	// Code is one of the Warn* constants.
	Code string
	// Message is human and always safe to render.
	Message string
	// Subject names what the warning is about (a receiver name, a URL). It is
	// never a secret.
	Subject string
	// At is when the warning was last observed, on oto's clock.
	At time.Time
}

// SourceHealth is the liveness projection of one AlertSource (source_health).
// It is a projection, updated in place — it is NOT an event, and there is no
// history of it.
type SourceHealth struct {
	SourceID uuid.UUID
	OrgID    uuid.UUID
	Status   HealthStatus

	LastPushAt          *time.Time
	LastReconcileAt     *time.Time
	LastReconcileStatus string
	LastError           string
	ConsecutiveFailures int

	// AMVersion gates notification_reason handling (AM >= 0.32.0).
	AMVersion string
	// SendResolved is nil when oto has not yet observed it (the NULL in the DDL).
	SendResolved *bool
	// ClockSkew is an EWMA of oto's clock minus the upstream's (C12).
	ClockSkew time.Duration
	// DivergenceCount is the reconciler's disagreement count from the last run.
	// It is the canary for every correctness bug in the system (SPEC §G.8.4).
	DivergenceCount int

	Warnings  []HealthWarning
	UpdatedAt time.Time
}

// BlocksReaper reports whether this health state forbids expiring occurrences.
// The reaper guard is the highest-value correctness rule in the system: a source
// that is merely unobserved must never produce a fabricated expiry (SPEC §B.4).
func (h SourceHealth) BlocksReaper() bool { return h.Status != HealthHealthy }

// Warning returns the warning carrying code, if there is one.
func (h SourceHealth) Warning(code string) (HealthWarning, bool) {
	for _, w := range h.Warnings {
		if w.Code == code {
			return w, true
		}
	}
	return HealthWarning{}, false
}
