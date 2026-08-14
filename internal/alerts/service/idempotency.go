package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// ⭐⭐ THIS FILE IS WHY A RETRIED SNOOZE OR COMMENT NO LONGER ACTS TWICE, AND IT
// IS IN THE SERVICE BECAUSE THE CLAIM MUST JOIN THE ACT'S OWN TRANSACTION.
//
// `Idempotency-Key` was declared on both operations by the contract and read by
// nothing (ticket a6cc834). Snooze superseded its own incumbent and inserted a
// second row; Comment minted its §C.8 dedupe key from the WALL CLOCK, so a retry
// arrived a few nanoseconds later, produced a different key, and appended a
// second annotation. Both are the exact failure the header exists to prevent, and
// both happened during precisely the network conditions a client retries under.
//
// ⛔ THERE IS NO MIDDLEWARE THAT COULD HAVE DONE THIS, and there cannot be:
// `platform/idempotency` refuses to claim outside the caller's transaction
// (`repository.go`, `idempotency_claim_outside_tx`), and a filter that ran before
// the handler opened one could never call Claim at all. It is wired per verb, the
// same way `createSource` and the three token operations were.

// The operationIds a key is claimed under here. One key must not be replayable
// across two different operations, so the operation is part of the claim's
// identity, and these are the contract's own operationIds spelled once so a claim
// and the contract cannot drift.
//
// ⭐ THE GROUP FORMS ARE THEIR OWN OPERATIONS. `snoozeAlertGroup` fans out onto
// the same `Snooze` primitive, but it is not the same request: one press means
// "quieten these forty", and claiming it under `snoozeAlert` would let a group
// gesture refuse a later single-alert one carrying the same client key.
// ⚠️ THEY ARE EXPORTED BECAUSE THE OPERATION IS THE CALLER'S FACT, NOT THIS
// SERVICE'S. `Snooze` serves two contract operations — `snoozeAlert` from the
// alerts router and `snoozeAlertGroup` from grouping's fan-out — and only the
// layer that received the request knows which one the caller's key belongs to.
var (
	OpSnoozeAlert         = idempotency.MustOperation("snoozeAlert")
	OpCommentOnAlert      = idempotency.MustOperation("commentOnAlert")
	OpSnoozeAlertGroup    = idempotency.MustOperation("snoozeAlertGroup")
	OpCommentOnAlertGroup = idempotency.MustOperation("commentOnAlertGroup")
)

var (
	errReplay        = errors.New("this key was already claimed; the act is rolled back and replayed")
	errNamelessClaim = errors.New("a keyed write must name the contract operation it is claimed under")
)

// CodeIdempotencyUnavailable means the caller sent an `Idempotency-Key` this
// deployment cannot honour. It is a DEPLOYMENT fact, not a caller error.
const CodeIdempotencyUnavailable = "idempotency_unavailable"

// Claims is the `Idempotency-Key` claim store, satisfied by
// `*platform/idempotency.Repository`.
//
// ⛔ It is OPTIONAL, like every other port this module declares — but "absent"
// does not degrade to "served unguarded". A KEYED request this deployment cannot
// honour is refused with a `503`; see requireClaims.
type Claims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
}

// Idempotency is the caller's `Idempotency-Key` intent for one human verb.
//
// ⭐⭐ THE CLAIM IT ASKS FOR IS TAKEN INSIDE THE VERB'S OWN TRANSACTION, so a key
// somebody already holds rolls the act back with it. That is the difference
// between "your retry was replayed" and "your retry superseded the snooze you
// already had and sent a second Slack message about it".
type Idempotency struct {
	// Keyed reports that the caller sent a key at all. False means every field
	// below is ignored, no claim is taken, and the verb behaves exactly as it did
	// before this file existed — which is what keeps the header optional.
	Keyed bool
	Key   idempotency.Key
	// Operation is the contract operationId the key belongs to, supplied by the
	// layer that read the header. One key must not be replayable across two
	// different operations, so it is part of the claim's identity.
	Operation idempotency.Operation
	// Principal is who sent it. A key is a client's private handle on its own
	// retry, so a claim is keyed by the principal as well as the org: one org
	// member's key must never be able to refuse another's request.
	Principal authn.Principal
	// RequestHash is what "the same request" means. Every verb here is addressed
	// by a path id and carries a body, so it is `HashTargetedRequest(subject,
	// body)` — see there for why the subject cannot be left out.
	RequestHash idempotency.RequestHash
}

// requireClaims refuses a KEYED request this deployment cannot honour.
//
// ⛔ IT IS REFUSED, NOT SERVED UNGUARDED. The defect this closes was a header the
// contract promised and the server ignored; ignoring it a second time because a
// collaborator is nil would reproduce it exactly, and the caller would have no
// way to tell a protected snooze from an unprotected one.
func (s *Service) requireClaims(idem Idempotency) error {
	if !idem.Keyed {
		return nil
	}
	if s.claims != nil && s.tx != nil {
		return nil
	}
	return errs.Unavailable(CodeIdempotencyUnavailable,
		"this deployment cannot honour Idempotency-Key", 0)
}

// claim takes the caller's key for op, and reports whether somebody — this
// caller's own first attempt — already did this.
//
// ⭐⭐ IT REPLAYS RATHER THAN REFUSING, AND THAT IS THE DIFFERENCE BETWEEN THESE
// VERBS AND THE FOUR THAT MINT A SECRET. `idempotency.Reuse` answers a held key
// with a `409` because a plaintext token genuinely cannot be produced twice.
// Nothing a snooze or a comment returns is a secret, so the honest answer to "did
// my retry land" is the row the first attempt made — which is what the contract's
// own wording promised all along. A key held for a DIFFERENT body is still that
// `409`: that is not a retry, it is a second request wearing the first one's name.
//
// ⛔ THE CALLER MUST ROLL BACK ON REPLAY. `replayed` means the act this call just
// performed is a duplicate and has to be undone, which is why every call site
// returns errReplay out of its transaction rather than carrying on.
func (s *Service) claim(
	ctx context.Context, scope db.TenantScope, idem Idempotency, created uuid.UUID,
) (replayed bool, ref uuid.UUID, err error) {
	if idem.Operation.IsZero() {
		return false, uuid.Nil, errs.Internal("idempotency_operation_missing", errNamelessClaim)
	}
	res, err := s.claims.Claim(ctx, scope, idempotency.Claim{
		OrgID:       scope.OrgID(),
		PrincipalID: idem.Principal.UserID,
		Operation:   idem.Operation,
		Key:         idem.Key,
		RequestHash: idem.RequestHash,
		CreatedRef:  created,
		ClaimedAt:   s.Now(),
	})
	if err != nil {
		return false, uuid.Nil, err
	}
	if res.Fresh() {
		return false, uuid.Nil, nil
	}
	if res.Outcome == idempotency.Conflicted {
		return false, uuid.Nil, idempotency.Reuse(res)
	}
	if res.Existing.CreatedRef == uuid.Nil {
		// A replay that names nothing cannot be served as one. Refusing is the
		// honest answer and carries the same `idempotency_key_reuse` code, so a
		// client still learns that its first attempt succeeded.
		return false, uuid.Nil, idempotency.Reuse(res)
	}
	return true, res.Existing.CreatedRef, nil
}

// commentDedupeKey is the §C.8 key a KEYED comment appends under.
//
// ⭐ IT IS DERIVED FROM THE CALLER'S KEY, NOT FROM THE WALL CLOCK, and that is
// what makes `alert_event_keys`' existing `ON CONFLICT` finally catch a retry.
// The unkeyed form (`Comment` below) still mints a clock key, because two people
// who type "restarted it" ten minutes apart wrote two facts and a content-derived
// key would silently discard the second — losing a human's words is worse than
// keeping one too many.
//
// ⛔ THE KEY IS HASHED RATHER THAN CONCATENATED, for a reason the constraint
// states: `alert_event_keys_ck` bounds a dedupe key at 200 characters and the
// contract lets a client send 200 on its own. `"comment:" + uuid + ":" + key`
// would overflow it and turn a legal header into a 23514. A sha256 is 64 hex
// characters whatever the client sent.
func commentDedupeKey(alertID uuid.UUID, idem Idempotency) string {
	sum := sha256.Sum256([]byte(idem.Key.String()))
	return "comment:" + alertID.String() + ":idem:" + hex.EncodeToString(sum[:])
}
