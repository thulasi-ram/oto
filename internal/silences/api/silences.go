package api

// THE SILENCES HTTP SURFACE.
//
// ⛔ It is READ-ONLY. There is no POST, no PATCH and no DELETE here, because oto
// has no write path into your cluster (SPEC R3): a silence created in oto would
// not suppress anything in Alertmanager, and an endpoint that looked like it did
// would be the most dangerous kind of lie this product could tell. Each row
// carries a deep link into the Alertmanager UI, which is v1's only silence
// affordance.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/silences/domain"
	"github.com/thulasiram/oto/internal/silences/service"
)

// SilenceService is the port this layer declares for itself, satisfied by
// *service.Service.
type SilenceService interface {
	List(ctx context.Context, s db.TenantScope, f domain.Filter, p db.Keyset) (service.ListResult, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.Detail, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ SilenceService = (*service.Service)(nil)

// Router serves the Silences tag.
type Router struct {
	svc SilenceService
	clk clock.Clock
	// alertmanagerURL is the base URL of the Alertmanager UI, used to build the
	// per-silence deep link. Empty when the deployment has not configured one, in
	// which case the link is null rather than a guess.
	alertmanagerURL string
}

// NewRouter builds the silences HTTP surface.
func NewRouter(svc SilenceService, alertmanagerURL string, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk, alertmanagerURL: strings.TrimRight(alertmanagerURL, "/")}
}

// Register mounts every route this package owns onto r, already rooted at
// /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Get("/silences", rt.listSilences)
	r.Get("/silences/{id}", rt.getSilence)
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

var listParams = []string{"state", "source_id", "created_by", "q", "limit", "cursor"}

// listSilences is `GET /api/v1/silences`.
func (rt *Router) listSilences(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, listParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q := ListSilencesQuery{
		State:     p.CSV("state"),
		SourceID:  p.String("source_id", ""),
		CreatedBy: p.String("created_by", ""),
		Q:         p.String("q", ""),
		Limit:     p.Limit(),
		Cursor:    p.Cursor(),
	}
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	f := domain.Filter{
		States:     q.State,
		CreatedBy:  q.CreatedBy,
		Query:      q.Q,
		FilterHash: silenceFilterHash(q),
	}
	if q.SourceID != "" {
		id, err := uuid.Parse(q.SourceID)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
		f.SourceID = id
	}

	cursor, err := httpx.DecodeCursor(q.Cursor, f.FilterHash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.List(r.Context(), scope, f, httpx.Keyset(q.Limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]SilenceDTO, 0, len(res.Silences))
	for _, s := range res.Silences {
		out = append(out, rt.silenceDTO(s))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, q.Limit), started)
}

// getSilence is `GET /api/v1/silences/{id}` — one mirrored silence with the
// alerts oto believes it currently covers: the answer to "why is this alert
// quiet, and when does it come back?"
func (rt *Router) getSilence(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	_, scope, err := authn.Scope(r.Context())
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

	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto := SilenceDetailDTO{
		SilenceDTO:    rt.silenceDTO(detail.Silence),
		MatchedAlerts: make([]AlertRefDTO, 0, len(detail.Matched)),
		MatchedCount:  int32(detail.MatchedCount),
	}
	for _, a := range detail.Matched {
		dto.MatchedAlerts = append(dto.MatchedAlerts, alertRefDTO(a))
	}
	httpx.Data(w, r, http.StatusOK, dto, started)
}

// silenceDTO maps a mirrored silence onto the wire.
func (rt *Router) silenceDTO(s domain.Silence) SilenceDTO {
	matchers := make([]SilenceMatcherDTO, 0, len(s.Matchers()))
	for _, m := range s.Matchers() {
		matchers = append(matchers, SilenceMatcherDTO{
			Name:    m.Name,
			Value:   m.Value,
			IsRegex: m.IsRegex,
			IsEqual: m.IsEqual,
			Op:      m.Op(),
		})
	}
	return SilenceDTO{
		ID:              s.ID(),
		SourceID:        s.SourceID(),
		SourceSilenceID: s.SourceSilenceID(),
		Matchers:        matchers,
		StartsAt:        s.StartsAt(),
		EndsAt:          s.EndsAt(),
		CreatedBy:       s.CreatedBy(),
		Comment:         s.Comment(),
		Annotations:     s.Annotations(),
		State:           s.State().String(),
		SourceUpdatedAt: timePtr(s.SourceUpdatedAt()),
		MirroredAt:      s.MirroredAt(),
		AlertmanagerURL: rt.deepLink(s),
	}
}

// deepLink builds the Alertmanager UI link for one silence.
//
// It is null when no Alertmanager base URL is configured. A guessed link is worse
// than none: an operator who clicks it during an incident and lands on a 404 has
// lost the one affordance v1 offers.
func (rt *Router) deepLink(s domain.Silence) *string {
	if rt.alertmanagerURL == "" {
		return nil
	}
	url := rt.alertmanagerURL + "/#/silences/" + s.SourceSilenceID()
	return &url
}

func alertRefDTO(a alertdomain.Alert) AlertRefDTO {
	return AlertRefDTO{
		ID:         a.ID(),
		AlertKey:   a.Key().String(),
		AlertName:  a.AlertName(),
		Severity:   strPtr(a.Severity().String()),
		Namespace:  strPtr(a.Namespace()),
		ClusterKey: a.ClusterKey().String(),
		State:      a.State().String(),
		AckState:   a.AckState().String(),
	}
}

// silenceFilterHash binds a cursor to the filter it was minted under.
func silenceFilterHash(q ListSilencesQuery) string {
	states := append([]string(nil), q.State...)
	sort.Strings(states)
	return httpx.FilterHash(
		"state="+strings.Join(states, ","),
		"source_id="+q.SourceID,
		"created_by="+q.CreatedBy,
		"q="+q.Q,
	)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
