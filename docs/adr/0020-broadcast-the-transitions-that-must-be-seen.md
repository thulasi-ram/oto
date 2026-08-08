# 0020 — Broadcast the transitions that must be seen

**Status:** Accepted · 2026-08-08
**Relates to:** [0008](0008-slack-update-in-place-primary.md) (`chat.update` is the primary verb),
[0005](0005-durable-group-key-owns-the-slack-thread.md) (the group owns the thread),
[0013](0013-alert-first-scope-boundary.md) (oto pages nobody)
**Touches:** SPEC §H.1 (S3, S5), §H.5, §H.6, `internal/notification/domain/mode.go`

## Context

ADR 0008 made `chat.update` the primary verb for good reasons — it is Tier 3 (50+/min) against
`chat.postMessage`'s ~1 message/second/channel, and one card that edits itself beats twelve that do
not. But it bought that quiet with a property nobody wrote down:

> **`chat.update` is completely silent.** No notification, no unread badge, no bump in the channel
> list, nothing rises. A card can go from `warning` to `critical` and every person in the channel can
> miss it.

That is the correct default — it is the whole reason oto is quieter than stock Alertmanager — and it is
also a hazard. The design that stops oto shouting also lets it whisper something that needed shouting.
Threads have the same shape: a reply notifies thread participants and nobody else, so a change posted
into a thread that nobody has opened is, in practice, invisible.

Slack's answer is `reply_broadcast` — the API form of the *"Also send to #channel"* checkbox. The thread
stays the record; the one change that matters surfaces once in the channel. oto already uses it for
exactly one thing (the unacked reminder). The question is whether it should be a policy over
transitions rather than a hard-coded special case.

### What the research established

**`reply_broadcast` is a parameter on `chat.postMessage`, used with `thread_ts`**: *"Used in conjunction
with `thread_ts` and indicates whether reply should be made visible to everyone in the channel or
conversation. Defaults to `false`."*
([chat.postMessage](https://docs.slack.dev/reference/methods/chat.postMessage/)) It is one API call, not
two.

**⚠️ The channel-visible artefact is a REFERENCE, not a copy — and it is stripped.** Slack delivers a
`thread_broadcast` message subtype which is *"a pointer or reference to the actual thread and is meant
more to be informational than to fully describe the message"*, and, decisively for oto:
**"The reference cannot contain attachments or message buttons."**
([thread_broadcast](https://docs.slack.dev/reference/events/message/thread_broadcast/)) SPEC §H.1 **S3**
puts *all* of oto's blocks inside exactly one attachment, because that attachment is the only way to
get the colour bar. **So the in-channel form of a broadcast carries no colour bar and no buttons.** This
was not anticipated and it changes a rendering rule; see the Decision.

**`chat.update` also accepts `reply_broadcast`**: *"Broadcast an existing thread reply to make it
visible to everyone in the channel or conversation."* — with the caveat *"Can't broadcast an old reply
and update the content at the same time."*
([chat.update](https://docs.slack.dev/reference/methods/chat.update/)) So a reply posted quietly can be
broadcast **later**, as a separate call.

**Nothing in Slack's documentation un-broadcasts.** `chat.update`'s `reply_broadcast` only adds. There
is no documented parameter, method or side effect that removes an existing channel reference.
**Broadcasting is a one-way door.** *(That deleting the reply with `chat.delete` also removes the
reference is plausible but* **UNVERIFIED** *— Slack does not say, and oto requests no scope it could use
to try. Treat deletion as unavailable.)*

**Errors are ordinary `chat.postMessage` errors.** The documented error list includes
`channel_not_found`, `is_archived`, `not_in_channel`, `cannot_reply_to_message`,
`restricted_action_thread_locked` and `message_not_found` — all already classified by
`internal/channels/providers/slack/errors.go`, in the same two buckets (`conversationDead`,
`threadPointerLost`) and with the same recovery. **`reply_broadcast` introduces no new failure mode.**
A broadcast into a channel oto has been removed from, or that was archived, fails exactly as an ordinary
reply into it would: terminal, thread marked dead, channel degraded, no retry.

**Rate limits.** `chat.postMessage` keeps its special tier — *"1 message per second to a specific
channel"*, plus a workspace-wide *"several hundred messages per minute"*. A broadcasting reply is one
`chat.postMessage` call and therefore one unit of that budget. *(Whether the channel-side reference
additionally counts toward the workspace-wide limit is* **UNVERIFIED** *— undocumented either way.
Assume it does.)* The load-bearing point needs no verification: **a broadcast is a post, and posts are
what the per-channel budget constrains. Updates are not.** Choosing to broadcast moves a fact from the
cheap verb to the expensive one.

**Block limits.** The reply is an ordinary message and obeys §H.7 unchanged. `reply_broadcast` does not
interact with the 50-block ceiling; the *reference* is smaller than the reply, never larger.

## Decision

**`reply_broadcast` becomes a property of the transition, set by policy, and is used for the transitions
an on-call engineer would be angry to have missed.**

### The default set

**Broadcast:**

| Transition | Why |
|---|---|
| **Severity increases** (`warning` → `critical`) | The purest case for this ADR. Today this is a `chat.update`: the colour changes from amber to red and the channel is told nothing at all. Nobody would design that on purpose. |
| **Re-fire after resolve, within `refire_grace`** | Exactly the case that is otherwise silent. A re-fire *after* the grace window opens a new group generation and therefore a **new root message**, which is already loud (SPEC §B.5). A re-fire *within* it reopens the existing occurrence and produces an update plus a thread reply — so "it came back, and it came back fast" is the version of this fact that is easiest to miss and the most alarming to have missed. |
| **Storm detected** | A **behaviour change in oto itself**: from this moment individual notifications are being suppressed. People must be told that the tool has started withholding things, or the silence that follows is indistinguishable from nothing happening — the failure mode ADR 0013 calls fatal. |
| **Unacked reminder** | Its entire purpose is to be seen. Already broadcast; this ADR restates it as an instance of the rule rather than a hard-coded exception. |

**Do not broadcast** — `acked`, `comment`, `enriched`, `snoozed`. Each is a fact *about the response*,
addressed to people who are already following the thread. That is what threads are for. Broadcasting an
ack in particular would double the channel traffic of every well-handled alert, which punishes the
behaviour oto wants.

**Configurable, default off** — `all_resolved`. Closure is genuinely welcome and several teams will want
it. On a busy channel it also doubles traffic for the least urgent fact oto has: nobody was ever paged
because a resolve arrived quietly. Default off, one switch to turn on.

### Three binding constraints

1. **Broadcast is a property of the transition, then modulated by the channel.** Policy decides that a
   transition *warrants* a broadcast; the destination channel's `verbosity` and `thread_updates`
   settings decide whether that channel gets it. A channel that has already opted out of thread replies
   does not receive louder ones. Broadcast never overrides a destination's own volume setting, with the
   single existing exception of the unacked reminder — a reminder nobody sees is not a reminder, and
   `PlanFor` already encodes that.

2. **Broadcast is irreversible, so the bar is "would an on-call engineer be angry to have missed this?"
   — not "is this interesting?"** There is no un-broadcast. A broadcast added because it seemed useful
   cannot be taken back from the channel it was sent to, and a channel that learns to scroll past oto's
   broadcasts has lost the only mechanism oto has for genuine urgency. Adding a transition to the
   broadcast set is a decision of the same weight as adding one to the notification path, and it goes
   through the same review.

3. **Broadcast is rate-limit relevant and must be damped during a storm.** A broadcast is a
   `chat.postMessage` against the ~1/second/channel budget; an update is not. During a storm, "one
   broadcast per interesting transition" is a self-inflicted flood — oto would be shouting about the
   fact that it has started being quiet, once per alert. **In storm mode, exactly one broadcast is
   permitted: the storm announcement itself.** `PlanFor` already drops every non-storm reply while
   `StormMode` is set, so this is a property to preserve rather than to add. Flap damping binds the same
   way: a flapping alert produces a digest, and a digest does not broadcast.

### Two rendering rules this forces (amendments to SPEC §H)

4. **The top-level `text` of a broadcasting reply must be self-sufficient.** Since the in-channel
   reference carries neither the attachment nor its blocks, the `text` is very nearly all a channel
   reader sees. SPEC §H.1 **S5** already requires `text` to be a complete, deliberate sentence — for
   broadcasts this stops being good practice and becomes correctness. A broadcast whose `text` reads
   *"Re-fired"* is a broadcast that communicates nothing.

5. **A broadcasting reply carries no colour and no buttons, and must not depend on either.** No
   severity colour bar, no Acknowledge button in the channel copy. The reply's own `text` must carry the
   severity in words and emoji, and the call to action is *open the thread*. Golden-file tests for
   broadcast reply types should assert the `text` alone is intelligible.

### One recorded escape hatch

Because `chat.update` can broadcast an existing reply **later**, a transition whose importance is not
knowable at post time may be posted quietly and broadcast on a subsequent evaluation — "still
unacknowledged after N", "still firing after N". This converts an irreversible decision made in
ignorance into a deferred one made with evidence. It costs one extra Tier-3 `chat.update`, and Slack is
explicit that the broadcast and a content change cannot ride in the same call. **It is the sanctioned
mechanism for any future transition that fails constraint 2 at post time**, and it is how the unacked
reminder would be built if it were being designed today.

## Consequences

- The channel regains a signal for the small number of changes that genuinely matter, without giving up
  update-in-place for the many that do not. This is the direct answer to the hazard ADR 0008 created and
  did not name.
- **`ModeBroadcastReply` stops being reserved for one Reason.** `internal/notification/domain/mode.go`
  currently returns it only for `ReasonUnackedReminder`, hard-coded at the top of `PlanFor`. Generalising
  it means the broadcast decision moves into the §H.6 table alongside the other per-Reason columns, and
  `CapBroadcast` degradation (broadcast → reply → root update) must apply to every broadcasting Reason,
  not just the reminder. That is real work in the notification module.
- **Severity increase is not currently a Reason and has no way to be one.** `notifications_reason_ck`
  enumerates eighteen values and none of them is a severity change; today a severity bump arrives as
  part of `new_alerts` or a bare root update. Delivering this ADR's first row requires a new Reason
  (`severity_raised`, or similar), an expand/contract migration on the CHECK constraint, a row in the
  §H.6 table and a reply type in §H.5. **This is the largest single cost of the decision and it is not
  optional** — without it the clearest case for broadcasting is the one case oto cannot express.
- Broadcasts consume the per-channel posting budget, so a channel receiving alerts near the ~1/second
  ceiling will queue. The storm and flap gates are what keep that theoretical.
- The broadcast set is now a thing that will be argued about, transition by transition, forever. That is
  the correct place for the argument to happen — it is a product question, and constraint 2 is the test
  it gets settled by.
- Because the in-channel reference is stripped of attachments and buttons, oto's colour system (§H.2,
  §M.6) does not reach the channel copy. A reader who sees only broadcasts sees a monochrome feed. That
  is acceptable — a broadcast is a summons to the thread, not a replacement for the card — but it should
  not be discovered later by someone wondering why the red bar is missing.

## Alternatives rejected

**Keep `reply_broadcast` reserved for the unacked reminder.** The status quo. Rejected: it leaves a
severity escalation from `warning` to `critical` as a silent edit, which is the one outcome nobody
would choose deliberately. The reminder is not special; it is simply the first instance of the rule
that was noticed.

**Broadcast every thread reply, and let `verbosity` restrain it.** Rejected on constraint 2. Verbosity
is a per-channel volume dial, not a per-transition importance judgement, and this inverts the burden of
proof: it makes broadcast the default and requires each quiet transition to argue for silence. Within a
month the channel is a scrollback again and oto has rebuilt the product it exists to replace.

**Post a second, separate root-level message instead of broadcasting.** Rejected: it costs the same
`chat.postMessage` budget, produces a message with no thread relationship, and re-creates the duplicate
cards ADR 0008 eliminated. `reply_broadcast` gets the same channel visibility while keeping the thread
as the single record.

**Use `chat.update` on the root with a loud emoji and hope people notice.** Rejected — this is precisely
the failure this ADR exists to fix. Updates are silent. There is no formatting that makes a silent edit
generate a notification.

**Broadcast on `acked`.** Rejected explicitly, because it will be proposed. It doubles channel traffic
for well-handled alerts and it broadcasts a fact about a *person's response*, which sits awkwardly with
the scope boundary even though the ack itself is a legitimate annotation on a signal (ADR 0013, FR-1).
Whoever is waiting to hear that it was acked is in the thread.
