package domain

import (
	"time"

	"github.com/google/uuid"
)

// This file is the READ MODEL a Notification is rendered from. It is DATA: no
// invariants, no constructors, no behaviour beyond a couple of convenience
// predicates. The invariants live on the entities these facts are projected
// from, in the modules that own them, and duplicating one here would give two
// answers to the same question.
//
// It exists because of C11: a delivery is rendered AT CLAIM TIME, not at enqueue
// time. What is queued is an INTENT; what is sent is the world as it is when the
// message actually goes out. That is why an alert which fired and resolved
// entirely inside a snooze window produces no stale card when the snooze ends,
// and it is why this type is a snapshot rather than a payload.

// SnapshotQuery asks for everything one delivery needs to render.
type SnapshotQuery struct {
	GroupID uuid.UUID
	// AlertID narrows the FOCUS: the one alert an ack, a re-fire or a rule change
	// is about.
	AlertID *uuid.UUID
	// OccurrenceID narrows to one firing episode.
	OccurrenceID *uuid.UUID
	// MaxAlerts caps the member list. The renderer shows at most this many
	// instances inline and then says "and N more"; fetching more than it can show
	// is work nobody sees.
	MaxAlerts int
}

// OrgFacts identifies the tenant.
type OrgFacts struct {
	ID   uuid.UUID
	Slug string
	Name string
}

// GroupFacts is one AlertGroup GENERATION — the thing that owns exactly one
// thread per destination.
type GroupFacts struct {
	ID         uuid.UUID
	GroupKey   string
	Generation int
	Title      string
	Receiver   string
	// SourceGroupKey is Alertmanager's own groupKey. DISPLAY ONLY, NEVER PARSED:
	// it is unescaped, unbounded, and changes on every alertmanager.yml reload.
	SourceGroupKey string
	GroupLabels    map[string]string
	State          string
	Severity       string
	ClusterKey     string
	StateVersion   int

	FiringCount     int
	SuppressedCount int
	ResolvedCount   int
	ExpiredCount    int
	TotalCount      int
	AckedCount      int

	StormMode  bool
	StormSince *time.Time
	// StormCount is how many alerts joined this generation. It is what the storm
	// card counts, and storm mode is a VISIBLE state, never a silent suppression.
	StormCount int

	// NotificationReason is `alert_groups.last_notification_reason` — the wire
	// value Alertmanager put on the most recent batch for this generation (§H.6).
	// It is Alertmanager's statement about its OWN delivery, kept verbatim, and it
	// is reconciled against oto's transition-derived Reason at evaluation time.
	NotificationReason string

	// FiringSince is the UPSTREAM start of this generation: the earliest
	// `alert_occurrences.source_starts_at` among the members that are still live.
	//
	// ⛔ IT IS NOT `FirstSeenAt`. `FirstSeenAt` is when OTO first heard about the
	// group, and the gap between the two is oto's own latency plus Alertmanager's
	// `group_wait` — twenty-one minutes in the first live run. A card that says
	// "Started 18:17" when the alert started at 17:56 has told an operator
	// something false about how long an outage has lasted, which is the one number
	// they act on at 03:00.
	FiringSince time.Time

	// AlertmanagerURL is the source's `base_url`. It is what the Silence and
	// "Open in Alertmanager" deep links are built from (§H.3, R3): oto never
	// writes a silence, it only shows you where to write one.
	AlertmanagerURL string

	FirstSeenAt    time.Time
	LastActivityAt time.Time
	ClosedAt       *time.Time
}

// StartedAt is the instant the card means by "Started": upstream's own
// `startsAt` when oto has one, and oto's first sighting when it does not.
//
// The fallback is honest rather than convenient — a group whose members have no
// recorded upstream start really is only known to oto from when oto saw it — and
// it is one function so that every renderer, the API and the UI answer the
// question the same way.
func (g GroupFacts) StartedAt() time.Time {
	if !g.FiringSince.IsZero() {
		return g.FiringSince
	}
	return g.FirstSeenAt
}

// AllResolved reports whether every member of this generation has stopped
// firing and stopped being suppressed, with at least one of them resolved.
//
// It is the fact behind §H.6's `all alerts resolved` row, and oto derives it
// from its OWN membership rather than trusting the wire value alone: the
// counts are a projection of the occurrences oto has recorded, and they cannot
// disagree with the card they render.
func (g GroupFacts) AllResolved() bool {
	return g.TotalCount > 0 && g.FiringCount == 0 && g.SuppressedCount == 0 &&
		g.ResolvedCount > 0 && g.ResolvedCount+g.ExpiredCount >= g.TotalCount
}

// Open reports whether this generation is still live.
func (g GroupFacts) Open() bool { return g.ClosedAt == nil }

// AlertFacts is one Alert as a card sees it.
type AlertFacts struct {
	ID                uuid.UUID
	AlertKey          string
	SourceFingerprint string
	AlertName         string
	// Severity is the RAW upstream label value, never normalised: users filter on
	// their own vocabulary and normalising here would destroy it.
	Severity     string
	Namespace    string
	Service      string
	ClusterKey   string
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string

	State    string
	AckState string
	// SnoozedUntil is the §B.8 projection. A BARE TIMESTAMP and therefore not a
	// person reference. It is the THIRD ORTHOGONAL AXIS: it is not a state, it
	// never touches severity, and a snoozed critical is still rendered as a
	// firing critical.
	SnoozedUntil *time.Time

	FirstSeenAt      time.Time
	LastSeenAt       time.Time
	TotalOccurrences int
	IsFlapping       bool
	FlapScore        float64
	Value            *float64
}

// Snoozed reports whether this alert's notifications are quiet as of now.
func (a AlertFacts) Snoozed(now time.Time) bool {
	return a.SnoozedUntil != nil && a.SnoozedUntil.After(now)
}

// OccurrenceFacts is one firing episode.
type OccurrenceFacts struct {
	ID                uuid.UUID
	Seq               int
	State             string
	AckState          string
	SuppressionReason string
	ResolveReason     string
	StartedAt         time.Time
	EndedAt           *time.Time
	ReopenCount       int
	AckedByLabel      string
	AckedAt           *time.Time
	AckNote           string
}

// Duration is how long the episode has run, or ran.
func (o OccurrenceFacts) Duration(now time.Time) time.Duration {
	if o.EndedAt != nil {
		return o.EndedAt.Sub(o.StartedAt)
	}
	return now.Sub(o.StartedAt)
}

// RuleFacts is what the alerting rule said when the occurrence fired. Capturing
// it is oto's defensible differentiator: every other tool shows you the alert,
// and none of them shows you that somebody lowered the threshold last Tuesday.
type RuleFacts struct {
	SnapshotID      uuid.UUID
	Fingerprint     string
	File            string
	Group           string
	Name            string
	Expr            string
	For             time.Duration
	KeepFiringFor   time.Duration
	Labels          map[string]string
	Annotations     map[string]string
	Origin          string
	MatchConfidence string
	CapturedAt      time.Time
}

// RuleChangeFacts is the diff between this occurrence's rule and the previous
// one's.
type RuleChangeFacts struct {
	PreviousSnapshotID  uuid.UUID
	PreviousFingerprint string
	PreviousCapturedAt  time.Time
	ExprChanged         bool
	PreviousExpr        string
	NewExpr             string
	ForChanged          bool
	PreviousFor         time.Duration
	NewFor              time.Duration
	// LabelDiff maps a name to [old, new]; "" means the label was absent.
	LabelDiff      map[string][2]string
	AnnotationDiff map[string][2]string
}

// EnrichmentFacts is one Enricher's provenanced result.
type EnrichmentFacts struct {
	Enricher   string
	Status     string
	Payload    map[string]any
	Warnings   []string
	Error      string
	ComputedAt time.Time
}

// ActorFacts is who caused the fact being communicated. ACTOR, NEVER SUBJECT: a
// person appears on a notification as metadata about an action, never as its
// topic.
type ActorFacts struct {
	Kind  string
	ID    string
	Label string
}

// TransitionFact is one entry of the group's state trail — a §B.3 edge that a
// human reading the card at 03:00 would want to see happened.
//
// ⭐ IT EXISTS BECAUSE `chat.update` IS BOTH SILENT AND DESTRUCTIVE. ADR 0008
// makes the root card the CURRENT state and the thread the history, which is
// exactly right if you are in the thread. In the channel, a firing card mutates
// into a resolved one with no notification and no trace: somebody scrolling past
// sees a calm green card and cannot tell that anything ever happened, when it
// fired, or for how long. The owner's words on watching it: "it means something
// happened and we don't know."
//
// The trail is the fix that keeps `chat.update` as the primary verb. The card
// stops being a live gauge that forgets, and becomes a live gauge that keeps its
// receipt.
type TransitionFact struct {
	// Type is the `alert_events.type` value, e.g. `occurrence.resolved`.
	Type string
	// At is the UPSTREAM clock (`occurred_at`), which is what a human reads.
	At time.Time
	// ActorLabel is who caused it, when a human did. ACTOR, NEVER SUBJECT.
	ActorLabel string
}

// Snapshot is the whole read model for one delivery.
type Snapshot struct {
	Org    OrgFacts
	Group  GroupFacts
	Alerts []AlertFacts
	Focus  *AlertFacts
	// Trail is the group's state history, oldest first, already capped.
	Trail []TransitionFact
	// NotificationCount is how many non-suppressed notifications oto has sent
	// about this group. It is a fact about OTO's behaviour, and the receipt on a
	// terminal card is the right place to answer "how loud was this?".
	NotificationCount int
	// Occurrence is the focused firing episode, when there is one.
	Occurrence *OccurrenceFacts
	Rule       *RuleFacts
	RuleChange *RuleChangeFacts
	// Enrichments are keyed by enricher name.
	Enrichments map[string]EnrichmentFacts
	Actor       *ActorFacts
	Comment     string
	// SnoozedAlerts is the subset of the RENDERED alerts whose notifications are
	// currently quiet, with the time each wakes up. It drives the card's
	// `*Notifications*` field.
	SnoozedAlerts map[uuid.UUID]time.Time
	// MemberCount and SnoozedMemberCount are counted over EVERY current member,
	// not over the capped Alerts slice. The suppression decision must not depend
	// on how many instances the renderer happens to show.
	MemberCount        int
	SnoozedMemberCount int
	// TakenAt is when this snapshot was read. It becomes the card's "updated"
	// timestamp, so it must be the read time and not the enqueue time.
	TakenAt time.Time
}

// AllMembersSnoozed reports whether every member alert is currently quiet.
//
// This is the group-level snooze test, and the conservative direction matters. A
// group with ONE awake member is not snoozed: silencing the whole card because
// most of it is quiet would hide the one alert nobody asked to be quiet about,
// which is the exact failure the snooze bounds in §B.8.3 exist to prevent.
func (s Snapshot) AllMembersSnoozed() bool {
	return s.MemberCount > 0 && s.SnoozedMemberCount >= s.MemberCount
}

// FocusSnoozed reports whether the ONE alert this fact is about is quiet.
//
// Snooze is scoped to an `alert_key` (§B.8.1), so when a fact is about one alert
// that alert's own snooze decides — never the group's.
func (s Snapshot) FocusSnoozed(now time.Time) bool {
	if s.Focus == nil {
		return false
	}
	if s.Focus.Snoozed(now) {
		return true
	}
	until, ok := s.SnoozedAlerts[s.Focus.ID]
	return ok && until.After(now)
}

// AnyFlapping reports whether any member is flapping.
func (s Snapshot) AnyFlapping() bool {
	if s.Focus != nil {
		return s.Focus.IsFlapping
	}
	for _, a := range s.Alerts {
		if a.IsFlapping {
			return true
		}
	}
	return false
}
