package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listPolicyDigestInvestigations serves GET /api/v1/notification-policies/{id}/investigations
// (review D4): the runs a policy's digest windows asked for, latest first, keyset-paged.
//
// ⭐ IT IS HOW A DIGEST RUN'S RECORD IS READ AT ALL. A Case's runs are on the Case and an
// Incident's on the Incident; a digest window's run had no list, and nothing handed out its
// id — so a run skipped for the day's budget or because its window closed, or one that
// failed, was on the record and unreachable. The answer is the same InvestigationDTO the
// other two lists answer, so the web reuses its row.
//
// A policy this org does not have, or a deleted one, is a 404 — the service reads it
// through the same port a count Suggestion's apply does.
func (rt *Router) listPolicyDigestInvestigations(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	policyID, err := pathPolicy(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, "limit", "cursor")
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	hash := httpx.FilterHash("digest_policy_id=" + policyID.String())
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	runs, next, err := rt.svc.ListPolicyDigestInvestigations(r.Context(), scope, policyID, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]InvestigationDTO, 0, len(runs))
	for _, inv := range runs {
		out = append(out, investigationDTO(inv))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// pathPolicy reads the policy `{id}`. Anything that is not a UUID names no policy: 404,
// `policy_not_found`, the answer the policy routes give another org's policy.
func pathPolicy(r *http.Request) (uuid.UUID, error) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return uuid.Nil, errs.NotFound("policy_not_found", "no such notification policy")
	}
	return id, nil
}
