package slack

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// renderRoot builds the card that is posted once per AlertGroup generation and
// then amended in place for its entire life (§H.3, ADR 0008).
//
// The layout is deliberately calm and dense. Eight blocks against a ceiling of
// fifty, one colour, one emoji, one bold title that is also the deep link. An
// operator reading it at 03:00 should be able to answer "what, where, how bad,
// how long, what do I do" without scrolling and without clicking.
func (r *Renderer) renderRoot(v *domain.NotificationView, o domain.RenderOptions) (Payload, string) {
	state := cardState(v)
	nonce := renderNonce(v, o)
	now := r.renderedAt(v)
	blocks := make([]Block, 0, 8)

	blocks = append(blocks, r.titleBlock(v, o, state, nonce))
	if b, ok := r.bodyBlock(v, nonce); ok {
		blocks = append(blocks, b)
	}
	blocks = append(blocks, r.fieldsBlock(v, o, state, now, nonce))
	if b, ok := r.membersBlock(v, o, state, nonce); ok {
		blocks = append(blocks, b)
	}
	if b, ok := r.trailBlock(v, state, nonce); ok {
		blocks = append(blocks, b)
	}
	if b, ok := r.ruleBlock(v, nonce); ok {
		blocks = append(blocks, b)
	}
	if b, ok := r.actionsBlock(v, state, nonce); ok {
		blocks = append(blocks, b)
	}
	blocks = append(blocks, r.footerBlock(v, o, state, now, nonce))

	fallback := rootText(v, state)

	return Payload{
		Text:        fallback,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			Color:    state.Colour(),
			Fallback: shortFallback(v, state),
			Blocks:   capBlocks(blocks),
		}},
	}, fallback
}

// titleBlock is a section, never a header (S1): a header is plain_text only, so
// it cannot carry the bold link into oto, and that link is the whole point.
func (r *Renderer) titleBlock(v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce string) Block {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	if title == "" {
		title = "Alert group"
	}

	head := leadEmoji(v, state) + " *" + link(v.Links.Group, truncateRunes(title, 140)) + "*"
	// The cluster is a CHIP beside the name, never part of it (§H.3). It comes
	// from the group's own cluster key when oto has one and from the `cluster`
	// group label when it does not — the first live run had only the label, so the
	// chip was empty and the cluster ended up dumped into the title instead.
	if cluster := clusterChip(v); cluster != "" {
		head += "  ·  " + code(cluster)
	}
	if summary := oneLine(annotation(v, "summary")); summary != "" {
		head += "\n_" + escape(truncateRunes(summary, 240)) + "_"
	}

	return sectionBlock(blockID("title", nonce), truncateSection(head, o.BaseURL))
}

// bodyBlock carries the alert's own prose. It is dropped entirely when there is
// none: an empty italic line is worse than no line (S11).
func (r *Renderer) bodyBlock(v *domain.NotificationView, nonce string) (Block, bool) {
	body := annotation(v, "description", "message")
	if body == "" {
		return Block{}, false
	}
	if body == annotation(v, "summary") {
		// Already shown under the title; repeating it is noise.
		return Block{}, false
	}
	return sectionBlock(blockID("body", nonce), truncateSection(escape(body), v.Links.Group)), true
}

// fieldsBlock renders the scannable two-column grid. Order is binding (§H.7) and
// lowest-priority fields are dropped first when the ten-item budget runs out.
func (r *Renderer) fieldsBlock(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, now time.Time, nonce string,
) Block {
	fields := make([]Text, 0, maxFields)
	add := func(label, value string) {
		if len(fields) >= maxFields || strings.TrimSpace(value) == "" {
			return
		}
		fields = append(fields, Text{
			Type: TypeMrkdwn,
			Text: truncateField("*"+label+"*\n"+value, v.Links.Group),
		})
	}

	add("Status", statusValue(v, state))
	add("Severity", severityValue(v, o))
	add("Service", code(firstNonEmpty(v.Group.GroupLabels["service"], focusField(v, func(a domain.AlertView) string { return a.Service }))))
	add("Namespace", code(firstNonEmpty(v.Group.GroupLabels["namespace"], focusField(v, func(a domain.AlertView) string { return a.Namespace }))))
	// ⛔ UPSTREAM'S `startsAt`, NOT OTO'S FIRST SIGHTING. They differ by oto's
	// ingest latency plus Alertmanager's `group_wait`, which was twenty-one
	// minutes in the first live run, and "Started 18:17" for a thing that started
	// at 17:56 is a false statement about how long an outage has lasted.
	add("Started", slackDate(groupStart(v)))
	add(durationLabel(state), durationValue(v, state, now))

	// ⭐ THE TERMINAL CARD IS A RECEIPT, NOT A BLANK STATE (§H.4, amended).
	//
	// `chat.update` is silent, so a channel reader watches a red card turn green
	// with no notification and no trace and cannot tell that anything happened.
	// These four fields are what makes the last version of the card readable as a
	// closed ticket: when it ended, what it hit, how loud oto was about it, and
	// whether a human had already picked it up. They are ordered after `Duration`
	// so §H.7's ten-field budget sheds `Flapping` and `Team` first, which are the
	// two that matter least once it is over.
	if state.IsTerminal() {
		add(terminalTimeLabel(state), slackDate(v.Group.LastActivityAt))
		add("Instances affected", instancesAffected(v))
		if v.Notifications > 0 {
			add("Notifications", plural(v.Notifications, "sent", "sent"))
		}
		add("Acknowledged", acknowledgedValue(v))
	}

	if n, ok := flappingCount(v); ok {
		add("Flapping", ":arrows_counterclockwise: "+plural(n, "transition", "transitions"))
	}
	// `team` is almost never a group-by label, so reading only the group labels
	// meant this field NEVER rendered. It is on the alert, which is where the
	// routing that produced the card came from.
	add("Team", escape(labelOf(v, "team")))

	return fieldsBlock(blockID("fields", nonce), fields)
}

func terminalTimeLabel(state CardState) string {
	if state == CardExpired {
		return "Last seen"
	}
	return "Resolved"
}

// instancesAffected is what the episode actually hit. It counts EVERY member of
// the generation, not the capped render list.
func instancesAffected(v *domain.NotificationView) string {
	if v.Group.TotalCount <= 0 {
		return ""
	}
	return plural(v.Group.TotalCount, "instance", "instances")
}

// acknowledgedValue records whether a human had taken this before it ended.
//
// ⛔ ACTOR, NEVER SUBJECT (ADR 0013). It says who acted on the signal; it is not
// a fact about a person's workload, it is not assignment, and "no" is a fact
// about the SIGNAL — that it resolved without anybody looking — which is one of
// the more useful things a receipt can tell an operator the next morning.
func acknowledgedValue(v *domain.NotificationView) string {
	if v.Case != nil && v.Case.AckedAt != nil {
		// ⛔ `ackedBy`, NOT `actorLabel`. This field is on the TERMINAL card, which
		// is amended by whatever fact ended the episode — and a receipt that read
		// the announcing notification's own actor would credit the ack to whoever
		// last commented on a resolved thread.
		// The receipt states the ack happened — `AckedAt` is the fact — so only the
		// NAME is wanted here; whether an unnamed ack was a machine is `by`'s
		// question, and this field never renders that clause.
		who, _ := ackedBy(v)
		out := ":eyes: yes"
		if who != "" {
			out += " — " + who
		}
		return out + ", " + slackDate(*v.Case.AckedAt)
	}
	if v.Group.AckedCount > 0 {
		return ":eyes: yes"
	}
	return "no — it resolved unacknowledged"
}

// membersBlock lists the affected instances, capped at MaxInstances with an
// explicit "and N more".
//
// ⛔ IT IS NO LONGER DROPPED WHEN THE CARD GOES TERMINAL, AND THAT REVERSAL IS
// THE POINT (§H.4, amended). §H.4 used to call the member list "zero information
// once resolved". That is true only for a reader who watched it happen. For
// everybody else — which is everybody in the channel, because `chat.update` is
// silent — the resolved card IS the record, and dropping the members made the
// card LEAST informative at exactly the moment it became the only thing left.
// A resolved card should read like a closed ticket, not an empty one.
//
// ⛔ NOTHING COLLAPSES THE LIST TO A COUNT ANY MORE. Storm mode used to, and it is
// removed (ADR 0042): a burst of members is a truthful report, and `MaxInstances`
// is the one bound on how much of it the card draws.
func (r *Renderer) membersBlock(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce string,
) (Block, bool) {
	if len(v.Alerts) <= 1 && !state.IsTerminal() {
		// One instance is zero information WHILE IT IS LIVE: the title already named
		// it (S11). On a terminal card it is the record of what was affected, and
		// "which box was it?" is the first question anybody asks afterwards.
		return Block{}, false
	}
	if len(v.Alerts) == 0 {
		return Block{}, false
	}

	limit := o.MaxInstances
	if limit <= 0 {
		limit = defaultMaxInstances
	}

	var b strings.Builder
	b.WriteString("*Affected instances*")
	shown := 0
	for _, a := range v.Alerts {
		if shown >= limit {
			break
		}
		b.WriteString("\n• ")
		b.WriteString(code(instanceName(a)))
		if extra := instanceDetail(a); extra != "" {
			b.WriteString(" — ")
			b.WriteString(extra)
		}
		shown++
	}
	if rest := v.Group.TotalCount - shown; rest > 0 {
		b.WriteString("\n_… and " + strconv.Itoa(rest) + " more_ · " + link(v.Links.Group, "see all in oto"))
	}

	return sectionBlock(blockID("members", nonce), truncateSection(b.String(), v.Links.Group)), true
}

// ruleBlock shows what the rule said at the moment this case fired. It is
// oto's defensible differentiator, and it costs one quiet context line.
//
// ⛔ IT SURVIVES THE CARD GOING TERMINAL, AND IT USED TO BE DROPPED. §H.4 said to
// shed the rule on resolve as "zero information once resolved". The opposite is
// true: the rule snapshot is THE RECORD OF WHY THIS FIRED, and the moment it
// matters most is afterwards, when somebody asks whether the threshold was
// sensible or when it last changed. Dropping it deleted the one thing oto has
// that nothing else does, from the one message that outlives the incident.
func (r *Renderer) ruleBlock(v *domain.NotificationView, nonce string) (Block, bool) {
	if v.Rule == nil {
		return Block{}, false
	}
	expr := oneLine(v.Rule.Expr)
	if expr == "" {
		return Block{}, false
	}
	text := ":mag: " + code(truncateRunes(expr, 900))
	if v.Rule.For > 0 {
		text += "   " + code("for: "+humanDuration(v.Rule.For))
	}
	if v.RuleChange != nil {
		text += "   :scroll: " + link(v.Links.Timeline, "the rule changed since the last case")
	}
	return contextBlock(blockID("rule", nonce), Text{Type: TypeMrkdwn, Text: truncateField(text, v.Links.Group)}), true
}

// trailBlock is the card's RECEIPT: the state trail, in one context line.
//
//	:red_circle: 09:14 fired → :eyes: 09:17 acked by @ram → :white_check_mark: 09:22 resolved · 8m
//
// ⭐ IT EXISTS BECAUSE `chat.update` IS SILENT AND DESTRUCTIVE. ADR 0008 makes
// the root the current state and the thread the history — right for a reader in
// the thread, and useless to the far larger number of people who only ever see
// the channel. They watch a red card become a green card with no notification and
// no trace, and, in the owner's words on seeing it happen: "it means something
// happened and we don't know."
//
// The trail is the answer that does not cost a message. It is one context block,
// it is re-rendered on every update, and it keeps `chat.update` as the primary
// verb: the goal is to stop the update erasing the story, not to start posting
// more of them.
//
// It is rendered in EVERY state, not only the terminal ones. A card that grows
// its history only at the end teaches nobody to look for it.
func (r *Renderer) trailBlock(v *domain.NotificationView, state CardState, nonce string) (Block, bool) {
	if len(v.Trail) < 2 {
		// One entry is not a trail: it says "it fired", which the Started field
		// already said better (S11).
		return Block{}, false
	}

	entries := v.Trail
	elided := 0
	if len(entries) > maxTrailShown {
		// ⛔ ELIDE THE MIDDLE, NEVER THE END. A long-lived flapper's trail is
		// unbounded, and the two entries a reader needs are the FIRST (when did this
		// begin) and the LAST (what is it now). Truncating the tail would throw away
		// the second one to keep transitions nobody is asking about.
		head := entries[:maxTrailHead]
		tail := entries[len(entries)-maxTrailTail:]
		elided = len(entries) - len(head) - len(tail)
		merged := make([]domain.TrailEntry, 0, maxTrailShown)
		merged = append(merged, head...)
		merged = append(merged, tail...)
		entries = merged
	}

	parts := make([]string, 0, len(entries)+1)
	for i, e := range entries {
		if elided > 0 && i == maxTrailHead {
			parts = append(parts, "_… "+strconv.Itoa(elided)+" more_")
		}
		parts = append(parts, trailEmoji(e.Kind)+" "+slackDate(e.At)+" "+trailVerb(e.Kind)+trailActor(e))
	}

	text := strings.Join(parts, "  →  ")
	if span := trailSpan(v, state); span != "" {
		text += "  ·  " + span
	}
	return contextBlock(blockID("trail", nonce),
		Text{Type: TypeMrkdwn, Text: truncateField(text, v.Links.Group)}), true
}

// The trail's budget. Twelve entries reach the renderer (MaxTrailEntries); six
// are shown, weighted to the recent end because that is where the current state
// is.
const (
	maxTrailHead  = 2
	maxTrailTail  = 4
	maxTrailShown = maxTrailHead + maxTrailTail
)

func trailEmoji(kind string) string {
	switch kind {
	case "fired", "refired":
		return ":red_circle:"
	case "acked":
		return ":eyes:"
	case "unacked":
		return ":arrow_uturn_left:"
	case "suppressed":
		return ":mute:"
	case "unsuppressed":
		return ":speaker:"
	case "resolved":
		return ":white_check_mark:"
	case "expired":
		return ":grey_question:"
	default:
		return ":white_circle:"
	}
}

func trailVerb(kind string) string {
	switch kind {
	case "fired":
		return "fired"
	case "refired":
		return "re-fired"
	case "acked":
		return "acked"
	case "unacked":
		return "un-acked"
	case "suppressed":
		return "silenced"
	case "unsuppressed":
		return "silence ended"
	case "resolved":
		return "resolved"
	case "expired":
		return "expired"
	default:
		return kind
	}
}

// trailActor attributes a transition a human caused. ACTOR, NEVER SUBJECT: a
// person appears here as metadata about an action, never as the topic (ADR 0013).
func trailActor(e domain.TrailEntry) string {
	if e.Actor == "" {
		return ""
	}
	return " by " + code(e.Actor)
}

// trailSpan closes the receipt with the total, which is the number somebody
// writing the post-mortem copies out.
func trailSpan(v *domain.NotificationView, state CardState) string {
	if !state.IsTerminal() {
		return ""
	}
	start, end := groupStart(v), v.Group.LastActivityAt
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return ""
	}
	return "total " + humanDuration(end.Sub(start))
}

// actionsBlock renders at most four elements: up to three buttons and one
// overflow (§H.7). Exactly one button may be primary and none may be danger
// inline (S10) — destructive things live behind a confirm, not one mis-tap away.
func (r *Renderer) actionsBlock(v *domain.NotificationView, state CardState, nonce string) (Block, bool) {
	elements := make([]Action, 0, 4)

	if !state.IsTerminal() {
		primaryUsed := false
		for _, a := range v.Actions {
			if len(elements) >= 3 {
				break
			}
			if a.ID == "" || a.Label == "" {
				continue
			}
			btn := Action{
				Type:     ElementButton,
				Text:     plain(truncateRunes(a.Label, maxButtonText)),
				ActionID: a.ID,
			}
			switch {
			case a.URL != "":
				// ⛔ A LINK BUTTON WHOSE URL IS UNUSABLE IS DROPPED, NOT EMITTED.
				// `Runbook` carries an upstream annotation verbatim; emitting a
				// relative or non-http(s) value would fail V10 and kill the entire
				// card rather than lose one button. See safeURL.
				btn.URL = safeURL(a.URL)
				if btn.URL == "" {
					continue
				}
			default:
				btn.Value = a.Value
			}
			// V13/S10: one primary, never an inline danger.
			if a.Style == "primary" && !primaryUsed {
				btn.Style = "primary"
				primaryUsed = true
			}
			elements = append(elements, btn)
		}
	}

	if of, ok := overflowMenu(v); ok {
		elements = append(elements, of)
	}
	if len(elements) == 0 {
		return Block{}, false
	}
	return actionsBlock(blockID("actions", nonce), elements...), true
}

// overflowMenu is built from the view's links, not from its actions: every entry
// is a place to look, never a thing to change. Each option still delivers an
// interaction payload that the handler must ack with a 200 (S9), which is why the
// action id is in the oto.noop.* family.
func overflowMenu(v *domain.NotificationView) (Action, bool) {
	// FIVE, because Slack's overflow element is documented as "an array of up to
	// five option objects". A sixth is `invalid_blocks` and takes the card with it.
	opts := make([]OverflowOption, 0, maxOverflowOptions)
	addOpt := func(label, url, value string) {
		// `Prometheus` is Alertmanager's `generatorURL` and `Timeline` is oto's own;
		// the first is upstream-controlled and unvalidated, so an entry whose URL
		// does not survive safeURL is DROPPED rather than allowed to fail V10 and
		// take the whole delivery with it.
		if url != "" {
			if url = safeURL(url); url == "" {
				return
			}
		}
		if len(opts) >= maxOverflowOptions || (url == "" && value == "") {
			return
		}
		opts = append(opts, OverflowOption{
			// An overflow option's label is a plain_text object capped at 75 — the
			// OPTION limit, which happens to equal the button's. They are different
			// rules that agree, not one rule.
			Text:  *plain(truncateRunes(label, maxOptionText)),
			URL:   url,
			Value: value,
		})
	}

	// ⛔ THE OVERFLOW DOES NOT SHRINK WHEN THE CARD GOES TERMINAL. The BUTTONS do —
	// Acknowledge is meaningless on something that is over — but every one of these
	// is a PLACE TO LOOK, and the moment a reader most needs to look is after it
	// ended. The old code dropped Prometheus, Alertmanager and the label list on
	// resolve, which made the only surviving record of the incident the least
	// navigable version of it (§H.4, amended).
	addOpt(":blue_book: Show timeline", v.Links.Timeline, "")
	addOpt(":chart_with_upwards_trend: Open in Prometheus", v.Links.Prometheus, "")
	addOpt(":bell: Open in Alertmanager", v.Links.Alertmanager, "")
	if v.Rule != nil {
		addOpt(":scroll: Rule history", v.Links.Group, "")
	}
	if v.Group.ID != "" {
		addOpt(":label: Show all labels", "", "labels|"+v.Group.ID)
	}

	if len(opts) == 0 {
		return Action{}, false
	}
	return Action{Type: ElementOverflow, ActionID: "oto.more", Options: opts}, true
}

// footerBlock is the provenance line: which group, which receiver, why this
// delivery happened, and when the card was last touched. It is what makes an
// update-in-place card trustworthy — the reader can see it is current.
func (r *Renderer) footerBlock(v *domain.NotificationView, o domain.RenderOptions, state CardState, now time.Time, nonce string) Block {
	parts := []string{"oto"}
	if k := v.Group.GroupKey; k != "" {
		parts = append(parts, code(shortKey(k)))
	}
	if v.Group.Receiver != "" {
		parts = append(parts, "receiver "+code(v.Group.Receiver))
	}
	if phrase := reasonPhrase(v.Reason); phrase != "" {
		parts = append(parts, "_"+phrase+"_")
	}
	if v.Group.Generation > 1 {
		parts = append(parts, "generation "+strconv.Itoa(v.Group.Generation))
	}
	if state == CardAcknowledged && v.Case != nil && v.Case.AckedAt != nil {
		parts = append(parts, "acked "+slackDate(*v.Case.AckedAt))
	}
	// The `Started` field is UPSTREAM's clock. When oto heard about it materially
	// later — `group_wait`, a retry, a backed-up queue — that gap is itself a fact
	// worth having, and the footer is where oto's own provenance belongs. Under a
	// minute it is noise and is dropped (S11).
	if lag := observationLag(v); lag >= time.Minute {
		parts = append(parts, "oto first saw it "+slackDate(v.Group.FirstSeenAt)+
			" ("+humanDuration(lag)+" later)")
	}
	parts = append(parts, "updated "+slackDate(now))
	if o.Continued {
		// §H.9: this card replaces one that is gone or unreachable. Saying so is
		// what stops a recovery reading as a second incident.
		parts = append(parts, "_continued from an earlier card_")
	}

	text := strings.Join(parts, "  ·  ")
	return contextBlock(blockID("footer", nonce), Text{Type: TypeMrkdwn, Text: truncateField(text, v.Links.Group)})
}

// leadEmoji is the emoji at the head of the title and of the top-level text.
//
// §H.2 says the leading emoji carries SEVERITY; §H.4's state table gives the card
// a per-state leading emoji. Both are honoured by scoping them: while the card is
// firing — the only time "how bad is this?" is the question being asked — the
// emoji is the severity. Once the card has moved to any other state, the question
// has become "what happened to it?", so the state emoji leads and §H.4's table is
// reproduced exactly. Severity is never lost: the Severity field carries its own
// emoji in every state.
func leadEmoji(v *domain.NotificationView, state CardState) string {
	if state == CardFiring {
		return SeverityEmoji(v.Group.Severity)
	}
	return state.Emoji()
}

// statusValue is where the strikethrough trick lives (§H.4). It is one field, and
// it tells a reader who saw the card an hour ago exactly what changed.
func statusValue(v *domain.NotificationView, state CardState) string {
	current := state.Label()

	switch state {
	case CardAcknowledged:
		who, attributed := ackedBy(v)
		current += by(who, " automatically", attributed)
	case CardSuppressed:
		current += silencedBy(v)
		if v.Case != nil && v.Case.EndedAt != nil {
			current += " until " + slackDate(*v.Case.EndedAt)
		}
	case CardExpired:
		current += " — oto stopped hearing about this"
	case CardFiring, CardResolved:
	}

	var previous string
	if v.Previous != nil {
		previous = previousLabel(v.Previous)
	}
	return state.Emoji() + " " + strike(previous, current)
}

func previousLabel(p *domain.PreviousState) string {
	switch {
	case p.State == "" && p.AckState == "":
		return ""
	case p.AckState == "acked" && p.State == "firing":
		return "Acked"
	case p.State == "firing":
		return "Firing"
	case p.State == "suppressed":
		return "Silenced"
	case p.State == "resolved":
		return "Resolved"
	case p.State == "expired":
		return "Expired"
	default:
		return ""
	}
}

func severityValue(v *domain.NotificationView, o domain.RenderOptions) string {
	sev := v.Group.Severity
	if sev == "" {
		return ""
	}
	if o.ShowFieldEmoji {
		return SeverityEmoji(sev) + " " + escape(sev)
	}
	return escape(sev)
}

func durationLabel(state CardState) string {
	switch state {
	case CardResolved:
		return "Duration"
	case CardExpired:
		return "Last seen"
	case CardSuppressed:
		return "Silenced for"
	case CardAcknowledged, CardFiring:
		return "Firing for"
	default:
		return "Firing for"
	}
}

// groupStart is what the ROOT CARD means by "Started": the upstream start of the
// whole generation, falling back to oto's first sighting when upstream gave none.
func groupStart(v *domain.NotificationView) time.Time {
	if !v.Group.StartedAt.IsZero() {
		return v.Group.StartedAt
	}
	return v.Group.FirstSeenAt
}

// durationValue is "Firing for", and it is A FACT ABOUT THE GROUP.
//
// ⛔ IT IS NOT THE TRIGGERING ALERT'S CASE DURATION, WHICH IS WHAT IT USED
// TO BE. The root card is about a generation; the case in the view is
// whichever alert's episode happened to mint this notification, and for a `fired`
// intent that episode is milliseconds old. The first live run therefore rendered
// "Firing for: under a second" on a group that had been firing for eighty
// seconds — a card that misstates the length of an outage is worse than one that
// omits it, because an operator triages on that number.
//
// The group's own clock is used in every state, and it starts at UPSTREAM's
// `startsAt` so the number agrees with the `Started` field directly above it.
func durationValue(v *domain.NotificationView, state CardState, now time.Time) string {
	if state == CardExpired {
		return slackDate(v.Group.LastActivityAt)
	}
	start := groupStart(v)
	if start.IsZero() {
		return ""
	}
	end := now
	if state.IsTerminal() && !v.Group.LastActivityAt.IsZero() {
		end = v.Group.LastActivityAt
	}
	if end.Before(start) {
		return ""
	}
	return humanDuration(end.Sub(start))
}

// observationLag is how long after the signal started oto first heard about it.
// A negative or unknown gap is zero: a clock that ran backwards is measured
// elsewhere (C12) and is not something to print on a card.
func observationLag(v *domain.NotificationView) time.Duration {
	if v.Group.StartedAt.IsZero() || v.Group.FirstSeenAt.IsZero() {
		return 0
	}
	if !v.Group.FirstSeenAt.After(v.Group.StartedAt) {
		return 0
	}
	return v.Group.FirstSeenAt.Sub(v.Group.StartedAt)
}

// clusterChip is the code-formatted cluster beside the title (§H.3).
func clusterChip(v *domain.NotificationView) string {
	return firstNonEmpty(v.Group.ClusterKey, v.Group.GroupLabels["cluster"],
		focusField(v, func(a domain.AlertView) string { return a.ClusterKey }),
		focusField(v, func(a domain.AlertView) string { return a.Labels["cluster"] }))
}

// labelOf reads a label from the group labels first, then from the alert the
// card is about. Group labels are only the subset Alertmanager grouped by, so a
// renderer that reads nothing else silently drops every other label a card wants.
func labelOf(v *domain.NotificationView, name string) string {
	if s := strings.TrimSpace(v.Group.GroupLabels[name]); s != "" {
		return s
	}
	return focusField(v, func(a domain.AlertView) string { return a.Labels[name] })
}

// rootText is the complete sentence that becomes the push notification, the
// sidebar preview, the search snippet and the screen-reader content (S5).
//
// It is the highest-leverage string in the product and it is written by hand
// here, not derived from the blocks. It never contains an <!date> token, which
// does not render in a push notification, and it is capped at 300 characters
// because a longer one is truncated by the operating system anyway.
func rootText(v *domain.NotificationView, state CardState) string {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	if title == "" {
		title = "Alert group"
	}

	var b strings.Builder
	b.WriteString(leadEmoji(v, state))
	b.WriteString(" [")
	b.WriteString(state.Banner())
	b.WriteString("] ")
	b.WriteString(title)

	if s := oneLine(annotation(v, "summary", "description", "message")); s != "" {
		b.WriteString(" — ")
		// Cut on a clause boundary, and let endSentence decide the terminator: the
		// old code appended "." to a string that already ended in "…", which is how
		// "…no real service…." reached a real Slack channel.
		b.WriteString(endSentence(truncateClause(s, 120)))
	} else if v.Group.TotalCount > 0 {
		b.WriteString(" — ")
		b.WriteString(endSentence(countPhrase(v)))
	} else {
		b.WriteString(".")
	}

	facts := make([]string, 0, 3)
	if v.Group.Severity != "" {
		facts = append(facts, "Severity "+v.Group.Severity)
	}
	if team := labelOf(v, "team"); team != "" {
		facts = append(facts, "team "+team)
	}
	facts = append(facts, stateClause(v, state))
	b.WriteString(" ")
	b.WriteString(endSentence(joinNonEmpty(", ", facts...)))

	sentence := oneLine(b.String())

	// The runbook is appended only if the WHOLE of it fits. A URL cut at a
	// boundary is still a broken URL, and a broken link is worse than no link: it
	// looks clickable and is not.
	//
	// ⛔⛔ AND ONLY IF IT IS A URL AT ALL. This is the one place a raw upstream
	// annotation reached the top-level `text` UNESCAPED — every other upstream
	// string on the card goes through `escape` or `code`, and a URL cannot,
	// because `<`, `>` and `&` are legal in one. So `runbook_url: "<!channel>"` on
	// any alert put a channel-wide ping in the push notification of every person
	// in the room, and `runbook_url: "<https://evil.example|Click here>"` put an
	// attacker-labelled link there. The top-level text is what a locked phone
	// shows and what a screen reader announces (S5); it is the last string in the
	// product that should accept unvalidated input.
	if u := safeURL(v.Links.Runbook); u != "" {
		tail := " Runbook: " + u
		if utf8.RuneCountInString(sentence)+utf8.RuneCountInString(tail) <= otoTopLevelText {
			return sentence + tail
		}
	}
	return truncateClause(sentence, otoTopLevelText)
}

func stateClause(v *domain.NotificationView, state CardState) string {
	switch state {
	case CardResolved:
		return "resolved at " + plainClock(v.Group.LastActivityAt)
	case CardExpired:
		return "last seen at " + plainClock(v.Group.LastActivityAt)
	case CardSuppressed:
		return "silenced since " + plainClock(v.Group.LastActivityAt)
	case CardAcknowledged:
		return "acknowledged, firing since " + plainClock(groupStart(v))
	case CardFiring:
		return "firing since " + plainClock(groupStart(v))
	default:
		return "firing since " + plainClock(groupStart(v))
	}
}

func countPhrase(v *domain.NotificationView) string {
	switch {
	case v.Group.FiringCount > 0 && v.Group.TotalCount > v.Group.FiringCount:
		return strconv.Itoa(v.Group.FiringCount) + " of " + strconv.Itoa(v.Group.TotalCount) + " instances firing"
	case v.Group.TotalCount > 0:
		return plural(v.Group.TotalCount, "instance", "instances")
	default:
		return ""
	}
}

// shortFallback is the attachment's own legacy fallback string.
func shortFallback(v *domain.NotificationView, state CardState) string {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	out := "[" + state.Banner() + "] " + title
	if v.Group.ClusterKey != "" {
		out += " on " + v.Group.ClusterKey
	}
	return truncateRunes(out, 200)
}

func rootMetadata(v *domain.NotificationView) *Metadata {
	if v.Group.ID == "" {
		return nil
	}
	return &Metadata{
		EventType: "oto_alert_group",
		EventPayload: map[string]any{
			"group_id":   v.Group.ID,
			"generation": v.Group.Generation,
			"reason":     v.Reason,
		},
	}
}

// capBlocks enforces the 50-block ceiling by dropping from the middle, keeping
// the title and the footer. It is a last line of defence: the renderer's own
// budget is seven blocks, so reaching this means a bug, and Validate will still
// refuse anything that got past it.
func capBlocks(blocks []Block) []Block {
	if len(blocks) <= maxBlocks {
		return blocks
	}
	out := make([]Block, 0, maxBlocks)
	out = append(out, blocks[:maxBlocks-1]...)
	out = append(out, blocks[len(blocks)-1])
	return out
}
