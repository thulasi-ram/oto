package domain

// THE INCIDENT (ADR 0052 §1–§4): a set of one or more Cases drawn together as one
// story. An Alert has Cases; an Incident spans Cases.
//
// ⛔ WHAT IS NOT IN THIS FILE IS THE DESIGN. There is no `Status`, no `Lead`, no
// `Severity`, no `ClosedAt` and no setter for `State`, and there never will be:
// the owner ruled the whole RESPONSE — "mitigated", who leads it, how bad a human
// says it is, the comms, the write-up — out of oto, because the incident tool an
// Incident is declared to already does it better (§5). What oto keeps is the
// GROUPING, and a grouping of signals is a fact about signals: it is `active`
// while any member Case is open and `quiet` otherwise, and that reading is the
// only state an Incident has.
//
// ⭐ THE TYPES BELOW ARE READ MODELS AND REQUESTS, NOT AN AGGREGATE WITH METHODS
// THAT MUTATE IT. Every write an Incident admits — draw, add, remove, move — is
// one membership row appearing or being tombstoned, and the invariant that
// matters across them, AT MOST ONE INCIDENT PER CASE, is a partial unique index
// (`incident_members_case_live_uniq`, migration 00083) rather than a field this
// package could hold. A domain object that "knew" its Case was in no other
// Incident would be a snapshot of a race.

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring migration 00083's CHECKs and the contract.
const (
	// MaxActorLabelBytes mirrors `incidents_label_ck` and its two siblings on
	// `incident_members`, and `IncidentAttributionDTO.label`'s maxLength.
	MaxActorLabelBytes = 200
	// MaxCasesPerDraw mirrors `CreateIncidentRequest.case_ids`' maxItems. A draw
	// over more Cases than this is a Correlator's job, not a hand's.
	MaxCasesPerDraw = 100
	// MaxAlertnames mirrors `IncidentDTO.alertnames`' maxItems: enough for a list
	// row to say what the story is about, never the whole membership.
	MaxAlertnames = 10
)

// State is an Incident's ONLY state, and it is DERIVED (ADR 0052 §3).
//
// ⛔ THERE IS NO CONSTRUCTOR FROM A STRING, AND THAT IS THE POINT. Every other
// closed enum in oto has a `NewXxx(s string)` because it is read back off a
// column; this one has no column to read back from. The only way to hold a
// State is `StateOf`, which takes the count it is derived from — so no request
// body, no row and no future endpoint can carry one in.
type State struct{ s string }

// The two Incident states.
var (
	// StateActive means at least one current member Case is open.
	StateActive = State{"active"}
	// StateQuiet means none is — every member has closed, or every member has
	// been removed. Quiet is not "fixed": whether the response is over is the
	// incident tool's fact, never this one's.
	StateQuiet = State{"quiet"}
)

// StateOf derives an Incident's state from how many of its CURRENT members are
// open. It is the one place the rule is written.
func StateOf(openMembers int) State {
	if openMembers > 0 {
		return StateActive
	}
	return StateQuiet
}

// String renders the state.
func (s State) String() string { return s.s }

// AllStates is the closed set in contract order, for the enum gate.
func AllStates() []State { return []State{StateActive, StateQuiet} }

// Fact is one thing oto OBSERVED about an Incident, declared outbound (ADR 0052
// §5). The five spellings are the notification Reasons they become; the
// `notification` module owns that vocabulary and `internal/app` maps one onto the
// other, so this module never imports it.
//
// ⛔ NONE OF THEM IS A COMMAND. There is no "resolved" and no "closed": oto sends an
// incident tool facts and never a resolve, the same rule ADR 0013 set the other way
// round.
type Fact string

// The five Incident facts.
const (
	// FactDrawn is the Incident being drawn over its first Cases.
	FactDrawn Fact = "drawn"
	// FactCaseAdded is a Case joining after the draw, including a move's arrival.
	FactCaseAdded Fact = "case_added"
	// FactCaseRemoved is a human taking a Case out, including a move's departure.
	FactCaseRemoved Fact = "case_removed"
	// FactQuiet is the derived state going from active to quiet: the last open
	// member Case closed or left. Never "the incident is over".
	FactQuiet Fact = "quiet"
	// FactActiveAgain is the derived state going from quiet to active: an open Case
	// joined. Cases never reopen (ADR 0040), so membership is the only way here.
	FactActiveAgain Fact = "active_again"
)

// Transition is the state fact a membership change produced, if any: the
// Incident's derived state read before and after the change, in the same
// transaction, under the Incident's row lock.
//
// It is the one place the active→quiet and quiet→active edges are decided, so
// every verb and the Case-ending observer agree on what counts as one.
func Transition(openBefore, openAfter int) (Fact, bool) {
	switch before, after := StateOf(openBefore), StateOf(openAfter); {
	case before == StateActive && after == StateQuiet:
		return FactQuiet, true
	case before == StateQuiet && after == StateActive:
		return FactActiveAgain, true
	default:
		return "", false
	}
}

// Attribution is the answer to "why is this here?" — exactly one of a human or
// an operator-written Correlator (ADR 0052 §2).
//
// ⭐ IT IS ACTOR METADATA IN THE `acked_by` MOULD. The user id is kept because the
// row references `users` (nulled when the user goes); the LABEL is what is read
// back, frozen at the time, so "drawn by alice" reads the same after alice is
// renamed or deleted. Nothing is aggregated per person (SPEC R8).
type Attribution struct {
	userID       uuid.UUID
	label        string
	correlatorID uuid.UUID
}

// Human builds the attribution of a person's decision. userID may be uuid.Nil
// for a principal with no `users` row; the label may not be blank, because a
// decision nobody signed is not attributable.
func Human(userID uuid.UUID, label string) (Attribution, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return Attribution{}, errs.New(errs.KindValidation, "not_blank",
			"an Incident decision must be attributed to a named human")
	}
	if len(label) > MaxActorLabelBytes {
		label = truncateUTF8(label, MaxActorLabelBytes)
	}
	return Attribution{userID: userID, label: label}, nil
}

// Restore rebuilds an attribution read back from a row, where exactly one of the
// label and the Correlator id is set (`incidents_drawn_by_ck`,
// `incident_members_added_by_ck`). A row that sets both or neither is a CHECK
// the database already refused, so it is an internal error here, never a guess.
func Restore(userID uuid.UUID, label string, correlatorID uuid.UUID) (Attribution, error) {
	if (label == "") == (correlatorID == uuid.Nil) {
		return Attribution{}, errs.New(errs.KindInternal, "incident_attribution_invalid",
			"an Incident attribution names exactly one of a human and a Correlator")
	}
	return Attribution{userID: userID, label: label, correlatorID: correlatorID}, nil
}

// IsHuman reports whether a person decided. The other answer is a Correlator.
func (a Attribution) IsHuman() bool { return a.correlatorID == uuid.Nil }

// UserID is the deciding user's id, or uuid.Nil for a Correlator or a user since
// deleted.
func (a Attribution) UserID() uuid.UUID { return a.userID }

// Label is the human's frozen display name, or "" for a Correlator.
func (a Attribution) Label() string { return a.label }

// CorrelatorID is the deciding Correlator, or uuid.Nil for a human.
func (a Attribution) CorrelatorID() uuid.UUID { return a.correlatorID }

// IsZero reports whether no attribution is set.
func (a Attribution) IsZero() bool { return a.label == "" && a.correlatorID == uuid.Nil }

// Ref names one Incident by both of its identities: the id rows reference, and
// the number a human quotes.
type Ref struct {
	ID     uuid.UUID
	Number int64
}

// CaseRef is what this module needs to know about a Case it is about to put in,
// or take out of, an Incident: its id, its own number for the sentences a human
// reads, and the Alert it belongs to so the timeline fact lands on both.
type CaseRef struct {
	ID      uuid.UUID
	Number  int64
	AlertID uuid.UUID
	// Synthetic reports that the Case's Alert is a delivery drill's
	// (`alerts.synthetic`). Such a Case is never drawn into an Incident.
	Synthetic bool
}

// Incident is one row of the list: who drew it and when, and the counts its
// derived state is read from.
type Incident struct {
	ID      uuid.UUID
	Number  int64
	DrawnAt time.Time
	DrawnBy Attribution
	// MemberCount is how many Cases are in the Incident NOW; removed ones are not
	// counted.
	MemberCount int
	// OpenMemberCount is how many of those are open. State is derived from it and
	// from nothing else.
	OpenMemberCount int
	// Alertnames are the distinct alertnames of the current members, sorted, at
	// most MaxAlertnames.
	Alertnames []string
	// Conversation reports that the Correlator which drew this Incident says its
	// Incidents are conversations (ADR 0052 §6, `correlators.incidents_are_conversations`).
	// READ, NEVER STORED on the Incident: it is the Correlator's setting as it is
	// now, so a human-drawn Incident — which has no Correlator — is never one.
	Conversation bool
}

// State is the derived state (ADR 0052 §3). There is no setter.
func (i Incident) State() State { return StateOf(i.OpenMemberCount) }

// ListFilter narrows the Incident list.
//
// ⭐ AN EMPTY INCIDENT IS A RECORD, NOT A ROW TO SCAN PAST (owner ruling
// 2026-10-04). An Incident whose every member was removed or moved away is kept —
// nothing here deletes an Incident, and "#4 held these Cases until alice moved them
// to #7" is history a human may need — but it is no longer a story anybody is
// following, and a list that leads with husks buries the ones that are. So the list
// leaves it out unless IncludeEmpty asks for it, and `Get` serves it by number
// whatever this says: the record is hidden from the scan, never from the address.
//
// "Empty" is ZERO CURRENT MEMBERS — the same `removed_at IS NULL` the summary counts
// — and nothing else. A quiet Incident whose Cases all closed still holds them, is
// still a story, and is listed.
type ListFilter struct {
	IncludeEmpty bool
}

// Member is one spell of one Case inside one Incident: current while RemovedAt
// is zero, a tombstone after.
type Member struct {
	CaseID     uuid.UUID
	CaseNumber int64
	CaseState  kernel.CaseState
	AlertID    uuid.UUID
	Alertname  string
	Labels     map[string]string
	AddedAt    time.Time
	AddedBy    Attribution
	// RemovedAt is zero while the Case is a member. A removed Case stays recorded
	// as removed; it is never deleted from the Incident's history.
	RemovedAt time.Time
	// RemovedByLabel is the human who removed it. Only a human removes (§4).
	RemovedByLabel string
	// MovedToNumber is the Incident the Case went to when the removal was half of
	// a move, and 0 for a plain removal.
	MovedToNumber int64
}

// Current reports whether the Case is still in the Incident.
func (m Member) Current() bool { return m.RemovedAt.IsZero() }

// Detail is one Incident with its whole membership history, current spells and
// tombstones alike, in the order they joined.
type Detail struct {
	Incident
	Members []Member
	// Outbound is every external incident a destination's tool echoed back for this
	// Incident — ADR 0052 §5's outbound mapping, recorded by the notification layer
	// as the receipt of a delivery (migration 00089, git-bug 506ff21). One per
	// channel, oldest first; empty when no receiver echoed anything.
	//
	// ⛔ IT IS NOT STATE AND NOTHING HERE DERIVES FROM IT. `State()` reads the member
	// Cases and only them; an external incident's link says where the response is
	// being handled, never whether it is over.
	Outbound []Outbound
}

// Outbound is one receipt: a destination, and the incident its tool opened for
// this Incident, as the tool named it.
type Outbound struct {
	ChannelID   uuid.UUID
	ChannelName string
	ExternalURL string
	ExternalID  string
	RecordedAt  time.Time
}

// ------------------------------------------------------------------- refusals

// CaseInIncident is the refusal that carries the AT-MOST-ONE rule to a human
// (ADR 0052 §4), and it POINTS AT THE MOVE: the caller is told which Incident
// holds the Case and the exact request that would take it from there, because
// "no" without the way forward is how a rule gets routed around.
//
// The database is what enforces the rule; this is only what the caller reads.
// See RaceLostToAnotherIncident for the answer when the index refused a write the
// service's read had not seen coming.
func CaseInIncident(c CaseRef, holder Ref) error {
	return errs.Conflict("case_in_incident", fmt.Sprintf(
		"Case #%d is already in Incident #%d, and a Case belongs to at most one Incident. "+
			"Move it instead: POST /api/v1/incidents/%d/cases/%s/move",
		c.Number, holder.Number, holder.Number, c.ID))
}

// RaceLostToAnotherIncident is CaseInIncident when the holder is not known: a
// concurrent draw or add committed between this request's read and its write,
// and `incident_members_case_live_uniq` refused the second. Same code, so a
// client branches on one thing.
func RaceLostToAnotherIncident() error {
	return errs.Conflict("case_in_incident",
		"the Case was added to another Incident while this request was being made, and a Case "+
			"belongs to at most one Incident. Read the Case's Incident and move it instead")
}

// AlreadyAMember refuses adding a Case to the Incident it is already in.
func AlreadyAMember(c CaseRef, in Ref) error {
	return errs.Conflict("already_a_member",
		fmt.Sprintf("Case #%d is already in Incident #%d", c.Number, in.Number))
}

// SyntheticCase refuses putting a delivery drill's Case into an Incident. A drill
// proves delivery and is disposed of afterwards; an Incident holding it would
// outlive the evidence and count a rehearsal as an outage.
func SyntheticCase(c CaseRef) error {
	return errs.Conflict("case_synthetic", fmt.Sprintf(
		"Case #%d is a delivery drill's synthetic Case and is never drawn into an Incident", c.Number))
}

// NotFound is the one answer for an Incident number this org has not drawn —
// including one another org has. A 403 would confirm it exists elsewhere.
func NotFound() error {
	return errs.NotFound("incident_not_found", "no such Incident")
}

// MemberNotFound is the answer for a Case that is not a CURRENT member of the
// Incident in the path: never one, removed already, or moved away meanwhile.
func MemberNotFound() error {
	return errs.NotFound("incident_member_not_found", "that Case is not in this Incident")
}

// CaseNotFound is the answer for a Case id this org does not have.
func CaseNotFound() error {
	return errs.NotFound("case_not_found", "no such Case")
}

// MoveToSelf refuses a move whose destination is the Incident it is leaving.
func MoveToSelf() error {
	return errs.Validation("validation_failed", "a Case cannot be moved to the Incident it is in",
		errs.Violation{Field: "to_number", Code: "move_to_self",
			Message: "must name a different Incident from the one in the path"})
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune. A display
// name longer than the column admits is clipped rather than refused: the person
// acted, and refusing to record that because their name is long would lose the
// fact to save the formatting.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
