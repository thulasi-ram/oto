# 0020 — Broadcast the transitions that must be seen

**Status:** Accepted · 2026-08-08 · **Amended four times — three by research, once by a live
Slack workspace** — see
[Amendment 1](#amendment-1--the-set-narrows-to-two-and-the-storm-moves-to-the-channel),
[Amendment 2](#amendment-2--severity_raised-is-unreachable-and-has-been-deleted) and
[Amendment 4](#amendment-4--the-stripping-premise-did-not-survive-the-first-live-workspace)

> ⚠️ **Read Amendment 4 before relying on anything below about the in-channel copy being
> "stripped".** Slack's documentation says the `thread_broadcast` reference cannot carry attachments
> *or* buttons. A live workspace says it is **half right**: the buttons really are gone, and the
> attachment and its colour bar really do render. Both binding rules survive — **the top-level
> `text` must be self-sufficient** and **no broadcast may depend on a button** — the first on
> independent grounds, the second now on direct evidence.

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

**⚠️ The channel-visible artefact is a REFERENCE, not a copy — and it is stripped.**
*(⛔ **CONTRADICTED BY OBSERVATION — see [Amendment 4](#amendment-4--the-stripping-premise-did-not-survive-the-first-live-workspace).**
The paragraph is kept because two binding rules were derived from it and an argument needs its
premise present.)* Slack delivers a
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

> ⚠️ **SUPERSEDED BY AMENDMENT 1 AND AMENDMENT 2.** The table below is the set as first accepted. Two
> of its four rows are gone: `severity_raised` names a transition oto cannot observe, and `storm` was
> relocated from a per-group broadcast to a once-per-channel notice. The surviving set is
> **`unacked_reminder`** and **`refired`**, plus the configurable `all_resolved`. The original table is
> kept because the amendments are arguments against it and an argument needs its opponent present.

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

3. **Broadcast is rate-limit relevant and must be damped during a storm.** *(⚠️ Restated by Amendment 1:
   the budget is counted per CHANNEL, not per group.)* A broadcast is a
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

   > ⛔ **SUPERSEDED BY AMENDMENT 4, IN HALF.** The colour bar *does* render. Rule 5 is now 5a
   > (colour is a progressive enhancement, kept, never the only carrier of a fact) and 5b (no
   > broadcast may depend on a button — still unverified, and binding either way). The conclusion —
   > *must not depend on either* — is unchanged.

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
- ⚠️ **SUPERSEDED BY AMENDMENT 2.** ~~**Severity increase is not currently a Reason and has no way to be
  one.**~~ It has no way to be one because it is not a thing that happens — see Amendment 2. The
  paragraph is kept because the amendment is an answer to it.

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


---

## Amendment 1 — the set narrows to two, and the storm moves to the channel

**2026-08-08. Product review of the accepted set.**

The set above was four transitions. It is now **two**, and the test that survives is narrower than
constraint 2 as originally written. "Would an on-call engineer be angry to have missed this?" is not
quite the question, because it does not distinguish a fact that is *quiet* from a fact that is
**invisible**. The sharper test, and the one the two survivors pass:

> **Is the quiet form of this fact invisible to the people who need it?** `chat.update` sends no
> notification at all, and a thread reply notifies only people already in the thread. A fact whose
> audience is "people already following this thread" is not invisible to that audience — it is simply
> quiet, which is what oto is for.

**Kept:**

| Transition | Why the quiet form is *invisible* |
|---|---|
| **`unacked_reminder`** | Its purpose is to reach somebody who has **not** engaged. In-thread it reaches only the already-engaged, which is precisely the wrong audience. This is the case `reply_broadcast` was put in oto for, and it remains the clearest one. |
| **`refired` within `refire_grace`** | The thread said *resolved*. People stopped following it — that is what "resolved" means to a reader. A re-fire inside the grace window reopens the existing occurrence, so it posts no new root, and the reply lands in a thread nobody is watching any more. A re-fire *after* the window opens a new generation and a new root, which is already loud (§B.5) and is not in this set. |

**`all_resolved`** remains configurable, default off. Unchanged.

### `storm` is removed as a per-alert broadcast

⛔ **A storm means many alerts. Broadcasting the storm notice per thread therefore produces exactly the
flood the damping exists to prevent** — oto shouting, once per group, about having started to be quiet.
Storm mode is decided **per group** (§B.6, `EvaluateStorm`), and one channel routinely carries many
groups: twenty generations collapsing inside a minute is twenty `chat.postMessage` calls into one
channel, against a ~1/second/channel budget, announcing a damper.

Constraint 3 already said "in storm mode, exactly one broadcast is permitted: the storm announcement
itself". It was **counted per group**, which is the wrong denominator. The right denominator is the
**channel**, because the channel is the audience.

**The replacement: a single channel-level storm notice.**

- The per-group `storm` reply is unchanged and still lands on each group's own thread. The record stays
  complete and §B.6 is still satisfied — nothing is silently withheld.
- **At most one of those replies broadcasts per channel per window.** The winner is decided by a latch
  the database holds: `channels.storm_notice_at` (migration `00027_channel_storm_notice.sql`), taken
  with one conditional `UPDATE ... WHERE storm_notice_at IS NULL OR storm_notice_at <= now - window`
  that returns the claimed row. Zero rows affected *is* the answer, in the same idiom as the §G.5
  delivery claim. Concurrent dispatchers cannot both win: the row lock serialises them and the loser's
  predicate no longer holds.
- The claim is taken **inside the evaluation transaction**, so an evaluation that rolls back cannot
  consume a channel's one notice, and **per destination**, because two channels are two audiences and
  each is entitled to be told once.
- The window is the org's own **`storm_cooldown_s`**. That is the setting that already defines the
  minimum distance between a storm starting and the same storm ending, so a storm's start and its own
  end each get through while every other group's storm inside it collapses into the first.

⛔ **The latch is a TIME WINDOW, not a reference count, and the choice is deliberate.** A count of
storming groups would be exact and would also **leak**: `Group.Close` clears `storm_mode` on an idle
generation with no storm-end evaluation, so a closed storming group decrements nothing and the count
never returns to zero — after which that channel is **never told about another storm, ever, silently**.
A permanently dead damper announcement is the worst outcome available here and §B.6 has a clear
preference; an occasional extra notice is the cheapest. The window self-heals, the counter does not.

⛔ **The notice cannot repeat per alert, structurally.** `PlanFor` will only upgrade a `storm` reply to a
broadcast when `ChannelNoticeClaimed` is set, and that flag is an **answer, not a question** — it is set
by the caller that already won the latch. `WarrantsChannelNotice` admits exactly one Reason, so a
future transition cannot acquire a channel-wide post by setting one boolean.
`TestTheChannelStormNoticeCannotRepeatPerAlert` pins it at twenty groups, one broadcast.

### Constraint 3, restated

**In storm mode, exactly one broadcast is permitted per CHANNEL: the storm notice.** Every other
transition's reply is dropped by the storm gate as before, and the unacked reminder is *damped* to a
quiet thread reply rather than dropped — a reminder still has to land.

---

## Amendment 2 — `severity_raised` is unreachable and has been deleted

**2026-08-08. Verified against a real database.**

The Consequences section below calls the severity-increase row "the largest single cost of the decision
and … not optional". It was neither, because **the transition it names cannot happen.**

### The finding

In Prometheus, `severity` is an ordinary **label**. SPEC §C.1/§C.2 computes an Alert's identity as

```
alert_key = "ak_" || base32hex( sha256( org_id ‖ cluster_key ‖ canon(labels, ignore_labels) )[0:16] )
```

so `severity` is part of `alert_key` unless it appears in the source's **ignore-label set**. It does
not. `alert_sources.ignore_labels` defaults to
`{prometheus_replica, __replica__, monitor, replica, pod_template_hash}` (migration `00004_sources.sql`,
mirrored by `sources/domain.DefaultIgnoreLabels`) — the five labels an HA Alertmanager pair, a
Prometheus replica pair or a Kubernetes deployment adds to otherwise identical alerts. `severity` is not
among them and there is no configuration oto ships in which it is.

**Therefore a "severity increase" is not one Alert changing. It is a different Alert** — different
`alert_key`, different row under `UNIQUE (org_id, alert_key)`, its own occurrences, its own thread. No
row is ever `warning` and later `critical`.

### The evidence

`test/integration/alert_identity_test.go` bootstraps a real Postgres, creates a source through the real
API so the column takes its **DDL default**, reads `ignore_labels` back out of the database, and computes
two identities from label sets differing in exactly one value:

```
warning  → ak_7nin42p5tguo2acs8960q8qoqo
critical → ak_pvr1n5fdg6e9veiv985nrusplo
```

The test is kept permanently. If somebody ever adds `severity` to the shipped ignore set, the whole of
§C.2's identity story changes and that test is where they find out.

The code that was supposed to emit the Reason confirms it independently: it looked the alert up in the
pre-upsert snapshot **by `alert.Key()`** and required `!WasInserted`. A changed severity is a changed
key, so the lookup was always a miss and the row was always a fresh insert. **Two independent guards,
both of which could only ever be false.**

### The ruling

`severity_raised` is **deleted**, not deprecated:

- the Reason value, from `notification/domain.Reason` and `AllReasons`;
- its rows in the §H.6 verbosity table and in `BroadcastPolicy.Warrants`;
- `alerts/domain.Severity.Rank` and `Raised`, which had no other caller;
- the emitter in `alerts/service.ObserveBatch`;
- the reply type and the `PreviousState.Severity` field it was the only reader of;
- migration `00027_severity_raised_reason.sql`, and the number was **reused** for the storm-notice
  migration. Nothing is deployed, so renumbering is safe, and a tombstone migration for an unreachable
  CHECK value is worse than no migration at all;
- the enum member in `api/openapi/openapi.yaml`.

⛔ **It was not kept "just in case".** An enum value with no writer is worse than an absence: every
client that switches on `NotificationReason` grows a branch that can never be taken, and the next
engineer reads the CHECK constraint as evidence that oto observes severity changes. `reason.go`,
`enums.go` and the OpenAPI description each carry a short refusal saying why it is not there, because
the argument for adding it is genuinely attractive and will be made again.

**If oto ever does want to observe "this alert got worse"**, the honest shape is a *relationship between
two Alerts* — "`ak_pvr…` is the `critical` sibling of `ak_7ni…`" — not a state change on one. That is a
different feature with a different data model, and it starts by adding `severity` to `ignore_labels`,
which changes what an Alert *is*. It is not a Reason.

---

## Amendment 3 — the verified mention behaviour, and the `unacked_reminder_mention` setting

**2026-08-08. Verified against Slack's documentation.**

Broadcasting gets a *reference* into the channel. It does not get a notification onto anybody's phone.
For the unacked reminder — the one message oto sends whose purpose is to reach somebody who has **not**
engaged — that gap matters, so a mention setting was specified. What it should default to turned on a
question of fact.

### What Slack documents

| Mention | In a thread reply | Source |
|---|---|---|
| `@here` / `@channel` / `@everyone` | **Does not notify.** *"These mentions won't notify people when their notifications are paused or when they're used in threads."* | [Notify a channel or workspace](https://slack.com/help/articles/202009646-Notify-a-channel-or-workspace) |
| `<@U…>` (user) | **Notifies.** *"If the mention is included in an app-published message, the mentioned user will also be notified about the reference."* | [Formatting message text](https://docs.slack.dev/messaging/formatting-message-text) |
| `<!subteam^S…>` (usergroup) | **Notifies.** *"If the mention is included in an app-published message, Slack will notify each user in the group about the mention."* | [Formatting message text](https://docs.slack.dev/messaging/formatting-message-text) |

**Does `reply_broadcast` change the first row?** *(**UNVERIFIED** — and the absence is the finding.)*
Slack documents **no exception** to the thread rule for `reply_broadcast`, and the
[`thread_broadcast`](https://docs.slack.dev/reference/events/message/thread_broadcast/) event is
described as *"a pointer or reference to the actual thread … meant more to be informational"* with no
mention semantics of its own. A broadcasting reply **is** a thread reply; `reply_broadcast` changes
where a reference appears, not what the message is. Neither documentation nor experiment was available
to prove otherwise, so oto treats `here` and `channel` as **silent no-ops in the position it uses them**.

**Verdict: the owner's belief is confirmed.** `@here` in a broadcast reply is, on the documented
evidence, a control that does nothing.

### The setting

`unacked_reminder_mention`, per org, alongside the tuning knobs: **`none` | `here` | `channel` | `list`**,
where `list` is an explicit audience of individuals and usergroups in
`unacked_reminder_mention_list`, capped at **10**.

**Default `none`, precisely because `here` may be a silent no-op.** A default that does nothing is a
trap: it manufactures the belief that somebody was told. `here` and `channel` remain *expressible* — an
operator may know something about their workspace the documentation does not say — but oto will not
choose one for them. **An explicit list is the only form Slack documents as notifying from a thread**,
and that is what the setting's own description says.

**Two binding constraints, both implemented:**

1. **Mentions are gated on severity, default `critical` only**
   (`unacked_reminder_mention_min_severity`). `@here` on every unacked warning is how a channel learns
   to mute oto, and a muted channel hides the real incident. ⛔ The gate **fails closed**: a severity oto
   cannot rank — an unknown label, or an absent one — gets no mention at any setting, so a typo'd
   `severity:` cannot ping ten people. (`page` shares `critical`'s rank; §H.2 renders them identically.)

2. **The mention goes in the top-level `text`, never inside a block.** This is a direct consequence of
   the stripping behaviour recorded above: §H.1 S3 puts every oto block inside one attachment, and the
   in-channel `thread_broadcast` reference *"cannot contain attachments or message buttons"*. A mention
   inside a block is not merely un-notifying in the channel — **it is not present in the channel copy at
   all.** It is appended *after* the sentence, so the message still reads for the people who were not
   mentioned, and it is deliberately absent from the attachment `fallback` and the delivery summary,
   which are previews rather than the message.
   `TestTheReminderMentionGoesInTheTopLevelTextAndNotInABlock` pins both halves.

⛔ **The anti-rota clause of ADR 0013 applies unchanged and is restated here because a mention list is
the most plausible place it will be violated.** The list is a **fixed audience an operator chose once,
in configuration**. It must never become time-aware, never acquire a second stage, and never be derived
from a schedule. The structural guarantee is that `MentionPolicy` has three fields — a mode, a list of
opaque ids and a severity floor — and nothing on the path that resolves it reads a clock. Adding a time
field to that struct is the review moment.

### One thing found on the way

`channels.config.mention_on_reminder` — a per-channel mention list, schema-validated, rendered into the
settings form, documented as a fixed audience — **was never read.** The renderer had the field, the
option and a `For()` method to mint a channel-scoped copy, and the registry built one shared
`slackrender.New(clk)` and handed it to every dispatch. Nothing ever called any of it. An operator could
set the list and nothing could ever happen: **exactly the trap this amendment exists to avoid, already
shipped.** It has been deleted, and the audience is now one org-level setting with one code path.

---

## Amendment 4 — the stripping premise did not survive the first live workspace

**2026-08-09. Observed in a real Slack workspace, not derived.**

The Decision above turns on one documented sentence: the in-channel `thread_broadcast`
reference **"cannot contain attachments or message buttons."** Two of this ADR's rendering rules
(4 and 5), the mention placement in Amendment 3, and three ⛔ comments in
`internal/channels/render/slack` were all derived from it. The first live Slack run contradicts it.

### The three data points, all of them

| # | Source | What it says |
|---|---|---|
| 1 | [Slack's documentation](https://docs.slack.dev/reference/events/message/thread_broadcast/) | The reference *"cannot contain attachments or message buttons"*. |
| 2 | `conversations.history` against the live channel | The `thread_broadcast` message is returned **with its `attachments` array intact** — `id`, `blocks`, `color` and `fallback` all present, not stripped. |
| 3 | A human looking at the message in their Slack client | **The colour bar renders. The buttons do not.** The root card and the resolved card both show their action buttons; the in-channel broadcast copy shows none. |

Data point 2 was read by a person with their own workspace credentials during verification.
**oto did not read it and must not be able to**: `channels:history` has been removed from
`deploy/slack/manifest.yaml`, and oto's `API` interface has three methods, none of which reads.

### The documentation is half right, and the halves have different consequences

The sentence names two things and is correct about exactly one of them:

| Claim | Verdict | Evidence |
|---|---|---|
| The reference **cannot contain message buttons** | ⭐ **TRUE.** | Data point 3: the broadcast has no buttons, while the root and resolved cards in the same channel do. |
| The reference **cannot contain attachments** | ⛔ **FALSE, as rendered today.** | Data points 2 and 3: the attachment survives the API round trip and the colour bar is on screen. |

That split is what the rest of this amendment is built on. It is more useful than "the docs are
wrong", because the two halves fail in opposite directions: one is a documented restriction that
turns out to be real, and the other is a documented restriction the client does not enforce — and
only the first is safe to build on.

### The ruling: colour is a progressive enhancement, never a carrier of meaning

**Colour demonstrably renders today, so oto neither strips it nor works around it.** A broadcasting
reply keeps its single attachment and its `color`, exactly as a thread reply does. There is nothing
to change in the renderer and nothing to special-case.

**But oto relies on it for nothing.** What renders is undocumented behaviour that Slack's own
documentation contradicts — which is the weakest possible ground to stand a correctness property
on. It can revert in any client release, on any platform, without notice or changelog, and the
failure would be silent: a card that quietly stops carrying the one signal it used colour for.

**Therefore rule 4 stands unchanged and binding: the top-level `text` of a broadcasting reply must
be self-sufficient.** It is worth being explicit that this is *not* merely defensive book-keeping
against a possible regression, because a rule kept "just in case" is a rule the next engineer
deletes:

> **The top-level `text` is what a screen reader announces and what a push notification shows on a
> locked phone.** Neither has ever rendered a colour bar, on any client, in any release. A person
> woken at 03:00 sees that string and nothing else. Rule 4 would be correct if data point 3 held
> forever and Slack documented it in writing.

The first live run failed rule 4 on its own broadcast — `":repeat: Re-fired: alertname=OtoSmokeTest,
cluster=smoke-test"`, with no severity, no duration and no state. That is fixed
(`replyFacts` in `internal/channels/render/slack/reply.go`), and it is fixed *because of the push
notification*, not because of the colour bar.

### Rule 5, split in two

Rule 5 said *"a broadcasting reply carries no colour and no buttons, and must not depend on
either."* Its premise is half wrong and its conclusion is entirely right. It is replaced by:

**5a. Colour on a broadcast is a progressive enhancement.** It renders today (data point 3). It is
kept, it is never the only carrier of a fact, and §H.2's rule that severity is *also* an emoji and
*also* a word covers the case where it stops rendering.

**5b. No broadcast may depend on a button — ⭐ CONFIRMED, not merely cautious.** The in-channel
copy shows **no buttons at all**, observed side by side with a root card in the same channel that
shows all of them. The documentation is right about this half, and `blocks` being present in the
stored attachment (data point 2) is storage, not rendering. So there is no Acknowledge in the
channel, and there never was: a broadcast's call to action is *open the thread*, and that must be
true in words. (This is the happier of the two possible answers. An inert-looking button would have
been worse than an absent one — a reader who presses Acknowledge and gets nothing learns to distrust
every button oto renders.)

### What is still unknown

- **Client parity.** Data point 3 is one client. **Desktop, web, iOS and Android have not been
  compared**, and Slack has shipped rendering differences between them before. Any future claim that
  a broadcast "shows X" needs all four, or it is a claim about one person's laptop. This matters
  most for the colour bar, which is the half that contradicts the documentation and is therefore the
  half most likely to differ between clients or revert in a release.

It is not worth blocking on, because no binding rule depends on the answer: rule 4 is grounded in
push notifications and screen readers, and 5b is grounded in an observed absence. It is recorded so
the next person does not mistake "one client showed it" for "Slack guarantees it" — which is the
mirror image of the mistake this amendment exists to correct.

### What does not change

- The broadcast set (Amendment 1): `unacked_reminder` and `refired`, plus configurable
  `all_resolved`.
- The mention placement (Amendment 3). The mention stays in the top-level `text`. Its justification
  shifts from *"a block is not present in the channel copy"* to *"the top-level text is the only
  position that reaches a push notification"* — which was always the stronger half of the argument.
- Constraints 1–3, and the one-way-door property of broadcasting.

---

## Amendment log

| # | Date | What changed | What forced it |
|---|---|---|---|
| 1 | 2026-08-08 | Broadcast set narrowed from four transitions to two; `storm` relocated to a once-per-channel latched notice | Product review: a storm is many alerts, so a per-group storm broadcast *is* the flood the damper prevents |
| 2 | 2026-08-08 | `severity_raised` and migration `00027` deleted | `severity` is hashed into `alert_key` (§C.2), so a severity rise is a new Alert; verified against a real database |
| 3 | 2026-08-08 | `unacked_reminder_mention` added, default `none`, gated on severity, rendered in the top-level `text` | Slack documents that `@here`/`@channel` do not notify in threads, so a default of `here` would ship a control that does nothing |
| 4 | 2026-08-09 | The "stripped reference" premise is recorded as **half true**: buttons ARE stripped, attachments and colour are NOT. Rule 5 split into 5a (colour is a progressive enhancement) and 5b (no broadcast may depend on a button — now confirmed by observation); **rule 4 kept unchanged and re-justified from screen readers and push notifications** | A live workspace: `conversations.history` returns the `thread_broadcast` with `attachments` intact, and a human saw the colour bar render while the buttons did not |

This ADR has now been revised four times after acceptance — by a product review, by a real
database, by a closer reading of Slack's documentation, and finally by a real Slack workspace. That
is the system working: the Decision was reached from documentation, the amendments were reached
from a product review, from a database and finally from production, and the parts of the original
argument that survived are stronger for having been attacked. Amendment 4 is the
most instructive of the four, because the documentation was simply wrong and the rule it produced
was right anyway — for a reason nobody had written down until they were forced to.
