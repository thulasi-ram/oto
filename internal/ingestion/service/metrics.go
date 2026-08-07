package service

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the ingestion module's Prometheus surface. It is built once at
// wiring time and injected, so nothing here reaches for a global registry.
//
// `oto_ingest_accepted_total`, `oto_ingest_rejected_total{reason}` and
// `oto_ingest_duration_seconds` are GUARANTEED by the published API contract
// (openapi `/metrics`), so their names are as much a contract as any JSON field.
type Metrics struct {
	// Accepted counts batches durably persisted, by mode.
	Accepted *prometheus.CounterVec
	// Duplicates counts batches collapsed by `ingest_dedup`. A steady non-zero
	// rate here is HEALTHY: it is an HA Alertmanager pair working as designed.
	Duplicates prometheus.Counter
	// Rejected counts recorded rejections by reason. This is the `reason` label of
	// the closed `ingest_rejections_reason_ck` enum.
	Rejected *prometheus.CounterVec
	// Alerts counts individual alerts accepted for processing.
	Alerts prometheus.Counter
	// Duration observes the accept path end to end. Its p99 budget is 250 ms and
	// its hard ceiling is 5 s — Alertmanager's retry floor is 10 s.
	Duration *prometheus.HistogramVec
	// ProcessDuration observes `ingest.process_batch`.
	ProcessDuration *prometheus.HistogramVec
	// Shed counts 503s by the reason we shed, which is the difference between
	// "Postgres is slow" and "the queue is behind".
	Shed *prometheus.CounterVec
	// FingerprintMismatch counts alerts whose wire fingerprint disagreed with
	// oto's recomputation (C10). Never fatal; always counted.
	FingerprintMismatch prometheus.Counter
	// ClockSkew observes `received_at - startsAt` in seconds. Measured and
	// surfaced, never a reason to reject (C12).
	ClockSkew prometheus.Histogram
}

// NewMetrics registers the ingestion metrics on reg. A nil registry yields
// unregistered collectors, which keeps tests cheap and wiring order free.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Accepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_ingest_accepted_total",
			Help: "Batches durably persisted and enqueued. A 2xx is a promise; this counts the promises.",
		}, []string{"mode"}),
		Duplicates: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_ingest_duplicate_total",
			Help: "Batches suppressed by ingest_dedup. Non-zero is healthy: HA Alertmanager is at-least-once by design.",
		}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_ingest_rejected_total",
			Help: "Observations recorded in ingest_rejections, by reason. oto never silently drops.",
		}, []string{"reason"}),
		Alerts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_ingest_alerts_total",
			Help: "Individual alerts accepted for processing.",
		}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oto_ingest_duration_seconds",
			Help:    "Webhook accept latency. p99 budget 250 ms, hard ceiling 5 s.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"outcome"}),
		ProcessDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oto_ingest_process_duration_seconds",
			Help:    "ingest.process_batch latency, by outcome.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"outcome"}),
		Shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_ingest_shed_total",
			Help: "Requests answered 503 + Retry-After as deliberate backpressure, by reason. Never 429 (C4).",
		}, []string{"reason"}),
		FingerprintMismatch: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "oto_ingest_fingerprint_mismatch_total",
			Help: "Alerts whose wire fingerprint differed from oto's recomputation. oto stores its own.",
		}),
		ClockSkew: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "oto_clock_skew_seconds",
			Help:    "received_at minus the upstream startsAt. Measured and surfaced, never a rejection.",
			Buckets: []float64{-60, -5, -1, 0, 1, 5, 30, 60, 300, 3600},
		}),
	}

	if reg != nil {
		reg.MustRegister(
			m.Accepted, m.Duplicates, m.Rejected, m.Alerts,
			m.Duration, m.ProcessDuration, m.Shed,
			m.FingerprintMismatch, m.ClockSkew,
		)
	}
	return m
}

// countRejections increments the reason counter for a set of recorded rejections.
func (m *Metrics) countRejections(reasons ...string) {
	for _, r := range reasons {
		m.Rejected.WithLabelValues(r).Inc()
	}
}
