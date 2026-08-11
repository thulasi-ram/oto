package api

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// The wire half of the route resolution: WHICH ROUTE the three headline numbers
// describe, and what happens when the routes reaching oto disagree.
//
// ⛔ THE ONE COMBINATION THAT MUST NEVER BE SERVED is `route: oto_receiver` with
// `routes_agree: false`. That would be oto picking one of several conflicting
// answers and presenting it as a reading — the exact failure the hand-typed
// tuning form was replaced for. Every case below exists to pin one side of that.

// step builds one route step for a fixture.
func step(matchers ...string) domain.RouteStep {
	return domain.RouteStep{Matchers: matchers}
}

// route builds one delivering route with its three timings stated at the depth
// given, so the fixtures read like the config they stand for.
func route(receiver string, path []domain.RouteStep, gw, gi, ri *time.Duration) domain.ReceiverRoute {
	depth := len(path) - 1
	at := func(d *time.Duration) domain.InheritedTiming {
		if d == nil {
			return domain.InheritedTiming{FromDepth: -1}
		}
		return domain.InheritedTiming{Value: d, FromDepth: depth}
	}
	return domain.ReceiverRoute{
		Receiver: receiver, Path: path,
		GroupWait: at(gw), GroupInterval: at(gi), RepeatInterval: at(ri),
	}
}

// probed is a health projection that HAS been read, which is the only state in
// which a route resolution can exist.
func probed(res domain.RouteResolution, top domain.RouteTimings) domain.SourceHealth {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	return domain.SourceHealth{
		Status:         domain.HealthHealthy,
		AMVersion:      "0.28.1",
		RouteTimings:   top,
		Routes:         res,
		RouteTimingsAt: &at,
	}
}

func TestTheHeadlineNumbersComeFromOtosOwnRouteWhenThereIsOne(t *testing.T) {
	t.Parallel()

	// The top-level route says 5m; the route oto's receiver hangs off says 1m.
	// 1m is what governs the alerts oto is sent, and 5m is the number every
	// tuning verdict used to be computed from.
	tm := timings(t, probed(
		domain.RouteResolution{
			Receiver: "oto",
			Basis:    domain.ReceiverSoleWebhook,
			Routes: []domain.ReceiverRoute{
				route("fallback", []domain.RouteStep{step()}, dur(30*time.Second), dur(5*time.Minute), nil),
				route("oto", []domain.RouteStep{step(), step(`team="sre"`)}, nil, dur(time.Minute), nil),
			},
			WebhookReceivers: []string{"oto"},
		},
		domain.RouteTimings{GroupWait: dur(30 * time.Second), GroupInterval: dur(5 * time.Minute)},
	))

	if tm["route"] != RouteOtoReceiver {
		t.Fatalf("route = %v, want %s", tm["route"], RouteOtoReceiver)
	}
	if gi := field(t, tm, "group_interval"); gi["value_ms"] != float64(60000) {
		t.Fatalf("group_interval = %v ms, want 60000 — the route oto is attached to, "+
			"not the top-level route's 300000", gi["value_ms"])
	}
	if tm["receiver"] != "oto" {
		t.Fatalf("receiver = %v, want oto", tm["receiver"])
	}
	if tm["receiver_basis"] != string(domain.ReceiverSoleWebhook) {
		t.Fatalf("receiver_basis = %v, want sole_webhook", tm["receiver_basis"])
	}
	if tm["routes_agree"] != true {
		t.Fatal("one reaching route was reported as disagreeing with itself")
	}
}

func TestTwoRoutesThatDisagreeFallBackToTheTopLevelAndSaySo(t *testing.T) {
	t.Parallel()

	tm := timings(t, probed(
		domain.RouteResolution{
			Receiver: "oto",
			Basis:    domain.ReceiverSoleWebhook,
			Routes: []domain.ReceiverRoute{
				route("oto", []domain.RouteStep{step(), step(`severity="critical"`)}, nil, dur(30*time.Second), nil),
				route("oto", []domain.RouteStep{step(), step(`team="sre"`)}, nil, dur(10*time.Minute), nil),
			},
			WebhookReceivers: []string{"oto"},
		},
		domain.RouteTimings{GroupWait: dur(30 * time.Second), GroupInterval: dur(5 * time.Minute)},
	))

	if tm["routes_agree"] != false {
		t.Fatal("30s and 10m were reported as agreeing")
	}
	if tm["route"] != RouteTopLevel {
		t.Fatalf("route = %v, want %s: with a disagreement there is no single reaching "+
			"route to argue from, and inventing one is the whole failure mode", tm["route"], RouteTopLevel)
	}
	if gi := field(t, tm, "group_interval"); gi["value_ms"] != float64(300000) {
		t.Fatalf("group_interval = %v ms, want the top-level 300000", gi["value_ms"])
	}

	routes, ok := tm["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("routes = %v, want both conflicting routes on the wire for the client to show", tm["routes"])
	}
	for i, raw := range routes {
		r, _ := raw.(map[string]any)
		if r["reaches_oto"] != true {
			t.Fatalf("route %d: reaches_oto = %v, want true", i, r["reaches_oto"])
		}
	}
}

func TestAnAmbiguousReceiverClaimsNoRouteAtAll(t *testing.T) {
	t.Parallel()

	// Two webhook receivers and a redacted URL apiece. oto shows every route and
	// claims none of them — `reaches_oto` is false everywhere, which is what makes
	// the ambiguity visible rather than resolved by a coin toss.
	tm := timings(t, probed(
		domain.RouteResolution{
			Basis: domain.ReceiverAmbiguous,
			Routes: []domain.ReceiverRoute{
				route("a", []domain.RouteStep{step(), step(`team="a"`)}, nil, dur(time.Minute), nil),
				route("b", []domain.RouteStep{step(), step(`team="b"`)}, nil, dur(2*time.Minute), nil),
			},
			WebhookReceivers: []string{"a", "b"},
		},
		domain.RouteTimings{GroupInterval: dur(5 * time.Minute)},
	))

	if tm["route"] != RouteTopLevel {
		t.Fatalf("route = %v, want %s", tm["route"], RouteTopLevel)
	}
	if tm["receiver"] != nil {
		t.Fatalf("receiver = %v, want null when oto cannot tell which is its own", tm["receiver"])
	}
	if tm["receiver_basis"] != string(domain.ReceiverAmbiguous) {
		t.Fatalf("receiver_basis = %v, want ambiguous", tm["receiver_basis"])
	}
	for i, raw := range tm["routes"].([]any) {
		r, _ := raw.(map[string]any)
		if r["reaches_oto"] != false {
			t.Fatalf("route %d claims to reach oto while the receiver is ambiguous", i)
		}
	}
	cands, _ := tm["webhook_receivers"].([]any)
	if len(cands) != 2 {
		t.Fatalf("webhook_receivers = %v, want both candidates named", tm["webhook_receivers"])
	}
}

func TestAnUnprobedSourceHasNoRoutesAndSaysUnknown(t *testing.T) {
	t.Parallel()

	tm := timings(t, domain.SourceHealth{Status: domain.HealthUnknown})

	if tm["receiver_basis"] != string(domain.ReceiverUnknown) {
		t.Fatalf("receiver_basis = %v, want unknown", tm["receiver_basis"])
	}
	if tm["route"] != RouteTopLevel {
		t.Fatalf("route = %v, want %s", tm["route"], RouteTopLevel)
	}
	// ⭐ AN EMPTY ARRAY, NOT NULL. A client that had to tell one from the other to
	// know whether oto has looked is a client with a bug waiting in it — that fact
	// lives in `receiver_basis` and `observed_at`.
	routes, ok := tm["routes"].([]any)
	if !ok || len(routes) != 0 {
		t.Fatalf("routes = %#v, want []", tm["routes"])
	}
	if _, ok := tm["webhook_receivers"].([]any); !ok {
		t.Fatalf("webhook_receivers = %#v, want []", tm["webhook_receivers"])
	}
	if tm["routes_agree"] != true {
		t.Fatal("routes_agree is false with nothing to disagree; it must only ever be " +
			"false when there is a real conflict to show")
	}
}

func TestAPerRouteTimingCarriesTheDepthThatStatedIt(t *testing.T) {
	t.Parallel()

	// group_wait is stated only at the root and group_interval only on the child.
	// Same numbers, two different lines of the operator's file — and only one of
	// them survives them editing the child.
	res := domain.RouteResolution{
		Receiver: "oto", Basis: domain.ReceiverSoleWebhook, WebhookReceivers: []string{"oto"},
		Routes: []domain.ReceiverRoute{{
			Receiver: "oto",
			Path:     []domain.RouteStep{step(), step(`team="sre"`)},
			GroupWait: domain.InheritedTiming{
				Value: dur(30 * time.Second), FromDepth: 0,
			},
			GroupInterval: domain.InheritedTiming{
				Value: dur(time.Minute), FromDepth: 1,
			},
			RepeatInterval: domain.InheritedTiming{FromDepth: -1},
			GroupBy:        []string{"alertname"},
		}},
	}

	tm := timings(t, probed(res, domain.RouteTimings{}))
	r, _ := tm["routes"].([]any)[0].(map[string]any)

	gw, _ := r["group_wait"].(map[string]any)
	if gw["provenance"] != string(domain.TimingObserved) || gw["from_depth"] != float64(0) {
		t.Fatalf("group_wait = %v, want observed from depth 0 (inherited from the root)", gw)
	}
	gi, _ := r["group_interval"].(map[string]any)
	if gi["from_depth"] != float64(1) {
		t.Fatalf("group_interval from_depth = %v, want 1 (this route states it)", gi["from_depth"])
	}

	// ⛔ A TIMING NO ROUTE ON THE PATH STATES IS `default_applies`, NOT `unknown`.
	// The route exists only because oto read the configuration that declares it,
	// so "we could not look" cannot arise here — and reporting it as unknown would
	// withdraw guidance that is perfectly valid.
	ri, _ := r["repeat_interval"].(map[string]any)
	if ri["provenance"] != string(domain.TimingDefaultApplies) {
		t.Fatalf("repeat_interval provenance = %v, want default_applies", ri["provenance"])
	}
	if ri["value_ms"] != float64(domain.DefaultRepeatInterval.Milliseconds()) {
		t.Fatalf("repeat_interval = %v ms, want Alertmanager's documented default", ri["value_ms"])
	}
	if ri["from_depth"] != nil {
		t.Fatalf("from_depth = %v on a value no route states", ri["from_depth"])
	}
}
