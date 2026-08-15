package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
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

// idempotencyIntent reads the caller's `Idempotency-Key` into the intent the
// two token operations claim under. The protocol runs in THIS layer here — the
// identity service has no transaction seam of its own to carry it — so the
// operation is filled here too, and the claim itself is `idempotency.Resolve`
// inside each handler's unit of work.
//
// ⭐⭐ THE UNWIRED-DEPLOYMENT CHECK RUNS HERE BECAUSE HERE IS BEFORE THE MINT. A
// claim without a transaction to join is refused by `idempotency.Repository.Claim`
// — but that refusal arrives AFTER the handler has minted, and with no
// transaction there is nothing to roll the mint back: the token is issued and
// committed, the claim then fails, and the caller receives a `500` for a
// credential that exists and whose secret it never saw. That is a worse outcome
// than the bug. So both collaborators are demanded up front (idempotency.Require),
// while refusing still costs nothing.
func (rt *Router) idempotencyIntent(
	r *http.Request, op idempotency.Operation, hash idempotency.RequestHash,
) (idempotency.Intent, error) {
	in, err := idempotency.IntentFromRequest(r, hash)
	if err != nil {
		return idempotency.Intent{}, err
	}
	if err := idempotency.Require(in, rt.claims, rt.tx); err != nil {
		return idempotency.Intent{}, err
	}
	in.Operation = op
	return in, nil
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

	idem, err := rt.idempotencyIntent(r, opCreateAPIToken, idempotency.HashRequest(raw))
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
		if idem.Keyed {
			// ⭐⭐ INSIDE THE SAME TRANSACTION AS THE MINT, AND AFTER IT, because
			// the claim records the id of what the act created. A claim that loses
			// the race therefore rolls that act back with it, which is the
			// difference between "your retry was refused" and "your retry minted a
			// second live credential and told you it was a duplicate".
			if _, cerr := idempotency.Resolve(ctx, rt.claims, scope, idem,
				idempotency.Refuse, minted.Token.ID, rt.clk.Now()); cerr != nil {
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
	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	tokenID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ⭐ THE DIGEST IS OF THE TOKEN BEING REVOKED, not of the empty body. A
	// DELETE has no body, so a body digest here is a CONSTANT and `{id}` is
	// not in the claim tuple — one key would then make "revoke A" and "revoke
	// B" indistinguishable, and the second would be refused as a replay of a
	// request that destroyed a different credential. Folding the target in
	// makes them two different requests, which is what they are, while a true
	// retry against the same token still digests identically and still
	// replays.
	idem, err := rt.idempotencyIntent(r, opRevokeAPIToken,
		idempotency.HashTargetedRequest(tokenID, nil))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	err = rt.inTx(r.Context(), func(ctx context.Context) error {
		if rerr := rt.svc.RevokeToken(ctx, scope, tokenID); rerr != nil {
			return rerr
		}
		if !idem.Keyed {
			return nil
		}
		// A revoke CREATES nothing, so the claim carries no reference.
		_, cerr := idempotency.Resolve(ctx, rt.claims, scope, idem,
			idempotency.Refuse, uuid.Nil, rt.clk.Now())
		return cerr
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusNoContent, nil)
}
