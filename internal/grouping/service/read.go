package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The read models `grouping/api` renders. They are grouping's own types, not row
// types and not DTOs, and no DTO may embed one (CONTEXT.md §5.5).

// ListResult is one page of group generations.
type ListResult struct {
	Groups []domain.Group
	// Snoozes is the §B.8.6 quiet roll-up of each generation on this page, keyed
	// by group id. An absent entry means nothing in that generation is muted.
	//
	// It rides beside the page rather than on domain.Group because it is NOT a
	// property of the group row: there is no group-level snooze (§B.8.3), only
	// the visible result of a fan-out over member alerts, and it is a function of
	// the clock rather than of the row.
	Snoozes map[uuid.UUID]domain.SnoozeRollup
	Cursor  db.Cursor
}

// ListQuery is the compiled form of `GET /api/v1/alert-groups`.
type ListQuery struct {
	Filter domain.GroupFilter
	// Sort is "" (meaning SortLastActivityDesc), SortLastActivityDesc or
	// SortFirstSeenDesc. An unrecognised value is REJECTED, never defaulted.
	Sort string
	Page db.Keyset
}

// List serves `GET /api/v1/alert-groups` — the default UI landing view.
//
// ⭐ EVERY FILTER AND THE SORT GO DOWN TO SQL. They used to stop here: this
// method accepted `states` alone, so the handler fetched a page and then removed
// rows from it in memory, and accepted a `sort` it never applied. Both are the
// same failure — a list that answers a question other than the one it was asked
// while looking exactly as if it had not.
//
// A closed generation is still listed when `state` is unset: it is the record of
// a conversation that happened, and hiding it would make the group list disagree
// with the chat channel it mirrors.
func (s *Service) List(ctx context.Context, scope db.TenantScope, q ListQuery) (ListResult, error) {
	for _, st := range q.Filter.States {
		if _, err := domain.NewState(st); err != nil {
			return ListResult{}, err
		}
	}
	switch q.Sort {
	case "", domain.SortLastActivityDesc, domain.SortFirstSeenDesc:
	default:
		return ListResult{}, errs.Validation("sort_invalid",
			"sort must be one of: -last_activity_at, -first_seen_at")
	}
	if !q.Page.Cursor.IsZero() && q.Page.Cursor.Hash != q.Filter.FilterHash {
		return ListResult{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was minted against a different set of filters")
	}

	groups, cur, err := s.groups.List(ctx, scope, q.Filter, q.Sort, q.Page)
	if err != nil {
		return ListResult{}, err
	}

	// ⭐ ONE extra query for the whole page, never one per group. The group
	// screen can offer the snooze fan-out; without this it could never show the
	// result, and a button whose effect is invisible is indistinguishable from a
	// button that does nothing.
	ids := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID())
	}
	snoozes, err := s.members.SnoozeRollup(ctx, scope, ids, s.Now())
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Groups: groups, Snoozes: snoozes, Cursor: cur}, nil
}

// Detail is `GET /api/v1/alert-groups/{id}` — the generation and its rollup.
type Detail struct {
	Group domain.Group
	// Members is the PREVIEW: at most domain.MemberPreviewLimit currently-joined
	// members, newest join first, as SQL returned them. It is not the membership
	// — a generation in a storm holds thousands — and nothing may count it. The
	// counts are in Group's rollup, the page is `/alert-groups/{id}/alerts`, and
	// past members are reachable through History, because membership is history
	// and not a boolean.
	Members []domain.Member
	// StormActive mirrors Group.StormMode and is repeated here because the UI
	// renders it as a badge next to the counts, and a damper the user cannot see
	// is the silent suppression §B.6 forbids.
	StormActive bool
	// Snooze is the §B.8.6 quiet roll-up: how many currently-joined members oto
	// is holding its tongue about, and when the last of them wakes. Snooze is the
	// MANUAL sibling of storm collapse and flap damping, and it is subject to the
	// same rule — a damper the user cannot see is not one oto ships.
	Snooze domain.SnoozeRollup
}

// Get serves `GET /api/v1/alert-groups/{id}`.
//
// ⭐ THE PREVIEW IS ONE PAGE, AND FOUR ENDPOINTS PAY FOR IT. Ack, snooze and
// unsnooze all render their reply through this method, so a group action during
// a storm used to re-read the whole membership — five thousand rows fetched,
// copied, sorted and 4 980 of them thrown away — every time a human pressed a
// button, which is exactly when they are pressing them. It now takes the first
// domain.MemberPreviewLimit rows of the same keyset read that
// `/alert-groups/{id}/alerts` pages through, already ordered newest-join-first
// by gm_current_idx.
//
// ⭐ THIS LIMIT IS THE ONLY PLACE THE PREVIEW IS BOUNDED. Nothing downstream cuts
// the slice again — `api` renders exactly what this returns — so the contract's
// `top_alerts: maxItems: 20` is satisfied here or nowhere.
//
// The cursor is discarded on purpose: a preview is not a page, and "there is
// more" is already on the card as the generation's member counts.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, groupID uuid.UUID) (Detail, error) {
	g, err := s.groups.GetByID(ctx, scope, groupID)
	if err != nil {
		return Detail{}, err
	}
	members, _, err := s.members.ListCurrentMembers(ctx, scope, groupID,
		db.Keyset{Limit: domain.MemberPreviewLimit})
	if err != nil {
		return Detail{}, err
	}
	snoozes, err := s.members.SnoozeRollup(ctx, scope, []uuid.UUID{groupID}, s.Now())
	if err != nil {
		return Detail{}, err
	}
	return Detail{
		Group:       g,
		Members:     members,
		StormActive: g.StormMode(),
		Snooze:      snoozes[groupID],
	}, nil
}

// MemberResult is one keyset page of a generation's current members.
type MemberResult struct {
	Members []domain.Member
	Cursor  db.Cursor
}

// Members serves `GET /api/v1/alert-groups/{id}/alerts` — the member alerts of a
// generation, newest join first, keyset-paginated.
//
// ⭐ IT RETURNS A DOMAIN TYPE AND A PAGE. It used to return
// `[]repository.MemberAlert`, a name `grouping/api` cannot even write down —
// depguard forbids `api` importing `repository` (CONTEXT.md §5.1) — so the
// handler could not call it at all and reached for `Get().Members` instead,
// materialising the whole membership and slicing it in Go. A service method the
// only caller that needs it is forbidden to name is not a service method.
//
// `Get` now reads through the same repository query for its preview, so the
// membership is bounded in SQL on BOTH routes and there is no longer a method on
// this service that returns every member row.
func (s *Service) Members(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, p db.Keyset,
) (MemberResult, error) {
	members, cur, err := s.members.ListCurrentMembers(ctx, scope, groupID, p)
	if err != nil {
		return MemberResult{}, err
	}
	return MemberResult{Members: members, Cursor: cur}, nil
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
//
// ⭐ THE INSTANT GOES DOWN TO SQL. This used to read `AllMembers` — every
// membership row the generation has ever had, joined and departed — and then run
// domain.Member.WasMemberAt over the lot in Go, which is a question the WHERE
// clause could answer while the rows were still in the database. The predicate
// in `membersAtSQL` is that method, clause for clause.
func (s *Service) MembersAt(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) ([]domain.Member, error) {
	return s.members.MembersAt(ctx, scope, groupID, at)
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
	// Members is how many currently-joined members the verb reached a CONCLUSION
	// on — it is exactly `Applied + Skipped()`. Since domain.FanOutLimit it is not
	// necessarily how many the generation has; Unreached carries the difference.
	Members int
	// Applied is how many accepted it. A member whose episode has already ended
	// cannot be acknowledged, and that is a normal outcome, not a failure of the
	// request.
	Applied int
	// SkippedCodes counts the members that REFUSED the verb, keyed by the stable
	// errs code of the refusal.
	//
	// ⭐ It exists because "nothing happened" has more than one honest
	// explanation, and a caller that has to tell a human which one it was cannot
	// get it from a count. `already_acked` means somebody got there first;
	// `no_open_occurrence` means every episode has already resolved or expired.
	// Those are different sentences, and a surface that cannot say which — the
	// Slack Acknowledge button, in particular — is back to being a button that
	// silently does nothing.
	SkippedCodes map[string]int
	// Unreached is how many currently-joined members the verb was NOT offered to,
	// because the call stopped at domain.FanOutLimit or because it failed partway.
	//
	// ⭐ IT IS WHAT MAKES A CEILING HONEST. A bounded fan-out that reported only
	// what it did would be a button that acked 500 of 5 000 alerts and looked
	// exactly like a button that acked all of them. Zero means the fan-out saw
	// every member of the generation — there is nothing outstanding — and that is
	// the ONLY reading of "this group has been acked" that is true.
	Unreached int
}

// Skipped is the total number of members that refused the verb.
func (r FanOutResult) Skipped() int {
	n := 0
	for _, c := range r.SkippedCodes {
		n += c
	}
	return n
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

// CommentResult is what one group comment wrote.
type CommentResult struct {
	FanOut FanOutResult
	// Event is the FIRST appended entry, which is the one the `201` body
	// carries. Members are fanned out in join order, so it is deterministic.
	Event kernel.Event
}

// Comment serves `POST /api/v1/alert-groups/{id}/comments`: one annotation on
// each member's timeline.
//
// ⭐ IT RETURNS THE APPENDED EVENT. The contract answers `201` with the event
// that was written, and the handler used to obtain it by re-reading the group
// timeline and picking the newest `comment.added` — a second query that can
// legitimately return somebody else's comment, appended a millisecond later, and
// hand it back as the caller's own. The write already knows what it wrote.
//
// A group with no currently-joined member is a `412`: there is no signal to
// annotate, and the timeline is a record of facts about signals.
//
// ⚠️ IT IS NOT RETRY-SAFE, AND IT IS THE ONE GROUP VERB THAT IS NOT. Ack, snooze
// and unsnooze are idempotent in the domain — a second pass meets `already_acked`
// and refuses — but a comment is an APPEND, and `alerts/service.Comment` mints
// its §C.8 dedupe key from the wall clock, so a second pass writes a second
// annotation on every member it already annotated. That matters most exactly
// where it is least visible: a fan-out that fails partway returns its partial
// account WITH the error, which is precisely what a caller retries on, and the
// members in front of the failure are annotated again.
//
// The mechanism that fixes this is the caller's `Idempotency-Key` on
// `commentOnAlertGroup` — open ticket a6cc834 — not a content-hashed dedupe key,
// which would collapse two people who typed the same sentence into one fact.
// `TestFanOutRetryOfACommentDuplicatesIt` pins the behaviour as it stands so the
// gap is a recorded one rather than a surprise.
func (s *Service) Comment(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	actorKind, actorID, actorLabel, body string,
) (CommentResult, error) {
	var (
		first kernel.Event
		got   bool
	)
	res, err := s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		ev, err := s.actions.CommentAs(ctx, scope, alertID, actorKind, actorID, actorLabel, body)
		if err != nil {
			return err
		}
		if !got {
			first, got = ev, true
		}
		return nil
	})
	if err != nil {
		// The audit rides out with the error: a comment that annotated 200
		// timelines before the database went away still annotated them.
		return CommentResult{FanOut: res}, err
	}
	if !got {
		return CommentResult{}, errs.Precondition("no_group_members",
			"this group has no member alert to annotate")
	}
	return CommentResult{FanOut: res, Event: first}, nil
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
	actorKind, actorID, actorLabel, note string,
) (FanOutResult, error) {
	return s.fanOut(ctx, scope, groupID, func(ctx context.Context, alertID uuid.UUID) error {
		return s.actions.UnsnoozeAs(ctx, scope, alertID, actorKind, actorID, actorLabel, note)
	})
}

// fanOut applies one member action across a generation's currently-joined
// members, AT MOST domain.FanOutLimit of them per call.
//
// A member that refuses the verb — an episode that has already ended cannot be
// acknowledged, an alert that is not snoozed cannot be unsnoozed — is SKIPPED and
// counted, never allowed to fail the whole request. Refusing the other 39 members
// because one had already resolved would make the group button unusable in exactly
// the situation it exists for.
//
// ⭐ IT IS ONE WRITE TRANSACTION PER MEMBER AND IT ALWAYS WILL BE. `apply` is a
// verb on ONE signal (§E.1.1) — there is no group-level ack row, no group-level
// snooze and no group-level comment to write instead — so the only question a
// fan-out gets to answer is HOW MANY of those transactions one press may open.
// The answer is domain.FanOutLimit, for the reasons argued there, and it is
// applied as the SQL LIMIT of the candidate read: a fan-out that read the whole
// membership and then used 500 of it has already paid the cost of the storm.
//
// ⛔ IT IS NOT ATOMIC, AND WRAPPING IT IN ONE TRANSACTION WOULD BE WORSE. Five
// hundred members' worth of rows locked for the length of the slowest one, to buy
// an all-or-nothing that nobody asked for: an ack that half-lands is 250 signals
// with a true receipt on them, and rolling that back would be discarding facts.
// What the caller is owed is not atomicity but an ACCOUNT, which is what
// FanOutResult is.
//
// ⚠️ THE ACCOUNT SURVIVES THE ERROR. A hard failure partway returns the partial
// result ALONGSIDE the error rather than a zero value: the members already
// applied are committed and are not coming back, and a result that forgot them
// would be the only record of them lost. Everything the call did not conclude on
// — the member that failed, the ones behind it, and anything past the ceiling —
// is counted in Unreached, so no member is silently dropped in either direction.
//
// ⛔ RE-RUNNING IT IS SAFE FOR THREE OF THE FOUR VERBS AND NOT FOR THE FOURTH.
// Ack, snooze and unsnooze are compare-and-set on the episode and refuse a second
// pass by code (`already_acked` and friends), so a retry finishes the job without
// disturbing what committed. A COMMENT IS AN APPEND and has no such refusal: see
// Comment, and ticket a6cc834 for the `Idempotency-Key` that would give it one.
// Since the account rides out with the error, a caller that retries on error is
// the normal way to reach that second annotation.
func (s *Service) fanOut(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	apply func(ctx context.Context, alertID uuid.UUID) error,
) (FanOutResult, error) {
	if s.actions == nil {
		return FanOutResult{}, errs.Internal("member_actions_missing", errMissingDep("MemberActions"))
	}
	members, err := s.members.CurrentMemberAlerts(ctx, scope, groupID, domain.FanOutLimit)
	if err != nil {
		return FanOutResult{}, err
	}

	// Only a FULL candidate read can have anything behind it, so the ordinary
	// group of forty never asks this question and never pays for the count.
	beyond := 0
	if len(members) >= domain.FanOutLimit {
		total, err := s.members.CountCurrentMembers(ctx, scope, groupID)
		if err != nil {
			return FanOutResult{}, err
		}
		if total > len(members) {
			beyond = total - len(members)
		}
	}

	var res FanOutResult
	seen := make(map[uuid.UUID]struct{}, len(members))
	for i, m := range members {
		// One alert can be in the generation through more than one episode; the
		// verb is about the SIGNAL, so it is applied once.
		if _, dup := seen[m.AlertID]; dup {
			continue
		}
		seen[m.AlertID] = struct{}{}

		if err := apply(ctx, m.AlertID); err != nil {
			if errs.IsKind(err, errs.KindPrecondition) || errs.IsKind(err, errs.KindNotFound) {
				// The refusal is recorded rather than merely tolerated: the CODE
				// is the only thing that can tell a human "somebody already acked
				// this" apart from "it resolved while you were reading it".
				code := errs.CodeOf(err)
				if code == "" {
					code = "refused"
				}
				if res.SkippedCodes == nil {
					res.SkippedCodes = map[string]int{}
				}
				res.SkippedCodes[code]++
				res.Members++
				continue
			}
			// This member and every member behind it, plus whatever the ceiling
			// already cut off. The ones in front are committed and are reported.
			res.Unreached = beyond + len(members) - i
			return res, err
		}
		res.Applied++
		res.Members++
	}

	res.Unreached = beyond
	if beyond > 0 {
		// A truncated fan-out is a normal, bounded outcome and never an error —
		// but it is one an operator must be able to find afterwards, because the
		// group is not finished and nothing else is going to say so.
		s.log.WarnContext(ctx, "grouping: group fan-out stopped at the member ceiling",
			"org_id", scope.OrgID(), "group_id", groupID,
			"limit", domain.FanOutLimit, "reached", res.Members, "unreached", res.Unreached)
	}
	return res, nil
}
