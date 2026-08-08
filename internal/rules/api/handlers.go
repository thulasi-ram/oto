package api

import (
	"net/http"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/rules/domain"
)

// listRuleSnapshots is `GET /api/v1/rule-snapshots` — the version history for one
// RuleKey, newest first.
//
// The versions are the DISTINCT TEXTS the rule has had, not the fires: a rule
// that fired ten thousand times unchanged has exactly one version, and a rule
// whose threshold was doubled last Tuesday has two.
func (rt *Router) listRuleSnapshots(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q, err := parseListSnapshots(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	key := domain.Key{
		SourceID: q.SourceID,
		File:     q.RuleFile,
		Group:    q.RuleGroup,
		Name:     q.RuleName,
	}

	// ⭐ A REAL KEYSET PAGE, not the head of a capped in-memory history. This
	// endpoint used to be served from `History()`: the first `limit` of at most
	// 200 versions, `has_more` set, and `next_cursor` structurally always null —
	// a list whose second page could not be asked for.
	res, err := rt.svc.ListSnapshots(r.Context(), scope, key, q.Page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]RuleSnapshotDTO, 0, len(res.Snapshots))
	for _, v := range res.Snapshots {
		out = append(out, snapshotDTO(v))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, q.Limit), started)
}

// getRuleSnapshot is `GET /api/v1/rule-snapshots/{id}`.
//
// Snapshots are immutable and deduplicated by content, so two occurrences that
// fired under an identical definition point at the same row.
func (rt *Router) getRuleSnapshot(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	snap, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, snapshotDTO(snap), started)
}

// getAlertRuleHistory is `GET /api/v1/alerts/{id}/rule` — **the differentiator**.
//
// It returns the rule as it was WHEN THIS EPISODE FIRED, every version oto has
// ever captured for the same RuleKey, and a structured diff when the definition
// changed between episodes. Provenance is always shown and never guessed:
// `origin` says where the definition came from, and an `ambiguous`
// `match_confidence` is surfaced rather than resolved arbitrarily.
func (rt *Router) getAlertRuleHistory(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, "limit")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Int("limit", 50)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := httpx.BindEmpty(HistoryQuery{Limit: limit}); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	bound, err := rt.boundSnapshot(r, scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := RuleHistoryDTO{RuleKey: keyDTO(bound.Key), Versions: []RuleSnapshotDTO{}}
	if bound.ID != "" {
		dto := snapshotDTO(bound)
		out.Current = &dto
	}

	history, err := rt.svc.History(r.Context(), scope, bound.Key)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	for _, v := range newestFirst(history) {
		if len(out.Versions) == limit {
			break
		}
		out.Versions = append(out.Versions, snapshotDTO(v))
	}

	if bound.Fingerprint != "" {
		if diff, changed, err := rt.svc.DiffSince(r.Context(), scope, bound.Key, bound.Fingerprint); err == nil && changed {
			c := changeDTO(diff)
			out.Change = &c
		}
	}

	httpx.Data(w, r, http.StatusOK, out, started)
}

// getOccurrenceRule is `GET /api/v1/occurrences/{id}/rule`.
//
// A `404` here means no snapshot could be captured for this episode at all — no
// Prometheus URL configured and no usable `generatorURL`. A snapshot whose
// `origin` is `unavailable` is a DIFFERENT fact: the capture was attempted and
// honestly recorded as empty, and it is returned with a 200.
func (rt *Router) getOccurrenceRule(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if rt.alerts == nil {
		httpx.WriteProblem(w, r, notFound("rule snapshot"))
		return
	}

	occ, err := rt.alerts.GetOccurrence(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	snapID := occ.RuleSnapshotID()
	if snapID == uuid.Nil {
		httpx.WriteProblem(w, r, notFound("rule snapshot"))
		return
	}

	snap, err := rt.svc.Get(r.Context(), scope, snapID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, snapshotDTO(snap), started)
}

// boundSnapshot resolves the snapshot bound to an alert's current episode.
//
// It is the CURRENT occurrence's snapshot and never the newest one upstream: the
// whole point is what the rule said when this alert fired.
func (rt *Router) boundSnapshot(
	r *http.Request, scope db.TenantScope, alertID uuid.UUID,
) (domain.Snapshot, error) {
	if rt.alerts == nil {
		return domain.Snapshot{}, notFound("alert")
	}
	detail, err := rt.alerts.Get(r.Context(), scope, alertID)
	if err != nil {
		return domain.Snapshot{}, err
	}

	occ := detail.CurrentOccurrence
	if occ == nil {
		occ = detail.LatestOccurrence
	}
	if occ == nil || occ.RuleSnapshotID() == uuid.Nil {
		// No snapshot was ever bound. The key is still answerable from the
		// alert itself, so the caller gets an empty history rather than a 404 —
		// "we have never captured this rule" is a fact worth returning.
		return domain.Snapshot{Key: keyOf(detail.Alert)}, nil
	}

	snap, err := rt.svc.Get(r.Context(), scope, occ.RuleSnapshotID())
	if err != nil {
		return domain.Snapshot{Key: keyOf(detail.Alert)}, nil //nolint:nilerr // a missing snapshot is an empty history, not an error
	}
	return snap, nil
}

// keyOf derives the RuleKey oto can name from the alert alone: the alertname is
// the rule name, and the source is unknown at this layer, which is exactly why
// `match_confidence` exists.
func keyOf(a alertdomain.Alert) domain.Key {
	return domain.Key{Name: a.AlertName()}
}

// newestFirst reverses the history, which the domain keeps oldest-first, into the
// order every endpoint in the contract returns.
func newestFirst(h domain.History) []domain.Snapshot {
	out := make([]domain.Snapshot, 0, len(h.Versions))
	for i := len(h.Versions) - 1; i >= 0; i-- {
		out = append(out, h.Versions[i].Snapshot)
	}
	return out
}
