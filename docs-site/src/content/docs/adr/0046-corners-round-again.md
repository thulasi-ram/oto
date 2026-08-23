---
title: "0046 — Corners round again: ADR 0031's radius override is reversed"
---
**Status:** Accepted · 2026-08-21 · reverses the radius decision of ADR 0031 (not its border-contrast
decision, and not ADR 0029's tier structure)

## Context

ADR 0031 took all three radius steps to 0px in service of an Enterprise Technical / Bloomberg
Terminal read, and it worked as geometry: every corner in the product went flush. Living with it
on the filter bar surfaced the cost. `/cases` and `/alerts` both lean on a row of small controls —
`FilterMenu` triggers, `Select` triggers, `Tabs` — sitting directly on the content they narrow, and
sharp-cornered rectangles at that size and density read as **more** chrome than the same shapes
rounded would, not less: a flush corner on a 28px pill draws a harder edge than the content beside
it needs. The technical-grid read ADR 0031 wanted from borders and grid lines survives; the part of
it that lived in the corner itself did not earn its keep at control scale.

This is a full reversal, not a partial one. Rounding only the chip-scale controls while leaving
buttons, inputs, and panels flush would revive the exact failure mode ADR 0031's own alternatives
section rejected in the other direction — "a smaller radius reads as rounded, just less so" applies
symmetrically to "some things round and some don't." The system's `chip`/`control`/`surface` tiers
exist to keep one radius decision consistent across every call site of a given kind; carving out
one tier's *value* by screen would break that guarantee rather than exercise it.

## Decision

**All three radius steps return to the values ADR 0029's census derived them from.** Not a new
scale, not a re-derivation — the same 3px/4px/6px §M.8 has documented as history since ADR 0029,
which ADR 0031 overrode without discarding:

| Token | ADR 0029 (census) | ADR 0031 | ADR 0046 (this one) |
|---|---|---|---|
| `--oto-radius-chip` | 3px | 0px | **3px** |
| `--oto-radius-control` | 4px | 0px | **4px** |
| `--oto-radius-surface` | 6px | 0px | **6px** |

The tier structure, and every call site's choice of which tier to reach for, is unchanged — a badge
still reaches for `rounded-chip`, a button for `rounded-control`, a dialog for `rounded-surface`.
Only the value each name renders moves back.

**ADR 0031's border-contrast decision is untouched.** `--oto-border` and `--oto-border-strong` stay
at the higher-contrast values ADR 0031 set — a dense table's grid lines reading clearly at a glance
is independent of whether the cells' corners are sharp, and nothing here revisits it.

## Consequences

- Every rendered corner in the product softens slightly: buttons, inputs, chips, badges, dropdowns,
  cards, dialogs. `rounded-full` shapes (avatars, status dots) and explicit `rounded-none` opt-outs
  are unaffected, as they were always shapes rather than scale steps.
- `scales.test.ts` reads `--oto-radius-*` off `tokens.css` and cross-checks it against §M.8 of
  `docs/design/SPEC.md` — both were updated together, so the gate stays green without needing its
  own change.
- The `features/linear-proto/*` sandbox keeps its own hardcoded `rounded-none` throughout, per its
  own `--lp-*` token set; it is a separate design surface (see `Keycap.tsx`'s own note) and is not
  in scope here.
- §M.8's amendment note now points at two ADRs in sequence rather than one — the census values, then
  ADR 0031's override, then this reversal — which is the accurate history rather than a rewrite of
  it. A future radius change amends this one the same way, rather than deleting the trail.

## Alternatives rejected

- **Round only the filter/tab controls, leave buttons/inputs/panels at 0px.** Rejected: it solves
  the one screen that prompted the question while leaving every other control inconsistent with it,
  which is the "mix sharp and soft randomly" failure the token scale exists to prevent. A dashboard
  where the filter bar is soft and the button beside it is sharp reads as two systems, not one.
- **A radius between 0px and the ADR 0029 values (e.g. 2px everywhere).** Considered, but there is
  no census or product reason behind a new number — 3px/4px/6px is what the actual call sites in
  this product were already using before ADR 0031 touched them, and re-deriving a fourth value with
  no evidence behind it is exactly what ADR 0029 was written to stop doing.
- **Leave ADR 0031 in place and address the filter bar's heaviness with tint/spacing/border changes
  alone.** Partially done already — the filter triggers dropped their border and gained a
  translucent tint in the same pass — but it did not resolve the complaint on its own: a flush
  corner on a small filled pill still reads harder-edged than the same pill rounded, independent of
  whether it also has a border.
