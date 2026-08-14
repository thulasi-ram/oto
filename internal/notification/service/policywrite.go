package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// ⭐⭐ THIS FILE IS THE SETTINGS WRITE PATH FOR POLICIES, AND IT IS A SEPARATE
// TYPE FROM `PolicyService` ON PURPOSE.
//
// `PolicyService` EVALUATES policies, and giving the thing that reads the rules a
// way to change them is a capability nobody asked for (see `api/ports.go`). What
// the settings write needed was not evaluation but a TRANSACTION: an
// `Idempotency-Key` claim refuses to run outside one, and `createNotificationPolicy`
// went handler → repository directly, so there was nowhere for one to be taken.
//
// ⛔ THERE IS NO UPDATE OR DELETE HERE. Neither declares the header and neither
// duplicates anything on a retry — a PATCH is already idempotent by construction
// and a soft delete converges. Adding them would be adding a write path for the
// sake of symmetry.

// opCreateNotificationPolicy is the contract operationId a key is claimed under,
// spelled once so a claim and the contract cannot drift.
var opCreateNotificationPolicy = idempotency.MustOperation("createNotificationPolicy")

// CodePolicyIdempotencyUnavailable means the caller sent an `Idempotency-Key`
// this deployment cannot honour. It is a DEPLOYMENT fact, not a caller error.
const CodePolicyIdempotencyUnavailable = "idempotency_unavailable"

// IdempotencyClaims is the `Idempotency-Key` claim store, satisfied by
// `*platform/idempotency.Repository`.
type IdempotencyClaims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
}

// PolicyWriteStore inserts and reads back one policy, satisfied by
// `*notification/repository.ConfigRepository`.
//
// ⛔ It takes the id inside PolicyDraft. The claim must name what the create made
// and must do so BEFORE the insert: a retry that inserted first would meet
// `policies_name_uniq` and be answered with a name conflict rather than with the
// policy the caller already has.
type PolicyWriteStore interface {
	CreatePolicy(ctx context.Context, s db.TenantScope, in domain.PolicyDraft) (domain.Policy, error)
	GetPolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Policy, error)
}

// Idempotency is the caller's `Idempotency-Key` intent for one settings write.
type Idempotency struct {
	// Keyed reports that the caller sent a key at all. False means every field
	// below is ignored and the write behaves exactly as it did before, which is
	// what keeps the header optional.
	Keyed bool
	Key   idempotency.Key
	// Principal is who sent it. A key is a client's private handle on its own
	// retry, so one org member's key must never refuse another's request.
	Principal authn.Principal
	// RequestHash is the sha256 of the bytes the caller actually sent.
	RequestHash idempotency.RequestHash
}

// PolicyWriterOptions are the writer's dependencies.
type PolicyWriterOptions struct {
	Store PolicyWriteStore
	// Tx is the SAME unit of work the dispatch path uses (notify.go). The claim
	// and the insert commit together or neither does.
	Tx     TxRunner
	Claims IdempotencyClaims
	Clock  clock.Clock
}

// PolicyWriter registers a routing policy.
type PolicyWriter struct {
	store  PolicyWriteStore
	tx     TxRunner
	claims IdempotencyClaims
	clk    clock.Clock
}

// NewPolicyWriter builds the writer.
func NewPolicyWriter(o PolicyWriterOptions) (*PolicyWriter, error) {
	if o.Store == nil {
		return nil, errs.New(errs.KindInternal, "policy_writer_deps",
			"a policy store is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &PolicyWriter{store: o.Store, tx: o.Tx, claims: o.Claims, clk: clk}, nil
}

// CreatePolicy registers a routing policy.
//
// ⛔ ROUTING IS SIGNAL → DESTINATION, and nothing here changes that: the draft
// carries no user, team, schedule or rotation, and it must never gain one.
//
// ⭐⭐ A RETRY CARRYING THE SAME KEY IS ANSWERED WITH THE ORIGINAL POLICY. This
// endpoint declared the header and read it nowhere; it was safe only by accident,
// because `policies_name_uniq (org_id, name)` refused a second create under the
// same name with a `409` that named nothing — so a client that never received its
// response could not learn the id of what it had already made.
//
// ⛔ THE DRAFT IS NOT RE-VALIDATED HERE. The domain's own Validate runs in the
// handler, before the write, so a bad matcher regex or an out-of-range priority
// is a field violation rather than a 23514 an operator has to decode; running it
// twice would put the rule in two places.
func (w *PolicyWriter) CreatePolicy(
	ctx context.Context, scope db.TenantScope, in domain.PolicyDraft, idem Idempotency,
) (domain.Policy, error) {
	if w.store == nil {
		return domain.Policy{}, errs.Unavailable("policies_unavailable",
			"the policy store is not configured in this deployment", 0)
	}
	if err := w.requireClaims(idem); err != nil {
		return domain.Policy{}, err
	}

	var (
		out      domain.Policy
		replayOf uuid.UUID
	)
	// Named before it exists, so the claim below can record it.
	if in.ID == uuid.Nil {
		in.ID = id.New()
	}
	err := w.inTx(ctx, func(ctx context.Context) error {
		if idem.Keyed {
			res, err := w.claims.Claim(ctx, scope, idempotency.Claim{
				OrgID:       scope.OrgID(),
				PrincipalID: idem.Principal.UserID,
				Operation:   opCreateNotificationPolicy,
				Key:         idem.Key,
				RequestHash: idem.RequestHash,
				CreatedRef:  in.ID,
				ClaimedAt:   w.clk.Now().UTC(),
			})
			if err != nil {
				return err
			}
			if !res.Fresh() {
				if res.Outcome == idempotency.Conflicted || res.Existing.CreatedRef == uuid.Nil {
					// One key, two different bodies — or a replay that names nothing.
					// Either way the honest answer is the contract's `409`, which still
					// tells the client its first attempt succeeded.
					return idempotency.Reuse(res)
				}
				replayOf = res.Existing.CreatedRef
				return errPolicyReplay
			}
		}
		created, err := w.store.CreatePolicy(ctx, scope, in)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if errors.Is(err, errPolicyReplay) {
		// Read OUTSIDE the rolled-back transaction, so what comes back is the row
		// the FIRST attempt committed.
		return w.store.GetPolicy(ctx, scope, replayOf)
	}
	if err != nil {
		return domain.Policy{}, err
	}
	return out, nil
}

func (w *PolicyWriter) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if w.tx == nil {
		return fn(ctx)
	}
	return w.tx.Tx(ctx, fn)
}

// requireClaims refuses a KEYED request this deployment cannot honour.
//
// ⛔ IT IS REFUSED, NOT SERVED UNGUARDED. The defect this closes was a header the
// contract promised and the server ignored; ignoring it a second time because a
// collaborator is nil would reproduce it exactly.
func (w *PolicyWriter) requireClaims(idem Idempotency) error {
	if !idem.Keyed {
		return nil
	}
	if w.claims != nil && w.tx != nil {
		return nil
	}
	return errs.Unavailable(CodePolicyIdempotencyUnavailable,
		"this deployment cannot honour Idempotency-Key", 0)
}

// errPolicyReplay carries a replayed claim out of its own transaction so the
// insert beside it rolls back. It never reaches a caller.
var errPolicyReplay = errors.New("this idempotency key already created a policy")
