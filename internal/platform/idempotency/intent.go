package idempotency

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐⭐ THIS FILE IS THE ONE SPELLING OF THE KEYED-WRITE PROTOCOL. Six call sites
// used to re-declare the intent struct, the unwired-deployment refusal and the
// claim interpretation, and they had already drifted into three subtly different
// answers for the same edge. A protocol that exists to make retries converge
// must itself converge: the modules keep only their operationIds, their choice
// of request hash, and what a replay read returns.

// CodeUnavailable means the caller sent an `Idempotency-Key` this deployment
// cannot honour. It is a DEPLOYMENT fact, not a caller error.
const CodeUnavailable = "idempotency_unavailable"

// ClaimStore is the claim table, satisfied by *Repository. It is declared here
// so Resolve can be handed any module's own claims port: each consumer keeps
// its interface (§F.5), and Go's structural typing makes them interchangeable.
type ClaimStore interface {
	Claim(ctx context.Context, s db.TenantScope, c Claim) (Result, error)
}

// Compile-time proof the repository satisfies the store Resolve consumes.
var _ ClaimStore = (*Repository)(nil)

// Intent is the caller's `Idempotency-Key` intent for one keyed write.
//
// ⭐⭐ THE CLAIM IT ASKS FOR IS TAKEN INSIDE THE VERB'S OWN TRANSACTION, so a key
// somebody already holds rolls the act back with it. That is the difference
// between "your retry was replayed" and "your retry superseded the snooze you
// already had and sent a second Slack message about it".
type Intent struct {
	// Keyed reports that the caller sent a key oto can CLAIM. False means no
	// claim is taken and the verb behaves exactly as it did before the header was
	// read — which is what keeps the header optional. ⚠️ It used to mean "every
	// field below is ignored", and that is now true of all but one: `KeyID` is
	// read either way, and the field says why.
	Keyed bool
	Key   Key
	// KeyID is `Key.ID()` — the caller's key reduced to a uuid — and it is the
	// ONE field a verb may read while `Keyed` is false.
	//
	// ⭐⭐ IT SEPARATES "THIS REQUEST HAS A NAME" FROM "THIS REQUEST CAN BE
	// CLAIMED", two facts the header's failure modes had quietly fused into one
	// boolean. Claiming needs a principal; naming does not. A Slack member who
	// never linked an oto account has no principal uuid to claim under at all
	// (`idempotency_claims.principal_id` is NOT NULL, `Claim.validate` refuses a
	// zero one, and nothing may invent one — see `app.slackIdempotency`), yet
	// Slack still sends a per-interaction `response_url`, so their press IS
	// named. Dropping the whole intent because half of it was unusable threw the
	// name away with the claim.
	//
	// ⛔ IT IS NOT A WEAKER CLAIM AND IT CONVERGES NO ACT. A named-but-unclaimed
	// retry still performs the write twice; only a claim can stop that. What the
	// name buys is a stable identity for the DOWNSTREAM keys that must not move
	// between two executions of one request: `alerts/service.Snooze` uses it as
	// the §C.7 occasion, so a redelivered press mints the notification key it
	// already minted and the second card is swallowed by
	// `notifications_idem_uniq` instead of posted into the channel.
	//
	// Set it whenever the caller holds a per-request identity, keyed or not.
	// Leave it zero when it holds none: zero means "no name", and the verbs that
	// read it fall back to whatever they named occasions before it existed.
	KeyID uuid.UUID
	// Operation is the contract operationId the key belongs to. One key must
	// not be replayable across two different operations, so it is part of the
	// claim's identity. It is filled by whichever layer OWNS that fact: the
	// header-reading layer when one verb serves two operations (`Snooze` serves
	// `snoozeAlert` and grouping's `snoozeAlertGroup`), and the claiming verb
	// itself everywhere else.
	Operation Operation
	// Principal is who sent it. A key is a client's private handle on its own
	// retry, so a claim is keyed by the principal as well as the org: one org
	// member's key must never be able to refuse another's request.
	Principal authn.Principal
	// RequestHash is what "the same request" means, and the choice is the
	// operation's own: the sha256 of the raw body for a create,
	// HashTargetedRequest for anything addressed by a path id, the subject
	// alone for a bodyless verb.
	RequestHash RequestHash
}

// Require refuses a KEYED request this deployment cannot honour, because one of
// the collaborators the claim needs — the claim store, the unit of work — was
// not wired. Pass those collaborators; any nil one is the refusal.
//
// ⛔ IT IS REFUSED, NOT SERVED UNGUARDED. The defect this protocol closes was a
// header the contract promised and the server ignored; ignoring it a second
// time because a collaborator is nil would reproduce it exactly, and the caller
// would have no way to tell a protected write from an unprotected one. `503`
// says so without inviting a retry of the same broken request.
//
// ⭐⭐ IT MUST RUN BEFORE THE ACT, NOT MERELY BEFORE THE CLAIM. A claim with no
// transaction to join is refused by `Repository.Claim` too — but that refusal
// arrives AFTER a mint, and with no transaction there is nothing to roll the
// mint back: the credential is issued and committed, the claim then fails, and
// the caller receives a `500` for a secret it never saw. Demanding the
// collaborators up front costs one cheap check.
func Require(idem Intent, collaborators ...any) error {
	if !idem.Keyed {
		return nil
	}
	for _, c := range collaborators {
		if c == nil {
			return errs.Unavailable(CodeUnavailable,
				"this deployment cannot honour Idempotency-Key", 0)
		}
	}
	return nil
}

// OnReplay is a verb's answer to a key that was already claimed for the SAME
// body. There are exactly two, and which one an operation gets is decided by
// whether its response carries a secret.
type OnReplay int

const (
	// Refuse answers every already-claimed key with the contract's `409`, for
	// the operations whose response is a plaintext credential (`createApiToken`,
	// `createSource`, `rotateSourceIngestToken`, `revokeApiToken` by family
	// rule). A secret genuinely cannot be produced twice, and storing one to
	// replay it would be a worse posture than the bug — see Reuse.
	Refuse OnReplay = iota
	// Replay reports the first attempt's result instead of acting again, for
	// the operations whose response holds no secret. The honest answer to "did
	// my retry land" is the row the first attempt made — which is what the
	// header's own wording promised all along.
	Replay
)

// ErrReplay reports that the caller's key was already claimed for the same
// body, under an operation whose policy is Replay. It never reaches a caller.
//
// ⛔ THE CALLER MUST ROLL BACK AND READ OUTSIDE THE TRANSACTION. ErrReplay
// means the act this transaction just performed is a duplicate and has to be
// undone, which is why every call site returns it out of its unit of work —
// rolling the duplicate back — and then serves the replay from a read taken
// OUTSIDE it, so what comes back is the row the FIRST attempt committed and
// nothing this one did.
var ErrReplay = errors.New(
	"this key was already claimed; the act is rolled back and replayed")

// errNamelessClaim is the wiring bug behind a claim with no operation.
var errNamelessClaim = errors.New(
	"a keyed write must name the contract operation it is claimed under")

// Resolve takes the caller's key and interprets the outcome under the verb's
// replay policy. It must be called INSIDE the transaction of the act it guards
// (the store enforces that), and with createdRef naming what this attempt
// created — or uuid.Nil for a verb that creates no row.
//
// The three answers:
//
//	(uuid.Nil, nil)       — fresh claim; this call is the one allowed to act.
//	(uuid.Nil, 409)       — a Conflicted key (same key, DIFFERENT body: not a
//	                        retry, a second request wearing the first one's
//	                        name), any non-fresh key under Refuse, or a replay
//	                        that names nothing (below).
//	(ref, ErrReplay)      — a true replay under Replay; roll back, then serve
//	                        what ref names from outside the transaction.
//
// ⭐ THE RECONCILED EDGE: a replay whose incumbent claim has no CreatedRef is
// answered with Reuse's `409` — but ONLY when this attempt itself carries a
// createdRef. A create whose replay names nothing cannot be served as one, and
// refusing carries the same `idempotency_key_reuse` code, so the client still
// learns that its first attempt succeeded. A verb that creates no row
// (createdRef == uuid.Nil, e.g. `testChannel`) legitimately replays a claim
// that names nothing: its replay is served from state the first attempt wrote
// elsewhere, not from anything the claim records.
func Resolve(
	ctx context.Context, claims ClaimStore, scope db.TenantScope,
	idem Intent, policy OnReplay, createdRef uuid.UUID, now time.Time,
) (replayOf uuid.UUID, err error) {
	if idem.Operation.IsZero() {
		// Caught here rather than as the repository's generic
		// `idempotency_claim_invalid`: the operation is the one claim field that
		// crosses a module seam (grouping hands alerts its own operationIds), so
		// the specific code names the seam that dropped it.
		return uuid.Nil, errs.Internal("idempotency_operation_missing", errNamelessClaim)
	}
	res, err := claims.Claim(ctx, scope, Claim{
		OrgID:       scope.OrgID(),
		PrincipalID: idem.Principal.UserID,
		Operation:   idem.Operation,
		Key:         idem.Key,
		RequestHash: idem.RequestHash,
		CreatedRef:  createdRef,
		ClaimedAt:   now,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if res.Fresh() {
		return uuid.Nil, nil
	}
	if policy == Refuse || res.Outcome == Conflicted {
		return uuid.Nil, Reuse(res)
	}
	if createdRef != uuid.Nil && res.Existing.CreatedRef == uuid.Nil {
		return uuid.Nil, Reuse(res)
	}
	return res.Existing.CreatedRef, ErrReplay
}
