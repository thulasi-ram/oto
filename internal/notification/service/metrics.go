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
}

// NewMetrics registers the delivery metrics on reg. A nil registry yields
// unregistered collectors, which keeps tests cheap and wiring order free.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ClaimLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_delivery_claim_lost_total",
			Help: "Sends that reached the provider but could not be recorded because the claim was gone. Every one is a possible duplicate message; sustained non-zero means the claim lease is too short.",
		}, []string{"mode"}),
	}
	if reg != nil {
		reg.MustRegister(m.ClaimLost)
	}
	return m
}
