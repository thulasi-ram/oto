package service

import (
	"context"
	"net/url"
	"sort"
	"strings"

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
	maxAlerts int
	clk       clock.Clock
}

// ViewConfig configures the view builder.
type ViewConfig struct {
	Snapshots SnapshotSource
	// BaseURL is oto's public URL, used for every deep link on the card.
	BaseURL string
	// MaxInstances caps the inline member list. Zero means DefaultMaxInstances.
	MaxInstances int
	Clock        clock.Clock
}

// NewViewService builds the view service.
func NewViewService(cfg ViewConfig) (*ViewService, error) {
	if cfg.Snapshots == nil {
		return nil, errs.New(errs.KindInternal, "view_service_deps", "a snapshot source is required")
	}
	v := &ViewService{
		snapshots: cfg.Snapshots,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		maxAlerts: cfg.MaxInstances,
		clk:       cfg.Clock,
	}
	if v.maxAlerts <= 0 {
		v.maxAlerts = DefaultMaxInstances
	}
	if v.clk == nil {
		v.clk = clock.New()
	}
	return v, nil
}

// ViewRequest names the fact to be rendered.
type ViewRequest struct {
	Notification domain.Notification
	// Actor labels who caused the fact, for the human-caused reasons.
	Actor string
	// Comment is the text of a `comment` notification.
	Comment string
}

// Build reads the world and projects it into the renderer's read model.
func (v *ViewService) Build(
	ctx context.Context, scope db.TenantScope, req ViewRequest,
) (*NotificationView, error) {
	n := req.Notification
	snap, err := v.snapshots.Snapshot(ctx, scope, domain.SnapshotQuery{
		GroupID:      n.GroupID,
		AlertID:      n.AlertID,
		OccurrenceID: n.OccurrenceID,
		MaxAlerts:    v.maxAlerts,
	})
	if err != nil {
		return nil, err
	}
	return v.project(snap, req), nil
}

// project is the pure mapping, exported through Build. It performs no I/O, so
// the seam is exercisable from a snapshot literal.
func (v *ViewService) project(snap domain.Snapshot, req ViewRequest) *NotificationView {
	n := req.Notification

	view := &NotificationView{
		Org: OrgRef{
			ID: snap.Org.ID.String(), Slug: snap.Org.Slug, Name: snap.Org.Name,
		},
		Reason:     string(n.Reason),
		Group:      v.group(snap),
		Alerts:     v.alerts(snap),
		Comment:    req.Comment,
		RenderedAt: snap.TakenAt,
	}

	if snap.Focus != nil {
		focus := alertView(*snap.Focus)
		view.Focus = &focus
	}
	if snap.Occurrence != nil {
		view.Occurrence = v.occurrence(snap)
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
	if req.Actor != "" {
		view.Actor = &ActorView{Kind: "user", Label: req.Actor}
	} else if snap.Actor != nil {
		view.Actor = &ActorView{
			Kind: snap.Actor.Kind, ID: snap.Actor.ID, Label: snap.Actor.Label,
		}
	}

	// StormMode is a VISIBLE state (§B.6). It is carried as a count rather than a
	// flag because the number is the point: "214 alerts in 60s" tells an operator
	// what happened, "storm" tells them a word.
	if snap.Group.StormMode {
		view.StormCount = snap.Group.StormCount
	}

	// §H.4's strikethrough trick needs the state the card showed BEFORE this
	// delivery, and the Reason is where that fact lives: a Reason is the name of a
	// transition, so it names both ends of it. Deriving it here rather than reading
	// the previously rendered payload keeps the renderer pure and keeps the answer
	// stable when Slack loses a message and oto posts a fresh root.
	view.Previous = previousState(n.Reason, snap)
	view.Trail = trail(snap)
	view.Notifications = snap.NotificationCount

	view.Links = v.links(snap)
	view.Actions = v.actions(snap)
	return view
}

// trailKinds maps `alert_events.type` onto the renderer's small closed
// vocabulary. A type with no entry is DROPPED, never printed raw: the trail is
// read by a human at 03:00 and `occurrence.unsuppressed` is not a sentence.
var trailKinds = map[string]string{
	"occurrence.opened":         "fired",
	"occurrence.reopened":       "refired",
	"occurrence.acknowledged":   "acked",
	"occurrence.unacknowledged": "unacked",
	"occurrence.suppressed":     "suppressed",
	"occurrence.unsuppressed":   "unsuppressed",
	"occurrence.resolved":       "resolved",
	"occurrence.expired":        "expired",
	"group.storm_started":       "storm",
	"group.storm_ended":         "storm_ended",
	// `group.opened` and `group.closed` are oto's OWN bookkeeping about a
	// generation, not things that happened to the signal. They are deliberately
	// absent: a trail that says "oto opened a group" answers a question nobody
	// asked and pushes out a line that answers one somebody did.
}

// trail projects the group's state history onto the card's receipt.
//
// ⛔ CONSECUTIVE DUPLICATES COLLAPSE. A group of twelve instances that all fire
// in one batch produces twelve `occurrence.opened` events, and a trail reading
// "fired → fired → fired …" is noise wearing a history's clothes. The FIRST of
// each run is kept, because the first is when the state actually changed.
func trail(snap domain.Snapshot) []TrailEntry {
	out := make([]TrailEntry, 0, len(snap.Trail))
	for _, f := range snap.Trail {
		kind, ok := trailKinds[f.Type]
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

	switch reason {
	case domain.ReasonAcked, domain.ReasonSuppressed, domain.ReasonExpired,
		domain.ReasonSomeResolved, domain.ReasonAllResolved:
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
		// The thread said resolved. That is exactly what makes a re-fire worth
		// broadcasting (ADR 0020), and it is what the strikethrough should say.
		return &PreviousState{State: "resolved"}
	case domain.ReasonFired, domain.ReasonNewAlerts, domain.ReasonRepeat,
		domain.ReasonSnoozed, domain.ReasonUnsnoozed, domain.ReasonEnriched,
		domain.ReasonRuleChanged, domain.ReasonComment,
		domain.ReasonUnackedReminder, domain.ReasonStorm:
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
		StormMode:       g.StormMode,
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
		Severity:         a.Severity,
		Namespace:        a.Namespace,
		Service:          a.Service,
		ClusterKey:       a.ClusterKey,
		Labels:           a.Labels,
		Annotations:      a.Annotations,
		GeneratorURL:     a.GeneratorURL,
		State:            a.State,
		AckState:         a.AckState,
		FirstSeenAt:      a.FirstSeenAt,
		LastSeenAt:       a.LastSeenAt,
		TotalOccurrences: a.TotalOccurrences,
		IsFlapping:       a.IsFlapping,
		Value:            a.Value,
	}
}

func (v *ViewService) occurrence(snap domain.Snapshot) *OccurrenceView {
	o := snap.Occurrence
	return &OccurrenceView{
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
		ReopenCount:  o.ReopenCount,
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
		l.Group = v.baseURL + "/groups/" + snap.Group.ID.String()
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
	groupID := snap.Group.ID.String()
	acked := snap.Occurrence != nil && snap.Occurrence.AckState == "acked"

	actions := make([]Action, 0, 4)
	if acked {
		actions = append(actions, Action{
			ID: "oto.unack", Label: "Un-acknowledge", Value: groupID,
		})
	} else {
		// Exactly ONE primary button, always (S10).
		actions = append(actions, Action{
			ID: "oto.ack", Label: "Acknowledge", Style: "primary", Value: groupID,
		})
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
