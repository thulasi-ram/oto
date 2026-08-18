package api

import (
	"time"

	"github.com/google/uuid"
)

// The wire DTOs of the Alerts, Cases and Discovery tags.
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
	// ⛔ THERE IS NO `ack_state` ON AN ALERT. An ack is a receipt for ONE firing
	// episode and stops being true when that episode ends; a field here would
	// keep asserting it. `include=current_case` is what makes a row show
	// ack state, and it costs no extra query.
	FirstSeenAt       time.Time `json:"first_seen_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	LastStateChangeAt time.Time `json:"last_state_change_at"`
	TotalCases        int32     `json:"total_cases"`
	FlapScore         float32   `json:"flap_score"`
	IsFlapping        bool      `json:"is_flapping"`
	// Synthetic marks an Alert oto manufactured for a DELIVERY DRILL. It is
	// carried from the mode of the batch that first observed this identity, never
	// from a label — a label is forgeable by any upstream and participates in
	// `alert_key` (§C.2), so marking one would change its identity.
	//
	// ⭐ It is on the row so that the one screen that shows a synthetic alert —
	// a drill's result — can say plainly that it is not a real one. Every default
	// list, roll-up, typeahead and aggregate excludes these entirely, so an
	// ordinary caller will only ever see `false` here.
	Synthetic bool `json:"synthetic"`

	// Snooze is the §B.8 quiet period in force, or an explicit `null`.
	//
	// ⭐ IT IS ON THE LIST ROW AND NOT ONLY ON THE DETAIL VIEW, and it has NO
	// omitempty. The default list includes snoozed alerts (§B.8.6) — hiding one
	// is how an incident is lost — so a row that could not say it was snoozed
	// left the operator with no way to tell a quiet alert from a noisy one
	// except by opening every one of them. `null` says "awake"; an absent key
	// would say "unknown", and those are different facts.
	//
	// ⛔ It sits BESIDE `state` and the episode's `ack_state` and never inside
	// them. The three are orthogonal axes: a snoozed critical is still critical
	// and still firing, and the row must keep rendering it that way, with the
	// snooze as a separate `:zzz:` badge and countdown.
	Snooze *SnoozeDTO `json:"snooze"`

	// The three `include=` sub-resources. Absent unless the caller asked, which
	// is what keeps the list one query instead of N+1.
	CurrentCase *CaseDTO         `json:"current_case,omitempty"`
	Enrichments []EnrichmentDTO  `json:"enrichments,omitempty"`
	Rule        *RuleSnapshotRef `json:"rule,omitempty"`
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
	CurrentCase       *CaseDTO               `json:"current_case"`
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
//
// ⭐ It is a JOIN KEY, not a consolation prize (ADR 0025). `GET
// /api/v1/rule-snapshots/batch?id=…` turns a page of these into the snapshots
// themselves in one call, which is how the list renders `expr` without a request
// per alert.
type RuleSnapshotRef struct {
	ID uuid.UUID `json:"id"`
}

// CaseDTO renders `CaseDTO`: one contiguous firing episode.
//
// ⭐ `state` IS `open | closed` AND NOTHING ELSE (ADR 0040). The four §B.2 words
// describe the ALERT and are on every alert-shaped DTO; what this episode adds is
// `resolve_reason` — `upstream` for a resolution upstream asserted, `timeout` for
// one oto never heard — and `suppression_reason`, which names the silence that
// muted THIS firing. A client that wants the four-way reading composes it from
// those three fields exactly as the server does.
//
// ⛔ THERE IS NO `reopen_count` AND NO `reopen_of`. A Case is strictly terminal:
// a re-fire opens the next `seq`, unacknowledged, and the episode it succeeded is
// the row at `seq - 1`.
type CaseDTO struct {
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

// CaseDetailDTO renders `CaseDetailDTO`.
//
// `group` and `rule` are optional properties of the contract schema and are
// deliberately not embedded — see AlertDetailDTO. `/cases/{id}/rule` serves
// the snapshot whole.
type CaseDetailDTO struct {
	CaseDTO
	Alert       *AlertRefDTO    `json:"alert,omitempty"`
	Enrichments []EnrichmentDTO `json:"enrichments"`
	// DeliverySummary is a value type for the same reason as on AlertDetailDTO:
	// an optional field that nothing emitted is a contract that lies quietly.
	DeliverySummary DeliverySummaryDTO `json:"delivery_summary"`
}

// CaseListItemDTO renders `CaseListItemDTO`: one row of `GET /api/v1/cases`.
//
// ⭐⭐ THE ALERT REFERENCE IS THE WHOLE REASON THIS TYPE EXISTS, AND IT IS A VALUE
// AND NOT A POINTER. A bare `CaseDTO` carries `alert_id` and no `alertname`, no
// `severity` and no `cluster_key` — those are columns of `alerts`, because they
// describe the IDENTITY rather than the episode — so a client rendering the org
// list from it would have to fetch one alert per row. The service batch-loads the
// whole page in ONE further query and the mapper indexes it here.
//
// It is not optional and it cannot be absent: the repository's `EXISTS` proved
// the alert is in the caller's org before the case was returned at all, so a
// nullable field would be declaring a state the query cannot produce — the same
// argument `DeliverySummaryDTO` is a value type on the two detail DTOs for.
type CaseListItemDTO struct {
	CaseDTO
	Alert AlertRefDTO `json:"alert"`
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
// `group_id`, `policy_id` and `updated_at` are NULLABLE and are null when the read
// model has nothing to report — never substituted from a neighbouring field.
//
// ⛔ `group_id` IS A POINTER BECAUSE THE SCHEMA IS NULLABLE, AND THE SCHEMA IS
// NULLABLE BECAUSE `notifications.group_id` LOST ITS NOT NULL IN MIGRATION 00058:
// a digest is a window over a namespace, spans many generations and has no single
// thread (`notifications_target_ck`). This endpoint only ever lists group-scoped
// intents, so it always has an id to send — but a value type here would serialise
// the ZERO UUID for anything that does not, and an id that resolves to nothing is
// the same class of lie as silence that looks like "no alert". Both Go structs
// behind the one `NotificationDTO` schema are shaped like the schema, or the
// schema describes only one of them.
type NotificationDTO struct {
	ID               uuid.UUID           `json:"id"`
	SubjectKind      string              `json:"subject_kind"`
	SubjectID        uuid.UUID           `json:"subject_id"`
	GroupID          *uuid.UUID          `json:"group_id"`
	AlertID          *uuid.UUID          `json:"alert_id"`
	CaseID           *uuid.UUID          `json:"case_id"`
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
	Key             string `json:"key"`
	GroupBy         string `json:"group_by"`
	State           string `json:"state"`
	TotalCount      int32  `json:"total_count"`
	FiringCount     int32  `json:"firing_count"`
	SuppressedCount int32  `json:"suppressed_count"`
	ResolvedCount   int32  `json:"resolved_count"`
	ExpiredCount    int32  `json:"expired_count"`
	// ⛔ THERE ARE NO ACK COUNTS. Every counter here is a property of the Alert;
	// the ack pair was a property of one of its episodes, and it read a column
	// that no longer exists. The acked count is a case-surface number.
	FlappingCount  int32            `json:"flapping_count"`
	SeverityCounts map[string]int32 `json:"severity_counts"`
	FirstSeenAt    time.Time        `json:"first_seen_at"`
	LastSeenAt     time.Time        `json:"last_seen_at"`
}

// CasePolicyDTO renders `CasePolicyDTO`: one row of `case_policy_config` — the
// CASE RETENTION WINDOW W for one (namespace, alertname) pair.
//
// ⭐⭐ W MOVES *WHEN* A CASE CLOSES AND NOTHING ELSE. A case whose alert has
// resolved stays open for W and closes only once the alert has stayed resolved for
// W, so a re-fire inside W lands in the still-open episode instead of opening the
// next one. Six flaps become ONE case, one notification and one thread reply — the
// noise never exists, instead of existing and being withheld at delivery.
//
// ⛔ IT IS A DELAYED CLOSE AND NEVER A REOPEN (ADR 0040). Nothing on this DTO can
// resurrect a closed episode, and no field here will ever be able to.
type CasePolicyDTO struct {
	ID uuid.UUID `json:"id"`
	// Namespace is the ADR 0038 axis. The EMPTY STRING is the absent-namespace
	// partition and not a missing value: Prometheus treats an absent and an empty
	// namespace as equivalent, so they are one partition and this is how it is
	// spelled.
	Namespace string `json:"namespace"`
	Alertname string `json:"alertname"`
	// RetentionWindowSeconds is W. Zero means the case closes on the resolve, which
	// is what oto did before this table existed, and is what an absent row means too.
	RetentionWindowSeconds int32 `json:"retention_window_seconds"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ------------------------------------------------------------------ requests

// CreateCasePolicyRequest is the body of `POST /case-policies`.
//
// ⭐ `namespace` IS OPTIONAL AND `alertname` IS NOT, which is the table's own
// asymmetry: every Alert has an alertname (§C.2) and every group key hashes it, so
// a row without one would be an org-wide default this table deliberately does not
// offer. An omitted `namespace` is the absent-namespace partition — the one every
// alert that carries no `namespace` label falls into.
//
// The two together are the row's identity under `case_policy_axes_uniq`: a second
// create for the same pair is a `409`, and the way to change a window is a PATCH.
type CreateCasePolicyRequest struct {
	// Namespace is trimmed to the partition it will be stored in. There is no
	// `min`: "" is a legal, meaningful value.
	Namespace string `json:"namespace,omitempty" validate:"omitempty,max=1024"`
	Alertname string `json:"alertname"           validate:"required,notblank,min=1,max=1024"`
	// RetentionWindowSeconds mirrors `case_policy_window_ck`: 0 to 86400. Zero is
	// legal and is not a no-op request — it records "no window here, on purpose",
	// which an absent row cannot distinguish from "nobody has decided".
	RetentionWindowSeconds int32 `json:"retention_window_seconds" validate:"min=0,max=86400"`
}

// UpdateCasePolicyRequest is the body of `PATCH /case-policies/{id}`.
//
// ⛔ IT CARRIES NO `namespace` AND NO `alertname`, AND MUST NEVER GAIN EITHER.
// The pair is the row's identity; moving a window from one pair to another is
// deleting one rule and writing a second, and a PATCH that could do it silently
// would let an operator believe a window applies to an alertname it no longer
// names. A field that cannot be sent cannot be sent by accident.
type UpdateCasePolicyRequest struct {
	RetentionWindowSeconds *int32 `json:"retention_window_seconds,omitempty" validate:"omitempty,min=0,max=86400"`
}

// IsEmpty reports whether the request asks for nothing.
func (r UpdateCasePolicyRequest) IsEmpty() bool { return r.RetentionWindowSeconds == nil }

// AckRequest is the body of `POST /cases/{id}/ack` and of the group fan-out that
// shares it.
//
// An acked case is STILL FIRING. Acknowledgement is an orthogonal axis that says
// "a human has seen this", never "this is over".
type AckRequest struct {
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// UnackRequest is the body of `POST /cases/{id}/unack`.
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

// MaxUnsnoozeAlertIDs bounds `POST /alerts/unsnooze`.
//
// ⭐ WHY THERE IS A CEILING AT ALL. Every id in the body becomes one write
// transaction — a read of the alert, a read of its active snooze, a compare-and-set
// on the row, an event insert and an enqueued notification — applied in series
// inside ONE HTTP request. An unbounded list is an unbounded request, and a wake
// that outlives its deadline is retried from the beginning of the list.
//
// ⭐ WHY 100. It is the same ceiling `batchGetRuleSnapshots` puts on the ids it will
// resolve in one call, and for the same stated reason: 100 is one page of the UI's
// alert list, so "wake everything I can see" is exactly one call, and a caller
// paging at the contract's 200 ceiling makes two — still constant in the size of
// the page. It is deliberately far below grouping's FanOutLimit of 500: that
// ceiling exists to TRUNCATE a membership nobody enumerated, where this one exists
// to bound a list somebody typed.
//
// ⛔ IT IS SPELLED TWICE, HERE AND IN THE `validate` TAG BELOW, because a struct
// tag cannot reference a constant. The third spelling is `maxItems: 100` in
// api/openapi/openapi.yaml, which gate G1 holds to this file.
const MaxUnsnoozeAlertIDs = 100

// UnsnoozeAlertsRequest is the body of `POST /alerts/unsnooze` — the bulk wake.
//
// ⛔⛔ IT NAMES ITS SUBJECTS AND IT WILL NEVER TAKE A FILTER. There is no
// `severity=`, no `cluster=`, no "everything currently quiet" spelling of this
// request, and that is a decision rather than an omission. A filter is evaluated
// on the server against rows the caller never saw: one press would resume
// thousands of alerts whose extent the person pressing it cannot see, and the
// notifications that follow land in channels nobody agreed to wake. An explicit
// list is a bound the server can check and a person can read back afterwards.
//
// ⛔ THERE IS NO BULK SNOOZE BESIDE IT. This is the UNDO of a gesture somebody
// already made deliberately, one alert at a time; going quiet in bulk is the
// blindfold §B.8.3 argues against on the group verb.
type UnsnoozeAlertsRequest struct {
	// AlertIDs is required and must name at least one alert: there is no spelling
	// of this request that means "everything". Duplicates are refused — this is a
	// write, and a repeated id is a caller that has lost track of what it is asking
	// for.
	AlertIDs []uuid.UUID `json:"alert_ids" validate:"required,min=1,max=100,unique"`
	// Note is recorded with EVERY wake-up this request performs. The fan-out is of
	// the primitive, note and all, exactly as the group unsnooze does it.
	Note string `json:"note" validate:"omitempty,max=2000"`
}

// UnsnoozeOutcomeDTO renders `UnsnoozeOutcomeDTO`: what happened to ONE alert.
type UnsnoozeOutcomeDTO struct {
	AlertID uuid.UUID `json:"alert_id"`
	// Outcome is `woken` or `skipped`. It is two values and not three because a
	// skip's explanation belongs in Reason: a caller that wants the count reads
	// Outcome, a caller that has to tell a person what happened reads both.
	Outcome string `json:"outcome"`
	// Reason is the stable errs code of the refusal, and nil when the alert woke.
	//
	// ⭐ "NOTHING HAPPENED" HAS MORE THAN ONE HONEST EXPLANATION. `not_snoozed`
	// means the alert was already awake; `alert_not_found` means no such alert in this
	// org — which is also what another tenant's id gets, because telling the two
	// apart would confirm that the other tenant's row is real.
	Reason *string `json:"reason"`
}

// UnsnoozeAlertsDTO renders `UnsnoozeAlertsDTO`: the account of a bulk wake.
//
// ⭐ IT IS AN ACCOUNT AND NOT A COUNT, for the reason FanOutResult gives: a
// partial result is the NORMAL one — an operator waking a page of quiet alerts
// will routinely find some already awake — and "3 of 5" is not something a bare
// number can explain to the person who pressed the button.
type UnsnoozeAlertsDTO struct {
	Requested int `json:"requested"`
	Woken     int `json:"woken"`
	Skipped   int `json:"skipped"`
	// Results carries one entry per requested id, in the order the request gave
	// them, so a surface can report per row rather than per request.
	Results []UnsnoozeOutcomeDTO `json:"results"`
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
	Matcher string `json:"matcher"    validate:"omitempty,max=8192"`
	// ⛔ THERE IS NO `ack` FILTER. It read `alerts.ack_state`, a column that no
	// longer exists because an ack is a statement about one firing and the Alert
	// outlives the firing. `?ack=acked` is now `400 unknown_parameter`, which is
	// the honest answer: a filter that quietly returned the wrong page is how a
	// triage queue starts lying. The ack facet is served on the case surface.
	Flapping *bool `json:"flapping"`
	// Snoozed is an EXPLICIT filter and its absence means INCLUDE BOTH (§B.8.6).
	// The default list never hides a snoozed alert — hiding one is how an
	// incident is lost.
	Snoozed *bool `json:"snoozed"`
	// Synthetic is the OPPOSITE default from Snoozed: its absence means EXCLUDE.
	// A synthetic Alert is one oto manufactured for a delivery drill; nothing
	// fired in any cluster, so counting it as history would make every list and
	// every aggregate lie about the customer's estate. `?synthetic=true` is how
	// a drill's own result screen links to the row it made.
	Synthetic *bool      `json:"synthetic"`
	Since     *time.Time `json:"since"`
	Q         string     `json:"q"          validate:"omitempty,max=200"`
	Sort      string     `json:"sort"       validate:"omitempty,oneof=-last_seen_at -first_seen_at"`
	Include   []string   `json:"include"    validate:"omitempty,max=3,unique,dive,oneof=current_case enrichments rule"`
	Limit     int        `json:"limit"      validate:"min=1,max=200"`
	Cursor    string     `json:"cursor"     validate:"omitempty,cursor"`
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
	Flapping          *bool      `json:"flapping"`
	Snoozed           *bool      `json:"snoozed"`
	Synthetic         *bool      `json:"synthetic"`
	Since             *time.Time `json:"since"`
	Q                 string     `json:"q"          validate:"omitempty,max=200"`
	Limit             int        `json:"limit"      validate:"min=1,max=200"`
	Cursor            string     `json:"cursor"     validate:"omitempty,cursor"`
}

// ListCasesQuery is the validated form of the `listCases` query string.
//
// ⭐⭐ IT IS NOT `ListAlertsQuery` WITH `ack` PUT BACK. Every dimension here is
// either a column of `alert_cases` — `state`, `ack`, `group_id` — or one
// of the four IDENTITY facets an operator narrows by before they narrow by
// anything else. What it deliberately does NOT take is the label selector, the
// free-text search, `flapping` and `snoozed`: the first two are answered by GIN
// indexes on `alerts` and reaching them per case row turns a keyset page into a
// scan of the identity table, and the last two are properties of the identity
// that say nothing about which of its episodes you are looking at. The alert
// list is where those questions are asked.
type ListCasesQuery struct {
	// State is the EPISODE's own state: `open`, `closed`, or both. It is the ONE
	// liveness axis this query has.
	//
	// ⭐⭐ IT ABSORBED THE `open` BOOLEAN (ADR 0040). While the column held four
	// values the two were genuinely different questions and only `open` produced a
	// predicate the planner could match a partial index against; with two values
	// they are the same axis, so the boolean is gone and `repository.ListCases`
	// emits THIS one as the liveness literal `case_terminal_ended` proves it equal
	// to. Naming both values is the same as naming neither.
	State []string `json:"state"     validate:"omitempty,max=2,unique,dive,oneof=open closed"`
	// Ack is the facet the endpoint exists for: `?ack=unacked` is the queue people
	// work from. It is served here and nowhere else — `alerts` has carried no ack
	// column since 00049, because a receipt belongs to the firing it was given
	// for and the identity outlives the firing.
	Ack       []string `json:"ack"       validate:"omitempty,max=2,unique,dive,oneof=unacked acked"`
	GroupID   []string `json:"group_id"  validate:"omitempty,max=32,unique,dive,uuid"`
	Severity  []string `json:"severity"  validate:"omitempty,max=16,unique,dive,max=4096"`
	Cluster   []string `json:"cluster"   validate:"omitempty,max=32,unique,dive,clusterkey"`
	Namespace []string `json:"namespace" validate:"omitempty,max=64,unique,dive,max=4096"`
	AlertName []string `json:"alertname" validate:"omitempty,max=64,unique,dive,max=1024"`
	// Synthetic mirrors the alert list: absent means EXCLUDE, because an episode
	// of an alert oto manufactured for a delivery drill is not the customer's
	// history.
	Synthetic *bool `json:"synthetic"`
	// Since is a lower bound on `started_at` — the column the keyset is over, so
	// it narrows the very scan the cursor walks.
	Since  *time.Time `json:"since"`
	Limit  int        `json:"limit"     validate:"min=1,max=200"`
	Cursor string     `json:"cursor"    validate:"omitempty,cursor"`
	// ⛔ THERE IS NO `sort`. The order is `-started_at` and there is no second
	// key to choose between: a keyset cursor is only sound over an indexed total
	// order, and offering an enum with one legal value would be a parameter that
	// cannot change the answer — the same defect `since_seq` was removed for.
}

// TimelineQuery is the validated form of the event-list query string shared by
// `listAlertEvents`, `listCaseEvents` and `getAlertGroupTimeline`.
type TimelineQuery struct {
	// The bound is the SIZE OF THE CLOSED ENUM (domain.AllEventTypes), so that
	// "give me everything" is always expressible. It must never fall below that
	// enum: a ceiling under it refuses a caller for naming a type the server is
	// already writing.
	Type   []string   `json:"type"      validate:"omitempty,max=32,unique"`
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
