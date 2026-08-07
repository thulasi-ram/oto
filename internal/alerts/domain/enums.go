package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// State is what the world is doing to one AlertOccurrence (SPEC §B.1, §B.2).
//
// It is owned by ingestion and the reconciler, and it is orthogonal to AckState:
// an acknowledged alert is still firing. The four values are a closed set and the
// same literal set the DDL CHECK accepts.
//
// The single most important distinction in oto lives here: `resolved` means an
// explicit upstream `status="resolved"` observation arrived, while `expired`
// means oto stopped hearing about the alert. Losing sight of an alert is NOT the
// same as the alert resolving, and oto never fabricates the difference.
type State struct{ s string }

// The closed State set. These are package-level vars because Go has no struct
// constants; they are never reassigned.
var (
	// StateNone is the zero State: the Alert has no open AlertOccurrence.
	StateNone = State{}
	// StateFiring means Alertmanager reports this label set active and not suppressed.
	StateFiring = State{"firing"}
	// StateSuppressed means active but suppressed. Set by the reconciler ONLY:
	// Alertmanager's MuteStage drops suppressed alerts before the webhook, so this
	// state is not observable via ingest (C1).
	StateSuppressed = State{"suppressed"}
	// StateResolved means an explicit per-alert status="resolved" was received.
	StateResolved = State{"resolved"}
	// StateExpired means oto stopped hearing about the alert while its AlertSource
	// was healthy. It means "Prometheus or Alertmanager went away", never "the
	// problem went away".
	StateExpired = State{"expired"}
)

// NewState parses a persisted state string.
func NewState(s string) (State, error) {
	switch s {
	case StateFiring.s, StateSuppressed.s, StateResolved.s, StateExpired.s:
		return State{s: s}, nil
	default:
		return State{}, errs.Newf(errs.KindValidation, "enum",
			"state must be one of: firing, suppressed, resolved, expired (got %q)", s)
	}
}

// String renders the state.
func (s State) String() string { return s.s }

// IsZero reports whether this is StateNone.
func (s State) IsZero() bool { return s.s == "" }

// IsTerminal reports whether the occurrence has ended. A terminal occurrence can
// only be left by a re-fire (T7 or T8).
func (s State) IsTerminal() bool { return s == StateResolved || s == StateExpired }

// IsOpen reports whether the occurrence is still live.
func (s State) IsOpen() bool { return s == StateFiring || s == StateSuppressed }

// AckState is what humans have done about an AlertOccurrence (SPEC §B.1).
// It is orthogonal to State: an acked alert is still firing.
type AckState struct{ s string }

// The closed AckState set.
var (
	// AckStateUnacked means nobody has taken this occurrence.
	AckStateUnacked = AckState{"unacked"}
	// AckStateAcked means a human took it. Acknowledgement identity IS stored;
	// per-person response-time aggregates are not, ever (R8).
	AckStateAcked = AckState{"acked"}
)

// NewAckState parses a persisted ack state.
func NewAckState(s string) (AckState, error) {
	switch s {
	case AckStateUnacked.s, AckStateAcked.s:
		return AckState{s: s}, nil
	default:
		return AckState{}, errs.Newf(errs.KindValidation, "enum",
			"ack_state must be one of: unacked, acked (got %q)", s)
	}
}

// String renders the ack state.
func (a AckState) String() string { return a.s }

// IsZero reports whether the ack state is unset.
func (a AckState) IsZero() bool { return a.s == "" }

// IsAcked reports whether a human has taken this occurrence.
func (a AckState) IsAcked() bool { return a == AckStateAcked }

// Severity is the class of an Alert's `severity` label, used to pick the leading
// emoji on a Slack card (§H.2) and the icon in the UI. Colour encodes STATE;
// severity encodes as an icon, never as colour alone.
type Severity struct{ s string }

// The closed Severity set.
var (
	// SeverityCritical is a page-worthy alert.
	SeverityCritical = Severity{"critical"}
	// SeverityPage is Alertmanager's other spelling of critical.
	SeverityPage = Severity{"page"}
	// SeverityWarning is a warning.
	SeverityWarning = Severity{"warning"}
	// SeverityInfo is informational.
	SeverityInfo = Severity{"info"}
	// SeverityNone is an explicitly severity-less alert.
	SeverityNone = Severity{"none"}
	// SeverityUnknown is "anything else, or absent" (§H.2). It is a first-class
	// value, not an error: a severity label oto has never seen must never cost an
	// alert.
	SeverityUnknown = Severity{"unknown"}
)

// NewSeverity parses a severity supplied by a trusted caller — an API filter, for
// example — and rejects anything outside the closed set.
func NewSeverity(s string) (Severity, error) {
	switch s {
	case SeverityCritical.s, SeverityPage.s, SeverityWarning.s,
		SeverityInfo.s, SeverityNone.s, SeverityUnknown.s:
		return Severity{s: s}, nil
	default:
		return Severity{}, errs.Newf(errs.KindValidation, "enum",
			"severity must be one of: critical, page, warning, info, none, unknown (got %q)", s)
	}
}

// SeverityFromLabel maps an UNTRUSTED upstream `severity` label onto the closed
// set. It never fails: anything unrecognised or absent is SeverityUnknown, which
// is what §H.2's last row says and what §L.3's governing rule requires.
func SeverityFromLabel(label string) Severity {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case SeverityCritical.s:
		return SeverityCritical
	case SeverityPage.s:
		return SeverityPage
	case SeverityWarning.s:
		return SeverityWarning
	case SeverityInfo.s:
		return SeverityInfo
	case SeverityNone.s:
		return SeverityNone
	default:
		return SeverityUnknown
	}
}

// String renders the severity.
func (s Severity) String() string { return s.s }

// IsZero reports whether the severity is unset.
func (s Severity) IsZero() bool { return s.s == "" }

// SuppressionReason says why an AlertOccurrence is suppressed. It exists ONLY
// while the occurrence is suppressed, and only the reconciler can set it (C1).
type SuppressionReason struct{ s string }

// The closed SuppressionReason set (SPEC §B.2, occ_suppress_ck).
var (
	// SuppressionSilence means an Alertmanager silence matched.
	SuppressionSilence = SuppressionReason{"silence"}
	// SuppressionInhibition means an inhibition rule matched.
	SuppressionInhibition = SuppressionReason{"inhibition"}
	// SuppressionMuteTimeInterval means a mute_time_interval was active.
	SuppressionMuteTimeInterval = SuppressionReason{"mute_time_interval"}
	// SuppressionActiveTimeInterval means an active_time_interval excluded it.
	SuppressionActiveTimeInterval = SuppressionReason{"active_time_interval"}
)

// NewSuppressionReason parses a suppression reason.
func NewSuppressionReason(s string) (SuppressionReason, error) {
	switch s {
	case SuppressionSilence.s, SuppressionInhibition.s,
		SuppressionMuteTimeInterval.s, SuppressionActiveTimeInterval.s:
		return SuppressionReason{s: s}, nil
	default:
		return SuppressionReason{}, errs.Newf(errs.KindValidation, "enum",
			"suppression_reason must be one of: silence, inhibition, mute_time_interval, active_time_interval (got %q)", s)
	}
}

// String renders the suppression reason.
func (r SuppressionReason) String() string { return r.s }

// IsZero reports whether no suppression reason is set.
func (r SuppressionReason) IsZero() bool { return r.s == "" }

// ResolveReason says how an AlertOccurrence ended, and it is bound one-to-one to
// the terminal state (occ_resolve_map_ck). This pairing is what stops oto ever
// claiming "resolved" when it means "expired".
type ResolveReason struct{ s string }

// The closed ResolveReason set.
var (
	// ResolveUpstream pairs with StateResolved: Alertmanager said so.
	ResolveUpstream = ResolveReason{"upstream"}
	// ResolveTimeout pairs with StateExpired: the reaper swept it.
	ResolveTimeout = ResolveReason{"timeout"}
)

// NewResolveReason parses a resolve reason.
func NewResolveReason(s string) (ResolveReason, error) {
	switch s {
	case ResolveUpstream.s, ResolveTimeout.s:
		return ResolveReason{s: s}, nil
	default:
		return ResolveReason{}, errs.Newf(errs.KindValidation, "enum",
			"resolve_reason must be one of: upstream, timeout (got %q)", s)
	}
}

// String renders the resolve reason.
func (r ResolveReason) String() string { return r.s }

// IsZero reports whether no resolve reason is set.
func (r ResolveReason) IsZero() bool { return r.s == "" }

// ActorKind names who or what caused an AlertEvent (alert_events.actor_kind).
// It is also the authority check on the lifecycle machine: `suppressed` can only
// ever be entered by the reconciler, and `expired` only by the reaper.
type ActorKind struct{ s string }

// The closed ActorKind set (SPEC §D.4).
var (
	// ActorSystem is oto itself, with no more specific attribution.
	ActorSystem = ActorKind{"system"}
	// ActorIngest is the webhook write path.
	ActorIngest = ActorKind{"ingest"}
	// ActorReconciler is the API v2 reconciler — the ONLY producer of suppressed.
	ActorReconciler = ActorKind{"reconciler"}
	// ActorReaper is the occurrence.reap job — the ONLY producer of expired.
	ActorReaper = ActorKind{"reaper"}
	// ActorEnricher is an Enricher completing.
	ActorEnricher = ActorKind{"enricher"}
	// ActorNotifier is the notification pipeline.
	ActorNotifier = ActorKind{"notifier"}
	// ActorUser is a human acting through oto's own API or UI.
	ActorUser = ActorKind{"user"}
	// ActorSlack is a human acting through a Slack interaction.
	ActorSlack = ActorKind{"slack"}
)

// NewActorKind parses an actor kind.
func NewActorKind(s string) (ActorKind, error) {
	switch s {
	case ActorSystem.s, ActorIngest.s, ActorReconciler.s, ActorReaper.s,
		ActorEnricher.s, ActorNotifier.s, ActorUser.s, ActorSlack.s:
		return ActorKind{s: s}, nil
	default:
		return ActorKind{}, errs.Newf(errs.KindValidation, "enum",
			"actor_kind must be one of: system, ingest, reconciler, reaper, enricher, notifier, user, slack (got %q)", s)
	}
}

// String renders the actor kind.
func (k ActorKind) String() string { return k.s }

// IsZero reports whether the actor kind is unset.
func (k ActorKind) IsZero() bool { return k.s == "" }

// IsHuman reports whether a person, rather than a machine, caused the event.
func (k ActorKind) IsHuman() bool { return k == ActorUser || k == ActorSlack }

// Actor is who caused an AlertEvent. For a human actor the identity and the
// display label are both required and both immutable: the label is denormalised
// on the event so that a renamed or deleted user never rewrites history.
type Actor struct {
	kind  ActorKind
	id    string
	label string
}

// NewActor builds an actor. A human actor (user or slack) MUST carry both an id
// and a label — ev_actor_ck says so in the DDL too.
func NewActor(kind ActorKind, id, label string) (Actor, error) {
	if kind.IsZero() {
		return Actor{}, errs.New(errs.KindValidation, "required", "actor_kind is required")
	}
	if kind.IsHuman() && (strings.TrimSpace(id) == "" || strings.TrimSpace(label) == "") {
		return Actor{}, errs.New(errs.KindValidation, "required",
			"a human actor requires both an id and a label")
	}
	return Actor{kind: kind, id: id, label: label}, nil
}

// SystemActor is the actor for machine-caused events of a given kind.
func SystemActor(kind ActorKind) (Actor, error) { return NewActor(kind, "", "") }

// Kind is the actor's kind.
func (a Actor) Kind() ActorKind { return a.kind }

// ID is the actor's stable identity, empty for a machine actor.
func (a Actor) ID() string { return a.id }

// Label is the actor's immutable display name, empty for a machine actor.
func (a Actor) Label() string { return a.label }

// IsZero reports whether the actor is unset.
func (a Actor) IsZero() bool { return a.kind.IsZero() }

// ObservationTime carries the two clocks every AlertEvent needs (C12).
//
// OccurredAt is the UPSTREAM claim and is what the UI DISPLAYS. RecordedAt is
// oto's own clock and is what timelines ORDER BY. They are never conflated, and
// their difference is measured as clock skew rather than rejected — which is
// precisely why alert_events has no (recorded_at >= occurred_at) CHECK.
type ObservationTime struct {
	occurredAt time.Time
	recordedAt time.Time
}

// NewObservationTime pairs an upstream claim with oto's clock reading. Both must
// be set; the domain never calls time.Now() itself.
func NewObservationTime(occurredAt, recordedAt time.Time) (ObservationTime, error) {
	if occurredAt.IsZero() {
		return ObservationTime{}, errs.New(errs.KindValidation, "required", "occurred_at is required")
	}
	if recordedAt.IsZero() {
		return ObservationTime{}, errs.New(errs.KindValidation, "required", "recorded_at is required")
	}
	return ObservationTime{occurredAt: occurredAt.UTC(), recordedAt: recordedAt.UTC()}, nil
}

// OccurredAt is the upstream claim. Display this.
func (t ObservationTime) OccurredAt() time.Time { return t.occurredAt }

// RecordedAt is oto's clock. Order by this.
func (t ObservationTime) RecordedAt() time.Time { return t.recordedAt }

// Skew is recorded_at - occurred_at. A persistently large skew is a source-health
// warning, never a reason to reject an observation.
func (t ObservationTime) Skew() time.Duration { return t.recordedAt.Sub(t.occurredAt) }

// IsZero reports whether the pair is unset.
func (t ObservationTime) IsZero() bool { return t.occurredAt.IsZero() && t.recordedAt.IsZero() }

// TimeWindow is a half-open query range. Every AlertEvent query takes one,
// because `recorded_at` is the partition key and an unbounded event query scans
// thirteen months of partitions.
type TimeWindow struct {
	from time.Time
	to   time.Time
}

// NewTimeWindow rejects an inverted or unbounded range at construction.
func NewTimeWindow(from, to time.Time) (TimeWindow, error) {
	if from.IsZero() {
		return TimeWindow{}, errs.New(errs.KindValidation, "required", "time window requires a start")
	}
	if !to.After(from) {
		return TimeWindow{}, errs.New(errs.KindValidation, "field_order",
			"time window end must be after its start")
	}
	return TimeWindow{from: from.UTC(), to: to.UTC()}, nil
}

// From is the inclusive start of the window.
func (w TimeWindow) From() time.Time { return w.from }

// To is the exclusive end of the window.
func (w TimeWindow) To() time.Time { return w.to }

// Duration is the width of the window.
func (w TimeWindow) Duration() time.Duration { return w.to.Sub(w.from) }

// Contains reports whether t falls inside the window.
func (w TimeWindow) Contains(t time.Time) bool { return !t.Before(w.from) && t.Before(w.to) }

// IsZero reports whether the window is unset.
func (w TimeWindow) IsZero() bool { return w.from.IsZero() && w.to.IsZero() }

// requireID rejects a zero UUID where the schema declares NOT NULL. A zero id
// reaching the driver is a mapper bug, and this is where it stops.
func requireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return errs.Newf(errs.KindValidation, "required", "%s is required", field)
	}
	return nil
}
