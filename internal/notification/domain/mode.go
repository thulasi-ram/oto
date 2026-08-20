package domain

// Mode is HOW one fact reaches one Channel — the closed set of
// `notification_deliveries.mode` (deliveries_mode_ck).
//
// It is the decision produced by the §H.6 table and it is the most
// consequential decision in oto. `chat.update` is Tier 3 (50/min) while
// `chat.postMessage` is roughly one message per second per channel: updating in
// place is about fifty times cheaper AND it is what an operator wants, because a
// card that edits itself has one truth rather than a scrollback of stale ones.
type Mode string

// The delivery modes.
const (
	// ModePostRoot posts a new root message. It happens once per THREAD, and a
	// thread is keyed by its SUBJECT — `channel_threads.subject_kind`, which since
	// migration 00056 can be an alert, a case or a group and which v1 always
	// spells as the AlertGroup generation. It is the ONLY genuinely at-risk
	// operation on recovery (§G.5).
	ModePostRoot Mode = "post_root"
	// ModeUpdateRoot amends the root in place. This is the PRIMARY mechanism.
	ModeUpdateRoot Mode = "update_root"
	// ModeThreadReply appends a reply under the root.
	ModeThreadReply Mode = "thread_reply"

	// ⛔ `ModeBroadcastReply` ("broadcast_reply") WAS HERE AND IS DELETED, WITH THE
	// WHOLE MECHANISM BEHIND IT (git-bug `7570090`). It was Slack's `reply_broadcast`,
	// the API form of the "Also send to #channel" checkbox: a thread reply that also
	// surfaces once in the channel. Nothing in oto produces it any more — read the ⛔⭐
	// block in `PlanFor` below for WHY the mechanism reached nobody, which is the part
	// worth carrying forward.
	//
	// ⛔ IT IS DELETED FROM THE MODE SET, NOT LEFT DECODABLE, and the reason is the
	// same one `reason.go` gives for the plurality Reasons: `api/openapi/openapi.yaml`
	// has already dropped it from `DeliveryMode`, oto is unreleased, and the database
	// is being reset — so no row, no client and no renderer can ever meet the value.
	// A mode kept readable for a reader that cannot exist is a value the next person
	// has to rule out. `Valid` refusing it is the point, not a regression.
)

// Valid reports whether m is in the closed set.
func (m Mode) Valid() bool {
	switch m {
	case ModePostRoot, ModeUpdateRoot, ModeThreadReply:
		return true
	default:
		return false
	}
}

// NeedsRoot reports whether this mode has nothing to attach to until a root
// message exists. It is the flag the per-thread ordering gate consumes.
func (m Mode) NeedsRoot() bool { return m != ModePostRoot }

// IsReply reports whether this mode appends to a thread rather than touching the
// root. Its ONE caller-facing job is the reply-ordering gate.
//
// ⛔ IT USED TO READ `m == ModeThreadReply || m == ModeBroadcastReply`, and it
// existed BECAUSE there were two reply modes: every caller asking "does this append
// to the thread?" had to remember both, and forgetting the broadcast half was a
// silent bug — a broadcast that skipped the reply-ordering gate would land out of
// order. There is one reply mode now (git-bug `7570090`), so the predicate is a
// single comparison.
//
// ⭐ IT IS KEPT RATHER THAN INLINED. `m == ModeThreadReply` at three call sites is
// three places to edit when a second reply mode returns, and the last time there
// were two the cost of forgetting one was silent misordering.
func (m Mode) IsReply() bool { return m == ModeThreadReply }

// Capability mirrors `channels.capabilities`, a persisted bitmask. The bit
// positions are a STORED WIRE CONTRACT shared with the channels port: renumbering
// one silently re-labels every configured channel in the database.
type Capability uint32

// The capability bits, in the channels port's declared order.
const (
	// CapThreading means replies attach to a parent message.
	CapThreading Capability = 1 << iota
	// CapAmend means an already-sent message can be edited in place.
	CapAmend
	// CapRichLayout means structured blocks rather than plain text.
	CapRichLayout
	// CapInteractive means buttons that call back into oto.
	CapInteractive
	// ⛔ `CapBroadcast` WAS BIT 5 AND IS DELETED (git-bug 7570090). The position is
	// HELD, not reclaimed: `channels.capabilities` is a persisted bitmask and
	// renumbering would silently re-label every configured channel in the database.
	_
	// CapDedupeKey means the provider does its own dedupe.
	CapDedupeKey
)

// Has reports whether every bit in want is set.
func (c Capability) Has(want Capability) bool { return c&want == want }

// rootModeFor is the §H.6 table's root column: what this Reason does to the root
// card. An empty Mode means the Reason touches no root at all.
func rootModeFor(r Reason, threadExists bool) Mode {
	switch r {
	case ReasonComment:
		// It does not change the signal's state, so it has nothing to say to the
		// root card. Rewriting the card to announce a comment would make the card's
		// `updated` timestamp lie about when the SIGNAL last changed.
		// (`unacked_reminder` was the other arm — git-bug bd0fb1d.)
		return ""

	default:
		// `first notification` posts the root — and so does anything else that
		// arrives before a root has landed. There is no such thing as amending a
		// card that was never posted, and a `new alerts added` on a destination
		// that never saw the original is that destination's first notification.
		if threadExists {
			return ModeUpdateRoot
		}
		return ModePostRoot
	}
}

// TouchesRoot reports whether this Reason has anything to say to the root card.
//
// It is what the dispatcher consults to decide whether a delivery whose primary
// mode is a REPLY must also amend the root in the same claim — the other half of
// §H.6's `update_root + thread_reply` rows, which the one-delivery-per-channel
// constraint cannot express as a second row.
func (r Reason) TouchesRoot() bool { return rootModeFor(r, true) != "" }

// hasReply is the §H.6 table's reply column: whether this Reason has a reply
// type at all, before any verbosity or capability gating.
func hasReply(r Reason) bool {
	switch r {
	case ReasonFired, ReasonRepeat, ReasonUnacked:
		// `repeat interval elapsed` is the important one. It UPDATES AND NEVER
		// POSTS — that single rule is the largest noise reduction available to
		// oto, and it is exactly what stock Alertmanager and Grafana get wrong.
		//
		// ⛔ `some_resolved` WAS THE FOURTH ARM AND THE REASON IS DELETED (git-bug
		// `7570090`). ⚠️ ITS REMOVAL IS A BEHAVIOUR CHANGE, NOT A TIDY-UP: §H.6 made
		// `some_resolved` update-only — one member of forty going quiet did not earn a
		// reply — and the fact it named is now minted as `all_resolved`, which is NOT in
		// this list and DOES earn one. So a single alert resolving now produces a thread
		// reply where it produced only a root amend before. That is the ruling, not a
		// leak: with one Alert per Case, "one of them stopped" and "the whole thing is
		// over" are the same event, and the second is the one worth a reply.
		return false
	default:
		return true
	}
}

// PlanInput is everything the §H.6 decision needs. It is a value, so the whole
// table is exercisable without a database.
type PlanInput struct {
	Reason Reason
	// Verbosity is the destination Channel's setting.
	Verbosity Verbosity
	// ThreadUpdates is `channels.thread_updates`. False reduces every mode to
	// update_root.
	//
	// ⛔ THERE USED TO BE ONE EXCEPTION — the unacked reminder, always a broadcast,
	// because a reminder nobody sees is not a reminder. The reminder is gone
	// (git-bug bd0fb1d) and so is the exception: this field now means exactly what
	// it says, for every Reason.
	ThreadUpdates bool
	// Capabilities is the destination's negotiated bitmask. Negotiation happens
	// HERE, centrally, and never inside a provider (§H.10).
	Capabilities Capability
	// ThreadExists reports that a live ChannelThread already binds this
	// notification's THREAD SUBJECT — the AlertGroup generation, whatever
	// `Reason.Subject()` says the fact is about — to this channel.
	ThreadExists bool
	// ⛔ `StormMode` WAS HERE AND IS DELETED, AND UNLIKE `Flapping` BELOW IT DID NOT
	// EVEN SURVIVE AS A FIELD. It collapsed a generation to one root card and dropped
	// every per-alert reply; the caller no longer has an answer to give, because
	// nothing evaluates a storm any more. Where `Flapping` was kept because
	// `alerts.is_flapping` is a STORED verdict a card still reports, storm's damper
	// had no other layer to move to — the object that should own a storm is an
	// incident, and incidents do not exist yet. There is nothing here to keep, and
	// since migration 00059 there is no `alert_groups.storm_mode` either.
	//
	// Flapping is RETIRED AND READ BY NOTHING HERE. It is `alerts.is_flapping`, and
	// it used to switch the group to update-only with a coalesced digest reply owned
	// by the flap damper.
	//
	// ⭐⭐ THE FLAP DAMPER MOVED OUT OF THE NOTIFICATION LAYER, and that is the whole
	// of migration 00057. Damping at DELIVERY makes a withheld notification
	// indistinguishable from a signal that never fired — the one thing an alerting
	// product cannot afford (§B.6) — and it was only ever needed because a flapping
	// alert produced one CASE per flap. The case retention window W removes the
	// cause: a re-fire inside W lands in the still-open case, so there is one case,
	// one root card and one thread reply, and there is no per-transition noise left
	// for a digest to coalesce.
	//
	// ⛔ THE FIELD SURVIVES ON PURPOSE AND MUST NOT ACQUIRE A NEW MEANING.
	// `notification/service.notify` still sets it from the snapshot
	// (`service/notify.go:113`), `PlanFor` ignores it, and a caller reading a plan
	// gets the same modes whether it is true or false. Giving it a second job would
	// put a damper back at the layer that cannot account for its own silence.
	//
	// ⛔ AND IT IS NO LONGER A LIVE FACT. `alerts.is_flapping` and `alerts.flap_score`
	// are RETIRED IN PLACE (SPEC §B.6.2, ADR 0041 Amendment 1): the `flap.score` job
	// and `AlertRepository.SetFlap` — the only statement in the tree that ever wrote
	// either column — are deleted, so both columns keep the last value they were
	// given and nothing recomputes them. They stay READABLE, and the readers are
	// real: `?flapping=`, the alert rollup's `flapping` counter, the `alert.history`
	// enrichment, this snapshot and the Slack card's `Flapping` field. What a reader
	// gets is a MEASUREMENT TAKEN AT A TIME, not a current judgement — the detector
	// was retired because the case retention window W (00057) made it report `false`
	// exactly when an alert was flapping hardest, and a detector that lies is worse
	// than no detector. So the words this field can support are "the last stored
	// verdict", never "is flapping now".
	//
	//oto:retired the flap detector was retired in place — `flap.score` and
	// `AlertRepository.SetFlap` are deleted and `PlanFor` deliberately ignores this
	// field, so it has no production reader BY DESIGN. This is not debt and not
	// analyzer blindness: the two paragraphs above are the argument for keeping the
	// declaration while its last reader is gone. `reachable-ok` would be the wrong
	// marker here — it claims a route exists, and the whole point is that none does.
	Flapping bool
	// ⛔ `Broadcast BroadcastPolicy` WAS HERE AND IS DELETED WITH THE MECHANISM IT
	// CONFIGURED (git-bug `7570090`). It was the org's policy over which transitions
	// surface in the channel (ADR 0020), and its zero value was the approved default
	// set — which is the tell: the default set was "broadcast nothing unless the org
	// opts `all_resolved` in", so the field's job was to carry an opt-in almost nobody
	// took. `broadcast_on_resolved` goes with it, and so does the `Warrants` call in
	// `PlanFor` below.
	// ⛔ `ChannelNoticeClaimed` WAS HERE AND IS DELETED WITH THE LATCH IT REPORTED
	// (`channels.storm_notice_at`). It said "this destination won the once-per-channel
	// right to announce that oto had started withholding" — a field that exists only
	// because oto withheld. `channels.storm_notice_at` is dropped by migration 00059;
	// there is no latch left to claim.
}

// Plan is the decision: which modes this fact produces on this destination, and
// why any reply was dropped.
//
// ⭐ EVERY MODE IN `Modes` GETS ITS OWN DELIVERY ROW. `deliveries_fanout_uniq` is
// UNIQUE on (notification_id, channel_id, MODE) since migration 00024, precisely
// so §H.6's `update_root` PLUS `thread_reply` can both exist. It did not always
// work that way: the root amend used to ride along inside the reply's claim
// writing no row at all, and a failed amend then vanished with no retry and no
// dead-letter while the card stayed stale forever. `Modes` is the whole answer
// now — there is no primary mode and no passenger.
type Plan struct {
	// Modes are the modes this fact produces, root first, reply second. Empty
	// means this fact produces nothing on this destination.
	Modes []Mode
	// ReplyDropped reports that the Reason has a reply which this destination
	// will not receive. It is not an error and not a suppression of the
	// NOTIFICATION — the root update still carries the same facts (§H.10).
	ReplyDropped bool
	// ReplyDropReason is a short, stable label for why: "verbosity",
	// "thread_updates", "no_threading", "fresh_root".
	//
	// ⛔ NEITHER `flapping` NOR `storm` IS ONE OF THEM ANY MORE, and neither may come
	// back. The flap damper moved from delivery to case formation (migration 00057);
	// storm damping was deleted outright, because the object that should own a storm
	// is an incident and incidents do not exist yet. Neither is DECODABLE any more
	// either: migrations 00059 and 00060 narrow `notifications_suppmap_ck` and
	// `notifications_reason_ck` with no backfill, so the values are deleted rather
	// than retired — unlike `case.reopened`, which `alerts/domain.retiredEventTypes`
	// still keeps readable because `ev_type_ck` still admits it.
	//
	// ⭐ WHAT IS LEFT IS NOT OTO'S OPINION. Every label above is a fact about the
	// DESTINATION — a human's volume setting, a channel switch, a missing capability
	// — or "the root card is being posted fresh and already says this". None of them
	// is oto judging a firing not worth mentioning.
	ReplyDropReason string
	// ⛔ `BroadcastDamped bool` AND `BroadcastDampReason string` WERE HERE AND ARE
	// DELETED (git-bug `7570090`). They reported that a transition WARRANTED a
	// broadcast and got a quiet reply instead — not an error and not a suppression,
	// because the fact still landed on the thread, but recorded anyway, since a damped
	// broadcast is invisible by construction and "oto decided not to shout" is exactly
	// the kind of decision §B.6 refuses to take silently.
	//
	// ⭐ THE PRINCIPLE THEY SERVED OUTLIVES THEM AND IS WHAT `ReplyDropReason` ABOVE IS
	// FOR: any narrowing oto applies to its own output has to be reportable. What died
	// is the thing being narrowed. By the end there was exactly one damp reason,
	// `no_capability` — the destination could not surface a reply in-channel, the
	// world's constraint rather than oto's opinion — and with no broadcast to damp,
	// nothing can set either field. A field nothing can write is the dead shape
	// `reason.go` refuses for a Reason, and a struct field is no different.
	//
	// ⚠️ THEIR LAST READER IS THE `p.BroadcastDamped` log line in
	// `notification/service.notify`, which goes with them.
}

// Empty reports that this destination gets nothing.
func (p Plan) Empty() bool { return len(p.Modes) == 0 }

// PrimaryMode is the ONE mode that still stands for this plan when the modes
// have to be re-derived and the second derivation disagrees with the first: the
// reply when there is one, otherwise the root touch. The reply is chosen because
// it is the message that is NEW — the amend is an edit of something already
// recorded on the thread.
//
// Its ONE caller is `NotificationService.modesFor`, which re-applies the §H.6
// table once the thread is known. If that second pass says "nothing", the first
// pass has already promised this destination something, and silently delivering
// nothing is the worse failure — so the primary mode is what gets sent.
//
// ⛔ It is NOT "the mode the delivery row records". Since migration 00024 every
// mode gets its own row; a caller reaching for this to pick one is reintroducing
// the amend-with-no-row bug that migration exists to fix.
//
// (`RootRefresh`, its counterpart, was deleted: it named the passenger half of a
// pairing that no longer exists.)
func (p Plan) PrimaryMode() Mode {
	if len(p.Modes) == 0 {
		return ""
	}
	return p.Modes[len(p.Modes)-1]
}

// PlanFor applies the §H.6 decision table plus the §H.10 capability negotiation.
//
// Order matters and is deliberate:
//
//  1. the root mode is chosen, then degraded if the destination cannot amend —
//     a channel with no edit-in-place gets a fresh standalone message rather
//     than nothing at all;
//  2. the reply is chosen, then gated by thread_updates, verbosity and threading
//     capability, in that order.
//
// ⛔ THERE USED TO BE A STEP AHEAD OF BOTH — "the reminder is decided first,
// because it is the one mode `thread_updates` may not reduce" — and it is deleted
// (git-bug `bd0fb1d`). It is called out rather than silently renumbered because it
// was the ONLY step that could not be reduced, so a reader who remembers a
// three-step order needs to know which one left: with it gone, `thread_updates`
// now means exactly what it says for every Reason, and this function has no
// unreducible mode at all. The ⛔ note further down, on the deleted `flapping` and
// `storm` gates, is the same class of correction.
//
// ⛔ TWO GATES USED TO STAND AHEAD OF THOSE THREE AND BOTH ARE DELETED. `flapping`
// went with migration 00057 — the flap damper is the case retention window now, not
// a withheld reply — and `storm` went with storm damping itself. Every gate that
// remains is a fact about the DESTINATION: what a human set, what a channel switch
// says, what the provider can do. NOT ONE OF THEM IS OTO'S JUDGEMENT ABOUT THE
// SIGNAL, and that is the property to preserve when the next gate is proposed here:
// a gate oto decides for itself makes a withheld notification indistinguishable
// from a signal that never fired (§B.6).
//
// Nothing here reads a provider type. A destination is described entirely by its
// capability bits and its two channel-level switches, which is what keeps this
// table honest when a second provider appears.
func PlanFor(in PlanInput) Plan {
	if !in.Reason.Valid() {
		return Plan{}
	}

	// ⛔ THE ONE REMINDER STAGE WAS PLANNED HERE AND IS DELETED (git-bug bd0fb1d).
	// It was a broadcast reply that `thread_updates=false` did not reduce — a
	// reminder nobody sees is not a reminder — degrading to a plain reply where the
	// destination could not broadcast and to a root update where it could not
	// thread. The owner withdrew the feature: oto sends nothing unprompted.
	//
	// ⭐ THE HISTORY IS WORTH KEEPING BECAUSE IT IS A DAMPER ARGUMENT, NOT A REMINDER
	// ARGUMENT. This branch once returned an UNCONDITIONAL broadcast; a storm across
	// two hundred unacknowledged alerts produced two hundred `chat.postMessage`
	// calls into one channel, so a storm gate was added and the reminder degraded to
	// a quiet thread reply. Both the flood and the gate are gone — nothing evaluates
	// a storm, and the case retention window keeps one case per flapping alert, so
	// the volume the gate existed for is removed at its SOURCE rather than at
	// delivery. That lesson outlives the reminder: the fix for a flood is upstream
	// of the send, never a damper on it.

	var p Plan

	// ---- root ------------------------------------------------------------
	root := rootModeFor(in.Reason, in.ThreadExists)
	if root == ModeUpdateRoot && !in.Capabilities.Has(CapAmend) {
		// §H.10: a state change on a non-amendable channel becomes a fresh
		// standalone message. Silence would be worse: the destination would
		// simply never learn the alert resolved.
		root = ModePostRoot
	}
	if root != "" {
		p.Modes = append(p.Modes, root)
	}

	// ---- reply -----------------------------------------------------------
	if !hasReply(in.Reason) {
		return p
	}

	drop := func(reason string) Plan {
		p.ReplyDropped, p.ReplyDropReason = true, reason
		return p
	}

	switch {
	case root == ModePostRoot:
		// The root is being posted fresh. A reply alongside it would be two
		// messages for one fact, and the fresh card already carries every field
		// the reply would have restated.
		return drop("fresh_root")

	// ⛔ THE STORM GATE WAS HERE AND IS DELETED. It read
	// `in.StormMode && in.Reason != ReasonStorm` and dropped the reply, because a
	// generation over the storm threshold was supposed to collapse to ONE root card
	// with a count and a link. The collapse was oto deciding that many real firings
	// were not worth mentioning individually, and the `storm` announcement it let
	// through was oto describing its own silence rather than the signal. A storm is
	// many DIFFERENT alerts arriving together; the object that owns that is an
	// INCIDENT (`correlation`, DEFERRED-POST-V1), and until that object exists there
	// is nowhere honest to put the fact — so the detection is removed, not moved.
	//
	// ⛔ DO NOT REINSTATE IT HERE WHEN INCIDENTS ARRIVE. The notification layer
	// cannot account for its own silence, which is the whole defect: a storm
	// notification belongs ON the incident.

	// ⛔ THE FLAPPING GATE WAS HERE AND IS DELETED (migration 00057). It read
	// `in.Flapping && in.Reason != ReasonRuleChanged` and dropped the reply, because
	// a flapping alert was supposed to get one coalesced digest instead of a reply
	// per transition. The transitions it was coalescing no longer happen: the case
	// retention window keeps ONE case open across a flap, so a flap now produces one
	// root card and one reply on its own. A gate here would drop that ONE reply and
	// leave the flap invisible in the thread — a damper firing after the thing it
	// damps has already been removed.
	//
	// ⛔ DO NOT REINSTATE IT WITHOUT DELETING W FIRST. Two dampers over one fact is
	// how oto arrives at silence it cannot account for, which is what §B.6 forbids.

	case !in.ThreadUpdates:
		// The destination asked for update-in-place only.
		return drop("thread_updates")

	case !in.Verbosity.AllowsReply(in.Reason):
		return drop("verbosity")

	case !in.Capabilities.Has(CapThreading):
		// §H.10: a reply on a non-threading channel is suppressed by default,
		// because the root update it travels with carries the same facts.
		return drop("no_threading")
	}

	// ---- the reply, and the broadcast decision that used to follow it ----
	//
	// ⛔⛔ THE BROADCAST STEP WAS HERE AND THE MECHANISM IS DELETED FROM OTO ENTIRELY
	// (git-bug `7570090`). It read `in.Broadcast.Warrants(in.Reason)` and, on a
	// destination carrying `CapBroadcast`, promoted the surviving thread reply to
	// `broadcast_reply` — Slack's `reply_broadcast`, the API form of "Also send to
	// #channel". `broadcast.go` held the whole argument and is gone with it; this note
	// is the part a reader still needs, because the PREMISE of that argument is still
	// true and the conclusion no longer is.
	//
	// ⭐ THE PREMISE, WHICH STILL HOLDS. ADR 0008 made `chat.update` the primary verb
	// and bought its quiet with a property nobody wrote down: **`chat.update` is
	// completely silent** — no notification, no unread badge, no bump in the channel
	// list. A card can go from `warning` to `critical` and every person in the channel
	// can miss it. Thread replies have the same shape: they notify thread participants
	// and nobody else. `reply_broadcast` was Slack's answer, and it was the ONLY
	// mechanism oto had for channel-level urgency. ⚠️ OTO NOW HAS NONE. That is a
	// product gap, not a solved problem, and anyone reaching for one should read the
	// three constraints below first — they are why the answer was never "broadcast
	// more".
	//
	// ⛔ THE THREE BINDING PROPERTIES OF A BROADCAST, ALL COUNTER-INTUITIVE, ALL STILL
	// TRUE OF SLACK:
	//
	//  1. THE CHANNEL-VISIBLE ARTEFACT IS A STRIPPED REFERENCE, NOT A COPY. Slack
	//     delivers a `thread_broadcast` subtype that is "a pointer or reference to the
	//     actual thread", and "the reference cannot contain attachments or message
	//     buttons". §H.1 S3 puts ALL of oto's blocks inside exactly one attachment,
	//     because that attachment is the only way to get the colour bar — so the
	//     in-channel form had NO COLOUR BAR AND NO BUTTONS, and its top-level `text`
	//     had to carry the severity in words and never lean on a colour or an
	//     Acknowledge button.
	//  2. BROADCASTING IS IRREVERSIBLE. Nothing un-broadcasts. The bar was therefore
	//     "would an on-call engineer be angry to have MISSED this?", never "is this
	//     interesting?" — a channel that learns to scroll past oto has lost the only
	//     mechanism oto had for genuine urgency.
	//  3. A BROADCAST IS A `chat.postMessage` AND AN UPDATE IS NOT. Posts are what the
	//     ~1 message/second/channel budget constrains; `chat.update` is Tier 3 and
	//     effectively free. Choosing to broadcast moved a fact from the cheap verb to
	//     the expensive one, which is why the set was tiny and nothing in it was
	//     per-alert.
	//
	// ⭐⭐ AND WHY IT GOES: BY THE END THE MECHANISM WAS ALREADY REACHING NOBODY. The
	// set was four Reasons, then a revised ADR 0020 cut it to the two whose quiet form
	// is genuinely INVISIBLE, then `unacked_reminder` left with git-bug bd0fb1d — its
	// audience was people who had NOT engaged and in-thread it reached only the
	// already-engaged, which is the wrong audience, and the owner withdrew the feature
	// because oto sends nothing unprompted. That left `refired` carrying the default
	// set alone, and ADR 0040 retired T8: a re-fire now always opens a NEW episode, so
	// nothing has produced a `refired` since. The only other member was `all_resolved`,
	// behind `broadcast_on_resolved`, OPT-IN AND DEFAULT OFF — closure is welcome on a
	// quiet channel and doubles traffic on a busy one, and nobody was ever woken
	// because a resolve arrived quietly. ⛔ SO ON DEFAULT SETTINGS `Warrants` COULD
	// RETURN TRUE FOR NOTHING AT ALL: one member had no producer, the other had no
	// subscriber. Deleting it removes no delivery any operator was receiving.
	//
	// ⛔ WHAT MUST NOT COME BACK AS A SUBSTITUTE. `storm` was removed from the set
	// before the set was: a storm means MANY alerts, so a per-thread broadcast of "oto
	// has gone quiet" produced exactly the flood the damping existed to prevent — oto
	// shouting, once per group, about having started to be quiet. It was replaced by a
	// once-per-channel latched notice, and both went with storm damping itself, because
	// a product that withholds nothing has nothing to announce. ⛔ AND A PER-REASON
	// DIAL IS NOT THE ANSWER EITHER: the policy was deliberately ONE boolean, because a
	// dial per Reason makes broadcast the thing each team negotiates rather than the
	// thing oto is confident about, which is how a channel ends up scrolling past oto.
	// ⭐ IF CHANNEL-LEVEL URGENCY IS REBUILT, THE HARD PART IS FINDING A TRANSITION
	// WITH A LIVE PRODUCER WHOSE QUIET FORM IS INVISIBLE — that is the requirement the
	// old set failed, and the capability bit is deliberately still reserved above so
	// the answer does not have to renumber a stored contract to arrive.
	//
	// A Reason with a reply but no root — `comment` — still needs the reply, and a
	// destination that cannot amend has already had its root promoted above.
	p.Modes = append(p.Modes, ModeThreadReply)
	return p
}

// ⛔ `dampReason` WAS HERE AND IS DELETED, BECAUSE THERE ARE NO DAMPERS LEFT TO
// NAME. It existed so the reminder branch — which runs BEFORE the ordinary gates,
// since `thread_updates` may not silence a reminder — asked the same question those
// gates asked, in the same order. It had two arms, `storm` and `flapping`, ordered
// on §B.8.2's reasoning that a storm is the louder fact about oto's own behaviour.
// `flapping` went with migration 00057, `storm` went with storm damping, and a
// zero-arm switch is not a function.
//
// ⛔ A REPLACEMENT WOULD BE THE DEFECT COMING BACK. If a future gate ever needs to
// exist in both places, it must be a fact about the DESTINATION — a capability, a
// switch, a human's setting. The moment oto is the one deciding, a suppressed
// notification stops being distinguishable from a signal that never fired (§B.6).
