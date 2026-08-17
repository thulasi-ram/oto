# 0033 — The alert filters are a toolbar above the table, not a panel in the rail

**Status:** Accepted · 2026-08-17 · amends PORTING-SPEC §5's "one chrome" rule (scope, not substance)
and retires the "nothing behind a popover" clause `FilterBar.tsx` was built on. Uses the §0.3 glyph
alphabet and the §M.2 tier rule unchanged.

## Context

The alert screen's filters have had three homes. They were a three-row strip above the table, then
an `<aside>` beside it, then — when the shell's top bar was retired in favour of a single left rail
— five stacked sections inside that rail, contributed through `~/components/SidebarSlot`.

The rail version was not wrong about structure. Every control was visible at once, each axis was a
labelled disclosure that opened by default, and what was applied was restated in words as removable
pills. It was wrong about **what a 256 px vertical column beside a table says**. Read top to bottom,
the rail offered Alerts / Groups / Settings and then, under one hairline, Views / Filter / Group by /
Sort by / Display — navigation and filtering in one plane, sharing one scrollbar. The filters read as
a place you go rather than as instruments belonging to the list, and they spent the width on the side
of the screen where the severity marks the operator actually came for were being squeezed.

The owner asked for them out of the sidebar.

## Decision

**1. The controls move into the content column, as one band above the table.** Search box first and
elastic, then the narrowing axes (Status, Severity, Cluster, Since), then — pushed to the far edge —
the arranging ones (Group, Sort) and `Clear N filters`. `AppShell`'s rail keeps navigation and the
two standing facts; the contextual zone stays, and on `/alerts` it is now empty.

**1a. Row density leaves the alert screen entirely.** It was a fifth control in that second group and
it was the odd one out: everything else in the band is a statement about *this list*, and density is a
statement about the person — written to `localStorage`, applied to every table in the product, and
outliving any filter set by design. It now lives only in the profile menu at the foot of the rail,
beside the other two preferences of exactly that kind (which theme, which account). It was offered in
both places before, sharing one signal; two controls for one fact is a bad trade even when the state
cannot diverge, and the split told the operator something untrue about what the preference is. The
menu item reads "Comfortable rows" / "Compact rows" rather than one bare adjective, because there is
no longer a labelled toggle anywhere else to learn the word from.

**2. PORTING-SPEC §5 is narrowed, not broken.** What §5 forbids is a second *rail*: a second vertical
plane competing for width and for the answer to "where am I". A toolbar is neither — it sits inside
the content column, spans exactly the table it filters, and cannot be mistaken for a destination.

**3. Four axes go behind popover menus, and this ADR pays for that explicitly.** `FilterBar.tsx`'s
governing rule was *"nothing is ever put behind a popover: 'why is this list empty?' must never be a
click away"*. Forty pixels of toolbar cannot hold twelve open controls, so that rule is spent. Three
properties replace it, all at rest, none requiring a click:

  - **A trigger states its value, not just its axis** — `Severity` when it is off, `Severity
    critical +1` when it is on, with the §0.3 severity ruler filled to the worst severity being
    filtered for. The axis name stays muted; the value carries the weight.
  - **`Clear N filters` is present whenever N > 0**, so the count of what is narrowing the list is
    never behind anything.
  - **`namespace` and `alertname` keep their removable pills** on a row beneath the band. They have
    no control anywhere — they only ever arrive from a roll-up drill-down — so for them a pill is
    not a second telling, it is the only one.

A closed Kobalte popover renders nothing, so a menu's *contents* are genuinely absent from the
accessibility tree. That cost is real and is stated rather than papered over; the three properties
above are what make it survivable, and they are pinned by their own suite
(`FilterBar.test.tsx` — "what the toolbar says without being opened").

**4. The band spends no accent at all.** An active control lifts with neutrals — a raised surface,
the stronger hairline, and a `text-ink-muted` → `text-ink` weight lift — so it reads in greyscale and
costs the severity column nothing (§0.6, §M.2). The rail's version tinted every pressed control with
`bg-accent-fill`, which put five accent marks in the chrome of a screen whose job is to make one
severity mark the loudest thing on it. The one accent left is the current *view* inside the Status
menu, the only control here that answers "where am I".

**5. The §0.3 glyph alphabet is borrowed, in Tier A ink.** The severity ruler on the Severity
trigger and inside its toggles is the same ruler the table's rows carry; the lifecycle toggles carry
the same six state shapes; Flapping carries the same square wave. All of them are drawn
`tone="inherit"`. The *shape* is the vocabulary the chrome may borrow; the *hue* is state's alone
(§M.2 Tier B), and lending it to a filter chip is how scarcity gets spent.

**6. The brand marks are the real ones, in three cuts of one drawing.**

  - `~/components/IconMark` — the ensō with the fūrin inside it, no signature.
  - `~/components/Wordmark` — the brush "oto" alone.
  - `~/components/Logo` — both, composed, the signature inside the ring.

The rail's header sets the first two side by side: 36 px ring, 20 px signature. The stacked
composition is the wrong shape for a 56 px band — it is square, so the header can only render it
small, and at that size the signature inside the ring stops resolving as writing. Split in two, the
same drawing uses the width instead of fighting the height. The composed cut keeps the login screen,
where there is room for it to be square, replacing the bare 32 px fūrin glyph that stood there.

**The icon mark is lifted out of `oto-logo-primary.svg`'s own mask, not off `oto-icon-mark.svg`.**
That file is a 1.8 MB embedded PNG: it cannot take `currentColor`, so it would need a copy per theme,
and it cannot be inlined at any size worth paying. The primary logo, however, is *composed* — its
mask holds the mark and the wordmark as two separate groups at the same 0.1-scale negative-Y potrace
emission every asset here uses, and the first of those is the mark, in its own 1375 × 1401 box, which
is exactly the viewBox `oto-icon-mark.svg` declares. Same drawing, in the form that is usable.

All three are inlined rather than referenced because they must take `currentColor` and must not be a
second request in a first paint, and all three are **single-ink** cuts rather than the gradient ones
(`oto-logo-primary.svg` and its `-dark` counterpart): one asset is then correct in both themes and
cannot drift from the palette, and §M.2 puts saturated colour under Tier B, where it means "this is
the state of an alert". A gradient on the login screen would break no rule — there is no alert on it
— but it would teach the eye, on the very first screen, that saturation in oto can be decoration.

**This retires the fūrin greeting from the chrome.** `Chime size="mark"` no longer renders anywhere,
and that glyph was what carried ADR 0028's one-shot 180 ms swing on first mount per document. The
drawn mark has no equivalent, and inventing one for it would be a new piece of brand motion rather
than the preservation of an old one. `Chime` itself is unchanged and still draws the empty states at
`glyph` size. Restoring the greeting means putting the chime back beside the lockup, at the cost of
two bells in one header.

## Consequences

- `SidebarSlot` now has exactly one occupant, settings' section list. It is kept rather than retired:
  the mechanism is sound and settings genuinely contributes destinations.
- Focus inside the band is explicitly *not* "reading the list". `routes/alerts.tsx` marks the toolbar
  with a ref and excludes it in `onFocusIn`/`onFocusOut`, or tabbing into a filter control would start
  withholding incoming alerts through the §0.5 held-alerts deferral.
- The search box's error / summary / teaching region **floats** instead of sitting in the flow. In the
  rail, growing by four lines cost nothing; in a toolbar it would push the table down every time a
  caret landed in the box. The live region stays mounted and silent; the card inside it is what comes
  and goes.
- Three tri-state controls (Ack, Snoozed, Flapping) stopped rendering blank. Their "no filter" member
  is the empty string, which Kobalte's `Select.Value` does not consider a selection, so all three had
  been showing an empty trigger in their default state. They now read their label straight off the
  filter set.
- The three band-level selects hide Kobalte's stock up/down chevron at the call site and render the
  same single caret the menus use, so seven controls from three primitives read as one set. `Select.tsx`
  itself is untouched — settings and the dialogs still get the stock icon.
- `GroupingTabs` keeps `orientation="horizontal"` while laid out as a column inside the Group menu, for
  the same reason it did in the rail: Kobalte derives the arrow-key axis from that prop, and `vertical`
  would retire ArrowLeft/ArrowRight along with the roving-focus contract the tests pin.
- `FilterBar.test.tsx` and `routes/alerts.test.tsx` grow an `openMenu()` step wherever they drive a
  control that is now behind one. This is deliberately explicit rather than hidden in the mount helper:
  the extra line at each call site *is* the cost of the decision, and it should be visible to whoever
  reads the suite next.

## Alternatives rejected

- **Keep everything expanded, inline above the table.** The honest reading of the old rule, and it
  costs ~120 px of vertical space on the one screen whose whole value is how many rows fit. Rejected.
- **Move the panel to a right-hand drawer.** Preserves the always-open sections almost verbatim and is
  the smallest diff, but it re-creates on the right the exact "two vertical planes" seam §5 retired on
  the left.
- **Fold Views into a separate trigger of its own.** A view is three axes agreeing (state, ack,
  snoozed); a separate control for it would be two controls for one fact. It sits at the top of the
  Status menu instead, and the Status trigger prefers the view's name over reciting the axes.
- **Tint active controls with `bg-accent-fill`, as the rail did.** Rejected under §0.6: five accent
  marks in the chrome of a screen that needs one severity mark to be the loudest thing on it.
- **`aria-label` on the Cluster / Since / Sort triggers.** It would override the trigger's text — and
  the text is where the *value* is. "Cluster" as an accessible name is a control that has stopped
  saying what it is filtering by, to precisely the reader who cannot see the word beside it.
