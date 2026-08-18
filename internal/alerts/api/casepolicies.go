package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// The CONFIGURATION surface of `case_policy_config` (§B.5, §B.6, migration 00057):
// the CASE RETENTION WINDOW W, per (namespace, alertname).
//
// ⭐⭐ WHY THIS FILE EXISTS. Migration 00057 shipped the table, the reader and the
// §B.3 machine that obeys W, and nothing anywhere could write a row: the feature
// was complete and unreachable, settable only by a hand-typed INSERT. A knob only
// reachable through psql is not a knob.
//
// ⭐ THE SHAPE IS `/api/v1/clusters`'s, DELIBERATELY. That is oto's existing
// per-org config collection with an IMMUTABLE NATURAL KEY and one mutable value —
// list, create by natural key, patch by id, and the key absent from the patch DTO —
// which is exactly this table's shape. There is no `PUT` in this codebase and no
// upsert-by-natural-key on any human-facing collection; a duplicate pair meets
// `case_policy_axes_uniq` and is answered `409`, and the retry story is the
// `Idempotency-Key` header rather than a second create that silently overwrites.
//
// ⛔ NO BOUND IS WRITTEN IN THIS FILE. The range 0..86400 lives in
// `case_policy_window_ck`, is mirrored once in `alerts/domain/casepolicy.go`, and is
// restated by layer 1 only as a `validate` tag and a `minimum`/`maximum` in the
// contract. A handler that range-checked W itself would be the fourth copy and the
// first to drift.

// listCasePolicies serves GET /api/v1/case-policies.
//
// The page is ordered by alertname, then namespace — the order an operator reads
// them in, and the order `case_policy_org_idx` exists to serve.
func (rt *Router) listCasePolicies(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	page, limit, err := simplePage(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	policies, next, err := rt.svc.CasePolicies(r.Context(), scope, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]CasePolicyDTO, 0, len(policies))
	for _, p := range policies {
		out = append(out, casePolicyDTO(p))
	}
	httpx.List(w, r, out, pageOf(next, limit), started)
}

// createCasePolicy serves POST /api/v1/case-policies.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS ANSWERED WITH THE ORIGINAL
// ROW. Without the claim this endpoint would be safe only by accident:
// `case_policy_axes_uniq` refuses a second create for the same pair with a `409`
// that names no id, so a client that never received its response could not learn
// the id of the rule it had already written.
func (rt *Router) createCasePolicy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// §E.3 rejects an unknown query parameter rather than ignoring it.
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The RAW bytes, before Bind consumes the stream: "the same body" is decided by
	// the sha256 of what the caller actually sent, not by a re-encoding of the DTO
	// it parsed into.
	raw, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateCasePolicyRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// There is no path subject to fold in — the natural key is in the body, and the
	// body is already hashed — so this is the untargeted form.
	idem, err := idempotency.IntentFromRequest(r, idempotency.HashRequest(raw))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ⭐ A REPLAY IS STILL A `201`, which is why the second result is discarded here
	// exactly as `commentOnAlert` and `snoozeAlert` discard theirs. The claim's whole
	// promise is that the retry and the original are ONE request; answering the
	// second one differently would make a client's success depend on which attempt
	// reached the server.
	p, _, err := rt.svc.CreateCasePolicy(r.Context(), scope, dto.toCasePolicyDraft(), idem)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, casePolicyDTO(p), started)
}

// updateCasePolicy serves PATCH /api/v1/case-policies/{id}.
//
// Only W is updatable. `namespace` and `alertname` are absent from the request DTO
// rather than merely rejected at runtime: they are the row's identity, and a field
// that cannot be sent cannot be sent by accident.
func (rt *Router) updateCasePolicy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateCasePolicyRequest](w, r)
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

	p, err := rt.svc.UpdateCasePolicy(r.Context(), scope, id, dto.toCasePolicyPatch())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, casePolicyDTO(p), started)
}

// deleteCasePolicy serves DELETE /api/v1/case-policies/{id}.
//
// ⭐ REMOVING THE ROW RESTORES W=0, which is the close-on-resolve behaviour oto had
// before migration 00057 — not a broken state and not a disabled feature. That is
// why this is a hard delete: nothing points at the row, so a tombstone would
// preserve nothing and would have to be excluded from the unique index that does
// the real work.
func (rt *Router) deleteCasePolicy(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.svc.DeleteCasePolicy(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}
