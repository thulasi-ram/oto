package api

import (
	"time"

	"github.com/google/uuid"
)

// The wire DTOs of the Alerts, Occurrences and Discovery tags.
//
// ⛔ THE THREE-MODEL RULE (CONTEXT.md §5.5). These structs are the ONLY shape
// that reaches a client. A repository row never appears in a handler and a domain
// entity is never marshalled: every field below is copied across explicitly in
// map.go, so that renaming a column cannot silently rename a JSON field.
//
// Every json tag, every bound and every enum here is byte-identical to
// api/openapi/openapi.yaml. If this file and that document disagree, that is a
// build failure, not a documentation nit.

// AlertDTO renders `AlertDTO`: the identity of a label set within (org, cluster).
type AlertDTO struct {
	ID                uuid.UUID         `json:"id"`
	AlertKey          string            `json:"alert_key"`
	SourceFingerprint string            `json:"source_fingerprint"`
	AlertName         string            `json:"alertname"`
	Severity          *string           `json:"severity"`
	Namespace         *string           `json:"namespace"`
	Service           *string           `json:"service"`
	ClusterKey        string            `json:"cluster_key"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	GeneratorURL      *string           `json:"generator_url"`
	State             string            `json:"state"`
	AckState          string            `json:"ack_state"`
	FirstSeenAt       time.Time         `json:"first_seen_at"`
	LastSeenAt        time.Time         `json:"last_seen_at"`
	LastStateChangeAt time.Time         `json:"last_state_change_at"`
	TotalOccurrences  int32             `json:"total_occurrences"`
	FlapScore         float32           `json:"flap_score"`
	IsFlapping        bool              `json:"is_flapping"`

	// The three `include=` sub-resources. Absent unless the caller asked, which
	// is what keeps the list one query instead of N+1.
	CurrentOccurrence *OccurrenceDTO   `json:"current_occurrence,omitempty"`
	Enrichments       []EnrichmentDTO  `json:"enrichments,omitempty"`
	Rule              *RuleSnapshotRef `json:"rule,omitempty"`
}

// AlertDetailDTO renders `AlertDetailDTO`.
//
// `rule`, `source` and `group` are declared by the contract but are NOT embedded
// here: they belong to `rules`, `sources` and `grouping`, and CONTEXT.md §5.4
// forbids this package from naming another domain's types. They are served whole
// by `/alerts/{id}/rule` and `/alert-groups/{id}`, and each is an optional
// property of the schema rather than a required one.
type AlertDetailDTO struct {
	AlertDTO
	CurrentOccurrence *OccurrenceDTO         `json:"current_occurrence"`
	EnrichmentSummary []EnrichmentSummaryDTO `json:"enrichment_summary"`
	DeliverySummary   *DeliverySummaryDTO    `json:"delivery_summary,omitempty"`
	Snooze            *SnoozeDTO             `json:"snooze,omitempty"`
}

// RuleSnapshotRef is the pointer this package may carry to a rule snapshot.
//
// The full `RuleSnapshotDTO` is owned by `rules/api`, which is the only package
// permitted to name `rules/domain`. Carrying the id here lets the alert list
// satisfy `include=rule` without inverting the dependency direction.
type RuleSnapshotRef struct {
	ID uuid.UUID `json:"id"`
}

// OccurrenceDTO renders `OccurrenceDTO`: one contiguous firing episode.
type OccurrenceDTO struct {
	ID                uuid.UUID        `json:"id"`
	AlertID           uuid.UUID        `json:"alert_id"`
	GroupID           *uuid.UUID       `json:"group_id"`
	Seq               int32            `json:"seq"`
	State             string           `json:"state"`
	SuppressionReason *string          `json:"suppression_reason"`
	SuppressedBy      *SuppressedByDTO `json:"suppressed_by,omitempty"`
	AckState          string           `json:"ack_state"`
	AckedByLabel      *string          `json:"acked_by_label"`
	AckedAt           *time.Time       `json:"acked_at"`
	AckNote           *string          `json:"ack_note"`
	StartedAt         time.Time        `json:"started_at"`
	EndedAt           *time.Time       `json:"ended_at"`
	LastObservedAt    time.Time        `json:"last_observed_at"`
	SourceStartsAt    time.Time        `json:"source_starts_at"`
	SourceEndsAt      *time.Time       `json:"source_ends_at"`
	DurationSeconds   *float64         `json:"duration_seconds"`
	ResolveReason     *string          `json:"resolve_reason"`
	ReopenCount       int32            `json:"reopen_count"`
	ReopenOf          *uuid.UUID       `json:"reopen_of"`
	RuleSnapshotID    *uuid.UUID       `json:"rule_snapshot_id"`
	Value             *float64         `json:"value"`
	ObservedSkewMS    int64            `json:"observed_skew_ms"`
}

// SuppressedByDTO renders the upstream ids responsible for suppression.
type SuppressedByDTO struct {
	SilencedBy  []string `json:"silenced_by,omitempty"`
	InhibitedBy []string `json:"inhibited_by,omitempty"`
	MutedBy     []string `json:"muted_by,omitempty"`
}

// OccurrenceDetailDTO renders `OccurrenceDetailDTO`.
//
// `group` and `rule` are optional properties of the contract schema and are
// deliberately not embedded — see AlertDetailDTO. `/occurrences/{id}/rule` serves
// the snapshot whole.
type OccurrenceDetailDTO struct {
	OccurrenceDTO
	Alert           *AlertRefDTO        `json:"alert,omitempty"`
	Enrichments     []EnrichmentDTO     `json:"enrichments"`
	DeliverySummary *DeliverySummaryDTO `json:"delivery_summary,omitempty"`
}

// AlertRefDTO renders `AlertRefDTO`: a compact Alert reference.
type AlertRefDTO struct {
	ID         uuid.UUID `json:"id"`
	AlertKey   string    `json:"alert_key"`
	AlertName  string    `json:"alertname"`
	Severity   *string   `json:"severity"`
	Namespace  *string   `json:"namespace"`
	ClusterKey string    `json:"cluster_key"`
	State      string    `json:"state"`
	AckState   string    `json:"ack_state"`
}

// AlertEventDTO renders `AlertEventDTO`: one immutable thing that happened at one
// instant.
//
// The two timestamps are never conflated. `occurred_at` is the upstream claim and
// is what the UI displays; `recorded_at` is oto's own clock and is what the
// timeline is ordered by (SPEC C12).
type AlertEventDTO struct {
	ID           uuid.UUID      `json:"id"`
	Seq          *int64         `json:"seq"`
	AlertID      *uuid.UUID     `json:"alert_id"`
	OccurrenceID *uuid.UUID     `json:"occurrence_id"`
	GroupID      *uuid.UUID     `json:"group_id"`
	Type         string         `json:"type"`
	OccurredAt   time.Time      `json:"occurred_at"`
	RecordedAt   time.Time      `json:"recorded_at"`
	ActorKind    string         `json:"actor_kind"`
	ActorID      *string        `json:"actor_id"`
	ActorLabel   *string        `json:"actor_label"`
	Summary      string         `json:"summary"`
	Payload      map[string]any `json:"payload,omitempty"`
}

// EnrichmentDTO renders `EnrichmentDTO`: one provenanced enricher result.
type EnrichmentDTO struct {
	ID              uuid.UUID      `json:"id"`
	SubjectKind     string         `json:"subject_kind"`
	SubjectID       uuid.UUID      `json:"subject_id"`
	Enricher        string         `json:"enricher"`
	EnricherVersion int32          `json:"enricher_version"`
	Phase           int32          `json:"phase"`
	Status          string         `json:"status"`
	Payload         map[string]any `json:"payload"`
	Warnings        []string       `json:"warnings,omitempty"`
	Error           *string        `json:"error"`
	DurationMS      int32          `json:"duration_ms"`
	FromCache       bool           `json:"from_cache"`
	ComputedAt      time.Time      `json:"computed_at"`
	ExpiresAt       *time.Time     `json:"expires_at"`
}

// EnrichmentSummaryDTO renders `EnrichmentSummaryDTO`.
type EnrichmentSummaryDTO struct {
	Enricher        string    `json:"enricher"`
	EnricherVersion int32     `json:"enricher_version,omitempty"`
	Status          string    `json:"status"`
	Headline        *string   `json:"headline"`
	ComputedAt      time.Time `json:"computed_at"`
}

// DeliverySummaryDTO renders `DeliverySummaryDTO`.
//
// ⭐ It is not decoration. oto's silence must never be indistinguishable from
// "no alert fired", so every alert can show whether its notifications landed, and
// a non-zero `dead` count is a product signal rather than a footnote.
type DeliverySummaryDTO struct {
	Total          int32      `json:"total"`
	Sent           int32      `json:"sent"`
	Failed         int32      `json:"failed"`
	Dead           int32      `json:"dead"`
	Skipped        int32      `json:"skipped"`
	Pending        int32      `json:"pending"`
	LastErrorClass *string    `json:"last_error_class,omitempty"`
	LastSentAt     *time.Time `json:"last_sent_at,omitempty"`
}

// NotificationDTO renders `NotificationDTO` for `listAlertNotifications`.
type NotificationDTO struct {
	ID               uuid.UUID           `json:"id"`
	SubjectKind      string              `json:"subject_kind"`
	SubjectID        uuid.UUID           `json:"subject_id"`
	GroupID          uuid.UUID           `json:"group_id"`
	AlertID          *uuid.UUID          `json:"alert_id"`
	OccurrenceID     *uuid.UUID          `json:"occurrence_id"`
	Reason           string              `json:"reason"`
	StateVersion     int32               `json:"state_version"`
	Status           string              `json:"status"`
	SuppressedReason *string             `json:"suppressed_reason"`
	DeliverySummary  *DeliverySummaryDTO `json:"delivery_summary,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// SnoozeDTO renders the §B.8 snooze in force on an Alert.
//
// A snooze is a fact about OTO'S NOTIFICATIONS and never about the signal: the
// alert is still firing, still whatever severity it was, and the detail page
// keeps rendering it that way.
type SnoozeDTO struct {
	ID             uuid.UUID  `json:"id"`
	SnoozedAt      time.Time  `json:"snoozed_at"`
	SnoozedUntil   time.Time  `json:"snoozed_until"`
	SnoozedByLabel string     `json:"snoozed_by_label"`
	Note           *string    `json:"note"`
	EndedAt        *time.Time `json:"ended_at"`
}

// LabelNameDTO renders `LabelNameDTO` for the filter bar.
type LabelNameDTO struct {
	Name       string `json:"name"`
	AlertCount int32  `json:"alert_count,omitempty"`
	Promoted   bool   `json:"promoted"`
}

// LabelValueDTO renders `LabelValueDTO` for typeahead.
type LabelValueDTO struct {
	Value      string `json:"value"`
	AlertCount int32  `json:"alert_count,omitempty"`
}

// ------------------------------------------------------------------ requests

// AckRequest is the body of `POST /alerts/{id}/ack`.
//
// An acked alert is STILL FIRING. Acknowledgement is an orthogonal axis that says
// "a human has seen this", never "this is over".
type AckRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// UnackRequest is the body of `POST /alerts/{id}/unack`.
type UnackRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// CommentRequest is the body of `POST /alerts/{id}/comments`.
type CommentRequest struct {
	Body string `json:"body" validate:"required,notblank,min=1,max=10000"`
}

// ------------------------------------------------------------- query objects

// ListAlertsQuery is the validated form of the `listAlerts` query string.
//
// It is a DTO like any other and carries the contract's bounds as `validate`
// tags, because layer 1 exists to produce a good error message wherever the
// values came from — a query string is not a lesser input.
type ListAlertsQuery struct {
	State     []string   `json:"state"      validate:"omitempty,max=4,unique,dive,oneof=firing suppressed resolved expired"`
	Severity  []string   `json:"severity"   validate:"omitempty,max=16,unique,dive,max=4096"`
	Cluster   []string   `json:"cluster"    validate:"omitempty,max=32,unique,dive,clusterkey"`
	Namespace []string   `json:"namespace"  validate:"omitempty,max=64,unique,dive,max=4096"`
	AlertName []string   `json:"alertname"  validate:"omitempty,max=64,unique,dive,max=1024"`
	Ack       string     `json:"ack"        validate:"omitempty,oneof=unacked acked"`
	Flapping  *bool      `json:"flapping"`
	Since     *time.Time `json:"since"`
	Q         string     `json:"q"          validate:"omitempty,max=200"`
	Sort      string     `json:"sort"       validate:"omitempty,oneof=-last_seen_at -first_seen_at"`
	Include   []string   `json:"include"    validate:"omitempty,max=3,unique,dive,oneof=current_occurrence enrichments rule"`
	Limit     int        `json:"limit"      validate:"min=1,max=200"`
	Cursor    string     `json:"cursor"     validate:"omitempty,cursor"`
	SinceSeq  int64      `json:"since_seq"  validate:"min=0"`
}

// TimelineQuery is the validated form of the event-list query string shared by
// `listAlertEvents`, `listOccurrenceEvents` and `getAlertGroupTimeline`.
type TimelineQuery struct {
	Type     []string   `json:"type"      validate:"omitempty,max=34,unique"`
	Since    *time.Time `json:"since"`
	Until    *time.Time `json:"until"`
	Order    string     `json:"order"     validate:"omitempty,oneof=asc desc"`
	Limit    int        `json:"limit"     validate:"min=1,max=200"`
	Cursor   string     `json:"cursor"    validate:"omitempty,cursor"`
	SinceSeq int64      `json:"since_seq" validate:"min=0"`
}

// LabelQuery is the validated form of the two Discovery list queries.
type LabelQuery struct {
	Q     string `json:"q"     validate:"omitempty,max=200"`
	Limit int    `json:"limit" validate:"min=1,max=200"`
}
