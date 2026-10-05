package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

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
// links to its own Case, which is where the buttons are. ⭐ The one exception is not the
// card: a PROPOSED Remedy's reply in the thread carries Approve and Decline, and they act
// on that Remedy and nothing else (`remedyActions`, git-bug ac9b492).
//
// ⭐ THE OUTBOUND LINK SITS IN THE CONTEXT LINE, BESIDE OTO'S OWN (git-bug 506ff21).
// ADR 0052 §5's outbound mapping is recorded when an incident tool echoes its own
// incident back in a 2xx response (migration 00089), and the card links it from the
// footer — the provenance line, which says where things are, not what they are. It
// is a receipt and the card reads nothing else off it: active and quiet are still
// the member Cases' alone. Incident cards take no template, so the view's
// `External` is the one place it is drawn. A tool that echoed nothing leaves the
// footer exactly as it was.

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
	reasonFinding     = "finding"
	// remedyReasonPrefix begins each of the six Remedy facts (ADR 0054 §2):
	// remedy_proposed, remedy_approved, and so on.
	remedyReasonPrefix = "remedy_"
)

// remedyEmoji marks a Remedy transition: a change to the cluster, proposed, decided or made.
// Not one of §H.2's state emoji, for the jigsaw's reason.
const remedyEmoji = ":wrench:"

// maxRemedyArgumentsRunes bounds the exact arguments quoted in a reply. The whole of them is
// on the Remedy's page; a reply that cut them says so.
const maxRemedyArgumentsRunes = 1500

// findingEmoji marks an Investigation's Finding. A magnifying glass because it is what
// somebody LOOKED at and concluded — and, like the jigsaw, not one of §H.2's state
// emoji, which a reader would take for a signal's state.
const findingEmoji = ":mag:"

// maxFindingRunes bounds a Finding quoted on a card or in a reply. The whole of it is
// on the Incident's page, which the card links; the card says what it begins with.
const maxFindingRunes = 600

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
	// A card whose Finding moved is a different card. Hashed only when there is one,
	// so every card without a Finding hashes exactly as it did before there could be.
	if f := iv.Finding; f != nil {
		h.Write([]byte("finding:" + f.InvestigationID))
		h.Write([]byte{0})
	}
	// A Remedy fact is about one transition; hashed only on those six facts.
	if rm := iv.Remedy; rm != nil {
		h.Write([]byte("remedy:" + rm.RemedyID + ":" + rm.State))
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
	if iv.Finding != nil {
		blocks = append(blocks, sectionBlock(blockID("incidentfinding", nonce),
			truncateSection(incidentFinding(*iv.Finding), iv.Link)))
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
	parts = append(parts, incidentExternal(iv)...)
	if o.Continued {
		parts = append(parts, continuedMarker)
	}
	return strings.Join(parts, "  ·  ")
}

// incidentExternal is one footer entry per external incident a tool echoed back:
// the tool's link, labelled with its own id when it gave one and with the
// destination's name when it did not. An id with no link is still worth showing —
// it is what a reader searches the tool for — so it is drawn as code.
//
// The URL is already an absolute https URL (domain.ValidExternalIncident); safeURL
// is asked anyway, because this is the renderer and mrkdwn is its problem: a `|` or
// `>` that is legal in a URL would end the link early here.
func incidentExternal(iv domain.IncidentView) []string {
	out := make([]string, 0, len(iv.External))
	for _, e := range iv.External {
		label := firstNonEmpty(e.ID, e.Destination, "external incident")
		switch u := safeURL(e.URL); {
		case u != "":
			out = append(out, ":link: "+link(u, label))
		case e.ID != "":
			out = append(out, ":link: "+code(e.ID))
		}
	}
	return out
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
	switch {
	case v.Reason == reasonCaseAdded:
		body = ":heavy_plus_sign: *A case joined* — now " + now
		sentence = "A case joined " + incidentName(iv) + "; it now has " + now
	case v.Reason == reasonCaseRemoved:
		body = ":heavy_minus_sign: *A case left* — now " + now
		sentence = "A case left " + incidentName(iv) + "; it now has " + now
	case v.Reason == reasonQuiet:
		body = incidentQuietEmoji + " *Quiet* — every member case has closed. _Whether the " +
			"response is over is for the incident tool to say._"
		sentence = incidentName(iv) + " is quiet: every member case has closed"
	case v.Reason == reasonActiveAgain:
		body = CardFiring.Emoji() + " *Active again* — " + plural(open, "case open", "cases open")
		sentence = incidentName(iv) + " is active again: " + plural(open, "case", "cases") + " open"
	case v.Reason == reasonFinding:
		// The Finding as of claim time — the newest, which is the one this fact
		// announced unless a later run has already overtaken it.
		body = findingEmoji + " *A new Finding* — " + now
		sentence = incidentName(iv) + " has a new Finding"
		if f := iv.Finding; f != nil {
			body = incidentFinding(*f)
			sentence += " by " + f.Investigator + " v" + strconv.Itoa(f.Version)
		}
	case strings.HasPrefix(v.Reason, remedyReasonPrefix):
		body, sentence = incidentRemedy(v.Reason, iv)
	case v.Reason == reasonDrawn:
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

	blocks := []Block{sectionBlock(blockID("incidentreply", nonce), truncateSection(body, iv.Link))}
	if b, ok := remedyActions(v.Reason, iv, nonce); ok {
		blocks = append(blocks, b)
	}
	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Attachments: []Attachment{{
			Color:    incidentColour(iv),
			Fallback: truncateRunes(text, 200),
			Blocks:   blocks,
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

// incidentRemedy is a Remedy fact's reply: the transition and who made it, then ⭐ THE EXACT
// COMMAND — the write Tool and its arguments, or that no configured Tool can carry it out —
// then what it is made to, how many approvals it needs and what set that, who has approved
// so far, and only then the Investigator's description of it (ADR 0054 §3).
func incidentRemedy(reason string, iv domain.IncidentView) (body, sentence string) {
	verb := strings.ReplaceAll(strings.TrimPrefix(reason, remedyReasonPrefix), "_", " ")
	sentence = "A Remedy on " + incidentName(iv) + " was " + verb
	r := iv.Remedy
	if r == nil {
		return remedyEmoji + " *Remedy " + escape(verb) + "*", sentence
	}
	var b strings.Builder
	b.WriteString(remedyEmoji + " *Remedy " + escape(verb) + "*")
	if r.ActorLabel != "" {
		b.WriteString(" by " + escape(r.ActorLabel))
		sentence += " by " + r.ActorLabel
	}
	if r.RequiredApprovals > 0 {
		b.WriteString(" — " + strconv.Itoa(len(r.Approvals)) + " of " + strconv.Itoa(r.RequiredApprovals) + " approvals")
	}
	if r.Tool != "" {
		b.WriteString("\n" + code(r.ToolServer+"__"+r.Tool))
		b.WriteString("\n```" + strings.ReplaceAll(escape(remedyArgumentsOnCard(*r)), "```", "'''") + "```")
		switch {
		case remedyArgumentsWhole(*r):
		case remedyAwaitingApproval(reason, *r):
			// ⭐ NO APPROVE BUTTON BELOW, AND THIS IS WHY: nobody approves a command from a
			// card that did not show all of it (`remedyActions`).
			b.WriteString("\n_the arguments are cut here, so it is approved on the Remedy's page, which shows them whole_")
		default:
			b.WriteString("\n_the arguments are cut here; the Remedy's page shows them whole_")
		}
	} else {
		b.WriteString("\n_" + escape(r.NoTool) + "_")
	}
	b.WriteString("\non " + escape(r.Target))
	if u := safeURL(iv.Link); u != "" {
		// Where the whole Remedy is read — every argument, its history — and decided in oto.
		b.WriteString("  ·  " + link(u, "open "+incidentName(iv)+" in oto"))
	}
	if tier := remedyTier(*r); tier != "" {
		b.WriteString("\n" + tier)
	}
	if len(r.Approvals) > 0 {
		names := make([]string, 0, len(r.Approvals))
		for _, a := range r.Approvals {
			names = append(names, escape(firstNonEmpty(a.Label, "a person")))
		}
		b.WriteString("\nApproved so far by " + strings.Join(names, ", "))
	}
	if r.FailureReason != "" {
		b.WriteString("\n*" + escape(r.FailureReason) + "*")
		if r.Detail != "" {
			b.WriteString(": " + escape(r.Detail))
		}
	} else if r.Detail != "" {
		b.WriteString("\n" + escape(r.Detail))
	}
	if text := strings.TrimSpace(r.Description); text != "" {
		b.WriteString("\n>" + strings.ReplaceAll(escape(truncateRunes(text, maxFindingRunes)), "\n", "\n>"))
	}
	return b.String(), sentence
}

// remedyArgumentsOnCard is the arguments as the reply quotes them: whole, or cut at
// maxRemedyArgumentsRunes.
func remedyArgumentsOnCard(r domain.IncidentRemedyView) string {
	return truncateRunes(r.Arguments, maxRemedyArgumentsRunes)
}

// remedyArgumentsWhole reports whether the reply quotes every byte of the arguments.
func remedyArgumentsWhole(r domain.IncidentRemedyView) bool {
	return remedyArgumentsOnCard(r) == r.Arguments
}

// remedyAwaitingApproval reports whether this reply is the proposal of a Remedy that is
// still waiting for a decision — the one reply that offers one.
func remedyAwaitingApproval(reason string, r domain.IncidentRemedyView) bool {
	return reason == remedyReasonPrefix+"proposed" && r.State == "proposed"
}

// remedyTier is how many approvals the Remedy needs and what set that number, in the
// approval screen's words (git-bug eb4f21b): a named rule, no rule, an unparseable command,
// or the risk model raising it. "" for a Remedy that needs none recorded.
func remedyTier(r domain.IncidentRemedyView) string {
	if r.RequiredApprovals <= 0 {
		return ""
	}
	need := "*Needs 1 approval*"
	if r.RequiredApprovals > 1 {
		need = "*Needs " + strconv.Itoa(r.RequiredApprovals) + " approvals from different people*"
	}
	rule := ""
	if strings.TrimSpace(r.ApprovalsRule) != "" {
		rule = code(r.ApprovalsRule)
	}
	var why string
	switch r.ApprovalsSetBy {
	case "rule":
		why = "set by the rule " + firstNonEmpty(rule, "an operator wrote")
	case "no_rule":
		why = "no rule matched this command"
	case "unparseable":
		why = "the rules could not parse this command"
	case "risk_model":
		why = "raised by the risk model"
		if rule != "" {
			why += "; the rule " + rule + " said one"
		}
	case "risk_model_failed":
		why = "the risk model gave no answer oto could take"
		if rule != "" {
			why += "; the rule " + rule + " said one"
		}
	}
	if why == "" {
		return need
	}
	return need + " — " + why
}

// remedyRunsWhen is the confirmation's last sentence: when the approval the reader is about to
// give makes the change happen.
func remedyRunsWhen(required int) string {
	if required <= 1 {
		return "It runs once you approve."
	}
	return "It runs once " + strconv.Itoa(required) + " different people have approved, you among them."
}

// remedyActions is the row under a PROPOSED Remedy's reply: Approve and Decline (ADR 0054
// §2, git-bug ac9b492). Each button's value is the Remedy's id (V11); the press is applied by
// `channels/service` through the same approval the UI makes, for a Slack member LINKED to an
// oto user, and refused with a sentence otherwise.
//
// ⛔ NO APPROVE BUTTON FOR A REMEDY THAT NAMES NO TOOL (ADR 0054 §1): nothing can carry it
// out, so it cannot be approved — only declined. ⛔ NOR FOR ONE WHOSE ARGUMENTS THE REPLY CUT:
// an approval in Slack is an approval of what the card showed, and it did not show it all.
//
// ⭐ APPROVE ASKS FIRST. It is the one button on any oto card that changes a cluster, so it
// carries Slack's confirmation dialog (S10: destructive things live behind a confirm). Decline
// does not: saying no is the safe direction.
//
// ⚠️ THE ROW IS NOT TAKEN DOWN WHEN THE REMEDY MOVES. A reply is posted, never amended, so a
// button on a Remedy that has since been approved, declined or expired stays on screen; its
// press is answered with why it no longer applies. The transition itself is a new reply.
func remedyActions(reason string, iv domain.IncidentView, nonce string) (Block, bool) {
	r := iv.Remedy
	if r == nil || !remedyAwaitingApproval(reason, *r) {
		return Block{}, false
	}
	if _, err := uuid.Parse(r.RemedyID); err != nil {
		return Block{}, false
	}
	elements := make([]Action, 0, 2)
	if r.Tool != "" && remedyArgumentsWhole(*r) {
		elements = append(elements, Action{
			Type: ElementButton, Text: plain("Approve"), ActionID: domain.ActionRemedyApprove, Value: r.RemedyID,
			Confirm: &Confirm{
				Title: plain(truncateRunes("Approve this Remedy?", maxConfirmTitle)),
				Text: plain(truncateRunes("You approve "+r.ToolServer+"__"+r.Tool+" on "+r.Target+
					" with exactly the arguments on this card. "+remedyRunsWhen(r.RequiredApprovals), maxConfirmText)),
				Confirm: plain("Approve"),
				Deny:    plain("Not yet"),
			},
		})
	}
	elements = append(elements, Action{
		Type: ElementButton, Text: plain("Decline"), ActionID: domain.ActionRemedyDecline, Value: r.RemedyID,
	})
	return actionsBlock(blockID("remedyactions", nonce), elements...), true
}

// incidentFinding is an Investigation's latest Finding as a card says it (ADR 0053
// §4): who concluded it and WHEN, then the opening of what it concluded, quoted.
//
// ⭐ IT SAYS "AS SEEN AT" EVERY TIME IT SAYS THE FINDING. A model's sentence about a
// storm reads as present tense unless the card stops it; the instant it was reached
// is what makes it a snapshot (ADR 0016) rather than a claim about now. A Finding a
// budget cut short says "partial" before anything else.
//
// ⭐ A CLASSIFICATION IS SAID AS THE INVESTIGATOR'S, NEVER AS A LABEL OF THE INCIDENT'S
// (ADR 0053 §5). "classified `x`" sits on the line that names who concluded it, so a
// reader cannot take the operator's class for a fact about the signal; a Finding with no
// classification — the org wrote no classes — says nothing about one.
func incidentFinding(f domain.IncidentFindingView) string {
	head := "Finding"
	if f.Partial {
		head = "Partial Finding"
	}
	var b strings.Builder
	b.WriteString(findingEmoji + " *" + head + "* by " + code(f.Investigator+" v"+strconv.Itoa(f.Version)))
	if !f.ConcludedAt.IsZero() {
		b.WriteString(", as seen at " + slackDateTime(f.ConcludedAt))
	}
	if f.Classification != "" {
		b.WriteString(", classified " + code(f.Classification))
	}
	if text := strings.TrimSpace(f.Summary); text != "" {
		b.WriteString("\n>" + strings.ReplaceAll(escape(truncateRunes(text, maxFindingRunes)), "\n", "\n>"))
	}
	return b.String()
}
