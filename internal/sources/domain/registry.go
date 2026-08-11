package domain

import (
	"time"

	"github.com/google/uuid"
)

// ⚠️ WHY THE WRITE COMMANDS LIVE IN `domain` AND NOT IN `repository`.
//
// `internal/sources/api` declares the port it needs and `internal/sources/
// repository` satisfies it, and neither may import the other (CONTEXT.md §5.1).
// The command types therefore have to live in the one package both are permitted
// to name. That is this one, and it is also where they belong: a `SourceDraft` is
// a statement about the DOMAIN — which fields make an upstream registerable —
// and not about SQL.

// Cluster is one identity and failure domain (`clusters`).
//
// `Key` participates in ALERT IDENTITY (§C.2): the same label set in prod-eu and
// prod-us are DIFFERENT Alerts, because they have different blast radii. That is
// why there is no method anywhere that changes it — see the ⛔ note on
// ClusterPatch.
type Cluster struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// Key is `clusters.cluster_key`, hashed into every alert_key in the cluster.
	Key string
	// DisplayName is the human label. Never hashed, therefore safe to rename.
	DisplayName string
	// SourceCount is how many live sources share this failure domain.
	// Alertmanager HA replicas are several sources in one cluster, so a count
	// above one is normal.
	SourceCount int

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// MaxClusterNameBytes mirrors clusters_name_ck.
const MaxClusterNameBytes = 120

// ClusterDraft creates an identity/failure domain.
type ClusterDraft struct {
	Key         string
	DisplayName string
}

// ClusterPatch is the partial update.
//
// ⛔ IT CARRIES NO `Key` FIELD AND MUST NEVER GAIN ONE. Changing `cluster_key`
// would re-key every alert identity in the cluster and silently fork the history
// of everything in it. The immutability is expressed as an ABSENT FIELD rather
// than as a runtime check, because a field that cannot be set cannot be set by
// accident.
type ClusterPatch struct {
	DisplayName *string
}

// IsEmpty reports whether the patch would change nothing.
func (p ClusterPatch) IsEmpty() bool { return p.DisplayName == nil }

// SourceDraft registers one upstream.
//
// It is deliberately not a `Source`: id, timestamps and health are server-owned,
// and a create that accepted them would let a caller assert them.
type SourceDraft struct {
	ClusterID     uuid.UUID
	Name          string
	Kind          Kind
	BaseURL       string
	PrometheusURL string

	// AuthCredentialID points into the shared sealed-secret store. Nil means the
	// upstream is unauthenticated.
	AuthCredentialID *uuid.UUID
	// TLSSkipVerify is an EXPLICIT operator opt-in for a self-signed upstream. It
	// is never inferred and never defaulted on.
	TLSSkipVerify bool

	InjectLabels      map[string]string
	IgnoreLabels      []string
	RedactLabels      []string
	RedactAnnotations []string

	PushEnabled bool
	// ReconcileInterval is the only reconciliation setting a create carries: the
	// reconciler runs for every source (ADR 0006), and there is no field here that
	// could ask for it not to.
	ReconcileInterval time.Duration
}

// SourcePatch is the partial update.
//
// Every field is a pointer so that "absent" and "set to the zero value" are
// different requests: a PATCH that could not tell them apart would silently
// disable ingestion on a source that only meant to be renamed.
//
// `Kind` is deliberately absent — turning an Alertmanager into a Grafana would
// reinterpret every payload already stored against it.
type SourcePatch struct {
	ClusterID *uuid.UUID
	Name      *string
	BaseURL   *string
	// PrometheusURL is a double pointer: nil leaves it, a pointer to nil clears
	// it, a pointer to a pointer sets it. The contract types this field as
	// `["string","null"]` for exactly that reason.
	PrometheusURL    **string
	AuthCredentialID **uuid.UUID
	TLSSkipVerify    *bool

	// ⚠️ Changing IgnoreLabels does NOT re-key existing alerts. It feeds the
	// alert-identity hash, so new identities are created from that point forward
	// (§C.2). Documented behaviour, not a defect.
	InjectLabels      *map[string]string
	IgnoreLabels      *[]string
	RedactLabels      *[]string
	RedactAnnotations *[]string

	PushEnabled *bool
	// ReconcileInterval is tunable; whether the reconciler runs at all is not.
	ReconcileInterval *time.Duration
}

// IsEmpty reports whether the patch would change nothing.
func (p SourcePatch) IsEmpty() bool {
	return p.ClusterID == nil && p.Name == nil && p.BaseURL == nil &&
		p.PrometheusURL == nil && p.AuthCredentialID == nil && p.TLSSkipVerify == nil &&
		p.InjectLabels == nil && p.IgnoreLabels == nil && p.RedactLabels == nil &&
		p.RedactAnnotations == nil && p.PushEnabled == nil &&
		p.ReconcileInterval == nil
}

// DefaultIgnoreLabels mirrors the `alert_sources.ignore_labels` DDL default and
// the contract's documented default for `CreateSourceRequest`.
//
// These five are the labels an HA Alertmanager pair, a Prometheus replica pair or
// a Kubernetes deployment adds to otherwise identical alerts. Hashing them would
// turn one alert into two every time a pod restarted.
func DefaultIgnoreLabels() []string {
	return []string{"prometheus_replica", "__replica__", "monitor", "replica", "pod_template_hash"}
}

// DefaultReconcileInterval mirrors the DDL default.
const DefaultReconcileInterval = 30 * time.Second

// ReconcileResult is the outcome of one forced reconcile pass.
//
// ⛔ The reconciler is NOT a second ingestion path. It reads the Alertmanager v2
// API, produces Observations, and feeds them to the same state machine the
// webhook feeds; there is exactly one write path into alerts (§G.8).
type ReconcileResult struct {
	SourceID   uuid.UUID
	OK         bool
	StartedAt  time.Time
	FinishedAt time.Time

	// Observed is how many alerts the v2 API returned in this pass.
	Observed int
	// SuppressedObserved is how many were reported suppressed. This is the ONLY
	// way suppression can be observed at all — Alertmanager's mute stage drops
	// silenced, inhibited and muted alerts before any webhook fires (C1).
	SuppressedObserved int
	// Recovered were present upstream and missing in oto: a webhook was missed and
	// has now been repaired.
	Recovered int
	// MissingUpstream are open in oto and absent upstream. They are CANDIDATES for
	// expiry only — the reaper still applies its grace period, and only when the
	// source is healthy (§B.4).
	MissingUpstream int
	// DivergenceCount is where oto and Alertmanager disagreed. It is the canary
	// for every correctness bug in the system (§G.8.4).
	DivergenceCount int

	// Error is the human, always-safe-to-render failure, or "".
	Error string
}
