package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The DTOs of the Sources tag. They live in `api` and NOWHERE ELSE (CONTEXT.md
// §5.5): a DTO may not embed a domain type or a row type, and a domain type may
// not carry a `json` tag. The three model sets are kept apart so that the
// compiler can tell them apart.
//
// Every bound below is mirrored in three places — this tag, the domain
// constructor, and the DDL CHECK — and they must be IDENTICAL (R9).

// ---------------------------------------------------------------- responses

// ClusterDTO is one identity/failure domain.
type ClusterDTO struct {
	ID          uuid.UUID `json:"id"`
	ClusterKey  string    `json:"cluster_key"`
	DisplayName string    `json:"display_name"`
	SourceCount int32     `json:"source_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SourceDTO is one configured upstream.
//
// ⛔ Credentials never appear here. The ingest token is returned exactly once, on
// creation or rotation, in a SourceCreatedDTO — this struct has no field that
// could carry one.
type SourceDTO struct {
	ID         uuid.UUID `json:"id"`
	ClusterID  uuid.UUID `json:"cluster_id"`
	ClusterKey string    `json:"cluster_key,omitempty"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`

	BaseURL       string  `json:"base_url"`
	PrometheusURL *string `json:"prometheus_url"`
	TLSSkipVerify bool    `json:"tls_skip_verify"`

	InjectLabels      map[string]string `json:"inject_labels"`
	IgnoreLabels      []string          `json:"ignore_labels"`
	RedactLabels      []string          `json:"redact_labels"`
	RedactAnnotations []string          `json:"redact_annotations"`

	PushEnabled              bool  `json:"push_enabled"`
	ReconcileEnabled         bool  `json:"reconcile_enabled"`
	ReconcileIntervalSeconds int32 `json:"reconcile_interval_seconds"`

	// IngestPath is the exact path to configure in this source's Alertmanager
	// `webhook_config`.
	IngestPath string `json:"ingest_path"`

	Health *SourceHealthDTO `json:"health"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SourceCreatedDTO is the ONLY response in this API that ever contains a secret.
type SourceCreatedDTO struct {
	Source SourceDTO `json:"source"`
	// IngestToken is returned EXACTLY ONCE and is never retrievable again: only a
	// sha256 of it is stored. Losing it means rotating.
	IngestToken string `json:"ingest_token"`
	// TokenPrefix is the secret's kind literal plus four characters, retained for
	// display so the token can be identified later without being recoverable.
	// `oto_ingest_` is eleven characters, so an ingest prefix is fifteen — not the
	// twelve a PAT's is.
	TokenPrefix string `json:"token_prefix,omitempty"`
	// WebhookURL is the absolute URL to paste into `webhook_config.url`.
	WebhookURL string `json:"webhook_url,omitempty"`
}

// HealthWarningDTO is one standing, non-fatal operator warning.
type HealthWarningDTO struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Subject string     `json:"subject,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
}

// SourceHealthDTO is liveness, lag, skew and divergence for one source.
//
// This is not decoration: HEALTH GATES THE REAPER (§B.4). While `status` is
// anything other than `healthy`, occurrences are held in place and cannot be
// expired, so killing Alertmanager can never make oto conclude that every alert
// went away.
type SourceHealthDTO struct {
	SourceID uuid.UUID `json:"source_id"`
	Status   string    `json:"status"`

	LastPushAt          *time.Time `json:"last_push_at"`
	LastReconcileAt     *time.Time `json:"last_reconcile_at"`
	LastReconcileStatus *string    `json:"last_reconcile_status"`
	LastError           *string    `json:"last_error"`
	ConsecutiveFailures int32      `json:"consecutive_failures"`

	AMVersion *string `json:"am_version"`
	// SendResolved is nil when oto has not yet observed it. `false` is a standing
	// warning: that receiver will never tell oto an alert resolved, so every alert
	// routed through it EXPIRES rather than resolves.
	SendResolved *bool `json:"send_resolved"`
	// ClockSkewMS is measured and surfaced, NEVER corrected away — which is why
	// every event carries two timestamps (C12).
	ClockSkewMS int64 `json:"clock_skew_ms"`
	// DivergenceCount is the canary for every correctness bug in the system.
	DivergenceCount int32 `json:"divergence_count"`

	Warnings  []HealthWarningDTO `json:"warnings"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// SourceTestDTO is the result of probing a source's status endpoint.
//
// A `200` with `ok: false` is the normal reporting of an unreachable upstream:
// the PROBE succeeded in discovering that the source is down.
type SourceTestDTO struct {
	OK           bool    `json:"ok"`
	AMVersion    *string `json:"am_version"`
	ClusterState *string `json:"cluster_status"`
	ClusterPeers *int32  `json:"cluster_peers"`

	ServerTime  *time.Time `json:"server_time"`
	ClockSkewMS *int64     `json:"clock_skew_ms"`

	// SendResolved maps receiver name to its effective `send_resolved`. Any
	// `false` here is a standing warning (C15).
	SendResolved map[string]bool `json:"send_resolved,omitempty"`

	PrometheusOK *bool     `json:"prometheus_ok"`
	Error        *string   `json:"error"`
	CheckedAt    time.Time `json:"checked_at"`
}

// ReconcileResultDTO is the outcome of one forced reconcile pass.
type ReconcileResultDTO struct {
	SourceID   uuid.UUID `json:"source_id"`
	OK         bool      `json:"ok"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	Observed int32 `json:"observed"`
	// SuppressedObserved is the ONLY way suppression can be observed at all: the
	// webhook path can never produce it.
	SuppressedObserved int32 `json:"suppressed_observed"`
	Recovered          int32 `json:"recovered"`
	// MissingUpstream are CANDIDATES for expiry only; the reaper still applies its
	// grace period, and only when the source is healthy.
	MissingUpstream int32 `json:"missing_upstream"`
	DivergenceCount int32 `json:"divergence_count"`

	Error *string `json:"error"`
}

// ----------------------------------------------------------------- requests

// CredentialInputDTO is secret material for an upstream.
//
// ⛔ WRITE-ONLY. No endpoint in this API ever returns it and nothing logs it.
// Supplying it replaces any existing credential and stamps a new rotation
// timestamp. There is deliberately no corresponding response DTO.
type CredentialInputDTO struct {
	Kind string `json:"kind" validate:"required,oneof=slack_bot_token slack_app_token slack_signing_secret basic bearer none"`
	// Values is at most 8 entries of at most 4096 bytes, sealed with AES-256-GCM
	// before it touches disk.
	Values map[string]string `json:"values,omitempty" validate:"omitempty,max=8,dive,max=4096"`
}

// CreateClusterRequest creates an identity/failure domain.
type CreateClusterRequest struct {
	// ClusterKey participates in ALERT IDENTITY and cannot be changed afterwards.
	ClusterKey  string `json:"cluster_key"  validate:"required,clusterkey"`
	DisplayName string `json:"display_name" validate:"required,notblank,min=1,max=120"`
}

// UpdateClusterRequest is the partial update.
//
// ⛔ `cluster_key` is deliberately absent because it is immutable — changing it
// would re-key every alert identity in the cluster.
type UpdateClusterRequest struct {
	DisplayName *string `json:"display_name,omitempty" validate:"omitempty,notblank,min=1,max=120"`
}

// IsEmpty reports whether the request asks for nothing. The contract marks the
// body `minProperties: 1`.
func (r UpdateClusterRequest) IsEmpty() bool { return r.DisplayName == nil }

// CreateSourceRequest registers an Alertmanager.
type CreateSourceRequest struct {
	Name      string    `json:"name"       validate:"required,notblank,min=1,max=120"`
	ClusterID uuid.UUID `json:"cluster_id" validate:"required"`
	Kind      string    `json:"kind"       validate:"required,oneof=alertmanager grafana"`
	// BaseURL is an absolute http(s) URL with NO trailing slash. The `httpurl`
	// rule mirrors both halves of alert_sources_base_ck, so a URL pasted out of a
	// browser bar fails here with a field violation rather than at layer 6 as a
	// 23514 that names nothing (§L.2.4).
	BaseURL       string `json:"base_url"                 validate:"required,max=2048,httpurl"`
	PrometheusURL string `json:"prometheus_url,omitempty" validate:"omitempty,max=2048,httpurl"`

	TLSSkipVerify *bool `json:"tls_skip_verify,omitempty"`

	InjectLabels      map[string]string `json:"inject_labels,omitempty"      validate:"omitempty,max=64,dive,max=4096"`
	IgnoreLabels      []string          `json:"ignore_labels,omitempty"      validate:"omitempty,max=64,unique,dive,labelname,max=1024"`
	RedactLabels      []string          `json:"redact_labels,omitempty"      validate:"omitempty,max=64,dive,max=256"`
	RedactAnnotations []string          `json:"redact_annotations,omitempty" validate:"omitempty,max=64,dive,max=256"`

	PushEnabled              *bool  `json:"push_enabled,omitempty"`
	ReconcileEnabled         *bool  `json:"reconcile_enabled,omitempty"`
	ReconcileIntervalSeconds *int32 `json:"reconcile_interval_seconds,omitempty" validate:"omitempty,min=10,max=3600"`

	Credential *CredentialInputDTO `json:"credential,omitempty" validate:"omitempty"`
}

// UpdateSourceRequest is the partial update.
//
// `kind` is absent because changing an Alertmanager into a Grafana would
// reinterpret every payload already stored against it.
//
// ⚠️ `ignore_labels` feeds the alert-identity hash. Changing it does NOT re-key
// existing alerts — new identities are created from that point forward, which is
// documented behaviour rather than a defect (§C.2).
type UpdateSourceRequest struct {
	Name      *string    `json:"name,omitempty"       validate:"omitempty,notblank,min=1,max=120"`
	ClusterID *uuid.UUID `json:"cluster_id,omitempty"`
	BaseURL   *string    `json:"base_url,omitempty"   validate:"omitempty,max=2048,httpurl"`
	// PrometheusURL is `["string","null"]` in the contract: an explicit null
	// CLEARS it, which is different from omitting the field. Its `httpurl` bound
	// is enforced in toPatch, because a custom unmarshaller has no field for a
	// validator tag to hang on and a nested one would report the wrong path.
	PrometheusURL NullableString `json:"prometheus_url,omitempty"`

	TLSSkipVerify *bool `json:"tls_skip_verify,omitempty"`

	InjectLabels      *map[string]string `json:"inject_labels,omitempty"      validate:"omitempty,max=64,dive,max=4096"`
	IgnoreLabels      *[]string          `json:"ignore_labels,omitempty"      validate:"omitempty,max=64,unique,dive,labelname,max=1024"`
	RedactLabels      *[]string          `json:"redact_labels,omitempty"      validate:"omitempty,max=64,dive,max=256"`
	RedactAnnotations *[]string          `json:"redact_annotations,omitempty" validate:"omitempty,max=64,dive,max=256"`

	PushEnabled              *bool  `json:"push_enabled,omitempty"`
	ReconcileEnabled         *bool  `json:"reconcile_enabled,omitempty"`
	ReconcileIntervalSeconds *int32 `json:"reconcile_interval_seconds,omitempty" validate:"omitempty,min=10,max=3600"`

	Credential *CredentialInputDTO `json:"credential,omitempty" validate:"omitempty"`
}

// IsEmpty reports whether the request asks for nothing. The contract marks the
// body `minProperties: 1`, and a PATCH that changes nothing but reports success
// is a PATCH whose author will believe something changed.
func (r UpdateSourceRequest) IsEmpty() bool {
	return r.Name == nil && r.ClusterID == nil && r.BaseURL == nil && !r.PrometheusURL.Set &&
		r.TLSSkipVerify == nil && r.InjectLabels == nil && r.IgnoreLabels == nil &&
		r.RedactLabels == nil && r.RedactAnnotations == nil && r.PushEnabled == nil &&
		r.ReconcileEnabled == nil && r.ReconcileIntervalSeconds == nil && r.Credential == nil
}

// NullableString is a contract field typed `["string","null"]`, where an explicit
// `null` means CLEAR and an omitted field means LEAVE ALONE.
//
// A plain `*string` cannot express both: JSON `null` and an absent key both
// decode to a nil pointer. Clearing a `prometheus_url` is a real operation — it
// turns off Prometheus-sourced rule snapshots — so the two have to stay
// distinguishable, and doing it with a custom unmarshaller keeps `httpx.Bind` the
// ONLY door a body comes through (CONTEXT.md §5b).
type NullableString struct {
	// Set is true when the key was present at all, whatever its value.
	Set bool
	// Value is the supplied string. It is "" for an explicit null.
	Value string
}

// UnmarshalJSON records presence as well as value.
func (n *NullableString) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = ""
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}

// MarshalJSON renders the field back, for symmetry. An unset value is null.
func (n NullableString) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Value == "" {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// Cleared reports an explicit request to remove the value.
func (n NullableString) Cleared() bool { return n.Set && n.Value == "" }

// Supplied reports an explicit request to set the value.
func (n NullableString) Supplied() bool { return n.Set && n.Value != "" }
