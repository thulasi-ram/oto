package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listNotificationPolicies serves GET /api/v1/notification-policies.
//
// The page is ordered by PRIORITY, lower first — the same order evaluation walks
// them in. A settings list that showed policies in a different order from the one
// they fire in would make "why did this go to #general?" unanswerable.
func (rt *Router) listNotificationPolicies(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.policies != nil, "policies_unavailable",
		"the policy store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, "limit", "cursor")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash("notification-policies"))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	policies, next, err := rt.policies.ListPolicies(r.Context(), scope, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]PolicyDTO, 0, len(policies))
	for _, pol := range policies {
		out = append(out, policyDTO(pol))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createNotificationPolicy serves POST /api/v1/notification-policies.
//
// ⛔ ROUTING IS SIGNAL → DESTINATION. The request DTO has no field naming a user,
// a team, a schedule, a rotation or a time of day, and it must never gain one: a
// policy that routes to a PERSON is a rota (SCOPE-BOUNDARY §5.3, FR-1, H-1).
//
// The domain's own Validate runs before the write, so a bad matcher regex or an
// out-of-range priority comes back as a field violation rather than as a 23514 an
// operator has to decode.
func (rt *Router) createNotificationPolicy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.policies != nil, "policies_unavailable",
		"the policy store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[CreatePolicyRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	draft, err := dto.toDraft()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := validateDraft(scope, draft); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	pol, err := rt.policies.CreatePolicy(r.Context(), scope, draft)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, policyDTO(pol), started)
}

// updateNotificationPolicy serves PATCH /api/v1/notification-policies/{id}.
func (rt *Router) updateNotificationPolicy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.policies != nil, "policies_unavailable",
		"the policy store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdatePolicyRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if dto.IsEmpty() {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"supply at least one field to change",
			errs.Violation{Field: "", Code: "min_properties", Message: "at least one property is required"}))
		return
	}

	patch, err := dto.toPatch()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The patch is validated against the MERGED policy rather than in isolation:
	// clearing `channel_ids` to an empty list is only invalid in the context of
	// the row it lands on, and a validator that could not see the row would have
	// to defer that to the CHECK constraint — a 500 where a 422 belongs.
	existing, err := rt.policies.GetPolicy(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if existing.DeletedAt != nil {
		httpx.WriteProblem(w, r, errs.NotFound("policy_deleted", "this policy has been deleted"))
		return
	}
	if err := validateMerged(existing, patch); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	pol, err := rt.policies.UpdatePolicy(r.Context(), scope, id, patch)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, policyDTO(pol), started)
}

// deleteNotificationPolicy serves DELETE /api/v1/notification-policies/{id}.
//
// It stops future matching. Notifications already created KEEP their `policy_id`
// reference until the row is purged, so the audit trail of WHY something was sent
// survives the policy that caused it.
func (rt *Router) deleteNotificationPolicy(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.policies != nil, "policies_unavailable",
		"the policy store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.policies.SoftDeletePolicy(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}
