package service

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the streaming module's Prometheus surface. It is built once at wiring
// time and handed to the Hub and the Bridge as plain functions, so neither of them
// depends on a metrics library or on a global registry.
type Metrics struct {
	Connections     prometheus.Gauge
	Published       prometheus.Counter
	Fetched         prometheus.Counter
	Dropped         prometheus.Counter
	Coalesced       prometheus.Counter
	Resyncs         *prometheus.CounterVec
	NotifyReceived  prometheus.Counter
	NotifyMalformed prometheus.Counter
	Reconnects      prometheus.Counter
	PollRecovered   prometheus.Counter
	FetchErrors     prometheus.Counter
}

// NewMetrics registers the streaming metrics on reg. A nil registry yields
// unregistered collectors, which keeps tests cheap.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Connections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "oto_stream_connections",
			Help: "Live SSE connections attached to this pod.",
		}),
		Published: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_events_published_total",
			Help: "UI events accepted into a subscriber's buffer.",
		}),
		Fetched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_events_fetched_total",
			Help: "UI event rows read from Postgres by the bridge and handed to the hub.",
		}),
		Dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_events_dropped_total",
			Help: "UI events dropped because a subscriber's bounded buffer was full. oto never blocks a writer for a reader.",
		}),
		Coalesced: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_events_coalesced_total",
			Help: "Frames superseded within the 250 ms coalescing window, latest wins.",
		}),
		Resyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_stream_resync_total",
			Help: "Resync frames sent, by reason. Sustained buffer_overflow means clients cannot keep up.",
		}, []string{"reason"}),
		NotifyReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_notify_received_total",
			Help: "LISTEN/NOTIFY doorbells received.",
		}),
		NotifyMalformed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_notify_malformed_total",
			Help: "NOTIFY payloads that did not parse as <org_id>:<seq>.",
		}),
		Reconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_listener_reconnects_total",
			Help: "Times the LISTEN connection was re-established. Each one is a window of lost notifications, covered by the reconciling poll.",
		}),
		PollRecovered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_poll_recovered_total",
			Help: "Events found by the reconciling poll below the published watermark: rows that committed out of sequence order, or notifications that were lost. NON-ZERO IS NORMAL; a spike means notifications are being missed.",
		}),
		FetchErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_stream_fetch_errors_total",
			Help: "Failed catch-up reads of ui_events.",
		}),
	}

	if reg != nil {
		reg.MustRegister(
			m.Connections, m.Published, m.Fetched, m.Dropped, m.Coalesced, m.Resyncs,
			m.NotifyReceived, m.NotifyMalformed, m.Reconnects, m.PollRecovered, m.FetchErrors,
		)
	}
	return m
}

// HubMetrics adapts Metrics to the Hub's injection points.
func (m *Metrics) HubMetrics() HubMetrics {
	if m == nil {
		return HubMetrics{}
	}
	return HubMetrics{
		Connections:     func(d float64) { m.Connections.Add(d) },
		Published:       func(n int) { m.Published.Add(float64(n)) },
		Dropped:         func(n int) { m.Dropped.Add(float64(n)) },
		Resync:          func(reason string) { m.Resyncs.WithLabelValues(reason).Inc() },
		CoalesceSkipped: func(n int) { m.Coalesced.Add(float64(n)) },
	}
}

// BridgeMetrics adapts Metrics to the Bridge's injection points.
func (m *Metrics) BridgeMetrics() BridgeMetrics {
	if m == nil {
		return BridgeMetrics{}
	}
	return BridgeMetrics{
		NotifyReceived:  m.NotifyReceived.Inc,
		NotifyMalformed: m.NotifyMalformed.Inc,
		Reconnects:      m.Reconnects.Inc,
		Fetched:         func(n int) { m.Fetched.Add(float64(n)) },
		PollRecovered:   func(n int) { m.PollRecovered.Add(float64(n)) },
		FetchErrors:     m.FetchErrors.Inc,
	}
}

// ResyncCounter exposes the resync counter to the API layer, which is where a
// replay_window_exceeded decision is actually written to the wire.
func (m *Metrics) ResyncCounter(reason string) {
	if m != nil {
		m.Resyncs.WithLabelValues(reason).Inc()
	}
}
