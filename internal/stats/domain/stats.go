package domain

// THE VALUE OBJECTS of alert-hygiene accounting.
//
// ⛔ TEAM- AND ALERT-SCOPED ONLY (SPEC R8, SCOPE-BOUNDARY). There is no user
// field anywhere in this package and there is no way to add one without changing
// the rollup table, which carries no user column either. Per-person response-time
// metrics, leaderboards and per-individual aggregates are not merely omitted —
// they are unrepresentable, and a feature that does not exist cannot be misused.
//
// The vocabulary is binding: the summed time an alert spent firing is called
// FIRING DURATION.

import "github.com/thulasiram/oto/internal/platform/errs"

// Sort names the hygiene problem a caller wants surfaced first.
//
// The set is closed because each value maps to one indexed ordering over the
// rollup; an arbitrary sort key would mean an unindexed sort over an aggregate.
type Sort string

// The five sort keys (openapi.yaml `getAlertQualityStats`).
const (
	// SortOccurrencesDesc finds the noisiest rules. It is the default.
	SortOccurrencesDesc Sort = "-occurrences"
	// SortNotificationsDesc finds the ones that cost the most interruptions.
	SortNotificationsDesc Sort = "-notifications"
	// SortAckRateAsc finds the ones nobody ever acknowledges — the rules whose
	// best possible future is deletion.
	SortAckRateAsc Sort = "ack_rate"
	// SortFlapTransitionsDesc finds the unstable ones.
	SortFlapTransitionsDesc Sort = "-flap_transitions"
	// SortFiringSecondsDesc finds the ones that were firing longest.
	SortFiringSecondsDesc Sort = "-total_firing_seconds"
)

// NewSort parses a sort key.
func NewSort(s string) (Sort, error) {
	switch Sort(s) {
	case SortOccurrencesDesc, SortNotificationsDesc, SortAckRateAsc,
		SortFlapTransitionsDesc, SortFiringSecondsDesc:
		return Sort(s), nil
	case "":
		return SortOccurrencesDesc, nil
	default:
		return "", errs.New(errs.KindValidation, "enum",
			"sort must be one of: -occurrences, -notifications, ack_rate, -flap_transitions, -total_firing_seconds")
	}
}

// String renders the sort key.
func (s Sort) String() string { return string(s) }

// AlertQuality is one row of the hygiene report: per alertname, per cluster.
//
// This is the row that answers *"this rule fired 47 times this month, cost 47
// notifications, and was acknowledged 0 times"* — which does more good than any
// enrichment, because the best alert is the one that no longer exists.
type AlertQuality struct {
	AlertName        string
	ClusterKey       string
	Occurrences      int
	Notifications    int
	Deliveries       int
	AckedOccurrences int
	AutoResolved     int
	// Expired is counted apart from AutoResolved, always. Losing sight of an
	// alert is not the alert going away, and summing the two into one "closed"
	// bucket is exactly the lie oto exists to prevent.
	Expired int
	// TotalFiringSeconds is the summed FIRING DURATION.
	TotalFiringSeconds int64
	FlapTransitions    int
}

// AckRate is AckedOccurrences / Occurrences, or 0 when there were none.
func (q AlertQuality) AckRate() float32 {
	if q.Occurrences <= 0 {
		return 0
	}
	return float32(q.AckedOccurrences) / float32(q.Occurrences)
}

// FlapScore is transitions per occurrence, the noisiness signal for the report.
func (q AlertQuality) FlapScore() float32 {
	if q.Occurrences <= 0 {
		return 0
	}
	return float32(q.FlapTransitions) / float32(q.Occurrences)
}

// SortValue is the number this row is ordered by, which is also the keyset
// position a cursor carries.
func (q AlertQuality) SortValue(s Sort) float64 {
	switch s {
	case SortNotificationsDesc:
		return float64(q.Notifications)
	case SortAckRateAsc:
		return float64(q.AckRate())
	case SortFlapTransitionsDesc:
		return float64(q.FlapTransitions)
	case SortFiringSecondsDesc:
		return float64(q.TotalFiringSeconds)
	default:
		return float64(q.Occurrences)
	}
}

// AlertCounts is the open-state roll-up of the dashboard.
//
// `Resolved` and `Expired` are separate fields and are never summed into one
// "closed" bucket.
type AlertCounts struct {
	Firing     int
	Suppressed int
	Resolved   int
	Expired    int
	Acked      int
	Unacked    int
	Flapping   int
}

// GroupCounts is the group half of the dashboard roll-up.
type GroupCounts struct {
	Open   int
	Closed int
	Storm  int
}

// DeliveryCounts is the delivery-health half.
//
// A non-zero Dead is a product signal, not a footnote: oto's silence must never
// be indistinguishable from "no alert fired".
type DeliveryCounts struct {
	Sent      int
	Failed    int
	Dead      int
	Skipped   int
	Pending   int
	Ambiguous int
}

// SourceCounts is the source-health half. It gates the reaper: while a source is
// anything other than healthy, occurrences are held rather than expired.
type SourceCounts struct {
	Healthy         int
	Degraded        int
	Unreachable     int
	Unknown         int
	MaxClockSkewMS  int64
	TotalDivergence int
}

// ChannelCounts is the channel-health half.
type ChannelCounts struct {
	Healthy       int
	Degraded      int
	AuthFailed    int
	ConfigInvalid int
}

// Overview is the whole dashboard roll-up. It deliberately contains no
// per-person data of any kind.
type Overview struct {
	Alerts     AlertCounts
	Groups     GroupCounts
	Deliveries DeliveryCounts
	Sources    SourceCounts
	Channels   ChannelCounts
}
