package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ListPolicyDigestInvestigations reads the runs a notification policy's digest windows
// asked for, latest first (review D4).
//
// ⭐ "RECORDED, NEVER SILENT" NEEDS A WAY TO READ THE RECORD. A Case's runs are on the
// Case and an Incident's on the Incident; a digest window's run has no page of its own,
// and before this nothing handed out its id — so a run that was skipped for the day's
// budget, skipped because its window closed, failed or exhausted was on the record and
// unreachable. The policy that names the digest Investigator is where a person looks.
//
// A policy this org does not have, or one that was deleted, is a 404 like the policy
// itself: the read goes through the same port the Suggestion apply uses, so the two
// cannot disagree about which policies exist.
func (s *Service) ListPolicyDigestInvestigations(
	ctx context.Context, scope db.TenantScope, policyID uuid.UUID, p db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if _, err := s.policies.SuggestionPolicy(ctx, scope, policyID); err != nil {
		return nil, db.Cursor{}, err
	}
	return s.investigations.ListBySubject(ctx, scope, domain.SubjectDigest, policyID, p)
}
