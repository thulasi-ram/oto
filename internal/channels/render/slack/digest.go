package slack

import (
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// digestEmoji leads every digest surface: the card, the fallback and the push
// notification. It is a CHART and not a bell on purpose — the leading glyph is the
// first thing a reader resolves, and a digest is the one message oto sends that is
// not asking for attention.
const digestEmoji = ":bar_chart:"

// renderDigest draws the periodic summary: one card per closed window per digest
// policy.
//
// ⭐⭐ IT IS THE ARM THAT DID NOT EXIST (git-bug `78388fb`). The word `digest`
// appeared nowhere in this package, so a digest was drawn by `replyBody`'s and
// `renderRoot`'s `default:` arms as `*Group.Title* — <status>` — with the whole of the
// digest smuggled into the group's name by `notification/service`'s `DigestHeadline`,
// under a comment that called itself "a floor, not a design". Every word on that card
// was true and none of it was laid out: no count as a field, no window boundaries, and
// a `Status` field reading `:grey_question: Expired — oto stopped hearing about this`,
// which is what `DeriveCardState` says about a view whose member counts are all zero.
// The message that stands in for the most was the least designed one oto sends.
//
// ⭐ ONE LAYOUT FOR BOTH MODES, AND THE MODE IS DELIBERATELY NOT READ. §H.6 does not
// apply to a digest — `notification/service.digestModes` is the whole of its mode
// rule, and it is two lines: OPEN THE CONVERSATION ONCE, THEN REPLY TO IT ONCE PER
// WINDOW. So the first window arrives as `post_root` and every later one as
// `thread_reply`, and they are the SAME FACT in the same shape: window N's summary is
// not a comment on window N−1's card, it is the next entry in a series. Sending the
// later windows through `renderReply` because their mode says `thread_reply` would
// have drawn every window after the first as a one-line `:information_source:` note,
// which is a worse version of the card this ticket closed.
// `update_root` never happens (amending would overwrite last window's summary with
// this one's), so nothing here has to answer "what changed".
//
// ⛔ NO ACTIONS AND NO LINKS, BOTH DECIDED RATHER THAN DEFERRED. The reasoning is
// recorded once, at the seam that owns the decision — see `ViewService.digest` — and
// the card's own consequence is `digestWhereToLook`, which says what to look at since
// there is nothing to click.
//
// ⛔ AND NO METADATA. `rootMetadata` is group-shaped (`group_id`, `generation`) and a
// digest has neither; its purpose is tracing an INTERACTION back to its delivery, and
// a card with no interactive element can never produce one. An `event_payload` of
// nulls would be worse than the absence.
func (r *Renderer) renderDigest(
	v *domain.NotificationView, o domain.RenderOptions,
) (Payload, string) {
	d := *v.Digest
	// ⚠️ THE SHARED NONCE, ON A VIEW WHOSE GROUP FIELDS ARE ALL ZERO. `renderNonce`
	// hashes the group's id, generation and member counts, and a digest has none — so
	// what actually varies here is `RenderedAt`, which is claim time and therefore
	// distinct for every window of every policy. That is enough for what a block_id is
	// for (S12: a fresh id per message iteration, and a digest is never iterated) and
	// for what the goldens need (the same fact must hash the same twice). It is NOT
	// enough to distinguish two policies' digests claimed inside the same second, and
	// nothing requires it to be: block ids are scoped to a message, and this card has
	// no interactive element whose id anything will ever route on.
	nonce := renderNonce(v, o)
	now := r.renderedAt(v)

	// ⚠️ NOTHING ON THIS CARD IS UPSTREAM TEXT, WHICH IS WHY NOTHING HERE IS
	// `escape`d. Every string is oto's own: a count, two timestamps and fixed prose.
	// The first upstream value added to it — a policy NAME is the obvious candidate —
	// has to go through `escape`, because it is operator-supplied and reaches a
	// message.
	blocks := make([]Block, 0, 4)
	blocks = append(blocks, sectionBlock(blockID("digest", nonce),
		truncateSection(digestHead(), "")))
	blocks = append(blocks, fieldsBlock(blockID("digestfacts", nonce), digestFields(d)))
	blocks = append(blocks, contextBlock(blockID("digestlook", nonce),
		Text{Type: TypeMrkdwn, Text: truncateField(digestWhereToLook(d), "")}))
	blocks = append(blocks, contextBlock(blockID("digestfooter", nonce),
		Text{Type: TypeMrkdwn, Text: truncateField(digestFooter(o, now), "")}))

	text := digestText(d, now)

	return Payload{
		Text:        text,
		UnfurlLinks: false,
		UnfurlMedia: false,
		Attachments: []Attachment{{
			Color: neutralColour(),
			// V1 permits exactly one attachment and V2 refuses a colour Slack cannot
			// parse, so a digest carries a bar like everything else; `neutralColour`
			// is where the choice of WHICH bar is argued.
			Fallback: truncateRunes(text, 200),
			Blocks:   blocks,
		}},
	}, text
}

// digestHead names the card in two words and then says what kind of thing it is.
//
// ⭐ THE SECOND LINE IS NOT DECORATION. A reader scrolling a channel has to be able to
// tell this card from a Case card at a glance, and the two cues that do it are the
// leading chart and the sentence that says the card will never change: every other
// message oto puts in a channel is amended in place for its whole life (ADR 0008),
// and a digest is the one that is written once. Somebody who does not know that waits
// for it to update.
func digestHead() string {
	return digestEmoji + " *Digest*\n" +
		"_One summary per closed window — not a live signal, and not a card that will change._"
}

// digestFields lays the digest out as FACTS, which is the whole of this ticket: a
// count and a span in the scannable §H.7 grid rather than a sentence in a title.
//
// Slack fills a fields block left to right in two columns, so the order pairs the
// count with the length of what it counted, and then the two ends of the span
// underneath: `[New cases | Span]` / `[From | Up to]`.
func digestFields(d domain.DigestView) []Text {
	fields := make([]Text, 0, maxFields)
	add := func(label, value string) {
		if len(fields) >= maxFields || strings.TrimSpace(value) == "" {
			return
		}
		fields = append(fields, Text{
			Type: TypeMrkdwn,
			Text: truncateField("*"+label+"*\n"+value, ""),
		})
	}

	// The number the digest asserts. It is at least 1 on anything oto sends — a
	// window below its policy's floor writes no row — so this field is never a zero
	// dressed up as news.
	add("New cases", strconv.Itoa(d.Count))

	if d.CoveredFrom.IsZero() || d.CoveredTo.IsZero() {
		// ⛔ THE ABSENCE IS A FIELD RATHER THAN A GAP, and it is the honest half of
		// migration 00070. A digest written before the span columns existed cannot be
		// given one: the only way to invent it is the window start times the policy's
		// window AS IT IS TODAY, and an operator who has since narrowed that window
		// would be shown a span no digest ever covered (git-bug `342e071`). A card
		// that says it does not know is a card an operator can still trust.
		add("Covers", "not recorded for this digest")
		return fields
	}

	// ⚠️ THE SPAN IS HALF-OPEN AND THE LABELS SAY SO. `CoveredTo` is the EXCLUSIVE
	// end (`notifications_digcover_ck`, and the column comment on
	// `digest_covered_to`), so the second boundary is "Up to" and never "To": the
	// Case that opened at exactly that instant is in the NEXT digest, and a card
	// that read as closed on both ends would have every reader double-counting one
	// boundary per window.
	//
	// ⭐ THE LENGTH IS SUBTRACTED FROM THE TWO ENDS AND NEVER READ FROM A POLICY.
	// That is the point of storing both: the span cannot be falsified by a later
	// edit to `digest_window_s`. It is also why it is usually LONGER than the
	// policy's window — the lookback reaches back for stragglers that committed too
	// late for the previous window's read, so "since the last digest, plus
	// stragglers" is the truth this arithmetic prints.
	add("Span", humanDuration(d.CoveredTo.Sub(d.CoveredFrom)))
	// `slackDateTime` and not `slackDate`: a digest's span can be older than the day
	// it is read on — a recovered tick emits up to `MaxDigestBackfill` windows at
	// once, and a long window is admissible — so a bare clock time would be
	// ambiguous in exactly the case a reader is least able to resolve it.
	add("From", slackDateTime(d.CoveredFrom))
	add("Up to", slackDateTime(d.CoveredTo))

	return fields
}

// digestWhereToLook is what a card with nothing to click owes its reader.
//
// ⭐ IT IS THE VISIBLE HALF OF THE "NO LINKS" RULING (git-bug `78388fb`, and see
// `ViewService.digest` for the two reasons). A card that simply had no link would
// leave an operator with a number and no next step; saying WHY there is no link, in
// one line, is what turns a missing affordance into an answered question. The moment
// the list grows an upper time bound and a policy filter, this line is replaced by a
// button and the reasoning above it is what proves the swap is allowed.
func digestWhereToLook(d domain.DigestView) string {
	out := ":mag: No link — a digest's set is a policy's matcher over a span, " +
		"and neither has a URL. Look at *Cases* in oto"
	// ⛔ IT MUST NOT POINT AT SOMETHING THE CARD DID NOT PRINT. On a digest whose span
	// was never recorded there is no window above to narrow to, and telling a reader
	// to use one is a smaller version of the same defect as a wrong link.
	if !d.CoveredFrom.IsZero() && !d.CoveredTo.IsZero() {
		out += ", narrowed to the window above"
	}
	return out + "."
}

// digestFooter is the provenance line: what this is and when it was written.
//
// ⛔ IT SAYS "POSTED" WHERE THE ROOT CARD SAYS "UPDATED", because the root card's
// footer exists to prove an amended message is current and a digest is never amended.
// "Updated" on a message that will never be touched again teaches a reader to expect
// something that is not coming.
func digestFooter(o domain.RenderOptions, now time.Time) string {
	parts := []string{"oto", "_digest_", "posted " + slackDate(now)}
	// ⚠️ §H.9 REACHES A DIGEST THREAD FASTER THAN ANY OTHER, which is why this flag
	// is honoured on a card that has no history to continue. One reply per window
	// means a ten-minute policy hits the 30-reply ceiling in five hours, so the
	// fresh-root-and-say-so path is ORDINARY here rather than exceptional; a
	// continued digest that did not say so would read as a policy that had started
	// over.
	if o.Continued {
		parts = append(parts, "_continued from an earlier card_")
	}
	return strings.Join(parts, "  ·  ")
}

// digestText is the digest's top-level `text`: the push notification, the sidebar
// preview, the search snippet and the whole of what a screen reader announces (S5).
//
// It is written by hand rather than derived from the blocks, for the reason `rootText`
// is: it must be one complete sentence, it must carry no `<!date>` token — which does
// not render in a push notification — and it is bounded at 300 characters because a
// longer one is truncated by the operating system anyway.
//
// ⭐ IT IS ALSO THE `rendered_fallback` THAT MADE THE OLD DESIGN NECESSARY. The
// headline rode `Group.Title` because an empty title produced an empty fallback and
// failed `deliveries_fb_ck` with a 23514 after the message had already gone to Slack.
// The constraint was real and it is now met where it belongs: the renderer writes the
// sentence, so the view does not have to carry one.
func digestText(d domain.DigestView, now time.Time) string {
	var b strings.Builder
	b.WriteString(digestEmoji)
	// `[DIGEST]` is the bracketed word `CardState.Banner()` puts on every other card,
	// and it is deliberately not one of the five: a reader who has learned to scan
	// for `[FIRING]` must not read this as a state.
	b.WriteString(" [DIGEST] ")
	b.WriteString(plural(d.Count, "new case", "new cases"))

	if !d.CoveredFrom.IsZero() && !d.CoveredTo.IsZero() {
		b.WriteString(" in ")
		b.WriteString(humanDuration(d.CoveredTo.Sub(d.CoveredFrom)))
		// `plainMoment` and not `plainClock`: the span may not be on the reader's own
		// day, and "from 17:56 UTC" about last Tuesday is a false statement to the
		// one person reading it.
		b.WriteString(", from ")
		b.WriteString(plainMoment(d.CoveredFrom, now))
		b.WriteString(" up to ")
		b.WriteString(plainMoment(d.CoveredTo, now))
	} else {
		b.WriteString(" in a window whose span was not recorded")
	}

	return truncateClause(oneLine(endSentence(b.String())), otoTopLevelText)
}
