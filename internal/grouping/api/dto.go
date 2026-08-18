package api

import (
	"time"

	"github.com/google/uuid"
)

// The wire DTOs of the Groups tag.
//
// ⛔ THE THREE-MODEL RULE (CONTEXT.md §5.5). Every field is copied across
// explicitly in map.go: no repository row and no domain entity is ever
// marshalled. Each json tag is byte-identical to api/openapi/openapi.yaml.

// GroupDTO renders `GroupDTO`: one generation of one Alertmanager notification
// group.
//
// A group is the unit humans actually respond to — forty pods crash-looping is
// one thing happening, not forty — and its `group_key` is oto's own durable hash,
// stable across `alertmanager.yml` route edits so an open thread is never
// orphaned by a reload.
type GroupDTO struct {
	ID              uuid.UUID         `json:"id"`
	GroupKey        string            `json:"group_key"`
	Generation      int32             `json:"generation"`
	SourceID        uuid.UUID         `json:"source_id"`
	ClusterKey      string            `json:"cluster_key"`
	SourceGroupKey  *string           `json:"source_group_key"`
	Receiver        string            `json:"receiver"`
	GroupLabels     map[string]string `json:"group_labels"`
	Title           string            `json:"title"`
	State           string            `json:"state"`
	Severity        *string           `json:"severity"`
	StateVersion    int32             `json:"state_version"`
	FiringCount     int32             `json:"firing_count"`
	SuppressedCount int32             `json:"suppressed_count"`
	ResolvedCount   int32             `json:"resolved_count"`
	ExpiredCount    int32             `json:"expired_count"`
	TotalCount      int32             `json:"total_count"`
	AckedCount      int32             `json:"acked_count"`
	// SnoozedCount is how many CURRENTLY-JOINED member alerts oto is quiet about
	// right now, and SnoozedUntil is when the last of them wakes.
	//
	// ⛔ THERE IS NO GROUP-LEVEL SNOOZE, and these two are not one. The group
	// snooze verb is a FAN-OUT of the per-alert primitive (§B.8.3) — one
	// `alert_snoozes` row per member, nothing on the group — and this is the only
	// place its result is visible. Without them the group screen could offer the
	// button and never show that it had worked.
	//
	// ⭐ Compare SnoozedCount against TotalCount rather than reading it as a
	// boolean: "one of forty is muted" and "all forty are" are different facts,
	// and a group is only wholly quiet when they are equal.
	//
	// ⛔ Neither changes how the group renders. Colour and counts follow member
	// STATE; a snoozed generation is still firing and is still listed.
	SnoozedCount           int32      `json:"snoozed_count"`
	SnoozedUntil           *time.Time `json:"snoozed_until"`
	StormMode              bool       `json:"storm_mode"`
	StormSince             *time.Time `json:"storm_since"`
	LastNotificationReason *string    `json:"last_notification_reason"`
	FirstSeenAt            time.Time  `json:"first_seen_at"`
	LastActivityAt         time.Time  `json:"last_activity_at"`
	ClosedAt               *time.Time `json:"closed_at"`
}

// GroupDetailDTO renders `GroupDetailDTO`.
//
// `source` is an optional property of the contract schema and is NOT embedded:
// it belongs to `sources`, and CONTEXT.md §5.4 forbids this package from naming
// another domain's types.
//
// `delivery_summary` IS carried, through a consumer-declared port rather than an
// import. A group generation is the thing oto actually notifies about — the
// intents are keyed on it — so a group card that could not say whether its own
// fan-out landed was the emptiest instance of a field declared four times and
// emitted nowhere.
//
// ⛔ `threads` IS GONE FROM THE CONTRACT, and this comment used to be the reason
// it survived. It said `threads` was "NOT embedded" for layering reasons — while
// no `ChannelThreadDTO` existed anywhere in oto's Go tree, so there was nothing
// to embed and nothing to port to. The UI's group screen rendered the array,
// permanently empty, which made a generation being actively discussed in Slack
// indistinguishable from one nobody had been told about. A layering note is not
// a reason to keep declaring a field; if the thread list is wanted back, it
// arrives the way `delivery_summary` did, as a port this package declares and
// the composition root satisfies.
type GroupDetailDTO struct {
	GroupDTO
	SeverityCounts map[string]int32 `json:"severity_counts"`
	TopAlerts      []AlertRefDTO    `json:"top_alerts"`
	// DeliverySummary is a value type with no omitempty: an all-zero roll-up is
	// the answer "nobody was told", which is the one an operator most needs when
	// a channel is quiet, and it must never be confused with a field the server
	// skipped.
	DeliverySummary DeliverySummaryDTO `json:"delivery_summary"`
}

// DeliverySummaryDTO renders `DeliverySummaryDTO` for a group generation.
//
// ⛔ IT IS DECLARED HERE rather than imported from `notification/api` or
// `alerts/api`, for the same reason `SnoozeDTO` below is: CONTEXT.md §5.5 gives
// each module its own wire types and §5.1 forbids one `api` package depending on
// another's. The json tags are byte-identical to the single `DeliverySummaryDTO`
// schema in openapi.yaml, which is what makes them one type on the wire without
// making them one type in Go.
type DeliverySummaryDTO struct {
	Total   int32 `json:"total"`
	Sent    int32 `json:"sent"`
	Failed  int32 `json:"failed"`
	Dead    int32 `json:"dead"`
	Skipped int32 `json:"skipped"`
	Pending int32 `json:"pending"`

	LastErrorClass *string    `json:"last_error_class,omitempty"`
	LastSentAt     *time.Time `json:"last_sent_at,omitempty"`
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
}

// AlertDTO renders `AlertDTO` for `listAlertGroupAlerts`.
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
	// ⛔ THERE IS NO `ack_state`. An Alert has no ack: the receipt belongs to the
	// firing episode it was given for. A member of a group card IS an episode —
	// membership is `alert_cases.group_id` — so the card reads ack from the
	// member's own case, and this schema does not carry one.
	FirstSeenAt       time.Time `json:"first_seen_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	LastStateChangeAt time.Time `json:"last_state_change_at"`
	TotalCases        int32     `json:"total_cases"`
	FlapScore         float32   `json:"flap_score"`
	IsFlapping        bool      `json:"is_flapping"`
	// Synthetic marks an Alert oto manufactured for a DELIVERY DRILL. It is the
	// SAME field, with the same contract, as on the alerts list — `AlertDTO` is
	// one schema, and it lists `synthetic` among its required members.
	//
	// ⭐ A drill's alert reaches a group like any other, and the group screen is
	// one of the places a synthetic alert is legitimately visible; a member row
	// that could not say it was manufactured would put oto's own plumbing into
	// the customer's estate with nothing to mark it. It is carried from the mode
	// of the batch that first observed the identity, never from a label — a label
	// is forgeable and participates in `alert_key` (§C.2).
	Synthetic bool `json:"synthetic"`
	// Snooze is the §B.8 quiet period in force on this member, or an explicit
	// `null`. It is the SAME field, with the same contract, as on the alert list:
	// `AlertDTO` is one schema, and a member row that could not say it was
	// snoozed would be the one place the group fan-out's own effect is invisible.
	Snooze *SnoozeDTO `json:"snooze"`
}

// SnoozeDTO renders `SnoozeDTO` for a member alert row.
//
// ⛔ It is DECLARED HERE rather than imported from `alerts/api`. CONTEXT.md §5.5
// gives each module its own wire types, and §5.1 forbids one `api` package
// depending on another's; the json tags are byte-identical to the single
// `SnoozeDTO` schema in openapi.yaml, which is what makes them one type on the
// wire without making them one type in Go.
type SnoozeDTO struct {
	ID             uuid.UUID  `json:"id"`
	SnoozedAt      time.Time  `json:"snoozed_at"`
	SnoozedUntil   time.Time  `json:"snoozed_until"`
	SnoozedByLabel string     `json:"snoozed_by_label"`
	Note           *string    `json:"note"`
	EndedAt        *time.Time `json:"ended_at"`
}

// AlertEventDTO renders `AlertEventDTO` for the merged group timeline.
//
// `occurred_at` is the upstream claim the UI displays; `recorded_at` is oto's own
// clock and is what the list is ordered by. The two are never conflated, because
// upstream clock skew is measured and badged rather than corrected away.
//
// ⛔ THERE IS NO `seq`. It was declared as the `ui_events.seq` a polling client
// echoes back in `?since_seq=`, and it was set by nothing — while no operation
// returning this DTO accepts `since_seq` either. A client following the
// documented resume protocol read `null` forever and could never advance its
// cursor. Timelines page by `cursor`, which is served.
type AlertEventDTO struct {
	ID         uuid.UUID      `json:"id"`
	AlertID    *uuid.UUID     `json:"alert_id"`
	CaseID     *uuid.UUID     `json:"case_id"`
	GroupID    *uuid.UUID     `json:"group_id"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	RecordedAt time.Time      `json:"recorded_at"`
	ActorKind  string         `json:"actor_kind"`
	ActorID    *string        `json:"actor_id"`
	ActorLabel *string        `json:"actor_label"`
	Summary    string         `json:"summary"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// ------------------------------------------------------------------ requests

// AckRequest is the body of `POST /alert-groups/{id}/ack`.
//
// ⛔ Acking a group is a FAN-OUT of the same receipt over every open member, not
// a new primitive. There is no group-level ack state: "I acked the group" means
// "I have seen each of these", never "this group is mine" and never "this group
// is over".
type AckRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// CommentRequest is the body of `POST /alert-groups/{id}/comments`.
type CommentRequest struct {
	Body string `json:"body" validate:"required,notblank,min=1,max=10000"`
}

// SnoozeRequest is the body of `POST /alert-groups/{id}/snooze` (§B.8.3).
//
// ⛔ It is the SAME request shape as the per-alert snooze, because the group
// verb is a fan-out of the same primitive and not a new one. Exactly one of
// `until` and `duration_seconds`; bounds 5 minutes to 30 days; no indefinite
// snooze, ever.
type SnoozeRequest struct {
	Until           *time.Time `json:"until"`
	DurationSeconds *int64     `json:"duration_seconds" validate:"omitempty,min=300,max=2592000"`
	Note            string     `json:"note" validate:"omitempty,max=2000"`
}

// UnsnoozeRequest is the body of `POST /alert-groups/{id}/unsnooze`.
type UnsnoozeRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// ------------------------------------------------------------- query objects

// ListGroupsQuery is the validated form of the `listAlertGroups` query string.
type ListGroupsQuery struct {
	State    []string   `json:"state"    validate:"omitempty,max=2,unique,dive,oneof=open closed"`
	Severity []string   `json:"severity" validate:"omitempty,max=16,unique,dive,max=4096"`
	Cluster  []string   `json:"cluster"  validate:"omitempty,max=32,unique,dive,clusterkey"`
	SourceID string     `json:"source_id" validate:"omitempty,uuid"`
	Receiver string     `json:"receiver" validate:"omitempty,max=4096"`
	Storm    *bool      `json:"storm"`
	Ack      string     `json:"ack"      validate:"omitempty,oneof=unacked acked"`
	Since    *time.Time `json:"since"`
	Q        string     `json:"q"        validate:"omitempty,max=200"`
	Sort     string     `json:"sort"     validate:"omitempty,oneof=-last_activity_at -first_seen_at"`
	Limit    int        `json:"limit"    validate:"min=1,max=200"`
	Cursor   string     `json:"cursor"   validate:"omitempty,cursor"`
}

// TimelineQuery is the validated form of the `getAlertGroupTimeline` query.
type TimelineQuery struct {
	// The bound is the size of the closed `AlertEventType` enum — 36, including
	// `alert.snoozed` and `alert.unsnoozed` — so that a caller can always ask for
	// every type it is allowed to see.
	Type   []string   `json:"type"      validate:"omitempty,max=36,unique"`
	Since  *time.Time `json:"since"`
	Until  *time.Time `json:"until"`
	Order  string     `json:"order"     validate:"omitempty,oneof=asc desc"`
	Limit  int        `json:"limit"     validate:"min=1,max=200"`
	Cursor string     `json:"cursor"    validate:"omitempty,cursor"`
}
