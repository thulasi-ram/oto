package domain

// Broadcast policy — ADR 0020, "Broadcast the transitions that must be seen".
//
// ⭐ WHY THIS FILE EXISTS. ADR 0008 made `chat.update` the primary verb and
// bought quiet with a property nobody wrote down: **`chat.update` is completely
// silent.** No notification, no unread badge, no bump in the channel list. A card
// can go from `warning` to `critical` and every person in the channel can miss
// it. Thread replies have the same shape — they notify thread participants and
// nobody else. `reply_broadcast` is Slack's answer: the thread stays the record,
// and the one change that matters surfaces once in the channel.
//
// ⛔⛔ THREE PROPERTIES OF A BROADCAST, ALL BINDING, ALL COUNTER-INTUITIVE:
//
//  1. **THE CHANNEL-VISIBLE ARTEFACT IS A STRIPPED REFERENCE, NOT A COPY.** Slack
//     delivers a `thread_broadcast` subtype that is "a pointer or reference to the
//     actual thread", and "the reference cannot contain attachments or message
//     buttons". SPEC §H.1 S3 puts ALL of oto's blocks inside exactly one
//     attachment, because that attachment is the only way to get the colour bar.
//     **So the in-channel form of a broadcast has no colour bar and no buttons.**
//     The reply's top-level `text` is very nearly all a channel reader sees, so it
//     must be self-sufficient, must carry the severity in words, and must never
//     depend on a colour or an Acknowledge button to convey its meaning. The call
//     to action is *open the thread*.
//
//  2. **BROADCASTING IS IRREVERSIBLE.** Nothing in Slack's documentation
//     un-broadcasts; `chat.update`'s own `reply_broadcast` only adds. The bar is
//     therefore "would an on-call engineer be angry to have MISSED this?", not "is
//     this interesting?" — a channel that learns to scroll past oto's broadcasts
//     has lost the only mechanism oto has for genuine urgency. Adding a Reason to
//     the set below carries the same weight as adding one to the notification
//     path.
//
//  3. **A BROADCAST IS A `chat.postMessage`, AND AN UPDATE IS NOT.** Posts are
//     what the ~1 message/second/channel budget constrains; `chat.update` is Tier
//     3 and effectively free. Choosing to broadcast moves a fact from the cheap
//     verb to the expensive one — which is why the set below is TWO Reasons and
//     nothing per-alert. It used to be why broadcasts were damped during a storm;
//     the answer to volume is now a narrow set, not a switch oto throws at 3am.

// BroadcastPolicy is which transitions warrant surfacing in the channel.
//
// It is deliberately ONE boolean and not a set of them. ADR 0020's default set is
// a product decision reached against constraint 2, and a per-Reason dial would
// make broadcast the thing each team negotiates rather than the thing oto is
// confident about — which is how a channel ends up scrolling past oto. `resolved`
// is configurable because closure is genuinely welcome on a quiet channel and
// genuinely doubles traffic on a busy one, and because nobody was ever woken
// because a resolve arrived quietly.
type BroadcastPolicy struct {
	// Resolved surfaces `all_resolved` in the channel. Default FALSE.
	Resolved bool
}

// DefaultBroadcastPolicy is ADR 0020's approved default set.
func DefaultBroadcastPolicy() BroadcastPolicy { return BroadcastPolicy{Resolved: false} }

// Warrants reports whether this transition is one an on-call engineer would be
// angry to have missed.
//
// ⛔ THE SET IS TWO REASONS. It was four; ADR 0020 was revised and it is now the
// two whose quiet form is genuinely INVISIBLE, which is the only property that
// earns an irreversible channel post:
//
//   - `unacked_reminder`  — its purpose is to reach someone who has NOT engaged.
//     In-thread it reaches only the already-engaged, which
//     is precisely the wrong audience. This is the case
//     `reply_broadcast` was put in oto for.
//   - `refired`           — ⛔ RETAINED FOR HISTORY; NOTHING PRODUCES IT. It was
//     T8's reason, and ADR 0040 retired T8: every re-fire
//     now opens a new episode. It stays in this set because
//     `notifications.reason` is a persisted enum and a
//     stored `refired` row must still render and still be
//     replayable, and because the reasoning below is what a
//     future re-fire-into-an-open-generation reason would
//     inherit — a re-fire that lands in a generation still
//     open is an update plus a thread reply and no new root,
//     and the thread said "resolved" so people stopped
//     following it. A re-fire that finds the generation
//     closed gets a new root message, which is already loud
//     (§B.5) and is not in this set.
//
// ⛔ `storm` WAS REMOVED FROM THIS SET AND THE REASON IT NAMED IS NOW DELETED. It
// was first taken out because a storm means MANY alerts, so a per-thread broadcast
// of "oto has gone quiet" produced exactly the flood the damping existed to prevent
// — oto shouting, once per group, about having started to be quiet. The channel was
// told once instead, behind a latch. Both the reply and the latch are gone with
// storm damping: a product that does not withhold has nothing to announce, and
// the Reason has left the vocabulary entirely (reason.go, migration 00060).
//
// ⛔ `severity_raised` is not here because it does not exist. See reason.go.
//
// NOT broadcast: `acked`, `comment`, `enriched`, `snoozed`. Each is a fact about
// the RESPONSE, addressed to people already following the thread — that is what
// threads are for. Broadcasting an ack in particular would double the channel
// traffic of every well-handled alert, punishing the behaviour oto wants.
func (p BroadcastPolicy) Warrants(r Reason) bool {
	switch r {
	case ReasonRefired, ReasonUnackedReminder:
		return true
	case ReasonAllResolved:
		return p.Resolved
	default:
		return false
	}
}

// ⛔ THE CHANNEL-LEVEL STORM NOTICE WAS HERE AND IS DELETED IN FULL: the
// `StormNoticeWindow` floor, `NormaliseStormNoticeWindow`, `WarrantsChannelNotice`
// and the `channels.storm_notice_at` latch they served.
//
// ⭐ WHAT IT WAS, AND WHY IT WAS REASONABLE. "oto has started withholding individual
// notifications" is a fact about OTO'S OWN BEHAVIOUR, and §B.6 refuses to let a
// damper engage silently. But storm mode was decided PER GROUP and a channel
// routinely carries many groups: in a real storm twenty generations entered storm
// mode inside a minute, and twenty per-thread broadcasts into one channel is the
// flood, not the fix. So the notice was addressed to the CHANNEL and issued at most
// once per window, by a latch the database held — a time window rather than a
// reference count, because a generation could be closed while storming (`Close`
// clears `storm_mode` with no storm-end evaluation) and a leaked count would mean
// the channel was never told about the next storm at all.
//
// ⭐⭐ AND WHY NONE OF IT SURVIVES. Every line of it was machinery for announcing
// oto's own silence, and the silence is gone: nothing evaluates a storm, so no
// generation collapses and no channel has anything to be told. The honest reading of
// the design it replaced is that a storm needed an OBJECT to be reported on, the
// object is an INCIDENT (`correlation`, DEFERRED-POST-V1), and in its absence the
// fact was pushed down into the notification layer, where the only vocabulary
// available was "withheld". A latch that rations an announcement is a tell that the
// announcement is about the tool rather than about the signal.
//
// ⛔ `channels.storm_notice_at` STAYS IN THE SCHEMA, unwritten and unread. Dropping
// it is a migration, and the destructive half of this removal is deferred as its own
// breaking change.
