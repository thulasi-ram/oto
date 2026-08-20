package idempotency

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Header is the contract's own spelling of the client-supplied key
// (`parameters/IdempotencyKeyHeader`).
//
// ⚠️ IT IS DECLARED ONCE, HERE. Two modules read this header and a third will;
// a handler that spelled it `Idempotency-key` would still compile, still pass its
// own tests, and quietly offer no protection at all on the one endpoint the
// contract advertises it hardest for.
const Header = "Idempotency-Key"

// CodeReuse is the refining `code` on the `409` an already-claimed key answers
// with (SPEC §L.1: a more specific code refines a Kind without changing its
// status).
//
// ⭐ THE CALLER MUST BE ABLE TO TELL THIS 409 FROM A DUPLICATE-NAME 409. They
// mean opposite things: a duplicate name means nothing happened and the caller
// should pick another name, while this one means THE FIRST ATTEMPT SUCCEEDED and
// the caller must stop retrying. A client that could not distinguish them would
// retry forever against a credential that already exists.
const CodeReuse = "idempotency_key_reuse"

// FromHeader reads the caller's key, reporting whether one was sent at all.
//
// ⭐ THE HEADER IS OPTIONAL AND MUST STAY OPTIONAL. The contract declares it
// `required: false` on every operation that takes it, and oto's own frontend
// sends it on some and not others. A handler that demanded it would break every
// client that never sent one in order to protect the clients that did — so an
// absent header means "behave exactly as before", and only a PRESENT one buys the
// guarantee.
//
// A key outside the contract's own 1..200 bound is refused by NewKey as a
// validation failure, which is the same answer the schema would have given: a
// value layer 1 accepts that layer 6 refuses is a 500 where nothing belongs
// (SPEC §L.1).
func FromHeader(r *http.Request) (Key, bool, error) {
	raw := r.Header.Get(Header)
	if raw == "" {
		return Key{}, false, nil
	}
	k, err := NewKey(raw)
	if err != nil {
		return Key{}, false, err
	}
	return k, true, nil
}

// IntentFromRequest reads the caller's `Idempotency-Key` and resolves the
// principal that sent it, into the Intent a keyed write acts on. An absent
// header is the zero Intent and no error, exactly as FromHeader reports it.
//
// ⭐ READING THE HEADER IS THE TRANSPORT LAYER'S JOB; TAKING THE CLAIM IS NOT.
// A claim has to be taken inside the transaction of the act it guards, and
// that transaction belongs to whichever service owns the act — so what crosses
// the seam is the caller's intent, and the claiming side decides whether this
// deployment can honour it (a `503`), whether somebody already holds the key
// for a different body (a `409`), and whether this call is a replay of one it
// already served.
//
// The hash is passed in because what "the same request" means is the
// operation's own choice: the raw body for a create, HashTargetedRequest for a
// verb addressed by `{id}` — without which a client that mints one key per
// gesture (which oto's own frontend does) and acts on subject A then subject B
// under one key would be told "that request already succeeded" about a request
// it never made. Operation is left for the layer that owns that fact — see
// Intent.Operation.
//
// ⛔ `KeyID` IS DELIBERATELY LEFT ZERO ON THIS PATH, and a reader who fills it in
// for symmetry will re-key notifications nobody asked to move. An HTTP caller that
// reaches here is AUTHENTICATED, so it has a principal and its key is claimable:
// the claim already converges its retries before any announcement is minted, and
// the field exists only for the callers a claim cannot reach (`Intent.KeyID`).
// Setting it here would change the §C.7 occasion of every API-driven snooze from
// the `alert_snoozes.id` SPEC §C.7 names to a digest of a client-chosen header,
// buying nothing the claim does not already do.
func IntentFromRequest(r *http.Request, hash RequestHash) (Intent, error) {
	key, keyed, err := FromHeader(r)
	if err != nil || !keyed {
		return Intent{}, err
	}
	p, _, err := authn.Scope(r.Context())
	if err != nil {
		return Intent{}, err
	}
	return Intent{Keyed: true, Key: key, Principal: p, RequestHash: hash}, nil
}

// Reuse is the refusal a key that is ALREADY CLAIMED answers with, for the
// endpoints whose response carries a credential.
//
// ⭐⭐ IT REFUSES RATHER THAN REPLAYING, AND THAT IS THE WHOLE DESIGN. The
// header's description reads like an instruction to cache and re-serve the first
// response, and for `createApiToken`, `createSource` and `rotateSourceIngestToken`
// that response IS a plaintext secret. Storing it would turn a table meant to prevent one
// orphaned credential into a table holding every credential, in the clear,
// addressed by a string the client chose — a worse posture than the bug. So oto
// tells the truth instead: the first attempt SUCCEEDED, here is the id of what it
// made, and the secret genuinely cannot be produced a second time.
//
// ⛔ NOTHING IT RETURNS IS A SECRET, and there is no field on Result that could
// hold one. The detail names an id, which is exactly what a caller who never
// received the secret needs in order to revoke it and start again.
func Reuse(res Result) error {
	if res.Fresh() {
		// A fresh claim is the call that is ALLOWED to act. Refusing it would
		// discard a mint that has already happened, so this is a handler bug and is
		// reported as one rather than becoming a 409 the caller cannot act on.
		return errs.Internal("idempotency_reuse_of_fresh_claim", errFreshReuse)
	}
	if res.Outcome == Conflicted {
		return errs.Conflict(CodeReuse,
			"this Idempotency-Key was already used for a different request; a key names one "+
				"request and cannot be reused for another")
	}
	if ref := res.Existing.CreatedRef; ref != uuid.Nil {
		return errs.Conflict(CodeReuse,
			"this Idempotency-Key was already used, and that request created "+ref.String()+
				"; its secret was returned to that request and oto cannot return it a second time. "+
				"If you never received it, revoke or rotate that resource's credential and retry "+
				"with a new key")
	}
	return errs.Conflict(CodeReuse,
		"this Idempotency-Key was already used and that request succeeded; oto does not act "+
			"twice on one key. Retry with a new key if you meant a new request")
}

var errFreshReuse = errors.New("a newly claimed key was refused as a replay")
