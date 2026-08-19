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
	// ⭐ `snoozed` AND `unsnoozed` ARE THE ONLY TWO REASONS A SNOOZE MAY NOT
	// SUPPRESS (§B.8.4): "a snooze that cannot announce its own beginning and end
	// is the silent suppression §B.6 forbids". They therefore reach a real channel
	// at EVERY verbosity, which is why having no branch for them was not a cosmetic
	// gap — it was the one card oto is never allowed to get wrong.
	reasonSnoozed   = "snoozed"
	reasonUnsnoozed = "unsnoozed"
	// ⛔ `reasonStorm` WAS HERE AND IS DELETED (ADR 0042). Storm damping is removed,
	// `storm` has left the Reason vocabulary, and migration 00060 narrows
	// `notifications_reason_ck` so no row can spell it — so the reply heading it
	// named can never be reached. It was briefly kept to draw a stored row; the
	// authorised database reset means there is no stored row to draw.

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

	// ⛔⛔ THE MENTION IS ADDED TO THE TOP-LEVEL `text` ONLY, AND THE SENTENCE IS
	// BUILT WITHOUT IT FIRST (ADR 0020, Amendment 4).
	//
	// The attachment's own `fallback` and the delivery `summary` are copies of the
	// sentence used for notification previews and for the timeline; a mention in
	// either is a duplicate of a thing that only means something in one position.
	// The top-level text is the ONLY place a mention reaches a push notification,
	// which is the whole point of mentioning somebody.
	sentence := replyText(v, nil)
	text := replyText(v, o.Mentions)

	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Metadata:    rootMetadata(v),
		Attachments: []Attachment{{
			Color:    colour,
			Fallback: truncateRunes(sentence, 200),
			Blocks:   blocks,
		}},
	}, sentence, sentence
}

// replyBody returns the reply's mrkdwn, an optional context line, and the colour
// bar. Every branch is one of the §H.5 reply types.
func (r *Renderer) replyBody(v *domain.NotificationView, o domain.RenderOptions) (body, extra, colour string) {
	state := cardState(v)

	switch v.Reason {
	case reasonAcked:
		colour = CardAcknowledged.Colour()
		who, attributed := ackedBy(v)
		body = ":eyes: *Acknowledged*" + by(who, " automatically", attributed)
		if note := ackNote(v); note != "" {
			body += " — _" + note + "_"
		}

	case reasonUnacked:
		colour = CardFiring.Colour()
		// ⭐ THE COMMON UN-ACKNOWLEDGEMENT HAS NO HUMAN BEHIND IT. T10 drops the ack
		// when a NEW episode opens, and `autoUnackEvent` records that with
		// `actor_kind = 'system'` — so " automatically" is not a fallback here, it is
		// the usual answer, and it is the difference between "somebody took this back"
		// and "it came back and your receipt no longer applies".
		// Here `who` and the impersonal test are the SAME fact — the un-ack's own
		// actor — which is why this call site passes `v.Actor != nil` itself.
		body = ":arrow_uturn_left: *Un-acknowledged*" +
			by(actorLabel(v), " automatically", v.Actor != nil)
		// ⭐ `Seq > 1` IS THE SURVIVING WITNESS. This used to read `ReopenCount > 0`
		// and meant "the receipt lapsed because the alert came back". Since ADR 0040
		// a re-fire ALWAYS opens the next episode, so the episode ordinal says the
		// same thing and says it from a column that still exists: an episode above
		// the first succeeded one that had ended.
		if v.Case != nil && v.Case.Seq > 1 {
			body += " — new case opened"
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
		if v.Case != nil {
			if v.Case.Duration > 0 {
				body += " after " + humanDuration(v.Case.Duration)
			}
			// The episode ordinal is the whole story now: a re-fire is a NEW case at
			// the next `seq`, never a reopen of this one (ADR 0040).
			body += " — case #" + strconv.Itoa(v.Case.Seq)
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
		body = ":mute: *Silenced*" + silencedBy(v)
		if until := suppressedUntil(v); until != "" {
			body += " until " + until
		}
		if note := suppressionNote(v); note != "" {
			body += " — _" + note + "_"
		}

	case reasonUnsuppressed:
		colour = CardFiring.Colour()
		body = ":speaker: *Silence ended* — this alert is firing again"

	case reasonSnoozed:
		// ⛔⛔ `state.Colour()`, NOT A COLOUR OF ITS OWN, AND NOT `CardSuppressed`.
		//
		// Snooze is the THIRD ORTHOGONAL AXIS (§B.8.1): a snoozed firing critical is
		// still a firing critical and every surface "MUST continue to render it that
		// way" (§B.8.6) — `#a30200` / `:rotating_light:`, unchanged. §H.4 states the
		// consequence in a call-out: "Colouring a snoozed critical calm would be the
		// exact lie §E.1.1 exists to prevent." `TestGoldenSnoozedCriticalKeepsTheFiringColour`
		// is §P-17's own proof of it.
		//
		// Borrowing `CardSuppressed`'s grey would be the same lie wearing a different
		// coat: that grey means an ALERTMANAGER SILENCE, which only the reconciler can
		// produce (C1), and a snooze writes nothing into the cluster (R3, §B.8.1).
		//
		// The defect this branch fixes was never the colour. It was that the card
		// changed and NEVER SAID WHY: `replyBody` fell to `default:` and posted
		// ":information_source: *Title* — :fire: Firing" — an announcement of the very
		// alert oto had just agreed to stop mentioning. The fix is words, not paint.
		colour = state.Colour()
		body = ":zzz: *Snoozed*" + snoozedBy(v)
		if until := snoozeUntil(v); until != "" {
			body += " until " + until
		}
		if note := snoozeNote(v); note != "" {
			body += " — _" + note + "_"
		}
		// What oto will DO, which is the only thing that actually changed. It is
		// phrased about oto's behaviour and never about the signal's state, because
		// this is the one card whose subject is oto's own quiet — and because §B.8.1's
		// two negations are what stop a reader filing a snooze as a silence or as a
		// resolution. The auto-expiry is named on purpose: there is no indefinite
		// snooze (§B.8.3), and the card that announces the quiet is the right place
		// for the sentence that promises it ends.
		//
		// ⛔ IT SAYS "while the snooze lasts", NOT "until then", AND THE DIFFERENCE IS
		// NOT STYLE. "until then" is a back-reference to the until-when clause above,
		// and that clause is CONDITIONAL: `SnoozedUntil` is nil whenever the snooze row
		// was swept between the suppression decision and this render. The first cut of
		// this branch read "until then" and the golden caught it pointing at nothing —
		// a card promising quiet until a moment it had not named, which is a smaller
		// copy of the very defect this ticket is about. This sentence is true with the
		// clause and without it.
		extra = "_oto will not post about this again while the snooze lasts. A snooze " +
			"changes nothing upstream and nothing about the alert's own state._"

	case reasonUnsnoozed:
		// The one card whose entire content is "oto is audible again", and §B.6's
		// requirement runs in both directions: a silence that cannot announce its END
		// is still a silence nobody can distinguish from "there was no alert".
		//
		// It must not read as a state change. Nothing about the alert moved when the
		// snooze ended — an expiry sweep or a human lifted oto's own quiet — so the
		// body names oto as the subject and the context line says outright that the
		// signal did not change. Without that sentence a reader who sees a card move
		// reasonably assumes the alert did something, which is the mirror of the
		// defect on the `snoozed` side.
		colour = state.Colour()
		body = ":bell: *Snooze ended* — oto is posting about this again"
		extra = "_The snooze was lifted or ran out. Nothing about the alert itself " +
			"changed when it did._"

	case reasonRuleChanged:
		colour = state.Colour()
		body, extra = ruleChangedReply(v)

	case reasonEnriched:
		colour = state.Colour()
		body = ":sparkles: " + enrichmentSummary(v)

	case reasonComment:
		colour = state.Colour()
		body = commentBody(v)

	case reasonUnackedReminder:
		colour = CardFiring.Colour()
		body = ":rotating_light: *Still unacknowledged"
		if d := unackedFor(v); d != "" {
			body += " after " + d
		}
		body += ".*"
		// ⛔ NO MENTION HERE. It goes in the TOP-LEVEL `text` — see replyText.
		// A mention inside a block does not reach a push notification, and a
		// reminder that does not reach the phone of somebody who has NOT engaged
		// is not a reminder. ADR 0020, Amendment 4.
		body += " — " + link(v.Links.Group, "open in oto")

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
// regardless of verbosity (§H.5): "the rule underneath this alert changed" is the
// single most valuable thing oto can say.
func ruleChangedReply(v *domain.NotificationView) (string, string) {
	rc := v.RuleChange
	if rc == nil {
		return ":scroll: *The rule changed since the last case.*", ""
	}

	var b strings.Builder
	b.WriteString(":scroll: *The rule changed since the last case.*")

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

// commentBody renders what a human typed, into the thread they typed it into.
//
// ⛔ THE TEXT IS THE ENTIRE MESSAGE. A `comment` notification exists for one
// reason — somebody wrote something — so a balloon with nothing after it is not
// a degraded card, it is a person's words replaced by an emoji in front of the
// channel they wrote them to. CONTEXT.md §6 is blunt about the stakes: human
// comments "live nowhere else".
//
// If the text really did not reach the view, SAY THAT and point at the one place
// it does exist. An operator who sees "a comment was added" opens the timeline;
// an operator who sees a lone balloon concludes oto is broken, and is right.
func commentBody(v *domain.NotificationView) string {
	text := escape(oneLine(v.Comment))
	if text == "" {
		return ":speech_balloon: *A comment was added* — " +
			link(v.Links.Group, "read it in oto") + "."
	}
	return ":speech_balloon: " + commentPrefix(actorLabel(v)) + text
}

func commentPrefix(who string) string {
	if who == "" {
		return ""
	}
	return who + ": "
}

// by renders the attribution clause of a sentence about something that was
// DONE — and never renders a dangling " by".
//
// ⛔ THE THREE CASES ARE THREE DIFFERENT FACTS AND THE CARD MUST NOT CONFLATE
// THEM (git-bug 56a9951, where every one of these read as the third):
//
//	a person did it        → " by <name>", the name the timeline froze
//	something else did it  → `impersonal`, the caller's own true phrase
//	oto does not know      → nothing, because inventing an agent is worse
//
// The middle case is detected from the actor rather than guessed: a HUMAN actor
// on `alert_events` is guaranteed both an id and a label (ev_actor_ck), so a
// recorded actor that renders to no name is one of oto's own machines — system,
// reconciler, ingest. `impersonal` is passed in per call site because "what else
// did it" is a different sentence for every fact — an acknowledgement no human
// took went `automatically` — and a call site with nothing true to say passes
// the empty string rather than reaching for a vague one.
//
// ⛔ `attributed` IS ABOUT THE FACT `who` CAME FROM, AND THE CALLER IS THE ONLY
// ONE WHO KNOWS WHICH FACT THAT IS. This used to read `v.Actor` — the actor of
// the ANNOUNCED fact — while `who` was computed from a DIFFERENT one, which is
// the conflation the three cases above exist to forbid: a `comment` on a
// suppressed member of an acknowledged group made `v.Actor` the commenter and
// the status line read "Acknowledged automatically" for an ack humans took.
func by(who, impersonal string, attributed bool) string {
	switch {
	case who != "":
		return " by " + who
	case attributed:
		return impersonal
	default:
		return ""
	}
}

// ackedBy names the human whose acknowledgement the card is talking about, and
// says whether that ACKNOWLEDGEMENT has a recorded actor at all.
//
// ⛔ THE REASON GATE IS THE WHOLE FUNCTION. `v.Actor` is the actor of the FACT
// BEING ANNOUNCED, not of the ack — on a `comment` card it is whoever commented
// — so reading it unconditionally would print one person's name against another
// person's action on any card that happens to be in the acknowledged state. The
// ack has its own frozen attribution on the case, and that is what every
// other card reads.
//
// It is one function because the root card's Status field, the root card's
// Acknowledged field and the thread reply are three renderings of ONE fact, and
// they disagreed for as long as they each answered the question themselves.
//
// ⛔ THE SECOND RETURN IS THE SAME GATE, FOR THE SAME REASON. An empty name means
// "no human is named", and only the ACK's own record can say whether that is a
// machine or an absence — so `attributed` is true only where this function
// actually looked at the acknowledgement: the announcing card's own actor, or the
// case's frozen label. On any other card an unnamed ack is oto not knowing,
// and `by` must render nothing rather than claim a machine took it.
func ackedBy(v *domain.NotificationView) (who string, attributed bool) {
	// The card IS the acknowledgement: its own actor is the freshest answer and
	// the only one that can carry a Slack mention rather than a display name.
	if v.Reason == reasonAcked {
		if name := actorLabel(v); name != "" {
			return name, true
		}
	}
	if v.Case != nil && v.Case.AckedByLabel != "" {
		return code(v.Case.AckedByLabel), true
	}
	// A recorded actor on the acknowledgement itself that renders to no name is
	// one of oto's machines, and " automatically" is the true sentence for it.
	return "", v.Reason == reasonAcked && v.Actor != nil
}

// silencedBy attributes a silence, and it ALWAYS attributes it.
//
// ⛔ "upstream" IS THE TRUE ANSWER, NOT A HEDGE, and it is why this one does not
// go through `by`. oto has no write path into the cluster and v1 will not grow
// one (R3, H-3), and only the reconciler can move a case into
// `suppressed` — so a silence was ALWAYS created in somebody else's UI, whether
// or not oto recorded who. Saying nothing would leave a reader to assume oto
// went quiet by itself, which is the one thing §B.6 will not have a card imply.
//
// Unlike an ack, a silence has no frozen attribution anywhere in the read model
// — `alert_cases` keeps the suppression's REASON, never its author — so
// the announcing notification is the only card that can name a person at all,
// and every later amend of the same root honestly says `upstream`.
func silencedBy(v *domain.NotificationView) string {
	if v.Reason == reasonSuppressed {
		if who := actorLabel(v); who != "" {
			return " by " + who
		}
	}
	return " upstream"
}

func ackNote(v *domain.NotificationView) string {
	if v.Case != nil && v.Case.AckNote != "" {
		return escape(oneLine(v.Case.AckNote))
	}
	if v.Comment != "" {
		return escape(oneLine(v.Comment))
	}
	return ""
}

func suppressionNote(v *domain.NotificationView) string {
	if v.Case != nil && v.Case.SuppressionReason != "" {
		return escape(oneLine(v.Case.SuppressionReason))
	}
	if v.Comment != "" {
		return escape(oneLine(v.Comment))
	}
	return ""
}

func suppressedUntil(v *domain.NotificationView) string {
	if v.Case != nil && v.Case.EndedAt != nil {
		return slackDate(*v.Case.EndedAt)
	}
	return ""
}

// snoozedBy attributes oto's own quiet, and unlike `silencedBy` it has no
// "upstream" to fall back on.
//
// ⛔ THE TWO ARE OPPOSITES AND THE FALLBACKS SAY SO. A silence is ALWAYS somebody
// else's — oto has no write path into the cluster (R3, H-3) — so `silencedBy`
// naming "upstream" when it knows no name is the true sentence. A snooze is
// ALWAYS oto's own and always a deliberate human act: §B.8.1 lists "attributed and
// visible" against "a silent suppression", and §B.8.3's only operation is
// `snooze(alert_id, until, note)`. So an unattributed snooze is oto failing to
// record something it owns, and the honest rendering is to say nothing rather than
// to invent an author or to blame a cluster that was never involved.
//
// The reason gate is `ackedBy`'s, for `ackedBy`'s reason: `v.Actor` is the actor of
// the FACT BEING ANNOUNCED, so on any other card it is a different person doing a
// different thing.
func snoozedBy(v *domain.NotificationView) string {
	if v.Reason != reasonSnoozed {
		return ""
	}
	if who := actorLabel(v); who != "" {
		return " by " + who
	}
	return ""
}

// snoozeUntil is the fact the card could not previously reach at all.
//
// ⛔ IT IS `NotificationView.SnoozedUntil` AND NEVER `Case.EndedAt`. `suppressedUntil`
// above reads the case because an Alertmanager silence ends the case's suppression;
// a snooze touches no case, no state and no column on the alert (§B.8.1), so the
// case has nothing to say about when oto starts talking again. Reading it here
// would print a silence's expiry on a snooze's card.
//
// ⛔ `slackDateTime`, NOT `slackDate`. Every other until-when on these cards points
// at something hours away at most, so `slackDate`'s bare `{time}` is unambiguous.
// A snooze runs up to thirty days (§B.8.3), and "until 22:02" a week out names the
// wrong evening.
func snoozeUntil(v *domain.NotificationView) string {
	if v.SnoozedUntil == nil {
		return ""
	}
	return slackDateTime(*v.SnoozedUntil)
}

// snoozeNote is what the human typed into `snooze(alert_id, until, note)`.
//
// ⛔ IT DOES NOT FALL BACK TO `Case.SuppressionReason` the way `suppressionNote`
// does. That column is Alertmanager's silence comment and is explicitly "NOT" oto's
// (`notification/domain/suppression.go`); printing it under "Snoozed by ram" would
// attribute a stranger's sentence to the person who asked for quiet.
func snoozeNote(v *domain.NotificationView) string {
	if v.Comment == "" {
		return ""
	}
	return escape(oneLine(v.Comment))
}

// resolvedAfter measures THE GENERATION, because the sentence it feeds is about
// the generation: "All resolved after <d> — N of M instances".
//
// ⛔ `v.Case.Duration` IS NOT CONSULTED, AND THAT IS THE POINT. It is one episode's
// span. On a group of ten pods that failed over forty minutes, reporting whichever
// case the view happened to focus — the first to resolve or the last, depending on
// how the notification was raised — puts one member's duration in a sentence whose
// own next clause counts ten instances. Contrast `unackedFor` below, which DOES
// prefer the case: "how long has nobody acknowledged this" is a question about the
// focused episode, and it starts when oto knew. Same shape, different question.
//
// ⛔ AND THE START IS `StartedAt`, NEVER `FirstSeenAt` DIRECTLY. FirstSeenAt is when
// oto first heard about the signal; the gap to upstream's `startsAt` is oto's
// latency plus Alertmanager's `group_wait`, measured at TWENTY-ONE MINUTES in the
// first live run (view.go:64-70). On a duration sentence that gap is subtracted
// straight off the outage. The fallback order here is exactly
// `GroupFacts.StartedAt()`'s — upstream's start when there is one, oto's first
// sighting only when there is none — so every renderer, the API and the UI keep
// answering this question the same way.
//
// Both old branches were biased the SAME direction: one member's episode is shorter
// than the generation's span, and FirstSeenAt is later than StartedAt. Low is the
// direction that makes an incident look smaller than it was, in the line most
// likely to be pasted into a postmortem.
func resolvedAfter(v *domain.NotificationView) string {
	start := v.Group.StartedAt
	if start.IsZero() {
		start = v.Group.FirstSeenAt
	}
	if start.IsZero() || !v.Group.LastActivityAt.After(start) {
		return ""
	}
	return humanDuration(v.Group.LastActivityAt.Sub(start))
}

func unackedFor(v *domain.NotificationView) string {
	start := v.Group.FirstSeenAt
	if v.Case != nil && !v.Case.StartedAt.IsZero() {
		start = v.Case.StartedAt
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
		if a.State == "firing" && a.TotalCases <= 1 {
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
// ⛔⛔ ADR 0020 RENDERING RULE 4 MAKES THIS CORRECTNESS, NOT STYLE, AND
// AMENDMENT 4 MAKES IT CORRECTNESS FOR A BETTER REASON THAN IT FIRST HAD.
//
// The rule was derived from Slack documenting the in-channel `thread_broadcast`
// reference as unable to carry attachments or buttons — which would have made
// this string very nearly everything a channel reader sees. A live workspace
// contradicts that: the attachment is returned intact by `conversations.history`
// and the colour bar was observed rendering. Colour is therefore a PROGRESSIVE
// ENHANCEMENT (5a) and buttons are UNVERIFIED (5b), and oto depends on neither.
//
// The rule stands because of what has never been in question: THIS STRING IS THE
// PUSH NOTIFICATION ON A LOCKED PHONE AND THE TEXT A SCREEN READER ANNOUNCES.
// Neither has ever rendered a colour bar. A broadcast whose text reads "Re-fired"
// is a broadcast that communicates nothing to the person it woke up.
func replyText(v *domain.NotificationView, mentions []string) string {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	out := replyLead(v.Reason) + " " + title
	if cluster := clusterChip(v); cluster != "" {
		out += " on " + cluster
	}
	out += "."

	// ⛔⛔ THE FACTS CLAUSE IS WHAT MAKES ADR 0020's RULE 4 TRUE RATHER THAN
	// ASPIRATIONAL. The first live run broadcast
	//
	//	":repeat: Re-fired: alertname=OtoSmokeTest, cluster=smoke-test"
	//
	// into a channel — no severity, no duration, no state. Rule 4 says a
	// broadcast's top-level text must be SELF-SUFFICIENT because the in-channel
	// copy carries no colour bar and no buttons, and that string fails its own
	// rule: it names a thing and says nothing about it. A reader in the channel
	// cannot tell whether to open the thread, which is the only action a broadcast
	// asks for.
	//
	// The same clause is added to every reply, not only the broadcasting ones. A
	// thread reply's push notification is read by the same people through the same
	// surface, and a rule that only holds for the replies that happen to broadcast
	// is a rule that breaks the first time the broadcast set changes.
	if facts := replyFacts(v); facts != "" {
		out += " " + endSentence(facts)
	}

	// ⛔⛔ THE MENTION LIVES HERE AND NOWHERE ELSE (ADR 0020). Everything the
	// renderer builds for the thread sits inside ONE attachment (§H.1 S3, the only
	// way to get a colour bar), and Slack strips attachments from the in-channel
	// `thread_broadcast` reference — so a mention inside a block is invisible in
	// the channel, notification or not. This string is very nearly all a channel
	// reader sees, which makes it the only position a mention can occupy.
	//
	// It goes AFTER the sentence: the sentence has to be readable by the people
	// who were not mentioned, and a message that opens with four user ids reads as
	// addressed to four people rather than to the channel.
	//
	// The audience is already resolved and already gated on severity by the org's
	// policy. This renderer does not decide WHO — only WHERE.
	if m := mentionList(mentions); m != "" {
		out += " " + m
	}
	return truncateClause(oneLine(out), otoTopLevelText)
}

// replyFacts is the severity-and-duration clause every reply's top-level text
// carries, in words rather than in colour.
//
// §H.2 encodes severity as colour AND emoji precisely because colour alone fails
// accessibility; a broadcast's in-channel reference has NEITHER, so words are all
// that is left. The duration answers the question the reader actually has, which
// is not "what happened" — the lead already said that — but "how bad is this and
// how long has it been going on".
func replyFacts(v *domain.NotificationView) string {
	state := cardState(v)
	facts := make([]string, 0, 3)

	if sev := v.Group.Severity; sev != "" {
		facts = append(facts, "Severity "+sev)
	}
	if team := labelOf(v, "team"); team != "" {
		facts = append(facts, "team "+team)
	}

	switch v.Reason {
	case reasonAllResolved:
		if d := resolvedAfter(v); d != "" {
			facts = append(facts, "resolved after "+d)
		}
	case reasonRefired:
		// "It came back, and it came back fast" is the whole reason a re-fire
		// broadcasts at all (ADR 0020, Amendment 1). Saying how fast is the point.
		if v.Case != nil && v.Case.Duration > 0 {
			facts = append(facts, "firing again after "+humanDuration(v.Case.Duration))
		} else {
			facts = append(facts, "firing again since "+plainClock(groupStart(v)))
		}
	case reasonExpired:
		facts = append(facts, "last seen at "+plainClock(v.Group.LastActivityAt))
	case reasonUnackedReminder:
		if d := unackedFor(v); d != "" {
			facts = append(facts, "unacknowledged for "+d)
		}
		facts = append(facts, "firing since "+plainClock(groupStart(v)))
	case reasonSnoozed:
		// ⛔⛔ THIS CLAUSE IS THE WHOLE POINT OF THE TICKET. The `default:` arm below
		// appends `stateClause`, so the string that reached a locked phone read
		// "… firing since 17:56 UTC" — oto announcing the alert at the exact moment
		// it agreed to stop announcing it. The snooze is the fact being communicated,
		// so the snooze is what this clause must name.
		//
		// Severity and team are appended ABOVE and deliberately survive: ADR 0020
		// Rule 4 binds this string to be self-sufficient, and a reader in the channel
		// still has to be able to tell a snoozed `info` from a snoozed `critical`.
		// Naming the snooze is not the same as hiding the signal.
		if v.SnoozedUntil != nil {
			facts = append(facts, "oto quiet until "+plainMoment(*v.SnoozedUntil, v.RenderedAt))
		} else {
			facts = append(facts, "oto has gone quiet about this")
		}
	case reasonUnsnoozed:
		facts = append(facts, "oto is posting about this again")
	default:
		facts = append(facts, stateClause(v, state))
	}

	return joinNonEmpty(", ", facts...)
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
	// `:zzz:` and `:bell:` are §B.8.6's own two emoji for the snooze axis — the
	// card's `*Notifications*` field and the `Unsnooze` action. They are reused here
	// so that one signal wears one symbol across every surface a reader meets it on.
	case reasonSnoozed:
		return ":zzz: Snoozed:"
	case reasonUnsnoozed:
		return ":bell: Snooze ended:"
	case reasonRuleChanged:
		return ":scroll: The alerting rule changed for:"
	case reasonEnriched:
		return ":sparkles: New enrichment for:"
	case reasonComment:
		return ":speech_balloon: New comment on:"
	case reasonUnackedReminder:
		return ":rotating_light: Still unacknowledged:"
	case reasonDegraded:
		return ":warning: oto could not update the thread for:"
	case reasonContinued:
		return ":arrow_right: Continued in a new message:"
	default:
		return ":bell: Update on:"
	}
}
