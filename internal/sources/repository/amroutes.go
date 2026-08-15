package repository

import (
	"encoding/json"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// The stored shape of `source_health.am_routes` (00037).
//
// It is UNEXPORTED, like `warningJSON` beside it: the wire shape of a jsonb
// column is a repository concern and nothing outside this package may key off
// it. The API renders `domain.RouteResolution` through its own DTO, which is a
// third, separate shape — CONTEXT.md §5.5, and the reason a schema change here
// cannot silently become an API change.
//
// ⛔ NULL AND AN EMPTY DOCUMENT ARE DIFFERENT FACTS. NULL means oto has never
// successfully read this source's configuration, and is rendered as
// `ReceiverUnknown`. A document with `routes: []` means oto DID read it and it
// declares no route — which is a real, if broken, Alertmanager. Collapsing the
// two would make "we have not looked" indistinguishable from "we looked and
// there is nothing", which is the same class of error as backfilling a documented
// default over an unobserved timing (00028).
type routesJSON struct {
	Receiver         string      `json:"receiver,omitempty"`
	Basis            string      `json:"basis"`
	WebhookReceivers []string    `json:"webhook_receivers,omitempty"`
	Dropped          int         `json:"dropped,omitempty"`
	Routes           []routeJSON `json:"routes"`
}

// routeJSON is one delivering route.
//
// Each timing is a PAIR: the value in milliseconds, and the depth of the route
// that stated it. The depth is not decoration — "5m because this route says so"
// and "5m because the root says so" send an operator to two different lines of
// their own file, and only the second survives them editing the child.
// Milliseconds for the same reason as 00028: `group_wait: 500ms` is legal.
type routeJSON struct {
	Receiver           string     `json:"receiver"`
	Path               []stepJSON `json:"path"`
	GroupWaitMS        *int64     `json:"group_wait_ms"`
	GroupWaitFrom      *int       `json:"group_wait_from,omitempty"`
	GroupIntervalMS    *int64     `json:"group_interval_ms"`
	GroupIntervalFrom  *int       `json:"group_interval_from,omitempty"`
	RepeatIntervalMS   *int64     `json:"repeat_interval_ms"`
	RepeatIntervalFrom *int       `json:"repeat_interval_from,omitempty"`
	GroupBy            []string   `json:"group_by,omitempty"`
	GroupByAll         bool       `json:"group_by_all,omitempty"`
	Shadowed           bool       `json:"shadowed,omitempty"`
}

// stepJSON is one route on the path from the top-level route.
type stepJSON struct {
	Matchers   []string `json:"matchers,omitempty"`
	Deprecated bool     `json:"deprecated,omitempty"`
	Continue   bool     `json:"continue,omitempty"`
}

// encodeRoutes renders the resolution for storage, or nil for "not observed".
//
// An empty Basis is the ONLY thing that means unobserved: every successful parse
// produces one of the three bases, even a config whose `receivers:` list is empty
// (that is `no_webhook`, a real finding about a source that cannot be pushing to
// oto). So a nil return here is exactly "no probe has ever read this config".
func encodeRoutes(r domain.RouteResolution) ([]byte, error) {
	if r.Basis == "" {
		return nil, nil
	}

	doc := routesJSON{
		Receiver:         r.Receiver,
		Basis:            string(r.Basis),
		WebhookReceivers: r.WebhookReceivers,
		Dropped:          max(r.Dropped, 0),
		Routes:           make([]routeJSON, 0, len(r.Routes)),
	}
	for _, rt := range r.Routes {
		gw, gwFrom := encodeTiming(rt.GroupWait)
		gi, giFrom := encodeTiming(rt.GroupInterval)
		ri, riFrom := encodeTiming(rt.RepeatInterval)

		path := make([]stepJSON, 0, len(rt.Path))
		for _, s := range rt.Path {
			path = append(path, stepJSON{
				Matchers: s.Matchers, Deprecated: s.Deprecated, Continue: s.Continue,
			})
		}
		doc.Routes = append(doc.Routes, routeJSON{
			Receiver:           rt.Receiver,
			Path:               path,
			GroupWaitMS:        gw,
			GroupWaitFrom:      gwFrom,
			GroupIntervalMS:    gi,
			GroupIntervalFrom:  giFrom,
			RepeatIntervalMS:   ri,
			RepeatIntervalFrom: riFrom,
			GroupBy:            rt.GroupBy,
			GroupByAll:         rt.GroupByAll,
			Shadowed:           rt.Shadowed,
		})
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return b, nil
}

// decodeRoutes reads the stored document back. A NULL or empty column is "not
// observed", which is the zero RouteResolution and NOT an error: it is the state
// every source is in before its first successful probe.
func decodeRoutes(raw []byte) (domain.RouteResolution, error) {
	if len(raw) == 0 {
		return domain.RouteResolution{}, nil
	}

	var doc routesJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return domain.RouteResolution{}, errs.Internal("jsonb_decode_failed", err)
	}

	out := domain.RouteResolution{
		Receiver:         doc.Receiver,
		Basis:            domain.ReceiverBasis(doc.Basis),
		WebhookReceivers: doc.WebhookReceivers,
		Dropped:          doc.Dropped,
		Routes:           make([]domain.ReceiverRoute, 0, len(doc.Routes)),
	}
	for _, rt := range doc.Routes {
		path := make([]domain.RouteStep, 0, len(rt.Path))
		for _, s := range rt.Path {
			path = append(path, domain.RouteStep{
				Matchers: s.Matchers, Deprecated: s.Deprecated, Continue: s.Continue,
			})
		}
		out.Routes = append(out.Routes, domain.ReceiverRoute{
			Receiver:       rt.Receiver,
			Path:           path,
			GroupWait:      decodeTiming(rt.GroupWaitMS, rt.GroupWaitFrom),
			GroupInterval:  decodeTiming(rt.GroupIntervalMS, rt.GroupIntervalFrom),
			RepeatInterval: decodeTiming(rt.RepeatIntervalMS, rt.RepeatIntervalFrom),
			GroupBy:        rt.GroupBy,
			GroupByAll:     rt.GroupByAll,
			Shadowed:       rt.Shadowed,
		})
	}
	return out, nil
}

// encodeTiming splits one inherited timing into its two stored halves. A negative
// duration cannot be produced by the parser, so it is treated as unstated rather
// than written — the same rule `durationMS` applies to the 00028 columns.
func encodeTiming(t domain.InheritedTiming) (*int64, *int) {
	if t.Value == nil || *t.Value < 0 {
		return nil, nil
	}
	ms := t.Value.Milliseconds()
	if t.FromDepth < 0 {
		return &ms, nil
	}
	from := t.FromDepth
	return &ms, &from
}

// decodeTiming rebuilds one inherited timing. FromDepth is -1 whenever there is
// no value, which is what `InheritedTiming.Stated` reads.
func decodeTiming(ms *int64, from *int) domain.InheritedTiming {
	if ms == nil {
		return domain.InheritedTiming{FromDepth: -1}
	}
	d := time.Duration(*ms) * time.Millisecond
	out := domain.InheritedTiming{Value: &d, FromDepth: -1}
	if from != nil && *from >= 0 {
		out.FromDepth = *from
	}
	return out
}
