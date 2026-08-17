# 0034 — Notifications is a destination, and the record it keeps is readable

**Status:** Accepted · 2026-08-17 · extends ADR 0033's rail (a fourth destination, same rules) and
gives `GET /api/v1/notifications` its first reader. Uses the §M.2 tier rule and §E.3's cursor rule
unchanged.

## Context

Two facts sat next to each other and neither was doing its job.

**Notification policies were a settings band.** They were the third of five sections under
`/settings`, between the channel list and the access tokens. That filing says a policy is
configuration — something you set up once and then forget, like a PAT or a tuning constant. It is
not. A policy answers *"who hears about this?"*, which is a question asked while looking at an alert
list, by the same person, in the same minute. Two clicks and a gear icon away from the screen that
provokes it is the wrong distance.

**The notification history had no reader at all.** oto records every notification intent it forms,
including — especially — the ones it decides *not* to send, with the reason it took that decision:
`no_policy`, `throttled`, `snoozed`, `storm`, `flapping`, `verbosity`, `channel_disabled`,
`duplicate_render`. That record is the entire answer to §B.6's requirement that **oto's silence must
never be indistinguishable from "no alert"**. It is written on every dispatch, it is retained, it is
served by `GET /api/v1/notifications` with keyset paging and six filter axes — and the web app called
that endpoint from nowhere. The only reader outside `psql` was the per-alert "Who was told" panel,
which answers the question for one alert at a time and only if you already know which alert to open.
An operator asking "has oto gone quiet on us?" had nowhere to look.

The owner asked for policies out of settings, and for the history to be shown.

## Decision

**1. `/notifications` is a top-level destination, with two sections.** `Policies` (the editor, moved
verbatim from `/settings/policies`) and `Activity log` (new). The primary rail now reads Alerts /
Groups / Notifications / Settings. The nav entry lands on Policies rather than the log because a
first-run org has no policies, and every row the log could show them would say `no_policy` — the
screen that explains that is the one to arrive on.

**2. The sections are drawn INSIDE the primary nav, indented under the destination that owns them —
and the contextual zone is retired.** ADR 0033 kept a separate zone at the foot of the rail, behind a
hairline, for "what genuinely reads as destinations", with settings' section list as its only
occupant. Adding a second occupant is what exposed the flaw: the rail read Alerts / Groups /
Notifications / Settings, then a rule, then Policies / Activity log — and **nothing on screen said
which of the four owned the last two.** They read as a second, peer-level list that happened to change
when you navigated. One occupant hid that, because "the sections of the screen you are on" is a
guessable rule when there is only ever one screen with sections.

Nested, the containment *is* the answer and the hairline is unnecessary — a rule drawn between a
parent and its own children would be arguing against the indent. `SidebarSlot`'s mechanism is
unchanged (a screen still hands its list to a shell that must never remount); only where the shell
puts it changed. The shared `SubNavLink` owns the appearance, so settings' list and notifications' —
which occupy the same pixels on different paths — cannot drift apart.

**2a. An expanded parent gives up both its accent rail and its `aria-current`.** Its child takes them.
Two accent rails stacked in one column is two answers to "where am I", and §0.6 spends saturation on
one thing at a time; two nodes claiming `aria-current="page"` announces two destinations for one
location. The parent still reads as where you are — `text-ink` and medium against three muted
siblings — it simply stops being the *precise* answer once a finer one is on screen.

**2b. The rail links to the bare `/notifications` and `/settings`, which redirect to their first
section.** This is a real constraint and not tidiness: `<A>` sets `aria-current="page"` itself on an
exact href match and `mergeProps` puts its own binding last, so a parent whose href was
`/notifications/policies` re-stamped the attribute no matter what the shell passed. An href the
location never exactly equals hands the decision back to `AppShell`, where the rule above is written
down. The routes take `:section?` and redirect the bare path.

**3. `/settings/policies` still resolves, and redirects.** That URL was linkable for the whole life
of the screen. A bookmark landing on "That page does not exist." teaches an operator that oto loses
things, which is the opposite of what the product is for. `routes/settings.tsx` carries a `MOVED`
table rather than an `if`, so the next section to move adds a line.

**4. The activity log shows suppressed intents as first-class rows, with a sentence each.** Not a
footnote, not behind a filter, not a status badge reading `suppressed` and nothing else. Every
suppression reason is rendered as English — "the per-subject throttle was already spent", "a person
asked oto to hold its notifications for this alert until a fixed time" — because the wire token *is*
the failure mode this record exists to prevent: a log that says `throttled` and stops has recreated
the silence one layer up. `ActivitySection.test.tsx` asserts every reason the contract publishes
reaches a sentence, derived from the generated enum rather than a list typed into the test.

**5. No `suppressed_reason` filter is offered, and the absence is deliberate.** The server's
allow-list for that query parameter is narrower than the enum the rows carry: `snoozed` is a
suppression oto *records* and not one it *accepts as a filter*. A control built from the enum would
offer a choice that 400s; one built from the narrower list would be a filter silently missing the
most useful reason on it — a silence a person chose. Until the two agree, the reason is shown on
every row and filtered on none. **This is a defect on the server side of the contract, written down
rather than papered over in the UI.**

**6. The log is live, and rate-limited at the cache entry rather than at the invalidation.** All
three frames that can mint or move an intent — `alert.upserted`, `event.appended`,
`delivery.updated` — invalidate `["notifications"]`. `delivery.updated` alone would have been the
tidy choice and the wrong one: a suppressed intent leaves no delivery behind, so precisely the rows
this screen exists for would never have announced themselves. The cost is paid where
`recentAlertsQuery` already pays it — a `staleTime` that resolves to `"static"` for twenty seconds
after each answer, so a storm costs one request per window rather than one per frame. Nothing an
operator does here turns on the log being right to the second; it is read to answer a question about
intents that have already happened.

**7. Tier A only (§M.2).** A `failed` intent is serious and it is still not the state of an alert.
Borrowing a state hue here would spend exactly the scarcity that makes a firing row loud. Weight, the
stronger hairline and unambiguous words carry it — including the status vocabulary, where `partial`
reads "some channels only" (the state most easily mistaken for delivered) and `suppressed` reads
"deliberately not sent" rather than anything that sounds like a failure.

**8. The channel picker on a policy becomes a searchable multi-select.** A checkbox per channel is
the right control at six channels and the wrong one at forty: the dialog grows a scroll region whose
only navigation is the eye, and the fields under it — the facts, the dry run, Save — get pushed below
the fold of a modal by data the operator does not control. The replacement is `@kobalte/core/combobox`
in `multiple` mode, themed as a sibling of `Select.tsx`, with three properties held:

  - **What is already chosen stays visible with the listbox shut**, as removable chips. A picker that
    hid the current selection would be a worse control than the wall of checkboxes it replaced,
    however much shorter. A policy's destinations are the answer to "where does this go".
  - **The search matches the type and the verbosity, not just the name.** The row reads
    `#platform-oncall webhook status_changes`; filtering on one of those three words and then failing
    to find the row by the other two is a control that lies about what it can do.
  - **A channel deleted under an open dialog is carried through the save untouched.** The picker can
    only ever hand back channels it offers, so mapping its answer straight onto the form would delete
    a destination that merely could not be named — a silent edit, on the field that decides who hears.

**9. The cap is stated rather than enforced by disabling.** The checkbox list could grey out rows past
`channel_ids`' `maxItems` because every row was on screen to be greyed; a picker you type into cannot
tell you about a row you have not found yet. The selection is accepted and the contract's own sentence
appears under the control with Save disabled behind it. The bound is still discovered *in the dialog*
rather than as a 422 after Save, which is the property that matters.

## Consequences

- `features/settings/PoliciesSection.tsx` moves to `features/notifications/`. It keeps importing
  `features/settings/rhythm` — deliberately shared, not copied: this dialog and the channel dialog
  are the same object drawn twice, and a second set of gap constants is the drift `rhythm.ts` ended.
- The reason and suppression vocabularies move to `features/notifications/vocabulary.ts`, shared by
  the activity log and the alert detail's "Who was told" panel. `PoliciesSection` keeps its own reason
  map on purpose: these label things that *have happened*, read beside a timestamp ("the rule
  changed"), and the editor labels the same enum as things a policy *may be told about*, read as the
  text of a toggle ("rule changed"). One map cannot be right in both places.
- `qk.notifications` is a key root of its own rather than a segment under `["alerts"]`. A row here is
  an intent, not an alert read another way, and filing it under alerts would mean every storm frame
  invalidated the history feed as a side effect of a row moving.
- `web/src/api/endpoints.ts` gains `listNotifications`. `listDeliveries` has been sitting there,
  unread by any screen, since the contract was written; it stays unread — a delivery is retried from
  the alert it belongs to, where the context to judge that lives.
- The nav is four entries where §5 of the porting spec described three. The rule §5 states is about
  there being one navigational *plane*, not about how many rows are in it.
- ADR 0033's "the contextual zone stays, and on `/alerts` it is now empty" is superseded by point 2:
  the zone is gone, the slot is not. `/alerts` contributes nothing and now simply has no children
  under it, which needs no wrapper to hide.
- `App.test.tsx` grows a suite for the containment, because the failure it guards is invisible to any
  check that merely finds the link: every link was present and worked in the broken shape. It asserts
  the sub-links are *inside* `nav[aria-label="Primary"]`, that there is exactly one navigation
  landmark (a screen contributing its own `<nav>` would announce the links twice), and that exactly
  one node claims `aria-current`.

## Alternatives rejected

- **Leave policies in settings and add only the log.** The smallest diff, and it would have put the
  log — a pure operational read, the same kind of object as the alert list — behind a gear icon,
  beside the access tokens. The two screens belong together; the question is which side they move to.
- **Make the activity log a tab on the alert list.** Tempting, because that is where the question is
  asked. Rejected: the log is not a list of alerts and does not filter like one, and a tab strip that
  swaps between two tables with different axes and different row shapes reads as one screen having a
  breakdown.
- **A `notification.created` frame, so the log could be live without riding the alert frames.** The
  honest fix, and a server change: a new `UiEventKind`, a publisher on the dispatch path, a contract
  amendment (§N). Worth doing, and not worth blocking a read-only screen on. The rate limit at the
  cache entry makes the interim behaviour indistinguishable to the operator.
- **Fetch each row's alert so the log can name it.** The list response carries ids and no names by
  design — a per-row roll-up would be a query per row server-side. The row links out instead, and the
  alert page is where the sentence is. Resolving fifty alerts client-side to decorate a history feed
  would be the `batchGetRuleSnapshots` pattern applied to a screen that has not earned it.
- **Keep the checkbox list and add a filter box above it.** Preserves "everything visible at once"
  and costs the vertical space anyway. The chips do the same job in one row instead of forty.
