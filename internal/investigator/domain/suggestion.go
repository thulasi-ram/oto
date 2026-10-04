package domain

// A FINDING SUGGESTS, AND A HUMAN APPLIES IT OR IT LAPSES (ADR 0053 §2, ADR 0052 §2 and
// §4; git-bug 8327c00).
//
// "Noise reduction arrives as Suggestions (e.g. "add `count_min=3` to this policy"), which
// a human applies and which then act deterministically. A Suggestion is applied or lapses;
// it has no reject verb, so it is never a queue." And from ADR 0052 §2: "The Investigator
// only proposes membership, as a Suggestion."
//
// ⭐⭐ TWO KINDS, BOTH A CHANGE TO OTO'S OWN CONFIGURATION, AND NOTHING OUTSIDE IT:
//
//   - `policy_count_condition` — set one notification policy's count condition (ADR 0044:
//     `count_min` over `count_window_seconds`), the one silence an operator may ask for by
//     name. A Suggestion of one is still the OPERATOR'S number once applied: a human read it
//     and pressed apply, and the edit is the ordinary policy edit, so ADR 0044 §3's line —
//     "a damper whose threshold is the operator's is their request" — holds.
//   - `incident_membership` — add one Case to one Incident (ADR 0052 §4). A Case already in
//     another Incident is MOVED, because a Case belongs to at most one; the read says so
//     before anyone presses anything (MovesFrom), and applying says so again.
//
// ⭐ THREE STATES, AND ONLY ONE OF THEM IS STORED. A Suggestion is `open` until a human
// applies it (`applied`, with who and when) or its lapse time passes (`lapsed`). Lapsing is
// READ OFF THE CLOCK, never written: nothing happens when a Suggestion lapses — no fact goes
// out, no job runs — so a sweeper would be a job whose only effect is a column nobody needs.
// StateAt is the one place the rule is written; the list read and the apply both use it.
//
// ⛔⛔ THERE IS NO REJECT VERB, AND THE ABSENCE IS THE RULING. Nothing here declines, refuses
// or hides a Suggestion on a human's say-so: an unapplied one lapses on its own. A verb that
// put a Suggestion in front of somebody until they acted on it would make it a queue of work
// routed to a person, which oto is not (H-1).
//
// ⛔ THE INVESTIGATOR NEVER APPLIES ONE. It proposes through two built-in Tools, the proposal
// is checked against the org as the run reads it, and an invalid one is refused on the
// record as a Step. Applying is a human's request, made through the same service method a
// human's own edit goes through, with the applier as actor and the Investigation as
// provenance.

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// SuggestionKind is what a Suggestion would change (`investigation_suggestions_kind_ck`).
type SuggestionKind string

// The two kinds.
const (
	// SuggestCountCondition sets one notification policy's count condition (ADR 0044).
	SuggestCountCondition SuggestionKind = "policy_count_condition"
	// SuggestMembership adds one Case to one Incident — or moves it there (ADR 0052 §4).
	SuggestMembership SuggestionKind = "incident_membership"
)

// ParseSuggestionKind reads a stored kind.
func ParseSuggestionKind(s string) (SuggestionKind, error) {
	switch k := SuggestionKind(s); k {
	case SuggestCountCondition, SuggestMembership:
		return k, nil
	default:
		return "", errs.Newf(errs.KindInternal, "suggestion_kind", "unknown Suggestion kind %q", s)
	}
}

// SuggestionState is where a Suggestion is. Only `applied` is stored; `lapsed` is read off
// the clock (StateAt).
type SuggestionState string

// The three states.
const (
	// SuggestionOpen can be applied.
	SuggestionOpen SuggestionState = "open"
	// SuggestionApplied was applied by a human; the edit it made is the ordinary one.
	SuggestionApplied SuggestionState = "applied"
	// SuggestionLapsed passed its lapse time unapplied. It is no longer shown or applicable.
	SuggestionLapsed SuggestionState = "lapsed"
)

// SuggestionLapse is how long a Suggestion stays open. ⚠️ A FIXED DEFAULT, NOT A SETTING:
// seven days is long enough for a weekend and a review, and short enough that a proposal
// about last week's storm does not sit beside this week's. It is stamped on each row as
// `lapses_at` when the Finding is recorded, so a later org setting changes the Suggestions
// made after it and none made before.
const SuggestionLapse = 7 * 24 * time.Hour

// MaxSuggestionsPerRun bounds what one Investigation may propose
// (`investigation_suggestions` is not a place to list every policy in the org). A proposal
// past it is refused on the record.
const MaxSuggestionsPerRun = 5

// MaxSuggestionWhy mirrors `investigation_suggestions_why_ck`, in characters: one or two
// sentences a human reads before pressing apply.
const MaxSuggestionWhy = 1000

// The count condition's bounds, as ADR 0044 §4 set them (`policies_count_min_ck`,
// `policies_count_window_ck`). ⚠️ A PROPOSAL IS CHECKED AGAINST THEM SO THE MODEL HEARS A
// REFUSAL WHILE IT CAN STILL ANSWER IT; the authority is still the policy edit itself, which
// validates the merged policy again when the Suggestion is applied.
const (
	MinSuggestedCountMin    = 2
	MaxSuggestedCountMin    = 10000
	MinSuggestedCountWindow = time.Minute
	MaxSuggestedCountWindow = 24 * time.Hour
)

// CountSubject is the one subject binding a count condition may sit on (ADR 0044 §4,
// `policies_count_case_ck`): a count needs a unit, and a Case is it.
const CountSubject = "case"

// PolicyTarget is a notification policy as a Suggestion reads it: its name, its binding and
// the count condition it carries now (zero when none). A copy — `investigator` never imports
// `notification` (ADR 0053 §2) — handed over by `internal/app`.
type PolicyTarget struct {
	ID           uuid.UUID
	Name         string
	SubjectKinds []string
	CountMin     int
	CountWindow  time.Duration
}

// Countable reports whether a count condition may sit on the policy at all: its binding is
// exactly the Case subject.
func (p PolicyTarget) Countable() bool {
	return len(p.SubjectKinds) == 1 && p.SubjectKinds[0] == CountSubject
}

// CountChange is a `policy_count_condition` Suggestion's change: the policy, the condition
// proposed, and the one it carried when it was proposed (zero for none) — so a human reads
// "from none to 3 in 10m" rather than a bare number.
type CountChange struct {
	PolicyID    uuid.UUID
	PolicyName  string
	CountMin    int
	CountWindow time.Duration
	WasMin      int
	WasWindow   time.Duration
}

// MembershipChange is an `incident_membership` Suggestion's change: this Case, into this
// Incident. Both are named by id and by the number a human quotes.
type MembershipChange struct {
	IncidentID     uuid.UUID
	IncidentNumber int64
	CaseID         uuid.UUID
	CaseNumber     int64
}

// SuggestionDraft is one proposal a run made, checked, waiting for the run's Finding. Build
// it with NewCountSuggestion or NewMembershipSuggestion.
type SuggestionDraft struct {
	Kind       SuggestionKind
	Count      CountChange
	Membership MembershipChange
	Why        string
}

// Target names what the draft would change, so one run cannot propose the same change twice.
func (d SuggestionDraft) Target() string {
	if d.Kind == SuggestCountCondition {
		return string(d.Kind) + ":" + d.Count.PolicyID.String()
	}
	return string(d.Kind) + ":" + d.Membership.IncidentID.String() + ":" + d.Membership.CaseID.String()
}

// newWhy checks the sentence a human reads first.
func newWhy(raw string) (string, error) {
	why := strings.TrimSpace(raw)
	if why == "" {
		return "", errs.Validation("suggestion_invalid", "say why, in a sentence a human reads before applying it",
			errs.Violation{Field: "why", Code: "required", Message: "a Suggestion says why"})
	}
	return clip(why, MaxSuggestionWhy), nil
}

// NewCountSuggestion proposes setting one policy's count condition.
//
// It refuses what could never be applied: a policy not bound to exactly the Case subject
// (the ordinary edit would refuse it — the operator must change the binding first, and that
// is not a Suggestion's to propose), a number outside ADR 0044's bounds, and the condition
// the policy already carries.
func NewCountSuggestion(p PolicyTarget, countMin int, window time.Duration, why string) (SuggestionDraft, error) {
	if !p.Countable() {
		bound := "every subject"
		if len(p.SubjectKinds) > 0 {
			bound = strings.Join(p.SubjectKinds, ", ")
		}
		return SuggestionDraft{}, errs.Validation("suggestion_invalid", fmt.Sprintf(
			"the policy %s is bound to %s, and a count condition sits only on a policy bound to exactly %s",
			p.Name, bound, CountSubject),
			errs.Violation{Field: "policy", Code: "not_countable", Message: "the policy's subject_kinds is not [case]"})
	}
	if countMin < MinSuggestedCountMin || countMin > MaxSuggestedCountMin {
		return SuggestionDraft{}, errs.Validation("suggestion_invalid",
			fmt.Sprintf("count_min is %d to %d", MinSuggestedCountMin, MaxSuggestedCountMin),
			errs.Violation{Field: "count_min", Code: "range", Message: "out of range"})
	}
	if window < MinSuggestedCountWindow || window > MaxSuggestedCountWindow || window%time.Second != 0 {
		return SuggestionDraft{}, errs.Validation("suggestion_invalid",
			"count_window_seconds is 60 to 86400 whole seconds",
			errs.Violation{Field: "count_window_seconds", Code: "range", Message: "out of range"})
	}
	if countMin == p.CountMin && window == p.CountWindow {
		return SuggestionDraft{}, errs.Validation("suggestion_invalid",
			fmt.Sprintf("the policy %s already carries that count condition", p.Name),
			errs.Violation{Field: "count_min", Code: "unchanged", Message: "nothing would change"})
	}
	w, err := newWhy(why)
	if err != nil {
		return SuggestionDraft{}, err
	}
	return SuggestionDraft{Kind: SuggestCountCondition, Why: w, Count: CountChange{
		PolicyID: p.ID, PolicyName: p.Name, CountMin: countMin, CountWindow: window,
		WasMin: p.CountMin, WasWindow: p.CountWindow,
	}}, nil
}

// NewMembershipSuggestion proposes adding one Case to one Incident. A Case already a current
// member of that Incident is refused — nothing would change. A Case in ANOTHER Incident is
// admitted: applying it is a move, and both the read and the apply say so.
func NewMembershipSuggestion(incident IncidentSubject, c CaseSubject, why string) (SuggestionDraft, error) {
	for _, m := range incident.Members {
		if m.Current() && m.CaseID == c.CaseID {
			return SuggestionDraft{}, errs.Validation("suggestion_invalid",
				fmt.Sprintf("Case #%d is already in Incident #%d", c.Number, incident.Number),
				errs.Violation{Field: "case_id", Code: "already_a_member", Message: "nothing would change"})
		}
	}
	w, err := newWhy(why)
	if err != nil {
		return SuggestionDraft{}, err
	}
	return SuggestionDraft{Kind: SuggestMembership, Why: w, Membership: MembershipChange{
		IncidentID: incident.IncidentID, IncidentNumber: incident.Number, CaseID: c.CaseID, CaseNumber: c.Number,
	}}, nil
}

// Suggestion is one proposal as stored, tied to the Investigation whose Finding made it.
type Suggestion struct {
	ID              uuid.UUID
	OrgID           uuid.UUID
	InvestigationID uuid.UUID
	Kind            SuggestionKind
	Count           CountChange
	Membership      MembershipChange
	Why             string
	ProposedAt      time.Time
	LapsesAt        time.Time
	// AppliedAt is zero until a human applies it; AppliedBy is who did — actor metadata in
	// the acked_by mould: the id nulled when the user goes, the label frozen.
	AppliedAt time.Time
	AppliedBy Requester

	// MovesFrom is READ, NEVER STORED: for an open membership Suggestion, the Incident the
	// Case is in NOW when that is not the one proposed — applying it moves the Case from
	// there. Zero when applying it would add. Filled by the service on every read, so the
	// screen says "this will move Case N from Incident M" before anyone presses anything.
	MovesFrom IncidentRef
}

// IncidentRef names one Incident by id and by the number a human quotes.
type IncidentRef struct {
	ID     uuid.UUID
	Number int64
}

// IsZero reports whether no Incident is named.
func (r IncidentRef) IsZero() bool { return r.ID == uuid.Nil }

// StateAt is the Suggestion's state at `now`: applied once a human applied it, lapsed once
// its lapse time has passed unapplied, open otherwise. The one place the rule is written.
func (s Suggestion) StateAt(now time.Time) SuggestionState {
	switch {
	case !s.AppliedAt.IsZero():
		return SuggestionApplied
	case !now.Before(s.LapsesAt):
		return SuggestionLapsed
	default:
		return SuggestionOpen
	}
}

// Applicable refuses applying a Suggestion that is not open: a typed 409 for each reason, so
// a client branches on the code rather than on the sentence.
func (s Suggestion) Applicable(now time.Time) error {
	switch s.StateAt(now) {
	case SuggestionApplied:
		return errs.Conflict("suggestion_already_applied", fmt.Sprintf(
			"this Suggestion was applied by %s at %s; applying it again would change nothing",
			s.AppliedBy.Label, s.AppliedAt.UTC().Format(time.RFC3339)))
	case SuggestionLapsed:
		return errs.Conflict("suggestion_lapsed", fmt.Sprintf(
			"this Suggestion lapsed unapplied at %s and is no longer offered; ask for another Investigation "+
				"if it still matters", s.LapsesAt.UTC().Format(time.RFC3339)))
	default:
		return nil
	}
}

// SuggestionTargetGone refuses applying a Suggestion whose policy, Incident or Case no
// longer exists — deleted since it was proposed. Typed, and a 409 rather than a 404: the
// Suggestion is here; what it would change is not.
func SuggestionTargetGone(what string) error {
	return errs.Conflict("suggestion_target_gone", fmt.Sprintf(
		"this Suggestion cannot be applied: %s no longer exists", what))
}

// SuggestionMovesCase refuses applying a membership Suggestion that would MOVE its Case
// when the request did not say it knew: the move is said before it is applied, and an
// apply that did not confirm the Incident it moves from is answered with the sentence it
// should have read.
func SuggestionMovesCase(m MembershipChange, from IncidentRef) error {
	return errs.Conflict("suggestion_moves_case", fmt.Sprintf(
		"applying this will move Case #%d from Incident #%d to Incident #%d, because a Case belongs to at "+
			"most one Incident; apply it again with moves_from_incident_number %d to confirm the move",
		m.CaseNumber, from.Number, m.IncidentNumber, from.Number))
}

// SuggestionNotFound is the one answer for a Suggestion this org does not have.
func SuggestionNotFound() error {
	return errs.NotFound("suggestion_not_found", "no such Suggestion")
}

// AppliedMembership is the ordinary membership edit a membership Suggestion asks for: add
// the Case to the Incident — or, when FromNumber is set, move it from there — by the human
// applying it, with the Investigation that suggested it as provenance.
type AppliedMembership struct {
	IncidentNumber int64
	CaseID         uuid.UUID
	// FromNumber is the Incident the Case is moved from, 0 for an add.
	FromNumber int64
	By         Requester
	// SuggestedBy is the Investigation whose Finding proposed it.
	SuggestedBy uuid.UUID
}
