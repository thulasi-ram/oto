package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// The CONFIGURATION side of `case_policy_config` — what an operator's settings
// screen calls, as opposed to what the §B.3 machine reads.
//
// ⭐⭐ THIS FILE IS WHY W IS SETTABLE AT ALL. Migration 00057 shipped the table,
// the reader and the state machine; nothing could write a row except a hand-typed
// INSERT, which means the feature existed and could not be turned on. A knob only
// reachable through psql is not a knob.
//
// ⛔ VALIDATION IS THE DOMAIN'S AND IT IS NOT REPEATED HERE. Every bound is
// `internal/alerts/domain/casepolicy.go`'s, mirrored from the three CHECKs; this
// layer calls Validate and does not re-derive a range. A second copy of "0 to
// 86400" in a service method is the copy that survives the day the ceiling moves.

// opCreateCasePolicy is the operationId a `createCasePolicy` claim is taken under.
// One key must not be replayable across two different operations, so the
// operation is part of the claim's identity, and this is the contract's own
// operationId spelled once.
var opCreateCasePolicy = idempotency.MustOperation("createCasePolicy")

// CasePolicies lists one org's case retention windows, in the order an operator
// reads them: by alertname, then by namespace.
func (s *Service) CasePolicies(
	ctx context.Context, scope db.TenantScope, p db.Keyset,
) ([]domain.CasePolicy, db.Cursor, error) {
	if err := s.requireCasePolicyConfig(); err != nil {
		return nil, db.Cursor{}, err
	}
	return s.casePolicyW.ListCasePolicies(ctx, scope, p)
}

// ⛔ THERE IS NO `GET /case-policies/{id}` AND THEREFORE NO SINGLE-ROW READ HERE.
// A retention rule is three fields; the list is the detail view, and an operator
// looking for one row is looking at the collection it belongs to. `UpdateCasePolicy`
// reads the row it patches through the store directly, because that read exists to
// prove the patch rather than to answer a request.

// CreateCasePolicy writes one (namespace, alertname) → W rule.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS ANSWERED WITH THE ORIGINAL
// ROW, and the second result says which of the two happened. Without the claim the
// endpoint would be safe only by accident: `case_policy_axes_uniq` refuses a second
// create for the same pair with a `409` that names no id, so a client that lost its
// response could not learn the id of the rule it had already written — and would
// have to list the collection to find out whether its own request had landed.
//
// ⛔ W=0 IS A LEGAL CREATE. A stored 0 and an absent row are the same instruction
// to the machine, so this is not a no-op the service should refuse: an operator
// pinning "this alertname gets no window, deliberately" is recording a decision,
// and refusing it would make the absence of a row ambiguous between "not decided"
// and "decided to be zero".
func (s *Service) CreateCasePolicy(
	ctx context.Context, scope db.TenantScope, in domain.CasePolicyDraft, idem Idempotency,
) (domain.CasePolicy, bool, error) {
	if err := s.requireCasePolicyConfig(); err != nil {
		return domain.CasePolicy{}, false, err
	}
	in = in.Normalised()
	if err := in.Validate(); err != nil {
		return domain.CasePolicy{}, false, err
	}
	if err := idempotency.Require(idem, s.claims, s.tx); err != nil {
		return domain.CasePolicy{}, false, err
	}

	// Named before it exists, so the claim below can record it — and so a replay
	// has an id to read the committed row back by.
	if in.ID == uuid.Nil {
		in.ID = id.New()
	}
	// A retention window carries no secret, so the policy is Replay: the honest
	// answer to a retry is the rule the first attempt wrote.
	idem.Operation = opCreateCasePolicy

	var (
		out      domain.CasePolicy
		replayOf uuid.UUID
	)
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		if idem.Keyed {
			ref, err := idempotency.Resolve(ctx, s.claims, scope, idem,
				idempotency.Replay, in.ID, s.Now())
			if err != nil {
				replayOf = ref
				return err
			}
		}
		created, err := s.casePolicyW.CreateCasePolicy(ctx, scope, in)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if errors.Is(err, idempotency.ErrReplay) {
		// Read OUTSIDE the rolled-back transaction, so what comes back is the row
		// the FIRST attempt committed. A row that has since been DELETED cannot be
		// handed back as the caller's own, and inventing a fresh one under a spent
		// key would be a second write for one request.
		existing, readErr := s.casePolicyW.GetCasePolicy(ctx, scope, replayOf)
		if readErr != nil {
			return domain.CasePolicy{}, false, errs.Conflict(idempotency.CodeReuse,
				"this Idempotency-Key was already used and that case retention policy was "+
					"written; it has since been deleted, so it cannot be returned. Retry with a "+
					"new key if you meant a new policy")
		}
		return existing, true, nil
	}
	if err != nil {
		return domain.CasePolicy{}, false, err
	}
	return out, false, nil
}

// UpdateCasePolicy changes W on one existing rule.
//
// The patch is validated against the MERGED row rather than in isolation, for the
// reason every patch in oto is: only the stored row knows which axes the new window
// is being asked to apply to, and a validator that could not see it would have to
// defer to the CHECK — a 500 where a 422 belongs.
func (s *Service) UpdateCasePolicy(
	ctx context.Context, scope db.TenantScope, policyID uuid.UUID, p domain.CasePolicyPatch,
) (domain.CasePolicy, error) {
	if err := s.requireCasePolicyConfig(); err != nil {
		return domain.CasePolicy{}, err
	}
	existing, err := s.casePolicyW.GetCasePolicy(ctx, scope, policyID)
	if err != nil {
		return domain.CasePolicy{}, err
	}
	if err := p.Validate(existing); err != nil {
		return domain.CasePolicy{}, err
	}
	return s.casePolicyW.UpdateCasePolicy(ctx, scope, policyID, p)
}

// DeleteCasePolicy removes one rule, which restores W=0 for its pair — the
// close-on-resolve behaviour oto had before migration 00057.
func (s *Service) DeleteCasePolicy(
	ctx context.Context, scope db.TenantScope, policyID uuid.UUID,
) error {
	if err := s.requireCasePolicyConfig(); err != nil {
		return err
	}
	return s.casePolicyW.DeleteCasePolicy(ctx, scope, policyID)
}

// requireCasePolicyConfig turns an unwired settings port into an honest `503`.
//
// ⛔ IT IS NOT A DEGRADATION TO "PRETEND IT WORKED". Reading W does not depend on
// this port, so a deployment without one behaves exactly as one whose table is
// empty; what it must never do is accept a write and drop it, because an operator
// who set a ten-minute window and still sees six cases has no way to tell which
// half of the system lied.
func (s *Service) requireCasePolicyConfig() error {
	if s.casePolicyW == nil {
		return errs.Unavailable("case_policies_unavailable",
			"the case retention policy store is not configured in this deployment", 0)
	}
	return nil
}
