package slack

import (
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// renderRoot builds the card that is posted once per AlertGroup generation and
// then amended in place for its entire life (§H.3, ADR 0008).
//
// The layout is deliberately calm and dense. Seven blocks against a ceiling of
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
	if b, ok := r.ruleBlock(v, state, nonce); ok {
		blocks = append(blocks, b)
	}
	if b, ok := r.actionsBlock(v, state, nonce); ok {
		blocks = append(blocks, b)
	}
	blocks = append(blocks, r.footerBlock(v, state, now, nonce))

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
	if cluster := v.Group.ClusterKey; cluster != "" {
		head += "  ·  " + code(cluster)
	}
	if state == CardStorm {
		head += "  ·  " + code("storm")
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
	add("Started", slackDate(v.Group.FirstSeenAt))
	add(durationLabel(state), durationValue(v, state, now))
	if n, ok := flappingCount(v); ok {
		add("Flapping", ":arrows_counterclockwise: "+plural(n, "transition", "transitions"))
	}
	add("Team", escape(v.Group.GroupLabels["team"]))

	return fieldsBlock(blockID("fields", nonce), fields)
}

// membersBlock lists the affected instances, capped at MaxInstances with an
// explicit "and N more".
//
// It is dropped once the card is terminal and replaced by a single count in storm
// mode: a list of 214 instances nobody will read is not information (§H.4, S11).
func (r *Renderer) membersBlock(
	v *domain.NotificationView, o domain.RenderOptions, state CardState, nonce string,
) (Block, bool) {
	if state.IsTerminal() {
		return Block{}, false
	}
	if state == CardStorm {
		text := "*Affected instances*\n" + plural(v.Group.TotalCount, "alert", "alerts") +
			" in this group. " + link(v.Links.Group, "See them all in oto") + "."
		return sectionBlock(blockID("members", nonce), truncateSection(text, v.Links.Group)), true
	}
	if len(v.Alerts) <= 1 {
		// One instance is zero information: the title already named it (S11).
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

// ruleBlock shows what the rule said at the moment this occurrence fired. It is
// oto's defensible differentiator, and it costs one quiet context line.
func (r *Renderer) ruleBlock(v *domain.NotificationView, state CardState, nonce string) (Block, bool) {
	if state.IsTerminal() || state == CardStorm || v.Rule == nil {
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
		text += "   :scroll: " + link(v.Links.Timeline, "the rule changed since the last occurrence")
	}
	return contextBlock(blockID("rule", nonce), Text{Type: TypeMrkdwn, Text: truncateField(text, v.Links.Group)}), true
}

// actionsBlock renders at most four elements: up to three buttons and one
// overflow (§H.7). Exactly one button may be primary and none may be danger
// inline (S10) — destructive things live behind a confirm, not one mis-tap away.
func (r *Renderer) actionsBlock(v *domain.NotificationView, state CardState, nonce string) (Block, bool) {
	elements := make([]Action, 0, 4)

	if !state.IsTerminal() && state != CardStorm {
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
				btn.URL = truncateURL(a.URL)
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

	if of, ok := overflowMenu(v, state); ok {
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
func overflowMenu(v *domain.NotificationView, state CardState) (Action, bool) {
	opts := make([]OverflowOption, 0, 5)
	addOpt := func(label, url, value string) {
		if len(opts) >= 5 || (url == "" && value == "") {
			return
		}
		opts = append(opts, OverflowOption{
			Text:  *plain(truncateRunes(label, maxButtonText)),
			URL:   truncateURL(url),
			Value: value,
		})
	}

	addOpt(":blue_book: Show timeline", v.Links.Timeline, "")
	if !state.IsTerminal() {
		addOpt(":chart_with_upwards_trend: Open in Prometheus", v.Links.Prometheus, "")
		addOpt(":bell: Open in Alertmanager", v.Links.Alertmanager, "")
	}
	if v.Rule != nil {
		addOpt(":scroll: Rule history", v.Links.Group, "")
	}
	if !state.IsTerminal() && v.Group.ID != "" {
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
func (r *Renderer) footerBlock(v *domain.NotificationView, state CardState, now time.Time, nonce string) Block {
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
	if state == CardAcknowledged && v.Occurrence != nil && v.Occurrence.AckedAt != nil {
		parts = append(parts, "acked "+slackDate(*v.Occurrence.AckedAt))
	}
	parts = append(parts, "updated "+slackDate(now))

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
		if who := actorLabel(v); who != "" {
			current += " by " + who
		}
	case CardSuppressed:
		if who := actorLabel(v); who != "" {
			current += " by " + who
		}
		if v.Occurrence != nil && v.Occurrence.EndedAt != nil {
			current += " until " + slackDate(*v.Occurrence.EndedAt)
		}
	case CardExpired:
		current += " — oto stopped hearing about this"
	case CardStorm:
		if v.StormCount > 0 {
			current = "Storm — " + plural(v.StormCount, "alert", "alerts") + " in this group"
		}
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
	case CardAcknowledged, CardFiring, CardStorm:
		return "Firing for"
	default:
		return "Firing for"
	}
}

func durationValue(v *domain.NotificationView, state CardState, now time.Time) string {
	if state == CardExpired {
		return slackDate(v.Group.LastActivityAt)
	}
	if v.Occurrence != nil && v.Occurrence.Duration > 0 {
		return humanDuration(v.Occurrence.Duration)
	}
	start := v.Group.FirstSeenAt
	if v.Occurrence != nil && !v.Occurrence.StartedAt.IsZero() {
		start = v.Occurrence.StartedAt
	}
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
		b.WriteString(truncateRunes(s, 120))
	} else if v.Group.TotalCount > 0 {
		b.WriteString(" — ")
		b.WriteString(countPhrase(v))
	}
	b.WriteString(".")

	facts := make([]string, 0, 3)
	if v.Group.Severity != "" {
		facts = append(facts, "Severity "+v.Group.Severity)
	}
	if team := v.Group.GroupLabels["team"]; team != "" {
		facts = append(facts, "team "+team)
	}
	facts = append(facts, stateClause(v, state))
	b.WriteString(" ")
	b.WriteString(joinNonEmpty(", ", facts...))
	b.WriteString(".")

	if v.Links.Runbook != "" {
		b.WriteString(" Runbook: ")
		b.WriteString(v.Links.Runbook)
	}

	return truncateRunes(strings.Join(strings.Fields(b.String()), " "), otoTopLevelText)
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
		return "acknowledged, firing since " + plainClock(v.Group.FirstSeenAt)
	case CardStorm:
		return "storm damping on since " + plainClock(v.Group.LastActivityAt)
	case CardFiring:
		return "firing since " + plainClock(v.Group.FirstSeenAt)
	default:
		return "firing since " + plainClock(v.Group.FirstSeenAt)
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
