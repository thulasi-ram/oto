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

	// Snooze is the §B.8 quiet period in force, or an explicit `null`.
	//
	// ⭐ IT IS ON THE LIST ROW AND NOT ONLY ON THE DETAIL VIEW, and it has NO
	// omitempty. The default list includes snoozed alerts (§B.8.6) — hiding one
	// is how an incident is lost — so a row that could not say it was snoozed
	// left the operator with no way to tell a quiet alert from a noisy one
	// except by opening every one of them. `null` says "awake"; an absent key
	// would say "unknown", and those are different facts.
	//
	// ⛔ It sits BESIDE `state` and `ack_state` and never inside them. The three
	// are orthogonal axes: a snoozed critical is still critical and still
	// firing, and the row must keep rendering it that way, with the snooze as a
	// separate `:zzz:` badge and countdown.
	Snooze *SnoozeDTO `json:"snooze"`

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
//
// `snooze` is NOT redeclared here. It is now a field of AlertDTO — the list row
// needs it too (§B.8.6) — and the contract carries it in the same place, on the
// base schema that `AlertDetailDTO` composes with `allOf`. A second declaration
// at this depth would still marshal correctly, because Go's shallower field
// wins, but two fields with one JSON name is exactly the drift this file's
// hand-copied mapping exists to prevent.
type AlertDetailDTO struct {
	AlertDTO
	CurrentOccurrence *OccurrenceDTO         `json:"current_occurrence"`
	EnrichmentSummary []EnrichmentSummaryDTO `json:"enrichment_summary"`
	// DeliverySummary is NOT a pointer and carries NO omitempty, and both are
	// deliberate. The field spent its whole life declared here and emitted
	// nowhere: optional in the schema, so every validator passed and the absence
	// was invisible. A value type makes omitting it structurally impossible.
	DeliverySummary DeliverySummaryDTO `json:"delivery_summary"`
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
	Alert       *AlertRefDTO    `json:"alert,omitempty"`
	Enrichments []EnrichmentDTO `json:"enrichments"`
	// DeliverySummary is a value type for the same reason as on AlertDetailDTO:
	// an optional field that nothing emitted is a contract that lies quietly.
	DeliverySummary DeliverySummaryDTO `json:"delivery_summary"`
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
//
// ⛔ THERE IS NO `seq`. It was declared as the `ui_events.seq` a polling client
// echoes back in `?since_seq=`, and it was set by nothing — while no operation
// returning this DTO accepts `since_seq` either. A client following the
// documented resume protocol read `null` forever and could never advance its
// cursor. Timelines page by `cursor`, which is served.
type AlertEventDTO struct {
	ID           uuid.UUID      `json:"id"`
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
//
// `policy_id` and `updated_at` are NULLABLE and are null when the read model has
// nothing to report — never substituted from a neighbouring field.
type NotificationDTO struct {
	ID               uuid.UUID           `json:"id"`
	SubjectKind      string              `json:"subject_kind"`
	SubjectID        uuid.UUID           `json:"subject_id"`
	GroupID          uuid.UUID           `json:"group_id"`
	AlertID          *uuid.UUID          `json:"alert_id"`
	OccurrenceID     *uuid.UUID          `json:"occurrence_id"`
	PolicyID         *uuid.UUID          `json:"policy_id"`
	Reason           string              `json:"reason"`
	StateVersion     int32               `json:"state_version"`
	Status           string              `json:"status"`
	SuppressedReason *string             `json:"suppressed_reason"`
	DeliverySummary  *DeliverySummaryDTO `json:"delivery_summary,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        *time.Time          `json:"updated_at"`
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

// SnoozeHistoryDTO renders one row of an Alert's snooze history.
//
// ⭐ Membership of a snooze is HISTORY, not a boolean (§B.8.6). An ended snooze
// keeps its row, with who asked for it, until when, and how it finished — which
// is what makes the feature auditable and therefore safe. `ended_reason` is one
// of manual | expired | superseded and is never invented: a snooze that was
// replaced by a longer one and a snooze someone cancelled are different facts.
type SnoozeHistoryDTO struct {
	SnoozeDTO
	EndedReason  *string `json:"ended_reason"`
	EndedByLabel *string `json:"ended_by_label"`
	Active       bool    `json:"active"`
}

// ActiveSnoozeDTO renders one row of the §B.8.6 ORG-WIDE view: something oto is
// currently quiet about, and until when.
//
// ⭐ WHY THIS IS NOT `GET /alerts?snoozed=true`. That endpoint pages ALERTS. It
// can say *which* alerts are quiet and it structurally cannot say who asked, why,
// or until when, because those are facts about an `alert_snoozes` row and one
// alert has a whole history of them. §B.8.6 requires a persistent banner
// enumerating every active snooze with its expiry, "so a snooze cannot be
// forgotten" — that is the counterweight that makes the whole feature safe, and
// a list of alerts is not it.
//
// The Alert is carried as a REFERENCE and never inlined whole: this row is about
// the snooze, and the alert is how a human recognises which one.
type ActiveSnoozeDTO struct {
	SnoozeDTO
	AlertID  uuid.UUID `json:"alert_id"`
	AlertKey string    `json:"alert_key"`
	// Alert is null when the Alert could not be read. The snooze is still
	// listed: a quiet period whose subject is unreadable is still a quiet period
	// somebody has to know about, and dropping the row would hide it.
	Alert *AlertRefDTO `json:"alert"`
	// RemainingSeconds is the countdown, measured against OTO'S clock rather than
	// the caller's, so a client with a skewed clock cannot render a badge that
	// disagrees with the server about what is muted. Never negative.
	RemainingSeconds float64 `json:"remaining_seconds"`
}

// LabelNameDTO renders `LabelNameDTO` for the filter bar.
type LabelNameDTO struct {
	Name       string `json:"name"`
	AlertCount int32  `json:"alert_count"`
	Promoted   bool   `json:"promoted"`
}

// LabelValueDTO renders `LabelValueDTO` for typeahead.
type LabelValueDTO struct {
	Value      string `json:"value"`
	AlertCount int32  `json:"alert_count"`
}

// AlertRollupDTO renders `AlertRollupDTO`: one bucket of the §E.3a aggregation.
//
// ⛔ It is a VIEW and NEVER an AlertGroup. An AlertGroup is one generation of one
// Alertmanager notification group — it has a row, a generation and a chat thread.
// A roll-up bucket has none of those and exists for the duration of one query
// (§A.1). The two are separate endpoints for exactly that reason.
type AlertRollupDTO struct {
	Key             string           `json:"key"`
	GroupBy         string           `json:"group_by"`
	State           string           `json:"state"`
	TotalCount      int32            `json:"total_count"`
	FiringCount     int32            `json:"firing_count"`
	SuppressedCount int32            `json:"suppressed_count"`
	ResolvedCount   int32            `json:"resolved_count"`
	ExpiredCount    int32            `json:"expired_count"`
	AckedCount      int32            `json:"acked_count"`
	UnackedCount    int32            `json:"unacked_count"`
	FlappingCount   int32            `json:"flapping_count"`
	SnoozedCount    int32            `json:"snoozed_count"`
	SeverityCounts  map[string]int32 `json:"severity_counts"`
	FirstSeenAt     time.Time        `json:"first_seen_at"`
	LastSeenAt      time.Time        `json:"last_seen_at"`
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

// SnoozeRequest is the body of `POST /alerts/{id}/snooze` (§B.8.3).
//
// ⛔ EXACTLY ONE of `until` and `duration_seconds` is required, and NEITHER may
// be absent. There is no indefinite snooze: an unexpiring snooze is a mute, and
// mutes are how channels die. The bounds are 5 minutes to 30 days and they are
// identical here, in domain.StartSnooze and in `snoozes_window_ck`.
//
// A snooze changes nothing about the signal. The alert stays firing, stays
// whatever severity it was, and is NOT hidden from the default list (§B.8.6).
type SnoozeRequest struct {
	// Until is the absolute instant the snooze ends.
	Until *time.Time `json:"until"`
	// DurationSeconds is the relative form the presets use: 30 m, 1 h, 4 h, 24 h,
	// 7 d. It is resolved against oto's clock, never the caller's, so a client
	// with a skewed clock cannot ask for a snooze outside the bounds.
	DurationSeconds *int64 `json:"duration_seconds" validate:"omitempty,min=300,max=2592000"`
	// Note is why. Optional, and shown wherever the snooze is shown.
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// UnsnoozeRequest is the body of `POST /alerts/{id}/unsnooze`. It carries
// nothing today and exists so the verb can gain a reason without a breaking
// change.
type UnsnoozeRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// ------------------------------------------------------------- query objects

// ListAlertsQuery is the validated form of the `listAlerts` query string.
//
// It is a DTO like any other and carries the contract's bounds as `validate`
// tags, because layer 1 exists to produce a good error message wherever the
// values came from — a query string is not a lesser input.
type ListAlertsQuery struct {
	State     []string `json:"state"      validate:"omitempty,max=4,unique,dive,oneof=firing suppressed resolved expired"`
	Severity  []string `json:"severity"   validate:"omitempty,max=16,unique,dive,max=4096"`
	Cluster   []string `json:"cluster"    validate:"omitempty,max=32,unique,dive,clusterkey"`
	Namespace []string `json:"namespace"  validate:"omitempty,max=64,unique,dive,max=4096"`
	AlertName []string `json:"alertname"  validate:"omitempty,max=64,unique,dive,max=1024"`
	// SourceFingerprint is the §C.3 axis, and it exists so a
	// `group_by=fingerprint` roll-up bucket can be OPENED. The other two axes —
	// alertname and namespace — have always had their filter; without this one
	// the fingerprint bucket could be counted and never drilled into, which is
	// the one thing a roll-up exists to let a user do.
	// The charset is proven by domain.NewSourceFingerprint rather than by a tag:
	// `platform/validate` registers no `fingerprint` rule, and the C.3 pattern
	// already lives in the domain constructor, where re-spelling it as a regex
	// here would make it live in two places that can disagree.
	SourceFingerprint []string `json:"source_fingerprint" validate:"omitempty,max=64,unique,dive,len=16"`
	// Matcher is the ADR 0017 label selector in Alertmanager syntax. It is the
	// ONLY spelling that can carry `=~` and `!~`; `label[k]=v` structurally
	// cannot, which is why regex matchers were unreachable before it existed.
	Matcher  string `json:"matcher"    validate:"omitempty,max=8192"`
	Ack      string `json:"ack"        validate:"omitempty,oneof=unacked acked"`
	Flapping *bool  `json:"flapping"`
	// Snoozed is an EXPLICIT filter and its absence means INCLUDE BOTH (§B.8.6).
	// The default list never hides a snoozed alert — hiding one is how an
	// incident is lost.
	Snoozed *bool      `json:"snoozed"`
	Since   *time.Time `json:"since"`
	Q       string     `json:"q"          validate:"omitempty,max=200"`
	Sort    string     `json:"sort"       validate:"omitempty,oneof=-last_seen_at -first_seen_at"`
	Include []string   `json:"include"    validate:"omitempty,max=3,unique,dive,oneof=current_occurrence enrichments rule"`
	Limit   int        `json:"limit"      validate:"min=1,max=200"`
	Cursor  string     `json:"cursor"     validate:"omitempty,cursor"`
}

// ListRollupsQuery is the validated form of the `listAlertRollups` query string.
//
// It repeats every filter `listAlerts` takes, deliberately: a roll-up that
// summarised a different set from the list beside it would be two answers to one
// question. What it does NOT take is `sort` or `include` — buckets have no
// last_seen_at ordering to choose and no sub-resources to embed.
type ListRollupsQuery struct {
	GroupBy   string   `json:"group_by"   validate:"required,oneof=alertname namespace fingerprint"`
	State     []string `json:"state"      validate:"omitempty,max=4,unique,dive,oneof=firing suppressed resolved expired"`
	Severity  []string `json:"severity"   validate:"omitempty,max=16,unique,dive,max=4096"`
	Cluster   []string `json:"cluster"    validate:"omitempty,max=32,unique,dive,clusterkey"`
	Namespace []string `json:"namespace"  validate:"omitempty,max=64,unique,dive,max=4096"`
	AlertName []string `json:"alertname"  validate:"omitempty,max=64,unique,dive,max=1024"`
	// SourceFingerprint is repeated from ListAlertsQuery for the same reason as
	// every other filter here: the bucket beside the list must summarise exactly
	// the list beside it.
	SourceFingerprint []string   `json:"source_fingerprint" validate:"omitempty,max=64,unique,dive,len=16"`
	Matcher           string     `json:"matcher"    validate:"omitempty,max=8192"`
	Ack               string     `json:"ack"        validate:"omitempty,oneof=unacked acked"`
	Flapping          *bool      `json:"flapping"`
	Snoozed           *bool      `json:"snoozed"`
	Since             *time.Time `json:"since"`
	Q                 string     `json:"q"          validate:"omitempty,max=200"`
	Limit             int        `json:"limit"      validate:"min=1,max=200"`
	Cursor            string     `json:"cursor"     validate:"omitempty,cursor"`
}

// TimelineQuery is the validated form of the event-list query string shared by
// `listAlertEvents`, `listOccurrenceEvents` and `getAlertGroupTimeline`.
type TimelineQuery struct {
	// The bound is the SIZE OF THE CLOSED ENUM (domain.AllEventTypes), so that
	// "give me everything" is always expressible. It was 34 while
	// `alert.snoozed` and `alert.unsnoozed` were missing from the contract; the
	// domain has always emitted 36, and a caller enumerating them all was
	// refused for asking for two types the server was already writing.
	Type   []string   `json:"type"      validate:"omitempty,max=36,unique"`
	Since  *time.Time `json:"since"`
	Until  *time.Time `json:"until"`
	Order  string     `json:"order"     validate:"omitempty,oneof=asc desc"`
	Limit  int        `json:"limit"     validate:"min=1,max=200"`
	Cursor string     `json:"cursor"    validate:"omitempty,cursor"`
}

// LabelQuery is the validated form of the two Discovery list queries.
type LabelQuery struct {
	Q     string `json:"q"     validate:"omitempty,max=200"`
	Limit int    `json:"limit" validate:"min=1,max=200"`
}
