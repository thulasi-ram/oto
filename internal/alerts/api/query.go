package api

import (
	"encoding/base64"
	"encoding/json"
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
	"state", "severity", "cluster", "namespace", "alertname", "source_fingerprint",
	"label[", "matcher",
	"ack", "flapping", "snoozed", "synthetic", "since", "q", "sort", "include",
	"limit", "cursor",
}

// listRollupsParams is the allow-list for `GET /api/v1/alerts/rollups`. It is
// listAlertsParams minus `sort` and `include` — a bucket has no `last_seen_at`
// ordering to choose and no sub-resources to embed — plus the required axis.
var listRollupsParams = []string{
	"group_by",
	"state", "severity", "cluster", "namespace", "alertname", "source_fingerprint",
	"label[", "matcher",
	"ack", "flapping", "snoozed", "synthetic", "since", "q",
	"limit", "cursor",
}

// timelineParams is the allow-list for every event-list endpoint.
var timelineParams = []string{"type", "since", "until", "order", "limit", "cursor"}

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
		State:             p.CSV("state"),
		Severity:          p.CSV("severity"),
		Cluster:           p.CSV("cluster"),
		Namespace:         p.CSV("namespace"),
		AlertName:         p.CSV("alertname"),
		SourceFingerprint: p.CSV("source_fingerprint"),

		Matcher:   p.String("matcher", ""),
		Ack:       p.String("ack", ""),
		Flapping:  p.Bool("flapping"),
		Snoozed:   p.Bool("snoozed"),
		Synthetic: p.Bool("synthetic"),
		Q:         p.String("q", ""),
		Sort:      p.String("sort", service.SortLastSeenDesc),
		Include:   p.CSV("include"),
		Limit:     p.Limit(),
		Cursor:    p.Cursor(),
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

	selector, err := labelSelector(p.All(), q.Matcher)
	if err != nil {
		return alertsListRequest{}, err
	}
	compiled, err := filter.Compile(selectorField(q.Matcher), selector.AST())
	if err != nil {
		return alertsListRequest{}, err
	}

	f, err := alertFilter(q.spec(), compiled)
	if err != nil {
		return alertsListRequest{}, err
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

// labelSelector merges the two spellings of the ADR 0017 label selector.
//
// ⭐ `label[k]=v` is the structured, OpenAPI-expressible form and carries `=`
// and `!=` only — the bracket syntax has nowhere to put an operator. `matcher=`
// carries the FULL Alertmanager syntax, which is the native idiom of this
// audience and the only spelling in which `severity=~"critical|warning"` — ADR
// 0017's own example — can be written down at all. Both compile to the same AST
// and are AND-ed, so a saved view built from chips and a matcher typed by hand
// compose rather than conflict.
func labelSelector(params map[string][]string, matcher string) (filter.Selector, error) {
	out, err := filter.ParseLabelParams(params)
	if err != nil {
		return filter.Selector{}, err
	}
	if strings.TrimSpace(matcher) == "" {
		return out, nil
	}
	parsed, err := filter.ParseSelector("matcher", matcher)
	if err != nil {
		return filter.Selector{}, err
	}
	out.Matchers = append(out.Matchers, parsed.Matchers...)
	if len(out.Matchers) > filter.MaxMatchers {
		return filter.Selector{}, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "matcher", Code: "max_items", Message: "too many label matchers"})
	}
	return out, nil
}

// selectorField names the parameter a compile refusal is reported against, so a
// caller learns which of the two spellings it has to fix.
func selectorField(matcher string) string {
	if strings.TrimSpace(matcher) != "" {
		return "matcher"
	}
	return "label"
}

// filterSpec is the §E.3 filter in the one shape BOTH query DTOs reduce to.
//
// It is a struct rather than a parameter list because the list and the roll-up
// must accept exactly the same dimensions — a roll-up that summarised a different
// set from the list beside it would be two answers to one question — and a
// positional signature is how one of them quietly acquires a filter the other
// does not.
type filterSpec struct {
	States       []string
	Severities   []string
	Namespaces   []string
	Clusters     []string
	AlertNames   []string
	Fingerprints []string
	Ack          string
	Flapping     *bool
	Snoozed      *bool
	Synthetic    *bool
	Since        *time.Time
	Query        string
}

func (q ListAlertsQuery) spec() filterSpec {
	return filterSpec{
		States: q.State, Severities: q.Severity, Namespaces: q.Namespace,
		Clusters: q.Cluster, AlertNames: q.AlertName, Fingerprints: q.SourceFingerprint,
		Ack: q.Ack, Flapping: q.Flapping, Snoozed: q.Snoozed, Synthetic: q.Synthetic,
		Since: q.Since, Query: q.Q,
	}
}

func (q ListRollupsQuery) spec() filterSpec {
	return filterSpec{
		States: q.State, Severities: q.Severity, Namespaces: q.Namespace,
		Clusters: q.Cluster, AlertNames: q.AlertName, Fingerprints: q.SourceFingerprint,
		Ack: q.Ack, Flapping: q.Flapping, Snoozed: q.Snoozed, Synthetic: q.Synthetic,
		Since: q.Since, Query: q.Q,
	}
}

// alertFilter assembles the compiled §E.3 filter shared by the list and the
// roll-up. It exists exactly once so the two can never diverge into two answers
// to one question.
func alertFilter(in filterSpec, compiled filter.Compiled) (domain.AlertFilter, error) {
	parsed := make([]domain.State, 0, len(in.States))
	for _, s := range in.States {
		st, err := domain.NewState(s)
		if err != nil {
			return domain.AlertFilter{}, err
		}
		parsed = append(parsed, st)
	}
	// The C.3 charset is proven by the domain constructor, which is where the
	// pattern lives; the values themselves travel as strings because that is what
	// the column holds.
	for _, fp := range in.Fingerprints {
		if _, err := domain.NewSourceFingerprint(fp); err != nil {
			return domain.AlertFilter{}, errs.Validation("validation_failed",
				"1 field failed validation.", errs.Violation{
					Field: "source_fingerprint", Code: "pattern",
					Message: "must be 16 lowercase hex characters",
				})
		}
	}

	f := domain.AlertFilter{
		States:       parsed,
		Severities:   in.Severities,
		Namespaces:   in.Namespaces,
		ClusterKeys:  in.Clusters,
		AlertNames:   in.AlertNames,
		Fingerprints: in.Fingerprints,
		Flapping:     in.Flapping,
		// nil means INCLUDE BOTH and nil is the default: the list NEVER hides a
		// snoozed alert (§B.8.6), because hiding one is how an incident is lost.
		Snoozed: in.Snoozed,
		// nil means EXCLUDE, and nil is the default — the OPPOSITE of Snoozed one
		// line up. A drill's alert is not history; letting one into the default
		// list would put oto's own plumbing into the customer's estate.
		Synthetic:  in.Synthetic,
		LabelsAll:  compiled.LabelsAll,
		LabelsAny:  compiled.LabelsAny,
		LabelsNone: compiled.LabelsNone,
		Since:      in.Since,
		Query:      in.Query,
	}
	if in.Ack != "" {
		a, err := domain.NewAckState(in.Ack)
		if err != nil {
			return domain.AlertFilter{}, err
		}
		f.AckState = &a
	}
	return f, nil
}

// rollupsRequest is one parsed, validated `listAlertRollups` call.
type rollupsRequest struct {
	Query   ListRollupsQuery
	Service service.RollupQuery
	Hash    string
}

// parseListRollups compiles `GET /api/v1/alerts/rollups`.
//
// The keyset position is the BUCKET KEY, not a timestamp: buckets are ordered by
// key, and a key appears exactly once per result set, so `key > cursor` is a
// total order and paging over it can neither skip nor repeat a bucket.
func parseListRollups(r *http.Request) (rollupsRequest, error) {
	p := httpx.NewParams(r, listRollupsParams...)
	if err := p.Err(); err != nil {
		return rollupsRequest{}, err
	}

	q := ListRollupsQuery{
		GroupBy:           p.String("group_by", ""),
		State:             p.CSV("state"),
		Severity:          p.CSV("severity"),
		Cluster:           p.CSV("cluster"),
		Namespace:         p.CSV("namespace"),
		AlertName:         p.CSV("alertname"),
		SourceFingerprint: p.CSV("source_fingerprint"),
		Matcher:           p.String("matcher", ""),
		Ack:               p.String("ack", ""),
		Flapping:          p.Bool("flapping"),
		Snoozed:           p.Bool("snoozed"),
		Synthetic:         p.Bool("synthetic"),
		Q:                 p.String("q", ""),
		Limit:             p.Limit(),
		Cursor:            p.Cursor(),
	}
	if p.Has("since") {
		since := p.Time("since")
		q.Since = &since
	}
	if err := p.Err(); err != nil {
		return rollupsRequest{}, err
	}
	if _, err := httpx.BindEmpty(q); err != nil {
		return rollupsRequest{}, err
	}

	by, err := domain.NewRollupKey(q.GroupBy)
	if err != nil {
		return rollupsRequest{}, err
	}

	selector, err := labelSelector(p.All(), q.Matcher)
	if err != nil {
		return rollupsRequest{}, err
	}
	compiled, err := filter.Compile(selectorField(q.Matcher), selector.AST())
	if err != nil {
		return rollupsRequest{}, err
	}

	f, err := alertFilter(q.spec(), compiled)
	if err != nil {
		return rollupsRequest{}, err
	}
	hash := rollupFilterHash(q, selector)
	f.FilterHash = hash

	after, err := decodeKeyCursor(q.Cursor, hash)
	if err != nil {
		return rollupsRequest{}, err
	}

	return rollupsRequest{
		Query: q,
		Hash:  hash,
		Service: service.RollupQuery{
			Filter: f,
			By:     by,
			After:  after,
			Limit:  q.Limit,
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
	return httpx.FilterHash(append(commonFilterParts(q.spec(), sel), "sort="+q.Sort)...)
}

// rollupFilterHash binds a roll-up cursor to its filter AND to its axis.
//
// The axis is part of the hash because changing it changes the bucket keys
// themselves: a cursor holding the alertname `KubePodCrashLooping` means nothing
// once the caller regroups by namespace, and serving a page from it would silently
// skip every namespace sorting before that string.
func rollupFilterHash(q ListRollupsQuery, sel filter.Selector) string {
	return httpx.FilterHash(append(commonFilterParts(q.spec(), sel), "group_by="+q.GroupBy)...)
}

// commonFilterParts is every dimension that changes the RESULT SET. `limit` and
// `cursor` deliberately do not contribute — paging is not a filter change.
//
// ⛔ A dimension missing from here is a cursor that survives a filter change it
// should not have survived, and a page served from the middle of a list that no
// longer exists. Taking the whole filterSpec is what makes adding one to the
// query and forgetting it here impossible.
func commonFilterParts(in filterSpec, sel filter.Selector) []string {
	parts := []string{
		"state=" + joinSorted(in.States),
		"severity=" + joinSorted(in.Severities),
		"cluster=" + joinSorted(in.Clusters),
		"namespace=" + joinSorted(in.Namespaces),
		"alertname=" + joinSorted(in.AlertNames),
		"source_fingerprint=" + joinSorted(in.Fingerprints),
		"ack=" + in.Ack,
		"q=" + in.Query,
		"label=" + sel.Canonical(),
	}
	if in.Flapping != nil {
		parts = append(parts, "flapping="+strconv.FormatBool(*in.Flapping))
	}
	if in.Snoozed != nil {
		parts = append(parts, "snoozed="+strconv.FormatBool(*in.Snoozed))
	}
	if in.Synthetic != nil {
		parts = append(parts, "synthetic="+strconv.FormatBool(*in.Synthetic))
	}
	if in.Since != nil {
		parts = append(parts, "since="+in.Since.UTC().Format(time.RFC3339Nano))
	}
	return parts
}

func joinSorted(in []string) string {
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// keyCursorPayload is the wire form of a TEXT keyset position, for the one list
// in this package ordered by a string rather than by (timestamp, uuid).
//
// It mirrors the platform cursor exactly — base64url of a small JSON object
// carrying the position and the filter hash — because the honesty property is
// the same one: a cursor minted under one filter and replayed against another
// describes a position in a sequence that no longer exists, and without `h` the
// server would serve a page from the middle of the wrong list with nothing
// looking wrong.
type keyCursorPayload struct {
	K string `json:"k"`
	H string `json:"h"`
}

// encodeKeyCursor renders a bucket-key position. An empty key encodes as "" —
// there is no cursor for "the first page".
func encodeKeyCursor(key, hash string) string {
	if key == "" {
		return ""
	}
	b, err := json.Marshal(keyCursorPayload{K: key, H: hash})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeKeyCursor parses a bucket-key position and binds it to filterHash.
func decodeKeyCursor(token, filterHash string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return "", errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	var p keyCursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	if p.H != filterHash {
		return "", errs.Malformed("cursor_filter_mismatch",
			"this cursor was issued for a different filter; restart from the first page")
	}
	return p.K, nil
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
		Type:   p.CSV("type"),
		Order:  p.Enum("order", defaultOrder, "asc", "desc"),
		Limit:  p.Limit(),
		Cursor: p.Cursor(),
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
