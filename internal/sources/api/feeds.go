package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// THE TWO FEEDS THAT ANSWER "MY ALERT NEVER APPEARED".
//
// ⭐ WHY THEY LIVE HERE RATHER THAN ON THE INGEST SURFACE. The data belongs to
// the ingestion module and is read through a port into it, but the ROUTE belongs
// beside `/sources/{id}/health`: the ingest router is mounted with no middleware
// at all — no session, no body cap, no timeout, because a webhook must never be
// rate-limited into a 429 — so a UI read cannot live there. It is also where the
// question is asked. An operator staring at a source that has gone quiet is one
// click from the reason.
//
// Both endpoints are keyset pages over PARTITIONED tables whose retention is
// `raw_retention_days` (30 by default since 00036; ADR 0024 Amendment 4 makes that
// 30 a CHOSEN number, and these two feeds are the first of the four reasons for it
// — nothing derives it from the `alert_event_keys` horizon any more). There is
// deliberately NO time-window parameter: the operator arriving here does not know
// when their alert went missing, and a defaulted window would answer "no
// rejections" to a question about one that is older than the default. So
// `raw_retention_days` IS the depth of both feeds, and lowering it empties a
// screen.

// Query-parameter allow-lists. §E.3 is binding: an unknown query parameter is
// REJECTED, because a typo'd `?resaon=undecodable` that is silently ignored
// returns the wrong page and looks right.
var (
	rejectionParams   = []string{"reason", "limit", "cursor"}
	failedBatchParams = []string{"status", "limit", "cursor"}
)

// The closed vocabularies the query parser accepts, mirroring the contract's
// `RejectionReason` and `FailedBatchStatus` — which in turn mirror
// `ingest_rejections_reason_ck` and the two troubled members of
// `ingest_batches_state_ck`.
//
// ⛔ AN UNKNOWN MEMBER IS REFUSED RATHER THAN IGNORED. A closed enum matches no
// row, so serving `?reason=too_many_lables` would return an empty page that
// reads as "nothing was rejected" — which on this screen is the one answer that
// must never be wrong.
var (
	rejectionReasons = []string{
		"too_many_labels", "label_value_too_large", "label_name_too_large",
		"labelset_too_large", "too_many_annotations", "annotation_too_large",
		"annotation_unstorable", "missing_alertname", "invalid_label_name",
		"invalid_label_value", "timestamp_out_of_window", "too_many_alerts",
		"body_too_large", "undecodable", "unknown_source",
	}
	failedBatchStatuses = []string{"failed", "partial"}
)

// listSourceRejections serves GET /api/v1/sources/{id}/rejections.
//
// ⭐ THIS IS THE SCREEN `ingest_rejections` EXISTS FOR. Every per-alert bound
// failure has been durably recorded since the table was created, and until this
// endpoint the only way to read one back was `psql`. SPEC AC-6 and AC-38 promise
// a rejection is visible with a reason; CONTEXT.md §5 makes "delivery failure
// must be visible per alert" a requirement. A 202 for a partially bad payload is
// only honest because this record exists AND can be read.
func (rt *Router) listSourceRejections(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.feedSubject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.feeds != nil, "sources_rejection_feed_unavailable",
		"the ingestion read path is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, rejectionParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	reasons := p.EnumCSV("reason", rejectionReasons...)
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ⛔ PROVE THE SOURCE IS OURS FIRST. The feed itself is org-scoped, so a
	// stranger's id would come back as an empty page — a 200 that says "no
	// rejections" about a source the caller may not see. 404 is the only true
	// statement oto can make about an id it does not own, and it is the same
	// refusal every other `/sources/{id}` endpoint makes.
	if _, err := rt.sources.Get(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	hash := httpx.FilterHash(rejectionFilterParts(id, reasons)...)
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, next, err := rt.feeds.ListRejections(
		r.Context(), scope, id, reasons, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]RejectionDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, rejectionDTO(e))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// listSourceFailedBatches serves GET /api/v1/sources/{id}/failed-batches.
//
// The batch-level half of the same question. A rejection says oto refused one
// element; this says oto took the whole body, answered 202, and then could not
// turn it into alerts. Both have to be readable or a 202 is a promise nobody can
// check.
func (rt *Router) listSourceFailedBatches(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.feedSubject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.feeds != nil, "sources_failed_batch_feed_unavailable",
		"the ingestion read path is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, failedBatchParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	statuses := p.EnumCSV("status", failedBatchStatuses...)
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, err := rt.sources.Get(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	hash := httpx.FilterHash(failedBatchFilterParts(id, statuses)...)
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, next, err := rt.feeds.ListFailedBatches(
		r.Context(), scope, id, statuses, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]FailedBatchDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, failedBatchDTO(b))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// feedSubject resolves the tenant and the path id WITHOUT rejecting the query
// string, which `subject` does and these two endpoints cannot: they carry
// filters of their own, so the allow-list is applied once, in the handler,
// against the whole set each accepts.
func (rt *Router) feedSubject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, err := scopeOf(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}

// rejectionFilterParts renders the filter for the cursor hash.
//
// ⭐ THE SOURCE ID IS PART OF IT. The hash is what makes a cursor honest: one
// minted under `?reason=undecodable` and replayed without it describes a
// position in a sequence that no longer exists, and a cursor from one source's
// feed replayed against another's would page through the wrong list without
// anything looking wrong (§E.1).
func rejectionFilterParts(sourceID uuid.UUID, reasons []string) []string {
	parts := make([]string, 0, len(reasons)+1)
	parts = append(parts, "source="+sourceID.String())
	for _, v := range reasons {
		parts = append(parts, "reason="+v)
	}
	return parts
}

// failedBatchFilterParts is the same binding for the batch feed.
func failedBatchFilterParts(sourceID uuid.UUID, statuses []string) []string {
	parts := make([]string, 0, len(statuses)+1)
	parts = append(parts, "source="+sourceID.String())
	for _, v := range statuses {
		parts = append(parts, "status="+v)
	}
	return parts
}
