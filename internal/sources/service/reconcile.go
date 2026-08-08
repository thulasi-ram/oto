package service

// `source.reconcile` — SPEC §G.8, ADR 0006. MANDATORY, not optional.
//
// ⭐⭐ WHY THIS FILE EXISTS AT ALL.
//
// Alertmanager's notification pipeline runs `MuteStage` BEFORE `RetryStage`, and
// `MuteStage` *drops* muted alerts from the slice that continues down the
// pipeline. A silenced, inhibited or muted alert is therefore never delivered to
// a webhook — at all — and the webhook `status` enum carries only
// `firing | resolved`. From a webhook's point of view "somebody silenced this",
// "this resolved and stopped arriving" and "Alertmanager died" are the SAME
// observation: silence.
//
// So the ONLY way oto can learn that an alert is suppressed is to poll
// `GET /api/v2/alerts` and read `status.state` with `silencedBy` / `inhibitedBy`
// / `mutedBy`. That is this file, and it is why `suppressed` has exactly one
// producer in the whole system.
//
// ⛔ IT IS NOT A SECOND INGESTION PATH (C18). It produces `Observation`s and
// hands them to `alerts/service.ObserveBatch`, the same method the webhook path
// uses. There remains exactly one write path into `alerts`.
//
// ⚠️ PLACEMENT. SPEC §I.2's tree draws this as
// `internal/ingestion/worker/reconcile_source`. It is implemented here instead,
// because every collaborator it needs — the AM v2 client factory, the credential
// store, `alert_sources`, `source_health`, the probe that already implements
// §G.8.1 — is owned by `sources`, and because ADR 0006 is explicit that the
// reconciler is not an ingestion mode. The seam is unchanged either way:
// `sources/api.Reconciler` is the port, `internal/app` injects the concrete.

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	alertsvc "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/sources/client/alertmanager"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// Error codes the reconciler mints.
const (
	// CodeReconcileUnwired means a collaborator the pass cannot work without is
	// absent. It is deliberately loud: a reconciler that silently no-ops makes an
	// entire alert state unreachable.
	CodeReconcileUnwired = "sources_reconciler_unwired"
	// CodeReconcileFailed means the pass could not complete for a reason that is
	// oto's, not the upstream's. An UNREACHABLE UPSTREAM IS NOT THIS — that is a
	// successful pass with `ok: false`.
	CodeReconcileFailed = "sources_reconcile_failed"
)

// `source_health.last_reconcile_status`. Two values, because an operator reading
// the row needs to know whether the divergence count beside it is fresh.
const (
	reconcileOK     = "ok"
	reconcileFailed = "failed"
)

// Bounds on ONE pass. Every one of them exists because an unbounded pass is an
// outage the first time somebody has a bad night.
const (
	// MaxUpstreamAlerts caps how many alerts one pass will normalise. Beyond it
	// the excess is dropped with a warning rather than the pass failing: 20 000
	// alerts recorded beats zero recorded.
	MaxUpstreamAlerts = 20_000
	// ObserveChunkSize is how many Observations go into one state-machine
	// transaction. It mirrors the ingest path's chunk size for the same reason: a
	// single transaction holding twenty thousand upserts blocks vacuum and bloats
	// WAL.
	ObserveChunkSize = 500
	// MaxDivergenceScan bounds the oto-side read of §G.8.4. Divergence is a
	// COUNTER, not a work list, so an approximate count over a bounded scan is
	// worth more than an exact one that scans the whole table.
	MaxDivergenceScan = 5_000
	// divergenceScanPage is one keyset page of that scan.
	divergenceScanPage = 500
	// FanOutLimit bounds how many sources one tenant contributes to one fan-out
	// tick. A deployment with a thousand sources must not turn one tick into a
	// thousand simultaneous outbound calls.
	FanOutLimit = 200
)

// redactedValue mirrors `ingestion/decode.RedactedValue`.
//
// ⚠️ IT IS DUPLICATED ON PURPOSE. `sources` may not import `ingestion/decode`
// (depguard, sources-must-not-reach-into-other-domains), and the reconciler MUST
// apply the same `redact_labels` / `redact_annotations` patterns the webhook path
// applies — otherwise a value an operator redacted out of the push path would
// walk straight back in through the pull path. The two implementations are eleven
// lines each and are cross-referenced in both directions.
const redactedValue = "[redacted]"

// Reconciler runs one reconcile pass per AlertSource, and the periodic fan-out
// that schedules them.
//
// It is a separate type from Service rather than more methods on it because it is
// constructed LATER: it needs the alerts state machine, and `sources` is built
// before `alerts` in the composition root. Keeping it separate makes that
// ordering a compile-time fact rather than a late-bound pointer.
type Reconciler struct {
	sources  *Service
	alerts   AlertObserver
	read     AlertReader
	clusters ClusterReader
	orgs     TenantLister
	enq      db.Enqueuer
	clk      clock.Clock
	log      *slog.Logger
}

// ReconcilerOptions are the Reconciler's dependencies. Everything is a port.
type ReconcilerOptions struct {
	// Sources is the read side: the probe, the AM client factory, `alert_sources`
	// and `source_health`.
	Sources *Service
	// Alerts is the one write path into `alerts` — the ingest orchestrator, so a
	// recovered alert joins a group and earns a notification like any other.
	Alerts AlertObserver
	// Read is the alert list, for divergence accounting only. Optional: without it
	// the pass still observes, it just reports zero divergence.
	Read AlertReader
	// Clusters resolves `cluster_key`, which participates in alert identity.
	Clusters ClusterReader
	// Orgs enumerates tenants for the fan-out. Optional: without it the
	// Reconciler still serves forced passes, it just schedules none.
	Orgs TenantLister
	// Enqueuer schedules the per-source jobs. Optional, as above.
	Enqueuer db.Enqueuer
	Clock    clock.Clock
	Logger   *slog.Logger
}

// NewReconciler builds the Reconciler, refusing a dependency set that cannot work.
//
// ⛔ IT REFUSES RATHER THAN DEGRADES. A reconciler missing its state machine
// would poll an Alertmanager, read suppression out of it, and throw the answer
// away — which looks exactly like a healthy deployment and is exactly the failure
// ADR 0006 exists to prevent.
func NewReconciler(o ReconcilerOptions) (*Reconciler, error) {
	switch {
	case o.Sources == nil:
		return nil, errs.New(errs.KindInternal, CodeReconcileUnwired, "the reconciler requires the sources service")
	case o.Alerts == nil:
		return nil, errs.New(errs.KindInternal, CodeReconcileUnwired, "the reconciler requires the alerts state machine")
	case o.Clusters == nil:
		return nil, errs.New(errs.KindInternal, CodeReconcileUnwired, "the reconciler requires a cluster reader")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Reconciler{
		sources:  o.Sources,
		alerts:   o.Alerts,
		read:     o.Read,
		clusters: o.Clusters,
		orgs:     o.Orgs,
		enq:      o.Enqueuer,
		clk:      clk,
		log:      lg,
	}, nil
}

// Reconcile runs ONE pass against one source. It is `POST
// /api/v1/sources/{id}/reconcile` and the body of the `source.reconcile` job.
//
// ⭐⭐ THE TWO RULES THIS METHOD EXISTS TO ENFORCE:
//
//  1. AN UNREACHABLE SOURCE HOLDS. IT DOES NOT EXPIRE ANYTHING. A pass that
//     cannot see the upstream records the failure, increments
//     `consecutive_failures`, and returns having observed nothing. At three
//     failures the source is `unreachable`, which BLOCKS THE REAPER (§B.4) — so
//     an outage cannot mass-expire a customer's live alerts. Reconciliation is
//     the only thing in oto that can look at a whole cluster's worth of alerts at
//     once, which makes it the only thing that could get this catastrophically
//     wrong.
//
//  2. IT NEVER EXPIRES ANYTHING ITSELF, EVEN WHEN THE SOURCE IS HEALTHY. Alerts
//     open in oto and absent upstream are COUNTED as `MissingUpstream` and left
//     entirely alone. They age out through `occurrence.reap`, which applies
//     `resolve_grace` and re-checks source health at the moment of expiry (§G.8.4,
//     T6). There is no code path in this file that writes a terminal state, and
//     there must never be one.
//
// It returns an error ONLY when the pass could not be attempted — an unknown
// source, an unreadable cluster key, an unwritable health row. "The upstream is
// down" is a RESULT (`ok: false`), not an exception: the caller needs the health
// projection updated either way, and a 502 here would tell an operator nothing
// the ReconcileResult does not.
func (r *Reconciler) Reconcile(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID,
) (domain.ReconcileResult, error) {
	started := r.clk.Now().UTC()
	out := domain.ReconcileResult{SourceID: sourceID, StartedAt: started, FinishedAt: started}

	src, err := r.sources.source(ctx, scope, sourceID)
	if err != nil {
		return out, err
	}

	// The health row this pass folds into. A failed READ is fatal to the pass:
	// starting from a zero value would reset `consecutive_failures`, and resetting
	// that counter is how a source that has been unreachable for ten minutes
	// silently becomes eligible for the reaper again.
	health, err := r.sources.repo.GetHealth(ctx, scope, sourceID)
	if err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return out, errs.Wrap(err, errs.KindUnavailable, CodeReconcileFailed,
			"this source's health could not be read, so the pass was not attempted")
	}

	// §G.8.1 — status first. It is what records `am_version`, the effective
	// `send_resolved` per receiver (C15) and the clock skew read off the HTTP
	// `Date` header (C12), and it is what decides `Reachable`.
	probe := r.sources.probeSource(ctx, scope, src)

	// §G.8.2 — the authoritative current world. All four booleans TRUE: the
	// silenced and inhibited ones are the entire point.
	var upstream []domain.GettableAlert
	if probe.Reachable {
		upstream, err = r.sources.Alerts(ctx, scope, sourceID, domain.AlertFilter{
			Active: true, Silenced: true, Inhibited: true, Unprocessed: true,
		})
		if err != nil {
			// A status call that answered and an alert call that did not is still a
			// source oto cannot see. Fold it into the SAME failure arithmetic, so it
			// counts towards `unreachable` and therefore towards blocking the reaper.
			probe = withError(probe, err)
		}
	}

	health = ApplyProbe(health, src, probe)

	if !probe.Reachable {
		// ⛔⛔ THE HOLD. Nothing observed, nothing expired, and `divergence_count`
		// is LEFT AT ITS PREVIOUS VALUE rather than zeroed — a stale divergence
		// count is honest, a fabricated zero is a lie about a source oto cannot see.
		out.FinishedAt = r.clk.Now().UTC()
		out.Error = probe.ErrorMessage
		out.DivergenceCount = health.DivergenceCount
		r.stamp(&health, out.FinishedAt, reconcileFailed)
		if serr := r.sources.repo.SaveHealth(ctx, scope, health); serr != nil {
			return out, errs.Wrap(serr, errs.KindUnavailable, CodeReconcileFailed,
				"the source's health could not be recorded")
		}
		r.log.WarnContext(ctx, "sources: reconcile held, source not reachable",
			"source_id", src.ID, "org_id", scope.OrgID(),
			"code", probe.ErrorCode, "consecutive_failures", health.ConsecutiveFailures,
			"status", string(health.Status), "blocks_reaper", health.BlocksReaper())
		return out, nil
	}

	// `cluster_key` is hashed into every `alert_key` (§C.2). Without it the pass
	// would mint identities that disagree with the webhook path's, so it fails
	// rather than guesses.
	cluster, err := r.clusters.Get(ctx, scope, src.ClusterID)
	if err != nil {
		return out, errs.Wrap(err, errs.KindUnavailable, CodeReconcileFailed,
			"this source's cluster could not be read, so alert identity cannot be computed")
	}
	clusterKey, err := alerts.NewClusterKey(cluster.Key)
	if err != nil {
		return out, errs.Wrap(err, errs.KindInternal, CodeReconcileFailed,
			"this source's cluster key is not usable for alert identity")
	}

	if len(upstream) > MaxUpstreamAlerts {
		r.log.WarnContext(ctx, "sources: reconcile truncated an oversized upstream alert set",
			"source_id", src.ID, "returned", len(upstream), "kept", MaxUpstreamAlerts)
		upstream = upstream[:MaxUpstreamAlerts]
	}

	// §G.8.4 — oto's belief is read BEFORE the observations are applied, because
	// "recovered" means "we had not seen this and now we have". Reading it
	// afterwards would show every recovery as agreement.
	before := r.openAlertKeys(ctx, scope, clusterKey)

	obs, rejected := r.observations(ctx, scope, src, clusterKey, upstream)
	out.Observed = len(obs)
	for _, o := range obs {
		if o.Status == statusSuppressed {
			out.SuppressedObserved++
		}
	}
	if rejected > 0 {
		r.log.WarnContext(ctx, "sources: reconcile skipped alerts that failed their bounds",
			"source_id", src.ID, "skipped", rejected)
	}

	err = r.apply(ctx, scope, obs)
	if err != nil {
		// RETRYABLE, and deliberately NOT recorded as an upstream failure: oto
		// could see the source perfectly well. Leaving `consecutive_failures`
		// alone here matters — inflating it on oto's own database trouble would
		// block the reaper for a reason that has nothing to do with visibility.
		out.FinishedAt = r.clk.Now().UTC()
		return out, err
	}

	upstreamKeys := make(map[string]struct{}, len(obs))
	for _, o := range obs {
		upstreamKeys[o.AlertKey.String()] = struct{}{}
	}
	for k := range upstreamKeys {
		if _, had := before[k]; !had {
			// Present upstream, absent in oto: a webhook was missed and this pass
			// has just repaired it. THIS IS THE RECOVERY PATH ADR 0006 PROMISES.
			out.Recovered++
		}
	}
	for k := range before {
		if _, still := upstreamKeys[k]; !still {
			// Open in oto, absent upstream. A CANDIDATE FOR EXPIRY AND NOTHING
			// MORE — see rule 2 on this method.
			out.MissingUpstream++
		}
	}
	out.DivergenceCount = out.Recovered + out.MissingUpstream
	out.OK = true
	out.FinishedAt = r.clk.Now().UTC()

	// ApplyProbe has already folded this pass's `Date`-header skew into the EWMA
	// (C12). Skew is measured and surfaced, never used to reject an observation.
	health.DivergenceCount = out.DivergenceCount
	r.stamp(&health, out.FinishedAt, reconcileOK)
	if err := r.sources.repo.SaveHealth(ctx, scope, health); err != nil {
		return out, errs.Wrap(err, errs.KindUnavailable, CodeReconcileFailed,
			"the source's health could not be recorded")
	}

	if out.DivergenceCount > 0 {
		// `oto_reconcile_divergence` is the canary for every correctness bug in
		// the system (§G.8.4), so a non-zero pass is never silent.
		r.log.InfoContext(ctx, "sources: reconcile divergence",
			"source_id", src.ID, "org_id", scope.OrgID(),
			"observed", out.Observed, "suppressed", out.SuppressedObserved,
			"recovered", out.Recovered, "missing_upstream", out.MissingUpstream)
	}
	return out, nil
}

// stamp records that a pass happened, on oto's clock.
func (r *Reconciler) stamp(h *domain.SourceHealth, at time.Time, status string) {
	when := at.UTC()
	h.LastReconcileAt = &when
	h.LastReconcileStatus = status
	h.UpdatedAt = when
}

// apply hands the Observations to the one write path into `alerts`, in bounded
// transactions.
//
// A chunk that fails aborts the pass and is retried by the job's own budget
// (§G.6). It is safe to retry from the start: the alert upsert is ON CONFLICT and
// every event is claimed through `alert_event_keys` first, so a re-applied chunk
// writes nothing twice.
func (r *Reconciler) apply(ctx context.Context, scope db.TenantScope, obs []alerts.Observation) error {
	for start := 0; start < len(obs); start += ObserveChunkSize {
		end := min(start+ObserveChunkSize, len(obs))
		if _, err := r.alerts.ObserveBatch(ctx, scope, obs[start:end]); err != nil {
			return errs.Wrap(err, errs.KindUnavailable, CodeReconcileFailed,
				"the observations from this pass could not be applied")
		}
	}
	return nil
}

// openAlertKeys reads the alert identities oto currently believes are live in
// this source's cluster.
//
// ⚠️ IT IS SCOPED BY CLUSTER, NOT BY SOURCE, and that is the honest scope: an
// Alert belongs to a `(org, cluster)` by §C.2, HA replicas of one Alertmanager
// are several Sources sharing one Cluster, and every replica reports the same
// alert set. Scoping tighter would report every replica's whole world as
// divergent.
//
// A failure to read is NOT a failure of the pass. Divergence accounting is
// observability; the observations above it are the correctness. An empty map
// makes the pass report zero divergence, which is visibly wrong in the metric and
// harmless everywhere else.
func (r *Reconciler) openAlertKeys(
	ctx context.Context, scope db.TenantScope, clusterKey alerts.ClusterKey,
) map[string]struct{} {
	out := make(map[string]struct{}, 128)
	if r.read == nil {
		return out
	}
	q := alertsvc.ListQuery{
		Filter: alerts.AlertFilter{
			// firing AND suppressed: a suppressed alert is still live upstream, and
			// counting it as missing would make every silence look like a divergence.
			States:      []alerts.State{alerts.StateFiring, alerts.StateSuppressed},
			ClusterKeys: []string{clusterKey.String()},
		},
		Page: db.Keyset{Limit: divergenceScanPage},
	}

	for len(out) < MaxDivergenceScan {
		res, err := r.read.List(ctx, scope, q)
		if err != nil {
			r.log.WarnContext(ctx, "sources: divergence accounting skipped, alert list unavailable",
				"cluster_key", clusterKey.String(), "error", err)
			return out
		}
		for _, a := range res.Alerts {
			out[a.Key().String()] = struct{}{}
		}
		if !res.Cursor.HasMore || len(res.Alerts) == 0 {
			return out
		}
		q.Page.Cursor = res.Cursor
	}
	r.log.WarnContext(ctx, "sources: divergence scan hit its bound; the count is a floor",
		"cluster_key", clusterKey.String(), "scanned", len(out))
	return out
}

// The upstream `status.state` values, and the two the state machine reads.
//
// §G.8.3: `suppressed` drives T3, `active` drives T4 when the episode is
// suppressed and T2 otherwise. `unprocessed` means Alertmanager has the alert but
// has not routed it yet, which is a firing alert oto has simply seen early.
const (
	upstreamSuppressed = "suppressed"
	statusSuppressed   = "suppressed"
	statusFiring       = "firing"
)

// observations normalises one upstream alert set into §B.3 inputs.
//
// ⭐ ONE BAD ALERT MUST NOT COST THE PASS. An alert that fails its bounds is
// counted and skipped; the rest are observed. The alternative — aborting — means
// one 9 KiB label value hides a customer's entire cluster.
//
// ⛔ NOTHING HERE EVER PRODUCES `resolved`. A resolution requires an explicit
// upstream `status="resolved"`, and `GET /api/v2/alerts` returns only what is
// currently active; absence is absence, and absence is `expired`'s business, not
// this function's (§B.2).
func (r *Reconciler) observations(
	ctx context.Context, scope db.TenantScope, src domain.Source,
	clusterKey alerts.ClusterKey, upstream []domain.GettableAlert,
) ([]alerts.Observation, int) {
	now := r.clk.Now().UTC()
	groups := r.groupLabels(ctx, scope, src)
	out := make([]alerts.Observation, 0, len(upstream))
	rejected := 0

	for _, a := range upstream {
		labels, err := alerts.NewLabelSet(boundLabels(a.Labels, src))
		if err != nil {
			// The same B3–B6/B11 bounds the webhook path applies, enforced by the
			// same kernel constructor so the two can never drift.
			r.log.DebugContext(ctx, "sources: reconcile dropped an alert that failed its bounds",
				"source_id", src.ID, "fingerprint", a.Fingerprint, "error", err)
			rejected++
			continue
		}

		startsAt := a.StartsAt.UTC()
		if startsAt.IsZero() {
			// B10: an alert with no start is legal on the wire and unusable as an
			// episode start. oto's own clock is the honest substitute and is
			// recorded as such by the skew below.
			startsAt = now
		}
		endsAt := a.EndsAt.UTC()
		if !endsAt.IsZero() && endsAt.Before(startsAt) {
			// B13/§B.3.2: a backward-skewed upstream clock is CLAMPED, never
			// rejected. The state machine clamps again on its own side; doing it
			// here as well keeps `source_ends_at` from ever going in backwards.
			endsAt = startsAt
		}

		status := statusFiring
		reason := ""
		if a.Status.State == upstreamSuppressed {
			status = statusSuppressed
			reason = suppressionReasonFor(a.Status)
			if reason == "" {
				// §G.8.3 requires a witness. Alertmanager saying `suppressed` while
				// naming nothing that suppresses it is a payload oto cannot honour:
				// T3 would fail `occ_suppress_ck`. Observe it as firing — which is
				// what it was a moment ago — rather than losing the alert.
				status = statusFiring
			}
		}

		// §C.4: a reconciler-sourced observation's `receiver` is "" and its
		// groupLabels are the AM ALERT GROUP's labels. An alert that belongs to no
		// group upstream (an unprocessed one) carries neither, and the orchestrator
		// records it without a generation rather than inventing one.
		grp := groups[a.Fingerprint]

		out = append(out, alerts.Observation{
			// ⭐ THE LOAD-BEARING FIELD. `reconciler` is what admits T3 (§B.3.1);
			// no other value in this enum may enter `suppressed`.
			Source:      alerts.ObservedByReconciler,
			SourceID:    src.ID,
			ClusterID:   src.ClusterID,
			Receiver:    grp.receiver,
			GroupLabels: grp.labels,
			// BatchID is deliberately zero. A reconcile pass is not a batch: there
			// is no raw payload to keep, nothing to replay, and no `ingest_batches`
			// row it could point at (C18).
			ClusterKey: clusterKey,

			AlertKey:          alerts.ComputeAlertKey(src.OrgID, clusterKey, labels, src.IgnoreLabels),
			SourceFingerprint: labels.Fingerprint(),
			Labels:            labels,
			Annotations:       boundAnnotations(a.Annotations, src),
			GeneratorURL:      truncateBytes(a.GeneratorURL, alerts.MaxGeneratorURLBytes),

			Status:            status,
			SuppressionReason: reason,
			SuppressedBy: alerts.SuppressedBy{
				SilencedBy:  a.Status.SilencedBy,
				InhibitedBy: a.Status.InhibitedBy,
				MutedBy:     a.Status.MutedBy,
			},

			SourceStartsAt: startsAt,
			SourceEndsAt:   endsAt,
			// UpdatedAt is a real per-alert field on API v2, unlike the webhook,
			// where it does not exist at all.
			SourceUpdatedAt: a.UpdatedAt.UTC(),

			ObservedAt: now,
			SkewMS:     now.Sub(startsAt).Milliseconds(),
		})
	}
	return out, rejected
}

// upstreamGroup is one alert's notification group as Alertmanager reports it.
type upstreamGroup struct {
	receiver string
	labels   map[string]string
}

// groupLabels reads `GET /api/v2/alerts/groups` and indexes each member alert's
// group by Alertmanager's own fingerprint.
//
// ⭐ WHY A SECOND CALL. §C.4 says a reconciler-sourced observation's group labels
// are "the AM alert group's labels", and `GET /api/v2/alerts` does not carry
// them. Without this, every alert the reconciler recovered would land in one
// degenerate per-source group — recorded, and threaded into a Slack conversation
// that means nothing.
//
// ⛔ AM's own `groupKey` is NOT read here even though the endpoint could supply
// it. It embeds the route path, changes on every `alertmanager.yml` reload, and is
// unescaped and unbounded (C3). oto's §C.4 key is computed from the group LABELS,
// which are stable.
//
// A failure DEGRADES, it does not fail the pass: the suppression truth in the
// alert set is the correctness, and the group is the delivery. Losing the alert
// to protect the notification would be exactly backwards.
func (r *Reconciler) groupLabels(
	ctx context.Context, scope db.TenantScope, src domain.Source,
) map[string]upstreamGroup {
	out := map[string]upstreamGroup{}

	groups, err := r.sources.AlertGroups(ctx, scope, src.ID, alertmanager.AlertGroupFilter{
		Active: true, Silenced: true, Inhibited: true, Muted: true,
	})
	if err != nil {
		r.log.WarnContext(ctx, "sources: reconcile could not read alert groups; observing without group labels",
			"source_id", src.ID, "code", errs.CodeOf(err))
		return out
	}

	for _, g := range groups {
		info := upstreamGroup{receiver: g.Receiver, labels: g.Labels}
		for _, a := range g.Alerts {
			if a.Fingerprint == "" {
				continue
			}
			out[a.Fingerprint] = info
		}
	}
	return out
}

// suppressionReasonFor maps Alertmanager's three witnesses onto its four
// suppression reasons, in §G.8.3's order.
//
// ⛔ `snoozed` IS NOT ONE OF THEM and must never be added. This enum mirrors what
// ALERTMANAGER is doing; a human asking oto to be quiet is a different fact about
// a different system (§B.8.2).
func suppressionReasonFor(st domain.AlertStatus) string {
	switch {
	case len(st.SilencedBy) > 0:
		return "silence"
	case len(st.InhibitedBy) > 0:
		return "inhibition"
	case len(st.MutedBy) > 0:
		return "mute_time_interval"
	default:
		return ""
	}
}

// ------------------------------------------------------------------ fan-out

// FanOut enqueues one `source.reconcile` per due source and one `silences.sync`
// per reconcile-enabled source, across every tenant.
//
// It is the body of the `source.reconcile` job when its payload names NO source
// — the periodic tick cannot know the source list, and `platform/jobs` says so
// out loud: "the fan-out needs the source list and belongs to the `sources`
// service, which enqueues them through db.Enqueuer".
//
// One tenant's failure logs and CONTINUES. The tick repeats within thirty
// seconds, which makes "carry on" strictly better than "abort": aborting turns
// one tenant's problem into every tenant's silence.
func (r *Reconciler) FanOut(ctx context.Context) (int, error) {
	if r.orgs == nil || r.enq == nil {
		return 0, errs.New(errs.KindInternal, CodeReconcileUnwired,
			"the reconcile fan-out has no tenant lister or no queue")
	}
	scopes, err := r.orgs.Scopes(ctx)
	if err != nil {
		return 0, err
	}

	enqueued := 0
	for _, scope := range scopes {
		n, err := r.fanOutTenant(ctx, scope)
		enqueued += n
		if err != nil {
			r.log.ErrorContext(ctx, "sources: reconcile fan-out failed for one tenant",
				"org_id", scope.OrgID(), "error", err)
		}
	}
	return enqueued, nil
}

// fanOutTenant schedules one tenant's passes.
func (r *Reconciler) fanOutTenant(ctx context.Context, scope db.TenantScope) (int, error) {
	// ListDue is the bounded fan-out query: only sources whose
	// `reconcile_interval_s` has elapsed, never-reconciled ones first.
	due, err := r.sources.repo.ListDue(ctx, scope, FanOutLimit)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}

	reqs := make([]db.JobRequest, 0, len(due)*2)
	for _, src := range due {
		reqs = append(reqs,
			db.JobRequest{Args: jobs.SourceReconcileArgs{SourceID: src.ID}},
			// `silences.sync` rides the same fan-out rather than a second periodic.
			// Its args carry a 60 s uniqueness window, so enqueueing it on every
			// 30 s tick collapses to one run a minute — the schedule §G.3 asks for,
			// enforced by the queue rather than by a second clock.
			db.JobRequest{Args: jobs.SilencesSyncArgs{SourceID: src.ID}},
		)
	}
	if _, err := r.enq.EnqueueMany(ctx, reqs); err != nil {
		return 0, errs.Wrap(err, errs.KindUnavailable, CodeReconcileFailed,
			"the reconcile fan-out could not be queued")
	}
	return len(reqs), nil
}

// ------------------------------------------------------------------- bounds
//
// The small half of `ingestion/decode`, reimplemented because depguard forbids
// the import and the pull path MUST apply the same rules as the push path.
// See the note on redactedValue.

// boundLabels merges `inject_labels` UNDER the upstream's own and applies
// `redact_labels`.
//
// UNDER, not over: an injected label loses to a label the upstream actually sent.
// oto adds context; it does not overwrite evidence.
func boundLabels(wire map[string]string, src domain.Source) map[string]string {
	out := make(map[string]string, len(wire)+len(src.InjectLabels))
	for name, value := range src.InjectLabels {
		out[name] = value
	}
	for name, value := range wire {
		out[name] = value
	}
	for name := range out {
		if matchesAny(name, src.RedactLabels) {
			out[name] = redactedValue
		}
	}
	return out
}

// annotationPriority are the annotations oto itself reads. When B7 forces a drop
// these survive regardless of where they sort: losing `summary` to alphabetical
// luck would blank the one line a human actually reads.
var annotationPriority = []string{
	alerts.AnnotationSummary,
	alerts.AnnotationDescription,
	alerts.AnnotationRunbookURL,
}

// boundAnnotations applies B7 (count), B8 (value length) and `redact_annotations`.
//
// Both bounds KEEP the alert. An annotation is display text and is deliberately
// not part of any identity (§C.9.3), so no amount of annotation abuse justifies
// losing the signal underneath it.
func boundAnnotations(wire map[string]string, src domain.Source) map[string]string {
	if len(wire) == 0 {
		return map[string]string{}
	}

	names := make([]string, 0, len(wire))
	for name := range wire {
		names = append(names, name)
	}
	if len(names) > alerts.MaxAnnotations {
		// Priority first, then the remainder in a stable order, so the same
		// upstream always keeps the same annotations.
		slices.Sort(names)
		kept := make([]string, 0, alerts.MaxAnnotations)
		for _, name := range annotationPriority {
			if _, ok := wire[name]; ok {
				kept = append(kept, name)
			}
		}
		for _, name := range names {
			if len(kept) >= alerts.MaxAnnotations {
				break
			}
			if !slices.Contains(kept, name) {
				kept = append(kept, name)
			}
		}
		names = kept
	}

	out := make(map[string]string, len(names))
	for _, name := range names {
		value := wire[name]
		if matchesAny(name, src.RedactAnnotations) {
			out[name] = redactedValue
			continue
		}
		out[name] = truncateBytes(value, alerts.MaxAnnotationValueBytes)
	}
	return out
}

// matchesAny reports whether name matches any glob, `path.Match`'s dialect.
//
// A malformed pattern is treated as no match rather than an error: a typo in a
// redaction list must not be able to fail a pass, and the safe direction on a
// typo is "redacted nothing", which is visible.
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if p == name {
			return true
		}
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// truncateBytes cuts s to at most n bytes without splitting a UTF-8 rune.
func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
