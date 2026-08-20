package idempotency

import (
	"crypto/sha256"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// RetentionWindow is how long a claimed key keeps its claim — the "retention
// window" the `Idempotency-Key` header's own description names, and which the
// contract states no number for.
//
// ⭐ THE FLOOR is how long one logical request keeps being retried by somebody
// who still thinks it might not have happened: an HTTP client's retry budget, a
// proxy that gave up and was re-driven, a queued client draining when the network
// returns, a person who closed the laptop and came back to press the button
// again. Minutes do not cover the last two. ⭐ THE CEILING is that a claim
// outliving the caller's memory of making it protects nobody, and every row is
// rent on a table that must stay cheap to prune. Twenty-four hours is also the
// window every mainstream payments API publishes, so a client library's default
// assumption is already right against oto.
//
// ⛔ IT IS NOT A SECOND `DedupTTL` AND DOES NOT CONTRADICT ONE.
// `ingestion/domain.DedupTTL` is five minutes and is bounded from ABOVE by
// `refire_grace`, because it suppresses SIGNAL: a window as wide as the grace
// period makes the re-fire path unreachable, since an alert firing again inside
// it is dropped at ingest and the state machine never sees it. That ceiling is
// the whole of its reasoning and none of it applies here. This window suppresses
// no signal. It suppresses a second MINTING of a credential by an authenticated
// caller — an act that is never a repeat of a real-world event the way a re-fire
// is, and that nothing is waiting to observe. The two numbers are 288× apart
// precisely so neither is ever read as the other.
const RetentionWindow = 24 * time.Hour

// The bounds of `idempotency_claims`, each mirroring a named DDL CHECK.
const (
	// MinKeyLen and MaxKeyLen mirror idempotency_claims_key_ck, which is the
	// contract's own bound on the header (`minLength: 1, maxLength: 200`).
	MinKeyLen = 1
	MaxKeyLen = 200

	// RequestHashBytes mirrors idempotency_claims_hash_ck: a sha256 digest,
	// exactly 32 bytes.
	RequestHashBytes = 32

	// PatternOperation mirrors idempotency_claims_op_ck.
	//
	// ⚠️ Declared here rather than in `internal/platform/validate` for the same
	// reason `identity/domain.PatternTokenPrefix` is — it bounds a value no DTO
	// carries — and written exactly as the migration spells it so it can be lifted
	// into `validate/patterns.go` unchanged if a DTO ever needs it.
	PatternOperation = `^[a-z][a-zA-Z0-9]{0,63}$`
)

var operationRe = regexp.MustCompile(PatternOperation)

// Key is the client's own `Idempotency-Key`, stored verbatim.
//
// ⛔ IT IS OPAQUE AND IS NOT NORMALISED. oto compares it and never parses it: the
// contract promises to accept any 1..200 character string, so trimming it, case
// folding it or requiring a shape would make two keys the caller believes are
// different collide, or reject a key the contract said was legal — and a value
// layer 1 accepts that layer 6 refuses is a 500 where nothing at all belongs
// (SPEC §L.1).
type Key struct{ v string }

// NewKey parses a client-supplied key, enforcing idempotency_claims_key_ck.
//
// ⚠️ The message names the BOUND and not the constraint. A problem+json body
// echoing a constraint name publishes an internal invariant to every caller,
// including the ones probing for one (SPEC §L.3).
func NewKey(s string) (Key, error) {
	if l := len(s); l < MinKeyLen || l > MaxKeyLen {
		return Key{}, errs.Validation("invalid_idempotency_key",
			"Idempotency-Key must be 1..200 characters")
	}
	return Key{v: s}, nil
}

// String returns the key. It is not a secret: it is a handle the caller chose and
// will send again on its own retry.
func (k Key) String() string { return k.v }

// IsZero reports whether the key is unset.
func (k Key) IsZero() bool { return k.v == "" }

// ID is the key reduced to a uuid: a deterministic, non-reversible name for the
// ONE request this key identifies.
//
// ⭐⭐ IT EXISTS FOR THE WRITES THAT MUST TELL TWO ATTEMPTS APART EVEN WHERE NO
// CLAIM CAN BE TAKEN. A claim needs a principal — `idempotency_claims.principal_id`
// is NOT NULL and `Claim.validate` refuses a zero one — while merely NAMING an
// attempt needs nothing but the key. A Slack member who never linked an oto
// account has the second and not the first, and `alerts/service.Snooze` reads this
// as its §C.7 occasion so a REDELIVERED press re-mints the announcement key it
// already minted instead of a fresh one. See `Intent.KeyID`, which is the field
// that carries it, for the whole argument.
//
// ⛔ IT IS A DIGEST, NOT AN ENCODING, and that is not tidiness. Whatever holds a
// derived id stores it: this path's key is a sha256 over a Slack `response_url`
// whose last path segment is a one-shot bearer token, and a value that could be
// walked back to it has no business in a notification's pre-image. A digest of a
// digest discloses neither.
//
// ⚠️ A ZERO KEY IS uuid.Nil AND MUST STAY SO. `uuid.Nil` is the "no occasion"
// sentinel downstream; mapping the empty key onto a real uuid would hand EVERY
// unnamed request in the deployment the SAME occasion, which is strictly worse
// than having none — two genuinely different snoozes would then collide on one
// §C.7 key and the second would be swallowed as a duplicate of the first.
//
// The pre-image is a fixed domain tag, a NUL, then the key. The tag makes the
// digest a function of what it is FOR as well as of the key, which is ADR 0022's
// concern — one pre-image shape must not be reachable from another — and with
// exactly one variable field, at the end, there is nothing that could be
// re-split. The version and variant nibbles are then stamped per RFC 4122 §4.3,
// which is also why this can never return `uuid.Nil` by accident: byte 6 is at
// least `0x50` for every input.
func (k Key) ID() uuid.UUID {
	if k.IsZero() {
		return uuid.Nil
	}
	sum := sha256.Sum256([]byte(keyIDDomain + "\x00" + k.v))
	var out uuid.UUID
	copy(out[:], sum[:len(out)])
	out[6] = (out[6] & 0x0f) | 0x50
	out[8] = (out[8] & 0x3f) | 0x80
	return out
}

// keyIDDomain is the domain tag in Key.ID's pre-image.
//
// ⛔ IT IS A CONSTANT AND NOT A KNOB. Changing it re-derives every id any key ever
// produced, which for the two snooze Reasons means one extra card per in-flight
// snooze at the moment of deploy — the `v1` is there so a future shape gets a new
// tag rather than a redefinition of this one.
const keyIDDomain = "oto/platform/idempotency/key-id/v1"

// Operation is the contract operationId a key was claimed for, e.g.
// `createApiToken`.
//
// ⭐ IT IS PART OF THE IDENTITY OF A CLAIM. One key must not be replayable across
// two DIFFERENT operations: a client that mints one key per user gesture — which
// oto's own frontend does — would otherwise find its second call refused by its
// first, and be told about a resource from the wrong endpoint. The key answers
// "is this the same request", and two operations are never the same request.
type Operation struct{ v string }

// NewOperation parses an operationId, enforcing idempotency_claims_op_ck.
//
// oto supplies this value and a caller never does, so a violation here is a
// handler bug — passing a URL path or a method+path string — and it is caught at
// the call site rather than as a 23514 from Postgres.
func NewOperation(s string) (Operation, error) {
	if !operationRe.MatchString(s) {
		return Operation{}, errs.Validation("invalid_idempotency_operation",
			"an operation is a contract operationId")
	}
	return Operation{v: s}, nil
}

// MustOperation is NewOperation for an operationId known at compile time, so a
// handler can declare its operation as a package-level value. It panics, which is
// correct for a literal that is wrong: the process must not start.
func MustOperation(s string) Operation {
	op, err := NewOperation(s)
	if err != nil {
		panic("idempotency: not an operationId: " + s)
	}
	return op
}

// String renders the operationId.
func (o Operation) String() string { return o.v }

// IsZero reports whether the operation is unset.
func (o Operation) IsZero() bool { return o.v == "" }

// RequestHash is the sha256 of a request body, and the ONLY thing that decides
// whether a replay carries "the same body".
//
// ⛔ THE BODY IS NEVER STORED. The hash answers the whole question, and a stored
// body would be a copy of every request an authenticated caller ever sent to a
// credential endpoint.
type RequestHash [RequestHashBytes]byte

// HashRequest digests a request body.
//
// It lives here so both call sites hash the same bytes the same way: a replay is
// only recognisable if the second request's digest is computed identically to the
// first's. A nil or empty body hashes to the sha256 of the empty string, which is
// a stable value — an operation with no body (a revoke) therefore compares equal
// to itself across retries rather than being unclaimable.
func HashRequest(body []byte) RequestHash {
	return RequestHash(sha256.Sum256(body))
}

// targetTag domain-separates a targeted digest from a plain body digest, so no
// body can ever hash to the same value as some request against some resource.
const targetTag = "oto/idempotency/target\x00"

// HashTargetedRequest digests a request body TOGETHER WITH the resource the
// request acts on, for the operations whose subject lives in the PATH rather
// than in the body.
//
// ⭐⭐ WITHOUT IT, ONE KEY REFUSES A GENUINELY DIFFERENT REQUEST. `revokeApiToken`
// and `rotateSourceIngestToken` send no body at all, so `HashRequest(nil)` is a
// CONSTANT — every such request has the identical digest — and `{id}` is not part
// of the claim tuple. A client that mints one key per gesture (which oto's own
// frontend does) and revokes token A and then token B under it would be told
// "that request already succeeded", naming nothing, about a request it has never
// made. The two calls are not the same request: they destroy different
// credentials.
//
// ⭐ THE TARGET GOES IN THE HASH RATHER THAN IN THE TUPLE because the hash is
// already the field that answers "is this the same request", and the tuple is
// the field that answers "whose key is this". Folding it in costs no column and
// no migration, and it makes the refusal HONEST: two targets under one key are
// `Conflicted` — "this key was already used for a different request" — which is
// exactly what happened, rather than a replay that names a resource the caller
// never asked about. A true retry against the SAME target still digests
// identically and still replays.
func HashTargetedRequest(target uuid.UUID, body []byte) RequestHash {
	h := sha256.New()
	h.Write([]byte(targetTag))
	h.Write(target[:])
	h.Write(body)
	var out RequestHash
	h.Sum(out[:0])
	return out
}

// NewRequestHash wraps a stored digest, enforcing the 32-byte CHECK.
func NewRequestHash(b []byte) (RequestHash, error) {
	var h RequestHash
	if len(b) != RequestHashBytes {
		return RequestHash{}, errs.Validation("invalid_idempotency_hash",
			"a request hash is a 32-byte sha256 digest")
	}
	copy(h[:], b)
	return h, nil
}

// Bytes returns the digest for a query parameter.
func (h RequestHash) Bytes() []byte { return h[:] }

// IsZero reports whether the digest is unset.
func (h RequestHash) IsZero() bool { return h == RequestHash{} }

// Claim is one row of `idempotency_claims`: the record that a key was used, what
// body it was used with, and what that use created.
//
// ⛔ THERE IS NO FIELD HERE FOR A RESPONSE OR A SECRET, and adding one would undo
// the entire point of this package. See the package doc.
type Claim struct {
	// OrgID is the tenant. It must equal the caller's TenantScope.
	OrgID uuid.UUID
	// PrincipalID is the acting principal (`authn.Principal.UserID` for a session
	// or a PAT). A key is a client's private handle on its own retry, so one org
	// member's key must never block another's.
	PrincipalID uuid.UUID
	// Operation is the contract operationId this key belongs to.
	Operation Operation
	// Key is the caller's string.
	Key Key
	// RequestHash is sha256 of the request body.
	RequestHash RequestHash
	// CreatedRef is the id of the row this call created — an `api_tokens.id` or
	// an `alert_sources.id` today — so a replay can name what already exists
	// instead of leaving the caller to guess. Zero when the operation creates
	// nothing.
	CreatedRef uuid.UUID
	// ClaimedAt is when the key was claimed, and the prune horizon.
	ClaimedAt time.Time
}

// The causes behind an invalid claim. They are `errs.Internal` causes and never
// reach a caller: every one of them is a wiring bug in oto, not something a
// request could have got wrong.
var (
	errNoOrg       = errors.New("a claim belongs to exactly one org")
	errNoPrincipal = errors.New("a claim names the principal that made it")
	errNoOperation = errors.New("a claim names the operation it belongs to")
	errNoKey       = errors.New("a claim needs the caller's key")
	errNoHash      = errors.New("a claim needs the request hash")
	errNoTime      = errors.New("a claim needs the instant it was made")
)

// validate enforces what the value objects cannot: that the row names a tenant, a
// principal and an instant. A zero here is a wiring bug and must not reach the
// driver as a 23502.
func (c Claim) validate() error {
	switch {
	case c.OrgID == uuid.Nil:
		return errs.Internal("idempotency_claim_invalid", errNoOrg)
	case c.PrincipalID == uuid.Nil:
		return errs.Internal("idempotency_claim_invalid", errNoPrincipal)
	case c.Operation.IsZero():
		return errs.Internal("idempotency_claim_invalid", errNoOperation)
	case c.Key.IsZero():
		return errs.Internal("idempotency_claim_invalid", errNoKey)
	case c.RequestHash.IsZero():
		// A zero digest is never a real sha256 of anything oto has hashed, so it
		// means the caller forgot to hash rather than that the body was empty —
		// HashRequest(nil) is the digest of the empty string, which is not zero.
		return errs.Internal("idempotency_claim_invalid", errNoHash)
	case c.ClaimedAt.IsZero():
		return errs.Internal("idempotency_claim_invalid", errNoTime)
	}
	return nil
}

// Outcome is what a Claim call did. There are exactly three answers and no
// fourth: the caller must be able to tell "I am the first" from "somebody already
// did this with my body" from "somebody already did this with a different body",
// because those are a mint, a replay and a 409 respectively.
type Outcome string

// The three outcomes.
const (
	// Claimed means the key was NEWLY claimed by this call. Only this outcome may
	// mint anything.
	Claimed Outcome = "claimed"
	// Replayed means the key was already claimed by the SAME body. The caller's
	// earlier attempt succeeded; this one must not act again.
	Replayed Outcome = "replayed"
	// Conflicted means the key was already claimed by a DIFFERENT body, which the
	// contract has always said is a 409.
	Conflicted Outcome = "conflicted"
)

// Result is what Claim reports.
type Result struct {
	// Outcome is which of the three happened.
	Outcome Outcome
	// Existing is the claim that owns the key AFTER the call: the one just
	// written on Claimed, and the incumbent on Replayed and Conflicted. On a
	// replay its CreatedRef is what the first call made, which is how a caller is
	// told which credential already exists without being shown its secret.
	Existing Claim
}

// Fresh reports whether this call is the one allowed to act.
//
// It exists so a caller writes `if !res.Fresh()` rather than comparing against
// one of three constants and forgetting the third: the failure mode of a missed
// case here is minting a second live credential.
func (r Result) Fresh() bool { return r.Outcome == Claimed }
