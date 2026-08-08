package api

import (
	"errors"
	"net/http"
	"sort"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// timelineRequest is one parsed `getAlertGroupTimeline` call.
type timelineRequest struct {
	Query  TimelineQuery
	Window db.TimeWindow
	Page   db.Keyset
	Types  map[string]bool
}

// parseTimeline compiles the group timeline query.
//
// The order defaults to `asc`: a group timeline answers "what happened to this
// thing", which reads in the order it happened.
//
// The lower time bound is left ZERO when the caller omits one. That is
// deliberate — `grouping/service.Timeline` fills it with the generation's own
// `first_seen_at`, which is both correct and the tightest bound available, and it
// is what keeps the read from scanning thirteen months of event partitions.
func parseTimeline(r *http.Request) (timelineRequest, error) {
	p := httpx.NewParams(r, timelineParams...)
	if err := p.Err(); err != nil {
		return timelineRequest{}, err
	}

	q := TimelineQuery{
		Type:   p.CSV("type"),
		Order:  p.Enum("order", "asc", "asc", "desc"),
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
		if _, err := alertdomain.NewEventType(t); err != nil {
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

	return timelineRequest{Query: q, Window: window, Page: httpx.Keyset(q.Limit, cursor), Types: types}, nil
}

// filterEvents applies the `type` filter over one already-bounded page.
func filterEvents(evs []alertdomain.Event, types map[string]bool) []alertdomain.Event {
	if len(types) == 0 {
		return evs
	}
	out := make([]alertdomain.Event, 0, len(evs))
	for _, e := range evs {
		if types[e.Type().String()] {
			out = append(out, e)
		}
	}
	return out
}

// orderEvents renders the requested direction.
//
// Ordering is always by `recorded_at` — oto's own clock — so the list is stable
// even when an upstream clock is skewed by minutes. The skew is measured and
// badged rather than silently corrected.
func orderEvents(evs []alertdomain.Event, order string) []alertdomain.Event {
	if order != "asc" {
		return evs
	}
	out := append([]alertdomain.Event(nil), evs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RecordedAt().Equal(out[j].RecordedAt()) {
			return out[i].ID().String() < out[j].ID().String()
		}
		return out[i].RecordedAt().Before(out[j].RecordedAt())
	})
	return out
}

// optionalBody decodes a request body the contract marks `required: false`.
func optionalBody[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var zero T
	if r.Body == nil || r.ContentLength == 0 {
		return zero, nil
	}
	dto, err := httpx.Bind[T](w, r)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Code == "empty_body" {
			return zero, nil
		}
		return zero, err
	}
	return dto, nil
}
