package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// The two operationIds this file claims keys under.
//
// ⭐ THE OPERATION IS PART OF THE KEY'S IDENTITY. A client that mints one key per
// user gesture — which oto's own frontend does — must not find its create refused
// by its revoke, and must never be told about a resource from the other endpoint.
// They are the contract's own operationIds, spelled once, so a claim and the
// contract cannot drift.
var (
	opCreateAPIToken = idempotency.MustOperation("createApiToken")
	opRevokeAPIToken = idempotency.MustOperation("revokeApiToken")
)

// tokenFilterHash binds a keyset cursor to the list it was minted for.
//
// The only axis this list varies on is the owner, so that is what the hash
// covers. A cursor minted for one user and replayed by another describes a
// position in a sequence that user never saw, and without the binding the server
// would serve a page from the middle of somebody else's list and nothing would
// look wrong (SPEC §E.1).
func (rt *Router) tokenFilterHash(p authn.Principal) string {
	return httpx.FilterHash("api-tokens", p.UserID.String())
}

// listAPITokens is `GET /api/v1/api-tokens` — operationId `listApiTokens`.
//
// ⚠️ THE SECRET IS NEVER SHOWN. APITokenDTO has no field for it and the row
// holds only a sha256, so there is no version of this handler that could return
// one.
//
// It requires a session: a token cannot enumerate its own siblings (contract:
// `security: [sessionCookie]`).
func (rt *Router) listAPITokens(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// SPEC §E.3: an unknown query parameter is REJECTED, never ignored. A typo'd
	// filter that is silently dropped returns a plausible page of the wrong rows.
	params := httpx.NewParams(r, "limit", "cursor")
	if err := params.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Layer 1 over a DTO assembled from the query string (§L.2.1). params.Limit
	// applies §E.1's silent cap; the DTO is what rejects a limit of zero or a
	// cursor that is not an opaque token.
	q, err := httpx.BindEmpty(PageQuery{Limit: params.Limit(), Cursor: params.Cursor()})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	cursor, err := httpx.DecodeCursor(q.Cursor, rt.tokenFilterHash(p))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	tokens, next, err := rt.svc.ListTokens(r.Context(), scope, p, httpx.Keyset(q.Limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.List(w, r, toAPITokenDTOs(tokens), httpx.PageOf(next, q.Limit), started)
}

// idempotencyKey reads the caller's `Idempotency-Key`, reporting whether one was
// sent.
//
// ⛔ A KEYED REQUEST THIS DEPLOYMENT CANNOT HONOUR IS REFUSED, NOT SERVED
// UNGUARDED. The whole defect this path is fixing was a header the contract
// promised and the server ignored; ignoring it a second time because a
// collaborator is nil would reproduce it exactly, and the caller would again have
// no way to tell a protected create from an unprotected one. `503` says so
// without inviting a retry of the same broken request.
//
// ⭐⭐ THE UNIT OF WORK IS PART OF THAT PRECONDITION, AND IT IS CHECKED HERE
// BECAUSE HERE IS BEFORE THE MINT. A claim without a transaction to join is
// refused by `idempotency.Repository.Claim` — but that refusal arrives AFTER the
// handler has minted, and with no transaction there is nothing to roll the mint
// back: the token is issued and committed, the claim then fails, and the caller
// receives a `500` for a credential that exists and whose secret it never saw.
// That is a worse outcome than the bug. So both collaborators are demanded up
// front, while refusing still costs nothing.
func (rt *Router) idempotencyKey(r *http.Request) (idempotency.Key, bool, error) {
	key, keyed, err := idempotency.FromHeader(r)
	if err != nil || !keyed {
		return key, keyed, err
	}
	if rt.claims == nil || rt.tx == nil {
		return idempotency.Key{}, false, errs.Unavailable("idempotency_unavailable",
			"this deployment cannot honour Idempotency-Key", 0)
	}
	return key, true, nil
}

// claim takes the caller's key for op, and turns a key somebody already holds
// into the contract's `409`.
//
// ⭐⭐ IT MUST BE CALLED INSIDE THE SAME TRANSACTION AS THE ACT IT GUARDS, and
// AFTER it, because the claim records the id of what the act created. A claim
// that loses the race therefore rolls that act back with it, which is the
// difference between "your retry was refused" and "your retry minted a second
// live credential and told you it was a duplicate".
//
// The hash is passed IN rather than computed here, because what "the same
// request" means differs by operation: a create is identified by its body, and a
// revoke — which has no body — is identified by the token it destroys.
func (rt *Router) claim(
	ctx context.Context, scope db.TenantScope, p authn.Principal,
	op idempotency.Operation, key idempotency.Key, hash idempotency.RequestHash, created uuid.UUID,
) error {
	res, err := rt.claims.Claim(ctx, scope, idempotency.Claim{
		OrgID:       scope.OrgID(),
		PrincipalID: p.UserID,
		Operation:   op,
		Key:         key,
		RequestHash: hash,
		CreatedRef:  created,
		ClaimedAt:   rt.clk.Now(),
	})
	if err != nil {
		return err
	}
	if !res.Fresh() {
		return idempotency.Reuse(res)
	}
	return nil
}

// createAPIToken is `POST /api/v1/api-tokens` — operationId `createApiToken`.
//
// ⚠️ THE 201 BODY CARRIES THE SECRET, AND IT IS THE ONLY RESPONSE IN OTO THAT
// EVER WILL. It is returned exactly once; only its sha256 is stored, so a lost
// token is replaced rather than recovered. The secret is not logged here, is not
// logged by the service, and exists in this process for the duration of one
// response write.
//
// ⭐⭐ THAT IS ALSO WHY A RETRY IS REFUSED RATHER THAN REPLAYED. The header
// promises "the original result rather than acting twice", and the original
// result here is a credential oto no longer has. Replaying it would mean storing
// every minted secret in the clear under a string the client chose; minting a
// second one would hand out a live credential whose secret went to a response
// that may never have arrived. So a claimed key is a `409` naming the token the
// first call created, and the mint that this call performed is rolled back with
// the failed claim — the two are one transaction, which this path did not have at
// all before.
func (rt *Router) createAPIToken(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The RAW bytes, before Bind consumes the stream: "the same body" is decided
	// by the sha256 of what the caller actually sent, not by a re-encoding of the
	// DTO it parsed into.
	raw, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req, err := httpx.Bind[CreateTokenRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	key, keyed, err := rt.idempotencyKey(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var issued service.IssuedToken
	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		minted, ierr := rt.svc.IssueToken(ctx, scope, p, req.toDomain())
		if ierr != nil {
			return ierr
		}
		if keyed {
			if cerr := rt.claim(ctx, scope, p, opCreateAPIToken, key,
				idempotency.HashRequest(raw), minted.Token.ID); cerr != nil {
				return cerr
			}
		}
		issued = minted
		return nil
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusCreated, toAPITokenCreatedDTO(issued), started)
}

// revokeAPIToken is `DELETE /api/v1/api-tokens/{id}` — operationId
// `revokeApiToken`.
//
// Revocation takes effect within the credential cache TTL, at most 60 seconds
// (contract). It is idempotent: revoking twice succeeds and does not move the
// revocation timestamp.
//
// A token in another org answers 404, never 403. A 403 would confirm that the id
// exists somewhere, which is a cross-tenant existence oracle; and v1's only
// cause of 403 is cross-org access, which is precisely the case this must not
// distinguish.
//
// ⭐ THE KEY IS STILL CLAIMED, ON AN ENDPOINT THAT DID NOT NEED IT. Revocation is
// idempotent by construction, so an UNKEYED retry answers `204` today and will
// keep doing so. A KEYED retry is answered `409` instead, for the same reason the
// create is: `Idempotency-Key` means one key names one request, and the family of
// credential endpoints answers it with ONE rule rather than three. A caller that
// wanted a second revoke sends a second key, which costs it nothing and makes the
// difference between "already done" and "done again" visible instead of guessed.
func (rt *Router) revokeAPIToken(w http.ResponseWriter, r *http.Request) {
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	tokenID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	key, keyed, err := rt.idempotencyKey(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		if rerr := rt.svc.RevokeToken(ctx, scope, tokenID); rerr != nil {
			return rerr
		}
		if !keyed {
			return nil
		}
		// A revoke CREATES nothing, so the claim carries no reference.
		//
		// ⭐ THE DIGEST IS OF THE TOKEN BEING REVOKED, not of the empty body. A
		// DELETE has no body, so a body digest here is a CONSTANT and `{id}` is
		// not in the claim tuple — one key would then make "revoke A" and "revoke
		// B" indistinguishable, and the second would be refused as a replay of a
		// request that destroyed a different credential. Folding the target in
		// makes them two different requests, which is what they are, while a true
		// retry against the same token still digests identically and still
		// replays.
		return rt.claim(ctx, scope, p, opRevokeAPIToken, key,
			idempotency.HashTargetedRequest(tokenID, nil), uuid.Nil)
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusNoContent, nil)
}
