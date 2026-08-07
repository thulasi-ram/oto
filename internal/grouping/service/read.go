package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The read models `grouping/api` renders. They are grouping's own types, not row
// types and not DTOs, and no DTO may embed one (CONTEXT.md §5.5).

// ListResult is one page of group generations.
type ListResult struct {
	Groups []domain.Group
	Cursor db.Cursor
}

// List serves `GET /api/v1/alert-groups` — the default UI landing view, keyset
// paginated by (last_activity_at DESC, id DESC).
//
// `states` is a filter over open|closed; empty means both. A closed generation is
// still listed: it is the record of a conversation that happened, and hiding it
// would make the group list disagree with the Slack channel it mirrors.
func (s *Service) List(
	ctx context.Context, scope db.TenantScope, states []string, p db.Keyset,
) (ListResult, error) {
	for _, st := range states {
		if _, err := domain.NewState(st); err != nil {
			return ListResult{}, err
		}
	}
	groups, cur, err := s.groups.List(ctx, scope, states, p)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Groups: groups, Cursor: cur}, nil
}

// Detail is `GET /api/v1/alert-groups/{id}` — the generation and its rollup.
type Detail struct {
	Group domain.Group
	// Members are the CURRENTLY-JOINED members. Past members are reachable
	// through History, because membership is history and not a boolean.
	Members []domain.Member
	// StormActive mirrors Group.StormMode and is repeated here because the UI
	// renders it as a badge next to the counts, and a damper the user cannot see
	// is the silent suppression §B.6 forbids.
	StormActive bool
}

// Get serves `GET /api/v1/alert-groups/{id}`.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, groupID uuid.UUID) (Detail, error) {
	g, err := s.groups.GetByID(ctx, scope, groupID)
	if err != nil {
		return Detail{}, err
	}
	members, err := s.members.CurrentMembers(ctx, scope, groupID)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Group: g, Members: members, StormActive: g.StormMode()}, nil
}

// Members serves `GET /api/v1/alert-groups/{id}/alerts` — the member alerts of a
// generation, in join order.
func (s *Service) Members(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
) ([]repository.MemberAlert, error) {
	return s.members.CurrentMemberAlerts(ctx, scope, groupID)
}

// History returns every membership a generation has ever had, so the group card
// can be replayed at any past instant.
func (s *Service) History(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
) ([]domain.Member, error) {
	return s.members.AllMembers(ctx, scope, groupID)
}

// MembersAt replays which occurrences were in the generation at one instant.
//
// This is what makes "what was in this group when the thread was posted?"
// answerable, and it is why a member that leaves keeps its row.
func (s *Service) MembersAt(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) ([]domain.Member, error) {
	all, err := s.members.AllMembers(ctx, scope, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Member, 0, len(all))
	for _, m := range all {
		if m.WasMemberAt(at) {
			out = append(out, m)
		}
	}
	return out, nil
}

// GroupsForAlert answers "which groups has this alert been part of", newest
// first.
func (s *Service) GroupsForAlert(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, limit int,
) ([]domain.Member, error) {
	return s.members.GroupsForAlert(ctx, scope, alertID, limit)
}

// Timeline serves `GET /api/v1/alert-groups/{id}/timeline` — §D.12(b), the
// MERGED, ORDERED lifecycle timeline and the signature view of the product.
//
// The lower time bound is REQUIRED because `recorded_at` is the partition key of
// `alert_events`. When the caller supplies none it defaults to the generation's
// own `first_seen_at`, which is both correct and the tightest bound available:
// nothing about a generation happened before it opened.
func (s *Service) Timeline(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (alerts.TimelineResult, error) {
	if s.timeline == nil {
		return alerts.TimelineResult{}, errs.Internal("timeline_port_missing",
			errMissingDep("TimelineReader"))
	}
	if w.From.IsZero() {
		g, err := s.groups.GetByID(ctx, scope, groupID)
		if err != nil {
			return alerts.TimelineResult{}, err
		}
		w.From = g.FirstSeenAt()
	}
	if w.To.IsZero() {
		w.To = s.Now()
	}
	return s.timeline.GroupTimeline(ctx, scope, groupID, w, p)
}

// StateVersion is the §C.7 input a notify job pins its intent to. It is exposed
// so `alerts` can enqueue `notify.evaluate` against the version the group had
// when the transition happened.
func (s *Service) StateVersion(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
) (int, error) {
	return s.groups.StateVersion(ctx, scope, groupID)
}

// -------------------------------------------------------------- group actions

// FanOutResult is the audit of a group-level human verb.
type FanOutResult struct {
	// Members is how many currently-joined members the verb was applied to.
	Members int
	// Applied is how many accepted it. A member whose episode has already ended
	// cannot be acknowledged, and that is a normal outcome, not a failure of the
	// request.
	Applied int
}

// Acknowledge serves `POST /api/v1/alert-groups/{id}/ack`: ack every OPEN member
// episode.
//
// ⛔ It is a FAN-OUT OF THE SAME PRIMITIVE, not a new one. There is no
// group-level ack_state column and there will not be one: ack is a receipt
// written on a SIGNAL, and "I acked the group" means "I have seen each of these",
// never "this group is mine" and never "this group is over" (§E.1.1).
func (s *Service) Acknowledge(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel, note string,
) (FanOutResult, error) {
	return s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		return s.actions.AcknowledgeAs(ctx, scope, alertID, actorKind, actorID, actorLabel, note)
	})
}

// Comment serves `POST /api/v1/alert-groups/{id}/comments`: one annotation on
// each member's timeline.
func (s *Service) Comment(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel, body string,
) (FanOutResult, error) {
	return s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		return s.actions.CommentAs(ctx, scope, alertID, actorKind, actorID, actorLabel, body)
	})
}

// Snooze serves `POST /api/v1/alert-groups/{id}/snooze` (§B.8.3).
//
// ⭐ It creates ONE SNOOZE PER CURRENTLY-JOINED MEMBER ALERT and nothing more.
// Alerts that join the group LATER are NOT snoozed: a snooze is never predictive,
// and a group-level mute would silence alerts nobody has ever seen. That is the
// difference between a quiet button and a blindfold.
func (s *Service) Snooze(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel string, until time.Time, note string,
) (FanOutResult, error) {
	return s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		return s.actions.SnoozeAs(ctx, scope, alertID, actorKind, actorID, actorLabel, until, note)
	})
}

// Unsnooze serves `POST /api/v1/alert-groups/{id}/unsnooze`: end the snooze on
// each currently-joined member.
func (s *Service) Unsnooze(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel string,
) (FanOutResult, error) {
	return s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		return s.actions.UnsnoozeAs(ctx, scope, alertID, actorKind, actorID, actorLabel)
	})
}

// fanOut applies one member action across a generation's currently-joined
// members.
//
// A member that refuses the verb — an episode that has already ended cannot be
// acknowledged, an alert that is not snoozed cannot be unsnoozed — is SKIPPED and
// counted, never allowed to fail the whole request. Refusing the other 39 members
// because one had already resolved would make the group button unusable in exactly
// the situation it exists for.
func (s *Service) fanOut(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	apply func(ctx context.Context, alertID uuid.UUID) error,
) (FanOutResult, error) {
	if s.actions == nil {
		return FanOutResult{}, errs.Internal("member_actions_missing", errMissingDep("MemberActions"))
	}
	members, err := s.members.CurrentMemberAlerts(ctx, scope, groupID)
	if err != nil {
		return FanOutResult{}, err
	}

	res := FanOutResult{Members: len(members)}
	seen := make(map[uuid.UUID]struct{}, len(members))
	for _, m := range members {
		if _, dup := seen[m.AlertID]; dup {
			continue
		}
		seen[m.AlertID] = struct{}{}

		if err := apply(ctx, m.AlertID); err != nil {
			if errs.IsKind(err, errs.KindPrecondition) || errs.IsKind(err, errs.KindNotFound) {
				continue
			}
			return FanOutResult{}, err
		}
		res.Applied++
	}
	return res, nil
}
