package service

import "github.com/prometheus/client_golang/prometheus"

// InteractionMetrics is the INBOUND Slack surface's Prometheus surface. It is
// built once at wiring time and injected, so nothing here reaches for a global
// registry.
type InteractionMetrics struct {
	// UnknownAction counts verified interactions naming an `action_id` oto has no
	// branch for. SPEC §H.8's routing table requires it by name.
	//
	// ⭐ IT IS A COUNTER BECAUSE A LOG LINE IS NOT AN OUTCOME. An unknown action
	// is answered 200 — it must be, because Slack disables an app's event
	// subscriptions when more than 95 % of deliveries fail inside a 60-minute
	// window — so the ONLY evidence that a human pressed a button oto could not
	// route is what is recorded here. Without it the endpoint is back to the
	// defect this whole path exists to abolish: an authentic press, a tick shown
	// to the user, and nothing anywhere that says it happened.
	//
	// Non-zero means one of exactly two things, and both are oto's fault:
	//   - a card rendered an `action_id` the consumer does not switch on, or
	//   - an `action_id` was RENAMED, which retroactively breaks every card
	//     already sitting in Slack (see the constant block in interactions.go).
	//
	// ⛔ IT CARRIES NO LABELS, DELIBERATELY. The obvious label is `action_id`,
	// and its values come from a payload oto does not author — by definition an
	// unknown one is outside oto's closed set, so labelling by it hands an
	// unbounded key space to the time-series database. The id is on the WARN log
	// beside the increment, which is where high-cardinality detail belongs.
	UnknownAction prometheus.Counter
}

// NewInteractionMetrics registers the inbound-interaction metrics on reg. A nil
// registry yields unregistered collectors, which keeps tests cheap and wiring
// order free.
func NewInteractionMetrics(reg prometheus.Registerer) *InteractionMetrics {
	m := &InteractionMetrics{
		UnknownAction: prometheus.NewCounter(prometheus.CounterOpts{
			// ⚠️ SPEC §H.8 spells this `slack_unknown_action_total`, without the
			// prefix every other metric in the document and in this codebase
			// carries (`oto_render_invalid_total` is two tables further down the
			// same section, `oto_ingest_rejected_total` in §C.9.1). The prefix is
			// the convention; the SPEC line is an omission, not a second rule, and
			// an unprefixed metric would be the only one in oto's namespace that a
			// scrape config could not select with `{__name__=~"oto_.*"}`.
			Name: "oto_slack_unknown_action_total",
			Help: "Verified Slack interactions naming an action_id oto has no branch for. Every one is a button a human pressed that oto answered 200 and could not route; sustained non-zero means a card renders an action id the consumer does not serve, or an action id was renamed under cards already posted.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.UnknownAction)
	}
	return m
}

// unknownAction records one unroutable action id. The nil check is what lets
// every test and every partial wiring build the service without a registry.
func (m *InteractionMetrics) unknownAction() {
	if m == nil || m.UnknownAction == nil {
		return
	}
	m.UnknownAction.Inc()
}
