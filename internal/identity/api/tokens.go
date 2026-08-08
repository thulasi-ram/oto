package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/httpx"
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

// createAPIToken is `POST /api/v1/api-tokens` — operationId `createApiToken`.
//
// ⚠️ THE 201 BODY CARRIES THE SECRET, AND IT IS THE ONLY RESPONSE IN OTO THAT
// EVER WILL. It is returned exactly once; only its sha256 is stored, so a lost
// token is replaced rather than recovered. The secret is not logged here, is not
// logged by the service, and exists in this process for the duration of one
// response write.
func (rt *Router) createAPIToken(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req, err := httpx.Bind[CreateTokenRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	issued, err := rt.svc.IssueToken(r.Context(), scope, p, req.toDomain())
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

	if err := rt.svc.RevokeToken(r.Context(), scope, tokenID); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusNoContent, nil)
}
