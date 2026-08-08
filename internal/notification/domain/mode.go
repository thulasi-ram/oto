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
	// ModePostRoot posts a new root message. It happens once per AlertGroup
	// generation and is the ONLY genuinely at-risk operation on recovery (§G.5).
	ModePostRoot Mode = "post_root"
	// ModeUpdateRoot amends the root in place. This is the PRIMARY mechanism.
	ModeUpdateRoot Mode = "update_root"
	// ModeThreadReply appends a reply under the root.
	ModeThreadReply Mode = "thread_reply"
	// ModeBroadcastReply surfaces a thread reply in-channel — Slack's
	// `reply_broadcast`, the API form of the "Also send to #channel" checkbox.
	//
	// It is decided by BroadcastPolicy over the TRANSITION and then modulated by
	// the destination (ADR 0020); it used to be hard-coded to one Reason. It is
	// IRREVERSIBLE, it costs a `chat.postMessage` against the ~1/second/channel
	// budget, and its in-channel form carries NEITHER COLOUR NOR BUTTONS — see
	// broadcast.go.
	ModeBroadcastReply Mode = "broadcast_reply"
)

// Valid reports whether m is in the closed set.
func (m Mode) Valid() bool {
	switch m {
	case ModePostRoot, ModeUpdateRoot, ModeThreadReply, ModeBroadcastReply:
		return true
	default:
		return false
	}
}

// NeedsRoot reports whether this mode has nothing to attach to until a root
// message exists. It is the flag the per-thread ordering gate consumes.
func (m Mode) NeedsRoot() bool { return m != ModePostRoot }

// IsReply reports whether this mode appends to a thread rather than touching the
// root.
func (m Mode) IsReply() bool { return m == ModeThreadReply || m == ModeBroadcastReply }

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
	// CapBroadcast means a thread reply can be surfaced in-channel.
	CapBroadcast
	// CapDedupeKey means the provider does its own dedupe.
	CapDedupeKey
)

// Has reports whether every bit in want is set.
func (c Capability) Has(want Capability) bool { return c&want == want }

// rootModeFor is the §H.6 table's root column: what this Reason does to the root
// card. An empty Mode means the Reason touches no root at all.
func rootModeFor(r Reason, threadExists bool) Mode {
	switch r {
	case ReasonComment, ReasonUnackedReminder:
		// Neither changes the signal's state, so neither has anything to say to
		// the root card. Rewriting the card to announce a comment would make the
		// card's `updated` timestamp lie about when the SIGNAL last changed.
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
	case ReasonFired, ReasonSomeResolved, ReasonRepeat, ReasonUnacked:
		// `repeat interval elapsed` is the important one. It UPDATES AND NEVER
		// POSTS — that single rule is the largest noise reduction available to
		// oto, and it is exactly what stock Alertmanager and Grafana get wrong.
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
	// update_root except the reminder, which is always a broadcast.
	ThreadUpdates bool
	// Capabilities is the destination's negotiated bitmask. Negotiation happens
	// HERE, centrally, and never inside a provider (§H.10).
	Capabilities Capability
	// ThreadExists reports that a live ChannelThread already binds this group
	// generation to this channel.
	ThreadExists bool
	// StormMode collapses a group to one root card: in storm mode every per-alert
	// reply is suppressed and only the storm announcement itself survives (§B.6).
	StormMode bool
	// Flapping switches the group to update-only, with the digest reply owned by
	// the flap damper rather than by each transition (§B.6).
	Flapping bool
	// Broadcast is the org's policy over which transitions surface in the channel
	// (ADR 0020). The zero value is the approved default set.
	Broadcast BroadcastPolicy
	// ChannelNoticeClaimed reports that THIS destination won the channel-level
	// storm-notice latch for this evaluation (`channels.storm_notice_at`).
	//
	// ⛔ IT IS AN ANSWER, NOT A QUESTION. The caller has already taken the latch
	// against the database before building this input; PlanFor neither asks for
	// nor could ask for it, because the whole point of the latch is that exactly
	// one caller wins it. False is the ordinary case and means "this channel has
	// already been told a storm is on" — the reply still lands on the group's
	// thread, quietly.
	ChannelNoticeClaimed bool
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
	// "thread_updates", "no_threading", "storm", "flapping".
	ReplyDropReason string
	// BroadcastDamped reports that this transition WARRANTED a broadcast and got a
	// quiet reply instead. It is not an error and not a suppression: the fact is
	// still delivered on the thread. It is recorded because a damped broadcast is
	// invisible by construction, and "oto decided not to shout" is exactly the
	// kind of decision §B.6 refuses to take silently.
	BroadcastDamped bool
	// BroadcastDampReason is why: "storm", "flapping" or "no_capability".
	BroadcastDampReason string
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
//  1. the reminder is decided first, because it is the one mode `thread_updates`
//     may not reduce;
//  2. the root mode is chosen, then degraded if the destination cannot amend —
//     a channel with no edit-in-place gets a fresh standalone message rather
//     than nothing at all;
//  3. the reply is chosen, then gated by storm, flapping, thread_updates,
//     verbosity and threading capability, in that order.
//
// Nothing here reads a provider type. A destination is described entirely by its
// capability bits and its two channel-level switches, which is what keeps this
// table honest when a second provider appears.
func PlanFor(in PlanInput) Plan {
	if !in.Reason.Valid() {
		return Plan{}
	}

	// ---- the one reminder stage -----------------------------------------
	// It is a broadcast reply and `thread_updates=false` does not reduce it: a
	// reminder nobody sees is not a reminder. Where the destination cannot
	// broadcast, it degrades to a plain reply, and where it cannot thread
	// either, to a root update — loud enough to still be a reminder.
	//
	// ⛔ THE DAMPERS STILL BIND HERE, and they did not used to. This branch
	// returned an UNCONDITIONAL broadcast, which meant a storm across two hundred
	// unacknowledged alerts produced two hundred `chat.postMessage` calls into one
	// channel — oto shouting, once per alert, about the fact that it had started
	// being quiet. ADR 0020 constraint 3 permits exactly ONE broadcast in storm
	// mode, the storm announcement itself. So the reminder is DAMPED, not dropped:
	// it degrades to a quiet thread reply and the reminder still lands.
	if in.Reason == ReasonUnackedReminder {
		damp := dampReason(in)
		mode := ModeThreadReply
		switch {
		case !in.Capabilities.Has(CapThreading):
			// Nothing to reply to. A root update is the loudest thing left.
			return Plan{Modes: []Mode{ModeUpdateRoot}}
		case damp == "":
			if in.Capabilities.Has(CapBroadcast) {
				mode = ModeBroadcastReply
			} else {
				damp = "no_capability"
			}
		}
		p := Plan{Modes: []Mode{mode}}
		if mode != ModeBroadcastReply && damp != "" {
			p.BroadcastDamped, p.BroadcastDampReason = true, damp
		}
		return p
	}

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

	case in.StormMode && in.Reason != ReasonStorm:
		// Storm mode posts ONE root with a count and a link and suppresses every
		// per-alert reply. The storm announcement itself is the exception, or the
		// collapse would be invisible.
		return drop("storm")

	case in.Flapping && in.Reason != ReasonRuleChanged:
		// A flapping alert switches to update-only; the coalesced digest is the
		// flap damper's job, not this transition's.
		return drop("flapping")

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

	// ---- broadcast -------------------------------------------------------
	// The reply survives. ADR 0020 decides whether it surfaces in the channel:
	// POLICY decides that the TRANSITION warrants it, and the destination's own
	// gates — already passed above — decide whether this channel gets it. Broadcast
	// never overrides a destination's volume setting; a channel that has opted out
	// of thread replies does not receive louder ones.
	//
	// Two ways to earn a broadcast, and they are not the same shape:
	//
	//  1. `Warrants` — the TRANSITION is one an on-call engineer would be angry to
	//     have missed. Two Reasons qualify and neither is per-alert noise.
	//  2. `WarrantsChannelNotice` — the fact is about the CHANNEL, not the thread,
	//     and the caller has already won the once-per-channel latch for it. This
	//     is `storm`, and it is the only broadcast permitted while a storm is on.
	mode := ModeThreadReply
	wantsBroadcast := in.Broadcast.Warrants(in.Reason) ||
		(WarrantsChannelNotice(in.Reason) && in.ChannelNoticeClaimed)
	switch {
	case !wantsBroadcast:
		// `storm` on a channel that has already been told: the group's own thread
		// still records that this generation went quiet. That is not a damped
		// broadcast — nothing was withheld, the channel simply already knows.
	case in.Capabilities.Has(CapBroadcast):
		mode = ModeBroadcastReply
	default:
		// §H.10 capability degradation: broadcast → reply. The fact still
		// lands on the thread; only the channel-level summons is lost.
		p.BroadcastDamped, p.BroadcastDampReason = true, "no_capability"
	}

	// A Reason with a reply but no root — `comment` — still needs the reply, and
	// a destination that cannot amend has already had its root promoted above.
	p.Modes = append(p.Modes, mode)
	return p
}

// dampReason names the damper in force, or "" when none is.
//
// It exists so the reminder branch — which runs BEFORE the ordinary gates,
// because `thread_updates` may not silence a reminder — asks the same question
// those gates ask, in the same order. Storm outranks flapping for the same reason
// §B.8.2 orders the suppressors: a storm is the louder fact about oto's own
// behaviour, and it is the one an operator needs to see named.
func dampReason(in PlanInput) string {
	switch {
	case in.StormMode && in.Reason != ReasonStorm:
		return "storm"
	case in.Flapping && in.Reason != ReasonRuleChanged:
		// A flapping alert produces a digest, and a digest does not broadcast.
		return "flapping"
	default:
		return ""
	}
}
