package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxAckNoteBytes bounds an acknowledgement note (occ_acknote_ck).
const MaxAckNoteBytes = 2000

// Occurrence is an AlertOccurrence: ONE contiguous firing episode of an Alert,
// identified by (alert_id, seq). It is what a human acknowledges and what MTTR is
// measured over. The authoritative lifecycle state machine runs here; Alert.state
// and AlertGroup.state are projections of it.
//
// Every field is unexported. An Occurrence can only come from a constructor or
// from a transition, so an illegal combination — `resolved` without an
// `ended_at`, `acked` without an `acked_at` — cannot be built at all. Each
// invariant below is mirrored by a DDL CHECK in §D.4, belt and braces.
type Occurrence struct {
	id      uuid.UUID
	orgID   uuid.UUID
	alertID uuid.UUID
	groupID uuid.UUID
	seq     int

	state             State
	suppressionReason SuppressionReason

	// oto clock
	startedAt      time.Time
	endedAt        time.Time
	lastObservedAt time.Time

	// upstream clock
	sourceStartsAt  time.Time
	sourceEndsAt    time.Time
	sourceUpdatedAt time.Time

	resolveReason ResolveReason
	reopenCount   int
	reopenOf      uuid.UUID

	ackState     AckState
	ackedBy      uuid.UUID
	ackedByLabel string
	ackedAt      time.Time
	ackNote      string

	ruleSnapshotID uuid.UUID
	value          *float64
	observedSkew   time.Duration
}

// OccurrenceParams is the full constructor input for an AlertOccurrence. It is
// also the rehydration shape: a repository maps a row into it, and the
// constructor re-proves every invariant at the boundary.
type OccurrenceParams struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	AlertID uuid.UUID
	// GroupID is the AlertGroup generation this occurrence belongs to, or
	// uuid.Nil before it has been bound.
	GroupID uuid.UUID
	// Seq is the 1-based episode number within the Alert.
	Seq int

	State             State
	SuppressionReason SuppressionReason

	StartedAt      time.Time
	EndedAt        time.Time
	LastObservedAt time.Time

	SourceStartsAt  time.Time
	SourceEndsAt    time.Time
	SourceUpdatedAt time.Time

	ResolveReason ResolveReason
	ReopenCount   int
	// ReopenOf is the previous occurrence when T7 followed a close.
	ReopenOf uuid.UUID

	AckState     AckState
	AckedBy      uuid.UUID
	AckedByLabel string
	AckedAt      time.Time
	AckNote      string

	RuleSnapshotID uuid.UUID
	// Value is the sample value that fired the rule, when upstream supplied one.
	Value *float64
	// ObservedSkew is received_at - startsAt for the observation that opened or
	// last touched this occurrence (C12).
	ObservedSkew time.Duration
}

// NewOccurrence builds an AlertOccurrence, enforcing every §D.4/§L.4 invariant.
func NewOccurrence(p OccurrenceParams) (Occurrence, error) {
	if err := requireID("occurrence id", p.ID); err != nil {
		return Occurrence{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Occurrence{}, err
	}
	if err := requireID("alert_id", p.AlertID); err != nil {
		return Occurrence{}, err
	}
	if p.Seq < 1 {
		return Occurrence{}, errs.New(errs.KindValidation, "min", "occurrence seq must be >= 1")
	}
	if p.ReopenCount < 0 {
		return Occurrence{}, errs.New(errs.KindValidation, "min", "reopen_count must be >= 0")
	}
	if !p.State.IsOpen() && !p.State.IsTerminal() {
		return Occurrence{}, errs.New(errs.KindValidation, "required", "occurrence state is required")
	}
	if p.StartedAt.IsZero() {
		return Occurrence{}, errs.New(errs.KindValidation, "required", "started_at is required")
	}
	if p.LastObservedAt.IsZero() {
		return Occurrence{}, errs.New(errs.KindValidation, "required", "last_observed_at is required")
	}
	if p.SourceStartsAt.IsZero() {
		return Occurrence{}, errs.New(errs.KindValidation, "required", "source_starts_at is required")
	}
	if p.ReopenOf == p.ID && p.ReopenOf != uuid.Nil {
		return Occurrence{}, errs.New(errs.KindValidation, "field_order",
			"an occurrence cannot reopen itself")
	}

	o := Occurrence{
		id:                p.ID,
		orgID:             p.OrgID,
		alertID:           p.AlertID,
		groupID:           p.GroupID,
		seq:               p.Seq,
		state:             p.State,
		suppressionReason: p.SuppressionReason,
		startedAt:         p.StartedAt.UTC(),
		lastObservedAt:    p.LastObservedAt.UTC(),
		sourceStartsAt:    p.SourceStartsAt.UTC(),
		resolveReason:     p.ResolveReason,
		reopenCount:       p.ReopenCount,
		reopenOf:          p.ReopenOf,
		ackState:          p.AckState,
		ackedBy:           p.AckedBy,
		ackedByLabel:      strings.TrimSpace(p.AckedByLabel),
		ackNote:           p.AckNote,
		ruleSnapshotID:    p.RuleSnapshotID,
		value:             p.Value,
		observedSkew:      p.ObservedSkew,
	}
	if !p.EndedAt.IsZero() {
		o.endedAt = p.EndedAt.UTC()
	}
	if !p.SourceEndsAt.IsZero() {
		o.sourceEndsAt = p.SourceEndsAt.UTC()
	}
	if !p.SourceUpdatedAt.IsZero() {
		o.sourceUpdatedAt = p.SourceUpdatedAt.UTC()
	}
	if !p.AckedAt.IsZero() {
		o.ackedAt = p.AckedAt.UTC()
	}
	if o.ackState.IsZero() {
		o.ackState = AckStateUnacked
	}

	if err := o.check(); err != nil {
		return Occurrence{}, err
	}
	return o, nil
}

// check re-proves the invariants of §L.4 and the §D.4 CHECKs. It runs on every
// construction and after every transition, so no code path can produce an
// Occurrence the database would refuse.
func (o Occurrence) check() error {
	// occ_terminal_ended: terminal iff ended_at is set.
	if o.state.IsTerminal() != !o.endedAt.IsZero() {
		return errs.Newf(errs.KindInternal, "occurrence_terminal_ended",
			"state %q and ended_at disagree", o.state)
	}
	// occ_order_ck / occ_obs_ck
	if !o.endedAt.IsZero() && o.endedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "occurrence_order", "ended_at must be >= started_at")
	}
	if o.lastObservedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "occurrence_observed_order",
			"last_observed_at must be >= started_at")
	}
	// occ_src_order_ck
	if !o.sourceEndsAt.IsZero() && o.sourceEndsAt.Before(o.sourceStartsAt) {
		return errs.New(errs.KindInternal, "occurrence_source_order",
			"source_ends_at must be >= source_starts_at")
	}
	// occ_suppress_ck: suppression_reason exists iff suppressed (C1).
	if (o.state == StateSuppressed) != !o.suppressionReason.IsZero() {
		return errs.New(errs.KindInternal, "occurrence_suppression",
			"suppression_reason exists only while suppressed")
	}
	// occ_resolve_ck / occ_resolve_map_ck: the terminal state and its reason are
	// bound one-to-one, which is what stops oto claiming resolved when it means
	// expired.
	if o.state.IsTerminal() != !o.resolveReason.IsZero() {
		return errs.New(errs.KindInternal, "occurrence_resolve_reason",
			"resolve_reason exists only on a terminal state")
	}
	if o.state == StateResolved && o.resolveReason != ResolveUpstream {
		return errs.New(errs.KindInternal, "occurrence_resolve_map",
			"resolved requires resolve_reason=upstream")
	}
	if o.state == StateExpired && o.resolveReason != ResolveTimeout {
		return errs.New(errs.KindInternal, "occurrence_resolve_map",
			"expired requires resolve_reason=timeout")
	}
	// occ_ack_ck / occ_acklabel_ck / occ_ackorder_ck: ack fields are all-or-nothing.
	if o.ackState.IsAcked() != !o.ackedAt.IsZero() {
		return errs.New(errs.KindInternal, "occurrence_ack",
			"ack_state and acked_at must agree")
	}
	if o.ackedAt.IsZero() != (o.ackedByLabel == "") {
		return errs.New(errs.KindInternal, "occurrence_ack_label",
			"acked_at and acked_by_label must agree")
	}
	if !o.ackedAt.IsZero() && o.ackedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "occurrence_ack_order",
			"acked_at must be >= started_at")
	}
	// occ_acknote_ck
	if len(o.ackNote) > MaxAckNoteBytes {
		return errs.Newf(errs.KindValidation, "max_length",
			"ack note must have at most %d characters", MaxAckNoteBytes)
	}
	return nil
}

// ID is the occurrence's uuidv7.
func (o Occurrence) ID() uuid.UUID { return o.id }

// OrgID is the tenant this occurrence belongs to.
func (o Occurrence) OrgID() uuid.UUID { return o.orgID }

// AlertID is the Alert this episode belongs to.
func (o Occurrence) AlertID() uuid.UUID { return o.alertID }

// GroupID is the AlertGroup generation this occurrence joined, or uuid.Nil.
func (o Occurrence) GroupID() uuid.UUID { return o.groupID }

// Seq is the 1-based episode number within the Alert.
func (o Occurrence) Seq() int { return o.seq }

// State is what the world is doing to this occurrence.
func (o Occurrence) State() State { return o.state }

// SuppressionReason says why the occurrence is suppressed, set only while it is.
func (o Occurrence) SuppressionReason() SuppressionReason { return o.suppressionReason }

// StartedAt is oto's clock reading when the episode opened.
func (o Occurrence) StartedAt() time.Time { return o.startedAt }

// EndedAt is oto's clock reading when the episode ended, zero while open.
func (o Occurrence) EndedAt() time.Time { return o.endedAt }

// LastObservedAt is when oto last heard about this occurrence.
func (o Occurrence) LastObservedAt() time.Time { return o.lastObservedAt }

// SourceStartsAt is Alertmanager's `startsAt` — the upstream claim.
func (o Occurrence) SourceStartsAt() time.Time { return o.sourceStartsAt }

// SourceEndsAt is Alertmanager's `endsAt`, zero when no end time is known. It is
// what the reaper measures resolve_grace against.
func (o Occurrence) SourceEndsAt() time.Time { return o.sourceEndsAt }

// SourceUpdatedAt is Alertmanager's `updatedAt`, zero when unknown.
func (o Occurrence) SourceUpdatedAt() time.Time { return o.sourceUpdatedAt }

// ResolveReason says how the occurrence ended: upstream said so, or oto timed it out.
func (o Occurrence) ResolveReason() ResolveReason { return o.resolveReason }

// ReopenCount is how many times this occurrence re-fired inside refire_grace.
func (o Occurrence) ReopenCount() int { return o.reopenCount }

// ReopenOf is the previous occurrence a T7 re-fire followed, or uuid.Nil.
func (o Occurrence) ReopenOf() uuid.UUID { return o.reopenOf }

// AckState is what humans have done. It is orthogonal to State.
func (o Occurrence) AckState() AckState { return o.ackState }

// AckedBy is the acknowledging user, or uuid.Nil.
func (o Occurrence) AckedBy() uuid.UUID { return o.ackedBy }

// AckedByLabel is the acknowledger's denormalised, immutable display name.
func (o Occurrence) AckedByLabel() string { return o.ackedByLabel }

// AckedAt is when the occurrence was acknowledged, zero when it is not.
func (o Occurrence) AckedAt() time.Time { return o.ackedAt }

// AckNote is the free-text note left with the acknowledgement.
func (o Occurrence) AckNote() string { return o.ackNote }

// RuleSnapshotID is the RuleSnapshot bound to this occurrence — what the rule
// said at the moment it fired (R6).
func (o Occurrence) RuleSnapshotID() uuid.UUID { return o.ruleSnapshotID }

// Value is the sample value that fired the rule, or nil when upstream sent none.
func (o Occurrence) Value() *float64 {
	if o.value == nil {
		return nil
	}
	v := *o.value
	return &v
}

// ObservedSkew is the measured difference between oto's clock and upstream's.
func (o Occurrence) ObservedSkew() time.Duration { return o.observedSkew }

// IsOpen reports whether the episode is still running.
func (o Occurrence) IsOpen() bool { return o.endedAt.IsZero() }

// Duration is how long the episode ran, measured to endedAt once closed and to
// asOf while still open. It takes the clock reading as a parameter: the domain
// never calls time.Now().
func (o Occurrence) Duration(asOf time.Time) time.Duration {
	if !o.endedAt.IsZero() {
		return o.endedAt.Sub(o.startedAt)
	}
	return asOf.Sub(o.startedAt)
}

// WithGroup binds the occurrence to an AlertGroup generation.
func (o Occurrence) WithGroup(groupID uuid.UUID) (Occurrence, error) {
	if err := requireID("group_id", groupID); err != nil {
		return Occurrence{}, err
	}
	o.groupID = groupID
	return o, nil
}

// WithRuleSnapshot binds the RuleSnapshot captured at fire time (R6).
func (o Occurrence) WithRuleSnapshot(snapshotID uuid.UUID) (Occurrence, error) {
	if err := requireID("rule_snapshot_id", snapshotID); err != nil {
		return Occurrence{}, err
	}
	o.ruleSnapshotID = snapshotID
	return o, nil
}
