package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// listClusters serves GET /api/v1/clusters.
func (rt *Router) listClusters(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.clusters != nil, "clusters_unavailable",
		"the cluster registry is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// `since_seq` is not on this operation in the contract, so it is not allowed
	// here: §E.3 rejects an unknown parameter rather than ignoring it.
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
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash("clusters"))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	clusters, next, err := rt.clusters.List(r.Context(), scope, false, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]ClusterDTO, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, clusterDTO(c))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createCluster serves POST /api/v1/clusters.
//
// ⛔ `cluster_key` participates in ALERT IDENTITY (§C.2) and cannot be changed
// afterwards: the same label set in two clusters is two different alerts, which
// is correct because they have different blast radii. Choosing it is therefore a
// decision, and this is the only endpoint that lets anyone make it.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS ANSWERED WITH THE ORIGINAL
// `201`. This endpoint declared the header and read it nowhere, and was safe only
// by accident — `clusters_key_uniq (org_id, cluster_key)` refused a second create
// under the same key with a `409` that named nothing, so a client that never
// received its response could not even learn the id of the cluster it had already
// made. The write now goes through `sources/service`, which takes the claim in
// the same transaction as the insert; see ClusterCreator for why it does not go
// through the repository any more.
func (rt *Router) createCluster(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.clusterW != nil, "clusters_unavailable",
		"the cluster registry is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
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
	dto, err := httpx.Bind[CreateClusterRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	idem, err := rt.idempotencyIntent(r, idempotency.HashRequest(raw))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	c, err := rt.clusterW.CreateCluster(r.Context(), scope, dto.ClusterKey, dto.DisplayName, idem)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, clusterDTO(c), started)
}

// updateCluster serves PATCH /api/v1/clusters/{id}.
//
// Only the display name is updatable. `cluster_key` is deliberately absent from
// the request DTO, not merely rejected at runtime: a field that cannot be sent
// cannot be sent by accident.
func (rt *Router) updateCluster(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.clusters != nil, "clusters_unavailable",
		"the cluster registry is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateClusterRequest](w, r)
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

	c, err := rt.clusters.UpdateDisplayName(r.Context(), scope, id, *dto.DisplayName)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, clusterDTO(c), started)
}
