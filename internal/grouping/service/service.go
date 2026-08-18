package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Deps is the explicit dependency set of the grouping service.
//
// The two repositories, the transaction runner and the group-key function are
// required. Events, streaming, notification and member actions are optional and
// degrade to doing nothing, so oto runs with notifications entirely disabled.
type Deps struct {
	Groups  GroupRepository
	Members MemberRepository
	Tx      TxRunner

	Events   EventAppender
	Timeline TimelineReader
	Actions  MemberActions
	Stream   StreamAppender
	Enqueuer db.Enqueuer
	Settings SettingsReader

	Clock  clock.Clock
	Logger *slog.Logger
}

// Service owns durable AlertGroups: their §C.4 identity, their generations, their
// membership and their rollups. ⛔ It no longer damps anything: storm collapse was
// this module's one damper and it is removed (domain/lifecycle.go).
//
// ⛔ An AlertGroup here is ONE GENERATION of ONE Alertmanager notification group,
// and it OWNS EXACTLY ONE Slack thread. It is never a UI grouping — that is a
// view, and a view has no row, no thread and no generation (§A.1).
//
// ⛔ It NEVER calls time.Now(); every instant comes from the injected clock.
type Service struct {
	groups  GroupRepository
	members MemberRepository
	tx      TxRunner

	events   EventAppender
	timeline TimelineReader
	actions  MemberActions
	stream   StreamAppender
	enqueuer db.Enqueuer
	settings SettingsReader

	clock clock.Clock
	log   *slog.Logger
}

// New builds the grouping service, refusing a dependency set that cannot work.
func New(d Deps) (*Service, error) {
	switch {
	case d.Groups == nil:
		return nil, errs.Internal("group_repo_required", errMissingDep("GroupRepository"))
	case d.Members == nil:
		return nil, errs.Internal("member_repo_required", errMissingDep("MemberRepository"))
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
		groups:   d.Groups,
		members:  d.Members,
		tx:       d.Tx,
		events:   d.Events,
		timeline: d.Timeline,
		actions:  d.Actions,
		stream:   d.Stream,
		enqueuer: d.Enqueuer,
		settings: d.Settings,
		clock:    clk,
		log:      logger,
	}, nil
}

func errMissingDep(name string) error {
	return errs.New(errs.KindInternal, "missing_dependency", "grouping service requires "+name)
}

// errFanOutSettled stops a fan-out because the caller's `Idempotency-Key` was
// already claimed: the gesture landed on a previous request, and every member
// behind this one was dealt with then.
//
// ⛔ IT IS NOT AN ERROR THE CALLER EVER SEES. `fanOut` recognises it, returns the
// partial account with a nil error, and the handler answers exactly as it would
// have on the first attempt. A sentinel is used rather than a second return value
// on `apply` because the only thing `apply` may otherwise say is "this member
// refused", and a replay is a statement about the WHOLE gesture.
var errFanOutSettled = errors.New("the caller's idempotency key was already claimed")

// Now is the service's clock reading, in UTC.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

// policy reads the org's generation-lifecycle tuning, degrading to §D.1's default on
// any failure: a settings lookup must never be able to stop a group being recomputed.
//
// ⛔ IT WAS `domain.StormPolicy` AND IT CARRIED FOUR NUMBERS. Three of them were the
// storm knobs; storm damping is removed, so `group_close_delay_s` is the whole policy
// now. The three `storm_*` keys REMAIN readable on `orgs.settings` and decide nothing.
func (s *Service) policy(ctx context.Context, scope db.TenantScope) domain.LifecyclePolicy {
	if s.settings == nil {
		return domain.DefaultLifecyclePolicy()
	}
	p, err := s.settings.GroupLifecycle(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "grouping: falling back to the default lifecycle policy",
			"org_id", scope.OrgID(), "error", err)
		return domain.DefaultLifecyclePolicy()
	}
	return p.Normalise()
}

// ------------------------------------------------------------------- resolve

// ResolveRequest names the AlertGroup an observation belongs to.
type ResolveRequest struct {
	SourceID  uuid.UUID
	ClusterID uuid.UUID
	// ClusterKey is the failure domain's stable machine name and the FIRST axis of
	// the §C.4 key. It is resolved from the source's configuration, never read out
	// of a label, and it is never absent: it participates in Alert identity (§C.2).
	ClusterKey kernel.ClusterKey
	// Labels is the ALERT'S OWN label set — the second and third axes (`alertname`
	// and `namespace-or-∅`) are projected out of it by kernel.SplitLabels.
	//
	// ⭐ It is the alert's labels and not Alertmanager's groupLabels because those
	// are the only labels present on EVERY alert on BOTH ingest paths.
	Labels kernel.LabelSet
	// Receiver is the Alertmanager receiver name, "" for a reconciler-sourced
	// group. PROVENANCE ONLY — it left the key with ADR 0038.
	Receiver string
	// SourceGroupKey is Alertmanager's raw groupKey. It is stored VERBATIM for
	// observability and MUST NOT be parsed: it is unescaped and unbounded (C3).
	SourceGroupKey string
	// NotificationReason is Alertmanager's own notification_reason for this
	// delivery, feeding the §H.6 decision table.
	NotificationReason string
	At                 time.Time
	// Synthetic marks a generation opened by a DELIVERY DRILL. It is carried from
	// the observations that triggered the resolve — which carry it from
	// `ingest_batches.mode` — and it is written to `alert_groups.synthetic` so
	// the dashboard counts can exclude drills.
	//
	// ⛔ It changes NOTHING about how the group behaves. A drill that took a
	// different code path anywhere below this line would stop being evidence.
	Synthetic bool
}

// Resolve returns the OPEN generation for a group key, opening one if none is
// open (§G.4 step 4).
//
// ⭐ The key is the DURABLE §C.4 key and never Alertmanager's `groupKey`. AM's
// value embeds the route path and changes on every `alertmanager.yml` reload, so
// a group keyed by it would be reborn — with a new Slack thread — every time an
// operator edited a route.
//
// ⭐ AND IT IS DERIVED, NOT MIRRORED. `(org, cluster, alertname, namespace-or-∅)`
// is computed here from the alert's own labels, identically for the webhook path
// and the reconciler path, and it is FIXED rather than configurable (ADR 0038).
// The same projection is stored as the generation's `group_labels`, so a
// notification policy matching `namespace` now matches something regardless of
// what the operator put in `group_by`.
//
// ⭐ Opening a generation when the previous one CLOSED is what gives a re-opened
// group a new thread: yesterday's conversation is not where today's incident
// belongs. Rejoining a still-open generation reuses the thread, which is why
// `chat.update` is the primary Slack verb and a new root message is the exception
// (§B.5).
func (s *Service) Resolve(
	ctx context.Context, scope db.TenantScope, in ResolveRequest,
) (domain.Group, error) {
	// The §C.4 identity function lives in the shared domain kernel and is called
	// directly: depguard RULE K sanctions the import, and re-exporting a pure
	// identity function through a service would give it a fake dependency on a
	// database.
	if scope.OrgID() == uuid.Nil || in.SourceID == uuid.Nil {
		return domain.Group{}, errs.Validation("group_key_inputs_required",
			"a group key needs both an org and a source")
	}
	if in.ClusterKey.IsZero() || in.Labels.IsZero() {
		// Both are guaranteed by §C.2 — an Observation that reached here without
		// them could not have produced an alert_key either — so this is a layer-3
		// invariant rather than input validation. It degrades in the orchestrator
		// (the alert is recorded without a group) instead of costing the batch.
		return domain.Group{}, errs.Validation("group_key_inputs_required",
			"a group key needs the alert's cluster and its labels")
	}
	// ⛔ The split labels are the GROUP's labels from here on. Alertmanager's own
	// groupLabels are not an input, are not stored, and are not consulted: the raw
	// envelope is already on disk in `ingest_batches.payload`.
	split := kernel.SplitLabels(in.Labels).Map()
	key := kernel.ComputeGroupKey(scope.OrgID(), in.ClusterKey, in.Labels).String()
	at := in.At
	if at.IsZero() {
		at = s.Now()
	}

	var out domain.Group
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		if g, ok, err := s.groups.GetOpenByKey(ctx, scope, key); err != nil {
			return err
		} else if ok {
			if err := s.groups.Touch(ctx, scope, g.ID(), at); err != nil {
				return err
			}
			if in.NotificationReason != "" {
				if err := s.groups.SetNotificationReason(ctx, scope, g.ID(),
					in.NotificationReason); err != nil {
					return err
				}
			}
			out = g.Touch(at)
			return nil
		}

		g, err := s.groups.OpenGeneration(ctx, scope, repository.NewGeneration{
			ID:             id.New(),
			SourceID:       in.SourceID,
			ClusterID:      in.ClusterID,
			GroupKey:       key,
			SourceGroupKey: in.SourceGroupKey,
			Receiver:       in.Receiver,
			GroupLabels:    split,
			// The fallback is never reached: `split` always carries a non-empty
			// alertname, so Title() always has a name to render. It is passed anyway
			// because Title's signature is total and a call site that pretends a
			// branch cannot happen is how it starts happening.
			Title:     domain.Title(split, in.Receiver),
			At:        at,
			Synthetic: in.Synthetic,
		})
		if err != nil {
			// Two ingest workers can race the same first observation. The unique
			// key (org_id, group_key, generation) decides it; the loser re-reads
			// rather than opening a second live generation of one group.
			if errs.IsKind(err, errs.KindConflict) {
				if g, ok, rerr := s.groups.GetOpenByKey(ctx, scope, key); rerr == nil && ok {
					out = g
					return nil
				}
			}
			return err
		}

		if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
			Type:    kernel.EventGroupOpened,
			GroupID: g.ID(),
			Summary: "Alert group opened: " + g.Title(),
			Payload: map[string]any{
				"group_key":  g.Key().String(),
				"generation": g.Generation(),
				"receiver":   g.Receiver(),
			},
			DedupeKey:  "group:" + g.ID().String() + ":opened",
			OccurredAt: at,
		}); err != nil {
			return err
		}
		if err := s.publish(ctx, scope, g); err != nil {
			return err
		}
		out = g
		return nil
	})
	if err != nil {
		return domain.Group{}, err
	}
	return out, nil
}

// -------------------------------------------------------------- membership

// ⛔ THERE IS NO `Join`, NO `JoinMany` AND NO `Leave`, AND THERE MUST NOT BE.
//
// Membership is not something this service does; it is something an episode HAS.
// Since ADR 0038 the group key is derived from the alert's own labels, so an
// episode belongs to exactly one generation, and `alert_cases.group_id` —
// written once by `alerts`, at the moment the episode opens — is the whole record
// (migration 00051). There is nothing left for a membership verb to write.
//
// `Leave` is the one worth naming, because it existed at three layers, appended
// `group.member_left`, and WAS CALLED FROM NOWHERE for its entire life. The
// consequence was not a missing feature: it was that `left_at` was never set, so
// every "current members" read matched every row ever inserted, the point-in-time
// replay could only show growth, `gm_current_idx` narrowed nothing, and an alert
// that resolved and re-fired inside one generation was listed twice — once
// resolved, once firing. Wiring it up was considered and rejected. A human does
// not end an episode's membership of a group; the episode ending is what ends it,
// and that fact already had a column.
//
// `group.member_joined` and `group.member_left` went with them. They were facts
// about the EPISODE phrased as if the group were the actor, and each is implied by
// one that survives: `case.opened` and `case.resolved`/`.expired`.
// They remain in the closed EventType enum, unemitted, because thirteen months of
// `alert_events` still contain them — see `kernel.EventType.Retired`.

// Recompute re-derives EVERYTHING about a generation that is a projection of its
// members, and it is the only entry point that does.
//
// It re-rolls the counts, the state and the severity. It is called ONCE PER GROUP PER
// BATCH by the ingest orchestrator, and again by nothing else.
//
// ⭐ ONCE PER BATCH, NOT ONCE PER MEMBER. A rollup is a PURE PROJECTION of the
// members (see `recompute`), so a 500-alert Alertmanager batch that recomputed per
// member would produce 499 results nobody reads — each one a full aggregate over
// the group plus a compare-and-set write to the same `alert_groups` row, which the
// CAS then serialises. That is O(n) contention on one row, arriving exactly when
// Alertmanager's ~5-minute retry budget (ADR 0007, `ingestion/api/shed.go`) is the
// only thing between a slow ingest and an alert that is lost silently.
//
// ⛔ THE §B.6 STORM EVALUATION USED TO RUN HERE, once per batch, and it is deleted.
// It counted DISTINCT ALERTS THAT JOINED INSIDE A WINDOW and, above the threshold,
// moved the generation into `storm_mode`, appended `group.storm_started` and enqueued
// a `notify.evaluate` carrying reason `storm` — which the notification layer turned
// into ONE root card, no per-alert replies, and a once-per-channel notice that oto had
// started withholding. All of it is gone: the collapse was oto deciding that many real
// firings were not worth mentioning individually, and the thirty-nine replies it
// dropped left no trace an operator could read. See the tombstone at the top of
// `domain/lifecycle.go`. Nothing sets `alert_groups.storm_mode`, so it reads false.
func (s *Service) Recompute(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) (domain.Group, error) {
	if at.IsZero() {
		at = s.Now()
	}
	// ⛔ THE PER-BATCH POLICY READ WENT WITH THE STORM EVALUATION. It existed so the
	// tuning could not change inside one batch and so `orgs.settings` was not re-read
	// per member — the same 500× waste in miniature. The rollup reads no org setting
	// at all, so there is nothing left to read here; `CloseIdle` still reads its own.
	var out domain.Group
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		g, material, err := s.recompute(ctx, scope, groupID, at)
		if err != nil {
			return err
		}
		out = g

		if material {
			return s.publish(ctx, scope, out)
		}
		return nil
	})
	if err != nil {
		return domain.Group{}, err
	}
	return out, nil
}

// recompute rolls the current members up onto the generation and reports whether
// anything MATERIAL moved.
//
// Material means a count changed or the severity changed. Only a material change
// bumps `state_version`, and only a bumped version lets a new Notification exist
// (§C.7) — which is exactly what stops a repeat observation re-notifying.
// It RE-READS AND RECOMPUTES when it loses the `state_version` compare-and-set.
// Read-recompute-write at READ COMMITTED is exactly the shape the alerts reaper
// got wrong: without the version predicate two concurrent recomputes both derive
// version N+1 from N, both write it, and the loser's counts vanish while §C.7's
// idempotency key claims both states are the same fact. Retrying is always
// correct here because a rollup is a PURE PROJECTION of the current members —
// recomputing it from a fresh read discards nothing.
func (s *Service) recompute(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) (domain.Group, bool, error) {
	for attempt := 1; ; attempt++ {
		g, err := s.groups.GetByID(ctx, scope, groupID)
		if err != nil {
			return domain.Group{}, false, err
		}
		counts, severity, err := s.members.Rollup(ctx, scope, groupID)
		if err != nil {
			return domain.Group{}, false, err
		}
		next, material, err := g.WithRollup(counts, severity, at)
		if err != nil {
			return domain.Group{}, false, err
		}
		if !material {
			return next, false, nil
		}
		err = s.groups.SetRollup(ctx, scope, next, g.StateVersion())
		if err == nil {
			return next, true, nil
		}
		if !errs.IsKind(err, errs.KindConflict) || attempt >= groupMaxAttempts {
			return domain.Group{}, false, err
		}
		s.log.InfoContext(ctx, "grouping: rollup lost the state_version compare-and-set, recomputing",
			"group_id", groupID, "attempt", attempt)
	}
}

// groupMaxAttempts bounds every optimistic-lock retry in this file. A generation
// that loses three in a row is contending with a writer that is winning every
// time; the conflict is returned and the caller's own retry budget takes over
// rather than spinning a worker.
const groupMaxAttempts = 3

// ⛔⛔ `stormOutcome` AND `evaluateStorm` WERE HERE AND ARE DELETED, along with the
// `SetStorm` write, the `DistinctJoinsSince` count, the `group.storm_started` /
// `group.storm_ended` timeline rows and the `notify.evaluate` job carrying reason
// `storm`.
//
// ⭐ WHAT IT DID. It counted distinct alerts that joined inside `storm_window`, and
// above `storm_threshold` it flipped `alert_groups.storm_mode`, appended a timeline
// event echoing the policy IN FORCE (so a reader saw the numbers that applied, not the
// ones configured later), and announced the transition — because a damper that does
// not announce itself is exactly the silent suppression §B.6 forbids. It ended after
// `storm_cooldown` without a new member, and it ran on resolve-only batches too so a
// storm ended on the batch that proved the flood was over.
//
// ⭐⭐ AND WHY EVERY LINE OF THAT WAS THE WRONG SHAPE. The announcement made the
// GROUP's collapse visible; it did not make the thirty-nine withheld replies visible,
// and those are what an operator would have read. So a suppressed notification stayed
// indistinguishable from a signal that never fired. The deeper fault is that a storm
// is many DIFFERENT alerts arriving together and the object that owns that is an
// INCIDENT (`correlation`, DEFERRED-POST-V1): with no such object, the detector had
// nowhere to put its verdict and put it in the notification layer, which is how it
// became a damper at all. `domain/lifecycle.go` carries the full tombstone.
//
// ⛔ `alert_groups.storm_mode` AND `storm_since` STAY, INERT. Nothing sets them and
// `Close` still clears them on a generation that predates the removal. Dropping the
// columns, `groups_storm_ck` and the three `storm_*` settings keys is the deferred,
// breaking half of this change.

// ------------------------------------------------------------------- close

// CloseResult is the audit of one `group.close` tick.
type CloseResult struct {
	Considered int
	Closed     int
	// Held is how many idle generations still had a live member and were left
	// open. Freezing the thread of a live incident is worse than a stale card.
	Held int
}

// CloseIdle is the `group.close` sweep (§G.3): open generations idle past
// group_close_delay_s, with no member still firing or suppressed.
//
// Closing freezes the generation's Slack thread. The next observation for the
// same group key opens generation N+1 and therefore a NEW thread — which is the
// entire reason generations exist (§B.5).
func (s *Service) CloseIdle(ctx context.Context, scope db.TenantScope, limit int) (CloseResult, error) {
	if limit <= 0 {
		limit = 200
	}
	p := s.policy(ctx, scope)
	now := s.Now()

	candidates, err := s.groups.CloseCandidates(ctx, scope, now.Add(-p.CloseDelay), limit)
	if err != nil {
		return CloseResult{}, err
	}
	res := CloseResult{Considered: len(candidates)}

	for _, g := range candidates {
		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			// Re-derive the rollup first: `last_activity_at` says nothing was
			// WRITTEN recently, not that nothing is FIRING.
			fresh, _, err := s.recompute(ctx, scope, g.ID(), now)
			if err != nil {
				return err
			}
			if !fresh.CanCloseAt(now, p.CloseDelay) {
				res.Held++
				return nil
			}
			closed, err := fresh.Close(now)
			if err != nil {
				return err
			}
			// `fresh` is what proved no member is still live, so `fresh`'s version
			// is what the close is entitled to overwrite. A member that joined in
			// between bumps it, the close is refused, and the live incident keeps
			// its thread.
			if err := s.groups.Close(ctx, scope, closed, fresh.StateVersion()); err != nil {
				return err
			}
			if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
				Type:    kernel.EventGroupClosed,
				GroupID: closed.ID(),
				Summary: "Alert group closed: " + closed.Title(),
				Payload: map[string]any{
					"generation":    closed.Generation(),
					"total":         closed.Counts().Total,
					"state_version": closed.StateVersion(),
				},
				DedupeKey:  "group:" + closed.ID().String() + ":closed",
				OccurredAt: now,
			}); err != nil {
				return err
			}
			if err := s.publish(ctx, scope, closed); err != nil {
				return err
			}
			res.Closed++
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "grouping: could not close group",
				"group_id", g.ID(), "error", err)
		}
	}
	return res, nil
}

// ------------------------------------------------------------------ helpers

func (s *Service) appendGroupEvent(
	ctx context.Context, scope db.TenantScope, in alerts.TimelineEventRequest,
) error {
	if s.events == nil {
		return nil
	}
	return s.events.AppendTimelineEvent(ctx, scope, in)
}

func (s *Service) publish(ctx context.Context, scope db.TenantScope, g domain.Group) error {
	if s.stream == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"group_id":   g.ID(),
		"state":      g.State().String(),
		"generation": g.Generation(),
		"firing":     g.Counts().Firing,
		"total":      g.Counts().Total,
		"acked":      g.Counts().Acked,
		// ⛔ `storm_mode` WAS THE NEXT KEY AND IT IS GONE (migration 00059). It rode
		// the frame as a permanent `false` after storm damping was removed, on the
		// argument that `group.upserted` is a published contract (SPEC §J). The
		// column it mirrored no longer exists, and a published field that can only
		// ever say one thing is not a contract, it is a decoy: a reader wiring a UI
		// against it would be filtering on an axis oto does not have.
		"state_version": g.StateVersion(),
	})
	if err != nil {
		return errs.Internal("ui_event_encode_failed", err)
	}
	return s.stream.Append(ctx, scope, StreamGroupUpserted, g.ID(), payload)
}

// ⛔ `notifyReasonStorm` WAS HERE AND IS DELETED. It was the §H.6 Reason a storm-mode
// transition enqueued, and `notification/domain.ReasonStorm` is deleted too:
// migration 00060 narrows `notifications_reason_ck` to the eighteen that remain, so
// the value is not decodable either. Nothing in this module may name it again.

// enqueueNotify queues policy evaluation IN THE CALLER'S TRANSACTION — the
// transactional outbox of ADR 0001.
func (s *Service) enqueueNotify(ctx context.Context, g domain.Group, reason string) error {
	if s.enqueuer == nil {
		return nil
	}
	_, err := s.enqueuer.Enqueue(ctx, jobs.NotifyEvaluateArgs{
		GroupID:      g.ID(),
		Reason:       reason,
		StateVersion: g.StateVersion(),
	})
	if err != nil {
		return errs.Wrap(err, errs.KindInternal, "enqueue_notify_failed",
			"could not queue notification evaluation")
	}
	return nil
}
