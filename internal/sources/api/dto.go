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

	PushEnabled bool `json:"push_enabled"`
	// ReconcileIntervalSeconds is HOW OFTEN the API v2 reconciler polls this
	// source. There is deliberately no `reconcile_enabled` beside it: the
	// reconciler is the only producer of `suppressed` and the only thing that
	// refreshes `source_health`, so a source with it switched off kept a frozen
	// `healthy` verdict that the reaper went on trusting — and ended episodes for
	// alerts that were merely silenced upstream. ADR 0006's second amendment and
	// migration 00038 removed the switch; the interval is the whole knob.
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

	// RouteTimings are the source's group_wait, group_interval and
	// repeat_interval with the provenance of each: read off its running
	// configuration, supplied by Alertmanager's documented default, or unknown.
	//
	// ⭐ IT IS ALWAYS PRESENT. It used to be null until oto had parsed a config
	// once, which forced every client to write the same "have we looked yet"
	// branch and then a second one for "we looked and it said nothing". The three
	// provenance states carry both facts inside the object, so the object itself
	// never has to be absent.
	RouteTimings RouteTimingsDTO `json:"route_timings"`

	Warnings  []HealthWarningDTO `json:"warnings"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// RouteTimingDTO is ONE route timing with the provenance of its number.
//
// ⛔ THE PROVENANCE IS NOT DECORATION AND MUST BE RENDERED. `observed` and
// `default_applies` carry equally valid arithmetic — a 2m re-fire grace is just
// as unreachable under a defaulted 5m `group_interval` as under a configured one
// — but they call for different actions, so a client that renders them
// identically has thrown away the only part of this that is advice. `unknown`
// carries no number at all.
type RouteTimingDTO struct {
	// Provenance is `observed`, `default_applies` or `unknown`.
	Provenance string `json:"provenance"`
	// ValueMS is the duration in force, in milliseconds, and is null exactly when
	// Provenance is `unknown`. Milliseconds because `group_wait: 500ms` is legal
	// and seconds would round it to "notify immediately".
	ValueMS *int64 `json:"value_ms"`
}

// RouteTimingsDTO is what governs an Alertmanager's batching, and how oto knows.
//
// ⭐ IT IS OBSERVED OR DERIVED, NEVER TYPED IN. Every oto tuning knob is a
// function of these three numbers (docs/setup/tuning.md), and the tuning screen
// used to ask an operator to enter them by hand and keep the answer in one
// browser's localStorage — unshared, unvalidated, and silently wrong the moment
// somebody edited `alertmanager.yml`. These come from the source itself, on the
// status call oto already makes.
//
// ⛔ A `default_applies` FIELD IS NOT A GUESS AND IS NOT AN OBSERVATION. Absence
// from the published configuration is itself an observation, and what it implies
// is documented: Alertmanager applies 30s / 5m / 4h in `dispatch.NewRoute`.
// Reporting that as `unknown` — which oto used to do — made the common case, a
// stock install, useless, while claiming to be the careful answer.
type RouteTimingsDTO struct {
	// GroupWait, GroupInterval and RepeatInterval are the three durations IN
	// FORCE for the alerts oto is sent. `Route` says which route they came from.
	GroupWait      RouteTimingDTO `json:"group_wait"`
	GroupInterval  RouteTimingDTO `json:"group_interval"`
	RepeatInterval RouteTimingDTO `json:"repeat_interval"`
	// Route names which route the three above describe: `oto_receiver` when oto
	// identified its own receiver and every route reaching it agrees on all
	// three, `top_level` otherwise.
	//
	// ⛔ `top_level` IS THE FALLBACK AND MUST BE RENDERED AS ONE. It is what
	// governs alerts matching nothing more specific, which is a real answer and
	// is frequently not oto's: the three settings are per-route and inherited.
	// When it is `top_level` because `Routes` disagree, `RoutesAgree` is false
	// and a client MUST show the set rather than asserting these three.
	Route string `json:"route"`
	// ChildRoutes and ChildRoutesWithTimings describe the TREE, not the route
	// above: how many descendant routes exist at any depth, and how many state a
	// timing of their own. They are the shape of the config in two numbers, and
	// they are what `Routes` below expands into detail.
	ChildRoutes            int32 `json:"child_routes"`
	ChildRoutesWithTimings int32 `json:"child_routes_with_timings"`
	// Receiver is the receiver oto believes is ITS OWN, and is null whenever
	// ReceiverBasis is not `sole_webhook`.
	Receiver *string `json:"receiver"`
	// ReceiverBasis is HOW that was decided — `sole_webhook`, `ambiguous`,
	// `no_webhook` or `unknown`.
	//
	// ⛔ IT IS AN INFERENCE AND MUST NEVER RENDER AS A READING. oto's ingest path
	// is `/api/v1/ingest/alertmanager/{source_id}`, so the webhook URL in an
	// operator's config contains the id of the source oto is probing and would
	// identify oto's receiver exactly — but `webhook_config.url` is a `SecretURL`
	// and `config.original` is the marshalled config, so it arrives as the
	// literal string `<secret>`.
	ReceiverBasis string `json:"receiver_basis"`
	// WebhookReceivers is every receiver in the configuration with a webhook
	// integration. With `ambiguous` it is the candidate list, and it is what
	// makes the ambiguity actionable rather than a shrug.
	WebhookReceivers []string `json:"webhook_receivers"`
	// Routes is EVERY delivering route in the tree, in the order Alertmanager
	// evaluates them, each with its receiver, its inherited timings, its matcher
	// path and whether it reaches oto.
	//
	// ⭐ IT IS A LIST BECAUSE THE ANSWER IS A SET. `continue: true` lets several
	// routes deliver to one receiver under different matchers with different
	// timings; there may be no single triple, and a client must say "these two
	// routes reach oto and disagree" rather than picking one.
	Routes []ReceiverRouteDTO `json:"routes"`
	// RoutesAgree reports whether every route reaching oto's receiver resolves to
	// the same three durations. It is true when there is nothing to disagree —
	// no identified receiver, or exactly one reaching route — so it is only ever
	// false when there is a real conflict to show.
	RoutesAgree bool `json:"routes_agree"`
	// RoutesDropped is how many delivering routes the parser's cap discarded.
	// Non-zero means `Routes` is incomplete and must be rendered as such.
	RoutesDropped int32 `json:"routes_dropped"`
	// DefaultsFromVersion is the Alertmanager version any `default_applies` field
	// is attributed to, and is null when no field defaulted. It is the source's
	// own reported version where oto has one.
	DefaultsFromVersion *string `json:"defaults_from_version"`
	// DefaultsVerified is false when the source is newer than the release oto
	// checked Alertmanager's constants against, so a client can say "the default
	// oto last verified" rather than asserting the source's own.
	DefaultsVerified bool `json:"defaults_verified"`
	// ObservedAt is when the configuration was last read off the source, and is
	// null exactly when nothing has ever been read. It is deliberately NOT
	// `updated_at`: that moves on every probe, including ones that could not reach
	// the source, so showing it here would claim a stale reading is fresh.
	ObservedAt *time.Time `json:"observed_at"`
}

// The values RouteTimingsDTO.Route takes.
const (
	// RouteTopLevel means the three durations are the top-level route's. It is
	// the FALLBACK: what governs every alert matching nothing more specific, and
	// what oto reports when it cannot name its own receiver or when the routes
	// that reach it disagree.
	RouteTopLevel = "top_level"
	// RouteOtoReceiver means the three durations are those of the route(s)
	// delivering to oto's own receiver — the numbers actually in force for the
	// alerts oto is sent, and the only ones its tuning arithmetic should use.
	RouteOtoReceiver = "oto_receiver"
)

// InheritedTimingDTO is one route timing with its provenance AND the route on
// the path that stated it.
//
// ⭐ FromDepth IS NOT DECORATION. `group_interval: 5m` inherited from the
// top-level route and `group_interval: 5m` stated on this route send an operator
// to two different lines of their own file, and only the second survives them
// editing the child. It indexes ReceiverRouteDTO.Path, so `from_depth` equal to
// `len(path) - 1` means "this route states it itself".
type InheritedTimingDTO struct {
	// Provenance is `observed` when some route on the path stated this value and
	// `default_applies` when none did. It is never `unknown` here: a route only
	// exists in this list because oto read the configuration it came from.
	Provenance string `json:"provenance"`
	// ValueMS is the duration in force for this route, in milliseconds.
	ValueMS *int64 `json:"value_ms"`
	// FromDepth is the index into Path of the route that stated the value, and is
	// null when no route on the path stated it.
	FromDepth *int32 `json:"from_depth"`
}

// RouteStepDTO is one route on the path from the top-level route, as an operator
// would recognise it in their own file.
type RouteStepDTO struct {
	// Matchers are this route's OWN matchers, normalised into Alertmanager's
	// current `matchers` spelling and sorted, so they can be pasted into
	// `amtool`. Empty means the route states none and therefore takes everything
	// its parent gives it.
	Matchers []string `json:"matchers"`
	// Deprecated reports that the route spelled them with `match` or `match_re`.
	// Both still route production traffic; oto renders the current spelling and
	// says where it came from rather than quietly rewriting the operator's file.
	Deprecated bool `json:"deprecated"`
	// Continue is this route's `continue`. It is the reason several routes can
	// reach one receiver: evaluation does not stop at a match that sets it.
	Continue bool `json:"continue"`
}

// ReceiverRouteDTO is ONE route that delivers to a receiver, fully resolved.
//
// "Delivers" is Alertmanager's own rule from `dispatch.Route.Match`: a route is
// the answer only when NO child of it matched, so a route with a matcher-less
// child never delivers itself. Nothing here evaluates a matcher against a label
// set — every field is structural and therefore true for every alert.
type ReceiverRouteDTO struct {
	// Receiver is the receiver this route delivers to, after inheritance.
	Receiver string `json:"receiver"`
	// Path is the route chain from the top-level route (index 0) to this one, and
	// is never empty.
	Path []RouteStepDTO `json:"path"`

	GroupWait      InheritedTimingDTO `json:"group_wait"`
	GroupInterval  InheritedTimingDTO `json:"group_interval"`
	RepeatInterval InheritedTimingDTO `json:"repeat_interval"`

	// GroupBy is the effective grouping labels, inherited. GroupByAll is the
	// `group_by: ['...']` form, which is worth surfacing beside the numbers
	// because no number captures it: grouping by every label means no group ever
	// accumulates a second member, so storm collapse is unreachable at any
	// threshold.
	GroupBy    []string `json:"group_by"`
	GroupByAll bool     `json:"group_by_all"`

	// ReachesOto reports that this route delivers to the receiver oto believes is
	// its own. It is false for every route when oto could not identify one, which
	// is exactly when a client should show the whole list and say why.
	ReachesOto bool `json:"reaches_oto"`
	// Unreachable reports that an earlier matcher-less sibling without `continue`
	// consumes everything before this route is evaluated, so it can never fire.
	// It is the only unreachability provable without labels, and it is a real
	// misconfiguration rather than a display detail.
	Unreachable bool `json:"unreachable"`
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

	PushEnabled *bool `json:"push_enabled,omitempty"`
	// ⛔ NO `reconcile_enabled`. The body decodes with DisallowUnknownFields, so a
	// client that sends one is REFUSED by name rather than quietly ignored — which
	// is the point: the field used to exist, tooling and runbooks may still carry
	// it, and a silent drop would let somebody believe they had turned
	// reconciliation off while oto carried on. See ADR 0006's second amendment.
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

	PushEnabled *bool `json:"push_enabled,omitempty"`
	// ⛔ NO `reconcile_enabled`, AND ITS ABSENCE IS THE POINT OF THIS STRUCT.
	// `PATCH {"reconcile_enabled": false}` used to return 200 and persist, which
	// turned off the component ADR 0006 calls mandatory for one source, forever,
	// with nothing on any screen to say so. It now fails validation naming the
	// field. `reconcile_interval_seconds` remains: how often oto polls is a
	// legitimate operational choice, whether it polls is not.
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
		r.ReconcileIntervalSeconds == nil && r.Credential == nil
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
