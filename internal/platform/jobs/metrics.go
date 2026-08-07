package jobs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the job runtime's Prometheus surface.
//
// oto is an alerting product: its own observability is expected to be exemplary,
// and every one of these has an obvious alert attached to it. `dead` and
// `unknown_version` in particular are the two that mean "work was accepted and
// then silently not done", which is the failure mode this whole subsystem exists
// to make impossible to miss.
type Metrics struct {
	Enqueued       *prometheus.CounterVec
	Started        *prometheus.CounterVec
	Succeeded      *prometheus.CounterVec
	Failed         *prometheus.CounterVec
	Dead           *prometheus.CounterVec
	Snoozed        *prometheus.CounterVec
	Panics         *prometheus.CounterVec
	UnknownVersion *prometheus.CounterVec
	Duration       *prometheus.HistogramVec
	QueueDepth     *prometheus.GaugeVec
}

// NewMetrics registers the job metrics on reg. A nil registry is legal and yields
// unregistered collectors, which keeps tests and one-shot commands cheap.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Enqueued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_enqueued_total",
			Help: "Jobs inserted into the queue, by kind and queue.",
		}, []string{"kind", "queue"}),

		Started: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_started_total",
			Help: "Job executions begun, by kind and queue.",
		}, []string{"kind", "queue"}),

		Succeeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_succeeded_total",
			Help: "Job executions that returned nil, by kind and queue.",
		}, []string{"kind", "queue"}),

		Failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_failed_total",
			Help: "Job executions that returned an error, by kind, queue and SPEC §G.6 error class.",
		}, []string{"kind", "queue", "class"}),

		Dead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_dead_total",
			Help: "Jobs sent to the dead-letter: terminal error class, or the attempt ceiling reached. ALERT ON THIS.",
		}, []string{"kind", "queue", "class"}),

		Snoozed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_snoozed_total",
			Help: "Job executions deferred without consuming an attempt, by kind, queue and reason.",
		}, []string{"kind", "queue", "reason"}),

		Panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_panics_total",
			Help: "Panics recovered inside a job handler. Always a bug. ALERT ON THIS.",
		}, []string{"kind", "queue"}),

		UnknownVersion: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_jobs_unknown_version_total",
			Help: "Jobs parked because their payload version is newer than this worker understands (SPEC §G.3). ALERT ON THIS.",
		}, []string{"kind", "queue"}),

		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "oto_job_duration_seconds",
			Help: "Wall time of one job execution, by kind, queue and outcome.",
			// Buckets span 5 ms to ~5 min: a delivery is tens of milliseconds, a
			// 5 000-alert ingest batch is seconds, a partition sweep is minutes.
			Buckets: []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"kind", "queue", "outcome"}),

		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "oto_job_queue_depth",
			Help: "Jobs in the queue by queue name and river state (available, running, retryable, scheduled, discarded, cancelled).",
		}, []string{"queue", "state"}),
	}

	if reg != nil {
		reg.MustRegister(
			m.Enqueued, m.Started, m.Succeeded, m.Failed, m.Dead, m.Snoozed,
			m.Panics, m.UnknownVersion, m.Duration, m.QueueDepth,
		)
	}
	return m
}
