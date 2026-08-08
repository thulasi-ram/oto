package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/service"
)

// IngestPathPrefix is the route the ingest webhook is mounted at. It is spelled
// here because `createSource` has to hand the operator the exact path to paste
// into `webhook_config`, and a value that drifts from the real route is a source
// that silently never delivers.
const IngestPathPrefix = "/api/v1/ingest/alertmanager/"

// ingestPath renders the webhook path for one source.
func ingestPath(sourceID uuid.UUID) string { return IngestPathPrefix + sourceID.String() }

// clusterDTO maps a cluster onto the wire.
func clusterDTO(c domain.Cluster) ClusterDTO {
	return ClusterDTO{
		ID:          c.ID,
		ClusterKey:  c.Key,
		DisplayName: c.DisplayName,
		SourceCount: int32(c.SourceCount), //nolint:gosec // a count of rows in one org
		CreatedAt:   c.CreatedAt.UTC(),
		UpdatedAt:   c.UpdatedAt.UTC(),
	}
}

// sourceDTO maps a source onto the wire.
//
// `clusterKey` and `health` are joined in by the handler rather than fetched
// here: a mapper that queried would turn a page of twenty sources into forty-one
// round trips.
func sourceDTO(s domain.Source, clusterKey string, health *domain.SourceHealth) SourceDTO {
	out := SourceDTO{
		ID:                       s.ID,
		ClusterID:                s.ClusterID,
		ClusterKey:               clusterKey,
		Name:                     s.Name,
		Kind:                     string(s.Kind),
		BaseURL:                  s.BaseURL,
		PrometheusURL:            optionalString(s.PrometheusURL),
		TLSSkipVerify:            s.TLSSkipVerify,
		InjectLabels:             nonNilMap(s.InjectLabels),
		IgnoreLabels:             nonNilSlice(s.IgnoreLabels),
		RedactLabels:             nonNilSlice(s.RedactLabels),
		RedactAnnotations:        nonNilSlice(s.RedactAnnotations),
		PushEnabled:              s.PushEnabled,
		ReconcileEnabled:         s.ReconcileEnabled,
		ReconcileIntervalSeconds: int32(s.ReconcileInterval / time.Second), //nolint:gosec // <= 3600
		IngestPath:               ingestPath(s.ID),
		CreatedAt:                s.CreatedAt.UTC(),
		UpdatedAt:                s.UpdatedAt.UTC(),
	}
	if health != nil {
		h := healthDTO(*health)
		out.Health = &h
	}
	return out
}

// healthDTO maps the liveness projection onto the wire.
func healthDTO(h domain.SourceHealth) SourceHealthDTO {
	warnings := make([]HealthWarningDTO, 0, len(h.Warnings))
	for _, w := range h.Warnings {
		warnings = append(warnings, HealthWarningDTO{
			Code:    w.Code,
			Message: w.Message,
			Subject: w.Subject,
			Since:   optionalTime(w.At),
		})
	}
	status := h.Status
	if status == "" {
		// A source that has never been probed is `unknown`, not blank. "Not yet
		// observed" is a state, and it is the state that blocks the reaper.
		status = domain.HealthUnknown
	}
	return SourceHealthDTO{
		SourceID:            h.SourceID,
		Status:              string(status),
		LastPushAt:          utcPtr(h.LastPushAt),
		LastReconcileAt:     utcPtr(h.LastReconcileAt),
		LastReconcileStatus: optionalString(h.LastReconcileStatus),
		LastError:           optionalString(h.LastError),
		ConsecutiveFailures: int32(h.ConsecutiveFailures), //nolint:gosec // bounded by source_health_fail_ck
		AMVersion:           optionalString(h.AMVersion),
		SendResolved:        h.SendResolved,
		ClockSkewMS:         h.ClockSkew.Milliseconds(),
		DivergenceCount:     int32(h.DivergenceCount), //nolint:gosec // bounded by source_health_div_ck
		Warnings:            warnings,
		UpdatedAt:           h.UpdatedAt.UTC(),
	}
}

// probeDTO maps a probe result onto the wire.
//
// It deliberately reports `ok: false` with a populated `error` rather than
// failing the request: the probe SUCCEEDED in establishing that the upstream is
// unreachable, and a 502 here would tell the operator strictly less.
func probeDTO(p service.ProbeResult, sendResolved map[string]bool) SourceTestDTO {
	out := SourceTestDTO{
		OK:           p.Reachable,
		CheckedAt:    p.CheckedAt.UTC(),
		SendResolved: sendResolved,
	}
	if p.AlertmanagerVersion != "" {
		v := p.AlertmanagerVersion
		out.AMVersion = &v
	}
	if p.ClusterStatus != "" {
		v := p.ClusterStatus
		out.ClusterState = &v
	}
	if p.ClusterPeers > 0 {
		v := int32(p.ClusterPeers) //nolint:gosec // an HA peer count
		out.ClusterPeers = &v
	}
	if p.ClockSkew != 0 {
		// Skew is oto's clock minus the upstream's. `server_time` is reconstructed
		// from it so the operator can see both halves of the comparison.
		ms := p.ClockSkew.Milliseconds()
		out.ClockSkewMS = &ms
		st := p.CheckedAt.Add(-p.ClockSkew).UTC()
		out.ServerTime = &st
	}
	if p.PrometheusConfigured {
		ok := p.PrometheusReachable
		out.PrometheusOK = &ok
	}
	if p.ErrorMessage != "" {
		msg := p.ErrorMessage
		out.Error = &msg
	}
	return out
}

// sendResolvedFrom rebuilds the receiver→send_resolved map the contract asks for
// out of the probe's two observations.
//
// The probe records the AGGREGATE (`SendResolved`) plus the names of the
// receivers that have it disabled (`MutedReceivers`), because C15 only needs the
// offenders. The contract wants the map, so it is reconstructed: a muted receiver
// is `false`, and when nothing is muted the aggregate answers for the whole set.
func sendResolvedFrom(p service.ProbeResult) map[string]bool {
	if p.SendResolved == nil && len(p.MutedReceivers) == 0 {
		return nil
	}
	out := make(map[string]bool, len(p.MutedReceivers))
	for _, name := range p.MutedReceivers {
		out[name] = false
	}
	return out
}

// reconcileDTO maps a reconcile pass onto the wire.
func reconcileDTO(r domain.ReconcileResult) ReconcileResultDTO {
	out := ReconcileResultDTO{
		SourceID:           r.SourceID,
		OK:                 r.OK,
		StartedAt:          r.StartedAt.UTC(),
		FinishedAt:         r.FinishedAt.UTC(),
		Observed:           int32(r.Observed),           //nolint:gosec // an alert count
		SuppressedObserved: int32(r.SuppressedObserved), //nolint:gosec // an alert count
		Recovered:          int32(r.Recovered),          //nolint:gosec // an alert count
		MissingUpstream:    int32(r.MissingUpstream),    //nolint:gosec // an alert count
		DivergenceCount:    int32(r.DivergenceCount),    //nolint:gosec // an alert count
	}
	if r.Error != "" {
		msg := r.Error
		out.Error = &msg
	}
	return out
}

// ------------------------------------------------------------- request → domain

// toDraft maps a create request onto the domain command.
//
// The three defaults it applies — push, reconcile, ignore labels — are the DDL's
// own, restated because the contract publishes them and a client that omits a
// field expects the documented default rather than Go's zero value. Turning
// reconcile off by accident would mean `suppressed` can never be observed at all.
func (r CreateSourceRequest) toDraft(credentialID *uuid.UUID) domain.SourceDraft {
	d := domain.SourceDraft{
		ClusterID:         r.ClusterID,
		Name:              r.Name,
		Kind:              domain.Kind(r.Kind),
		BaseURL:           r.BaseURL,
		PrometheusURL:     r.PrometheusURL,
		AuthCredentialID:  credentialID,
		TLSSkipVerify:     boolOr(r.TLSSkipVerify, false),
		InjectLabels:      r.InjectLabels,
		IgnoreLabels:      r.IgnoreLabels,
		RedactLabels:      r.RedactLabels,
		RedactAnnotations: r.RedactAnnotations,
		PushEnabled:       boolOr(r.PushEnabled, true),
		ReconcileEnabled:  boolOr(r.ReconcileEnabled, true),
		ReconcileInterval: domain.DefaultReconcileInterval,
	}
	if r.IgnoreLabels == nil {
		d.IgnoreLabels = domain.DefaultIgnoreLabels()
	}
	if r.ReconcileIntervalSeconds != nil {
		d.ReconcileInterval = time.Duration(*r.ReconcileIntervalSeconds) * time.Second
	}
	return d
}

// toPatch maps an update request onto the domain command.
//
// `prometheus_url` is a NullableString because an explicit `null` and an omitted
// field mean different things: CLEAR versus LEAVE ALONE.
func (r UpdateSourceRequest) toPatch(credential **uuid.UUID) domain.SourcePatch {
	p := domain.SourcePatch{
		ClusterID:         r.ClusterID,
		Name:              r.Name,
		BaseURL:           r.BaseURL,
		AuthCredentialID:  credential,
		TLSSkipVerify:     r.TLSSkipVerify,
		InjectLabels:      r.InjectLabels,
		IgnoreLabels:      r.IgnoreLabels,
		RedactLabels:      r.RedactLabels,
		RedactAnnotations: r.RedactAnnotations,
		PushEnabled:       r.PushEnabled,
		ReconcileEnabled:  r.ReconcileEnabled,
	}
	switch {
	case r.PrometheusURL.Supplied():
		v := &r.PrometheusURL.Value
		p.PrometheusURL = &v
	case r.PrometheusURL.Cleared():
		var none *string
		p.PrometheusURL = &none
	}
	if r.ReconcileIntervalSeconds != nil {
		d := time.Duration(*r.ReconcileIntervalSeconds) * time.Second
		p.ReconcileInterval = &d
	}
	return p
}

// ------------------------------------------------------------------- helpers

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// nonNilSlice renders an absent list as `[]` and never `null`. A UI that has to
// null-check every array is a UI that will forget once.
func nonNilSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}
