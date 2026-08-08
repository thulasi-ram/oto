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
	List(ctx context.Context, s db.TenantScope, q service.ListQuery) (service.ListResult, error)
	Get(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (service.Detail, error)
	Members(ctx context.Context, s db.TenantScope, groupID uuid.UUID, p db.Keyset) (service.MemberResult, error)
	Timeline(ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset) (alerts.TimelineResult, error)
	Acknowledge(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel, note string) (service.FanOutResult, error)
	Comment(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel, body string) (service.CommentResult, error)

	// ⛔ The group snooze is a FAN-OUT OF THE SAME PRIMITIVE, never a new one:
	// one snooze per CURRENTLY-JOINED member alert. Alerts that join later are
	// NOT snoozed — a snooze is never predictive, and a group-level mute would
	// silence alerts nobody has ever seen (§B.8.3).
	Snooze(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel string, until time.Time, note string) (service.FanOutResult, error)
	Unsnooze(ctx context.Context, s db.TenantScope, groupID uuid.UUID, actorKind, actorID, actorLabel, note string) (service.FanOutResult, error)
}

// AlertReader is the cross-domain port that turns a member id into an Alert.
//
// It is declared HERE, by the consumer (CONTEXT.md §5.4), and is satisfied by
// `alerts/service`. `grouping` never reaches into the alerts repository.
type AlertReader interface {
	Get(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (alerts.AlertDetail, error)
}

// DeliveryRollupReader answers "did this generation's notifications land".
//
// ⛔ It is declared HERE, by the consumer (CONTEXT.md §5.4), and satisfied by an
// adapter over `notification/service` in internal/app. `grouping` never imports
// `notification`: a group card must still render in a deployment with
// notifications wired out, and it does — with an all-zero roll-up, which is the
// truth for that deployment.
type DeliveryRollupReader interface {
	DeliveryRollupForGroup(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (DeliveryRollup, error)
}

// DeliveryRollup is the fan-out health of one group generation, in this
// package's own terms.
type DeliveryRollup struct {
	Total   int
	Sent    int
	Failed  int
	Dead    int
	Skipped int
	Pending int

	LastErrorClass string
	LastSentAt     *time.Time
}

// Compile-time proof that the services satisfy the ports this layer declares.
var (
	_ GroupService = (*service.Service)(nil)
	_ AlertReader  = (*alerts.Service)(nil)
)

// Router serves the Groups tag.
type Router struct {
	svc     GroupService
	alerts  AlertReader
	rollups DeliveryRollupReader
	clk     clock.Clock
}

// NewRouter builds the groups HTTP surface. alertReader may be nil, in which case
// member alerts render as an empty page rather than failing the request;
// rollupReader may be nil, in which case the delivery roll-up is all zeroes —
// which is what a deployment with no notification module actually delivers.
func NewRouter(svc GroupService, alertReader AlertReader, rollupReader DeliveryRollupReader, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, alerts: alertReader, rollups: rollupReader, clk: clk}
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
			r.Post("/snooze", rt.snoozeAlertGroup)
			r.Post("/unsnooze", rt.unsnoozeAlertGroup)
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
	"since", "q", "sort", "limit", "cursor",
}

var timelineParams = []string{"type", "since", "until", "order", "limit", "cursor"}

// ⛔ `since_seq` is absent from all three allow-lists. It was accepted,
// validated and then never pushed down — see the note in `alerts/api/helpers.go`
// — and it is removed from the contract with the parsing.
var simplePageParams = []string{"limit", "cursor"}

// groupsListRequest is one parsed, validated `listAlertGroups` call.
type groupsListRequest struct {
	Query   ListGroupsQuery
	Service service.ListQuery
}

// parseListGroups compiles the query into a filter the SERVICE applies, rather
// than into a set of predicates the handler re-applies to a fetched page.
func parseListGroups(r *http.Request) (groupsListRequest, error) {
	p := httpx.NewParams(r, listGroupParams...)
	if err := p.Err(); err != nil {
		return groupsListRequest{}, err
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
		Sort:     p.String("sort", domain.SortLastActivityDesc),
		Limit:    p.Limit(),
		Cursor:   p.Cursor(),
	}
	if p.Has("since") {
		v := p.Time("since")
		q.Since = &v
	}
	if err := p.Err(); err != nil {
		return groupsListRequest{}, err
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		return groupsListRequest{}, err
	}

	f := domain.GroupFilter{
		States:      q.State,
		Severities:  q.Severity,
		ClusterKeys: q.Cluster,
		Receiver:    q.Receiver,
		Storm:       q.Storm,
		Since:       q.Since,
		Query:       q.Q,
	}
	if q.SourceID != "" {
		id, err := uuid.Parse(q.SourceID)
		if err != nil {
			return groupsListRequest{}, errs.Validation("validation_failed",
				"1 field failed validation.", errs.Violation{
					Field: "source_id", Code: "uuid", Message: "must be a UUID",
				})
		}
		f.SourceID = &id
	}
	if q.Ack != "" {
		// `acked` means EVERY member carries a receipt; `unacked` means at least
		// one does not. Ack is orthogonal to state — an acked group is still
		// firing — so this never touches `state` (§B.1).
		fully := q.Ack == "acked"
		f.FullyAcked = &fully
	}
	f.FilterHash = groupFilterHash(q)

	cursor, err := httpx.DecodeCursor(q.Cursor, f.FilterHash)
	if err != nil {
		return groupsListRequest{}, err
	}
	return groupsListRequest{
		Query: q,
		Service: service.ListQuery{
			Filter: f,
			Sort:   q.Sort,
			Page:   httpx.Keyset(q.Limit, cursor),
		},
	}, nil
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

// ⛔ THERE IS NO POST-FILTER IN THIS PACKAGE, and there must never be one again.
//
// Every predicate `listAlertGroups` accepts is compiled into domain.GroupFilter
// above and applied by SQL BEFORE the LIMIT. Removing rows from an
// already-fetched keyset page is not filtering, it is truncation: the page came
// back holding `limit` rows, the predicate deleted most of them, and the caller
// received a short page plus a cursor that had already stepped past everything
// the predicate never got to look at. It is wrong on every page but the last,
// and nothing on the screen says so.
