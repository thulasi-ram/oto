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

	PushEnabled bool
	// ReconcileInterval is HOW OFTEN `source.reconcile` polls this upstream, and it
	// is the whole of the reconciliation tuning surface.
	//
	// ⛔ THERE IS NO COMPANION BOOLEAN, AND ONE MUST NOT BE ADDED BACK. There was
	// one — `alert_sources.reconcile_enabled`, dropped in 00038 — and it made the
	// component ADR 0006 calls mandatory switchable off with one PATCH. The
	// reconciler is the only producer of `suppressed` AND the only thing that keeps
	// `source_health` fresh, so a source with it off kept a frozen `healthy`
	// verdict that the reaper went on trusting: silenced alerts stopped arriving by
	// webhook, `resolve_grace` elapsed, and oto recorded an ending that never
	// happened. A deployment that needs to poll gently raises this number, up to an
	// hour; a deployment oto cannot reach at all is `unreachable`, which blocks the
	// reaper, which is the honest answer rather than a silent one.
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
	ClusterID      *uuid.UUID
	Kind           Kind
	IncludeDeleted bool
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
	// ⛔ THERE IS NO `reconcile_disabled` WARNING, BECAUSE THERE IS NOTHING TO WARN
	// ABOUT. It existed while `alert_sources.reconcile_enabled` did, as the thing
	// that was supposed to make the switch non-silent — and it never reached a
	// human: it was raised only by `GET /sources/{id}/health`, the sources list
	// never carried it, and the settings screen renders no warnings at all. A
	// degradation nobody is shown is a degradation nobody consented to, which is
	// why 00038 removed the switch instead of the warning (ADR 0006, second
	// amendment).
	//
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

	// RouteTimings are this source's OWN group_wait, group_interval and
	// repeat_interval, observed from its running configuration rather than typed
	// in by a human. nil in any field means UNKNOWN and must stay unknown.
	RouteTimings RouteTimings
	// Routes is the source's whole route tree, resolved: every delivering route
	// with its receiver, its INHERITED timings and its matcher path, plus which
	// receiver oto believes is its own and on what basis (routes.go).
	//
	// ⭐ IT IS WHY RouteTimings ABOVE IS A FALLBACK. The three timings are
	// per-route and inherited, so the numbers governing the alerts oto is
	// actually sent are the ones on the route(s) reaching oto's receiver — which
	// is usually not the top-level route on any Alertmanager that overrides
	// anything. It is read on the same probe, under the same
	// `RouteTimingsAt` timestamp.
	Routes RouteResolution
	// RouteTimingsAt is when the three were last read off the source, on oto's
	// clock. It is nil until the first successful parse.
	//
	// ⚠️ IT IS A SEPARATE TIMESTAMP FROM UpdatedAt ON PURPOSE. `updated_at` moves
	// on every probe, including the ones that could not reach the source or could
	// not parse its config; this one moves only when the numbers beside it were
	// actually observed. A screen that showed `updated_at` beside a stale set of
	// timings would claim they were fresh.
	RouteTimingsAt *time.Time

	Warnings  []HealthWarning
	UpdatedAt time.Time
}

// BlocksReaper reports whether this health state forbids expiring cases.
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
