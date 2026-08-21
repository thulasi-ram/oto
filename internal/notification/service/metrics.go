package service

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the notification module's delivery-side Prometheus surface. It is
// built once at wiring time and injected, so nothing here reaches for a global
// registry.
type Metrics struct {
	// ClaimLost counts sends that reached the provider and could NOT be recorded,
	// because the row was no longer this worker's to write: the §G.5 lease expired
	// mid-call and somebody else reclaimed it, or a recovery resolved the slot.
	//
	// THIS COUNTER IS AN ALERT, NOT A STATISTIC. Every increment is a message that
	// exists in somebody's channel with no `sent` row behind it, which means oto
	// has forgotten a delivery it made and may make it again. Sustained non-zero
	// means the claim lease is shorter than the provider's real latency.
	ClaimLost *prometheus.CounterVec

	// RenderInvalid counts deliveries that died because oto could not build a
	// legal payload for them — a renderer error, or a payload its own §L.6 checks
	// refused.
	//
	// THIS COUNTER IS AN ALERT, NOT A STATISTIC, AND IT IS THE ONLY ONE THIS
	// FAILURE HAS. A render failure is oto's bug, never the destination's: nobody
	// was told, and no provider was ever called. The delivery row is marked dead
	// in the same transaction, so the job goes on to report success and
	// `oto_jobs_dead_total` stays flat — which is why the SPEC's metrics table was
	// wrong to name that counter as the alarm here (git-bug: finding 7).
	//
	// ⛔ THERE IS NO `check` LABEL, THOUGH V0–V18 WOULD BE BOUNDED ENOUGH FOR ONE.
	// The check that refused the payload is a Slack concept and this module holds
	// no provider-specific code; `render/slack/validate.go` refuses the same label
	// from the other side. The check name reaches an operator through the log line
	// and through the dead delivery's stored error, next to the bytes that failed.
	RenderInvalid *prometheus.CounterVec
}

// NewMetrics registers the delivery metrics on reg. A nil registry yields
// unregistered collectors, which keeps tests cheap and wiring order free.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ClaimLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_delivery_claim_lost_total",
			Help: "Sends that reached the provider but could not be recorded because the claim was gone. Every one is a possible duplicate message; sustained non-zero means the claim lease is too short.",
		}, []string{"mode"}),
		RenderInvalid: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_render_invalid_total",
			Help: "Deliveries that died because oto could not render a legal payload. Every one is an oto bug: nobody was told and no provider was called.",
		}, []string{"provider", "renderer", "mode"}),
	}
	if reg != nil {
		reg.MustRegister(m.ClaimLost, m.RenderInvalid)
	}
	return m
}
