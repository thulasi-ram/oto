package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// ADR 0052 §6: AN INCIDENT MAY BE A CONVERSATION (git-bug bf5fc7e).
//
// When a Correlator says its Incidents are conversations, an Incident gets a Slack
// thread of its own, and three messages exist that did not before:
//
//   - the ROOT CARD — the Incident as it is now: its member Cases, active or quiet.
//     It is posted by whichever fact reaches the channel first and amended by every
//     fact after, Incident fact or Case fact alike (`ViewRequest.IncidentRoot`);
//   - an INCIDENT FACT REPLY under it — a Case joined or left, quiet, active again;
//   - the POINTER — the one "now part of Incident #N" reply posted into each member
//     Case's own thread, so a reader following that thread learns where it went.
//
// A Case fact posted in the Incident's thread is drawn by `renderReply`, as it would
// be in its own thread, with the Case named in front (`incidentCasePrefix`).
//
// ⭐ IT CARRIES FACTS ABOUT SIGNALS AND NOTHING ABOUT THE RESPONSE. Active and quiet
// are read off the member Cases (ADR 0052 §3); there is no status, lead or severity
// here because oto holds none. "Quiet" is every member Case closed — never "over".
//
// ⛔ NO ACTIONS. Every action on a card acts on a signal; the Incident card is about
// the story, and a button on it would have to pick one of its Cases. Each member
// links to its own Case, which is where the buttons are.
//
// ⚠️ NO OUTBOUND LINK, BECAUSE THERE IS NONE TO DRAW. ADR 0052 §5's outbound mapping
// — `(destination, external incident id)` — is not stored by any table yet, so the
// card has no URL into the incident tool to offer. When the mapping lands, its link
// belongs in the context line beside oto's own, and nowhere a template cannot see.

// incidentEmoji leads every Incident surface. A jigsaw because an Incident is pieces
// drawn into one picture — and because it is not any of §H.2's state emoji, which a
// reader who has learned the palette would read as a signal's state.
const incidentEmoji = ":jigsaw:"

// incidentQuietEmoji marks an Incident gone quiet. NOT CardResolved's check mark:
// quiet is every member Case closed, and whether the response is over is the
// incident tool's to say (ADR 0052 §3) — a green tick would say it for them. A
// neutral circle, as the card's bar is the neutral colour and not the resolved
// green. A closed MEMBER Case still wears CardResolved: that Case did close.
const incidentQuietEmoji = ":white_circle:"

// incidentStateActive is the derived state string `IncidentView.State` carries.
const incidentStateActive = "active"

// The Incident Reasons, duplicated here as the Case Reasons are in reply.go and for
// the same reason: channels/domain does not depend on the notification enum.
const (
	reasonDrawn       = "drawn"
	reasonCaseAdded   = "case_added"
	reasonCaseRemoved = "case_removed"
	reasonQuiet       = "quiet"
	reasonActiveAgain = "active_again"
)

// incidentNonce is `renderNonce` for a view with no group: the Incident's identity,
// the membership shape the card shows, the mode and the claim time. Two renders of
// one fact hash the same; a card whose members or state moved does not.
func incidentNonce(v *domain.NotificationView, o domain.RenderOptions) string {
	iv := v.Incident
	h := sha256.New()
	for _, p := range []string{
		iv.ID, iv.State, iv.PointsFrom, v.Reason, string(o.Mode), strconv.FormatBool(o.Continued),
		strconv.FormatInt(v.RenderedAt.UTC().Unix(), 10), strconv.Itoa(len(iv.Members)),
	} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	for _, m := range iv.Members {
		h.Write([]byte(m.CaseID + m.CaseState))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// renderIncident draws whichever of the three Incident messages the view and the
// mode call for.
func (r *Renderer) renderIncident(v *domain.NotificationView, o domain.RenderOptions) (Payload, string, string) {
	switch {
	case v.Incident.PointsFrom != "":
		// In the CASE's thread, whatever the mode: a pointer that a lost root turned
		// into `post_root` is still a pointer, and the Incident's card in a Case's
		// thread would read as a second story.
		return r.renderIncidentPointer(v, o)
	case o.Mode == domain.ModeThreadReply:
		return r.renderIncidentReply(v, o)
	default:
		p, text := r.renderIncidentRoot(v, o)
		return p, text, text
	}
}

// incidentCounts is the current membership in two numbers: how many Cases are in
// the Incident now, and how many of those are open.
func incidentCounts(iv domain.IncidentView) (current, open int) {
	for _, m := range iv.Members {
		if !m.RemovedAt.IsZero() {
			continue
		}
		current++
		if m.CaseState == "open" {
			open++
		}
	}
	return current, open
}

func incidentActive(iv domain.IncidentView) bool { return iv.State == incidentStateActive }

// incidentColour answers §H.1 S4's question — "do I need to act?" — for the story:
// the firing colour while a member Case is open, and the neutral bar once every one
// has closed. ⛔ NOT the resolved green: quiet is a fact about the signals, and
// whether the response is over is the incident tool's to say (ADR 0052 §3).
func incidentColour(iv domain.IncidentView) string {
	if incidentActive(iv) {
		return CardFiring.Colour()
	}
	return neutralColour()
}

func incidentName(iv domain.IncidentView) string {
	return "Incident #" + strconv.FormatInt(iv.Number, 10)
}

// incidentAuthorPhrase is who drew it, as a clause. A Correlator is named by kind
// only: the view carries its id, and a UUID is a string nobody can act on.
func incidentAuthorPhrase(a domain.IncidentAuthorView) string {
	if a.CorrelatorID != "" {
		return "a Correlator"
	}
	if strings.TrimSpace(a.Label) != "" {
		return code(a.Label)
	}
	return "a person"
}

// renderIncidentRoot is the Incident's card: the root of its conversation.
func (r *Renderer) renderIncidentRoot(v *domain.NotificationView, o domain.RenderOptions) (Payload, string) {
	iv := *v.Incident
	nonce := incidentNonce(v, o)
	now := r.renderedAt(v)
	current, open := incidentCounts(iv)

	blocks := make([]Block, 0, 4)
	blocks = append(blocks, sectionBlock(blockID("incident", nonce),
		truncateSection(incidentHead(iv, current, open), iv.Link)))
	blocks = append(blocks, fieldsBlock(blockID("incidentfacts", nonce), incidentFields(iv, current, open)))
	if members := incidentMembers(iv, o); members != "" {
		blocks = append(blocks, sectionBlock(blockID("incidentmembers", nonce),
			truncateSection(members, iv.Link)))
	}
	blocks = append(blocks, contextBlock(blockID("incidentfooter", nonce),
		Text{Type: TypeMrkdwn, Text: truncateField(incidentFooter(iv, o, now), "")}))

	text := incidentText(iv, current, open, now)
	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Attachments: []Attachment{{
			Color:    incidentColour(iv),
			Fallback: truncateRunes(text, 200),
			Blocks:   blocks,
		}},
	}, text
}

// incidentHead is the title — a section with the bold deep link (S1) — and the one
// line that says what the card is and what the thread under it is for.
func incidentHead(iv domain.IncidentView, current, open int) string {
	var b strings.Builder
	b.WriteString(incidentEmoji + " *" + link(safeURL(iv.Link), incidentName(iv)) + "* — ")
	if incidentActive(iv) {
		b.WriteString(CardFiring.Emoji() + " *Active*, " + plural(open, "case open", "cases open"))
	} else {
		b.WriteString(incidentQuietEmoji + " *Quiet*, every member case has closed")
	}
	b.WriteString("\n_One story drawn over " + plural(current, "case", "cases") +
		". Later updates about its cases are posted in this thread._")
	return b.String()
}

// incidentFields lays the Incident out as facts. S11: a zero is not rendered —
// a quiet Incident has no `Open` field rather than an `Open: 0`.
func incidentFields(iv domain.IncidentView, current, open int) []Text {
	fields := make([]Text, 0, 4)
	add := func(label, value string) {
		if len(fields) >= maxFields || strings.TrimSpace(value) == "" {
			return
		}
		fields = append(fields, Text{Type: TypeMrkdwn, Text: truncateField("*"+label+"*\n"+value, "")})
	}
	add("Cases", strconv.Itoa(current))
	if open > 0 {
		add("Open", strconv.Itoa(open))
	}
	add("Drawn", slackDateTime(iv.DrawnAt))
	add("Drawn by", incidentAuthorPhrase(iv.DrawnBy))
	return fields
}

// incidentMembers lists the CURRENT members, each linked to its own Case, capped at
// the channel's instance budget with "… and N more" (§H.3's rule for instances).
// Removed and moved members are history the Incident's page keeps; the card shows
// what the story is made of now.
func incidentMembers(iv domain.IncidentView, o domain.RenderOptions) string {
	limit := o.MaxInstances
	if limit <= 0 {
		limit = defaultMaxInstances
	}
	lines := make([]string, 0, limit+1)
	more := 0
	for _, m := range iv.Members {
		if !m.RemovedAt.IsZero() {
			continue
		}
		if len(lines) >= limit {
			more++
			continue
		}
		emoji := CardResolved.Emoji()
		if m.CaseState == "open" {
			emoji = CardFiring.Emoji()
		}
		line := "• " + emoji + " " + link(safeURL(m.Link), "Case #"+strconv.FormatInt(m.CaseNumber, 10))
		if name := strings.TrimSpace(m.AlertName); name != "" {
			line += " " + code(name)
		}
		lines = append(lines, line)
	}
	if more > 0 {
		lines = append(lines, "… and "+strconv.Itoa(more)+" more")
	}
	return strings.Join(lines, "\n")
}

// incidentFooter is the provenance line, in the root card's words: what this is and
// when it was last true.
func incidentFooter(iv domain.IncidentView, o domain.RenderOptions, now time.Time) string {
	parts := []string{"oto", "_" + incidentName(iv) + "_", "updated " + slackDate(now)}
	if o.Continued {
		parts = append(parts, continuedMarker)
	}
	return strings.Join(parts, "  ·  ")
}

// incidentText is the card's top-level text (S5): one sentence, no `<!date>` token,
// bounded at 300.
func incidentText(iv domain.IncidentView, current, open int, now time.Time) string {
	var b strings.Builder
	b.WriteString(incidentEmoji + " [INCIDENT] " + incidentName(iv))
	if incidentActive(iv) {
		b.WriteString(" is active: " + plural(open, "case", "cases") + " open of " + strconv.Itoa(current) +
			", drawn " + plainMoment(iv.DrawnAt, now))
	} else {
		b.WriteString(" is quiet: all " + plural(current, "case", "cases") + " in it have closed")
	}
	return truncateClause(oneLine(endSentence(b.String())), otoTopLevelText)
}

// renderIncidentReply is one Incident fact as a line in the Incident's thread.
//
// ⚠️ IT DOES NOT NAME WHICH CASE JOINED OR LEFT, ON PURPOSE. The fact names the
// Incident and its occasion and nothing narrower (`notifications_subject_ck`'s
// incident arm), and the view is the Incident as it is at claim time — so picking
// "the newest member" would name the wrong Case whenever two joined before this
// was sent. What is true at claim time is the count, and the card above lists the
// members.
func (r *Renderer) renderIncidentReply(v *domain.NotificationView, o domain.RenderOptions) (Payload, string, string) {
	iv := *v.Incident
	nonce := incidentNonce(v, o)
	current, open := incidentCounts(iv)
	now := plural(current, "case", "cases") + ", " + strconv.Itoa(open) + " open"

	var body, sentence string
	switch v.Reason {
	case reasonCaseAdded:
		body = ":heavy_plus_sign: *A case joined* — now " + now
		sentence = "A case joined " + incidentName(iv) + "; it now has " + now
	case reasonCaseRemoved:
		body = ":heavy_minus_sign: *A case left* — now " + now
		sentence = "A case left " + incidentName(iv) + "; it now has " + now
	case reasonQuiet:
		body = incidentQuietEmoji + " *Quiet* — every member case has closed. _Whether the " +
			"response is over is for the incident tool to say._"
		sentence = incidentName(iv) + " is quiet: every member case has closed"
	case reasonActiveAgain:
		body = CardFiring.Emoji() + " *Active again* — " + plural(open, "case open", "cases open")
		sentence = incidentName(iv) + " is active again: " + plural(open, "case", "cases") + " open"
	case reasonDrawn:
		// `incidentModes` never gives `drawn` a reply — the card says it all — so this
		// arm is for a preview or a future mode, and it still says something true.
		body = incidentEmoji + " *" + escape(incidentName(iv)) + " drawn* — " + now
		sentence = incidentName(iv) + " was drawn; it has " + now
	default:
		// A fact this renderer has no words for yet. The count is still true.
		body = incidentEmoji + " *" + escape(incidentName(iv)) + "* — " + now
		sentence = incidentName(iv) + ": " + now
	}
	text := truncateClause(oneLine(endSentence(incidentEmoji+" "+sentence)), otoTopLevelText)

	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Attachments: []Attachment{{
			Color:    incidentColour(iv),
			Fallback: truncateRunes(text, 200),
			Blocks: []Block{sectionBlock(blockID("incidentreply", nonce),
				truncateSection(body, iv.Link))},
		}},
	}, text, text
}

// renderIncidentPointer is the one reply posted into a member Case's OWN thread
// when that Case's later facts start going to the Incident's (ADR 0052 §6).
//
// ⭐ IT IS THE ONLY TRACE IN THE CASE'S THREAD THAT ANYTHING CHANGED, so it says the
// two things a reader of that thread needs: where the updates went, and that the
// silence here from now on is not the alert going quiet. The link is in the body,
// where a reader can click it; the top-level text is a sentence (S5).
//
// Its bar is the neutral one, like the digest's: it is not a reading of any state,
// the Case's or the Incident's — it says where to look.
func (r *Renderer) renderIncidentPointer(v *domain.NotificationView, o domain.RenderOptions) (Payload, string, string) {
	iv := *v.Incident
	nonce := incidentNonce(v, o)

	body := ":arrow_right: *Now part of " + link(safeURL(iv.Link), incidentName(iv)) + "* — later updates " +
		"about this case are posted in that Incident's thread, not here."
	text := truncateClause(oneLine(endSentence(":arrow_right: This case is now part of "+
		incidentName(iv)+"; later updates about it are posted in that Incident's thread")), otoTopLevelText)

	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Attachments: []Attachment{{
			Color:    neutralColour(),
			Fallback: truncateRunes(text, 200),
			Blocks: []Block{sectionBlock(blockID("incidentpointer", nonce),
				truncateSection(body, iv.Link))},
		}},
	}, text, text
}

// incidentCasePrefix names the Case a reply in an Incident's thread is about (ADR
// 0052 §6). In the Case's own thread the thread IS the Case and the reply needs no
// subject; in the Incident's, forty Cases share one thread and an unnamed
// "Acknowledged" is about nobody. "" for every reply outside an Incident's thread,
// so those bytes do not move.
func incidentCasePrefix(v *domain.NotificationView) string {
	in := v.InIncident
	if in == nil {
		return ""
	}
	label := firstNonEmpty(v.Group.Title, v.Group.GroupLabels["alertname"], "this case")
	if in.CaseNumber > 0 {
		label = "Case #" + strconv.FormatInt(in.CaseNumber, 10)
	}
	out := "*" + link(safeURL(v.Links.Group), label) + "*"
	if in.CaseNumber > 0 && v.Group.Title != "" {
		out += " " + code(v.Group.Title)
	}
	return out + " · "
}

// incidentCaseClause is the same naming for the reply's top-level text, which has no
// link and no markup: "(case #412 in Incident #12)".
func incidentCaseClause(v *domain.NotificationView) string {
	in := v.InIncident
	if in == nil {
		return ""
	}
	incident := "Incident #" + strconv.FormatInt(in.Number, 10)
	if in.CaseNumber > 0 {
		return " (case #" + strconv.FormatInt(in.CaseNumber, 10) + " in " + incident + ")"
	}
	return " (in " + incident + ")"
}
