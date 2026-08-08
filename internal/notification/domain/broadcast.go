package domain

import "time"

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
// ⛔ THE SET IS TWO REASONS. It was four; ADR 0020 was revised and it is now the
// two whose quiet form is genuinely INVISIBLE, which is the only property that
// earns an irreversible channel post:
//
//   - `unacked_reminder`  — its purpose is to reach someone who has NOT engaged.
//     In-thread it reaches only the already-engaged, which
//     is precisely the wrong audience. This is the case
//     `reply_broadcast` was put in oto for.
//   - `refired`           — a re-fire INSIDE `refire_grace` reopens the existing
//     occurrence: an update plus a thread reply, no new root.
//     The thread said "resolved" and people stopped following
//     it. A re-fire AFTER the grace window opens a new
//     generation and a new root message, which is already
//     loud (§B.5) and is not in this set.
//
// ⛔ `storm` WAS REMOVED FROM THIS SET AND MUST NOT COME BACK. A storm means MANY
// alerts; a per-thread broadcast of "oto has gone quiet" therefore produces
// exactly the flood the damping exists to prevent — oto shouting, once per
// group, about having started to be quiet. The fact is real and still delivered:
// the per-group `storm` reply stays on that group's thread, and the CHANNEL is
// told once, by StormNotice below.
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

// ⭐ THE CHANNEL-LEVEL STORM NOTICE.
//
// "oto has started withholding individual notifications" is a fact about OTO'S
// OWN BEHAVIOUR, and §B.6 refuses to let a damper engage silently. But storm mode
// is decided PER GROUP, and a channel routinely carries many groups: in a real
// storm, twenty generations enter storm mode inside a minute, and twenty
// per-thread broadcasts into one channel is the flood, not the fix.
//
// So the notice is addressed to the CHANNEL and issued at most once per channel
// per window, by a latch the database holds (`channels.storm_notice_at`). The
// per-group `storm` reply still lands on each group's own thread — the record is
// complete — but exactly one of those replies is allowed to surface in-channel.
//
// ⛔ THE LATCH IS A TIME WINDOW, NOT A REFERENCE COUNT, AND THAT IS DELIBERATE. A
// count of storming groups would be exact and would also LEAK: a generation can
// be closed while storming (`Close` clears `storm_mode` with no storm-end
// evaluation), and a leaked count means the channel is never told about the next
// storm at all. A silent, permanent failure of a damper's own announcement is the
// worst outcome available here; an occasional extra notice is the cheapest. The
// window self-heals.

// StormNoticeWindow is the shortest gap between two channel-level storm notices,
// when the org's own storm cooldown is unusable.
//
// The caller should pass the org's `storm_cooldown_s`, because that is the
// setting that already defines the minimum distance between a storm starting and
// the same storm ending (§B.6): a window equal to it lets a storm's start and its
// own end each get through, while collapsing every other group's storm inside it.
const StormNoticeWindow = 5 * time.Minute

// NormaliseStormNoticeWindow bounds a caller-supplied window. Zero or negative
// means "the org set nothing usable", which must not mean "no latch at all" —
// that would restore the per-group flood.
func NormaliseStormNoticeWindow(d time.Duration) time.Duration {
	if d <= 0 {
		return StormNoticeWindow
	}
	return d
}

// WarrantsChannelNotice reports whether this Reason is the one that speaks for
// the CHANNEL rather than for the thread, and therefore has to pass the latch
// before it may broadcast.
//
// It is exactly one Reason and is not configurable. A second member would be a
// second thing competing for one channel-wide latch, and the loser would be
// silently dropped.
func WarrantsChannelNotice(r Reason) bool { return r == ReasonStorm }
