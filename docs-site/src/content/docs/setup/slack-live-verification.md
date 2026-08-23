---
title: The live Slack verification run
---
**Who this is for:** one person with a Slack workspace they can install an app into,
a phone with Slack on it, and about thirty-five minutes. **Nothing in this document
can be done by an agent, a test or a fake.** That is the whole reason it exists.

**What it settles.** Four things oto claims about Slack have never been watched
happening: the in-place `chat.update` that
[ADR 0008](/adr/0008-slack-update-in-place-primary/) makes load-bearing, an
`@`-mention that actually reaches a phone, the resolved and snoozed cards, and
whether Slack's four clients agree with each other —
[ADR 0020](/adr/0020-broadcast-the-transitions-that-must-be-seen/) Amendment 4
records that last one as unknown in writing. Each step below names the unknown it
discharges.

> **An unknown is discharged by an OBSERVATION, not by a run finishing.** Every
> step has an *expect exactly* line. If you did not see that, the step failed, and a
> failed step is a finding — the most valuable output this document has. Amendment 4
> exists because a live run contradicted Slack's own documentation; that is the
> normal outcome, not the alarming one.

## Before you start

1. Install the app and invite it: [slack.md sections 1–3 and 5](/setup/slack/). Use a
   **scratch channel** — you are going to leave test cards in it. Interactivity
   (section 3) is required for step 4.
2. **Removed.** This step set the reminder audience and is gone with step 8 (git-bug
   `bd0fb1d`): oto no longer sends an unacked reminder and has no mention surface.
3. Open the checked-in payloads in a second window. They are what oto sends, byte
   for byte, and they are what you compare against:
   - `internal/channels/render/slack/testdata/*.golden.json` — the renderer's own
     fixtures, including `root_snoozed.golden.json` and `reply_snoozed.golden.json`.
   - `test/fixtures/slack/*.message.json` — the same cards in a human-reviewable
     corpus, plus `*.blockkit.json` for
     [Block Kit Builder](https://app.slack.com/block-kit-builder).
   If a card on screen disagrees with its fixture, the fixture is the record of what
   was sent and the screen is the record of what Slack did with it. Both matter and
   they are different facts.
4. **Do not add screenshots or any other binary to the repository.** Record what you
   saw as text, in the ADR (step 11). A screenshot of a card is not evidence anyone
   can diff.

---

## 1. Metadata is accepted from a bot token — do this first

**Do:** fire one synthetic alert into the scratch channel. Look at the delivery
record in oto (Deliveries view, or `notification_deliveries`).

**Expect exactly:** the delivery succeeded and a card is in the channel.

**Discharges:** nothing on its own. It is a **gate**: Slack lists
`metadata_must_be_sent_from_app` on both write methods with the text *"message
metadata can only be posted or updated using an app-level token"*, and oto attaches
`metadata` to **every** card under an `xoxb-` bot token (`rootMetadata` in
`internal/channels/render/slack/root.go`).

**If it fails** with `metadata_must_be_sent_from_app`: stop. No oto card has ever
been deliverable, every step below is moot, and `rootMetadata` has to go. This is
the highest-value five seconds in the document.

## 2. The root card renders

**Do:** look at the firing card in the channel.

**Expect exactly:** the seven blocks of `root_firing.golden.json` —
a **bold, clickable** title with the cluster as a separate grey chip, a two-column
field grid, the rule expression in a code span showing a literal `>` (not `&gt;`),
three buttons and a `…` overflow — **plus a red `#a30200` bar down the left edge**.

**Discharges:** nothing that is open. It is the baseline the next nine steps are
read against, and the colour bar is the one thing
[Block Kit Builder cannot show](/setup/slack/#71-five-minutes-no-workspace-admin-needed-block-kit-builder).

**If it fails:** no colour bar means attachments have stopped rendering, and §H.2's
peripheral-vision answer to *"do I need to act?"* is gone. That is an ADR 0008
finding and it invalidates rule 5a in ADR 0020 Amendment 4.

## 3. A reply is threaded under the root, and stays out of the channel

**Do:** trigger a thread reply — press **Acknowledge**, or add a comment in oto.
Look at the channel body **and** the thread.

**Expect exactly:** the reply is **under** the root card in the thread, in order,
and does **not** appear as a top-level channel message.

**Discharges:** *"a threaded reply posted under a root card"* — listed in git-bug
`2078a07` as never observed.

**If it fails:** a reply in the channel body means `thread_ts` was omitted; replies
nested under each other mean oto threaded off a reply's `ts` instead of the root's
(`threadRoot` in `internal/channels/providers/slack/channel.go`).

## 4. `chat.update` edits the root in place

**Do:** with the thread **closed**, watch the channel while you acknowledge and then
resolve the alert. Then read oto's `channel_threads` row for that group.

**Expect exactly:** **one** message in the channel, whose colour and content change
under you — red `#a30200` → amber `#daa038` → green `#2eb886`. No new message
appears. No notification or unread badge accompanies the change. The `ts` in
`channel_threads` is the same string before and after.

**Discharges:** ADR 0008's central claim — *"`chat.update` on the existing root is
the primary mechanism"* — which has never been watched happening. The silence you
observe is also the direct evidence for ADR 0020's premise that `chat.update` is
*"completely silent"*.

**If it fails:** a second card means the update path fell back to posting. A card
that stops changing means the update is being refused — check the delivery record
for `cant_update_message` or `edit_window_closed`.

## 5. `chat.update` after the token changes

**Do:** rotate the bot token on the oto channel (reinstall the app, paste the new
`xoxb-`), then cause one more update to the **same** root — resolve it, or add a
member to the group.

**Expect exactly:** one of two outcomes, and you must record **which**:
either the edit succeeds, or the delivery fails and oto posts a **fresh root card
carrying the `_continued from an earlier card_` footer** while the old card is left
untouched.

**Discharges:** *"`chat.update` on a root posted by a different token"* — listed in
`2078a07` as never observed. Slack's own guidance is that only the app that posted a
message may edit it; what is unknown is which error code arrives and whether oto's
recovery is the one an operator wants.

**If it fails** in a third way — the old card is mangled, or the delivery is retried
twelve times — that is a classification bug in
`internal/channels/providers/slack/errors.go`. Name the code you saw.

## 6. The resolved card is a receipt

**Do:** look at the final state of the root card from step 4, in the channel, as
somebody scrolling past would.

**Expect exactly:** `root_resolved.golden.json` on screen — green bar, `Status`
reading `~Firing~ → Resolved` with **real strikethrough** and not literal tildes,
the rule expression still present, both instances still listed, the four receipt
fields (`Resolved`, `Instances affected`, `Notifications`, `Acknowledged`), and the
state trail as **one** context line with `→` separators and times **in your own
timezone**, not UTC.

**Discharges:** *"the resolved card variant"* (`2078a07`). It also proves or breaks
§H.4's strikethrough and the `<!date^…^{time}|fallback>` token, neither of which has
been rendered by Slack.

**If it fails:** literal `~` means Slack's strikethrough is not single-tilde. A raw
`<!date^…>` on screen means the token or its fallback is malformed — which would
mean every timestamp on every oto card is wrong.

## 7. The snoozed card — expect it to disappoint you

**Do:** fire a fresh alert. Snooze it in oto (`POST /api/v1/alerts/{id}/snooze`, or
the snooze control on the Case). Watch the channel and the thread.

**Expect exactly:** the root card stays **red** and says nothing about the snooze,
and the thread gains a line reading `:information_source: *<name>* — :fire: Firing`.
That is `root_snoozed.golden.json` and `reply_snoozed.golden.json`, which are checked
in **because the bytes are wrong**: the Slack renderer has no `snoozed` branch, so
the announcement whose entire purpose is to say *"oto is going quiet about this"*
renders as an ordinary firing update.

**Discharges:** *"the snoozed card variant"* (`2078a07`) — by confirming that the
variant does not exist rather than by finding it acceptable. What you are checking is
Slack's part: that the card and the reply are **delivered and legible**, so the fix is
a rendering change and not a delivery one.

**If it looks fine to you:** say so, and say why. That is a product judgement and it
belongs in the issue, not in this document.

## 8. Removed — the mention reaches a locked phone

**This step is deleted with the feature it tested** (git-bug `bd0fb1d`). oto no
longer sends an unacked reminder and has no mention audience, so there is nothing
to observe.

**It is worth recording what was never discharged.** This step existed because
ADR 0020 **Amendment 3** shipped `unacked_reminder_mention` on the strength of
Slack's documentation alone, and Amendment 4 is the standing proof that Slack's
documentation can be half wrong. It was **never once run** (`2078a07`) — so the
question *"does an `@`-mention from a broadcast reply actually notify a locked
phone?"* was open for the whole life of the feature and is now closed by deletion
rather than by evidence. That is the honest reason removing it was cheap: nobody
could show it worked.

⚠️ **If any mention surface is ever added back, this step comes back with it**, and
the three observations it demanded are the right ones: visible in the channel body
and not only the thread; a push notification on a locked screen; and that
notification reading as a complete sentence rather than a truncated one.

## 9. The in-channel broadcast copy

**Do:** look at the reminder from step 8 **in the channel body**, not in the thread.

**Expect exactly:** record three yes/no answers — does the **colour bar** show, do
the **buttons** show, does the **top-level text** show in full?

**Discharges:** it re-observes ADR 0020 Amendment 4's data point 3, which is the one
piece of evidence in this repository that contradicts Slack's own documentation
(*"the reference cannot contain attachments or message buttons"*). Amendment 4's
ruling is that the attachment survives and the buttons do not.

**If it disagrees with Amendment 4:** that is the most consequential finding
available here. Rule 5b (*no broadcast may depend on a button*) is grounded in the
buttons being absent; if buttons appear, an operator can press an Acknowledge that
may or may not work, and 5b needs re-arguing.

## 10. Client parity — the step that is the unknown

**Do:** open the **same two messages** — the resolved root card from step 6 and the
in-channel broadcast from step 9 — on **all four clients**: macOS/Windows desktop,
`app.slack.com` in a browser, iOS, Android. Borrow a phone if you have to; three
clients is not this step.

**Expect exactly:** fill in every cell of this table and put it in the ADR verbatim.

| | desktop | web | iOS | Android |
|---|---|---|---|---|
| Resolved card: colour bar visible | | | | |
| Resolved card: strikethrough renders | | | | |
| Resolved card: trail times in local zone | | | | |
| Broadcast copy: colour bar visible | | | | |
| Broadcast copy: buttons visible | | | | |
| Broadcast copy: full top-level text | | | | |
| Card wrapped / truncated / behind "show more" | | | | |

**Discharges:** ADR 0020 Amendment 4, *"What is still unknown"*, in its own words:
*"Desktop, web, iOS and Android have not been compared… Any future claim that a
broadcast 'shows X' needs all four, or it is a claim about one person's laptop."*
The last row is the one ADR 0008 named as an accepted risk and never checked —
Slack warns that legacy attachment content *"may be wrapped, truncated, or hidden
behind a 'show more'"*.

**If a cell disagrees with another cell:** that is the answer, and it is a bigger
result than four agreeing cells. A colour bar that renders on desktop and not on iOS
means rule 5a's *"progressive enhancement"* framing is doing real work and nothing
may ever depend on colour.

## 11. Write it down, in the ADRs, as observation

**Do:** while it is still on screen, record what you saw.

- **ADR 0020**, a new **Amendment 5**: the step 10 table verbatim, plus the step 9
  answers. If you compared fewer than four clients, **re-record the unknown with
  what you actually compared** rather than narrowing the claim — Amendment 4's
  instruction is explicit about this.
- **ADR 0008**: the step 4 and step 5 outcomes, dated, in the same voice the ADR
  already uses for *"three verified facts"*. Update in place has been the primary
  verb since 2026-08-07 on the strength of rate-limit documentation alone; step 4 is
  the first time anybody has watched it work.
- **[slack.md](/setup/slack/)**: anything in section 7 or in Troubleshooting that turned
  out to be untrue. A setup document that describes behaviour nobody has seen is the
  thing this run exists to fix.
- **git-bug `2078a07`**: the step numbers that passed, the step numbers that failed,
  and the error codes verbatim.

> **Do not mark an unknown discharged that you did not observe.** A step you
> skipped is a step nobody has done, and an ADR that claims otherwise is worse than
> one that admits the gap — the whole point of Amendment 4 is that a confident
> sentence with no observation behind it survived four revisions.

---

## What this run still cannot settle

Listed so their absence is not mistaken for a pass. None has a documented answer and
none has an offline test; [slack.md §7.4](/setup/slack/#74-what-is-still-unverifiable-after-all-of-the-above)
carries the same list with the reasoning.

- **The attachment block limit.** Slack documents 50 blocks per *message* and states
  nothing for an attachment's blocks. oto applies 50 anyway; its own budget is seven.
- **`metadata_too_large`.** The error is documented, the size is not, anywhere. oto
  guesses 8 000 bytes and its own metadata is three short scalars.
- **Total request size.** Undocumented. oto guesses 100 000 bytes.
- **Whether a broadcast reply counts against the workspace-wide posting limit.** oto's
  limiter models per-conversation only.
- **Whether deleting a broadcast reply removes the channel reference.** oto requests
  no scope that could try, so broadcasting stays a one-way door.
