package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Deps is the explicit dependency set of the alerts service. It is a struct
// rather than a fifteen-argument constructor so that the wiring in
// `internal/app/container.go` reads as a list of decisions.
//
// The four repositories, the transaction runner and the clock are REQUIRED.
// Everything else is optional and degrades to the safest behaviour: no grouping,
// no notifications, no enrichment, and a reaper that holds everything. That is
// the configuration §I.1 requires oto to be runnable in.
type Deps struct {
	// Required.
	Alerts  AlertRepository
	Cases   CaseRepository
	Events  EventRepository
	Snoozes SnoozeRepository
	Tx      TxRunner

	// Optional repository capabilities. Each is satisfied by the same concrete
	// repository as its required sibling; they are separate interfaces because a
	// consumer should depend on the methods it calls and no more.
	AlertLister   AlertLister
	AlertBatch    AlertBatchReader
	OccBatch      CaseBatchReader
	OccSources    CaseSourceResolver
	SnoozeHistory SnoozeHistoryReader
	// CasePolicies reads `case_policy_config` — the case retention window W
	// (migration 00057). ⭐ UNWIRED MEANS W=0 FOR EVERY ALERT, which is the
	// pre-00057 close path exactly: a case closes on the resolve. Nothing degrades
	// and nothing is silently disabled, because 0 is also the shipped default for a
	// deployment that HAS the port.
	CasePolicies CasePolicyRepository
	// CasePolicyConfig is the SETTINGS side of the same table — list, create,
	// patch, delete. ⭐ UNWIRED MEANS THE FOUR `/api/v1/case-policies` OPERATIONS
	// ANSWER `503`, and nothing else changes: reading W does not depend on it, so a
	// deployment that omits it behaves exactly as one whose table is empty.
	CasePolicyConfig CasePolicyConfigStore

	// Claims is the `Idempotency-Key` claim store (§E.1). It is optional in the
	// same sense the rest of this block is — oto runs without it — but a KEYED
	// request arriving at a deployment that lacks it is REFUSED with a `503`
	// rather than served unguarded. See requireClaims.
	Claims Claims

	// Optional cross-module ports.
	Enqueuer      db.Enqueuer
	Stream        StreamAppender
	Health        SourceHealth
	Settings      SettingsReader
	GroupVersions GroupVersionReader
	Enrichments   EnrichmentReader
	Notifications NotificationReader

	Clock  clock.Clock
	Logger *slog.Logger
}

// Service is the alerts module's business logic: identity and dedup, the §B.3
// case state machine, the append-only timeline, snooze, and the read paths
// the API serves.
//
// ⛔ It NEVER calls time.Now(). Every clock reading comes from the injected
// clock, which is what makes the reaper and the re-fire grace testable without
// sleeping.
//
// ⛔ It NEVER imports `notification` or `channels` (§I.1). It appends events and
// enqueues jobs; notification subscribes.
type Service struct {
	alerts      AlertRepository
	lister      AlertLister
	alertBatch  AlertBatchReader
	cases       CaseRepository
	occBatch    CaseBatchReader
	occSources  CaseSourceResolver
	casePolicy  CasePolicyRepository
	casePolicyW CasePolicyConfigStore
	events      EventRepository
	snoozes     SnoozeRepository
	snoozeHist  SnoozeHistoryReader

	tx            TxRunner
	claims        Claims
	enqueuer      db.Enqueuer
	stream        StreamAppender
	health        SourceHealth
	settings      SettingsReader
	groupVersions GroupVersionReader
	enrichments   EnrichmentReader
	notifications NotificationReader

	clock clock.Clock
	log   *slog.Logger
}

// New builds the alerts service, refusing a dependency set that cannot work.
func New(d Deps) (*Service, error) {
	switch {
	case d.Alerts == nil:
		return nil, errs.Internal("alerts_repo_required", errMissingDep("AlertRepository"))
	case d.Cases == nil:
		return nil, errs.Internal("case_repo_required", errMissingDep("CaseRepository"))
	case d.Events == nil:
		return nil, errs.Internal("event_repo_required", errMissingDep("EventRepository"))
	case d.Snoozes == nil:
		return nil, errs.Internal("snooze_repo_required", errMissingDep("SnoozeRepository"))
	case d.Tx == nil:
		return nil, errs.Internal("tx_runner_required", errMissingDep("TxRunner"))
	}

	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		alerts:        d.Alerts,
		lister:        d.AlertLister,
		alertBatch:    d.AlertBatch,
		cases:         d.Cases,
		occBatch:      d.OccBatch,
		occSources:    d.OccSources,
		casePolicy:    d.CasePolicies,
		casePolicyW:   d.CasePolicyConfig,
		events:        d.Events,
		snoozes:       d.Snoozes,
		snoozeHist:    d.SnoozeHistory,
		tx:            d.Tx,
		claims:        d.Claims,
		enqueuer:      d.Enqueuer,
		stream:        d.Stream,
		health:        d.Health,
		settings:      d.Settings,
		groupVersions: d.GroupVersions,
		enrichments:   d.Enrichments,
		notifications: d.Notifications,
		clock:         clk,
		log:           logger,
	}, nil
}

func errMissingDep(name string) error {
	return errs.New(errs.KindInternal, "missing_dependency", "alerts service requires "+name)
}

// Now is the service's clock reading, in UTC. It exists so that callers
// assembling a domain command share one instant with the service that will
// persist it.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// lifecycleSettings resolves the org's tuning, falling back to the §D.1 defaults
// when no reader is wired or the reader fails. A settings lookup must never be
// able to stop an alert being recorded.
func (s *Service) lifecycleSettings(ctx context.Context, scope db.TenantScope) Settings {
	if s.settings == nil {
		return DefaultSettings()
	}
	cfg, err := s.settings.Lifecycle(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "alerts: falling back to default lifecycle settings",
			"org_id", scope.OrgID(), "error", err)
		return DefaultSettings()
	}
	return cfg.normalise()
}

// ------------------------------------------------------------------- events

// appendEvents writes the timeline entries and publishes the matching UI frames.
//
// It is only ever called inside a transaction: the event, its idempotency key,
// the projection it explains and the SSE frame that announces it all commit
// together, or none of them do.
func (s *Service) appendEvents(ctx context.Context, scope db.TenantScope, evs []domain.Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	if err := refuseRetired(evs); err != nil {
		return 0, err
	}
	n, err := s.events.AppendBatch(ctx, scope, evs)
	if err != nil {
		return 0, err
	}
	for _, e := range evs {
		if err := s.publishEvent(ctx, scope, e); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// refuseRetired is this module's half of the retirement guarantee, and it exists
// because `seam.go`'s is deliberately narrower than `alert_events`.
//
// ⭐⭐ THE SEAM COULD NOT COVER `case.reopened`. `AppendTimelineEvent` refuses a
// retired type for every caller OUTSIDE `alerts`, which was the whole population
// for `group.member_joined` and `group.member_left`. `case.reopened` was minted
// by this module's own transition table and reached the column through the two
// functions above, which never touch the seam — so ADR 0040 retiring it needed a
// refusal HERE or the retirement would have been a comment.
//
// ⛔ IT IS `KindInternal` AND NOT `KindValidation`, for the reason seam.go gives:
// no request can ask for a retired type, so reaching this is code asking for it.
// The transition rows that built one are gone, which is what should make this
// unreachable; this is what makes "should" into "cannot".
func refuseRetired(evs []domain.Event) error {
	for _, e := range evs {
		if e.Type().Retired() {
			return errs.Newf(errs.KindInternal, "event_type_retired",
				"%q is a retired alert_events.type and may be read but never appended", e.Type())
		}
	}
	return nil
}

// appendEventsBatched is appendEvents for the observe path: the timeline write
// is the same one round trip, but the matching UI frames are QUEUED on the
// accumulator for the batch's single flush instead of being published one by
// one. The frames land after the case and alert frames the loop queued,
// which is exactly where the per-event appends used to put them.
func (s *Service) appendEventsBatched(ctx context.Context, scope db.TenantScope, acc *observeAccum) (int, error) {
	if len(acc.events) == 0 {
		return 0, nil
	}
	if err := refuseRetired(acc.events); err != nil {
		return 0, err
	}
	n, err := s.events.AppendBatch(ctx, scope, acc.events)
	if err != nil {
		return 0, err
	}
	for _, e := range acc.events {
		if err := s.queueFrame(acc, StreamEventAppended, e.ID(), eventFramePayload(e)); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// publishEvent announces one timeline entry on the SSE spine. The envelope is a
// CHANGE NOTICE, not a resource (§E.4): the client re-reads for detail.
func (s *Service) publishEvent(ctx context.Context, scope db.TenantScope, e domain.Event) error {
	if s.stream == nil {
		return nil
	}
	return s.publish(ctx, scope, StreamEventAppended, e.ID(), eventFramePayload(e))
}

// eventFramePayload is the §E.4 envelope of one timeline entry, shared by the
// per-event publish and the batched flush so the two can never drift apart.
func eventFramePayload(e domain.Event) map[string]any {
	payload := map[string]any{"type": e.Type().String()}
	if e.AlertID() != uuid.Nil {
		payload["alert_id"] = e.AlertID()
	}
	if e.GroupID() != uuid.Nil {
		payload["group_id"] = e.GroupID()
	}
	if e.CaseID() != uuid.Nil {
		payload["case_id"] = e.CaseID()
	}
	return payload
}

// publishAlert announces that an Alert row changed.
func (s *Service) publishAlert(ctx context.Context, scope db.TenantScope, alertID uuid.UUID, extra map[string]any) error {
	if s.stream == nil {
		return nil
	}
	return s.publish(ctx, scope, StreamAlertUpserted, alertID, extra)
}

// publishCase announces that an episode changed.
func (s *Service) publishCase(ctx context.Context, scope db.TenantScope, o domain.Case) error {
	if s.stream == nil {
		return nil
	}
	return s.publish(ctx, scope, StreamCaseUpserted, o.ID(), caseFramePayload(o))
}

// caseFramePayload is the §E.4 envelope of one episode change, shared by
// the per-item publish and the batched observe path.
//
// ⭐ `state` HERE IS THE CASE'S OWN, `open | closed` (ADR 0040), and the alert
// frame published beside it in the same flush carries the four §B.2 words. That
// is the split on purpose: this frame says an EPISODE moved, and the only thing
// an episode can say about itself is whether it is still running.
func caseFramePayload(o domain.Case) map[string]any {
	return map[string]any{
		"alert_id": o.AlertID(),
		"state":    o.State().String(),
		"ack":      o.AckState().String(),
	}
}

func (s *Service) publish(
	ctx context.Context, scope db.TenantScope, kind string, resourceID uuid.UUID, payload map[string]any,
) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := encodeFramePayload(payload)
	if err != nil {
		return err
	}
	if err := s.stream.Append(ctx, scope, kind, resourceID, raw); err != nil {
		return err
	}
	return nil
}

// queueFrame stages one §E.4 change notice on the accumulator for the batch's
// single flush. It is publish() with the round trip deferred: the encoding, its
// error and the nil-stream degradation are identical, so a queued frame and a
// published one can never disagree about anything but WHEN the round trip runs.
func (s *Service) queueFrame(
	acc *observeAccum, kind string, resourceID uuid.UUID, payload map[string]any,
) error {
	if s.stream == nil {
		return nil
	}
	raw, err := encodeFramePayload(payload)
	if err != nil {
		return err
	}
	acc.frames = append(acc.frames, StreamFrame{Kind: kind, ResourceID: resourceID, Payload: raw})
	return nil
}

// flushFrames publishes every queued frame of one batch in ONE round trip. A
// failed flush fails the caller's transaction exactly as a failed per-item
// Append did: none of the state the frames describe survives either.
func (s *Service) flushFrames(ctx context.Context, scope db.TenantScope, frames []StreamFrame) error {
	if s.stream == nil || len(frames) == 0 {
		return nil
	}
	return s.stream.AppendBatch(ctx, scope, frames)
}

func encodeFramePayload(payload map[string]any) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.Internal("ui_event_encode_failed", err)
	}
	return raw, nil
}

// --------------------------------------------------------------------- jobs

// notifyRequest is one queued `notify.evaluate`. It is assembled here rather
// than at each call site so that the §C.7 inputs — group, reason, state version —
// are never partially filled.
type notifyRequest struct {
	groupID uuid.UUID
	reason  string
	alertID *uuid.UUID
	caseID  *uuid.UUID
	actor   string
}

// enqueueNotify queues policy evaluation IN THE CALLER'S TRANSACTION.
//
// ⭐ This is oto's transactional outbox (ADR 0001): the job and the state change
// that justifies it commit together, which is what makes "202 Accepted is a
// promise" true (§G.1). A request whose group is unknown is DROPPED, not
// guessed: `notifications.group_id` is NOT NULL and a fabricated group would
// mint an intent about nothing.
// awaitingEnrichment is the set of case ids whose inline enrichment pass
// was queued in this same transaction; their `fired` evaluation is scheduled at
// the end of the pre-notification budget instead of immediately.
//
// ⭐ THIS IS THE FIX FOR "THE RULE IS NOT ON THE FIRST CARD" AND IT IS A JOB
// SCHEDULE, NOT A LONGER BUDGET. §F.3 already says the inline phase "runs inside
// the pre-notification budget"; nothing enforced the ordering, so `enrich.run`
// and `notify.evaluate` were enqueued together and raced. `notify.evaluate` won,
// the first card had no rule block, and the PromQL snapshot — the one thing oto
// has that nothing else does — appeared later under a SILENT `chat.update` that
// nobody is notified about.
//
// The delay is a CEILING and almost never paid in full: the pipeline enqueues
// the same evaluation the instant its inline pass completes, and the two collapse
// on `notifications_idem_uniq` (§C.7). What is left is the guarantee that matters
// — if the enrich worker is dead, backed up, or the org has enrichment switched
// off, the card still goes out, without the rule, at the budget's edge. Silence
// is never the degradation.
func (s *Service) enqueueNotify(
	ctx context.Context, scope db.TenantScope, reqs []notifyRequest,
	awaitingEnrichment map[uuid.UUID]struct{},
) (int, error) {
	if s.enqueuer == nil || len(reqs) == 0 {
		return 0, nil
	}

	versions := map[uuid.UUID]int{}
	out := make([]db.JobRequest, 0, len(reqs))
	for _, r := range reqs {
		if r.groupID == uuid.Nil || r.reason == "" {
			continue
		}
		v, ok := versions[r.groupID]
		if !ok {
			v = s.groupStateVersion(ctx, scope, r.groupID)
			versions[r.groupID] = v
		}
		req := db.JobRequest{Args: jobs.NotifyEvaluateArgs{
			GroupID:      r.groupID,
			Reason:       r.reason,
			StateVersion: v,
			AlertID:      r.alertID,
			CaseID:       r.caseID,
			Actor:        r.actor,
		}}
		if r.reason == reasonFired && r.caseID != nil {
			if _, waiting := awaitingEnrichment[*r.caseID]; waiting {
				req.Opts = append(req.Opts,
					db.WithScheduledAt(s.Now().Add(jobs.PreNotificationBudget)))
			}
		}
		out = append(out, req)
	}
	if len(out) == 0 {
		return 0, nil
	}
	if _, err := s.enqueuer.EnqueueMany(ctx, out); err != nil {
		return 0, errs.Wrap(err, errs.KindInternal, "enqueue_notify_failed",
			"could not queue notification evaluation")
	}
	return len(out), nil
}

func (s *Service) groupStateVersion(ctx context.Context, scope db.TenantScope, groupID uuid.UUID) int {
	if s.groupVersions == nil {
		return 0
	}
	v, err := s.groupVersions.StateVersion(ctx, scope, groupID)
	if err != nil {
		s.log.WarnContext(ctx, "alerts: could not read group state version",
			"group_id", groupID, "error", err)
		return 0
	}
	return v
}

// enqueueEnrich queues the inline enrichment pass for a freshly opened episode.
// Enrichment is NEVER on the ingest critical path; this is a queue insert in the
// same transaction and nothing more.
func (s *Service) enqueueEnrich(ctx context.Context, caseIDs []uuid.UUID) (int, error) {
	if s.enqueuer == nil || len(caseIDs) == 0 {
		return 0, nil
	}
	reqs := make([]db.JobRequest, 0, len(caseIDs))
	for _, id := range caseIDs {
		reqs = append(reqs, db.JobRequest{Args: jobs.EnrichRunArgs{
			CaseID: id,
			Phase:  "inline",
		}})
	}
	if _, err := s.enqueuer.EnqueueMany(ctx, reqs); err != nil {
		return 0, errs.Wrap(err, errs.KindInternal, "enqueue_enrich_failed",
			"could not queue enrichment")
	}
	return len(reqs), nil
}

// ------------------------------------------------------------- notify reasons

// The §H.6 Reason values this module produces. They are the vocabulary
// `notifications.reason` accepts, and adding one requires a SPEC amendment plus a
// migration to notifications_reason_ck.
const (
	reasonFired        = "fired"
	reasonSomeResolved = "some_resolved"
	reasonSuppressed   = "suppressed"
	reasonUnsuppressed = "unsuppressed"
	reasonExpired      = "expired"
	reasonRefired      = "refired"
	reasonAcked        = "acked"
	reasonUnacked      = "unacked"
	reasonComment      = "comment"
	reasonSnoozed      = domain.NotifyReasonSnoozed
	reasonUnsnoozed    = domain.NotifyReasonUnsnoozed
)

// reasonFor maps a §B.3 transition onto the §H.6 Reason it justifies.
//
// T5 maps to `some_resolved` and never to `all_resolved`: whether a group is
// wholly resolved is a fact about the GROUP's membership, which this module does
// not read. The notify worker, which does, upgrades it.
func reasonFor(id domain.TransitionID) string {
	switch id {
	case domain.TransitionT1, domain.TransitionT7:
		return reasonFired
	case domain.TransitionT3:
		return reasonSuppressed
	case domain.TransitionT4:
		return reasonUnsuppressed
	case domain.TransitionT5:
		return reasonSomeResolved
	case domain.TransitionT6:
		return reasonExpired
	default:
		// T2 changes nothing anybody needs to be told about. A repeat
		// observation is not news.
		return ""
	}
}
