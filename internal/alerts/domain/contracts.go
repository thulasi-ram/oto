package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// This file is SPEC §F.5.2, literally: the parameter and result types the §F.5
// repository interfaces refer to. They were referenced but never defined, which
// blocked every implementation agent, so they are defined here once and nowhere
// else.
//
// They are DATA, not entities. They carry no invariants of their own and have no
// constructors: an invariant belongs on the entity the repository returns
// (Alert, Occurrence, Snooze), and duplicating it here would give two answers to
// the same question. They also carry NO `json` STRUCT TAGS — those are what
// would quietly turn a domain type into a DTO (§L.4.1, §P-21b). `encoding/json`
// itself is permitted here; it does no I/O.

// ---------------------------------------------------------------- observations

// ObservationSource distinguishes the two producers. It is load-bearing: only
// the reconciler may ENTER `suppressed` (T3); EITHER may leave it (T4, §B.3.1).
type ObservationSource string

const (
	// ObservedByIngest is the webhook write path. It can never witness
	// suppression beginning — Alertmanager's MuteStage drops suppressed alerts
	// before the webhook fires — but its arrival IS proof that suppression ended.
	ObservedByIngest ObservationSource = "ingest"
	// ObservedByReconciler is the GET /api/v2/alerts reconcile pass, the only
	// witness that can see `suppressed` begin.
	ObservedByReconciler ObservationSource = "reconciler"
)

// Observation is one normalised alert from one batch or one reconcile pass.
//
// It is the ONLY input to the state machine, and it is TRANSIENT — never
// persisted as itself. What is persisted is the raw batch (short retention) and
// the AlertEvents the machine appends.
type Observation struct {
	Source ObservationSource
	// Synthetic is true when this observation came from a batch oto manufactured
	// for a DELIVERY DRILL rather than from a cluster.
	//
	// ⭐ It is PROVENANCE and never payload: it is carried from
	// `ingest_batches.mode`, which is set by the code path that accepted the
	// batch. Deriving it from a reserved label would let any upstream forge it,
	// and — because labels participate in `alert_key` (§C.2) — would make marking
	// an alert change its identity.
	//
	// ⛔ IT PROPAGATES TO `alerts.synthetic`, AND EVERY AGGREGATE EXCLUDES IT.
	// See the column comment in 00039_delivery_drills.sql for the complete list
	// of reads that were changed. An aggregate that forgets is a silently wrong
	// number in a report a customer is paying for.
	Synthetic  bool
	BatchID    uuid.UUID
	SourceID   uuid.UUID
	ClusterID  uuid.UUID
	ClusterKey ClusterKey

	AlertKey          AlertKey
	SourceFingerprint SourceFingerprint
	Labels            LabelSet
	Annotations       map[string]string
	GeneratorURL      string

	// Status is the upstream status. Webhook: firing|resolved only.
	// Reconciler: adds active|suppressed.
	Status string
	// SuppressionReason is reconciler-only and "" otherwise (C1).
	SuppressionReason string
	SuppressedBy      SuppressedBy

	SourceStartsAt time.Time
	// SourceEndsAt is Alertmanager's endsAt. THE ZERO TIME IS LEGAL and means
	// "unknown" for this payload (B10/B13) — it never means "forget the end time
	// you already had".
	SourceEndsAt    time.Time
	SourceUpdatedAt time.Time
	Value           *float64

	// ObservedAt is oto's clock at normalisation.
	ObservedAt time.Time
	// SkewMS is ObservedAt - SourceStartsAt. Measured and surfaced, never a
	// reason to reject an observation (C12).
	SkewMS int64

	// ---------------------------------------------------- the grouping inputs
	//
	// ⭐ The four fields below are CARRIED, NEVER READ, by this module. They are
	// the §C.4 inputs the INGEST ORCHESTRATOR needs to resolve the AlertGroup
	// generation at §G.4 step 4 — between the alert upsert and the state machine
	// — and they travel on the Observation because the Observation is the only
	// thing that crosses from `ingestion` to the orchestrator.
	//
	// ⛔ `alerts` must not import `grouping` to record a signal: an observation
	// whose group could not be resolved is still recorded in full. The RESOLVED
	// group comes back in through ObserveOptions.GroupID, which is why that field
	// exists at all.

	// Receiver is the Alertmanager receiver this webhook was delivered to. It is
	// "" for a reconciler-sourced observation, which has no receiver (§C.4).
	Receiver string
	// GroupLabels are the labels Alertmanager grouped by — the second half of the
	// durable §C.4 key. Empty is legal and hashes as the empty object.
	GroupLabels map[string]string
	// SourceGroupKey is Alertmanager's OWN `groupKey`, carried verbatim so it can
	// be stored verbatim for observability.
	//
	// ⛔ IT IS NEVER PARSED, and it is never the group's identity. It embeds the
	// route path, it is unescaped and unbounded, and it changes on every
	// `alertmanager.yml` reload — a group keyed by it would be reborn, with a new
	// Slack thread, every time an operator edited a route (§C.4).
	SourceGroupKey string
	// NotificationReason is Alertmanager's `notification_reason` for this
	// delivery (C5), recorded on the generation and feeding the §H.6 table. Empty
	// on Alertmanager older than 0.32.0.
	NotificationReason string
}

// SuppressedBy mirrors Alertmanager's three suppression witnesses, as stored in
// `alert_occurrences.suppressed_by`.
//
// The JSONB keys are Alertmanager's own camelCase, which is why this type
// marshals through an explicit shadow struct rather than `json` tags: tags on a
// domain type are forbidden (§L.4.1), but the foreign system's key spelling is
// not ours to change.
type SuppressedBy struct {
	SilencedBy  []string
	InhibitedBy []string
	MutedBy     []string
}

// suppressedByWire is the on-the-wire spelling of SuppressedBy.
type suppressedByWire struct {
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
	MutedBy     []string `json:"mutedBy"`
}

// MarshalJSON renders Alertmanager's key spelling.
func (s SuppressedBy) MarshalJSON() ([]byte, error) {
	return json.Marshal(suppressedByWire(s))
}

// UnmarshalJSON reads Alertmanager's key spelling.
func (s *SuppressedBy) UnmarshalJSON(b []byte) error {
	var w suppressedByWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*s = SuppressedBy(w)
	return nil
}

// IsZero reports whether no witness was named.
func (s SuppressedBy) IsZero() bool {
	return len(s.SilencedBy) == 0 && len(s.InhibitedBy) == 0 && len(s.MutedBy) == 0
}

// ---------------------------------------------------------------------- alerts

// AlertUpsert is one row of the batched ON CONFLICT upsert (§D.12c). Dedup is
// enforced by alerts_key_uniq, never by a read-then-write check (C.2).
type AlertUpsert struct {
	// ID is a pre-generated uuidv7, used only if this turns out to be an INSERT.
	ID          uuid.UUID
	ClusterID   uuid.UUID
	AlertKey    AlertKey
	Fingerprint SourceFingerprint
	AlertName   string
	// Severity is the RAW upstream label value, NOT normalised (§L.4.2). Bounded
	// by MaxRawSeverityBytes / alerts_sev_ck and by nothing else: users filter on
	// their own vocabulary and normalising here would destroy it.
	Severity     *string
	Namespace    *string
	Service      *string
	ClusterKey   ClusterKey
	Labels       LabelSet
	Annotations  map[string]string
	GeneratorURL *string
	State        State
	SeenAt       time.Time
	// Synthetic marks an Alert oto manufactured for a delivery drill. Carried
	// from Observation.Synthetic, written to `alerts.synthetic`, and excluded
	// from every aggregate in oto.
	//
	// ⛔ On an ON CONFLICT update it is deliberately NOT re-written: an identity
	// is synthetic because of how it FIRST arrived, and a real alert that later
	// collides with a drill's label set must not be able to erase itself from
	// the statistics by doing so.
	Synthetic bool
}

// AlertUpsertResult is what one AlertUpsert produced.
type AlertUpsertResult struct {
	Alert Alert
	// WasInserted comes from (xmax = 0) and drives whether `alert.created` is
	// emitted. Reading it from the returning row is what makes the upsert
	// idempotent without a prior SELECT.
	WasInserted bool
}

// AlertProjection is the denormalised current-state summary written onto
// `alerts` in the SAME TRANSACTION as the occurrence transition that caused it.
// Current state is a projection; alert_events is the truth.
type AlertProjection struct {
	State               State
	CurrentOccurrenceID *uuid.UUID
	AckState            AckState
	// SnoozedUntil is the §B.8 projection of the active alert_snoozes row, nil
	// when the Alert is awake. It is NOT a state and it NEVER affects State,
	// AckState or severity — the three axes are independent (§B.1).
	SnoozedUntil      *time.Time
	LastSeenAt        time.Time
	LastStateChangeAt time.Time
	TotalOccurrences  int
}

// AlertFilter is the compiled, validated form of the §E.3 query string. A nil or
// empty slice means "no constraint on this dimension".
type AlertFilter struct {
	States      []State
	Severities  []string // RAW label values (§L.4.2)
	Namespaces  []string
	ClusterKeys []string
	Services    []string
	AlertNames  []string
	// Fingerprints filters on the §C.3 source fingerprint — the axis
	// `group_by=fingerprint` buckets on. Without it a fingerprint roll-up bucket
	// is a dead end: the UI can show the count and can never open it, which is
	// the one thing a roll-up exists to let a user do. `alertname` and
	// `namespace`, the other two axes, have had their filter since the first
	// draft; this is the third.
	Fingerprints []string
	AckState     *AckState
	Flapping     *bool
	// Snoozed is nil for INCLUDE BOTH, and nil is the default. THE DEFAULT LIST
	// NEVER HIDES SNOOZED ALERTS (§B.8.6) — hiding them is how an incident is
	// lost. `?snoozed=true|false` is an explicit, visible filter chip.
	Snoozed *bool
	// Synthetic filters on `alerts.synthetic`, and nil means EXCLUDE — the
	// opposite default from Snoozed, one field up, on purpose.
	//
	// A snoozed alert is a real thing happening in a real cluster, so hiding it
	// by default is how an incident is lost. A synthetic alert is one oto
	// manufactured for a delivery drill: nothing fired anywhere, and letting it
	// into a default list would put oto's own plumbing into the customer's
	// history. `?synthetic=true` is the explicit way to look at one.
	Synthetic *bool
	// LabelsAll is the AND form: labels @> {…}.
	LabelsAll map[string]string
	// LabelsAny is the IN form: labels @> {k:v1} OR labels @> {k:v2}.
	LabelsAny map[string][]string
	// LabelsNone is the NOT form: NOT (labels @> {k:v1}) AND NOT (labels @> {k:v2}).
	//
	// It is multi-valued because `NOT (a OR b)` is `NOT a AND NOT b`, which is
	// exactly what `label[!tier]=canary,staging` and `tier!~"canary|staging"`
	// mean. A single-valued form would have to REFUSE both rather than
	// under-filter, and refusing a predicate the containment index can answer is
	// as wrong as degrading one it cannot.
	LabelsNone map[string][]string
	Since      *time.Time
	// Query is full-text over alerts_text_idx.
	Query string
	// FilterHash must equal Cursor.Hash or the cursor is rejected (§E.1). A
	// keyset cursor minted against one filter is meaningless against another.
	FilterHash string
}

// ----------------------------------------------------------------- occurrences

// OpenOccurrence is the repository input for T1 and T7 — opening a new firing
// episode. The domain-side factory that produces it and its `occurrence.opened`
// event is OpenNewOccurrence.
type OpenOccurrence struct {
	ID              uuid.UUID
	AlertID         uuid.UUID
	GroupID         *uuid.UUID
	Seq             int
	StartedAt       time.Time
	SourceStartsAt  time.Time
	SourceEndsAt    *time.Time
	SourceUpdatedAt *time.Time
	ReopenOf        *uuid.UUID
	Value           *float64
	SkewMS          int64
}

// TransitionKind names an edge in §B.3. IT IS NOT A STATE, and conflating the
// two is how a state machine acquires a fifth state nobody meant to add.
type TransitionKind string

const (
	// TransitionObserve is T2: a repeat observation.
	TransitionObserve TransitionKind = "observe"
	// TransitionSuppress is T3. RECONCILER ONLY (§B.3.1).
	TransitionSuppress TransitionKind = "suppress"
	// TransitionUnsuppress is T4. RECONCILER *OR* INGEST — a webhook arrival is
	// positive proof of non-suppression (§B.3.1). The asymmetry with
	// TransitionSuppress is deliberate.
	TransitionUnsuppress TransitionKind = "unsuppress"
	// TransitionResolve is T5: an explicit upstream status="resolved".
	TransitionResolve TransitionKind = "resolve"
	// TransitionExpire is T6: the reaper, and only while the source is healthy.
	TransitionExpire TransitionKind = "expire"
	// TransitionReopen is T8: a re-fire inside refire_grace.
	TransitionReopen TransitionKind = "reopen"
)

// Transition is the persisted effect of one edge. It is produced by the domain
// state machine (Apply) and NEVER assembled by hand in a repository or a handler
// — assembling one by hand is how an occurrence acquires a state no §B.3 row
// permits.
type Transition struct {
	Kind              TransitionKind
	ToState           State
	SuppressionReason *string
	SuppressedBy      *SuppressedBy
	ResolveReason     *string
	// EndedAt is ALREADY CLAMPED to >= started_at (§B.3.2). A repository must
	// never re-derive it.
	EndedAt         *time.Time
	LastObservedAt  time.Time
	SourceEndsAt    *time.Time
	SourceUpdatedAt *time.Time
	ReopenCount     *int
	// SuppressCount is the post-image `suppress_count`. T3 increments it; every
	// other edge leaves it alone, and nil means "do not write this column".
	SuppressCount *int
	Value         *float64
	// DetectedBy names the witness, and is what makes T4's event honest about
	// whether a reconcile pass or a webhook proved suppression had ended.
	DetectedBy ObservationSource
	// Clamped is true when EndedAt was pulled forward by a backward-skewed
	// upstream clock. It is surfaced on the event payload and accumulated into
	// source_health.clock_skew_ms. Measured, never rejected (C12).
	Clamped bool

	// Expected is the PRE-IMAGE this edge was computed from. It is REQUIRED, and
	// a repository MUST refuse a Transition without one — see
	// TransitionPrecondition.
	Expected TransitionPrecondition
}

// TransitionPrecondition is the row as it looked when the §B.3 machine read it,
// and it is what turns the persisted edge into a COMPARE-AND-SET.
//
// ⭐⭐ THIS IS THE MECHANISM THAT STOPS A RESOLUTION BEING FABRICATED.
//
// Every §B.3 edge is a read, a decision and a write, and `db.Tx` runs at READ
// COMMITTED. Without a precondition the loser of a race blocks on the row lock,
// re-evaluates nothing but `id`, and then overwrites the winner with a verdict
// reached against state that has since changed. Three real consequences, all
// traced: the reaper stamping `expired`/`timeout` on an occurrence a webhook
// just proved is firing; the reaper clobbering a genuine `resolved` from ingest;
// and a reconciler T3 resurrecting `suppressed` over an ingest T5 and erasing
// `ended_at`, which puts a closed episode back inside occ_one_open_idx.
//
// Carrying the pre-image on the Transition itself — rather than as an optional
// argument some call site can forget — is deliberate: the repository refuses a
// Transition whose State is zero, so a new call site CANNOT write an unguarded
// state change.
//
// ⭐ IT IS ONE FIELD, AND THAT IS THE POINT. This began as a four-column
// pre-image — state, ended_at, source_ends_at, reopen_count — because the schema
// offered nothing better. `alert_occurrences.state_version` (migration 00023) now
// does, and collapsing onto it is stronger rather than merely cheaper: a
// multi-column pre-image can be PARTIALLY specified, so a future call site could
// assert three of the four, read as guarded in review, and still lose the one
// column that mattered. A single version cannot be half-asserted.
//
// ⛔ EVERY WRITE THAT MOVES A DECISION INPUT MUST BUMP IT, and that includes
// `Observe` — a T2 repeat observation changes no state letter but moves
// `source_ends_at`, which is the entire input to the §B.4 grace check. A version
// that tracked only state changes would leave the reaper's compare-and-set blind
// to precisely the webhook that disproves the expiry.
type TransitionPrecondition struct {
	// StateVersion is the `state_version` the machine read. It is REQUIRED: zero
	// means this Transition was never bound to a pre-image at all, and the
	// repository refuses it rather than writing unguarded.
	StateVersion int
}

// PreconditionFor renders the compare-and-set pre-image of an occurrence.
//
// It is the ONLY way a TransitionPrecondition should be built: hand-assembling
// one is how a caller ends up asserting a pre-image that is not the one it
// actually decided against, which is worse than no precondition at all because it
// looks guarded.
func PreconditionFor(o Occurrence) TransitionPrecondition {
	return TransitionPrecondition{StateVersion: o.StateVersion()}
}

// AckChange carries both directions of T9/T10. Ack fields are all-or-nothing
// (occ_ack_ck): a repository writing three of the four is writing a row the
// database will refuse.
type AckChange struct {
	To AckState
	// By is nil for actor_kind='system' — the T10 automatic unack when a new
	// occurrence opens has no human behind it.
	By      *uuid.UUID
	ByLabel *string
	At      time.Time
	Note    *string
	// Reason is "manual" or "new_occurrence" (UnackReasonManual,
	// UnackReasonNewOccurrence).
	Reason string
}

// ----------------------------------------------------------------- discovery

// LabelCount is one row of the filter bar's typeahead: a distinct label name or
// value, and HOW MANY ALERTS CARRY IT.
//
// The count is not decoration. A filter bar that offers a label matching nothing
// wastes the one minute of an incident that matters most, and the contract has
// always declared `alert_count` on both discovery DTOs — it was the server that
// had nothing to put in it.
type LabelCount struct {
	// Value is the label name (for LabelNames) or the label value (for
	// LabelValues).
	Value string
	// Count is how many Alerts in the caller's org carry it.
	Count int
}

// ---------------------------------------------------------------- roll-ups

// RollupKey names the axis an alert roll-up buckets on (§E.3a).
//
// ⛔ A roll-up is a VIEW over the alert list and is NEVER an AlertGroup. An
// AlertGroup is one generation of one Alertmanager notification group, it owns a
// chat thread and it has a row; a roll-up bucket has none of those and exists
// only for the duration of one query (§A.1).
type RollupKey struct{ s string }

// The closed set of roll-up axes.
var (
	// RollupByAlertName buckets on the promoted `alertname` label.
	RollupByAlertName = RollupKey{"alertname"}
	// RollupByNamespace buckets on the promoted `namespace` label.
	RollupByNamespace = RollupKey{"namespace"}
	// RollupByFingerprint buckets on the §C.3 source fingerprint.
	RollupByFingerprint = RollupKey{"fingerprint"}
)

// NewRollupKey parses a roll-up axis, refusing anything outside the closed set.
//
// The set is closed because each member has to be answerable from an index; an
// arbitrary axis would be a GROUP BY over a sequential scan, which is the
// failure ADR 0017 forbids.
func NewRollupKey(s string) (RollupKey, error) {
	switch s {
	case RollupByAlertName.s, RollupByNamespace.s, RollupByFingerprint.s:
		return RollupKey{s: s}, nil
	default:
		return RollupKey{}, errs.Newf(errs.KindValidation, "enum",
			"group_by must be one of: alertname, namespace, fingerprint (got %q)", s)
	}
}

// String renders the axis.
func (k RollupKey) String() string { return k.s }

// IsZero reports whether no axis was chosen.
func (k RollupKey) IsZero() bool { return k.s == "" }

// AlertRollup is one bucket of the §E.3a aggregation: every Alert that shares
// one value of the chosen axis, counted by state.
//
// ⭐ The counts are over the WHOLE filtered result set, not over one page. That
// is the entire reason this exists: a roll-up computed client-side over the rows
// that happen to be loaded is a count that is quietly wrong, and a quietly wrong
// count during an incident is worse than none.
type AlertRollup struct {
	// Key is the bucket value: the alertname, the namespace ("" when the alert
	// carries none) or the source fingerprint.
	Key string
	// Total is every Alert in the bucket.
	Total int
	// The four §B.2 states, never merged. `resolved` and `expired` are different
	// facts and collapsing them would hide the more interesting of the two.
	Firing     int
	Suppressed int
	Resolved   int
	Expired    int
	// Acked is how many have a human receipt. Orthogonal to state: an acked
	// alert is still firing (§B.1).
	Acked int
	// Flapping and Snoozed are the two damping facets, counted so the bucket can
	// render them as the VISIBLE states §B.6 and §B.8.6 require.
	Flapping int
	Snoozed  int
	// SeverityCounts is the RAW severity label to its count. Raw because
	// operators filter on their own vocabulary (§L.4.2), which also means oto
	// must not rank it — the client applies its own precedence.
	SeverityCounts map[string]int
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
}

// RollupState is the bucket's roll-up state: a bucket is as alive as its
// liveliest member.
//
// `expired` outranks `resolved` because "we stopped hearing about this" is an
// open question and "the upstream said it ended" is a closed one. An empty
// bucket cannot exist — a bucket is created by having a member — so the zero
// answer is unreachable and the fallback is the most conservative reading.
func (r AlertRollup) RollupState() State {
	switch {
	case r.Firing > 0:
		return StateFiring
	case r.Suppressed > 0:
		return StateSuppressed
	case r.Expired > 0:
		return StateExpired
	default:
		return StateResolved
	}
}

// Unacked is how many members carry no human receipt.
func (r AlertRollup) Unacked() int {
	if r.Acked >= r.Total {
		return 0
	}
	return r.Total - r.Acked
}

// -------------------------------------------------------------- small helpers

func derefID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
