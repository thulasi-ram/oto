package service

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultMaxInstances is how many member instances a card lists inline before it
// says "and N more" (§H.3).
const DefaultMaxInstances = 10

// ViewService builds the NotificationView — THE SEAM where alert state becomes
// presentation.
//
// Everything above this line is signals and policy; everything below it is
// blocks and colours. The view is the whole of the contract between them, and it
// is deliberately channel-agnostic: it contains no block, no colour, no emoji
// and no provider name. A renderer turns it into bytes; two renderers turn it
// into two completely different things from the same facts, which is the only
// honest proof that the abstraction holds.
//
// It is built ONCE PER DELIVERY, AT CLAIM TIME (C11). Not at enqueue time: a
// card describing a state the alert left twenty minutes ago is worse than no
// card, because a reader has no way to tell which one they are looking at.
type ViewService struct {
	snapshots SnapshotSource
	baseURL   string
	clk       clock.Clock
}

// ViewConfig configures the view builder.
type ViewConfig struct {
	Snapshots SnapshotSource
	// BaseURL is oto's public URL, used for every deep link on the card.
	BaseURL string
	// ⛔ `MaxInstances int` WAS HERE AND IS DELETED (git-bug `7570090`). It capped
	// the inline member list — the number of instances a card shows before it says
	// "and N more" — by capping the SNAPSHOT the card is built from, and it is the
	// dead half of a value that has a live half elsewhere.
	//
	// ⭐ THE CAP ITSELF IS NOT GONE AND IS NOT THIS FIELD. `RenderOptions.MaxInstances`
	// still bounds what `channels/render/slack`'s member block and
	// `webhookjson`'s alert list actually print, fed from `dispatch`'s own copy —
	// that is where a card's "and N more" comes from and always was. What went is the
	// SECOND cap, the one that trimmed the read model before the renderer ever saw
	// it: a Case has exactly one Alert, so the list this bounded is at most one row
	// and no reader passes it to a `LIMIT` any more.
	//
	// ⛔ IT WAS NEVER WIRED EITHER, which is why deleting it changes nothing at
	// runtime: `container.go` has never set it, so this view builder has been using
	// `DefaultMaxInstances` on every deployment oto has ever had.
	Clock clock.Clock
}

// NewViewService builds the view service.
func NewViewService(cfg ViewConfig) (*ViewService, error) {
	if cfg.Snapshots == nil {
		return nil, errs.New(errs.KindInternal, "view_service_deps", "a snapshot source is required")
	}
	v := &ViewService{
		snapshots: cfg.Snapshots,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		clk:       cfg.Clock,
	}
	if v.clk == nil {
		v.clk = clock.New()
	}
	return v, nil
}

// ViewRequest names the fact to be rendered.
//
// ⛔ IT CARRIES NO ACTOR AND NO COMMENT TEXT, and it used to carry both. Nothing
// ever set them: a caller at claim time has a notification row and no memory of
// the click that produced it, so the two fields were a channel with no source
// and every card rendered `Acknowledged` with nobody's name on it. Who acted and
// what they typed are read from the timeline, where they were written once and
// frozen — see `readCause` in the snapshot repository. Do not add them back: a
// second way to name a human is a second name to disagree with.
type ViewRequest struct {
	Notification domain.Notification
}

// Build reads the world and projects it into the renderer's read model.
func (v *ViewService) Build(
	ctx context.Context, scope db.TenantScope, req ViewRequest,
) (*NotificationView, error) {
	n := req.Notification

	// ⛔ A DIGEST READS NOTHING, AND IT CANNOT. The snapshot is keyed by a group
	// generation and a digest has none (migration 00058) — `Snapshot` with the nil
	// UUID would come back `case_not_found`, the delivery would retry twelve times
	// and dead-letter, and the operator would see a policy that silently never
	// digested. There is also nothing for it to read: the window is CLOSED, so the
	// count the digest asserts is frozen on the row (`DigestCount`), which is exactly
	// why 00058 stores it instead of recomputing at claim time.
	if n.Digest() {
		return v.digest(n), nil
	}

	// ⛔ IT PASSED `GroupID: n.GroupID` PLUS AN OPTIONAL `CaseID` (git-bug
	// `7570090`). The Case is the subject now, and `ConversationID` is where a
	// non-digest notification's Case id lives — `mint` writes the pair, so reading it
	// back here is reading what was stored rather than re-deriving it.
	snap, err := v.snapshots.Snapshot(ctx, scope, domain.SnapshotQuery{
		CaseID:  n.ConversationID,
		AlertID: n.AlertID,
		// The Reason travels because it is what names the timeline entry that
		// caused this card: without it the read model can say what the world looks
		// like but not who moved it.
		Reason: n.Reason,
	})
	if err != nil {
		return nil, err
	}
	return v.project(snap, req), nil
}

// digest projects a digest Notification, which needs no snapshot: everything it
// asserts is on the row.
//
// ⭐⭐ THE HEADLINE NO LONGER RIDES `Group.Title`, AND THE VIEW CARRIES NO SENTENCE AT
// ALL (git-bug `78388fb`). This function used to return
// `Group: GroupView{Title: DigestHeadline(n, "", 0), State: "open"}` and its own
// comment called that "a floor, not a design". The floor worked — a renderer that had
// never heard of a digest drew `*<sentence>* — <status>` and every word of it was true
// — and it worked for a reason that could not last: `Group.Title` was the one field
// that could not be left empty, because an empty title produced an empty
// `rendered_fallback` and failed `deliveries_fb_ck` with a 23514 AFTER the message had
// gone to Slack. Being forced is not the same as being right, and the shape carried a
// future defect: a group title holding something that is not a title, and
// `State: "open"` on a view with no group, are both read correctly by accident until
// some generic reader stops guessing.
//
// The facts go into `DigestView` and the renderer lays them out. `Digest != nil` is
// now the discriminator — a renderer branches on it before anything else — and the
// non-empty fallback is the digest card's own sentence, written where sentences
// belong.
//
// ⭐ WHAT IS DELIBERATELY ABSENT IS AS IMPORTANT AS WHAT IS PRESENT, and two of the
// three absences are now DECIDED rather than deferred.
//
//   - NO Actions. Every action on a card acts on a signal — acknowledge this case,
//     snooze this alert, silence in Alertmanager. A digest is about a window; a button
//     on it would have to pick one of the things it counted, which is a decision the
//     digest deliberately did not make.
//   - NO Links, AND THAT IS NOW A RULING AND NOT AN OMISSION. Two independent reasons,
//     either of which alone is enough. (1) `GET /cases`'s `since` is a LOWER BOUND
//     ONLY — the OpenAPI parameter says so in as many words, "combined with `sort`,
//     this is the time-range control: page backwards through the sorted list to reach
//     an upper bound" — so `/cases?since=<covered_from>` shows everything since the
//     span's start, which for any digest but the newest is unboundedly more than the
//     digest counted. (2) Membership is decided by the policy's compiled matcher in Go
//     (`Policy.Matches`, folded in `service/digest.go`), and a regular expression over
//     a label map has no URL form at all — so even with an upper bound the link would
//     name a different set. A link that looks like the digest's contents and is not is
//     worse than none, so the card states the span and says where to look instead.
//     What would unlock a real link is an upper time bound on the list plus a
//     policy-scoped filter; both are API work, not renderer work.
//   - NO Org. `snap.Org` comes from the snapshot, and the digest does not take one.
//     It costs the org name in the footer of a card whose destination already belongs
//     to exactly one org.
func (v *ViewService) digest(n domain.Notification) *NotificationView {
	d := &DigestView{}
	// ⛔ THE NIL READS ARE UNREACHABLE AND ARE STILL WRITTEN AS READS. A digest row
	// satisfies `notifications_digest_ck`, so `DigestCount` is present and at least 1
	// on every row that can get here; a nil would be a repository bug, and a card
	// saying "0" is a smaller wrong answer than a panic inside a claimed delivery.
	if n.DigestCount != nil {
		d.Count = *n.DigestCount
	}
	// ⭐ THE SPAN IS READ AND NEVER DERIVED. It is nil only on a digest written before
	// migration 00070, and the pair stays nil there for the reason 00070 exists: the
	// only way to invent it is the window start times the policy's CURRENT
	// `digest_window_s`, which is the inference `342e071` is about. The renderer draws
	// the absence.
	if n.DigestCoveredFrom != nil && n.DigestCoveredTo != nil {
		d.CoveredFrom, d.CoveredTo = n.DigestCoveredFrom.UTC(), n.DigestCoveredTo.UTC()
	}
	return &NotificationView{
		Reason:     string(n.Reason),
		Digest:     d,
		RenderedAt: v.clk.Now().UTC(),
	}
}

// project is the pure mapping, exported through Build. It performs no I/O, so
// the seam is exercisable from a snapshot literal.
func (v *ViewService) project(snap domain.Snapshot, req ViewRequest) *NotificationView {
	n := req.Notification

	view := &NotificationView{
		Org: OrgRef{
			ID: snap.Org.ID.String(), Slug: snap.Org.Slug, Name: snap.Org.Name,
		},
		Reason: string(n.Reason),
		Group:  v.group(snap),
		Alerts: v.alerts(snap),
		// The words a human typed, from the event they typed them onto. A comment
		// lives nowhere else (CONTEXT.md §6), so an empty one here is a person's
		// message replaced by an emoji in the channel they wrote it in.
		Comment:    snap.Comment,
		RenderedAt: snap.TakenAt,
	}

	if snap.Focus != nil {
		focus := alertView(*snap.Focus)
		view.Focus = &focus
	}
	if snap.Case != nil {
		view.Case = v.caseView(snap)
	}
	if snap.Rule != nil {
		view.Rule = ruleView(*snap.Rule)
	}
	if snap.RuleChange != nil {
		view.RuleChange = ruleChangeView(*snap.RuleChange)
	}
	if len(snap.Enrichments) > 0 {
		view.Enrichments = make(map[string]EnrichmentView, len(snap.Enrichments))
		for name, e := range snap.Enrichments {
			view.Enrichments[name] = EnrichmentView{
				Enricher: e.Enricher, Status: e.Status, Payload: e.Payload,
				Warnings: e.Warnings, Error: e.Error, ComputedAt: e.ComputedAt,
			}
		}
	}
	if snap.Actor != nil {
		view.Actor = &ActorView{
			Kind: actorViewKind(snap.Actor.Kind), ID: snap.Actor.ID, Label: snap.Actor.Label,
		}
	}

	// ⛔ `view.StormCount` WAS SET HERE FROM `snap.Group.StormMode`, and neither end of
	// that assignment exists now. Migration 00059 drops `alert_groups.storm_mode` and
	// `storm_since`, so the snapshot carries no storm state, and the field is gone from
	// `NotificationView` as well: migration 00060 deletes the `storm` Reason, so there
	// is no stored notification left for the renderer to draw a count on.

	// §H.4's strikethrough trick needs the state the card showed BEFORE this
	// delivery, and the Reason is where that fact lives: a Reason is the name of a
	// transition, so it names both ends of it. Deriving it here rather than reading
	// the previously rendered payload keeps the renderer pure and keeps the answer
	// stable when Slack loses a message and oto posts a fresh root.
	view.Previous = previousState(n.Reason, snap)
	view.Trail = trail(snap)
	view.Notifications = snap.NotificationCount
	view.SnoozedUntil = snoozedUntil(snap)

	view.Links = v.links(snap)
	view.Actions = v.actions(snap)
	return view
}

// snoozedUntil carries oto's own quiet across the seam, and it is the only fact
// on the snooze axis a renderer cannot already reach: `Actor` is who asked and
// `Comment` is what they wrote, and both are projected above.
//
// ⛔ THE FOCUS DECIDES WHENEVER THERE IS ONE, exactly as `Snapshot.FocusSnoozed`
// decides. A snooze is scoped to an `alert_key` (§B.8.1) and `snoozed` /
// `unsnoozed` are raised per alert (§B.8.3) — a group snooze is a FAN-OUT of the
// same primitive, one row per currently-joined member, not a group-level fact —
// so the card's own alert is the one whose clock the card must print. Reading the
// group's latest here would put another member's wake-up time on a card about
// this one.
//
// The group-wide fallback is for the card that has no focus, and it takes the
// LATEST: a reader of a partly-snoozed group asks when oto starts talking again,
// and that is when the last of them wakes. `grouping/api/dto.go` states the same
// reading of the same fact, which is why this one is phrased to agree with it.
//
// ⚠️ IT DOES NOT RE-DECIDE WHETHER OTO IS QUIET, so it does not take a clock and
// does not filter on one. Whether to speak was settled before this projection ran;
// asking again here against `TakenAt` would let a row that expired between the
// suppression check and the render silently erase the until-when from a card that
// is being sent precisely because of it. An expired-but-unswept row still
// truthfully names the moment the quiet ends (`snapshot.go`, `SnoozedUntil`).
func snoozedUntil(snap domain.Snapshot) *time.Time {
	if snap.Focus != nil {
		if until, ok := snap.SnoozedAlerts[snap.Focus.ID]; ok {
			return &until
		}
		return snap.Focus.SnoozedUntil
	}

	var latest time.Time
	for _, until := range snap.SnoozedAlerts {
		if until.After(latest) {
			latest = until
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

// actorViewKind translates the timeline's actor vocabulary into the view's.
//
// `alert_events.actor_kind` says `slack` for a human acting through a Slack
// interaction; the view says `slack_user`, which is the word every renderer
// tests for before it turns the id into a real `<@U…>` mention instead of
// printing it as text. They are the same fact in two vocabularies, and THIS IS
// THE SEAM, so this is where they meet: a renderer that learned the database's
// word would be a renderer that knows the database.
//
// Every other kind travels UNCHANGED, including oto's own machines — `system`,
// `reconciler`, `ingest`. Carrying a machine actor is not noise: an actor with
// no label is how a card knows that a transition was automatic, which is a
// different answer from not knowing who did it, and §B.6 makes the difference
// something oto owes the reader.
func actorViewKind(kind string) string {
	if kind == "slack" {
		return "slack_user"
	}
	return kind
}

// trailKinds maps `alert_events.type` onto the renderer's small closed
// vocabulary. A type with no entry is DROPPED, never printed raw: the trail is
// read by a human at 03:00 and `case.unsuppressed` is not a sentence.
//
// ⭐ IT IS KEYED BY THE KERNEL'S `EventType`, NOT BY TEN STRING LITERALS. Keyed by
// string, this map was the quietest of the re-declaration sites and the one with
// the worst failure mode: a mistyped key does not fail, it simply never matches,
// and the entry it was meant to render vanishes from every card with nothing
// anywhere reporting a problem. `EventType` is comparable — one unexported string
// field — so it is a map key like any other, and now a key that is not one of the
// closed 36 does not compile.
var trailKinds = map[kernel.EventType]string{
	kernel.EventCaseOpened:         "fired",
	kernel.EventCaseReopened:       "refired",
	kernel.EventCaseAcknowledged:   "acked",
	kernel.EventCaseUnacknowledged: "unacked",
	kernel.EventCaseSuppressed:     "suppressed",
	kernel.EventCaseUnsuppressed:   "unsuppressed",
	kernel.EventCaseResolved:       "resolved",
	kernel.EventCaseExpired:        "expired",
	// ⛔ `group.storm_started` -> "storm" AND `group.storm_ended` -> "storm_ended"
	// WERE HERE. Both event types are DELETED from `alerts/domain` (migration 00060
	// narrows `ev_type_ck` to refuse the spellings), and `groupTrailSQL` no longer
	// selects them — so a mapping for them would name a value that cannot exist.
	// `group.opened` and `group.closed` are oto's OWN bookkeeping about a
	// generation, not things that happened to the signal. They are deliberately
	// absent: a trail that says "oto opened a group" answers a question nobody
	// asked and pushes out a line that answers one somebody did.
}

// trail projects the group's state history onto the card's receipt.
//
// ⛔ CONSECUTIVE DUPLICATES COLLAPSE. A group of twelve instances that all fire
// in one batch produces twelve `case.opened` events, and a trail reading
// "fired → fired → fired …" is noise wearing a history's clothes. The FIRST of
// each run is kept, because the first is when the state actually changed.
func trail(snap domain.Snapshot) []TrailEntry {
	out := make([]TrailEntry, 0, len(snap.Trail))
	for _, f := range snap.Trail {
		// `TransitionFact.Type` is still a `string` because it is read straight off
		// `alert_events.type` by the repository, and a row written by a future
		// version of oto must not be able to make a card fail to render. Parsing
		// here keeps the DROP behaviour identical for an unknown value — the
		// renderer's whole rule — while the map above stays closed over the enum.
		typ, err := kernel.NewEventType(f.Type)
		if err != nil {
			continue
		}
		kind, ok := trailKinds[typ]
		if !ok {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Kind == kind {
			continue
		}
		out = append(out, TrailEntry{Kind: kind, At: f.At, Actor: f.ActorLabel})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// previousState names the state the card was in before this Reason moved it
// (§H.4). Nil means "nothing changed", and nil is the common case: a repeat, an
// enrichment or a comment leaves the card where it was, and rendering
// `~Firing~ → Firing` for one would be noise dressed as a change.
//
// ⛔ IT IS DERIVED FROM THE REASON, NOT GUESSED FROM THE COUNTS. The counts are
// the state the card is in NOW; they cannot say what it was. The Reason can,
// because oto only ever mints one for a transition it observed.
func previousState(reason domain.Reason, snap domain.Snapshot) *PreviousState {
	firing := &PreviousState{State: "firing", AckState: "unacked"}

	if snap.Group.AckedCount > 0 {
		// A card that was acked and is now resolved should say so: "~Acked~ →
		// Resolved" is the honest history, and "~Firing~ → Resolved" would erase
		// the fact that somebody had already picked it up.
		firing = &PreviousState{State: "firing", AckState: "acked"}
	}

	// ⛔ `some_resolved` WAS IN THE FIRST ARM AND `new_alerts` IN THE LAST, AND BOTH
	// REASONS ARE DELETED (git-bug `7570090`). They asserted a plurality and a
	// conversation holds one Case. Neither removal changes a rendered card: a resolve
	// arrives as `all_resolved`, which is still in the first arm and still draws
	// `~Firing~ → Resolved`, and `new_alerts` sat with `fired` in the nil arm because a
	// member joining changed no STATE — the card was firing before and after.
	switch reason {
	case domain.ReasonAcked, domain.ReasonSuppressed, domain.ReasonExpired,
		domain.ReasonAllResolved:
		if reason == domain.ReasonAcked {
			// The transition being announced IS the ack, so the state before it
			// cannot have been acked whatever the current count says.
			return &PreviousState{State: "firing", AckState: "unacked"}
		}
		return firing
	case domain.ReasonUnacked:
		// An ack was withdrawn, or a new episode invalidated it (T10).
		return &PreviousState{State: "firing", AckState: "acked"}
	case domain.ReasonUnsuppressed:
		return &PreviousState{State: "suppressed"}
	case domain.ReasonRefired:
		// The thread said resolved, and the strikethrough is what says so.
		//
		// ⛔ THE ARGUMENT USED TO END "which is exactly what makes a re-fire worth
		// BROADCASTING (ADR 0020)", and thread-broadcast is removed from oto entirely
		// (git-bug `7570090`, and see the ⛔⭐ block in `domain.PlanFor`). ⭐ THE
		// OBSERVATION SURVIVES ITS CONSEQUENCE AND MOVES HERE: a thread whose last word
		// was "resolved" is a thread people stopped following, so a re-fire arriving into
		// it is easy to miss. Rendering the previous state is now the ONLY thing that
		// marks it — which raises the stakes on this line rather than lowering them.
		return &PreviousState{State: "resolved"}
	case domain.ReasonFired, domain.ReasonRepeat,
		domain.ReasonSnoozed, domain.ReasonUnsnoozed, domain.ReasonEnriched,
		domain.ReasonRuleChanged, domain.ReasonComment:
		return nil
	default:
		return nil
	}
}

func (v *ViewService) group(snap domain.Snapshot) GroupView {
	g := snap.Group
	state := "open"
	if !g.Open() {
		state = "closed"
	}
	return GroupView{
		ID:              g.ID.String(),
		GroupKey:        g.GroupKey,
		Generation:      g.Generation,
		Title:           g.Title,
		Receiver:        g.Receiver,
		GroupLabels:     g.GroupLabels,
		State:           state,
		Severity:        g.Severity,
		FiringCount:     g.FiringCount,
		SuppressedCount: g.SuppressedCount,
		ResolvedCount:   g.ResolvedCount,
		ExpiredCount:    g.ExpiredCount,
		TotalCount:      g.TotalCount,
		AckedCount:      g.AckedCount,
		// StartedAt is upstream's `startsAt`; FirstSeenAt is oto's own sighting.
		// Both travel, because "when did this begin" and "when did oto learn about
		// it" are two questions and only one of them is the outage.
		StartedAt:      g.StartedAt(),
		FirstSeenAt:    g.FirstSeenAt,
		LastActivityAt: g.LastActivityAt,
		// DISPLAY ONLY, NEVER PARSED: Alertmanager's own groupKey is unescaped,
		// unbounded, and changes on every alertmanager.yml reload.
		SourceGroupKey: g.SourceGroupKey,
		ClusterKey:     g.ClusterKey,
	}
}

func (v *ViewService) alerts(snap domain.Snapshot) []AlertView {
	out := make([]AlertView, 0, len(snap.Alerts))
	for _, a := range snap.Alerts {
		out = append(out, alertView(a))
	}
	return out
}

func alertView(a domain.AlertFacts) AlertView {
	return AlertView{
		ID:                a.ID.String(),
		AlertKey:          a.AlertKey,
		SourceFingerprint: a.SourceFingerprint,
		AlertName:         a.AlertName,
		// The RAW upstream severity, never normalised: users filter on their own
		// vocabulary and normalising here would destroy it.
		Severity:     a.Severity,
		Namespace:    a.Namespace,
		Service:      a.Service,
		ClusterKey:   a.ClusterKey,
		Labels:       a.Labels,
		Annotations:  a.Annotations,
		GeneratorURL: a.GeneratorURL,
		State:        a.State,
		AckState:     a.AckState,
		FirstSeenAt:  a.FirstSeenAt,
		LastSeenAt:   a.LastSeenAt,
		TotalCases:   a.TotalCases,
		IsFlapping:   a.IsFlapping,
		Value:        a.Value,
	}
}

func (v *ViewService) caseView(snap domain.Snapshot) *CaseView {
	o := snap.Case
	return &CaseView{
		ID:                o.ID.String(),
		Seq:               o.Seq,
		State:             o.State,
		AckState:          o.AckState,
		SuppressionReason: o.SuppressionReason,
		ResolveReason:     o.ResolveReason,
		StartedAt:         o.StartedAt,
		EndedAt:           o.EndedAt,
		// Computed server-side and re-computed on every update, because a duration
		// baked into a message at post time is wrong one second later.
		Duration:     o.Duration(snap.TakenAt),
		AckedByLabel: o.AckedByLabel,
		AckedAt:      o.AckedAt,
		AckNote:      o.AckNote,
	}
}

func ruleView(r domain.RuleFacts) *RuleView {
	return &RuleView{
		SnapshotID:      r.SnapshotID.String(),
		Fingerprint:     r.Fingerprint,
		File:            r.File,
		Group:           r.Group,
		Name:            r.Name,
		Expr:            r.Expr,
		For:             r.For,
		KeepFiringFor:   r.KeepFiringFor,
		Labels:          r.Labels,
		Annotations:     r.Annotations,
		Origin:          r.Origin,
		MatchConfidence: r.MatchConfidence,
		CapturedAt:      r.CapturedAt,
	}
}

func ruleChangeView(c domain.RuleChangeFacts) *RuleChangeView {
	return &RuleChangeView{
		PreviousSnapshotID:  c.PreviousSnapshotID.String(),
		PreviousFingerprint: c.PreviousFingerprint,
		PreviousCapturedAt:  c.PreviousCapturedAt,
		ExprChanged:         c.ExprChanged,
		PreviousExpr:        c.PreviousExpr,
		NewExpr:             c.NewExpr,
		ForChanged:          c.ForChanged,
		PreviousFor:         c.PreviousFor,
		NewFor:              c.NewFor,
		LabelDiff:           c.LabelDiff,
		AnnotationDiff:      c.AnnotationDiff,
	}
}

// links builds the deep links a card offers.
//
// Note what is NOT here: any link that WRITES. The Alertmanager silence link is
// a deep link into somebody else's UI, prefilled with a matcher; oto has no
// write path into the cluster and v1 will not grow one (R3, H-3). A button that
// silenced an alert from a chat message would be a world-changing action taken
// from a surface with no confirmation, no audit trail of oto's own, and no way
// to undo.
func (v *ViewService) links(snap domain.Snapshot) Links {
	var l Links
	if v.baseURL != "" {
		// ⛔⛔ `/cases/`, AND THE OLD RULE HAS INVERTED (git-bug `7570090`). It read:
		// "`/groups/`, AND NOT `/cases/`. A card is about an ALERTGROUP … minting
		// `/cases/<group id>` sent an operator to a detail page addressed by an id
		// that names a different table."
		//
		// ⭐ THE RULE WAS ALWAYS "the id must name the table the screen reads", and it
		// is the ID THAT MOVED, not the rule. `snap.Group.ID` is a CASE id now — the
		// snapshot is taken by `SnapshotQuery.CaseID` and a conversation holds exactly
		// one Case — so `/groups/` would be the defect this comment was written to
		// prevent, pointing the other way. `/cases/` is also where the product already
		// sends people: `web/src/App.tsx` redirects `/` there, and the `/groups`
		// screens are deleted.
		l.Group = v.baseURL + "/cases/" + snap.Group.ID.String()
		l.Timeline = l.Group + "/timeline"
		if snap.Focus != nil {
			l.Alert = v.baseURL + "/alerts/" + snap.Focus.ID.String()
		}
	}

	// ⛔ THE ONLY SILENCE AFFORDANCE v1 HAS (R3, §H.3). It is a DEEP LINK into
	// Alertmanager's own UI, pre-filled with a matcher, and it performs no API call
	// and creates no oto state. Without it the card is missing the one action an
	// operator reaches for at 03:00 after they have understood the alert, and they
	// go and hand-build the matcher themselves.
	//
	// The filter is Alertmanager's own matcher syntax, `{a="b", c="d"}`, built from
	// the GROUP LABELS — the labels Alertmanager itself grouped by, so the silence
	// covers exactly this card and not the whole alertname across every cluster.
	//
	// ⛔ THE EMPTY CHECK IS ALSO THE SOURCE-KIND GUARD, and both of these URL
	// shapes are ALERTMANAGER'S OWN CONSOLE — `/#/alerts`, `/#/silences/new` — not
	// a shape every source serves. `AlertmanagerURL` is a UI root oto has vouched
	// for or nothing at all (see its doc on `domain.GroupFacts`), so a group whose
	// source is a Grafana, or whose source resolved to nothing, arrives here empty
	// and leaves with no link — and `actions` below already reads an empty
	// `AlertmanagerSilenceNew` as no Silence button. Never guess the missing half:
	// an operator who clicks a fabricated link mid-incident and lands on a 404 has
	// lost the one action v1 offers and gained a reason not to trust the next card.
	if base := strings.TrimRight(snap.Group.AlertmanagerURL, "/"); base != "" {
		if filter := alertmanagerFilter(snap); filter != "" {
			escaped := url.QueryEscape(filter)
			l.Alertmanager = base + "/#/alerts?filter=" + escaped
			l.AlertmanagerSilenceNew = base + "/#/silences/new?filter=" + escaped
		} else {
			l.Alertmanager = base + "/#/alerts"
		}
	}

	source := snap.Focus
	if source == nil && len(snap.Alerts) > 0 {
		source = &snap.Alerts[0]
	}
	if source == nil {
		return l
	}

	// The generator URL is Prometheus's own link to the expression that fired. It
	// comes from the upstream payload, so it is the one link guaranteed to point
	// at the query an operator actually wants.
	l.Prometheus = source.GeneratorURL
	l.Runbook = firstAnnotation(source.Annotations,
		"runbook_url", "runbook", "runbook_uri", "playbook_url")
	l.GrafanaDashboard = firstAnnotation(source.Annotations, "__dashboardUid__", "dashboard_url")
	l.GrafanaPanel = firstAnnotation(source.Annotations, "__panelId__", "panel_url")
	return l
}

// actions builds the interactive affordances.
//
// Every action id is stable and every URL action is still an INTERACTION the
// provider will deliver and oto must acknowledge — hence the explicit
// `oto.noop.*` namespace (S9). A silent no-op button shows the user "this app is
// not responding", which is worse than no button.
//
// `value` carries an OPAQUE ID ONLY (S8). Never a payload, never trusted: state
// is looked up in oto's own database when the click comes back.
func (v *ViewService) actions(snap domain.Snapshot) []Action {
	// ⛔⛔ THIS VALUE WAS A GROUP ID AND IS NOW A CASE ID (git-bug `7570090`), and it
	// is the one field in this file where getting it wrong is SILENT. The action ids
	// (`oto.ack` / `oto.unack`) and the shape are unchanged — `value` is still one
	// opaque uuid, still never trusted, still looked up in oto's own database when
	// the click comes back (S8). What changed is which table that lookup reads:
	// `channels/service`'s ack port is Case-shaped now, so a group uuid here would
	// resolve to nothing and every Acknowledge button in every channel would fail.
	//
	// ⭐ It fails HONESTLY rather than silently — the port answers a missing Case with
	// a refusal the user sees — which is why this is a correctness bug and not a data
	// one. But it would be a correctness bug on every card at once.
	caseID := snap.Group.ID.String()
	acked := snap.Case != nil && snap.Case.AckState == "acked"

	actions := make([]Action, 0, 4)
	if acked {
		actions = append(actions, Action{
			ID: "oto.unack", Label: "Un-acknowledge", Value: caseID,
		})
	} else {
		// Exactly ONE primary button, always (S10).
		actions = append(actions, Action{
			ID: "oto.ack", Label: "Acknowledge", Style: "primary", Value: caseID,
		})
	}

	// ⭐ §B.8.6's SNOOZE PAIR, AND IT IS ONE AFFORDANCE IN TWO SHAPES. "The `Snooze`
	// action becomes `:bell: Unsnooze`" — never both, because a card offering both
	// would be asking the reader which of two contradictory facts about oto is true.
	// `SnoozedUntil` is the same fact the `*Notifications*` field on the card is
	// drawn from, so the field and the action can no longer disagree: they read one
	// projection (git-bug `0a8ca4a`).
	//
	// ⛔ THE SUBJECT IS THE ALERT AND NOT THE CASE, WHICH IS WHY IT IS NOT `caseID`.
	// A snooze is a fact about a SIGNAL's notification behaviour (§B.8.7): it is
	// scoped to an `alert_key`, it outlives the episode that provoked it, and
	// `alerts/service`'s verbs take an alert id for exactly that reason. Two bare
	// uuids on one card naming two different tables is a hazard, so it is stated
	// here and stated again on the handler's action ids.
	//
	// ⛔ IT COMES BEFORE THE TWO LINK BUTTONS ON PURPOSE. A renderer with a narrower
	// row than Slack's sheds from the end, and "where do I look" survives being one
	// tap further away in a way that "make oto stop shouting at 03:00" does not.
	if alertID := snoozeAlertID(snap); alertID != "" {
		if snoozedUntil(snap) != nil {
			actions = append(actions, Action{
				ID: "oto.unsnooze", Label: "Unsnooze", Value: alertID,
			})
		} else {
			// ⛔ FIVE PRESETS AND NO FREE-TEXT DURATION (§B.8.3). The list is
			// `channels/domain`'s, not this file's, because the handler that decodes
			// the answer reads the same table — a menu offering a choice the handler
			// cannot decode is a button that does nothing, which is the defect this
			// whole affordance was filed against.
			//
			// The option value carries the preset TOKEN as well as the alert, because
			// a menu press has to say which of the offered choices was taken. It is
			// still two selectors and no state: see `channels/service`'s
			// snoozeValueSeparator for why that is not a widening of S8.
			presets := snoozePresets()
			opts := make([]ActionOption, 0, len(presets))
			for _, p := range presets {
				opts = append(opts, ActionOption{
					Label: p.Label,
					Value: p.Token + snoozeValueSeparator + alertID,
				})
			}
			actions = append(actions, Action{
				ID: "oto.snooze", Label: "Snooze for…", Options: opts,
			})
		}
	}

	links := v.links(snap)
	if links.Runbook != "" {
		actions = append(actions, Action{
			ID: "oto.noop.runbook", Label: "Runbook", URL: links.Runbook,
		})
	}
	if links.AlertmanagerSilenceNew != "" {
		actions = append(actions, Action{
			ID: "oto.noop.silence", Label: "Silence", URL: links.AlertmanagerSilenceNew,
		})
	}
	return actions
}

// snoozeAlertID is the ALERT the card's snooze affordance acts on, as a string, and
// "" when there is none to name.
//
// ⛔ IT IS THE SAME CHOICE `links` MAKES, AND DELIBERATELY THE SAME CODE SHAPE:
// focus first, then the newest member. A Case holds exactly one Alert, so the two
// arms agree in production; the fallback is what keeps a snapshot built without a
// focus from silently rendering a menu whose every option names the nil uuid.
//
// ⚠️ IT IS NOT `snoozedUntil`'S SUBJECT BY COINCIDENCE. That function reads the
// focus for the same reason — a snooze is scoped to an `alert_key` and a group
// snooze is a fan-out of one row per member — so the alert whose clock the card
// PRINTS is the alert the card's button ACTS on. If those two ever diverged, a card
// would offer to wake an alert other than the one it says is asleep.
func snoozeAlertID(snap domain.Snapshot) string {
	source := snap.Focus
	if source == nil && len(snap.Alerts) > 0 {
		source = &snap.Alerts[0]
	}
	if source == nil || source.ID == uuid.Nil {
		return ""
	}
	return source.ID.String()
}

// alertmanagerFilter renders Alertmanager's matcher syntax for this generation.
//
// It uses the GROUP LABELS and nothing else. They are the labels Alertmanager
// grouped by, so a silence built from them mutes exactly the set this card is
// about — a filter built from one member's full label set would silence one pod,
// and one built from `alertname` alone would silence every cluster.
//
// Values are quoted and the quotes inside them escaped: a label value is
// upstream-supplied text and this string ends up in a URL somebody clicks.
func alertmanagerFilter(snap domain.Snapshot) string {
	labels := snap.Group.GroupLabels
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(labels[n])
		parts = append(parts, n+`="`+v+`"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func firstAnnotation(annotations map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(annotations[k]); v != "" {
			return v
		}
	}
	return ""
}
