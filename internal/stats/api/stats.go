package api

// THE STATS HTTP SURFACE.
//
// ⛔ THERE IS NO PER-PERSON DATA HERE AND NO WAY TO ASK FOR IT (SPEC R8,
// SCOPE-BOUNDARY). Response-time leaderboards and per-individual aggregates are
// not merely omitted from these endpoints; they are unrepresentable in the
// underlying rollup, which carries no user column at all. A feature that does not
// exist cannot be misused.
//
// The vocabulary is binding: the summed time an alert spent firing is FIRING
// DURATION.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/stats/domain"
	"github.com/thulasiram/oto/internal/stats/service"
)

// StatsService is the port this layer declares for itself, satisfied by
// *service.Service.
type StatsService interface {
	Overview(ctx context.Context, s db.TenantScope, since, until time.Time, clusters []string) (service.OverviewResult, error)
	AlertQuality(ctx context.Context, s db.TenantScope, q service.QualityQuery) (service.QualityResult, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ StatsService = (*service.Service)(nil)

// Router serves the Stats tag.
type Router struct {
	svc StatsService
	clk clock.Clock
}

// NewRouter builds the stats HTTP surface.
func NewRouter(svc StatsService, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk}
}

// Register mounts every route this package owns onto r, already rooted at
// /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Route("/stats", func(r chi.Router) {
		r.Get("/overview", rt.getStatsOverview)
		r.Get("/alert-quality", rt.getAlertQualityStats)
	})
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

var overviewParams = []string{"since", "until", "cluster"}

var qualityParams = []string{"since", "until", "cluster", "alertname", "sort", "limit", "cursor"}

// getStatsOverview is `GET /api/v1/stats/overview`.
//
// `resolved` and `expired` are returned as separate counts and are never summed
// into a single "closed" bucket, because conflating them is exactly the lie oto
// exists to prevent.
func (rt *Router) getStatsOverview(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, overviewParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q := OverviewQuery{Cluster: p.CSV("cluster")}
	if p.Has("since") {
		v := p.Time("since")
		q.Since = &v
	}
	if p.Has("until") {
		v := p.Time("until")
		q.Until = &v
	}
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Overview(r.Context(), scope, deref(q.Since), deref(q.Until), q.Cluster)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, overviewDTO(res), started)
}

// getAlertQualityStats is `GET /api/v1/stats/alert-quality`.
//
// Per alertname per cluster: occurrences, notifications sent, acknowledgement
// rate and flap score. This is the row that answers *"this rule fired 47 times
// this month, cost 47 notifications, and was acknowledged 0 times"* — which does
// more good than any enrichment, because the best alert is the one that no longer
// exists.
func (rt *Router) getAlertQualityStats(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, qualityParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q := AlertQualityQuery{
		Cluster:   p.CSV("cluster"),
		AlertName: p.CSV("alertname"),
		Sort:      p.String("sort", domain.SortOccurrencesDesc.String()),
		Limit:     p.Limit(),
		Cursor:    p.Cursor(),
	}
	if p.Has("since") {
		v := p.Time("since")
		q.Since = &v
	}
	if p.Has("until") {
		v := p.Time("until")
		q.Until = &v
	}
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	sortKey, err := domain.NewSort(q.Sort)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	hash := qualityFilterHash(q)
	pos, err := decodePosition(q.Cursor, hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.AlertQuality(r.Context(), scope, service.QualityQuery{
		Since:      deref(q.Since),
		Until:      deref(q.Until),
		Clusters:   q.Cluster,
		AlertNames: q.AlertName,
		Sort:       sortKey,
		Limit:      q.Limit,
		AfterValue: pos.Value,
		AfterKey:   pos.Key,
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]AlertQualityDTO, 0, len(res.Rows))
	next := position{Hash: hash}
	for _, row := range res.Rows {
		out = append(out, qualityDTO(row.Quality))
		v := row.SortValue
		next.Value, next.Key = &v, row.KeysetKey
	}

	page := httpx.Page{Limit: q.Limit, HasMore: res.HasMore}
	if res.HasMore {
		page.NextCursor = encodePosition(next)
	}
	httpx.List(w, r, out, page, started)
}

// ------------------------------------------------------------------- cursor

// position is the keyset position of the hygiene report.
//
// It is stats-local rather than the shared time-keyed cursor, because this list
// is ordered by a NUMBER — occurrences, notifications, an ack rate — and a
// time-shaped cursor cannot name a position in it. `h` binds it to the filter set
// exactly as the shared cursor does: a cursor minted under one window replayed
// against another describes a position in a sequence that no longer exists.
type position struct {
	Value *float64 `json:"v,omitempty"`
	Key   string   `json:"k,omitempty"`
	Hash  string   `json:"h"`
}

func encodePosition(p position) string {
	if p.Value == nil {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePosition(token, hash string) (position, error) {
	if token == "" {
		return position{Hash: hash}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return position{}, errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	var p position
	if err := json.Unmarshal(raw, &p); err != nil {
		return position{}, errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	if p.Hash != hash {
		return position{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was issued for a different filter; restart from the first page")
	}
	return p, nil
}

func qualityFilterHash(q AlertQualityQuery) string {
	clusters := append([]string(nil), q.Cluster...)
	names := append([]string(nil), q.AlertName...)
	sort.Strings(clusters)
	sort.Strings(names)

	parts := []string{
		"cluster=" + strings.Join(clusters, ","),
		"alertname=" + strings.Join(names, ","),
		"sort=" + q.Sort,
	}
	if q.Since != nil {
		parts = append(parts, "since="+q.Since.UTC().Format(time.RFC3339Nano))
	}
	if q.Until != nil {
		parts = append(parts, "until="+q.Until.UTC().Format(time.RFC3339Nano))
	}
	return httpx.FilterHash(parts...)
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
