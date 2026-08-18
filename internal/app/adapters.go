package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	channelsservice "github.com/thulasiram/oto/internal/channels/service"
	drillservice "github.com/thulasiram/oto/internal/drill/service"
	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	groupingapi "github.com/thulasiram/oto/internal/grouping/api"
	groupingdomain "github.com/thulasiram/oto/internal/grouping/domain"
	groupingservice "github.com/thulasiram/oto/internal/grouping/service"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	ingestiondomain "github.com/thulasiram/oto/internal/ingestion/domain"
	ingestionservice "github.com/thulasiram/oto/internal/ingestion/service"
	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	rulesdomain "github.com/thulasiram/oto/internal/rules/domain"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	sourcesapi "github.com/thulasiram/oto/internal/sources/api"
	sourcesrepo "github.com/thulasiram/oto/internal/sources/repository"
	"github.com/thulasiram/oto/internal/sources/rulematch"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
	streamingdomain "github.com/thulasiram/oto/internal/streaming/domain"
	streamingservice "github.com/thulasiram/oto/internal/streaming/service"
)

// THE ADAPTERS.
//
// Every type in this file exists because two modules must not know about each
// other, and the composition root is the ONE place that may know both. Each one
// is deliberately tiny: an adapter with logic in it is a module that ended up in
// the wrong package.
//
// Three of them are late-bound (`*Late` fields set after construction) because
// the dependency graph has genuine cycles at the SERVICE level even though it
// has none at the package level — `alerts` needs a group's state_version and
// `grouping` needs the alert timeline. The cycle is broken here, in the wiring,
// rather than by widening either module's imports.

// ---------------------------------------------------------------- streaming

// streamAppender adapts `streaming/service` onto the identical `StreamAppender`
// port that `alerts/service` and `grouping/service` each declare for themselves.
//
// The kind travels as a plain string across the port — depguard keeps
// `streaming/domain` out of both consumers — so this is where it becomes a
// typed, CHECK-constrained `ui_events.kind`. An unknown kind is REFUSED rather
// than coerced: a frame the client cannot decode is worse than no frame.
//
// It is called inside the caller's transaction, so the row, its NOTIFY and the
// state change they describe commit together (§E.4).
type streamAppender struct {
	svc *streamingservice.Service
}

func (a streamAppender) Append(
	ctx context.Context, s db.TenantScope, kind string, resourceID uuid.UUID, payload []byte,
) error {
	if a.svc == nil {
		return nil
	}
	_, err := a.svc.Append(ctx, s, streamingdomain.Kind(kind), resourceID, json.RawMessage(payload))
	return err
}

// AppendBatch is the batched half of the alerts-side port, onto
// `streaming/service.AppendBatch`: one round trip and one NOTIFY for a whole
// observe batch (SPEC §G.4). Frames are validated and appended IN SLICE ORDER —
// the durable log's seq is assigned in insertion order, so the batched flush
// replays exactly as the per-frame appends it replaced did.
func (a streamAppender) AppendBatch(
	ctx context.Context, s db.TenantScope, frames []alertsservice.StreamFrame,
) error {
	if a.svc == nil || len(frames) == 0 {
		return nil
	}
	in := make([]streamingdomain.NewEvent, 0, len(frames))
	for _, f := range frames {
		ev, err := streamingdomain.NewAppend(streamingdomain.Kind(f.Kind), f.ResourceID,
			json.RawMessage(f.Payload))
		if err != nil {
			return err
		}
		in = append(in, ev)
	}
	_, err := a.svc.AppendBatch(ctx, s, in)
	return err
}

// ---------------------------------------------------------------- rules

// ruleLookup is `rules/service.RuleLookup` over `sources/service.ResolveRule`.
//
// It is the adapter the rules module documents in its own port comment: rules
// owns content-addressing and diffing, sources owns generatorURL decoding and
// the /api/v1/rules match, and neither may import the other. The two result
// shapes are field-for-field equivalent, so this is a translation and nothing
// more.
//
// ⛔ A lookup that recovers nothing returns a ZERO Recovery and a NIL ERROR.
// Failing to find a rule is the normal degraded path; returning an error here
// would produce no snapshot at all, which is precisely the outcome the design
// refuses — "we looked and could not see it" has to be a recorded fact.
type ruleLookup struct {
	sources *sourcesservice.Service
}

func (l ruleLookup) Lookup(
	ctx context.Context, s db.TenantScope, req rulesservice.LookupRequest,
) (rulesdomain.Recovery, error) {
	if l.sources == nil {
		return rulesdomain.Recovery{}, nil
	}

	m, err := l.sources.ResolveRule(ctx, s, req.SourceID, sourcesservice.RuleQuery{
		Labels:         req.Labels,
		Annotations:    req.Annotations,
		GeneratorURL:   req.GeneratorURL,
		SkipPrometheus: req.SkipUpstream,
		// A federated deployment's rule lives on the Prometheus named by the
		// alert's own generatorURL, not on the source's configured one.
		FollowGeneratorURL: !req.SkipUpstream,
	})
	if err != nil {
		// Degrade rather than fail: see the contract note above.
		return rulesdomain.Recovery{}, nil
	}
	return recoveryOf(m), nil
}

// recoveryOf maps a rulematch.Match onto rules/domain.Recovery.
func recoveryOf(m rulematch.Match) rulesdomain.Recovery {
	return rulesdomain.Recovery{
		Origin:               rulesdomain.Origin(m.Origin),
		Strategy:             rulesdomain.Strategy(m.Strategy),
		Confidence:           rulesdomain.Confidence(m.Confidence),
		CandidateCount:       m.CandidateCount,
		RuleName:             m.RuleName,
		RuleGroup:            m.RuleGroup,
		RuleFile:             m.RuleFile,
		Expr:                 m.Expr,
		ForSeconds:           m.ForSeconds,
		KeepFiringForSeconds: m.KeepFiringForSeconds,
		Labels:               m.Labels,
		Annotations:          m.Annotations,
		PrometheusURL:        m.PrometheusURL,
		Notes:                m.Notes,
	}
}

// ---------------------------------------------------------------- timeline

// timelineRecorder is BOTH `rules/service.EventRecorder` AND
// `enrichment/service.EventRecorder`, over the one seam that owns `alert_events`.
//
// ⭐ WHY IT IS ONE TYPE AND NOT TWO. The two ports differ only in the name of
// their method and the name of their struct; both describe the same act, which is
// "append one closed-enum fact about this case to the timeline". Two
// adapters would be two places for the actor, the dedupe key and the subject rules
// to drift apart in. So the translation lives once and the two methods are the
// two doors onto it.
//
// ⛔ NEITHER MODULE MAY IMPORT `alerts` FOR THIS. CONTEXT.md §4 draws no
// `enrichment ──► alerts` edge at all, and the `rules ──► alerts` edge it does
// draw is `rules/api` resolving the subject it narrates — not a licence for
// `rules/service` to open somebody else's table. Both therefore declare a port and
// this file, the composition root, satisfies it. `test/arch/arch_test.go` is what
// notices if that is ever "simplified" into an import.
//
// ⚠️ LATE-BOUND. `c.Rules` is built before `c.Alerts` — rules depends on nothing
// and the alerts service is the heart everything else is wired around — so the
// holder is injected empty and filled the moment the alerts service exists. A nil
// service answers nil: an un-narrated capture is the documented degradation, and
// it is strictly better than a boot ordering that decides whether rules can be
// constructed at all.
//
// ⛔ THE ACTOR IS `enricher`, WHICH IS NOT A GUESS. Both of these facts are
// produced inside an enrichment pass — `rule.*` by the `prom.rule` enricher
// calling `rules/service.Capture`, `enrichment.*` by the pipeline that ran it —
// and §D.4.1's actor vocabulary spells that `enricher`. It is also what the
// published example in `api/openapi/openapi.yaml` shows on a `rule.*` row.
type timelineRecorder struct {
	svc *alertsservice.Service
}

// ⭐ THE TWO LINES THAT WERE MISSING. Both ports were declared, documented and
// called, and NOTHING in the tree implemented either — so five of the thirty-six
// §D.4.1 types had no writer at all and the `s.events == nil` guard in each
// narrator was unconditional in a shipped binary. Stated as assertions because a
// port satisfied only by a field assignment in container.go is a port that can be
// unsatisfied again by deleting one.
var (
	_ rulesservice.EventRecorder  = (*timelineRecorder)(nil)
	_ enrichservice.EventRecorder = (*timelineRecorder)(nil)
)

// The actor identities the timeline shows for these two writers. They are
// denormalised onto every row (`actor_label` is immutable by design), so they are
// named once here rather than spelled at each call.
const (
	timelineActorRules      = "rules"
	timelineActorRulesLabel = "Rule snapshot"
	timelineActorEnrich     = "enrichment"
	timelineActorEnrichLbl  = "Enrichment"
)

// RecordRuleEvent appends one of the three `rule.*` facts (§D.4.1, T12).
//
// SnapshotID travels in the PAYLOAD and not in a column: `alert_events` has no
// snapshot column, and the case's binding to its snapshot is
// `alert_cases.rule_snapshot_id`, written by the enricher. What the timeline
// carries is which snapshot the sentence is about.
func (r *timelineRecorder) RecordRuleEvent(
	ctx context.Context, s db.TenantScope, ev rulesservice.RuleEvent,
) error {
	if r.svc == nil {
		return nil
	}
	payload := ev.Payload
	if ev.SnapshotID != uuid.Nil {
		payload = withKey(payload, "snapshot_id", ev.SnapshotID.String())
	}
	return r.svc.AppendTimelineEvent(ctx, s, alertsservice.TimelineEventRequest{
		Type:       ev.Type,
		AlertID:    ev.AlertID,
		CaseID:     ev.CaseID,
		Summary:    ev.Summary,
		Payload:    payload,
		DedupeKey:  ev.DedupeKey,
		ActorKind:  alertsdomain.ActorEnricher.String(),
		ActorID:    timelineActorRules,
		ActorLabel: timelineActorRulesLabel,
	})
}

// RecordEnrichmentEvent appends `enrichment.completed` or `enrichment.failed`
// (§D.4.1, T11). One event per PHASE, never one per enricher — the coalescing is
// the pipeline's and this must not undo it.
func (r *timelineRecorder) RecordEnrichmentEvent(
	ctx context.Context, s db.TenantScope, ev enrichservice.EnrichmentEvent,
) error {
	if r.svc == nil {
		return nil
	}
	return r.svc.AppendTimelineEvent(ctx, s, alertsservice.TimelineEventRequest{
		Type:       ev.Type,
		AlertID:    ev.AlertID,
		CaseID:     ev.CaseID,
		Summary:    ev.Summary,
		Payload:    ev.Payload,
		DedupeKey:  ev.DedupeKey,
		ActorKind:  alertsdomain.ActorEnricher.String(),
		ActorID:    timelineActorEnrich,
		ActorLabel: timelineActorEnrichLbl,
	})
}

// withKey returns the payload with one more entry, without mutating the caller's
// map. The caller keeps its own copy — a recorder that edited it would be editing
// the map the emitting service still holds.
func withKey(in map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}

// ---------------------------------------------------------------- notification

// notificationReader is `alerts/service.NotificationReader` over
// `notification/service.HistoryService`.
//
// ⛔ SPEC §I.1 binds `alerts` never to import `notification`. This struct copy is
// the entire cost of that rule, and it is worth paying: it is what lets oto run
// with notifications wired out completely.
//
// A SUPPRESSED NOTIFICATION IS IN THIS LIST. "oto decided not to tell you, and
// here is why" is the answer an operator needs; an empty page is not.
type notificationReader struct {
	svc *notifservice.HistoryService
}

func (r notificationReader) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) ([]alertsservice.NotificationSummary, db.Cursor, error) {
	if r.svc == nil {
		return nil, db.Cursor{}, nil
	}
	rows, cursor, err := r.svc.ListForAlert(ctx, s, alertID, p)
	if err != nil {
		return nil, db.Cursor{}, err
	}

	out := make([]alertsservice.NotificationSummary, 0, len(rows))
	for _, n := range rows {
		out = append(out, alertsservice.NotificationSummary{
			ID:               n.ID,
			GroupID:          n.GroupID,
			AlertID:          n.AlertID,
			CaseID:           n.CaseID,
			Reason:           n.Reason,
			Status:           n.Status,
			SuppressedReason: n.SuppressedReason,
			StateVersion:     n.StateVersion,
			CreatedAt:        n.CreatedAt,
			DeliveriesTotal:  n.DeliveriesTotal,
			DeliveriesSent:   n.DeliveriesSent,
			DeliveriesFailed: n.DeliveriesFailed,
			DeliveriesDead:   n.DeliveriesDead,
			// PolicyID, UpdatedAt, DeliveriesSkipped and DeliveriesPending are
			// ABSENT rather than substituted: the v1 read model does not track
			// them, and a projection that reports a value it did not read is
			// worse than one that reports nothing.
		})
	}
	return out, cursor, nil
}

// DeliveryRollupForAlert is `alerts/service.NotificationReader`'s roll-up half.
//
// ⭐ THE STRUCT COPY IS THE WHOLE COST OF §I.1, AND IT IS WORTH PAYING TWICE.
// With no notification module wired the answer is an all-zero roll-up, which is
// the truth for that deployment — nothing delivers, so nothing was delivered —
// rather than an omitted field, which is the ambiguity `delivery_summary` exists
// to remove.
func (r notificationReader) DeliveryRollupForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (alertsservice.DeliveryRollup, error) {
	return r.rollup(ctx, s, notifrepo.RollupAlert, alertID)
}

// DeliveryRollupForCase is the same question narrowed to one episode.
func (r notificationReader) DeliveryRollupForCase(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (alertsservice.DeliveryRollup, error) {
	return r.rollup(ctx, s, notifrepo.RollupCase, caseID)
}

func (r notificationReader) rollup(
	ctx context.Context, s db.TenantScope, subject notifrepo.RollupSubject, id uuid.UUID,
) (alertsservice.DeliveryRollup, error) {
	if r.svc == nil {
		return alertsservice.DeliveryRollup{}, nil
	}
	got, err := r.svc.DeliveryRollup(ctx, s, subject, id)
	if err != nil {
		return alertsservice.DeliveryRollup{}, err
	}
	// The two structs are field-identical by construction, so the copy is a
	// compile-checked type conversion: a field added on either side without the
	// other stops the BUILD here, rather than crossing the seam as a silent zero.
	return alertsservice.DeliveryRollup(got), nil
}

// groupDeliveryRollups is `grouping/api.DeliveryRollupReader`.
//
// A group generation is the subject oto actually notifies about — the intents are
// keyed on it — so this is the least derived of the three roll-ups and the one an
// operator reads first when a channel has gone quiet.
type groupDeliveryRollups struct {
	svc *notifservice.HistoryService
}

func (g groupDeliveryRollups) DeliveryRollupForGroup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) (groupingapi.DeliveryRollup, error) {
	if g.svc == nil {
		return groupingapi.DeliveryRollup{}, nil
	}
	got, err := g.svc.DeliveryRollup(ctx, s, notifrepo.RollupGroup, groupID)
	if err != nil {
		return groupingapi.DeliveryRollup{}, err
	}
	// Field-identical by construction: the conversion is the same compile-checked
	// copy `notificationReader.rollup` makes for the alerts-side type.
	return groupingapi.DeliveryRollup(got), nil
}

// subjectResolver is `notification/api.SubjectResolver`: it maps an alert or an
// case onto the group generation whose card would carry the fact.
//
// Routing is about the GROUP — routing two members of one group to two channels
// would split one conversation across two rooms — so a preview asked about an
// alert has to resolve that alert's group first.
type subjectResolver struct {
	alerts   *alertsservice.Service
	grouping *groupingservice.Service
}

func (r subjectResolver) GroupIDForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (uuid.UUID, error) {
	if r.grouping == nil {
		return uuid.Nil, errs.Unavailable("grouping_unavailable",
			"group resolution is not wired in this deployment", 0)
	}
	members, err := r.grouping.GroupsForAlert(ctx, s, alertID, 1)
	if err != nil {
		return uuid.Nil, err
	}
	if len(members) == 0 {
		return uuid.Nil, errs.NotFound("group_not_found", "this alert is not in any group")
	}
	return members[0].GroupID(), nil
}

func (r subjectResolver) GroupIDForCase(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (uuid.UUID, error) {
	if r.alerts == nil {
		return uuid.Nil, errs.Unavailable("alerts_unavailable",
			"case resolution is not wired in this deployment", 0)
	}
	ac, err := r.alerts.GetCase(ctx, s, caseID)
	if err != nil {
		return uuid.Nil, err
	}
	if ac.GroupID() == uuid.Nil {
		return uuid.Nil, errs.NotFound("group_not_found", "this case is not in any group")
	}
	return ac.GroupID(), nil
}

// ---------------------------------------------------------------- enrichment

// subjectLoader is `enrichment/service.SubjectLoader`.
//
// It denormalises one case into the frozen Subject an Enricher sees, plus
// the notification coordinates the async phase must quote back. Every read goes
// through a SERVICE — never another module's repository — which is why this
// lives here and not in `enrichment/repository`.
type subjectLoader struct {
	alerts   *alertsservice.Service
	grouping *groupingservice.Service
	sources  *sourcesservice.Service
	occSrc   *caseSourceReader
}

func (l subjectLoader) LoadSubject(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (enrichservice.Loaded, error) {
	if l.alerts == nil {
		return enrichservice.Loaded{}, errs.Unavailable("alerts_unavailable",
			"the alerts service is not wired in this deployment", 0)
	}

	ac, err := l.alerts.GetCase(ctx, s, caseID)
	if err != nil {
		return enrichservice.Loaded{}, err
	}
	// The Alert row alone, never the UI detail read: the enricher needs the
	// subject's frozen identity, and it already holds the case — `Get`'s
	// latest-case and active-snooze re-reads would be paid on every run
	// and discarded.
	alert, err := l.alerts.GetAlert(ctx, s, ac.AlertID())
	if err != nil {
		return enrichservice.Loaded{}, err
	}

	out := enrichservice.Loaded{
		AlertID: alert.ID(),
		GroupID: ac.GroupID(),
		Subject: enrichdomain.Subject{
			OrgID:       s.OrgID().String(),
			SubjectKind: "case",
			SubjectID:   caseID.String(),
			Alert: enrichdomain.AlertSnapshot{
				ID:                alert.ID().String(),
				AlertKey:          alert.Key().String(),
				SourceFingerprint: alert.Fingerprint().String(),
				AlertName:         alert.AlertName(),
				Severity:          alert.Severity().String(),
				Namespace:         alert.Namespace(),
				Service:           alert.Service(),
				ClusterKey:        alert.ClusterKey().String(),
				Labels:            alert.Labels().Map(),
				Annotations:       alert.Annotations().Map(),
				GeneratorURL:      alert.GeneratorURL(),
			},
			Case: enrichdomain.CaseSnapshot{
				ID:             ac.ID().String(),
				Seq:            ac.Seq(),
				State:          ac.State().String(),
				StartedAt:      ac.StartedAt(),
				SourceStartsAt: ac.SourceStartsAt(),
			},
		},
	}

	// The group's state_version pins a late enrichment to the group state it was
	// minted against (§C.7), which is what stops an amended card resending an old
	// one. No group means nothing to amend, and that is not an error.
	if out.GroupID != uuid.Nil && l.grouping != nil {
		if v, verr := l.grouping.StateVersion(ctx, s, out.GroupID); verr == nil {
			out.StateVersion = v
		}
	}

	// The source is only needed by enrichers that call upstream. Failing to
	// resolve it must not fail the run: a missing SourceRef degrades promrule to
	// its generatorURL-only path, which is the documented behaviour.
	if l.occSrc != nil {
		if sourceID, ok := l.occSrc.SourceID(ctx, s, caseID); ok {
			out.SourceID = sourceID
			out.Subject.Source.ID = sourceID.String()
			if l.sources != nil {
				if src, serr := l.sources.Get(ctx, s, sourceID); serr == nil {
					out.Subject.Source.ClusterID = src.ClusterID.String()
					out.Subject.Source.ClusterKey = alert.ClusterKey().String()
					out.Subject.Source.BaseURL = src.BaseURL
					out.Subject.Source.PrometheusURL = src.PrometheusURL
					out.Subject.Source.Kind = string(src.Kind)
				}
			}
		}
	}
	return out, nil
}

// caseSourceReader answers "which AlertSource did this episode come from",
// through the batch port `alerts/service` already declares for the reaper guard.
type caseSourceReader struct {
	resolver alertsservice.CaseSourceResolver
}

func (r *caseSourceReader) SourceID(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (uuid.UUID, bool) {
	if r == nil || r.resolver == nil {
		return uuid.Nil, false
	}
	m, err := r.resolver.SourceIDs(ctx, s, []uuid.UUID{caseID})
	if err != nil {
		return uuid.Nil, false
	}
	src, ok := m[caseID]
	return src, ok && src != uuid.Nil
}

// enrichmentReader is `alerts/service.EnrichmentReader` over the enrichment
// repository's own read model.
//
// A FAILED enrichment and a MISSING one stay distinguishable — that is what
// Status is for, and collapsing them here would hide the exact case the
// provenance exists to record.
type enrichmentReader struct {
	repo *enrichrepo.EnrichmentRepository
}

func (r enrichmentReader) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, caseID *uuid.UUID,
) ([]alertsservice.EnrichmentSummary, error) {
	if r.repo == nil {
		return nil, nil
	}

	kind, subject := "alert", alertID
	if caseID != nil && *caseID != uuid.Nil {
		kind, subject = "case", *caseID
	}

	rows, err := r.repo.ListBySubject(ctx, s, kind, subject.String())
	if err != nil {
		return nil, err
	}

	out := make([]alertsservice.EnrichmentSummary, 0, len(rows))
	for _, e := range rows {
		summary := alertsservice.EnrichmentSummary{
			SubjectKind:     e.SubjectKind(),
			SubjectID:       subject,
			Enricher:        e.Enricher(),
			EnricherVersion: e.Version(),
			Phase:           int(e.Phase()),
			Status:          string(e.Status()),
			Payload:         payloadMap(e.Payload()),
			Warnings:        e.Warnings(),
			Error:           e.ErrorText(),
			DurationMS:      int(e.Duration() / time.Millisecond),
			FromCache:       e.FromCache(),
			ComputedAt:      e.ComputedAt(),
		}
		if parsed, perr := uuid.Parse(e.ID()); perr == nil {
			summary.ID = parsed
		}
		if expiresAt := e.ExpiresAt(); !expiresAt.IsZero() {
			summary.ExpiresAt = &expiresAt
		}
		out = append(out, summary)
	}
	return out, nil
}

// payloadMap renders an enricher's typed payload as the generic object the
// alerts read model carries. A payload that will not round-trip is reported as
// an empty object rather than dropped: the enrichment RAN, and the row saying so
// is the provenance.
func payloadMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// ---------------------------------------------------------------- sources

// sourceHealth is ⭐ THE REAPER GUARD (§B.4), wired.
//
//	Losing sight of an alert is NOT the same as the alert resolving.
//
// When the sources service is absent, or cannot answer, this returns NOTHING and
// the sweep HOLDS every candidate. The reaper defaults to silence, never to a
// fabricated ending.
type sourceHealth struct {
	svc *sourcesservice.Service
}

// HealthyFor answers the guard for every source a reap tick resolved, in one
// round trip. `HealthFor` omits sources that were never probed and ids this org
// does not own, and the omission is passed through untouched: the sweep reads
// absence as "cannot prove healthy" and holds, which is exactly what unknown
// must mean under §B.4.
func (h sourceHealth) HealthyFor(
	ctx context.Context, s db.TenantScope, sourceIDs []uuid.UUID,
) (map[uuid.UUID]bool, error) {
	if h.svc == nil {
		return nil, nil
	}
	health, err := h.svc.HealthFor(ctx, s, sourceIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(health))
	for id, sh := range health {
		out[id] = !sh.BlocksReaper()
	}
	return out, nil
}

// The per-source ingest token is minted and revoked by `identity/service`
// itself (IssueIngestToken / RevokeIngestTokens): the mint recipe, the
// mint-before-revoke transaction and the revocation sweep are credential
// lifecycle, so they live beside the PAT mint they mirror rather than in this
// file. `container.go` hands `c.Identity` straight to
// `sources/service.IngestTokens` — the signatures match by construction
// (assertions.go), and `sources` still imports nothing of `identity`.

// ---------------------------------------------------------------- org settings

// orgSettings serves the two SettingsReader ports — `alerts/service.Lifecycle`
// and `grouping/service.Storm` — from `orgs.settings`, which `identity` owns.
//
// A settings lookup MUST NEVER be able to stop an alert being recorded, so every
// failure falls back to the §D.1 defaults rather than propagating.
type orgSettings struct {
	svc *identityservice.Service
}

func (o orgSettings) Lifecycle(ctx context.Context, s db.TenantScope) (alertsservice.Settings, error) {
	if o.svc == nil {
		return alertsservice.DefaultSettings(), nil
	}
	org, err := o.svc.GetOrg(ctx, s)
	if err != nil {
		return alertsservice.DefaultSettings(), nil
	}
	cfg := org.Settings.Normalise()
	return alertsservice.Settings{
		RefireGrace:   cfg.RefireGrace,
		ResolveGrace:  cfg.ResolveGrace,
		FlapThreshold: cfg.FlapThreshold,
		FlapWindow:    cfg.FlapWindow,
	}, nil
}

func (o orgSettings) Storm(ctx context.Context, s db.TenantScope) (groupingdomain.StormPolicy, error) {
	if o.svc == nil {
		return groupingdomain.DefaultStormPolicy(), nil
	}
	org, err := o.svc.GetOrg(ctx, s)
	if err != nil {
		return groupingdomain.DefaultStormPolicy(), nil
	}
	cfg := org.Settings.Normalise()
	return groupingdomain.StormPolicy{
		Threshold:  cfg.StormThreshold,
		Window:     cfg.StormWindow,
		Cooldown:   cfg.StormCooldown,
		CloseDelay: cfg.GroupCloseDelay,
	}.Normalise(), nil
}

// NotificationDefaults serves `notification/service.SettingsReader` from
// `orgs.settings`: which transitions surface in the channel (ADR 0020), the
// fallback verbosity for a Channel that names none, and the fallback unacked
// reminder delay for a policy that names none.
//
// ⛔ IT NEVER PROPAGATES A FAILURE, for the same reason as the two ports above:
// a settings lookup must not be able to stop a notification. An unreadable
// settings row yields oto's shipped defaults, which are the quiet-but-honest
// ones.
//
// ⭐ THERE IS NO CACHE HERE, DELIBERATELY. Every evaluation reads the row, so a
// settings change binds on the very next evaluation in every pod — no restart, no
// invalidation message, and no window in which two pods disagree about how loud
// oto should be. If a cache is ever added it MUST carry a bounded TTL: the
// failure it would introduce is the silent one, where an operator raises
// `storm_threshold` mid-incident, sees nothing change, and cannot tell a wrong
// setting from a stale one.
func (o orgSettings) NotificationDefaults(
	ctx context.Context, s db.TenantScope,
) (notifservice.OrgDefaults, error) {
	def := notifservice.OrgDefaults{
		Broadcast: notifdomain.DefaultBroadcastPolicy(),
		Verbosity: notifdomain.VerbosityStatusChanges,
	}
	if o.svc == nil {
		return def, nil
	}
	org, err := o.svc.GetOrg(ctx, s)
	if err != nil {
		return def, nil
	}
	cfg := org.Settings.Normalise()
	return notifservice.OrgDefaults{
		Broadcast:            notifdomain.BroadcastPolicy{Resolved: cfg.BroadcastOnResolved},
		Verbosity:            notifdomain.Verbosity(cfg.DefaultVerbosity),
		UnackedReminderAfter: cfg.UnackedReminderAfter,
		StormCooldown:        cfg.StormCooldown,
		ReminderMention: notifdomain.MentionPolicy{
			Mode:        cfg.UnackedReminderMention,
			List:        cfg.UnackedReminderMentionList,
			MinSeverity: cfg.UnackedReminderMentionMinSeverity,
		},
	}, nil
}

// ---------------------------------------------------------------- late binding

// groupVersions is `alerts/service.GroupVersionReader`, late-bound.
//
// `alerts` is constructed before `grouping` (grouping needs the alert timeline
// and the member actions), but `alerts` needs a group's `state_version` to mint
// a notify job's idempotency key (§C.7). The service-level cycle is broken here
// rather than by widening either module.
//
// Version 0 is the documented degraded answer: the notify worker resolves the
// real version at evaluation time.
type groupVersions struct {
	svc *groupingservice.Service
}

func (g *groupVersions) StateVersion(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error) {
	if g == nil || g.svc == nil {
		return 0, nil
	}
	return g.svc.StateVersion(ctx, s, groupID)
}

// `notification` is late-bound for the same reason — it is constructed after
// `alerts` — but it needs no holder type of its own. The container hands `alerts`
// a `*notificationReader` with no service in it and fills the field once
// `notification` exists; `notificationReader`'s own `svc == nil` branches are the
// degraded answer during that window, and they are the same branches a deployment
// with notifications wired out entirely relies on.
//
// The asymmetry with `groupVersions` above is deliberate but thin: that type
// guards `g == nil` as well, because `grouping`'s `StateVersion` has no
// nil-receiver guard of its own. `notificationReader`'s methods take VALUE
// receivers, so a nil `*notificationReader` would panic before any `svc == nil`
// branch could run. Nothing can produce one today — `container.go` allocates the
// holder unconditionally — but a future `dropNotifications` degradation that left
// the port nil would be a startup panic, not a degraded read. Allocate the holder
// or add the guard; do not leave the port nil.

// ---------------------------------------------------------------- job scopes

// caseScopes resolves the tenant that owns a case, for the
// `enrich.run` worker.
//
// ⚠ It is one of the few unscoped queries in the process, and it is unscoped
// because it is the query that PRODUCES the scope: `EnrichRunArgs` names an
// case and no org (§G.3). A worker must not take the tenant on trust from
// a job payload — a job row is data, and data that decided its own authorisation
// would undo the tenancy boundary — so the org comes from the SUBJECT.
type caseScopes struct {
	pool *pgxpool.Pool
}

const orgOfCaseSQL = `SELECT org_id FROM alert_cases WHERE id = $1`

func (r caseScopes) ScopeForCase(
	ctx context.Context, caseID uuid.UUID,
) (db.TenantScope, error) {
	var orgID uuid.UUID
	if err := r.pool.QueryRow(ctx, orgOfCaseSQL, caseID).Scan(&orgID); err != nil {
		return db.TenantScope{}, errs.NotFound("case_not_found", "no such case")
	}
	return db.NewTenantScope(orgID)
}

// orgLister enumerates every LIVE tenant, for the periodic sweeps that are
// global.
//
// The sweeps run per org because every repository method takes a TenantScope, by
// construction, and this is THE tenant list for all of them. The reminder sweep
// used to keep its own copy of this query in `notification/repository` — same
// `deleted_at IS NULL` filter, same `ORDER BY id`, but UNBOUNDED, read whole on
// every tick, and turned into scopes directly instead of through `LiveScope` —
// which is why a second copy is not allowed back: two lists drift, and the one
// that drifts is the one nobody is reading in review. This one is WALKED IN
// PAGES rather than read in one query, because it is read on every tick of every
// periodic sweep and a single unbounded scan is a query whose cost is set by the
// customer count. Its two halves are
// the two sides of the fan-out: `ScopePage` hands the enqueue side one bounded
// page of bare ids at a time, and `LiveScope` is the only thing that turns one
// of those ids back into an authorisation, against the table, when its job
// runs.
//
// ⛔ A SOFT-DELETED TENANT IS NOT SWEPT, which is the whole point of the filter:
// sweeping a departed tenant is work that produces alerts, reminders and flap
// scores nobody will ever read. This lister was the one place in the process
// that joined `orgs` without the filter, and the comment that used to sit here
// claimed parity with the reminder query while the SQL below said otherwise.
type orgLister struct {
	pool *pgxpool.Pool
}

// listOrgIDsSQL reads ONE keyset page of live tenants, walking the primary key.
//
// `id` is the primary key and a UUIDv7, so `id > $1 ORDER BY id` is a cursor
// that can neither skip nor repeat a tenant when one is created or soft-deleted
// mid-walk — which, on a sweep that runs every minute, it eventually is. There
// is no OFFSET here for the reason `db.Keyset` gives for the whole codebase.
const listOrgIDsSQL = `SELECT id FROM orgs WHERE deleted_at IS NULL AND id > $1 ORDER BY id LIMIT $2`

// orgPageSize bounds ONE query the way `sweepLimit` bounds one tick's work per
// tenant, and is the same size for the same reason: a bound nobody reaches in
// practice is still the bound that keeps a bad night from becoming an outage.
//
// ⛔ IT IS `jobs.TenantFanOutLimit` AND NOT A SECOND 500, BECAUSE TWO 500s CAN
// DIVERGE AND ONE CANNOT. `FanOutTenants` asks for `TenantFanOutLimit` ids and
// queues a continuation only when the page comes back FULL. While this was its own
// literal, raising the fan-out's ceiling above it — a one-line edit in another
// package, with nothing pointing here — made every page come back short, so no
// continuation was ever queued and every tenant past the smaller number was never
// swept again. That is the permanent starvation `TenantFanOutLimit`'s own ⛔
// comment promises is impossible, reachable without either comment being wrong on
// its own. Now the fan-out's ceiling and the page that serves it are ONE number.
const orgPageSize = jobs.TenantFanOutLimit

// ScopePage is `jobs.TenantPager`: ONE page of the walk, handed to the
// per-tenant fan-out rather than accumulated.
//
// ⭐ IT RETURNS BARE IDS AND NEVER SCOPES, AND THAT IS THE POINT. What it hands
// out goes into a job PAYLOAD, and a payload is data. The scope for the work is
// produced later, by `LiveScope`, against the table — so an org id that is
// forged, stale, or simply departed between the tick and the pass cannot become
// an authorisation. It is also why there is no `NewTenantScope` filter here: an
// id that cannot become a scope must still advance the cursor — a page whose
// only rows were unusable would otherwise be asked for forever — and the only
// place that can be decided is where the scope is actually needed.
//
// ⛔ A LIMIT IT CANNOT SERVE IS AN ERROR, NEVER A QUIETLY SMALLER PAGE. The caller
// reads a short page as "the end of the tenant table" — that is the whole contract
// of `TenantPager`, and it is what decides whether a continuation is queued. A
// pager that clamped the limit down would answer "there are no more tenants" to a
// caller asking about tenants that exist, and the tenants past the clamp would
// never be swept again. So an over-large limit says so, loudly, in the one place
// that can tell the difference.
func (l orgLister) ScopePage(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = orgPageSize
	}
	if limit > orgPageSize {
		return nil, errs.Newf(errs.KindInternal, "org_page_too_large",
			"a tenant page of %d was asked for and this lister serves at most %d; "+
				"a short page means the end of the table, so it must not mean a clamp",
			limit, orgPageSize)
	}
	rows, err := l.pool.Query(ctx, listOrgIDsSQL, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var orgID uuid.UUID
		if err := rows.Scan(&orgID); err != nil {
			return nil, err
		}
		out = append(out, orgID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// liveOrgSQL asks the ONE question a per-tenant job payload may not answer for
// itself: is this org real, and is it still here.
const liveOrgSQL = `SELECT id FROM orgs WHERE id = $1 AND deleted_at IS NULL`

// LiveScope is `jobs.TenantScoper`: the org id a job payload NAMES, resolved into
// the scope the table AUTHORISES.
//
// ⛔ THE FILTER IS THE SAME `deleted_at IS NULL` THE LISTER USES, and it is here
// as well as there because the two are separated by a queue. be3d314 stopped the
// sweeps visiting departed tenants by fixing the list; a per-tenant job that
// trusted its payload would have put them straight back, since a tenant can leave
// in the seconds between the fan-out reading the table and the pass running. A
// departed tenant resolves to NotFound, which `jobs.ForTenant` reads as "nothing
// to do and nothing to retry".
//
// ⚠️ ONLY "NO SUCH ROW" IS NotFound. Every other failure — a dead connection, a
// cancelled context — is returned AS IS, so the job retries. Collapsing them all
// into NotFound would turn a database blip into "this tenant no longer exists"
// and skip its sweep without a word, which is the silent-stop this whole file is
// careful about.
func (l orgLister) LiveScope(ctx context.Context, orgID uuid.UUID) (db.TenantScope, error) {
	var found uuid.UUID
	switch err := l.pool.QueryRow(ctx, liveOrgSQL, orgID).Scan(&found); {
	case errors.Is(err, pgx.ErrNoRows):
		return db.TenantScope{}, errs.NotFound("org_not_found", "no such org, or it has been deleted")
	case err != nil:
		return db.TenantScope{}, err
	}
	return db.NewTenantScope(found)
}

// ---------------------------------------------------------------- ingestion

// alertObserver is `ingestion/service.AlertObserver` — THE ONLY WRITE PATH INTO
// `alerts` (§G.4, C18) — and it is THE INGEST ORCHESTRATOR: the one place that
// may know both `alerts` and `grouping`.
//
// ⭐ IT RESOLVES THE GROUP. §G.4 step 4 sits between the alert upsert and the
// state machine, and it belongs here rather than inside `alerts` because a module
// that records a signal must not depend on `grouping` to do it (depguard enforces
// exactly that). The Observation carries the §C.4 inputs; this resolves them into
// a generation and hands the id back in through `ObserveOptions.GroupID`.
//
// ⭐ IT IS ALL ONE TRANSACTION. `ingestion` has already opened one on the ingest
// pool before calling, and `db.Tx` nests, so the group, the case, its
// membership, the events and the `notify.evaluate` job commit together or not at
// all. An alert whose group rolled back would own a Slack thread nobody could
// find.
type alertObserver struct {
	svc      *alertsservice.Service
	grouping *groupingservice.Service
	log      *slog.Logger
}

func (o alertObserver) ObserveBatch(
	ctx context.Context, s db.TenantScope, obs []alertsdomain.Observation,
) (int, error) {
	applied := 0
	// One webhook carries exactly one notification group, so this is one partition
	// and one ObserveBatch in the overwhelming case. It is a partition rather than
	// an assumption because the reconciler feeds the same port with observations
	// from many groups at once.
	for _, part := range partitionByGroup(obs) {
		groupID, err := o.resolveGroup(ctx, s, part[0])
		if err != nil {
			return applied, err
		}
		res, err := o.svc.ObserveBatch(ctx, s, part, alertsservice.ObserveOptions{
			GroupID:     groupID,
			GroupReason: groupReasonFor(part[0].NotificationReason),
		})
		if err != nil {
			return applied, err
		}
		applied += len(res.Outcomes)
		if err := o.settleGroup(ctx, s, groupID, part[0], res.Outcomes); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// groupReasonFor applies the §H.6 wire table to the batch as a whole, for the
// one row no per-alert transition can reach.
//
// `repeat interval elapsed` means Alertmanager is telling oto the same thing
// again: nothing transitioned, so `alerts` produces no Reason and — until this
// existed — no notification at all, which left the card's "Firing for" frozen at
// whatever it said hours ago. §H.6 answers it with a root UPDATE and NEVER a
// repost, and that single rule is the largest noise reduction oto has over stock
// Alertmanager and Grafana Alerting, both of which repost.
//
// Every other wire value either has a transition behind it (so `alerts` already
// said it, and this must not say it twice) or is `none`/absent (so there is
// nothing to say). This is the composition root, which is the only layer allowed
// to know both Alertmanager's vocabulary and oto's.
func groupReasonFor(wire string) string {
	reason, verdict := notifdomain.ReasonFromWire(wire)
	if verdict == notifdomain.WireMapped && reason == notifdomain.ReasonRepeat {
		return string(notifdomain.ReasonRepeat)
	}
	return ""
}

// resolveGroup opens or rejoins the §C.4 generation these observations belong to.
//
// ⛔ A VALIDATION failure DEGRADES rather than fails. Group labels that will never
// pass their bounds would otherwise cost the whole batch on every one of its
// retries, and losing the alert is far worse than recording it groupless: the
// signal is kept in full, and only the notification intent is missed. Anything
// else — an unreachable database, a conflict that could not be re-read — is
// propagated, because a retry can fix it.
func (o alertObserver) resolveGroup(
	ctx context.Context, s db.TenantScope, sample alertsdomain.Observation,
) (*uuid.UUID, error) {
	if o.grouping == nil {
		return nil, nil //nolint:nilnil // no grouping wired: record the signal, skip the intent.
	}
	g, err := o.grouping.Resolve(ctx, s, groupingservice.ResolveRequest{
		SourceID:  sample.SourceID,
		ClusterID: sample.ClusterID,
		// ⭐ THE TWO KEY INPUTS, and they are the alert's OWN facts. Everything else
		// on this request is provenance recorded on the generation (ADR 0038).
		ClusterKey: sample.ClusterKey,
		Labels:     sample.Labels,
		Receiver:   sample.Receiver,
		// ⛔ SourceGroupKey is stored verbatim and NEVER parsed (§C.4).
		SourceGroupKey:     sample.SourceGroupKey,
		NotificationReason: sample.NotificationReason,
		At:                 sample.ObservedAt,
		// Carried from `ingest_batches.mode` via the Observation. A drill's group
		// is marked so the dashboard counts can exclude it; nothing else about the
		// generation differs, which is the whole point of a drill.
		Synthetic: sample.Synthetic,
	})
	if err != nil {
		if errs.IsKind(err, errs.KindValidation) {
			o.log.WarnContext(ctx, "ingest: the group key could not be computed; recording the alert without a group",
				"source_id", sample.SourceID, "receiver", sample.Receiver, "error", err)
			return nil, nil //nolint:nilnil // degraded on purpose: see the doc comment.
		}
		return nil, err
	}
	id := g.ID()
	return &id, nil
}

// settleGroup re-derives the generation this batch landed on, ONCE.
//
// ⛔ IT NO LONGER RECORDS MEMBERSHIP, BECAUSE NOTHING DOES. `ObserveBatch` was
// already handed `GroupID` above and writes it onto every episode it opens, so by
// the time this runs the membership is on disk — `alert_cases.group_id` is
// the record, and migration 00051 removed the join table that used to duplicate
// it. What is left is the derivation: the generation's counts, its state, its
// severity and its §B.6 storm mode are all projections of its members, and
// `grouping.Recompute` is where they are refreshed.
//
// ⭐ ONE CALL FOR THE WHOLE PARTITION. `partitionByGroup` has already established
// that every outcome here belongs to ONE generation, so recomputing per outcome
// would be 500 full aggregates and 500 compare-and-set writes to one
// `alert_groups` row for one 500-alert Alertmanager batch, all but the last of
// them discarded.
//
// ⭐ A BATCH THAT MOVED NOTHING IS NOT RECOMPUTED. A repeat observation of an
// episode that is already open changes no count, so the aggregate would return
// what the row already says. That guard is the difference between one query per
// scrape and none, on the most frequent shape of request oto receives.
func (o alertObserver) settleGroup(
	ctx context.Context, s db.TenantScope, groupID *uuid.UUID,
	sample alertsdomain.Observation, outcomes []alertsservice.ObserveOutcome,
) error {
	if o.grouping == nil || groupID == nil {
		return nil
	}
	moved := false
	for _, out := range outcomes {
		if out.CaseID == uuid.Nil {
			continue
		}
		if out.CaseOpened || out.Transition != "" {
			moved = true
			break
		}
	}
	if !moved {
		return nil
	}
	_, err := o.grouping.Recompute(ctx, s, *groupID, sample.ObservedAt)
	return err
}

// partitionByGroup splits a batch into runs that share one §C.4 group identity,
// preserving first-seen order so two runs of the same batch resolve in the same
// order.
//
// ⭐ ONE WEBHOOK IS NO LONGER ONE PARTITION, AND THAT IS THE POINT. While the key
// hashed Alertmanager's own grouping, every alert in one envelope shared it by
// construction and this loop found exactly one run. Since ADR 0038 the key is
// derived per alert from `(cluster, alertname, namespace-or-∅)`, so a single
// `group_by: [cluster]` envelope carrying six alertnames now resolves six
// generations and six threads — which is the routing precision the derivation
// was for, and the reason this function was written to handle many runs in the
// first place (the reconciler already fed it many).
func partitionByGroup(obs []alertsdomain.Observation) [][]alertsdomain.Observation {
	if len(obs) <= 1 {
		if len(obs) == 0 {
			return nil
		}
		return [][]alertsdomain.Observation{obs}
	}

	index := map[string]int{}
	var parts [][]alertsdomain.Observation

	for _, o := range obs {
		k := groupingKey(o)
		at, ok := index[k]
		if !ok {
			index[k] = len(parts)
			parts = append(parts, []alertsdomain.Observation{o})
			continue
		}
		parts[at] = append(parts[at], o)
	}
	return parts
}

// groupingKey renders the §C.4 axes of one Observation as a comparable string.
//
// It writes the axes rather than the hash for one reason: `org_id` is not on the
// Observation and is constant across a batch anyway, so hashing here would mean
// either threading the scope in or hashing under a fake org. The axes ARE the
// identity up to that constant, and canon()'s length prefixes make the rendering
// injective — which matters, because two alerts that partition together are then
// resolved from `part[0]` alone.
func groupingKey(o alertsdomain.Observation) string {
	var b strings.Builder
	b.WriteString(o.ClusterKey.String())
	b.WriteByte(0)
	b.Write(alertsdomain.SplitLabels(o.Labels).Canonical(nil))
	return b.String()
}

// ------------------------------------------------- slack interactions (§H.8)

// THE THREE ADAPTERS AN ACKNOWLEDGE BUTTON NEEDS.
//
// `channels` is the LAST module in the dependency direction (SPEC §I.1), and an
// inbound button press is the one thing that runs the other way: it has to reach
// back into `grouping` to write the receipt and into `identity` to name the
// human. Rather than widen the channels module's imports — which would put an
// arrow from the end of the chain to its middle in every future reader's head —
// the ports are declared in primitives and adapted HERE, which is the one place
// allowed to know both sides.

// slackConversations adapts the channels repository onto the port
// `channels/service` declares for tenant resolution.
//
// The two SlackDestination types are structurally identical and deliberately
// separate: the repository's is a row-shaped fact and the service's is a port
// vocabulary, and collapsing them would make the service import the repository.
type slackConversations struct {
	channels *channelsrepo.ChannelRepository
}

func (c slackConversations) ResolveSlackConversation(
	ctx context.Context, teamID, conversationID string,
) (channelsservice.SlackDestination, error) {
	if c.channels == nil {
		return channelsservice.SlackDestination{}, errs.Unavailable("channels_unavailable",
			"the channel store is not wired in this deployment", 0)
	}
	d, err := c.channels.ResolveSlackConversation(ctx, teamID, conversationID)
	if err != nil {
		return channelsservice.SlackDestination{}, err
	}
	return channelsservice.SlackDestination{OrgID: d.OrgID, ChannelID: d.ChannelID}, nil
}

// slackActors adapts `identity/service` onto the actor port.
//
// ⭐ IT RECORDS THE SIGHTING AS WELL AS READING IT. `RecordSlackIdentity` upserts
// on `slack_identities_uniq (org_id, team_id, slack_user_id)` and refreshes the
// denormalised handle — which is exactly what that table's own comment says it is
// for: "a repeat sighting of the same Slack member is not a conflict, it is the
// same person pressing a button again". The row is what a settings screen later
// offers to LINK, so an install where nobody has linked anything still
// accumulates the identities that make linking a one-click job.
//
// ⛔ IT IS ORG-SCOPED, and never the unscoped ResolveBySlackUser. One workspace
// may be connected to two oto tenants; resolving across them would attribute a
// press in one tenant's channel to the other tenant's user. The scope comes from
// the CHANNEL, which the operator configured, so the answer is always "who is
// this Slack member inside the org that owns this conversation".
type slackActors struct {
	identity *identityservice.Service
}

func (a slackActors) SlackActor(
	ctx context.Context, s db.TenantScope, teamID, slackUserID, handle string,
) (channelsservice.SlackActor, error) {
	if a.identity == nil {
		return channelsservice.SlackActor{}, nil
	}

	si, err := a.identity.RecordSlackIdentity(ctx, s, teamID, slackUserID, handle)
	if err != nil {
		return channelsservice.SlackActor{}, err
	}
	if !si.Linked() {
		// The normal state for anybody who has not linked their account, and a
		// SUCCESS: the caller records the ack against the Slack member.
		return channelsservice.SlackActor{}, nil
	}

	user, err := a.identity.GetUser(ctx, s, si.UserID)
	if err != nil {
		// Linked to a user this org can no longer read — disabled, or removed. The
		// link is stale, not the press: fall back to the Slack handle rather than
		// losing the acknowledgement.
		if errs.IsKind(err, errs.KindNotFound) {
			return channelsservice.SlackActor{}, nil
		}
		return channelsservice.SlackActor{}, err
	}
	// The email, because that is what every other human actor's label is on this
	// timeline — the UI ack path labels by principal email — and two spellings of
	// the same person would read as two people.
	return channelsservice.SlackActor{UserID: user.ID, Label: user.Email.String()}, nil
}

// slackGroupActions adapts `grouping/service` onto the narrow verb port.
//
// ⛔ ONE VERB AND ITS UNDO. The port could name `Snooze` and `Comment` too — the
// service has them — and it deliberately does not: what a chat button may do is a
// product decision, and the narrowest possible port is where that decision is
// legible.
type slackGroupActions struct {
	grouping *groupingservice.Service
}

func (g slackGroupActions) GroupExists(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) (bool, error) {
	if g.grouping == nil {
		return false, errs.Unavailable("grouping_unavailable",
			"the grouping service is not wired in this deployment", 0)
	}
	// StateVersion is the cheapest scoped read that proves existence: one indexed
	// column, one row, and it answers NotFound for a group in another tenant —
	// which is the answer that matters.
	if _, err := g.grouping.StateVersion(ctx, s, groupID); err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g slackGroupActions) AcknowledgeGroup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel string,
) (channelsservice.GroupAckResult, error) {
	if g.grouping == nil {
		return channelsservice.GroupAckResult{}, errs.Unavailable("grouping_unavailable",
			"the grouping service is not wired in this deployment", 0)
	}
	// No note. A Slack button carries no text field, and inventing one — "acked
	// from Slack" — would put oto's words on a human's timeline entry.
	res, err := g.grouping.Acknowledge(ctx, s, groupID, actorKind, actorID, actorLabel, "")
	if err != nil {
		return channelsservice.GroupAckResult{}, err
	}
	return channelsservice.GroupAckResult{
		Members:      res.Members,
		Applied:      res.Applied,
		SkippedCodes: res.SkippedCodes,
		Unreached:    res.Unreached,
	}, nil
}

// UnacknowledgeGroup is AcknowledgeGroup read backwards, over the same fan-out and
// with the same account. Neither takes an `Idempotency-Key`: both are
// compare-and-set on the episode, so a job retry meets `already_acked` /
// `not_acked` and converges.
func (g slackGroupActions) UnacknowledgeGroup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel string,
) (channelsservice.GroupAckResult, error) {
	if g.grouping == nil {
		return channelsservice.GroupAckResult{}, errs.Unavailable("grouping_unavailable",
			"the grouping service is not wired in this deployment", 0)
	}
	// No note, for the reason given on the ack: a Slack button carries no text
	// field, and inventing one would put oto's words on a human's timeline entry.
	res, err := g.grouping.Unacknowledge(ctx, s, groupID, actorKind, actorID, actorLabel, "")
	if err != nil {
		return channelsservice.GroupAckResult{}, err
	}
	return channelsservice.GroupAckResult{
		Members:      res.Members,
		Applied:      res.Applied,
		SkippedCodes: res.SkippedCodes,
		Unreached:    res.Unreached,
	}, nil
}

// -------------------------------------------------------------------- drills

// drillIngest is `drill/service.IngestAcceptor`.
//
// ⭐⭐ IT IS THE SAME `ingestion/service.Service` THE WEBHOOK HANDLER CALLS, and
// that identity is the entire argument for the feature. If this adapter ever
// pointed somewhere else — a helper, a "fast path", a direct ObserveBatch — a
// passing drill would stop proving that the accept transaction, the outbox
// enqueue, the decoder, the bounds, the redactor and the worker all work, which
// is most of what an operator is asking about.
//
// The only thing it adds is `Mode: synthetic`, which is THE PROVENANCE MARK. It
// is set here, in the composition root, on the object that accepted the batch —
// never anywhere a payload could reach.
type drillIngest struct{ svc *ingestionservice.Service }

func (d drillIngest) Accept(
	ctx context.Context, s db.TenantScope, cmd drillservice.AcceptCommand,
) (drillservice.AcceptResult, error) {
	if d.svc == nil {
		return drillservice.AcceptResult{}, errs.Unavailable("ingestion_unavailable",
			"the ingestion service is not wired in this deployment", 0)
	}
	res, err := d.svc.Accept(ctx, s, ingestionservice.AcceptCommand{
		SourceID: cmd.SourceID,
		Body:     cmd.Body,
		Mode:     ingestiondomain.ModeSynthetic,
	})
	if err != nil {
		return drillservice.AcceptResult{}, err
	}
	return drillservice.AcceptResult{BatchID: res.BatchID, Duplicate: res.Duplicate}, nil
}

// drillSources is `drill/service.SourceReader`.
//
// It exists because a consumer-declared port may not name another domain's types
// (RULE K grants only `alerts/domain`), so the translation from
// `sources/domain.Source` into the three facts a drill needs happens here, in the
// one layer allowed to know both.
type drillSources struct {
	sources  *sourcesservice.Service
	clusters *sourcesrepo.ClusterRepository
}

func (d drillSources) DrillTarget(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (drillservice.SourceTarget, error) {
	if d.sources == nil {
		return drillservice.SourceTarget{}, errs.Unavailable("sources_unavailable",
			"the sources service is not wired in this deployment", 0)
	}
	src, err := d.sources.Get(ctx, s, id)
	if err != nil {
		return drillservice.SourceTarget{}, err
	}
	out := drillservice.SourceTarget{
		HasPrometheus: src.HasPrometheus(),
		Deleted:       src.DeletedAt != nil,
	}
	if d.clusters != nil {
		// ⭐ THE CLUSTER KEY, NOT A GUESS AT ONE. It participates in Alert identity
		// (§C.2), so a drill whose payload carried the wrong `cluster` label would
		// land in a different identity space from the source's real alerts — and
		// would then be routed by policies that a real alert from here would never
		// meet. A failure to resolve it is fatal to the drill for the same reason.
		cluster, cerr := d.clusters.Get(ctx, s, src.ClusterID)
		if cerr != nil {
			return drillservice.SourceTarget{}, cerr
		}
		out.ClusterKey = cluster.Key
	}
	return out, nil
}

// ---------------------------------------------------------------- ingestion

// ingestFeeds is `sources/api.IngestFeeds` over `ingestion/service`.
//
// ⭐ IT IS THE SEAM THAT PUTS "WHY DID MY ALERT NEVER APPEAR" ON THE SOURCE
// SCREEN. The rejection feed and the failed-batch list are ingestion's data, and
// the route that serves them sits beside `/sources/{id}/health` — because the
// ingest router is mounted with no middleware at all and a UI read cannot live
// there, and because that is where the question is asked. `sources` may not
// import `ingestion/domain` (depguard), so the reasons and statuses cross this
// boundary as plain strings and become typed here, exactly as `streamAppender`
// does for a `ui_events.kind`.
//
// An unknown member is refused by the handler's own allow-list before it reaches
// this adapter, and refused AGAIN by the service: a closed enum matches no row,
// so a typo'd reason served rather than refused would return an empty page that
// reads as "nothing was rejected".
type ingestFeeds struct {
	svc *ingestionservice.Service
}

func (f ingestFeeds) ListRejections(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID, reasons []string, p db.Keyset,
) ([]sourcesapi.RejectionEntry, db.Cursor, error) {
	if f.svc == nil {
		return nil, db.Cursor{}, errs.Unavailable("ingestion_unavailable",
			"the ingestion service is not wired in this deployment", 0)
	}
	filter := ingestiondomain.RejectionFilter{SourceID: sourceID}
	for _, r := range reasons {
		filter.Reasons = append(filter.Reasons, ingestiondomain.Reason(r))
	}
	rows, next, err := f.svc.ListRejections(ctx, s, filter, p)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	out := make([]sourcesapi.RejectionEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, sourcesapi.RejectionEntry{
			ID:         e.ID,
			SourceID:   e.SourceID,
			BatchID:    e.BatchID,
			ReceivedAt: e.ReceivedAt,
			Reason:     e.Reason.String(),
			Detail:     e.Detail,
			// Already redacted on disk (`decode.Redactor`, §C.9.2). Nothing here
			// un-redacts anything and nothing here may ever grow a way to.
			Labels: e.Labels,
		})
	}
	return out, next, nil
}

func (f ingestFeeds) ListFailedBatches(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID, statuses []string, p db.Keyset,
) ([]sourcesapi.BatchFailure, db.Cursor, error) {
	if f.svc == nil {
		return nil, db.Cursor{}, errs.Unavailable("ingestion_unavailable",
			"the ingestion service is not wired in this deployment", 0)
	}
	filter := ingestiondomain.BatchFailureFilter{SourceID: sourceID}
	for _, st := range statuses {
		filter.Statuses = append(filter.Statuses, ingestiondomain.Status(st))
	}
	rows, next, err := f.svc.ListFailedBatches(ctx, s, filter, p)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	out := make([]sourcesapi.BatchFailure, 0, len(rows))
	for _, b := range rows {
		out = append(out, sourcesapi.BatchFailure{
			ID:              b.ID,
			SourceID:        b.SourceID,
			Mode:            b.Mode.String(),
			ReceivedAt:      b.ReceivedAt,
			Status:          b.Status.String(),
			ProcessedAt:     b.ProcessedAt,
			Error:           b.Error,
			AlertCount:      b.AlertCount,
			TruncatedAlerts: b.TruncatedAlerts,
		})
	}
	return out, next, nil
}
