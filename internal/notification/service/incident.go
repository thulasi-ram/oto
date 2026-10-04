package service

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0052 §5: DECLARING AN INCIDENT IS A NOTIFICATION (git-bug aa6d18b).
//
// ⭐ AN INCIDENT FACT IS AN ORDINARY NOTIFICATION, ON PURPOSE. It gets the §C.7
// idempotency key, a `notifications` row, `notification_deliveries` with their own
// retry budget, the dead-letter, the delivery audit and the transactional outbox —
// everything that makes a Notification trustworthy — because those are what "every
// Incident goes to incident.io" needs and none of them is Incident-shaped. What this
// file adds is only what IS: the subject (the Incident, not a Case), the routing
// input (labels the whole story shares), and the destinations an Incident can reach
// today.
//
// ⛔ NOTHING HERE SENDS A COMMAND. The six Reasons are facts — drawn, a Case added
// or removed, quiet, active again, a new Finding — and none of them means resolve or
// close. An org that wants its incident tool to resolve on `quiet` writes that in the
// tool.

// IncidentReader is the port this module declares to read the Incident a fact is
// about. `internal/app` satisfies it over `incidents/service`; `notification` never
// imports `incidents` (CONTEXT.md §4).
type IncidentReader interface {
	Incident(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.IncidentFacts, error)
}

// IncidentConversations is the port this module declares to ask which Incident
// conversation a Case's fact belongs in (ADR 0052 §6): the Incident the Case is a
// CURRENT member of, when the Correlator that drew it says its Incidents are
// conversations. `internal/app` satisfies it over `incidents/service`.
//
// ⭐ IT IS ASKED ONCE PER FACT, WHEN THE FACT IS EVALUATED, AND THE ANSWER IS FROZEN
// ON THE ROW. See `placeInIncident`.
type IncidentConversations interface {
	ConversationFor(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.IncidentRef, bool, error)
}

// IncidentIntent is one Incident fact, as `notify.incident` receives it.
type IncidentIntent struct {
	IncidentID uuid.UUID
	Reason     domain.Reason
	// OccasionID is WHICH TIME this fact happened, and for an Incident it is the
	// whole discriminator: an Incident has no `state_version`. Required.
	OccasionID uuid.UUID
}

// incidentSkipReason is the sentence a threaded destination's delivery is recorded
// as skipped with, for an Incident that is NOT a conversation (ADR 0052 §6).
//
// ⭐ IT NAMES THE SWITCH. Until 00087 this said an Incident could not be a
// conversation on any threaded channel "yet"; now it can, and the only thing between
// this delivery and a thread is a setting an operator owns — so the sentence says
// which one, and a human-drawn Incident, which has no Correlator to carry it, is
// covered by the same words.
const incidentSkipReason = "this channel threads its messages, and this Incident is not a " +
	"conversation: only a Correlator that says its Incidents are conversations makes one " +
	"(ADR 0052 §6); the generic webhook carries Incident facts"

// EvaluateIncident is `notify.incident`: decide, record and fan out ONE Incident
// fact, in one transaction, with the delivery jobs enqueued inside it (ADR 0001).
//
// Order of operations, the same as `Evaluate`'s and for the same reasons: read the
// world once, route, record the intent suppressed or not — "oto decided not to tell
// you, and here is why" is the feature (§B.6) — and only then fan out.
//
// ⭐ AN ORG WITH NO INCIDENT POLICY SENDS NOTHING, AND SAYS SO. The intent is still
// recorded, as `no_policy`, exactly like any other fact nobody routed: oto drawing
// Incidents for analysis and conversation is ordinary (ADR 0052, Consequences), and
// declaring them is the narrowing a policy states.
func (s *NotificationService) EvaluateIncident(
	ctx context.Context, scope db.TenantScope, in IncidentIntent,
) (Result, error) {
	switch {
	case !in.Reason.Valid() || in.Reason.Subject() != domain.SubjectIncident:
		return Result{}, errs.Validation("unknown_reason", "not an Incident notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: string(in.Reason)})
	case in.IncidentID == uuid.Nil:
		return Result{}, errs.Validation("incident_required", "an Incident fact names its Incident",
			errs.Violation{Field: "incident_id", Code: "required", Message: "an incident id is required"})
	case in.OccasionID == uuid.Nil:
		// ⛔ REFUSED, NOT WARNED, UNLIKE `Evaluate`'s occasion-less snooze. There a
		// missing occasion costs only a SECOND announcement in one episode; here the
		// occasion is the key's only discriminator, so without it every `case_added`
		// on one Incident would collapse into the first one forever.
		return Result{}, errs.Validation("occasion_required", "an Incident fact names its occasion",
			errs.Violation{Field: "occasion_id", Code: "required", Message: "an occasion id is required"})
	case s.incidents == nil:
		return Result{}, errs.New(errs.KindInternal, "incident_reader_unwired",
			"the notification service has no Incident reader, so it cannot evaluate an Incident fact")
	}

	var out Result
	err := s.txr.InTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.evaluateIncident(ctx, scope, in)
		return err
	})
	return out, err
}

func (s *NotificationService) evaluateIncident(
	ctx context.Context, scope db.TenantScope, in IncidentIntent,
) (Result, error) {
	now := s.clk.Now().UTC()

	facts, err := s.incidents.Incident(ctx, scope, in.IncidentID)
	if err != nil {
		return Result{}, err
	}

	// ⭐ THE POINTERS GO FIRST, AND WHETHER OR NOT ANY POLICY ROUTES THIS FACT. They
	// are not a declaration of the Incident — they tell the readers of each member
	// Case's own thread where its later facts now go, and those facts are routed by
	// the CASE's policy, not by an Incident policy an org may never write.
	if facts.Conversation {
		if err := s.pointMembers(ctx, scope, facts, now); err != nil {
			return Result{}, err
		}
	}

	n := domain.Notification{
		ID:          uuid.New(),
		OrgID:       scope.OrgID(),
		SubjectKind: domain.SubjectIncident,
		SubjectID:   facts.ID,
		// The Incident's own conversation (migrations 00084, 00087). On a destination
		// that keeps no thread that is simply the destination; on a threaded one it is
		// the Incident's thread when the Incident is a conversation, and a skipped
		// delivery when it is not (`incidentFanOut`).
		ConversationKind: domain.ConversationIncident,
		ConversationID:   facts.ID,
		Reason:           in.Reason,
		// An Incident has no compare-and-set to version. The occasion is the key's
		// discriminator, and the version is the constant `notifications_sver_ck`
		// admits.
		StateVersion: 1,
		Status:       domain.StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion, in.OccasionID)

	match, err := s.policies.Evaluate(ctx, scope, MatchRequest{
		Reason: in.Reason,
		Labels: facts.MatchLabels(),
	})
	if err != nil {
		return Result{}, err
	}
	if !match.Routed() {
		return s.record(ctx, scope, n, domain.SuppressedNoPolicy, nil, now)
	}
	id := match.Policy.ID
	n.PolicyID = &id
	if !match.Deliverable() {
		return s.record(ctx, scope, n, domain.SuppressedChannelDisabled, nil, now)
	}

	stored, created, err := s.notifications.Insert(ctx, scope, n)
	if err != nil {
		return Result{}, err
	}
	if !created && stored.Status == domain.StatusSuppressed {
		return Result{Notification: stored, Suppressed: stored.SuppressedReason}, nil
	}

	count, err := s.incidentFanOut(ctx, scope, stored, match.Live, facts.Conversation, now)
	if err != nil {
		return Result{}, err
	}
	if err := s.notifications.SetStatus(ctx, scope, stored.ID, domain.StatusDispatched, now); err != nil {
		return Result{}, err
	}
	stored.Status = domain.StatusDispatched
	// No `AppendNotificationCreated`: an Incident fact names no alert and no case, so
	// it belongs on no signal's timeline (`subjectOfEvent`), exactly as a digest does.
	// The member Cases already carry the `incident.case_*` facts themselves.
	return Result{Notification: stored, Created: created, Deliveries: count}, nil
}

// incidentFanOut splits the live destinations by what they can carry.
//
// A destination that keeps no thread — the generic webhook — gets one `post_root`
// through the ordinary `fanOut`, with its sequence-free delivery and its job
// enqueued in this transaction. A THREADED destination depends on the Incident:
//
//   - when its Correlator says its Incidents are conversations (ADR 0052 §6), the
//     destination goes through the same `fanOut`, which ensures the Incident's
//     thread and takes the modes `incidentModes` gives;
//   - otherwise the delivery is recorded as SKIPPED, with the sentence that says
//     why, and no job — the honest record of "oto decided not to post this here"
//     is a skipped delivery, not an absent one (§B.6).
func (s *NotificationService) incidentFanOut(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	live []domain.Channel, conversation bool, now time.Time,
) (int, error) {
	channels := append([]domain.Channel(nil), live...)
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID.String() < channels[j].ID.String() })

	var sendable []destination
	created := 0
	for _, c := range channels {
		if !needsThread(c) || conversation {
			sendable = append(sendable, destination{channel: c})
			continue
		}
		row, madeNew, err := s.deliveries.Create(ctx, scope, repository.NewDelivery{
			ID:             uuid.New(),
			NotificationID: n.ID,
			ChannelID:      c.ID,
			Mode:           domain.ModePostRoot,
			CreatedAt:      now,
		})
		if err != nil {
			return created, err
		}
		if !madeNew {
			continue
		}
		created++
		if err := s.deliveries.MarkSkipped(ctx, scope, row.ID, incidentSkipReason, now); err != nil {
			return created, err
		}
	}
	made, err := s.fanOut(ctx, scope, n, sendable, now)
	return created + made, err
}

// incidentModes is the whole of an Incident fact's §H.6, for the reason
// `digestModes` is a digest's: an Incident fact is not a signal transition, and the
// table's columns — verbosity per Reason, the state the card colours — say nothing
// about it.
//
// ⭐ THE ROOT IS THE INCIDENT'S CARD AND EVERY FACT KEEPS IT CURRENT. A fact that
// arrives before the card has landed on this channel posts it; one that arrives
// after amends it, so members and active/quiet are always what the Incident is now.
// `drawn` says nothing the card does not, so it never replies; every other fact —
// a Case joined or left, quiet, active again — is a line in the thread, unless the
// channel asked for update-in-place only.
//
// A destination that keeps no thread gets `post_root`, a standalone post, every
// time: the webhook's contract since 00084, unchanged.
func incidentModes(n domain.Notification, c domain.Channel, rootLanded bool) []domain.Mode {
	if !needsThread(c) {
		return []domain.Mode{domain.ModePostRoot}
	}
	if n.IncidentPointer() {
		// Minted by `pointMembers` with its own delivery; never fanned out. Stated
		// for completeness, so no path can turn a pointer into a root card.
		return []domain.Mode{domain.ModeThreadReply}
	}
	modes := make([]domain.Mode, 0, 2)
	switch {
	case !rootLanded:
		modes = append(modes, domain.ModePostRoot)
	case c.Capabilities.Has(domain.CapAmend):
		modes = append(modes, domain.ModeUpdateRoot)
	}
	if n.Reason != domain.ReasonDrawn && c.ThreadUpdates && c.Capabilities.Has(domain.CapThreading) {
		modes = append(modes, domain.ModeThreadReply)
	}
	if len(modes) == 0 {
		// A landed card this channel can neither amend nor reply under: a fresh card,
		// §H.10's answer for a state change on a non-amendable channel. Silence would
		// leave the channel's card describing an Incident that has moved on.
		modes = append(modes, domain.ModePostRoot)
	}
	return modes
}

// caseModesInIncident is §H.6 for a CASE fact placed in an Incident's conversation
// (ADR 0052 §6).
//
// ⭐ THE FACT IS NEVER THE THREAD'S ROOT. The root is the Incident's card, and a
// Case fact touches it only by amending it — the card lists the member Case with its
// state, so "this Case was acknowledged" or "it resolved" moves the card the way it
// would have moved the Case's own. So the table is asked as though a root exists
// and can be amended — which is the truth about the conversation, if not yet about
// this channel — and its answer is mapped:
//
//   - a root-touching fact amends the Incident's card, where the channel can amend;
//     where it cannot, the card cannot show it, so the fact is said in the thread;
//   - a reply stays a reply, gated by the channel's own switches as always;
//   - and if no card has landed on this channel yet, the fact posts the Incident's
//     card first and replies under it, rather than becoming a root of its own.
//
// A destination that keeps no thread is asked exactly what the Case path asks it.
func caseModesInIncident(in domain.PlanInput, c domain.Channel, rootLanded bool) []domain.Mode {
	if !needsThread(c) {
		return domain.PlanFor(in).Modes
	}
	canAmend, canThread := c.Capabilities.Has(domain.CapAmend), c.Capabilities.Has(domain.CapThreading)
	in.ThreadExists = true
	in.Capabilities |= domain.CapAmend

	var root, reply bool
	for _, m := range domain.PlanFor(in).Modes {
		if m.IsReply() {
			reply = true
		} else {
			root = true
		}
	}
	if root && !canAmend {
		root, reply = false, true
	}
	reply = reply && canThread
	if !root && !reply {
		return nil
	}

	modes := make([]domain.Mode, 0, 2)
	switch {
	case !rootLanded:
		modes = append(modes, domain.ModePostRoot)
	case root:
		modes = append(modes, domain.ModeUpdateRoot)
	}
	if reply {
		modes = append(modes, domain.ModeThreadReply)
	}
	return modes
}

// pointMembers posts the ONE "now part of Incident #N" reply into every thread a
// current member Case already has (ADR 0052 §6), so a reader following a Case's own
// thread learns where its later facts went instead of watching it fall silent.
//
// ⭐ ONE PER CASE PER INCIDENT, EVER, BY THE §C.7 KEY. Each pointer is an ordinary
// `notifications` row — an Incident fact, `case_added`, delivered into the CASE's
// conversation — keyed over the Incident with the Case as its occasion. Every
// Incident fact runs this sweep, so a Case that joined by any route (a Correlator, a
// human's add or move, a Correlator switched to conversations after the draw) is
// pointed at most once, and a redelivered or later fact finds the key taken.
//
// ⭐ WHERE IS READ FROM `channel_threads`, NOT FROM ROUTING. The threads are where
// the Case's messages actually landed; the policy that put them there may have
// changed since, and an Incident policy — which may not exist at all — has no say
// over the Case's own channel. A thread with nothing in it yet has nothing to point
// from, and a dead one has nobody reading it.
//
// ⚠️ NOTHING MOVES. The pointer is a reply appended to the Case's thread, in its
// order; the messages already there stay exactly where they are.
func (s *NotificationService) pointMembers(
	ctx context.Context, scope db.TenantScope, facts domain.IncidentFacts, now time.Time,
) error {
	current := make([]uuid.UUID, 0, len(facts.Members))
	for _, m := range facts.Members {
		if m.Current() {
			current = append(current, m.CaseID)
		}
	}
	threads, err := s.threads.ForSubjects(ctx, scope, domain.SubjectCase, current)
	if err != nil || len(threads) == 0 {
		return err
	}

	byCase := make(map[uuid.UUID][]domain.Thread, len(current))
	var channelIDs []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, th := range threads {
		if !th.Sendable() || (!th.RootLanded() && th.NextSeq <= 1) {
			continue
		}
		byCase[th.SubjectID] = append(byCase[th.SubjectID], th)
		if !seen[th.ChannelID] {
			seen[th.ChannelID] = true
			channelIDs = append(channelIDs, th.ChannelID)
		}
	}
	if len(channelIDs) == 0 {
		return nil
	}
	listed, err := s.channels.ListByIDs(ctx, scope, channelIDs)
	if err != nil {
		return err
	}
	live := make(map[uuid.UUID]domain.Channel, len(listed))
	for _, c := range listed {
		if c.Live() && c.Capabilities.Has(domain.CapThreading) {
			live[c.ID] = c
		}
	}

	for _, caseID := range current {
		var targets []domain.Thread
		for _, th := range byCase[caseID] {
			if _, ok := live[th.ChannelID]; ok {
				targets = append(targets, th)
			}
		}
		if len(targets) == 0 {
			continue
		}

		n := domain.Notification{
			ID:               uuid.New(),
			OrgID:            scope.OrgID(),
			SubjectKind:      domain.SubjectIncident,
			SubjectID:        facts.ID,
			ConversationKind: domain.ConversationCase,
			ConversationID:   caseID,
			Reason:           domain.ReasonCaseAdded,
			StateVersion:     1,
			Status:           domain.StatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		n.IdempotencyKey = domain.IdempotencyKey(
			scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion, caseID)

		stored, created, err := s.notifications.Insert(ctx, scope, n)
		if err != nil {
			return err
		}
		if !created {
			// Pointed already — by an earlier fact, or by this one's redelivery. The
			// pointer and its deliveries committed together, so there is nothing to
			// finish.
			continue
		}
		for _, th := range targets {
			id := th.ID
			if _, err := s.createDelivery(ctx, scope, stored, destination{channel: live[th.ChannelID]},
				&id, domain.ModeThreadReply, now); err != nil {
				return err
			}
		}
		if err := s.notifications.SetStatus(ctx, scope, stored.ID, domain.StatusDispatched, now); err != nil {
			return err
		}
	}
	return nil
}

// incidentCard projects an Incident's card from the Incident as it is NOW (C11).
//
// ⭐ IT NAMES NO SIGNAL AND OFFERS NO ACTION. Every action on a card acts on a
// signal — acknowledge this case, snooze this alert — and an Incident fact is about
// the story; a button would have to pick one of its Cases. What it carries instead
// is the membership, each Case with its labels and its own link, so an incident
// tool receiving this can show what the story is made of.
//
// It serves three deliveries, which differ only in what they are asked to say: an
// Incident fact (the webhook's post, a reply in the Incident's thread), the ROOT of
// an Incident's conversation whichever fact moved it (`ViewRequest.IncidentRoot`),
// and the pointer posted into a member Case's own thread, which carries that Case's
// id in `PointsFrom` (ADR 0052 §6).
func (v *ViewService) incidentCard(
	ctx context.Context, scope db.TenantScope, incidentID uuid.UUID, n domain.Notification, pointsFrom string,
) (*NotificationView, error) {
	if v.incidents == nil {
		return nil, errs.New(errs.KindInternal, "incident_reader_unwired",
			"the view service has no Incident reader, so it cannot build an Incident card")
	}
	f, err := v.incidents.Incident(ctx, scope, incidentID)
	if err != nil {
		return nil, err
	}

	state := "quiet"
	if f.Active {
		state = "active"
	}
	iv := &IncidentView{
		ID:      f.ID.String(),
		Number:  f.Number,
		State:   state,
		DrawnAt: f.DrawnAt.UTC(),
		DrawnBy: incidentAuthor(f.DrawnByLabel, f.DrawnByCorrelator),
		Members: make([]IncidentMemberView, 0, len(f.Members)),
		// "" except on the pointer posted into a member Case's own thread.
		PointsFrom: pointsFrom,
	}
	if v.baseURL != "" {
		iv.Link = v.baseURL + "/incidents/" + strconv.FormatInt(f.Number, 10)
	}
	for _, m := range f.Members {
		caseState := "closed"
		if m.CaseOpen {
			caseState = "open"
		}
		mv := IncidentMemberView{
			CaseID:         m.CaseID.String(),
			CaseNumber:     m.CaseNumber,
			CaseState:      caseState,
			AlertID:        m.AlertID.String(),
			AlertName:      m.Alertname,
			Labels:         m.Labels,
			AddedAt:        m.AddedAt.UTC(),
			AddedBy:        incidentAuthor(m.AddedByLabel, m.AddedByCorrelator),
			RemovedByLabel: m.RemovedByLabel,
			MovedToNumber:  m.MovedToNumber,
		}
		if !m.Current() {
			mv.RemovedAt = m.RemovedAt.UTC()
		}
		if v.baseURL != "" {
			mv.Link = v.baseURL + "/cases/" + m.CaseID.String()
		}
		iv.Members = append(iv.Members, mv)
	}
	for _, o := range f.Outbound {
		iv.External = append(iv.External, IncidentExternalView{
			Destination: o.ChannelName, URL: o.ExternalURL, ID: o.ExternalID,
		})
	}
	if fd := f.Finding; fd != nil {
		iv.Finding = &IncidentFindingView{
			InvestigationID: fd.InvestigationID.String(),
			Investigator:    fd.Investigator,
			Version:         fd.Version,
			Summary:         fd.Summary,
			Classification:  fd.Classification,
			Partial:         fd.Partial,
			ConcludedAt:     fd.ConcludedAt.UTC(),
		}
	}
	return &NotificationView{
		Reason:     string(n.Reason),
		Incident:   iv,
		RenderedAt: v.clk.Now().UTC(),
	}, nil
}

func incidentAuthor(label string, correlator uuid.UUID) IncidentAuthorView {
	if correlator != uuid.Nil {
		return IncidentAuthorView{CorrelatorID: correlator.String()}
	}
	return IncidentAuthorView{Label: label}
}
