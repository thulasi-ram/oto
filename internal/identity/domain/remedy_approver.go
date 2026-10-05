package domain

// THE REMEDY APPROVER GRANT (ADR 0054 §4, migration 00103, git-bug 47f67c8): the one
// permission oto has. A user holding it on a write ToolServer may approve a Remedy that
// ToolServer would carry out; double approval means two DIFFERENT holders.
//
// ⭐⭐ IT IS GIVEN AND TAKEN FROM THE HOST SHELL ONLY — `oto grant remedy-approver` and
// `oto revoke remedy-approver` (internal/app/remedyapprover.go) — exactly as `oto
// bootstrap` mints the first credential. Nothing in this package, its repository or its
// service writes a grant, and no HTTP route can: one holder able to mint a second
// approver from inside oto (an alt account) would defeat double approval. This file only
// says what a grant read back MEANS.
//
// ⛔ A PERMISSION, NEVER AN OBLIGATION (H-1). Nothing here names a holder as the one who
// must act, routes anything to one, or makes a queue of what they have not approved.

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// RemedyApproverGrantedByCLI is the one value `remedy_approver_grants.granted_by` may
// hold (`remedy_approver_grants_by_ck`): the row was written by the CLI, and nothing else
// writes one.
const RemedyApproverGrantedByCLI = "cli"

// RemedyApproverRequiredCode is the typed refusal of an approval by a user who does not
// hold a grant that counts on the Remedy's ToolServer.
const RemedyApproverRequiredCode = "remedy_approver_required"

// RemedyApprover is one grant on one ToolServer, as read back with its holder.
type RemedyApprover struct {
	UserID uuid.UUID
	// Email is the holder's address. ⚠️ Zero only for a shadow member, which the CLI can
	// never name (it resolves by address) — so a zero here is a row written by hand, and
	// it does not count.
	Email       Email
	DisplayName string
	GrantedAt   time.Time
	// GrantedBy is who wrote the row: always RemedyApproverGrantedByCLI.
	GrantedBy string
	// DisabledAt is the HOLDER's soft disable, not the grant's: a disabled user's grant
	// stays on the record, shown, and stops counting until they are re-enabled or it is
	// revoked.
	DisabledAt *time.Time
}

// Counts reports whether this grant lets its holder approve a Remedy now: the holder is
// not disabled and has an address. A revoked grant is not read at all — revoking deletes
// the row.
func (a RemedyApprover) Counts() bool {
	return a.DisabledAt == nil && !a.Email.IsZero()
}

// RemedyApproverRequired is the refusal of an approval by a user with no grant that
// counts on the Remedy's ToolServer: no grant, a revoked one, or one held by a disabled
// user. It is a 403 and names how a grant is given, because the person reading it cannot
// give one from where they are.
func RemedyApproverRequired() error {
	return errs.New(errs.KindForbidden, RemedyApproverRequiredCode,
		"approving a Remedy needs the remedy-approver grant on its ToolServer; it is given from the host shell "+
			"with `oto grant remedy-approver`, never from inside oto")
}
