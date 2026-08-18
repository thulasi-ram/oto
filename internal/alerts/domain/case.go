package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxAckNoteBytes bounds an acknowledgement note (case_acknote_ck).
const MaxAckNoteBytes = 2000

// Case is an AlertCase: ONE contiguous firing episode of an Alert,
// identified by (alert_id, seq). It is what a human acknowledges and what firing
// duration is measured over. The authoritative lifecycle state machine runs
// here; Alert.state and AlertGroup.state are projections of it.
//
// Every field is unexported. A Case can only come from a constructor or
// from a transition, so an illegal combination — `resolved` without an
// `ended_at`, `acked` without an `acked_at` — cannot be built at all. Each
// invariant below is mirrored by a DDL CHECK in §D.4, belt and braces.
type Case struct {
	id      uuid.UUID
	orgID   uuid.UUID
	alertID uuid.UUID
	groupID uuid.UUID
	seq     int

	state             State
	suppressionReason SuppressionReason
	// suppressedBy is `alert_cases.suppressed_by`: the ids Alertmanager
	// named — `silencedBy`, `inhibitedBy`, `mutedBy` — on the observation that
	// suppressed this episode. It answers WHICH silence is muting the alert,
	// which `suppression_reason` alone never could.
	suppressedBy SuppressedBy

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

	// stateVersion is the row's optimistic lock. It is READ from the database and
	// asserted on write; the machine never sets it, because a version the domain
	// invented would guard nothing.
	stateVersion int
	// suppressCount is how many times this episode has ENTERED `suppressed`. It is
	// reopenCount's analogue for the suppressed path.
	suppressCount int

	ackState     AckState
	ackedBy      uuid.UUID
	ackedByLabel string
	ackedAt      time.Time
	ackNote      string

	ruleSnapshotID uuid.UUID
	value          *float64
	observedSkew   time.Duration
}

// CaseParams is the full constructor input for an AlertCase. It is
// also the rehydration shape: a repository maps a row into it, and the
// constructor re-proves every invariant at the boundary.
type CaseParams struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	AlertID uuid.UUID
	// GroupID is the AlertGroup generation this case belongs to, or
	// uuid.Nil before it has been bound.
	GroupID uuid.UUID
	// Seq is the 1-based episode number within the Alert.
	Seq int

	State             State
	SuppressionReason SuppressionReason
	// SuppressedBy is `alert_cases.suppressed_by` as read. It is carried
	// only while the case is suppressed; see the SuppressedBy accessor.
	SuppressedBy SuppressedBy

	StartedAt      time.Time
	EndedAt        time.Time
	LastObservedAt time.Time

	SourceStartsAt  time.Time
	SourceEndsAt    time.Time
	SourceUpdatedAt time.Time

	ResolveReason ResolveReason
	ReopenCount   int
	// ReopenOf is the previous case when T7 followed a close.
	ReopenOf uuid.UUID

	// StateVersion is `alert_cases.state_version` as read. A zero value
	// rehydrates as 1 (the column's DEFAULT), so an in-memory case built for
	// a test is still a legal compare-and-set subject.
	StateVersion int
	// SuppressCount is `alert_cases.suppress_count` as read.
	SuppressCount int

	AckState     AckState
	AckedBy      uuid.UUID
	AckedByLabel string
	AckedAt      time.Time
	AckNote      string

	RuleSnapshotID uuid.UUID
	// Value is the sample value that fired the rule, when upstream supplied one.
	Value *float64
	// ObservedSkew is received_at - startsAt for the observation that opened or
	// last touched this case (C12).
	ObservedSkew time.Duration
}

// NewCase builds an AlertCase, enforcing every §D.4/§L.4 invariant.
func NewCase(p CaseParams) (Case, error) {
	if err := requireID("case id", p.ID); err != nil {
		return Case{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Case{}, err
	}
	if err := requireID("alert_id", p.AlertID); err != nil {
		return Case{}, err
	}
	if p.Seq < 1 {
		return Case{}, errs.New(errs.KindValidation, "min", "case seq must be >= 1")
	}
	if p.ReopenCount < 0 {
		return Case{}, errs.New(errs.KindValidation, "min", "reopen_count must be >= 0")
	}
	if p.SuppressCount < 0 {
		return Case{}, errs.New(errs.KindValidation, "min", "suppress_count must be >= 0")
	}
	if p.StateVersion < 0 {
		return Case{}, errs.New(errs.KindValidation, "min", "state_version must be >= 1")
	}
	if !p.State.IsOpen() && !p.State.IsTerminal() {
		return Case{}, errs.New(errs.KindValidation, "required", "case state is required")
	}
	if p.StartedAt.IsZero() {
		return Case{}, errs.New(errs.KindValidation, "required", "started_at is required")
	}
	if p.LastObservedAt.IsZero() {
		return Case{}, errs.New(errs.KindValidation, "required", "last_observed_at is required")
	}
	if p.SourceStartsAt.IsZero() {
		return Case{}, errs.New(errs.KindValidation, "required", "source_starts_at is required")
	}
	if p.ReopenOf == p.ID && p.ReopenOf != uuid.Nil {
		return Case{}, errs.New(errs.KindValidation, "field_order",
			"a case cannot reopen itself")
	}

	o := Case{
		id:                p.ID,
		orgID:             p.OrgID,
		alertID:           p.AlertID,
		groupID:           p.GroupID,
		seq:               p.Seq,
		state:             p.State,
		suppressionReason: p.SuppressionReason,
		suppressedBy:      p.SuppressedBy,
		startedAt:         p.StartedAt.UTC(),
		lastObservedAt:    p.LastObservedAt.UTC(),
		sourceStartsAt:    p.SourceStartsAt.UTC(),
		resolveReason:     p.ResolveReason,
		reopenCount:       p.ReopenCount,
		reopenOf:          p.ReopenOf,
		stateVersion:      max(p.StateVersion, 1),
		suppressCount:     p.SuppressCount,
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
		return Case{}, err
	}
	return o, nil
}

// check re-proves the invariants of §L.4 and the §D.4 CHECKs. It runs on every
// construction and after every transition, so no code path can produce an
// Case the database would refuse.
func (o Case) check() error {
	// case_terminal_ended: terminal iff ended_at is set.
	if o.state.IsTerminal() != !o.endedAt.IsZero() {
		return errs.Newf(errs.KindInternal, "case_terminal_ended",
			"state %q and ended_at disagree", o.state)
	}
	// case_order_ck / case_obs_ck
	if !o.endedAt.IsZero() && o.endedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "case_order", "ended_at must be >= started_at")
	}
	if o.lastObservedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "case_observed_order",
			"last_observed_at must be >= started_at")
	}
	// case_src_order_ck
	if !o.sourceEndsAt.IsZero() && o.sourceEndsAt.Before(o.sourceStartsAt) {
		return errs.New(errs.KindInternal, "case_source_order",
			"source_ends_at must be >= source_starts_at")
	}
	// case_suppress_ck: suppression_reason exists iff suppressed (C1).
	if (o.state == StateSuppressed) != !o.suppressionReason.IsZero() {
		return errs.New(errs.KindInternal, "case_suppression",
			"suppression_reason exists only while suppressed")
	}
	// case_resolve_ck / case_resolve_map_ck: the terminal state and its reason are
	// bound one-to-one, which is what stops oto claiming resolved when it means
	// expired.
	if o.state.IsTerminal() != !o.resolveReason.IsZero() {
		return errs.New(errs.KindInternal, "case_resolve_reason",
			"resolve_reason exists only on a terminal state")
	}
	if o.state == StateResolved && o.resolveReason != ResolveUpstream {
		return errs.New(errs.KindInternal, "case_resolve_map",
			"resolved requires resolve_reason=upstream")
	}
	if o.state == StateExpired && o.resolveReason != ResolveTimeout {
		return errs.New(errs.KindInternal, "case_resolve_map",
			"expired requires resolve_reason=timeout")
	}
	// case_ack_ck / case_acklabel_ck / case_ackorder_ck: ack fields are all-or-nothing.
	if o.ackState.IsAcked() != !o.ackedAt.IsZero() {
		return errs.New(errs.KindInternal, "case_ack",
			"ack_state and acked_at must agree")
	}
	if o.ackedAt.IsZero() != (o.ackedByLabel == "") {
		return errs.New(errs.KindInternal, "case_ack_label",
			"acked_at and acked_by_label must agree")
	}
	if !o.ackedAt.IsZero() && o.ackedAt.Before(o.startedAt) {
		return errs.New(errs.KindInternal, "case_ack_order",
			"acked_at must be >= started_at")
	}
	// case_acknote_ck
	if len(o.ackNote) > MaxAckNoteBytes {
		return errs.Newf(errs.KindValidation, "max_length",
			"ack note must have at most %d characters", MaxAckNoteBytes)
	}
	return nil
}

// ID is the case's uuidv7.
func (o Case) ID() uuid.UUID { return o.id }

// OrgID is the tenant this case belongs to.
func (o Case) OrgID() uuid.UUID { return o.orgID }

// AlertID is the Alert this episode belongs to.
func (o Case) AlertID() uuid.UUID { return o.alertID }

// GroupID is the AlertGroup generation this case joined, or uuid.Nil.
func (o Case) GroupID() uuid.UUID { return o.groupID }

// Seq is the 1-based episode number within the Alert.
func (o Case) Seq() int { return o.seq }

// State is what the world is doing to this case.
func (o Case) State() State { return o.state }

// SuppressionReason says why the case is suppressed, set only while it is.
func (o Case) SuppressionReason() SuppressionReason { return o.suppressionReason }

// SuppressedBy names WHICH upstream objects are suppressing this episode:
// Alertmanager's `silencedBy`, `inhibitedBy` and `mutedBy`, all three.
//
// ⛔ IT IS EMPTY UNLESS THE CASE IS SUPPRESSED, and the gate is here rather
// than at every write site for the same reason `case_suppress_ck` ties
// `suppression_reason` to `state = 'suppressed'`: witnesses left behind on an
// case that is demonstrably firing would make oto keep saying "silenced by
// <id>" about an alert nobody is silencing. The persistence path clears the
// column on every non-suppressed edge; this makes a row written before it did —
// or by anything else — read the same way.
func (o Case) SuppressedBy() SuppressedBy {
	if o.state != StateSuppressed {
		return SuppressedBy{}
	}
	return o.suppressedBy
}

// StartedAt is oto's clock reading when the episode opened.
func (o Case) StartedAt() time.Time { return o.startedAt }

// EndedAt is oto's clock reading when the episode ended, zero while open.
func (o Case) EndedAt() time.Time { return o.endedAt }

// LastObservedAt is when oto last heard about this case.
func (o Case) LastObservedAt() time.Time { return o.lastObservedAt }

// SourceStartsAt is Alertmanager's `startsAt` — the upstream claim.
func (o Case) SourceStartsAt() time.Time { return o.sourceStartsAt }

// SourceEndsAt is Alertmanager's `endsAt`, zero when no end time is known. It is
// what the reaper measures resolve_grace against.
func (o Case) SourceEndsAt() time.Time { return o.sourceEndsAt }

// SourceUpdatedAt is Alertmanager's `updatedAt`, zero when unknown.
func (o Case) SourceUpdatedAt() time.Time { return o.sourceUpdatedAt }

// ResolveReason says how the case ended: upstream said so, or oto timed it out.
func (o Case) ResolveReason() ResolveReason { return o.resolveReason }

// ReopenCount is how many times this case re-fired inside refire_grace.
func (o Case) ReopenCount() int { return o.reopenCount }

// ReopenOf is the previous case a T7 re-fire followed, or uuid.Nil.
func (o Case) ReopenOf() uuid.UUID { return o.reopenOf }

// StateVersion is the row's optimistic lock (case_sver_ck, >= 1). It is the whole
// compare-and-set predicate for a §B.3 transition: see TransitionPrecondition.
func (o Case) StateVersion() int { return o.stateVersion }

// SuppressCount is how many times this episode has entered `suppressed`. It is
// what makes T3 and T4's §C.8 dedupe keys stable, exactly as ReopenCount does for
// T8 — a suppression is a COUNTED fact, not a timestamped one.
func (o Case) SuppressCount() int { return o.suppressCount }

// AckState is what humans have done. It is orthogonal to State.
func (o Case) AckState() AckState { return o.ackState }

// AckedBy is the acknowledging user, or uuid.Nil.
func (o Case) AckedBy() uuid.UUID { return o.ackedBy }

// AckedByLabel is the acknowledger's denormalised, immutable display name.
func (o Case) AckedByLabel() string { return o.ackedByLabel }

// AckedAt is when the case was acknowledged, zero when it is not.
func (o Case) AckedAt() time.Time { return o.ackedAt }

// AckNote is the free-text note left with the acknowledgement.
func (o Case) AckNote() string { return o.ackNote }

// RuleSnapshotID is the RuleSnapshot bound to this case — what the rule
// said at the moment it fired (R6).
func (o Case) RuleSnapshotID() uuid.UUID { return o.ruleSnapshotID }

// Value is the sample value that fired the rule, or nil when upstream sent none.
func (o Case) Value() *float64 {
	if o.value == nil {
		return nil
	}
	v := *o.value
	return &v
}

// ObservedSkew is the measured difference between oto's clock and upstream's.
func (o Case) ObservedSkew() time.Duration { return o.observedSkew }

// IsOpen reports whether the episode is still running.
func (o Case) IsOpen() bool { return o.endedAt.IsZero() }

// Duration is how long the episode ran, measured to endedAt once closed and to
// asOf while still open. It takes the clock reading as a parameter: the domain
// never calls time.Now().
func (o Case) Duration(asOf time.Time) time.Duration {
	if !o.endedAt.IsZero() {
		return o.endedAt.Sub(o.startedAt)
	}
	return asOf.Sub(o.startedAt)
}

// WithGroup binds the case to an AlertGroup generation.
func (o Case) WithGroup(groupID uuid.UUID) (Case, error) {
	if err := requireID("group_id", groupID); err != nil {
		return Case{}, err
	}
	o.groupID = groupID
	return o, nil
}

// WithRuleSnapshot binds the RuleSnapshot captured at fire time (R6).
func (o Case) WithRuleSnapshot(snapshotID uuid.UUID) (Case, error) {
	if err := requireID("rule_snapshot_id", snapshotID); err != nil {
		return Case{}, err
	}
	o.ruleSnapshotID = snapshotID
	return o, nil
}
