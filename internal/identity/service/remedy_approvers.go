package service

// WHO MAY APPROVE A REMEDY (ADR 0054 §4, git-bug 47f67c8): the read side of the one
// permission oto has. The approval path (git-bug 4148256) asks RequireRemedyApprover once
// per approval; the settings read lists a ToolServer's holders.
//
// ⛔⛔ NOTHING HERE GRANTS OR REVOKES. A grant is written by `oto grant remedy-approver`
// and deleted by `oto revoke remedy-approver`, from the host shell (internal/app). This
// file can only say whether a grant that exists COUNTS.

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// CanApproveRemedies reports whether a user holds a grant that COUNTS on a ToolServer:
// one exists (never granted and revoked are both "no row"), and its holder is neither
// disabled nor a shadow member.
//
// ⛔ IT FAILS CLOSED. An unreadable grant is an error, never a yes; and a service built
// without the grant reader answers an internal error rather than "anyone may".
func (s *Service) CanApproveRemedies(ctx context.Context, scope db.TenantScope, toolServerID, userID uuid.UUID) (bool, error) {
	if err := db.RequireScope(scope); err != nil {
		return false, err
	}
	if s.approvers == nil {
		return false, errs.New(errs.KindInternal, "remedy_approvers_unwired",
			"the remedy approver grants cannot be read in this process")
	}
	if toolServerID == uuid.Nil || userID == uuid.Nil {
		return false, nil
	}
	grant, err := s.approvers.Grant(ctx, scope, toolServerID, userID)
	if errs.IsKind(err, errs.KindNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return grant.Counts(), nil
}

// RequireRemedyApprover is CanApproveRemedies as the approval path wants it: nil when the
// user may approve a Remedy on this ToolServer, and otherwise the typed 403
// `remedy_approver_required` (domain.RemedyApproverRequired) — or the read's own error.
func (s *Service) RequireRemedyApprover(ctx context.Context, scope db.TenantScope, toolServerID, userID uuid.UUID) error {
	ok, err := s.CanApproveRemedies(ctx, scope, toolServerID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.RemedyApproverRequired()
	}
	return nil
}

// RemedyApprovers lists every grant on one ToolServer with its holder, disabled holders
// included (each says whether it counts). Whether the ToolServer exists is the
// `investigator` module's question; an id this org has no grant on reads as empty.
func (s *Service) RemedyApprovers(ctx context.Context, scope db.TenantScope, toolServerID uuid.UUID) ([]domain.RemedyApprover, error) {
	if err := db.RequireScope(scope); err != nil {
		return nil, err
	}
	if s.approvers == nil {
		return nil, errs.New(errs.KindInternal, "remedy_approvers_unwired",
			"the remedy approver grants cannot be read in this process")
	}
	return s.approvers.ListForToolServer(ctx, scope, toolServerID)
}
