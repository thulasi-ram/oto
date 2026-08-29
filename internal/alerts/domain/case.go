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
// ⭐⭐ ITS OWN STATE IS `open | closed` AND NOTHING ELSE (ADR 0040). The §B.2
// values belong to the ALERT; this episode's part in them is `resolveReason` (WHY
// it ended), which AlertState reads the terminal half back out of. A Case is
// STRICTLY TERMINAL: it closes once and a re-fire opens the next `seq`,
// unacknowledged.
//
// ⭐ `suppressionReason` IS NOT PART OF THAT READING ANY MORE (ADR 0041). It is
// the SUPPRESSION AXIS — which silence muted this firing — and it sits beside the
// state rather than inside it, because a silenced alert is still firing and every
// counter has to say so. `lifecyclePhase` is the one reading that still folds the
// two together, and it exists only for the §B.3 transition table.
//
// Every field is unexported. A Case can only come from a constructor or
// from a transition, so an illegal combination — `resolved` without an
// `ended_at`, `acked` without an `acked_at` — cannot be built at all. Each
// invariant below is mirrored by a DDL CHECK in §D.4, belt and braces.
type Case struct {
	id      uuid.UUID
	orgID   uuid.UUID
	alertID uuid.UUID
	// ⛔ `groupID uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`). A Case IS
	// the conversation now, so it joins no container; `alert_cases.group_id` is
	// dropped by 00069 and `repository/case.go` stopped supplying a value before
	// this field went, which is why every rehydrated Case had been answering
	// `uuid.Nil` in the meantime.
	seq int
	// number is `alert_cases.number`: the case's NAME within its org — 1-based,
	// monotonic, and the thing a human quotes. It is not `seq`, which is the
	// firing ordinal within one Alert; forty alerts that have each fired once
	// carry forty different numbers and forty `seq` of 1.
	//
	// ⛔ THE MACHINE NEVER INVENTS IT, exactly as it never invents `stateVersion`.
	// The value is allocated by the INSERT, from `org_case_numbers`, and comes
	// back on the same statement — so a Case built by `OpenNewCase` and not yet
	// written answers 0 here, and the persisted one the repository returns
	// answers the real name. A number chosen in Go would be a guess about a
	// counter another transaction may already have moved.
	number int64

	state             CaseState
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

	// resolvePendingAt and resolvePendingEndAt are THE PENDING CLOSE — the
	// `alert_cases` columns migration 00057 added, and the whole of the case
	// retention window W.
	//
	// ⭐⭐ W MOVES *WHEN* A CASE CLOSES AND NOTHING ELSE. A case whose alert has
	// resolved stays OPEN for W and closes only once the alert has stayed resolved
	// for W, so a re-fire inside W is an ordinary repeat observation (T2) landing in
	// the still-open episode rather than the next `seq`. That is what turns six
	// flaps into ONE case, one notification and one thread reply — the noise never
	// exists, instead of existing and being withheld at delivery, which is the
	// distinction §B.6 refuses to blur.
	//
	// ⛔ IT IS A DELAYED CLOSE AND NEVER A REOPEN. A Case is still strictly
	// terminal (ADR 0040): `ended_at` is written ONCE, `case_terminal_ended` still
	// refuses a closed row with no end, and `case_pending_open_ck` refuses a
	// pending close on a row that has one. T8 is not coming back.
	//
	// ⭐ WHY TWO VALUES AND NOT ONE. `resolvePendingAt` is oto's clock: when the
	// close falls due, i.e. the LAST upstream resolve plus W. It moves forward on
	// each fresh resolve inside the window, because the rule is "stayed resolved
	// for W" and not "resolved W ago". `resolvePendingEndAt` is the `ended_at` that
	// close will stamp — the UPSTREAM claim, already clamped by §B.3.2 — so W is
	// never charged to the signal's firing duration (R8). Closing at the sweep's
	// clock instead would make every reader of `ended_at` report an episode W
	// longer than the signal actually burned.
	//
	// ⭐ BOTH ARE ZERO ON EVERY ROW UNTIL AN OPERATOR SETS W. W defaults to 0 and
	// the T5 arm's deferral branch is not entered at 0, so a deployment that has
	// configured nothing has no pending closes at all and reads exactly as it read
	// before this field existed.
	resolvePendingAt    time.Time
	resolvePendingEndAt time.Time

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
	// Seq is the 1-based episode number within the Alert.
	Seq int
	// Number is `alert_cases.number` as READ. Zero on a Case that has not been
	// written yet: the INSERT allocates it. See the field on Case.
	Number int64

	State             CaseState
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

	// ResolvePendingAt and ResolvePendingEndAt are `alert_cases.resolve_pending_at`
	// and `.resolve_pending_end_at` as read (migration 00057). Both zero — which is
	// every row until an operator sets a retention window — rehydrates a case with
	// no pending close, which is what every case was before W existed.
	ResolvePendingAt    time.Time
	ResolvePendingEndAt time.Time

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
	if p.SuppressCount < 0 {
		return Case{}, errs.New(errs.KindValidation, "min", "suppress_count must be >= 0")
	}
	if p.StateVersion < 0 {
		return Case{}, errs.New(errs.KindValidation, "min", "state_version must be >= 1")
	}
	if p.State.IsZero() {
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

	o := Case{
		id:                p.ID,
		orgID:             p.OrgID,
		alertID:           p.AlertID,
		seq:               p.Seq,
		number:            p.Number,
		state:             p.State,
		suppressionReason: p.SuppressionReason,
		suppressedBy:      p.SuppressedBy,
		startedAt:         p.StartedAt.UTC(),
		lastObservedAt:    p.LastObservedAt.UTC(),
		sourceStartsAt:    p.SourceStartsAt.UTC(),
		resolveReason:     p.ResolveReason,
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
	if !p.ResolvePendingAt.IsZero() {
		o.resolvePendingAt = p.ResolvePendingAt.UTC()
	}
	if !p.ResolvePendingEndAt.IsZero() {
		o.resolvePendingEndAt = p.ResolvePendingEndAt.UTC()
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
	// case_terminal_ended: closed iff ended_at is set.
	if o.state.IsClosed() != !o.endedAt.IsZero() {
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
	// case_suppress_ck: a CLOSED episode cannot be suppressed (C1).
	//
	// ⭐ THIS IS ONE DIRECTION AND THE OLD INVARIANT WAS TWO, AND NOTHING WAS
	// WEAKENED BY THE LOSS. It used to read `(state == suppressed) ==
	// (suppression_reason set)`; since ADR 0040 "suppressed" IS "open with a
	// suppression reason" — AlertState is the definition — so the other direction
	// became a tautology rather than a check. What remains is the half that can
	// still be false: a reason left behind on an episode that has ended, which
	// would make oto keep saying "silenced by <id>" about a firing that is over.
	if !o.suppressionReason.IsZero() && !o.state.IsOpen() {
		return errs.New(errs.KindInternal, "case_suppression",
			"suppression_reason exists only while the episode is open")
	}
	// case_resolve_ck: a closed episode says why it closed, and an open one has
	// nothing to say. `resolve_reason` is the SOLE record of resolved-versus-
	// expired since ADR 0040, which is what makes AlertState total below and what
	// stops oto claiming resolved when it means expired.
	if o.state.IsClosed() != !o.resolveReason.IsZero() {
		return errs.New(errs.KindInternal, "case_resolve_reason",
			"resolve_reason exists exactly on a closed episode")
	}
	// case_pending_pair_ck / case_pending_open_ck / case_pending_order_ck /
	// case_pending_supp_ck (migration 00057) — the four DDL CHECKs that keep a
	// DELAYED close from becoming a second one.
	if o.resolvePendingAt.IsZero() != o.resolvePendingEndAt.IsZero() {
		return errs.New(errs.KindInternal, "case_pending_pair",
			"resolve_pending_at and resolve_pending_end_at must agree")
	}
	if !o.resolvePendingAt.IsZero() {
		// The one that makes the close SINGLE-SHOT. A closed episode carrying a
		// pending close is a second close waiting to happen, and a Case closes
		// exactly once (ADR 0040).
		if !o.state.IsOpen() {
			return errs.New(errs.KindInternal, "case_pending_open",
				"a pending close exists only while the episode is open")
		}
		if o.resolvePendingEndAt.Before(o.startedAt) {
			return errs.New(errs.KindInternal, "case_pending_order",
				"resolve_pending_end_at must be >= started_at")
		}
		// An upstream resolve is POSITIVE PROOF OF NON-SUPPRESSION — Alertmanager
		// would not have delivered it otherwise, the same argument §B.3.1 uses to
		// let ingest drive T4 — so the deferral clears the witnesses exactly as an
		// immediate T5 does. Nothing may say "silenced by <id>" about an episode
		// whose alert upstream has already called resolved.
		if !o.suppressionReason.IsZero() {
			return errs.New(errs.KindInternal, "case_pending_supp",
				"a pending close cannot coexist with a suppression reason")
		}
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

// ⛔ `GroupID() uuid.UUID` WAS AN ACCESSOR HERE AND IS DELETED (git-bug `7570090`).
// `repository/case.go` recorded the end state this completes: with the column
// dropped there was no value to supply, so the accessor answered `uuid.Nil` for
// every Case ever rehydrated and its three readers — `alerts/api/map.go`, the event
// payload in `alerts/service/service.go`, and `alerts/service/lifecycle.go` — were
// each copying a zero into a field that meant "no group" and "unknown group"
// indistinguishably. A reader that cannot tell those apart is why the accessor goes
// rather than being left to answer nil politely.

// Seq is the 1-based episode number within the Alert.
func (o Case) Seq() int { return o.seq }

// Number is the case's name within its org: 1-based, monotonic, allocated by the
// INSERT. Zero on a Case the repository has not written yet.
func (o Case) Number() int64 { return o.number }

// State is the case's OWN state: open while the episode runs, closed once it has
// ended. These are the only two values `alert_cases.state` may hold (ADR 0040).
func (o Case) State() CaseState { return o.state }

// AlertState is the §B.2 state of the ALERT as this episode last observed it,
// DERIVED and never stored (ADR 0040). It is exactly what `alerts.state` holds:
// `firing | resolved | expired`.
//
// ⛔⭐ IT DOES NOT CONSULT SUPPRESSION, AND THAT IS ADR 0041. It used to test
// suppression FIRST and return StateSuppressed, which made StateFiring
// UNREACHABLE for a silenced open episode — and this method is what
// `AlertProjection` writes to `alerts.state`, so every reader asking
// `state = 'firing'` silently missed every alert that was firing while silenced.
// A silence is the most common thing an operator does to a firing alert, so what
// that lost was not an edge case: "is anything still on fire?" could not be
// answered from the column whose job is to answer it.
//
// Suppression is a STATEMENT ABOUT ANOTHER SYSTEM — is Alertmanager delivering
// this — and the signal goes on firing underneath it. It is therefore an
// orthogonal axis, read from `SuppressionReason()` beside this value and never
// inside it, exactly as snooze has been since 00017 and for the argument written
// out in snooze.go:25-32.
//
// ⭐ THE DERIVATION IS STILL TOTAL, and `check` is what makes it so: a closed
// episode always carries a `resolve_reason` and it is one of exactly two values,
// so the closed half is exhaustive; the open half is now a single answer and
// cannot fail to be.
//
// It returns StateNone only for the zero Case, which is the state T1's row comes
// from — an Alert with no episode at all.
func (o Case) AlertState() State {
	switch {
	case o.state.IsOpen():
		return StateFiring
	case o.state.IsClosed() && o.resolveReason == ResolveTimeout:
		return StateExpired
	case o.state.IsClosed():
		return StateResolved
	default:
		return StateNone
	}
}

// lifecyclePhase is the SPEC §B.3 machine's reading of this episode, and it is
// the ONLY reading that still folds suppression into a State value.
//
// ⭐⭐ IT IS SEPARATE FROM AlertState ON PURPOSE, AND THE SPLIT IS THE WHOLE OF
// ADR 0041 IN TWO METHODS. The transition table's `from`/`to` columns route T3
// (firing → suppressed), T4 (suppressed → firing), and the suppressed arms of T5
// and T6; collapsing them would make four edges unreachable and stop the Case
// recording that it was ever muted. But `alerts.state` is a PROJECTION READ BY
// AGGREGATES, and there `suppressed` hid firing alerts inside another word.
//
// So the machine keeps its four phases and the column loses one: the same fact,
// read for two different purposes, and neither purpose has to lie to serve the
// other. It is unexported because a caller outside this package asking "what
// phase is the machine in?" is asking the wrong question — it wants AlertState
// and SuppressionReason, which are the two axes.
func (o Case) lifecyclePhase() State {
	if o.state.IsOpen() && !o.suppressionReason.IsZero() {
		return StateSuppressed
	}
	return o.AlertState()
}

// SuppressionReason says why the case is suppressed, set only while it is.
func (o Case) SuppressionReason() SuppressionReason { return o.suppressionReason }

// SuppressedBy names WHICH upstream objects are suppressing this episode:
// Alertmanager's `silencedBy`, `inhibitedBy` and `mutedBy`, all three.
//
// ⛔ IT IS EMPTY UNLESS THE CASE IS SUPPRESSED, and the gate is here rather than
// at every write site for the same reason `case_suppress_ck` keeps
// `suppression_reason` off a closed episode: witnesses left behind on a case that
// is demonstrably firing would make oto keep saying "silenced by <id>" about an
// alert nobody is silencing. The persistence path clears the column on every
// non-suppressed edge; this makes a row written before it did — or by anything
// else — read the same way.
//
// Since ADR 0041 the gate asks `suppressionReason` DIRECTLY rather than asking
// AlertState, because AlertState no longer knows: suppression is an axis beside
// the state, so "is this suppressed" is the presence of a reason and nothing
// else. `case_suppress_ck` and `check` together keep a reason off a closed
// episode, so this is still the same question it was asking before.
func (o Case) SuppressedBy() SuppressedBy {
	if o.suppressionReason.IsZero() {
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

// ResolvePendingAt is when this episode's DELAYED CLOSE falls due — the last
// upstream resolve plus the retention window W — and is zero when no close is
// pending, which is every episode on a deployment that has set no W.
func (o Case) ResolvePendingAt() time.Time { return o.resolvePendingAt }

// ResolvePendingEndAt is the `ended_at` the pending close will stamp: the upstream
// claim from the resolve observation, already clamped to >= `started_at` (§B.3.2).
// It is what keeps W off the signal's firing duration.
func (o Case) ResolvePendingEndAt() time.Time { return o.resolvePendingEndAt }

// ClosePending reports that upstream has resolved this episode and the close is
// waiting for the retention window to elapse.
//
// ⛔ IT IS NOT "THE CASE IS CLOSED" AND IT IS NOT "THE ALERT IS FIRING AGAIN".
// The episode is OPEN — `IsOpen`, `AlertState` and `alerts.state` all say firing,
// because a case is the unit oto reasons in and this one has not ended. What this
// answers is the one question those cannot: is there a resolve on the row already,
// waiting to be spent? The reaper asks it, because an episode holding a resolve is
// not one oto has stopped hearing about, and T6 must not overwrite `upstream` with
// `timeout` — which is exactly the fabrication 00007 forbids.
func (o Case) ClosePending() bool { return !o.resolvePendingAt.IsZero() }

// CloseDue reports that a pending close has come due as of asOf. The clock arrives
// as a parameter: the domain never calls time.Now().
func (o Case) CloseDue(asOf time.Time) bool {
	return o.ClosePending() && !asOf.Before(o.resolvePendingAt)
}

// StateVersion is the row's optimistic lock (case_sver_ck, >= 1). It is the whole
// compare-and-set predicate for a §B.3 transition: see TransitionPrecondition.
func (o Case) StateVersion() int { return o.stateVersion }

// SuppressCount is how many times this episode has entered `suppressed`. It is
// what makes T3 and T4's §C.8 dedupe keys stable — a suppression is a COUNTED
// fact, not a timestamped one, and two passes over the same suppression must not
// mint two events.
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
//
// ⭐ THE RETENTION WINDOW IS NOT CHARGED TO THE SIGNAL, and this is where that is
// visible. Once closed, `endedAt` is the UPSTREAM claim the pending close carried
// (`resolvePendingEndAt`), never the sweep's clock, so a flap damped into one case
// measures from its first firing to its last resolve and gains no W. While a close
// is pending the episode is still open and this still measures to `asOf`, which
// grows by up to W and then settles back on the true end at close — the one
// transient the design accepts, because the alternative is a case that reports a
// duration before it has ended.
func (o Case) Duration(asOf time.Time) time.Duration {
	if !o.endedAt.IsZero() {
		return o.endedAt.Sub(o.startedAt)
	}
	return asOf.Sub(o.startedAt)
}

// ⛔ `WithGroup(groupID uuid.UUID)` WAS HERE AND IS DELETED (git-bug `7570090`). It
// bound a Case to an `alert_groups` generation and refused `uuid.Nil`, which is the
// tell that it was never optional: a group id was a REQUIRED late binding, applied
// by the ingest orchestrator between the case opening and the state machine. There
// is no generation to bind and no orchestrator to bind it. `WithRuleSnapshot` below
// is the sibling that survives, because a rule snapshot is a fact about the alert
// rather than a container it was filed into.

// WithRuleSnapshot binds the RuleSnapshot captured at fire time (R6).
func (o Case) WithRuleSnapshot(snapshotID uuid.UUID) (Case, error) {
	if err := requireID("rule_snapshot_id", snapshotID); err != nil {
		return Case{}, err
	}
	o.ruleSnapshotID = snapshotID
	return o, nil
}
