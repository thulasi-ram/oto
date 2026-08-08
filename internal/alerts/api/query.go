package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/alerts/api/filter"
	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listAlertsParams is the allow-list for `GET /api/v1/alerts`.
//
// ⭐ Anything outside it is `400 unknown_parameter` (SPEC §E.3). A typo'd
// `?serverity=critical` that is silently ignored returns a page of the WRONG
// alerts and looks right, which is how a dashboard starts quietly lying about
// what it shows. `label[` is a prefix permission, which is how the `label[team]`
// and `label[!tier]` families are admitted without enumerating every label an
// operator might own.
var listAlertsParams = []string{
	"state", "severity", "cluster", "namespace", "alertname", "label[",
	"ack", "flapping", "since", "q", "sort", "include",
	"limit", "cursor", "since_seq",
}

// timelineParams is the allow-list for every event-list endpoint.
var timelineParams = []string{"type", "since", "until", "order", "limit", "cursor", "since_seq"}

// labelParams is the allow-list for the two Discovery endpoints.
var labelParams = []string{"q", "limit"}

// includeSet is the bounded whitelist of embeddable sub-resources.
type includeSet struct {
	CurrentOccurrence bool
	Enrichments       bool
	Rule              bool
}

// alertsListRequest is one parsed, validated `listAlerts` call.
type alertsListRequest struct {
	Query   ListAlertsQuery
	Include includeSet
	Service service.ListQuery
}

// parseListAlerts turns a request into a compiled service query.
//
// Every filter the contract exposes is honoured here, and the label selector goes
// through the ADR 0017 AST rather than being pieced together ad hoc: matchers
// parse, lift into the AST, and compile onto the three index-backed containment
// shapes — or are refused with a message naming the matcher.
func parseListAlerts(r *http.Request) (alertsListRequest, error) {
	p := httpx.NewParams(r, listAlertsParams...)
	if err := p.Err(); err != nil {
		return alertsListRequest{}, err
	}

	q := ListAlertsQuery{
		State:     p.CSV("state"),
		Severity:  p.CSV("severity"),
		Cluster:   p.CSV("cluster"),
		Namespace: p.CSV("namespace"),
		AlertName: p.CSV("alertname"),
		Ack:       p.String("ack", ""),
		Flapping:  p.Bool("flapping"),
		Q:         p.String("q", ""),
		Sort:      p.String("sort", service.SortLastSeenDesc),
		Include:   p.CSV("include"),
		Limit:     p.Limit(),
		Cursor:    p.Cursor(),
		SinceSeq:  int64(p.Int("since_seq", 0)),
	}
	if p.Has("since") {
		since := p.Time("since")
		q.Since = &since
	}
	if err := p.Err(); err != nil {
		return alertsListRequest{}, err
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		return alertsListRequest{}, err
	}

	selector, err := filter.ParseLabelParams(p.All())
	if err != nil {
		return alertsListRequest{}, err
	}
	compiled, err := filter.Compile("label", selector.AST())
	if err != nil {
		return alertsListRequest{}, err
	}

	states := make([]domain.State, 0, len(q.State))
	for _, s := range q.State {
		st, err := domain.NewState(s)
		if err != nil {
			return alertsListRequest{}, err
		}
		states = append(states, st)
	}

	f := domain.AlertFilter{
		States:      states,
		Severities:  q.Severity,
		Namespaces:  q.Namespace,
		ClusterKeys: q.Cluster,
		AlertNames:  q.AlertName,
		Flapping:    q.Flapping,
		LabelsAll:   compiled.LabelsAll,
		LabelsAny:   compiled.LabelsAny,
		LabelsNone:  compiled.LabelsNone,
		Since:       q.Since,
		Query:       q.Q,
	}
	if q.Ack != "" {
		ack, err := domain.NewAckState(q.Ack)
		if err != nil {
			return alertsListRequest{}, err
		}
		f.AckState = &ack
	}
	f.FilterHash = alertFilterHash(q, selector)

	cursor, err := httpx.DecodeCursor(q.Cursor, f.FilterHash)
	if err != nil {
		return alertsListRequest{}, err
	}

	inc := includeSet{}
	for _, v := range q.Include {
		switch v {
		case "current_occurrence":
			inc.CurrentOccurrence = true
		case "enrichments":
			inc.Enrichments = true
		case "rule":
			inc.Rule = true
		}
	}

	return alertsListRequest{
		Query:   q,
		Include: inc,
		Service: service.ListQuery{
			Filter: f,
			Sort:   q.Sort,
			Page:   httpx.Keyset(q.Limit, cursor),
		},
	}, nil
}

// alertFilterHash binds a cursor to the filter it was minted under (SPEC §E.1).
//
// Every dimension that changes the RESULT SET contributes; `limit` and `cursor`
// deliberately do not, because paging is not a filter change. The parts are
// sorted inside httpx.FilterHash, so a caller reordering its own query string is
// never told its own cursor is invalid.
func alertFilterHash(q ListAlertsQuery, sel filter.Selector) string {
	parts := []string{
		"state=" + joinSorted(q.State),
		"severity=" + joinSorted(q.Severity),
		"cluster=" + joinSorted(q.Cluster),
		"namespace=" + joinSorted(q.Namespace),
		"alertname=" + joinSorted(q.AlertName),
		"ack=" + q.Ack,
		"q=" + q.Q,
		"sort=" + q.Sort,
		"label=" + sel.Canonical(),
	}
	if q.Flapping != nil {
		parts = append(parts, "flapping="+strconv.FormatBool(*q.Flapping))
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

// timelineRequest is one parsed event-list call.
type timelineRequest struct {
	Query  TimelineQuery
	Window db.TimeWindow
	Page   db.Keyset
	Types  map[string]bool
	Hash   string
}

// parseTimeline compiles an event-list query.
//
// `defaultOrder` differs by endpoint — the alert timeline defaults to `desc`,
// the occurrence and group timelines to `asc` — because they answer different
// questions: "what has this rule ever done" reads newest first, "what happened
// during this outage" reads in the order it happened.
func parseTimeline(r *http.Request, defaultOrder string) (timelineRequest, error) {
	p := httpx.NewParams(r, timelineParams...)
	if err := p.Err(); err != nil {
		return timelineRequest{}, err
	}

	q := TimelineQuery{
		Type:     p.CSV("type"),
		Order:    p.Enum("order", defaultOrder, "asc", "desc"),
		Limit:    p.Limit(),
		Cursor:   p.Cursor(),
		SinceSeq: int64(p.Int("since_seq", 0)),
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
		return timelineRequest{}, err
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		return timelineRequest{}, err
	}

	types := map[string]bool{}
	for _, t := range q.Type {
		if _, err := domain.NewEventType(t); err != nil {
			return timelineRequest{}, errs.Validation("validation_failed",
				"1 field failed validation.", errs.Violation{
					Field: "type", Code: "enum", Message: "unknown event type: " + t,
				})
		}
		types[t] = true
	}

	hash := httpx.FilterHash("type="+joinSorted(q.Type), "order="+q.Order)
	cursor, err := httpx.DecodeCursor(q.Cursor, hash)
	if err != nil {
		return timelineRequest{}, err
	}

	var window db.TimeWindow
	if q.Since != nil {
		window.From = q.Since.UTC()
	}
	if q.Until != nil {
		window.To = q.Until.UTC()
	}
	if !window.From.IsZero() && !window.To.IsZero() && window.To.Before(window.From) {
		return timelineRequest{}, errs.Validation("validation_failed",
			"1 field failed validation.", errs.Violation{
				Field: "until", Code: "field_order", Message: "until must be >= since",
			})
	}

	return timelineRequest{
		Query:  q,
		Window: window,
		Page:   httpx.Keyset(q.Limit, cursor),
		Types:  types,
		Hash:   hash,
	}, nil
}

// parseLabelQuery compiles the two Discovery queries.
func parseLabelQuery(r *http.Request) (LabelQuery, error) {
	p := httpx.NewParams(r, labelParams...)
	if err := p.Err(); err != nil {
		return LabelQuery{}, err
	}
	q := LabelQuery{Q: p.String("q", ""), Limit: p.Int("limit", 50)}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if err := p.Err(); err != nil {
		return LabelQuery{}, err
	}
	return httpx.BindEmpty(q)
}

// filterEvents applies the `type` filter in the API layer.
//
// The repository read is bounded by `(subject, recorded_at)`, which is the
// partition-pruning index; type is a cheap post-filter over one already-bounded
// page rather than a second predicate that would defeat it.
func filterEvents(evs []domain.Event, types map[string]bool) []domain.Event {
	if len(types) == 0 {
		return evs
	}
	out := make([]domain.Event, 0, len(evs))
	for _, e := range evs {
		if types[e.Type().String()] {
			out = append(out, e)
		}
	}
	return out
}

// orderEvents renders the requested direction.
//
// Ordering is always by `recorded_at` — oto's own clock — so a skewed upstream
// can never make a timeline render out of order. The UI displays `occurred_at`;
// it never sorts by it.
func orderEvents(evs []domain.Event, order string) []domain.Event {
	if order != "asc" {
		return evs
	}
	out := append([]domain.Event(nil), evs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RecordedAt().Equal(out[j].RecordedAt()) {
			return out[i].ID().String() < out[j].ID().String()
		}
		return out[i].RecordedAt().Before(out[j].RecordedAt())
	})
	return out
}
