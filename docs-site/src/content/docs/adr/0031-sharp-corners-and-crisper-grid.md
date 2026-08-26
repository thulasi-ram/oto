---
title: 0031 — Sharp corners and higher-contrast grid lines within Washi & Ink; colours unchanged
---
**Status:** Accepted · 2026-08-16 · **SUPERSEDED IN PART by
[0046](/oto/adr/0046-corners-round-again/)** (2026-08-21), which reverses this ADR's radius decision and
returns all three steps to ADR 0029's values. The border-contrast decision below is **not** reversed
and still stands · amends the radius values of ADR 0029 (not the tier structure)
and the `--oto-border`/`--oto-border-strong` values of ADR 0030 (not the rest of the palette)

## Context
The owner asked for an Enterprise Technical / Bloomberg Terminal aesthetic — or a Sharp Bento /
Swiss Modernist hybrid — for a dashboard whose whole surface is dense tables, rows, filters and
group-by controls. A first pass (since reverted) took that literally: a generic cool-slate palette
with a monochrome ink accent, replacing Washi & Ink entirely. The owner corrected that: keep Washi &
Ink's colours — the warm cream/sumi-ink light theme, the indigo Konshi Sutra dark theme, the beni
crimson-pink accent — and get the Bloomberg/Swiss Modernist read from **geometry**, not from a new
palette. That is what this ADR does.

Two structural properties carry that aesthetic, independent of hue: **sharp corners** (the rounded,
soft-edged look is the opposite of a terminal grid) and **crisp, legible grid lines** (a dense table
needs borders that read at a glance, not hairlines that all but disappear). Neither requires
touching `--oto-bg`, `--oto-surface`, `--oto-text`, or `--oto-accent` — all four, in both themes,
are byte-identical to what ADR 0030 shipped.

## Decision

**1. All three radius steps go to 0px.** `--oto-radius-chip`, `--oto-radius-control`, and
`--oto-radius-surface` — 3px/4px/6px since ADR 0029 — all render 0px. The three tiers themselves are
unchanged: a badge still reaches for `rounded-chip`, a button for `rounded-control`, a dialog for
`rounded-surface`, and the reason ADR 0029 gave for keeping three distinct names (inline vs
operable vs surface-holding-controls) is not in dispute — only the value each name carries changed.
This is an explicit override of ADR 0029's "derived from the census, not designed" methodology: 0px
was never one of the 342 literals that census was built from, and this ADR does not pretend
otherwise. `rounded-full` and `rounded-none` are unaffected, as they were always shapes rather than
scale steps.

**2. `--oto-border` and `--oto-border-strong` get darker (light) / brighter (dark), same hue.**
Both move within their existing hue family — the warm khaki/tan of Washi & Ink's light border
(~42° hue) and the indigo of Konshi Sutra's dark border (~222–226° hue) are unchanged; only
lightness and saturation shift, toward more contrast:

| Token | Theme | Was | Now | vs `--oto-surface` |
|---|---|---|---|---|
| `--oto-border` | light | `#E1D2AF` | `#D2B879` | 1.9:1 (was ~1.5:1) |
| `--oto-border-strong` | light | `#C4AE7E` | `#A3833E` | **3.5:1** (was 2.1:1) |
| `--oto-border` | dark | `#303C58` | `#435889` | 2.1:1 (was ~1.4:1) |
| `--oto-border-strong` | dark | `#46527A` | `#6476AF` | **3.3:1** (was un-tabulated) |

`--oto-border-strong` now clears 3:1 in both themes — genuinely "reads as a line" rather than a
decorative wash — while `--oto-border` stays a hairline, just a less washed-out one. `--oto-chart-grid`
moves with `--oto-border`, as it always has (§M.4/§M.5).

Both tokens remain decorative per §M.4's note: neither is the sole carrier of meaning, and any
border that *does* carry meaning is a Tier-B `-border`/`-solid` token, all of which already clear
3:1 and are untouched here.

**3. Everything else is untouched.** `--oto-bg`, `--oto-surface`, `--oto-surface-raised`,
`--oto-surface-sunken`, `--oto-text`, `--oto-text-muted`, `--oto-text-subtle`, `--oto-accent` (and
its `-hover`/`-fill`/`-border`/`-focus` siblings), every Tier-B state token, and the type/spacing
scales are exactly what ADR 0030 and ADR 0029 shipped.

## Consequences
- Every rendered corner in the product goes flush; nothing rounds except `rounded-full` shapes
  (avatars, status dots) and anywhere a component explicitly opts into `rounded-none` (already a
  no-op today).
- Table grids, panel dividers and input outlines read noticeably more defined, especially
  `--oto-border-strong`, which now clears the same 3:1 bar Tier-B borders already meet.
- §M.4/§M.5's measured-contrast tables gain one new row each (`--oto-border-strong` vs
  `--oto-surface`, both themes) and `contrast.test.ts`/`tokens.test.ts` were run green with the new
  values before this landed.
- §M.8's census table is retained as history — it still correctly explains why there are three
  radius tiers and where `chip` and `control` split — with a note that the *values* those tiers hold
  were overridden, not re-derived, by this ADR.
- The product UI and `DESIGN.md`'s marketing brand stay visually related (same washi/konshi hues,
  same beni accent) even though the product's corners and grid weight now read more technical than
  the marketing site's — that divergence is intentional: a dashboard's tables have a density and
  precision job the marketing site does not.

## Alternatives rejected
- **A generic slate/monochrome repaint** (the first pass, reverted). Rejected per the owner's
  direction: it solved the geometry problem by discarding the brand, when the geometry problem
  (sharp corners, crisp grid) doesn't require touching colour at all.
- **A smaller radius (e.g. 2px) instead of 0px.** Considered, but a partial round still reads as
  "rounded, just less so" rather than as the sharp, drafting-table geometry the owner asked for;
  0px is the unambiguous version of the same request.
- **Leave `--oto-border`/`--oto-border-strong` as-is and rely on radius alone for the technical
  read.** Rejected: a dense table's legibility depends on its grid lines being visible at a glance,
  and the previous `border-strong` (2.1:1) was tuned as a subtle decorative wash, not a working grid
  line — sharp corners alone would not have delivered "Bloomberg Terminal" density.
