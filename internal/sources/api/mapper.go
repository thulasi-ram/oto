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
		RouteTimings:        routeTimingsDTO(h),
		Warnings:            warnings,
		UpdatedAt:           h.UpdatedAt.UTC(),
	}
}

// routeTimingsDTO renders what governs this source's batching, and how oto knows.
//
// ⛔ THE DERIVATION HAPPENS HERE, ON READ, AND NOWHERE ELSE. `source_health`
// stores only what was observed — NULL for a key the configuration did not state
// — and `domain.RouteTimings.Resolve` turns that absence into `default_applies`
// with the documented value beside it. Storing the derived number would fuse
// "stated 5m" and "stated nothing" into one row forever, and would need a
// backfill the day Alertmanager moves a default instead of a deploy.
//
// `h.RouteTimingsAt` is the load-bearing input: it is the ONLY record of whether
// oto has ever managed to read this source's configuration, and therefore the
// only thing that separates `unknown` from `default_applies`.
func routeTimingsDTO(h domain.SourceHealth) RouteTimingsDTO {
	parsed := h.RouteTimingsAt != nil

	// ⭐ WHICH ROUTE THE THREE HEADLINE NUMBERS DESCRIBE. The top-level route is
	// the fallback, not the answer: these settings are per-route and inherited,
	// so the numbers governing the alerts OTO is sent are the ones on the route
	// delivering to oto's own receiver. They are used when — and only when — oto
	// could name that receiver AND every route reaching it agrees on all three.
	//
	// ⛔ A DISAGREEMENT IS NOT RESOLVED HERE OR ANYWHERE. Two routes can reach one
	// receiver with different timings (`continue: true`), and picking one — first
	// match, slowest, an average — would be a number oto invented, which is the
	// exact failure the hand-typed tuning form was replaced for. The headline
	// falls back to the top-level route, `RoutesAgree` goes false, and `Routes`
	// carries the conflict for the client to show.
	timings, which := h.RouteTimings, RouteTopLevel
	reaching := h.Routes.ForOto()
	if gov, ok := domain.Governing(reaching); ok {
		t := gov.Timings()
		// The child counts describe the TREE and survive the swap unchanged; they
		// are not a property of whichever route won.
		t.ChildRoutes, t.ChildrenWithTimings = h.RouteTimings.ChildRoutes, h.RouteTimings.ChildrenWithTimings
		timings, which = t, RouteOtoReceiver
	}
	resolved := timings.Resolve(parsed, h.AMVersion)

	basis := h.Routes.Basis
	if basis == "" || !parsed {
		// No probe has ever read this configuration, so there is not even a
		// receiver list to reason about. `unknown` is that state, and it is the
		// only basis this mapper synthesises.
		basis = domain.ReceiverUnknown
	}

	out := RouteTimingsDTO{
		GroupWait:              routeTimingDTO(resolved.GroupWait),
		GroupInterval:          routeTimingDTO(resolved.GroupInterval),
		RepeatInterval:         routeTimingDTO(resolved.RepeatInterval),
		Route:                  which,
		ChildRoutes:            int32(resolved.ChildRoutes),         //nolint:gosec // bounded by source_health_am_timings_ck
		ChildRoutesWithTimings: int32(resolved.ChildrenWithTimings), //nolint:gosec // bounded by source_health_am_timings_ck
		ReceiverBasis:          string(basis),
		WebhookReceivers:       stringsOrEmpty(h.Routes.WebhookReceivers),
		Routes:                 receiverRoutesDTO(h.Routes),
		RoutesAgree:            domain.Agree(reaching),
		RoutesDropped:          int32(max(h.Routes.Dropped, 0)), //nolint:gosec // capped by domain.MaxResolvedRoutes
		DefaultsVerified:       resolved.DefaultsVerified,
		ObservedAt:             utcPtr(h.RouteTimingsAt),
	}
	if h.Routes.Receiver != "" {
		r := h.Routes.Receiver
		out.Receiver = &r
	}
	if resolved.DefaultsFromVersion != "" {
		v := resolved.DefaultsFromVersion
		out.DefaultsFromVersion = &v
	}
	return out
}

// receiverRoutesDTO renders every delivering route in the tree.
//
// It always returns a non-nil slice: an empty list is "oto read the config and it
// declares no delivering route", and a client that had to tell `null` from `[]`
// to know that would be doing the mapper's job.
func receiverRoutesDTO(res domain.RouteResolution) []ReceiverRouteDTO {
	out := make([]ReceiverRouteDTO, 0, len(res.Routes))
	for _, r := range res.Routes {
		out = append(out, ReceiverRouteDTO{
			Receiver:       r.Receiver,
			Path:           routeStepsDTO(r.Path),
			GroupWait:      inheritedTimingDTO(r.GroupWait, domain.DefaultGroupWait),
			GroupInterval:  inheritedTimingDTO(r.GroupInterval, domain.DefaultGroupInterval),
			RepeatInterval: inheritedTimingDTO(r.RepeatInterval, domain.DefaultRepeatInterval),
			GroupBy:        stringsOrEmpty(r.GroupBy),
			GroupByAll:     r.GroupByAll,
			// A route reaches oto only when oto actually named a receiver. With
			// `ambiguous` this is false everywhere, which is the honest rendering:
			// the whole list is shown and none of it is claimed.
			ReachesOto:  res.Receiver != "" && r.Receiver == res.Receiver && !r.Shadowed,
			Unreachable: r.Shadowed,
		})
	}
	return out
}

// routeStepsDTO renders one route's matcher path.
func routeStepsDTO(path []domain.RouteStep) []RouteStepDTO {
	out := make([]RouteStepDTO, 0, len(path))
	for _, s := range path {
		out = append(out, RouteStepDTO{
			Matchers:   stringsOrEmpty(s.Matchers),
			Deprecated: s.Deprecated,
			Continue:   s.Continue,
		})
	}
	return out
}

// inheritedTimingDTO renders one per-route timing.
//
// The provenance rule is the SAME as for the headline three and deliberately so:
// a value stated anywhere on the path is `observed`, and a value stated nowhere
// on it is `default_applies` carrying Alertmanager's documented number. There is
// no `unknown` here — a route is in this list only because oto read the
// configuration that declares it, so "we could not look" cannot arise.
func inheritedTimingDTO(t domain.InheritedTiming, fallback time.Duration) InheritedTimingDTO {
	if !t.Stated() {
		ms := fallback.Milliseconds()
		return InheritedTimingDTO{
			Provenance: string(domain.TimingDefaultApplies),
			ValueMS:    &ms,
		}
	}
	ms := t.Value.Milliseconds()
	out := InheritedTimingDTO{Provenance: string(domain.TimingObserved), ValueMS: &ms}
	if t.FromDepth >= 0 {
		d := int32(t.FromDepth) //nolint:gosec // bounded by alertmanager.MaxRouteDepth
		out.FromDepth = &d
	}
	return out
}

// stringsOrEmpty keeps a JSON array an array. A nil slice marshals to `null`, and
// a client forced to branch on null-versus-empty for a list of matchers is a
// client with a bug waiting in it.
func stringsOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// routeTimingDTO renders one provenanced timing. An `unknown` field carries NO
// number: this is the boundary that must not invent one.
func routeTimingDTO(t domain.RouteTiming) RouteTimingDTO {
	out := RouteTimingDTO{Provenance: string(t.Provenance)}
	if t.Known() {
		ms := t.Value.Milliseconds()
		out.ValueMS = &ms
	}
	return out
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

// rejectionDTO maps one refused element onto the wire.
//
// ⛔ THE LABEL MAP IS NEVER NIL. `{}` and `null` mean different things to the
// screen: the empty set is the honest answer for a rejection with no alert to
// name — an undecodable body, an unknown source, a truncated batch — and a
// `null` would read as "we do not know", which is the one thing this feed is
// never allowed to say.
func rejectionDTO(e RejectionEntry) RejectionDTO {
	return RejectionDTO{
		ID:         e.ID,
		SourceID:   e.SourceID,
		BatchID:    e.BatchID,
		ReceivedAt: e.ReceivedAt.UTC(),
		Reason:     e.Reason,
		Detail:     e.Detail,
		Labels:     nonNilMap(e.Labels),
	}
}

// failedBatchDTO maps one unprocessed batch onto the wire.
func failedBatchDTO(b BatchFailure) FailedBatchDTO {
	return FailedBatchDTO{
		ID:              b.ID,
		SourceID:        b.SourceID,
		Mode:            b.Mode,
		ReceivedAt:      b.ReceivedAt.UTC(),
		Status:          b.Status,
		ProcessedAt:     utcPtr(b.ProcessedAt),
		Error:           b.Error,
		AlertCount:      int32(b.AlertCount),      //nolint:gosec // an alert count in one batch
		TruncatedAlerts: int32(b.TruncatedAlerts), //nolint:gosec // an alert count in one batch
	}
}

// ------------------------------------------------------------- request → domain

// toInput maps the write-only credential DTO onto the service's plain-typed
// input.
//
// ⭐ A NIL DTO STAYS NIL, and the difference is load-bearing on an update: an
// absent `credential` means LEAVE THE SECRET ALONE, while `{"kind":"none"}`
// means detach it. Collapsing the two would silently unauthenticate a source
// that was only being renamed.
//
// ⛔ The values are secret material and pass straight through. Nothing here
// copies, logs or echoes them.
func (c *CredentialInputDTO) toInput() *service.CredentialInput {
	if c == nil {
		return nil
	}
	return &service.CredentialInput{Kind: c.Kind, Values: c.Values}
}

// toDraft maps a create request onto the domain command.
//
// The two defaults it applies — push and ignore labels — are the DDL's own,
// restated because the contract publishes them and a client that omits a field
// expects the documented default rather than Go's zero value. Reconciliation is
// not among them: it is not defaulted on, it is simply on (ADR 0006).
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
