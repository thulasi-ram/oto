package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// GroupService is the port this layer declares for itself, satisfied by
// *service.Service.
type GroupService interface {
	List(ctx context.Context, s db.TenantScope, states []string, p db.Keyset) (service.ListResult, error)
	Get(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (service.Detail, error)
	Timeline(ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset) (alerts.TimelineResult, error)
	Acknowledge(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel, note string) (service.FanOutResult, error)
	Comment(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel, body string) (service.FanOutResult, error)
}

// AlertReader is the cross-domain port that turns a member id into an Alert.
//
// It is declared HERE, by the consumer (CONTEXT.md §5.4), and is satisfied by
// `alerts/service`. `grouping` never reaches into the alerts repository.
type AlertReader interface {
	Get(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (alerts.AlertDetail, error)
}

// Compile-time proof that the services satisfy the ports this layer declares.
var (
	_ GroupService = (*service.Service)(nil)
	_ AlertReader  = (*alerts.Service)(nil)
)

// Router serves the Groups tag.
type Router struct {
	svc    GroupService
	alerts AlertReader
	clk    clock.Clock
}

// NewRouter builds the groups HTTP surface. alertReader may be nil, in which case
// member alerts render as an empty page rather than failing the request.
func NewRouter(svc GroupService, alertReader AlertReader, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, alerts: alertReader, clk: clk}
}

// Register mounts every route this package owns onto r, already rooted at
// /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Route("/alert-groups", func(r chi.Router) {
		r.Get("/", rt.listAlertGroups)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getAlertGroup)
			r.Get("/alerts", rt.listAlertGroupAlerts)
			r.Get("/timeline", rt.getAlertGroupTimeline)
			r.Post("/ack", rt.ackAlertGroup)
			r.Post("/comments", rt.commentOnAlertGroup)
		})
	})
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes. It is the injected clock and
// never time.Now().
func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// listGroupParams is the allow-list for `GET /api/v1/alert-groups`. Anything
// outside it is `400 unknown_parameter`.
var listGroupParams = []string{
	"state", "severity", "cluster", "source_id", "receiver", "storm", "ack",
	"since", "q", "sort", "limit", "cursor", "since_seq",
}

var timelineParams = []string{"type", "since", "until", "order", "limit", "cursor", "since_seq"}

var simplePageParams = []string{"limit", "cursor", "since_seq"}

func parseListGroups(r *http.Request) (ListGroupsQuery, db.Keyset, error) {
	p := httpx.NewParams(r, listGroupParams...)
	if err := p.Err(); err != nil {
		return ListGroupsQuery{}, db.Keyset{}, err
	}

	q := ListGroupsQuery{
		State:    p.CSV("state"),
		Severity: p.CSV("severity"),
		Cluster:  p.CSV("cluster"),
		SourceID: p.String("source_id", ""),
		Receiver: p.String("receiver", ""),
		Storm:    p.Bool("storm"),
		Ack:      p.String("ack", ""),
		Q:        p.String("q", ""),
		Sort:     p.String("sort", "-last_activity_at"),
		Limit:    p.Limit(),
		Cursor:   p.Cursor(),
		SinceSeq: int64(p.Int("since_seq", 0)),
	}
	if p.Has("since") {
		v := p.Time("since")
		q.Since = &v
	}
	if err := p.Err(); err != nil {
		return ListGroupsQuery{}, db.Keyset{}, err
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		return ListGroupsQuery{}, db.Keyset{}, err
	}

	cursor, err := httpx.DecodeCursor(q.Cursor, groupFilterHash(q))
	if err != nil {
		return ListGroupsQuery{}, db.Keyset{}, err
	}
	return q, httpx.Keyset(q.Limit, cursor), nil
}

// groupFilterHash binds a cursor to the filter it was minted under, so that
// changing a filter chip resets pagination instead of serving a page from the
// middle of a list that no longer exists.
func groupFilterHash(q ListGroupsQuery) string {
	parts := []string{
		"state=" + joinSorted(q.State),
		"severity=" + joinSorted(q.Severity),
		"cluster=" + joinSorted(q.Cluster),
		"source_id=" + q.SourceID,
		"receiver=" + q.Receiver,
		"ack=" + q.Ack,
		"q=" + q.Q,
		"sort=" + q.Sort,
	}
	if q.Storm != nil {
		parts = append(parts, "storm="+strconv.FormatBool(*q.Storm))
	}
	if q.Since != nil {
		parts = append(parts, "since="+q.Since.UTC().Format(time.RFC3339Nano))
	}
	return httpx.FilterHash(parts...)
}

func joinSorted(in []string) string {
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// scopeOf resolves the caller's tenant.
func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

// actorOf resolves the human a group verb is attributed to.
//
// ⛔ A non-human principal is refused: a group ack is forty receipts, and a
// receipt nobody signed is not a receipt.
func actorOf(ctx context.Context) (kind, id, label string, err error) {
	p, err := authn.Require(ctx)
	if err != nil {
		return "", "", "", err
	}
	ak, err := alertdomain.NewActorKind(p.ActorKind())
	if err != nil || !ak.IsHuman() {
		return "", "", "", errs.Forbidden("forbidden", "this action requires a human actor")
	}
	return ak.String(), p.ActorID(), p.ActorLabel(), nil
}

// matchesFilters applies the filters the grouping service does not push down.
//
// ⚠️ These are POST-FILTERS over one already-fetched keyset page. They are honest
// about what they are: the service's List accepts only `state`, so severity,
// cluster, source, receiver, storm, ack, free text and the time bound are applied
// here. It is a service-signature gap, not a design: `grouping/service.List`
// pushes only `state` down to SQL.
func matchesFilters(g domain.Group, q ListGroupsQuery) bool {
	if len(q.Severity) > 0 && !contains(q.Severity, g.Severity()) {
		return false
	}
	if len(q.Cluster) > 0 && !contains(q.Cluster, g.GroupLabels()[clusterLabel]) {
		return false
	}
	if q.SourceID != "" && g.SourceID().String() != q.SourceID {
		return false
	}
	if q.Receiver != "" && g.Receiver() != q.Receiver {
		return false
	}
	if q.Storm != nil && g.StormMode() != *q.Storm {
		return false
	}
	if q.Since != nil && g.LastActivityAt().Before(*q.Since) {
		return false
	}
	if q.Ack != "" {
		c := g.Counts()
		fullyAcked := c.Total > 0 && c.Acked >= c.Total
		if (q.Ack == "acked") != fullyAcked {
			return false
		}
	}
	if q.Q != "" && !matchesText(g, q.Q) {
		return false
	}
	return true
}

func matchesText(g domain.Group, needle string) bool {
	needle = strings.ToLower(needle)
	if strings.Contains(strings.ToLower(g.Title()), needle) {
		return true
	}
	for k, v := range g.GroupLabels() {
		if strings.Contains(strings.ToLower(k), needle) || strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
