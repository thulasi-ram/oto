package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpc"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// skewAlpha is the EWMA weight applied to a new clock-skew sample. Skew is
// measured and surfaced, never used to reject an event (C12), so the smoothing
// only has to be stable, not fast.
const skewAlpha = 0.25

// ProbeResult is the outcome of one source probe. It is what
// POST /api/v1/sources/{id}/test reports.
type ProbeResult struct {
	SourceID uuid.UUID
	// CheckedAt is oto's clock at the start of the probe.
	CheckedAt time.Time
	// Latency is how long the Alertmanager status call took.
	Latency time.Duration

	// Reachable reports whether GET /api/v2/status answered usefully.
	Reachable bool
	// AlertmanagerVersion gates notification_reason handling (AM >= 0.32.0).
	AlertmanagerVersion string
	// ClusterStatus is ready | settling | disabled.
	ClusterStatus string
	// ClusterPeers is how many HA peers the source sees. More than zero is why
	// ingest is at-least-once and why batch_dedup_key exists (C.5).
	ClusterPeers int
	// ResolveTimeout is global.resolve_timeout. Recorded, never used in
	// arithmetic: it is irrelevant for Prometheus-sourced alerts (research A5).
	ResolveTimeout time.Duration
	// SendResolved is nil when the config could not be parsed. nil means
	// UNKNOWN, and must never be collapsed to false.
	SendResolved *bool
	// MutedReceivers are the receivers with send_resolved disabled (C15).
	MutedReceivers []string
	// RouteTimings are the source's own group_wait, group_interval and
	// repeat_interval. Every field is nil until observed, and nil stays nil: oto
	// never substitutes Alertmanager's documented defaults (domain.RouteTimings).
	RouteTimings domain.RouteTimings
	// RouteTimingsObserved reports whether this probe actually read the running
	// configuration. It is FALSE for an unreachable source and for one whose
	// config would not parse, and it is what stops a failed probe from erasing
	// the last numbers oto did manage to observe.
	RouteTimingsObserved bool
	// ClockSkew is oto's clock minus the upstream's Date header.
	ClockSkew time.Duration

	// PrometheusConfigured reports whether prometheus_url is set. Without it,
	// RuleSnapshots can only ever carry the generatorURL expression.
	PrometheusConfigured bool
	// PrometheusReachable reports whether buildinfo answered.
	PrometheusReachable bool
	// PrometheusVersion is the paired Prometheus's version.
	PrometheusVersion string

	// Warnings are the structured operator warnings this probe produced.
	Warnings []domain.HealthWarning
	// ErrorCode is the stable code of the failure, "" on success. It is what
	// distinguishes "the source is down" (`alertmanager_unreachable`,
	// `alertmanager_timeout`) from "the source returned garbage"
	// (`alertmanager_malformed_response`, `alertmanager_unexpected_content_type`).
	ErrorCode string
	// ErrorMessage is the human, always-safe-to-render half.
	ErrorMessage string
}

// Failed reports whether the probe could not reach a usable Alertmanager.
func (p ProbeResult) Failed() bool { return !p.Reachable }

// Probe tests one source and records the result in source_health.
//
// This is what POST /api/v1/sources/{id}/test calls, and what the reconciler
// calls at the top of every pass (SPEC §G.8.1). It never returns an error for an
// unreachable source: "down" is a RESULT, not an exception — the caller needs
// the health projection updated either way, and a 502 from this endpoint would
// tell an operator nothing the ProbeResult does not.
//
// It returns an error only when the source or its credential cannot be loaded,
// or when the health projection cannot be written.
func (s *Service) Probe(ctx context.Context, scope db.TenantScope, id uuid.UUID) (ProbeResult, error) {
	src, err := s.source(ctx, scope, id)
	if err != nil {
		return ProbeResult{}, err
	}

	res := s.probeSource(ctx, scope, src)

	health, herr := s.repo.GetHealth(ctx, scope, src.ID)
	if herr != nil && !errs.IsKind(herr, errs.KindNotFound) {
		return res, herr
	}
	health = ApplyProbe(health, src, res)
	if err := s.repo.SaveHealth(ctx, scope, health); err != nil {
		return res, err
	}
	return res, nil
}

// probeSource performs the probe without touching persistence, so the health
// arithmetic can be tested separately from the I/O.
func (s *Service) probeSource(ctx context.Context, scope db.TenantScope, src domain.Source) ProbeResult {
	now := s.clk.Now()
	res := ProbeResult{
		SourceID:             src.ID,
		CheckedAt:            now,
		PrometheusConfigured: src.HasPrometheus(),
	}

	cred, err := s.credential(ctx, scope, src)
	if err != nil {
		return withError(res, err)
	}
	am, err := s.clients.Alertmanager(src, cred)
	if err != nil {
		return withError(res, errs.Wrap(err, errs.KindValidation, CodeClientFailed,
			"this source's connection settings are not usable"))
	}

	st, err := am.StatusDetail(ctx)
	if err != nil {
		return withError(res, err)
	}

	res.Reachable = true
	res.Latency = st.Latency
	res.AlertmanagerVersion = st.AM.Version
	res.ClusterStatus = st.AM.ClusterStatus
	res.ClusterPeers = st.AM.ClusterPeers
	res.ResolveTimeout = st.AM.ResolveTimeout

	if !st.ServerTimeUnknown() {
		res.ClockSkew = now.Sub(st.AM.ServerTime)
		if abs(res.ClockSkew) > domain.MaxTolerableSkew {
			res.Warnings = append(res.Warnings, domain.HealthWarning{
				Code:    domain.WarnClockSkew,
				Message: "oto's clock and this Alertmanager's differ by more than " + domain.MaxTolerableSkew.String(),
				At:      now,
			})
		}
	}

	switch {
	case !st.ConfigParsed:
		res.Warnings = append(res.Warnings, domain.HealthWarning{
			Code:    domain.WarnConfigUnparseable,
			Message: "the running configuration could not be read, so send_resolved is unknown",
			Subject: st.ConfigError,
			At:      now,
		})
	default:
		// C15: a receiver with send_resolved:false silently turns every alert
		// into an expiry, because oto will never be told it resolved. That is a
		// persistent warning, not a one-off log line.
		anySends := len(st.AM.SendResolved) == 0
		for name, sends := range st.AM.SendResolved {
			if sends {
				anySends = true
				continue
			}
			res.MutedReceivers = append(res.MutedReceivers, name)
			res.Warnings = append(res.Warnings, domain.HealthWarning{
				Code:    domain.WarnSendResolvedFalse,
				Message: "this receiver has send_resolved disabled: alerts routed to it will expire rather than resolve",
				Subject: name,
				At:      now,
			})
		}
		res.SendResolved = &anySends

		// The source's own batching timings, straight off the config it just
		// handed over. They are recorded even when all three are absent: "we read
		// the configuration and it states none of them" is a genuine observation,
		// and it is the observation that tells an operator their Alertmanager is
		// running on built-in defaults oto cannot see.
		res.RouteTimings = st.AM.RouteTimings
		res.RouteTimingsObserved = true
	}

	if st.ClusterName != "" && st.AM.ClusterStatus != "" && st.AM.ClusterStatus != "ready" {
		res.Warnings = append(res.Warnings, domain.HealthWarning{
			Code:    domain.WarnClusterNotReady,
			Message: "the Alertmanager cluster is not ready",
			Subject: st.AM.ClusterStatus,
			At:      now,
		})
	}

	s.probePrometheus(ctx, src, cred, now, &res)
	return res
}

// probePrometheus checks the paired Prometheus, if there is one. Its failure is
// a WARNING, never a probe failure: an Alertmanager without a reachable
// Prometheus still ingests alerts perfectly, it just cannot enrich a RuleSnapshot
// beyond the generatorURL expression.
func (s *Service) probePrometheus(ctx context.Context, src domain.Source, cred domain.Credential, now time.Time, res *ProbeResult) {
	if !src.HasPrometheus() {
		res.Warnings = append(res.Warnings, domain.HealthWarning{
			Code:    domain.WarnPrometheusUnconfigured,
			Message: "no Prometheus URL, so rule snapshots are limited to the expression in generatorURL",
			At:      now,
		})
		return
	}

	pc, err := s.clients.Prometheus(src, cred, "")
	if err != nil {
		res.Warnings = append(res.Warnings, domain.HealthWarning{
			Code:    domain.WarnPrometheusUnreachable,
			Message: "the Prometheus connection settings are not usable",
			Subject: src.PrometheusURL,
			At:      now,
		})
		return
	}
	info, err := pc.BuildInfo(ctx)
	if err != nil {
		res.Warnings = append(res.Warnings, domain.HealthWarning{
			Code:    domain.WarnPrometheusUnreachable,
			Message: "the paired Prometheus did not answer; rule snapshots will fall back to generatorURL",
			Subject: errs.CodeOf(err),
			At:      now,
		})
		return
	}
	res.PrometheusReachable = true
	res.PrometheusVersion = info.Version
}

// withError stamps a failure onto a probe result. The distinction it preserves
// is the one that matters operationally: unreachable versus malformed.
func withError(res ProbeResult, err error) ProbeResult {
	res.Reachable = false
	res.ErrorCode = errs.CodeOf(err)
	if e, ok := errs.As(err); ok {
		res.ErrorMessage = e.Message
	} else {
		res.ErrorMessage = "the source could not be probed"
	}

	code := domain.WarnAlertmanagerUnreachable
	msg := "the Alertmanager did not answer"
	if httpc.IsMalformed(err) {
		code = domain.WarnAlertmanagerMalformed
		msg = "the Alertmanager answered with something oto cannot parse; check that base_url points at an Alertmanager API root"
	}
	res.Warnings = append(res.Warnings, domain.HealthWarning{
		Code:    code,
		Message: msg,
		Subject: res.ErrorCode,
		At:      res.CheckedAt,
	})
	return res
}

// ApplyProbe folds a ProbeResult into the health projection.
//
// It is a pure function of (old health, source, result) so that the state
// machine — three consecutive failures means unreachable, which blocks the
// reaper — is testable without a database or a clock.
func ApplyProbe(h domain.SourceHealth, src domain.Source, res ProbeResult) domain.SourceHealth {
	h.SourceID = src.ID
	h.OrgID = src.OrgID
	h.UpdatedAt = res.CheckedAt
	h.Warnings = res.Warnings

	if !res.Reachable {
		h.ConsecutiveFailures++
		h.LastError = res.ErrorCode
		if h.LastError == "" {
			// source_health_error_ck: an unreachable row MUST carry an error.
			h.LastError = "unknown_error"
		}
		if h.ConsecutiveFailures >= domain.UnreachableAfterFailures {
			h.Status = domain.HealthUnreachable
		} else {
			h.Status = domain.HealthDegraded
		}
		return h
	}

	h.ConsecutiveFailures = 0
	h.LastError = ""
	h.AMVersion = res.AlertmanagerVersion
	h.SendResolved = res.SendResolved

	// ⚠️ THE TIMINGS ARE REPLACED ONLY BY A PROBE THAT ACTUALLY READ THEM. A probe
	// that reached the source but could not parse its configuration leaves the
	// last observed set — and the timestamp beside it — exactly where they were,
	// so the screen shows an honestly stale reading rather than blanking three
	// numbers because of one bad parse. When they ARE read, they are replaced
	// WHOLESALE, absences included: a key removed from `alertmanager.yml` must
	// become unknown again, not linger as the last value oto happened to see.
	if res.RouteTimingsObserved {
		h.RouteTimings = res.RouteTimings
		at := res.CheckedAt.UTC()
		h.RouteTimingsAt = &at
	}

	if res.ClockSkew != 0 {
		h.ClockSkew = ewma(h.ClockSkew, res.ClockSkew)
	}

	h.Status = domain.HealthHealthy
	for _, w := range res.Warnings {
		if _, degrades := degradingWarnings[w.Code]; degrades {
			h.Status = domain.HealthDegraded
			break
		}
	}
	return h
}

// degradingWarnings are the warnings that make oto's VIEW of a source
// untrustworthy, and therefore move it out of healthy and block the reaper
// (SPEC §B.4). The set is deliberately tiny, and the exclusions matter more than
// the inclusions:
//
//   - send_resolved_false does NOT degrade. When a receiver never sends
//     resolutions, expiry is the only ending an alert can ever have — blocking
//     the reaper there would strand every one of those alerts as `firing`
//     forever, which is the exact opposite of what C15 is warning about.
//   - clock_skew does NOT degrade. Skew is measured and surfaced, never used to
//     reject or delay anything (C12).
//   - alertmanager_config_unparseable does NOT degrade. It costs oto knowledge
//     of send_resolved, not sight of the alerts.
//   - prometheus_* do NOT degrade. A missing or dead Prometheus costs rule
//     enrichment, not alert visibility; a source with no prometheus_url at all
//     is a completely healthy source and must never be permanently degraded.
//
// A settling or disabled HA cluster is different in kind: it can return an
// incomplete alert set, so oto's view really is untrustworthy.
var degradingWarnings = map[string]struct{}{
	domain.WarnClusterNotReady: {},
}

// ewma folds a new sample into an exponentially weighted moving average.
func ewma(old, sample time.Duration) time.Duration {
	if old == 0 {
		return sample
	}
	return time.Duration(float64(old)*(1-skewAlpha) + float64(sample)*skewAlpha)
}

// abs is time.Duration.Abs, spelled out for clarity at the call site.
func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
