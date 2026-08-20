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
	reasonFired        = "fired"
	reasonAllResolved  = "all_resolved"
	reasonRepeat       = "repeat"
	reasonSuppressed   = "suppressed"
	reasonUnsuppressed = "unsuppressed"
	reasonExpired      = "expired"
	reasonRefired      = "refired"
	reasonAcked        = "acked"
	reasonUnacked      = "unacked"
	reasonEnriched     = "enriched"
	reasonRuleChanged  = "rule_changed"
	reasonComment      = "comment"
	// ⭐ `snoozed` AND `unsnoozed` ARE THE ONLY TWO REASONS A SNOOZE MAY NOT
	// SUPPRESS (§B.8.4): "a snooze that cannot announce its own beginning and end
	// is the silent suppression §B.6 forbids". They therefore reach a real channel
	// at EVERY verbosity, which is why having no branch for them was not a cosmetic
	// gap — it was the one card oto is never allowed to get wrong.
	reasonSnoozed   = "snoozed"
	reasonUnsnoozed = "unsnoozed"
	// ⛔ `reasonNewAlerts` AND `reasonSomeResolved` WERE HERE AND ARE DELETED
	// (git-bug 7570090). Both asserted a PLURALITY — "more of them started", "some
	// of them stopped" — and a conversation now holds exactly ONE Case, which is one
	// Alert's firing episode. There is no second member for either to be about.
	//
	// ⚠️ NOTHING WOULD HAVE FAILED TO COMPILE. These constants are duplicated here
	// on purpose (see the block comment above) precisely so that this file does not
	// depend on the notification module's enum — so when `ReasonNewAlerts` and
	// `ReasonSomeResolved` left `internal/notification/domain`, the arms keyed on
	// these strings would simply have become code no stored row can reach. Migration
	// 00069 narrows `notifications_reason_ck` so no row can spell either. Unreachable
	// render arms are the defect class this project has closed five tickets about;
	// they are deleted here in the same change rather than left for a sixth.
	//
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
	body, extra, colour := r.replyBody(v)

	blocks := []Block{sectionBlock(blockID("reply", nonce), truncateSection(body, v.Links.Group))}
	if extra != "" {
		blocks = append(blocks, contextBlock(blockID("replyctx", nonce),
			Text{Type: TypeMrkdwn, Text: truncateField(extra, v.Links.Group)}))
	}

	// ⛔⛔ THE MENTION THIS BLOCK USED TO DESCRIBE IS DELETED (git-bug `bd0fb1d`).
	// It said the mention was added to the top-level `text` only, with the sentence
	// built without it first, because `fallback` and the delivery `summary` are
	// copies of the sentence and a mention in either duplicates a thing that means
	// something in one position only. There is no mention anywhere in oto now, and
	// `text := sentence` — an alias whose only purpose was to hold the augmented
	// string — went with it.
	//
	// ⭐ THE STRUCTURAL INSIGHT OUTLIVED BOTH ITS SUBJECTS AND IS WHY NOTHING IS
	// APPENDED HERE. Two affordances have now been argued into position and then
	// deleted — the mention (git-bug `bd0fb1d`) and the "open in oto" link (git-bug
	// `7570090`, with broadcast) — and they came out in OPPOSITE places for a reason
	// that has nothing to do with either of them. A mention only means anything in a
	// push notification, so it had to be in the `text`; a link has to be somewhere a
	// reader can click, and the `text` is bound by ADR 0020 rule 4 to be a
	// self-sufficient SENTENCE — which is what
	// `TestAReplyTopLevelTextCarriesSeverityAndDuration` asserts, by refusing
	// `<http` in this exact string. The rule survives the broadcast it was written
	// for: this string is still a locked phone's notification and still what a
	// screen reader announces. The reasoning is kept because the next affordance
	// will have to choose again, and both precedents are now history rather than
	// code.
	sentence := replyText(v)

	return Payload{
		Text:        sentence,
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
// ⛔ IT NO LONGER TAKES `domain.RenderOptions`, AND THAT IS THE VISIBLE EDGE OF THE
// BROADCAST DELETION (git-bug 7570090). Two things consumed the options here and
// both are gone: `o.Mode` keyed the "open in oto" link (see the tombstone at the
// tail of this function), and `o.MaxInstances` bounded the `new_alerts` arm's member
// list. A reply's body is now a pure function of the view — no delivery mode, no
// per-channel limit, changes it — which is worth stating rather than hiding behind
// an ignored parameter.
func (r *Renderer) replyBody(v *domain.NotificationView) (body, extra, colour string) {
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

	// ⛔ THE `new_alerts` ARM WAS HERE AND IS DELETED (git-bug 7570090). It drew
	// ":heavy_plus_sign: *N more instances now firing*" with the newcomers named and
	// a running total. One Case is one Alert's episode, so there is never a second
	// instance to arrive and never a total above one; the Reason is gone from the
	// vocabulary and 00069 stops any row spelling it.

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
		// ⛔ THE "N of M instances" CLAUSE WAS HERE AND IS DELETED (git-bug 7570090),
		// AND IT IS THE REASON THAT SURVIVES ITS COUNT RATHER THAN ITS COUNT
		// SURVIVING. `all_resolved` STAYS — it is how oto says a thing stopped, and
		// the vocabulary has no plain `resolved` — but one Case holds one Alert, so
		// the clause could only ever have read "1 of 1 instances". A tautology on
		// every single resolve card is worse than silence: it invites a reader to
		// look for the number that is missing.

	// ⛔ THE `some_resolved` ARM WAS HERE AND IS DELETED (git-bug 7570090). It said
	// "N of M instances resolved — the rest are still firing", which is a sentence
	// about a set. §H.6 already made the Reason update-only and this arm existed only
	// so a channel that asked for it anyway got something true; there is now nothing
	// true left to say, because a Case whose one Alert resolved is `all_resolved`.

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

	// ⛔⛔ THE WAY BACK TO OTO WAS HERE AND IS DELETED (git-bug 7570090), AND IT
	// WENT BECAUSE THE THING IT WAS KEYED ON WENT.
	//
	// `68653ca` added `if o.Mode == ModeBroadcastReply && v.Links.Group != ""` and
	// appended " — <link|open in oto>" to the body, with fifty-seven lines arguing
	// the placement. The premise was that a broadcasting reply is the one message
	// oto addresses to people who are NOT following the thread, and that Slack's
	// in-channel copy shows no buttons (ADR 0020 rule 5b, ⭐ confirmed by
	// observation) — so a mrkdwn link was not the safest affordance but the ONLY
	// one. Broadcast is deleted, every reply oto now sends is read inside the thread
	// where the root card's buttons are one scroll away, and the guard is
	// unreachable: no `o.Mode` value can satisfy it.
	//
	// ⭐ THE PART THAT IS NOT ABOUT BROADCAST AND MUST NOT BE LOST WITH IT. The
	// argument settled WHERE an affordance goes, and that answer still holds for the
	// next one: a link belongs in the BODY, not in the top-level `text`, because the
	// `text` is a self-sufficient SENTENCE and the push notification on a locked
	// phone (ADR 0020 rule 4, which survives its mechanism — see `replyText`), and
	// because the Block Kit Builder capture is the attachment's block list LIFTED
	// OUT of the payload, so a `text`-only affordance appears in no fixture a human
	// reviews. A mention went the other way for the mirror-image reason. Both
	// answers are recorded on `renderReply` and on `replyText`.
	//
	// ⚠️ ONE OBSERVATION IS PERMANENTLY UNOWED NOW. Amendment 4 never watched a
	// section block's TEXT render in an in-channel copy — it saw the colour bar and
	// the absent buttons — and git-bug `2078a07` carried that as an observation
	// owed. Nothing renders an in-channel copy any more, so the question is retired
	// rather than answered.

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
// own next clause counts ten instances. (`unackedFor` was the contrasting case
// below — it DID prefer the case, because "how long has nobody acknowledged this"
// is a question about the focused episode. It went with the unacked reminder,
// git-bug bd0fb1d. Same shape, different question, and worth remembering the next
// time a duration is rendered.)
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

// ⛔ `newlyFiring` AND `nameList` WERE HERE AND ARE DELETED (git-bug 7570090).
// `newlyFiring` picked out "the members that arrived with this notification" and
// `nameList` rendered them as a comma-separated, `MaxInstances`-bounded list of
// code spans. The `new_alerts` arm was their only caller, and a Case has one
// Alert: there is no arrival to detect and no list to bound. Left in place they
// would be unreachable helpers whose signatures still advertise a member list —
// the same misleading survival the deleted arms would have been.

// replyText is the reply's own complete sentence. A thread reply's push
// notification is read exactly as often as the root's, and by the same people.
//
// ⛔⛔ ADR 0020 RENDERING RULE 4 MAKES THIS CORRECTNESS, NOT STYLE — AND IT IS
// THE ONE PART OF ADR 0020 THAT OUTLIVED THE ADR'S OWN MECHANISM.
//
// ⛔ THE DERIVATION IS NOW HISTORY, AND KEEPING IT MATTERS BECAUSE THE RULE DID
// NOT DEPEND ON IT. Rule 4 was derived from Slack documenting the in-channel
// `thread_broadcast` reference as unable to carry attachments or buttons, which
// would have made this string very nearly everything a channel reader sees. A live
// workspace then contradicted half of that (Amendment 4: the attachment comes back
// intact and the colour bar renders), so colour became a PROGRESSIVE ENHANCEMENT
// and buttons were confirmed absent. Broadcast is now deleted outright (git-bug
// 7570090) and there is no in-channel reference of any kind.
//
// ⭐ THE RULE STANDS ON THE GROUND IT HAS ALWAYS ACTUALLY STOOD ON: THIS STRING IS
// THE PUSH NOTIFICATION ON A LOCKED PHONE AND THE TEXT A SCREEN READER ANNOUNCES.
// Neither has ever rendered a colour bar, an attachment or a button, and neither
// cares whether the message was broadcast. A reply whose text reads "Re-fired"
// communicates nothing to the person it woke up, in a thread exactly as in a
// channel. Deleting the mechanism narrowed the rule's audience; it did not weaken
// the rule, and `TestAReplyTopLevelTextCarriesSeverityAndDuration` still asserts
// it.
func replyText(v *domain.NotificationView) string {
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
	// ASPIRATIONAL, AND ITS ORIGINATING DEFECT IS STILL THE BEST ARGUMENT FOR IT.
	// The first live run sent
	//
	//	":repeat: Re-fired: alertname=OtoSmokeTest, cluster=smoke-test"
	//
	// — no severity, no duration, no state. It named a thing and said nothing about
	// it, so a reader could not tell whether to open the thread.
	//
	// ⭐ AND THIS IS THE CLAUSE THAT ALREADY DID NOT CARE ABOUT BROADCAST, which is
	// why the mode's deletion (git-bug 7570090) costs it nothing. It was added to
	// EVERY reply on the stated ground that "a rule that only holds for the replies
	// that happen to broadcast is a rule that breaks the first time the broadcast
	// set changes". The broadcast set has now changed to the empty set, and the
	// clause is exactly as necessary as it was: a thread reply's push notification
	// is the same notification, read by the same people, on the same locked phone.
	// The line that was written to survive a change in the set survived the set.
	if facts := replyFacts(v); facts != "" {
		out += " " + endSentence(facts)
	}

	// ⛔⛔ NOTHING IS APPENDED HERE ANY MORE, AND EVERY PREMISE THE OLD ARGUMENT
	// RESTED ON HAS NOW BEEN DELETED IN TURN. Kept because the shape of the argument
	// is what the next affordance will need, not the conclusion.
	//
	// It said the mention lives here and nowhere else, because everything the
	// renderer builds sits inside ONE attachment (§H.1 S3, the only way to get a
	// colour bar) and Slack STRIPS attachments from the in-channel
	// `thread_broadcast` reference — so a mention inside a block would be invisible
	// in the channel. Three things happened to that:
	//
	//   1. The mention was deleted (git-bug `bd0fb1d`): the owner withdrew the
	//      unacked reminder and ruled the mention goes with it.
	//   2. ⭐ THE STRIPPING PREMISE ITSELF TURNED OUT TO BE FALSE. ADR 0020
	//      Amendment 4: a live workspace returns the `thread_broadcast` message WITH
	//      its `attachments` array intact and the colour bar renders. What Slack's
	//      documentation is right about is BUTTONS — confirmed absent by observation
	//      (rule 5b), not merely suspected. So a block was not invisible in the
	//      channel; an interactive element was.
	//   3. ⛔ AND THEN THE IN-CHANNEL COPY ITSELF WENT (git-bug 7570090). Broadcast
	//      is deleted, so there is no second rendering of a reply anywhere and no
	//      position that is visible in one place and stripped in another. The
	//      `text`/body distinction now turns on ONE thing only, which is the thing
	//      that was always load-bearing: the `text` is the push notification and the
	//      screen-reader string, the body is what a reader looks at and clicks.
	return truncateClause(oneLine(out), otoTopLevelText)
}

// replyFacts is the severity-and-duration clause every reply's top-level text
// carries, in words rather than in colour.
//
// §H.2 encodes severity as colour AND emoji precisely because colour alone fails
// accessibility, and a push notification or a screen reader has NEITHER, so words
// are all that is left. ⛔ THIS USED TO CITE THE BROADCAST'S IN-CHANNEL REFERENCE
// as the surface with no colour and no emoji; broadcast is deleted (git-bug
// 7570090) and the argument did not need it — a locked phone was always the
// stronger example, because it is the one every reply reaches.
// The duration answers the question the reader actually has, which
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
		// "It came back, and it came back fast" is the whole reason a re-fire is worth
		// a reply of its own (ADR 0020, Amendment 1 — which argued it as the reason a
		// re-fire BROADCAST; broadcast is deleted, git-bug 7570090, and the
		// observation about what a reader needs to be told outlived it). Saying how
		// fast is the point.
		if v.Case != nil && v.Case.Duration > 0 {
			facts = append(facts, "firing again after "+humanDuration(v.Case.Duration))
		} else {
			facts = append(facts, "firing again since "+plainClock(groupStart(v)))
		}
	case reasonExpired:
		facts = append(facts, "last seen at "+plainClock(v.Group.LastActivityAt))
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
	case reasonRefired:
		return ":repeat: Re-fired:"
	case reasonAllResolved:
		return ":white_check_mark: All resolved:"
	// ⛔ `new_alerts` AND `some_resolved` HAD LEADS HERE AND THEY ARE DELETED
	// (git-bug 7570090) — ":heavy_plus_sign: More instances now firing:" and
	// ":arrow_down: Partly resolved:". Both name a plurality one Case cannot have.
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
	case reasonDegraded:
		return ":warning: oto could not update the thread for:"
	case reasonContinued:
		return ":arrow_right: Continued in a new message:"
	default:
		return ":bell: Update on:"
	}
}
