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
//     verb to the expensive one — which is why broadcasts MUST be damped during a
//     storm. See PlanFor.

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
// ⛔ THE FOUR FIXED MEMBERS ARE NOT CONFIGURABLE, and the omissions are as
// deliberate as the members:
//
//   - `severity_raised`   — the purest case. Today's alternative is a silent edit
//     from amber to red.
//   - `refired`           — a re-fire INSIDE `refire_grace` reopens the existing
//     occurrence and produces an update plus a thread reply, so
//     it posts no new root and is otherwise invisible. A re-fire
//     AFTER the grace window opens a new generation and a new
//     root message, which is already loud (§B.5).
//   - `storm`             — a behaviour change in OTO ITSELF: from this moment
//     individual notifications are being withheld. People must
//     be told the tool changed, or the silence that follows is
//     indistinguishable from nothing happening.
//   - `unacked_reminder`  — its entire purpose is to be seen. It was the original
//     hard-coded exception; it is now an instance of the rule.
//
// NOT broadcast: `acked`, `comment`, `enriched`, `snoozed`. Each is a fact about
// the RESPONSE, addressed to people already following the thread — that is what
// threads are for. Broadcasting an ack in particular would double the channel
// traffic of every well-handled alert, punishing the behaviour oto wants.
func (p BroadcastPolicy) Warrants(r Reason) bool {
	switch r {
	case ReasonSeverityRaised, ReasonRefired, ReasonStorm, ReasonUnackedReminder:
		return true
	case ReasonAllResolved:
		return p.Resolved
	default:
		return false
	}
}
