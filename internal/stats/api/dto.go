package api

import (
	"time"

	"github.com/thulasiram/oto/internal/stats/domain"
	"github.com/thulasiram/oto/internal/stats/service"
)

// The wire DTOs of the Stats tag. Every json tag is byte-identical to
// api/openapi/openapi.yaml.

// AlertStateCountsDTO is the open-state roll-up.
//
// `resolved` and `expired` are separate fields and are NEVER summed into one
// "closed" bucket: losing sight of an alert is not the alert going away.
type AlertStateCountsDTO struct {
	Firing     int32 `json:"firing"`
	Suppressed int32 `json:"suppressed"`
	Resolved   int32 `json:"resolved"`
	Expired    int32 `json:"expired"`
	Acked      int32 `json:"acked"`
	Unacked    int32 `json:"unacked"`
	Flapping   int32 `json:"flapping"`
}

// GroupCountsDTO is the group half of the roll-up.
type GroupCountsDTO struct {
	Open   int32 `json:"open"`
	Closed int32 `json:"closed"`
}

// DeliveryCountsDTO is the delivery-health half. A non-zero `dead` is a product
// signal, not a footnote.
type DeliveryCountsDTO struct {
	Sent      int32 `json:"sent"`
	Failed    int32 `json:"failed"`
	Dead      int32 `json:"dead"`
	Skipped   int32 `json:"skipped"`
	Pending   int32 `json:"pending"`
	Ambiguous int32 `json:"ambiguous,omitempty"`
}

// SourceCountsDTO is the source-health half. It is what gates the reaper.
type SourceCountsDTO struct {
	Healthy         int32 `json:"healthy"`
	Degraded        int32 `json:"degraded"`
	Unreachable     int32 `json:"unreachable"`
	Unknown         int32 `json:"unknown"`
	MaxClockSkewMS  int64 `json:"max_clock_skew_ms,omitempty"`
	TotalDivergence int32 `json:"total_divergence,omitempty"`
}

// ChannelCountsDTO is the channel-health half.
type ChannelCountsDTO struct {
	Healthy       int32 `json:"healthy"`
	Degraded      int32 `json:"degraded"`
	AuthFailed    int32 `json:"auth_failed"`
	ConfigInvalid int32 `json:"config_invalid"`
}

// WindowDTO is the range a roll-up was computed over.
type WindowDTO struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// StatsOverviewDTO renders `StatsOverviewDTO`: the dashboard roll-up.
//
// It deliberately contains no per-person data of any kind.
type StatsOverviewDTO struct {
	Alerts      AlertStateCountsDTO `json:"alerts"`
	Groups      GroupCountsDTO      `json:"groups"`
	Deliveries  DeliveryCountsDTO   `json:"deliveries"`
	Sources     SourceCountsDTO     `json:"sources"`
	Channels    *ChannelCountsDTO   `json:"channels,omitempty"`
	Window      *WindowDTO          `json:"window,omitempty"`
	GeneratedAt time.Time           `json:"generated_at"`
}

// AlertQualityDTO renders `AlertQualityDTO`: alert hygiene per alertname per
// cluster.
//
// ⛔ There is no user field here and no way to add one: the underlying rollup
// carries no user column, so per-person aggregates are unrepresentable rather
// than merely absent.
type AlertQualityDTO struct {
	AlertName     string  `json:"alertname"`
	ClusterKey    string  `json:"cluster_key"`
	Cases         int32   `json:"cases"`
	Notifications int32   `json:"notifications"`
	Deliveries    int32   `json:"deliveries"`
	AckedCases    int32   `json:"acked_cases"`
	AckRate       float32 `json:"ack_rate"`
	AutoResolved  int32   `json:"auto_resolved"`
	Expired       int32   `json:"expired"`
	// TotalFiringSeconds is the summed FIRING DURATION.
	TotalFiringSeconds int64   `json:"total_firing_seconds"`
	FlapTransitions    int32   `json:"flap_transitions"`
	FlapScore          float32 `json:"flap_score"`
}

// ------------------------------------------------------------- query objects

// OverviewQuery is the validated form of the `getStatsOverview` query string.
type OverviewQuery struct {
	Since   *time.Time `json:"since"`
	Until   *time.Time `json:"until"`
	Cluster []string   `json:"cluster" validate:"omitempty,max=32,unique,dive,clusterkey"`
}

// AlertQualityQuery is the validated form of the `getAlertQualityStats` query.
type AlertQualityQuery struct {
	Since     *time.Time `json:"since"`
	Until     *time.Time `json:"until"`
	Cluster   []string   `json:"cluster"   validate:"omitempty,max=32,unique,dive,clusterkey"`
	AlertName []string   `json:"alertname" validate:"omitempty,max=64,unique,dive,max=1024"`
	Sort      string     `json:"sort"      validate:"omitempty,oneof=-cases -notifications ack_rate -flap_transitions -total_firing_seconds"`
	Limit     int        `json:"limit"     validate:"min=1,max=200"`
	Cursor    string     `json:"cursor"    validate:"omitempty,cursor"`
}

// ------------------------------------------------------------------ mappers

func overviewDTO(res service.OverviewResult) StatsOverviewDTO {
	o := res.Overview
	return StatsOverviewDTO{
		Alerts: AlertStateCountsDTO{
			Firing:     int32(o.Alerts.Firing),
			Suppressed: int32(o.Alerts.Suppressed),
			Resolved:   int32(o.Alerts.Resolved),
			Expired:    int32(o.Alerts.Expired),
			Acked:      int32(o.Alerts.Acked),
			Unacked:    int32(o.Alerts.Unacked),
			Flapping:   int32(o.Alerts.Flapping),
		},
		Groups: GroupCountsDTO{
			Open:   int32(o.Groups.Open),
			Closed: int32(o.Groups.Closed),
		},
		Deliveries: DeliveryCountsDTO{
			Sent:      int32(o.Deliveries.Sent),
			Failed:    int32(o.Deliveries.Failed),
			Dead:      int32(o.Deliveries.Dead),
			Skipped:   int32(o.Deliveries.Skipped),
			Pending:   int32(o.Deliveries.Pending),
			Ambiguous: int32(o.Deliveries.Ambiguous),
		},
		Sources: SourceCountsDTO{
			Healthy:         int32(o.Sources.Healthy),
			Degraded:        int32(o.Sources.Degraded),
			Unreachable:     int32(o.Sources.Unreachable),
			Unknown:         int32(o.Sources.Unknown),
			MaxClockSkewMS:  o.Sources.MaxClockSkewMS,
			TotalDivergence: int32(o.Sources.TotalDivergence),
		},
		Channels: &ChannelCountsDTO{
			Healthy:       int32(o.Channels.Healthy),
			Degraded:      int32(o.Channels.Degraded),
			AuthFailed:    int32(o.Channels.AuthFailed),
			ConfigInvalid: int32(o.Channels.ConfigInvalid),
		},
		Window:      &WindowDTO{Since: res.Window.Since, Until: res.Window.Until},
		GeneratedAt: res.GeneratedAt,
	}
}

func qualityDTO(q domain.AlertQuality) AlertQualityDTO {
	return AlertQualityDTO{
		AlertName:          q.AlertName,
		ClusterKey:         q.ClusterKey,
		Cases:              int32(q.Cases),
		Notifications:      int32(q.Notifications),
		Deliveries:         int32(q.Deliveries),
		AckedCases:         int32(q.AckedCases),
		AckRate:            q.AckRate(),
		AutoResolved:       int32(q.AutoResolved),
		Expired:            int32(q.Expired),
		TotalFiringSeconds: q.TotalFiringSeconds,
		FlapTransitions:    int32(q.FlapTransitions),
		FlapScore:          q.FlapScore(),
	}
}
