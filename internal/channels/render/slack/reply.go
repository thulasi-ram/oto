package slack

import (
	"slices"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// The oto Reason values (§H.6, and the notifications_reason_ck CHECK in §D.8).
//
// They are duplicated here as unexported constants rather than imported: the
// notification module owns the enum, and channels/domain deliberately does not
// depend on it. If the two ever disagree, the reply falls through to the generic
// branch and still says something true.
const (
	reasonFired           = "fired"
	reasonNewAlerts       = "new_alerts"
	reasonSomeResolved    = "some_resolved"
	reasonAllResolved     = "all_resolved"
	reasonRepeat          = "repeat"
	reasonSuppressed      = "suppressed"
	reasonUnsuppressed    = "unsuppressed"
	reasonExpired         = "expired"
	reasonRefired         = "refired"
	reasonAcked           = "acked"
	reasonUnacked         = "unacked"
	reasonEnriched        = "enriched"
	reasonRuleChanged     = "rule_changed"
	reasonComment         = "comment"
	reasonUnackedReminder = "unacked_reminder"
	reasonStorm           = "storm"
	reasonSeverityRaised  = "severity_raised"

	// System reply types. They have no Reason in the DDL because they are facts
	// about oto's own delivery machinery, not about the signal (§H.5).
	reasonDegraded  = "degraded"
	reasonContinued = "continued"
)

// renderReply builds a thread reply (§H.5).
//
// Replies are the EXCEPTION, not the rule. The root card is amended in place for
// almost everything, because chat.update is fifty times cheaper than a post and
// because a thread that grows a line per event is a thread nobody reads. A reply
// exists only when a human needs to be told something the card cannot show by
// changing: who acted, what they said, and that the rule underneath moved.
func (r *Renderer) renderReply(v *domain.NotificationView, o domain.RenderOptions) (Payload, string, string) {
	nonce := renderNonce(v, o)
	body, extra, colour := r.replyBody(v, o)

	blocks := []Block{sectionBlock(blockID("reply", nonce), truncateSection(body, v.Links.Group))}
	if extra != "" {
		blocks = append(blocks, contextBlock(blockID("replyctx", nonce),
			Text{Type: TypeMrkdwn, Text: truncateField(extra, v.Links.Group)}))
	}

	fallback := replyText(v)
	return Payload{
		Text:        fallback,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			Color:    colour,
			Fallback: truncateRunes(fallback, 200),
			Blocks:   blocks,
		}},
	}, fallback, fallback
}

// replyBody returns the reply's mrkdwn, an optional context line, and the colour
// bar. Every branch is one of the §H.5 reply types.
func (r *Renderer) replyBody(v *domain.NotificationView, o domain.RenderOptions) (body, extra, colour string) {
	state := cardState(v)
	who := actorLabel(v)

	switch v.Reason {
	case reasonAcked:
		colour = CardAcknowledged.Colour()
		body = ":eyes: *Acknowledged*" + by(who)
		if note := ackNote(v); note != "" {
			body += " — _" + note + "_"
		}

	case reasonUnacked:
		colour = CardFiring.Colour()
		body = ":arrow_uturn_left: *Un-acknowledged*" + by(who)
		if v.Occurrence != nil && v.Occurrence.ReopenCount > 0 {
			body += " — new occurrence opened"
		}

	case reasonNewAlerts:
		colour = CardFiring.Colour()
		added := newlyFiring(v)
		body = ":heavy_plus_sign: *" + plural(len(added), "more instance now firing", "more instances now firing") + "*"
		if names := nameList(added, o); names != "" {
			body += " — " + names
		}
		if v.Group.TotalCount > 0 {
			body += " (" + strconv.Itoa(v.Group.TotalCount) + " total)"
		}

	case reasonRefired:
		colour = CardFiring.Colour()
		body = ":repeat: *Re-fired*"
		if v.Occurrence != nil {
			if v.Occurrence.Duration > 0 {
				body += " after " + humanDuration(v.Occurrence.Duration)
			}
			body += " — occurrence #" + strconv.Itoa(v.Occurrence.Seq)
			if v.Occurrence.ReopenCount > 0 {
				body += ", reopen #" + strconv.Itoa(v.Occurrence.ReopenCount)
			}
		}

	case reasonAllResolved:
		colour = CardResolved.Colour()
		body = ":white_check_mark: *All resolved*"
		if d := resolvedAfter(v); d != "" {
			body += " after " + d
		}
		if v.Group.TotalCount > 0 {
			body += " — " + strconv.Itoa(v.Group.ResolvedCount) + " of " +
				strconv.Itoa(v.Group.TotalCount) + " instances"
		}

	case reasonSomeResolved:
		// §H.6 makes some_resolved update-only. If a channel asks for it anyway,
		// say something true rather than something empty.
		colour = CardFiring.Colour()
		body = ":arrow_down: *" + strconv.Itoa(v.Group.ResolvedCount) + " of " +
			strconv.Itoa(v.Group.TotalCount) + " instances resolved* — the rest are still firing"

	case reasonExpired:
		colour = CardExpired.Colour()
		body = ":grey_question: *Expired* — oto has not heard about this since " +
			slackDate(v.Group.LastActivityAt) + ". This is NOT a resolution."
		extra = "_oto stopped receiving this alert. It may still be happening._"

	case reasonSuppressed:
		colour = CardSuppressed.Colour()
		body = ":mute: *Silenced*" + by(who)
		if until := suppressedUntil(v); until != "" {
			body += " until " + until
		}
		if note := suppressionNote(v); note != "" {
			body += " — _" + note + "_"
		}

	case reasonUnsuppressed:
		colour = CardFiring.Colour()
		body = ":speaker: *Silence ended* — this alert is firing again"

	case reasonRuleChanged:
		colour = state.Colour()
		body, extra = ruleChangedReply(v)

	case reasonEnriched:
		colour = state.Colour()
		body = ":sparkles: " + enrichmentSummary(v)

	case reasonComment:
		colour = state.Colour()
		body = ":speech_balloon: " + commentPrefix(who) + escape(oneLine(v.Comment))

	case reasonUnackedReminder:
		colour = CardFiring.Colour()
		body = ":rotating_light: *Still unacknowledged"
		if d := unackedFor(v); d != "" {
			body += " after " + d
		}
		body += ".*"
		if m := mentionList(r.mentions); m != "" {
			body += " " + m
		}
		body += " — " + link(v.Links.Group, "open in oto")

	case reasonSeverityRaised:
		// ⛔ ADR 0020 RENDERING RULE 5: this reply BROADCASTS, and the in-channel
		// reference Slack builds from it carries NEITHER THE ATTACHMENT NOR ITS
		// BLOCKS — no colour bar, no Acknowledge button. So the severity is stated
		// in WORDS and in an EMOJI, never left to the colour, and the call to
		// action is "open the thread". The colour below is for the thread copy
		// only; a reader who sees only the channel reference must lose nothing.
		colour = CardFiring.Colour()
		body = ":rotating_light: *Severity raised to " + escape(severityWord(v)) + "*"
		if was := previousSeverity(v); was != "" {
			body += " — was " + code(was)
		}
		body += ". " + link(v.Links.Group, "open in oto") + "."

	case reasonStorm:
		colour = CardStorm.Colour()
		body = ":zap: *Storm damping on* — " + plural(v.StormCount, "alert", "alerts") +
			" in this group. Individual notifications are suppressed. " +
			link(v.Links.Group, "see them all") + "."

	case reasonDegraded:
		colour = CardExpired.Colour()
		body = ":warning: oto could not deliver an update to this thread. " +
			link(v.Links.Group, "See Deliveries in oto") + "."

	case reasonContinued:
		colour = state.Colour()
		body = ":arrow_right: *Continued in a new message* — this thread reached 30 replies. " +
			link(v.Links.Group, "jump") + "."

	case reasonFired, reasonRepeat:
		// Neither ever produces a reply (§H.6): first notification is a root post
		// and repeat is update-only. Reaching here is a dispatch bug, so say so
		// plainly rather than posting a blank line.
		colour = state.Colour()
		body = ":bell: *" + escape(v.Group.Title) + "* — " + statusValue(v, state)

	default:
		colour = state.Colour()
		body = ":information_source: *" + escape(v.Group.Title) + "* — " + statusValue(v, state)
	}

	return body, extra, colour
}

// ruleChangedReply is the headline differentiator. It is ALWAYS delivered,
// regardless of verbosity, unless the group is in storm mode (§H.5): "the rule
// underneath this alert changed" is the single most valuable thing oto can say.
func ruleChangedReply(v *domain.NotificationView) (string, string) {
	rc := v.RuleChange
	if rc == nil {
		return ":scroll: *The rule changed since the last occurrence.*", ""
	}

	var b strings.Builder
	b.WriteString(":scroll: *The rule changed since the last occurrence.*")

	if rc.ExprChanged {
		b.WriteString("\n```")
		b.WriteString("\n- " + oneLine(rc.PreviousExpr))
		b.WriteString("\n+ " + oneLine(rc.NewExpr))
		b.WriteString("\n```")
	}
	if rc.ForChanged {
		b.WriteString("\n`for:` " + humanDuration(rc.PreviousFor) + " → " + humanDuration(rc.NewFor))
	}
	for _, d := range sortedDiff(rc.LabelDiff) {
		b.WriteString("\n`" + escape(d.name) + ":` " + diffPair(d.pair))
	}
	for _, d := range sortedDiff(rc.AnnotationDiff) {
		b.WriteString("\n`" + escape(d.name) + ":` " + diffPair(d.pair))
	}

	ctx := "_captured " + slackDateTime(rc.PreviousCapturedAt) + "_"
	if v.Links.Timeline != "" {
		ctx += " · " + link(v.Links.Timeline, "rule history")
	}
	return b.String(), ctx
}

func diffPair(pair [2]string) string {
	old, cur := pair[0], pair[1]
	switch {
	case old == "":
		return "added " + code(cur)
	case cur == "":
		return "removed (was " + code(old) + ")"
	default:
		return code(old) + " → " + code(cur)
	}
}

type diffEntry struct {
	name string
	pair [2]string
}

// sortedDiff yields a stable iteration order. Map order is random in Go, and a
// renderer whose output changes between two identical inputs cannot be golden-file
// tested and would trigger a pointless chat.update on every evaluation.
func sortedDiff(m map[string][2]string) []diffEntry {
	out := make([]diffEntry, 0, len(m))
	for k, v := range m {
		out = append(out, diffEntry{name: k, pair: v})
	}
	slices.SortFunc(out, func(a, b diffEntry) int { return strings.Compare(a.name, b.name) })
	return out
}

func sortStrings(s []string) { slices.Sort(s) }

func enrichmentSummary(v *domain.NotificationView) string {
	names := make([]string, 0, len(v.Enrichments))
	for _, e := range v.Enrichments {
		if e.Status != "" && e.Status != "ok" && e.Status != "success" {
			continue
		}
		names = append(names, enricherLabel(e.Enricher))
	}
	if len(names) == 0 {
		return "*Enrichment finished* — nothing new to add"
	}
	sortStrings(names)
	return "+" + strconv.Itoa(len(names)) + " enrichments — " + strings.Join(names, ", ")
}

// enricherLabel turns "oto.rules.definition" into "rule definition": the reader
// wants the fact, not oto's package naming.
func enricherLabel(name string) string {
	parts := strings.Split(name, ".")
	if len(parts) > 1 {
		parts = parts[1:]
	}
	return escape(strings.ReplaceAll(strings.Join(parts, " "), "_", " "))
}

// severityWord is the severity this reply is announcing, in WORDS.
//
// ⛔ ADR 0020 RENDERING RULE 5. The channel-visible form of a broadcast carries
// no attachment, so it carries no colour bar: "the card went red" is a fact only
// a thread reader gets. The word is the whole message for everybody else.
//
// It prefers the FOCUS — a severity rise is a fact about one Alert
// (notifications_focus_ck) — and falls back to the group so the sentence is never
// left with a hole in it.
func severityWord(v *domain.NotificationView) string {
	raw := ""
	if v.Focus != nil {
		raw = v.Focus.Severity
	}
	if raw == "" {
		raw = v.Group.Severity
	}
	if raw == "" {
		return "a higher severity"
	}
	return raw
}

// previousSeverity is the severity the card showed before, or "" when the view
// does not carry one. The reply must read correctly either way.
func previousSeverity(v *domain.NotificationView) string {
	if v.Previous != nil {
		return v.Previous.Severity
	}
	return ""
}

func commentPrefix(who string) string {
	if who == "" {
		return ""
	}
	return who + ": "
}

func by(who string) string {
	if who == "" {
		return ""
	}
	return " by " + who
}

func ackNote(v *domain.NotificationView) string {
	if v.Occurrence != nil && v.Occurrence.AckNote != "" {
		return escape(oneLine(v.Occurrence.AckNote))
	}
	if v.Comment != "" {
		return escape(oneLine(v.Comment))
	}
	return ""
}

func suppressionNote(v *domain.NotificationView) string {
	if v.Occurrence != nil && v.Occurrence.SuppressionReason != "" {
		return escape(oneLine(v.Occurrence.SuppressionReason))
	}
	if v.Comment != "" {
		return escape(oneLine(v.Comment))
	}
	return ""
}

func suppressedUntil(v *domain.NotificationView) string {
	if v.Occurrence != nil && v.Occurrence.EndedAt != nil {
		return slackDate(*v.Occurrence.EndedAt)
	}
	return ""
}

func resolvedAfter(v *domain.NotificationView) string {
	if v.Occurrence != nil && v.Occurrence.Duration > 0 {
		return humanDuration(v.Occurrence.Duration)
	}
	if !v.Group.FirstSeenAt.IsZero() && v.Group.LastActivityAt.After(v.Group.FirstSeenAt) {
		return humanDuration(v.Group.LastActivityAt.Sub(v.Group.FirstSeenAt))
	}
	return ""
}

func unackedFor(v *domain.NotificationView) string {
	start := v.Group.FirstSeenAt
	if v.Occurrence != nil && !v.Occurrence.StartedAt.IsZero() {
		start = v.Occurrence.StartedAt
	}
	if start.IsZero() || !v.RenderedAt.After(start) {
		return ""
	}
	return humanDuration(v.RenderedAt.Sub(start))
}

// newlyFiring is the members that arrived with this notification.
func newlyFiring(v *domain.NotificationView) []domain.AlertView {
	out := make([]domain.AlertView, 0, len(v.Alerts))
	if v.Focus != nil {
		out = append(out, *v.Focus)
		return out
	}
	for _, a := range v.Alerts {
		if a.State == "firing" && a.TotalOccurrences <= 1 {
			out = append(out, a)
		}
	}
	if len(out) == 0 && len(v.Alerts) > 0 {
		out = append(out, v.Alerts[0])
	}
	return out
}

func nameList(alerts []domain.AlertView, o domain.RenderOptions) string {
	limit := o.MaxInstances
	if limit <= 0 {
		limit = defaultMaxInstances
	}
	names := make([]string, 0, limit)
	for i, a := range alerts {
		if i >= limit {
			names = append(names, "_and "+strconv.Itoa(len(alerts)-limit)+" more_")
			break
		}
		names = append(names, code(instanceName(a)))
	}
	return strings.Join(names, ", ")
}

// replyText is the reply's own complete sentence. A thread reply's push
// notification is read exactly as often as the root's, and by the same people.
//
// ⛔⛔ ADR 0020 RENDERING RULE 4 MAKES THIS CORRECTNESS, NOT STYLE. When a reply
// broadcasts, Slack delivers a `thread_broadcast` reference into the channel, and
// that reference "cannot contain attachments or message buttons". SPEC §H.1 S3
// puts ALL of oto's blocks inside one attachment — so for a broadcasting reply
// THIS STRING IS VERY NEARLY EVERYTHING A CHANNEL READER SEES. No colour bar, no
// Acknowledge button, no blocks. A broadcast whose text reads "Re-fired" is a
// broadcast that communicates nothing.
func replyText(v *domain.NotificationView) string {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	lead := replyLead(v.Reason)
	if v.Reason == reasonSeverityRaised {
		// The severity goes in the lead itself, because "Severity raised:" without
		// the new value is the one thing a reader of the channel copy cannot look
		// up without opening the thread — which is what this sentence is trying to
		// make them decide to do.
		lead = SeverityEmoji(severityWord(v)) + " Severity raised to " + severityWord(v) + ":"
	}
	out := lead + " " + title
	if v.Group.ClusterKey != "" {
		out += " on " + v.Group.ClusterKey
	}
	out += "."
	return truncateRunes(oneLine(out), otoTopLevelText)
}

func replyLead(reason string) string {
	switch reason {
	case reasonAcked:
		return ":eyes: Acknowledged:"
	case reasonUnacked:
		return ":arrow_uturn_left: Un-acknowledged:"
	case reasonNewAlerts:
		return ":heavy_plus_sign: More instances now firing:"
	case reasonRefired:
		return ":repeat: Re-fired:"
	case reasonAllResolved:
		return ":white_check_mark: All resolved:"
	case reasonSomeResolved:
		return ":arrow_down: Partly resolved:"
	case reasonExpired:
		return ":grey_question: Expired — not resolved:"
	case reasonSuppressed:
		return ":mute: Silenced:"
	case reasonUnsuppressed:
		return ":speaker: Silence ended:"
	case reasonRuleChanged:
		return ":scroll: The alerting rule changed for:"
	case reasonEnriched:
		return ":sparkles: New enrichment for:"
	case reasonComment:
		return ":speech_balloon: New comment on:"
	case reasonUnackedReminder:
		return ":rotating_light: Still unacknowledged:"
	case reasonStorm:
		return ":zap: Storm damping on for:"
	case reasonSeverityRaised:
		// Overridden by replyText, which has the view and can name the severity.
		// This is the fallback for a caller that has only the Reason.
		return ":rotating_light: Severity raised for:"
	case reasonDegraded:
		return ":warning: oto could not update the thread for:"
	case reasonContinued:
		return ":arrow_right: Continued in a new message:"
	default:
		return ":bell: Update on:"
	}
}
