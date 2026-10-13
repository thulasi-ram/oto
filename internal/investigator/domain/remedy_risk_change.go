package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// A REMEDY RISK RULE CHANGE MADE FROM THE APP (ADR 0054 §3, owner ruling O3, 2026-10-06;
// migration 00111).
//
// ⛔⛔ A PROPOSAL CHANGES NO TIER. The rules say whether a Remedy needs one approval or two, and
// they only ever loosen (oto ships none, so with none every Remedy needs two). A member who could
// write them directly could write a one-approval rule and then approve alone — the loophole 5ace8f3
// opened and the 2026-10-05 ruling closed. So a change made from the app is a RiskChange: a record
// of what somebody proposes, which a DIFFERENT member confirms before anything is written. The
// second person is a CHECK on the row (`remedy_risk_changes_two_people_ck`) as well as a rule here.

// RiskChangeStatus is where a RiskChange stands. It starts pending and ends in exactly one of the
// other three, once.
type RiskChangeStatus string

const (
	// RiskChangePending is waiting for a second person. At most one per org.
	RiskChangePending RiskChangeStatus = "pending"
	// RiskChangeApplied was confirmed by someone other than its proposer, and wrote the rules.
	RiskChangeApplied RiskChangeStatus = "applied"
	// RiskChangeDiscarded was withdrawn or refused by a person, and wrote nothing.
	RiskChangeDiscarded RiskChangeStatus = "discarded"
	// RiskChangeSuperseded was overtaken by a newer proposal, or by `oto remedy-rules apply`.
	RiskChangeSuperseded RiskChangeStatus = "superseded"
)

// RiskChangeDraft is what a member proposes: the whole rule set and the risk model, as
// `oto remedy-rules apply` takes them. It replaces the rules whole when confirmed, because a
// partial edit of an ordered list is a list nobody wrote.
type RiskChangeDraft struct {
	// Rules is every rule, in order. Empty is legal: it says every Remedy needs two.
	Rules []RiskRule
	// RiskModelProviderID is one of the org's model endpoints, or uuid.Nil for none.
	RiskModelProviderID uuid.UUID
}

// RiskChange is one proposed replacement of the org's rules, as stored.
type RiskChange struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// Rules are the rules as proposed, validated and normalised.
	Rules               RiskRules
	RiskModelProviderID uuid.UUID
	Status              RiskChangeStatus
	// ProposedBy is who proposed it. UserID is uuid.Nil once that user is deleted; the label is frozen.
	ProposedBy Requester
	ProposedAt time.Time
	// DecidedBy is who applied or discarded it; zero while pending. When it was decided is on the row
	// (`decided_at`) and read by nothing in the product, so it is not carried here.
	DecidedBy Requester
}

// Pending reports whether it still waits for a second person.
func (c RiskChange) Pending() bool { return c.Status == RiskChangePending }

// ProposedByUser reports whether this user proposed it. A deleted user's nil id is nobody's.
func (c RiskChange) ProposedByUser(userID uuid.UUID) bool {
	return userID != uuid.Nil && c.ProposedBy.UserID == userID
}

// ConfirmableBy says whether `by` may confirm this change, as the refusal a person reads.
//
// ⛔ THE PROPOSER CANNOT CONFIRM THEIR OWN CHANGE, however many sessions they hold, and a change
// that is no longer pending cannot be confirmed at all: confirming one that was superseded meanwhile
// would apply what the confirmer did not read. The database refuses the first too.
func (c RiskChange) ConfirmableBy(by Requester) error {
	switch {
	case !c.Pending():
		return RiskChangeNotPending(c.Status)
	case c.ProposedByUser(by.UserID):
		return errs.Forbidden("remedy_risk_change_needs_a_second_person",
			"a rule change is confirmed by a different member than the one who proposed it; "+
				"ask a colleague to confirm it, or discard it and propose another")
	}
	return nil
}

// RiskChangeNotFound is the answer for a change that does not exist, or is another org's.
func RiskChangeNotFound() error {
	return errs.NotFound("remedy_risk_change_not_found", "no such rule change")
}

// RiskChangeNotPending is the answer for acting on a change that has already been decided.
func RiskChangeNotPending(status RiskChangeStatus) error {
	return errs.Newf(errs.KindConflict, "remedy_risk_change_not_pending",
		"this rule change is already %s, so it can no longer be confirmed or discarded; reload to see what stands", status)
}

// ValidateRiskChange checks a draft as `oto remedy-rules apply` checks a file, and returns the
// validated rules. The model endpoint's existence is the service's to check: it is a read.
func ValidateRiskChange(d RiskChangeDraft) (RiskRules, error) {
	return NewRiskRules(d.Rules)
}
