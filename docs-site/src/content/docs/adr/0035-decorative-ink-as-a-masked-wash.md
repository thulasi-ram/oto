---
title: "0035 — Decorative ink as a masked wash: one primitive, four motifs, and the placements it is refused"
---
**Status:** Accepted · 2026-08-17 · adds §M.9 to the SPEC · sits alongside ADR 0028 (which still owns
motion), ADR 0030 (the palette the tints derive from), ADR 0031 (the geometry this coexists with) and
ADR 0032 (why the accent reads differently per theme)

## Context

The brand sheet's brush existed as two inlined logo components and nothing else. `Wordmark.tsx` and
`Logo.tsx` are `currentColor` SVG elements, which is exactly right for a mark that sits in the DOM
as *content* — and useless for decoration, because a decorative stroke has to sit **behind** or
**beside** something without occupying a slot in the layout.

ADR 0031 took the interface to sharp corners and a crisp technical grid. That is the right call for
a table an operator reads at 3am, and it left the product with no surviving trace of the
ink-painting language ADR 0030 chose: `text-page` was a type step and a font weight, every empty
state was the same 32 px fūrin, and `/login` was a centred glyph on a flat cream field.

Four motifs were queued, and a design study found they all resolved to the same three lines of CSS:
a CSS mask over a Tier-A token fill. There was no token for that fill, no utility for the mask, and
no guard for what the mask does under forced colours. Landing the four independently meant four
hardcoded tints that drift apart — none of which `TestNoStateHueInChrome` would catch, because a
decorative `rgba()` is not a state hue.

## Decision

**1. Three derived tints, in a bare `:root` block.** `--oto-wash` (6% of `--oto-text`),
`--oto-wash-heading` (12% of `--oto-border-strong`) and `--oto-wash-heading-accent` (12% of
`--oto-accent`), each written as `color-mix(in oklab, var(--token) N%, transparent)`.

Two properties of that placement are load-bearing rather than incidental. **Derived, not themed:**
the mix names a token that *is* themed, so one declaration is correct in both palettes and the two
cannot drift. **Outside every `[data-theme]` block:** `tokens.test.ts` reads only theme-prefixed
rules, so nothing here is asked for a dark counterpart, and `contrast.test.ts` demands a measured
pair for every token in §M.4/§M.5 — which a decorative tint has no way to supply, because its
obligation is a *composite* and not a pair. The same arrangement `--oto-row-h` and the §M.8 type
steps have had since U6 and ADR 0029.

**2. One `@utility`, `oto-ink`, carrying the whole CSS half of the accessibility contract.** The
fill, the mask layers, `pointer-events: none`, `user-select: none`, and `display: none` under
`forced-colors: active`. The last is the one everybody misses and the reason this is a primitive
rather than a convention: under forced colours the OS overrides the tint to a system colour at full
strength, and a 6% wash becomes an opaque slab across the panel. A per-motif copy of these lines is
three chances to omit it.

**3. One component, `Ink.tsx`, carrying the other half.** `aria-hidden`, which no stylesheet can
set, and the `/motifs/` URL as a typed union, so a misspelt motif is a compile error rather than a
mask that resolves to nothing and paints a flat rectangle of wash.

**4. The contrast guarantee is geometry, not a carefully chosen opacity.** Where ink shares a screen
with text it is not made faint enough to be safe — a second mask layer, a horizontal gradient
transparent across a fixed centre column, is intersected with the art (`mask-composite: intersect`)
so the ink is *incapable* of entering the column the text is centred in, at every viewport width and
with no media query.

This is the decision with the most evidence behind it. `--oto-text-subtle` is 4.90:1 on `--oto-bg`
in light and **4.37:1** under a flat 6% wash — an AA failure, in one theme only, at the weight the
wash was supposed to be safe at. Nothing in CI would have caught it: `contrast.test.ts` measures
token pairs and not composites, and the axe row that would see it is the one UNWRITTEN entry in
§M.7. §M.9 tabulates that failure as a ❌ row and `ink.test.ts` recomputes it, so the number that
justifies the geometry is itself a gate rather than a claim.

**5. Four placements permitted, and the refusals are the design.** The door; a page heading; a
full-page empty state; and nothing else. `PanelHeader`/`PanelTitle`/`SECTION_LABEL` are refused
because alert detail stacks six panels and at six a gesture becomes a texture — which is also why
the shared `EmptyState` did not grow a `motif` prop but got a separate `PageEmptyState` beside it:
six of its eighteen call sites are sub-panels on one alert. Body copy, tertiary text, rows, chips
and status surfaces are refused outright.

**6. `sakura` is gated to `expired`, and that required the product to grow a sentence.** `expired`
is the one state whose meaning is transience (§M.1: *"oto stopped hearing about this"*, never
*"resolved"*), and an empty alert list filtered to it used to borrow the copy a typo'd cluster name
gets. `isExpiredOnly()` now tells the two apart and the screen says which one it is — the motif is
the second channel beside that sentence (U1), never a substitute for it.

## Consequences

- `web/public/` stops being an empty directory with a 0-byte `.gitkeep` and becomes
  `web/public/motifs/` — five assets and a README that records, beside the files, the three things
  that are easy to get wrong about a mask (`preserveAspectRatio`, opaque-black fill, two shapes not
  one stretched one).
- `/login` gains atmosphere and the form stays **bare** — no `bg-surface` plate. Plating it is the
  trivially safe way to guarantee contrast and it turns a threshold into a card.
- Two page headings (`alert-detail`, `group-detail`) carry a brush; the other pages have no
  `text-page` heading to put one behind, so nothing there changed.
- Five full-page empty states carry kumo and one carries sakura. The blocked-filter state on
  `/alerts` is a full-page empty state and deliberately gets **no** motif: kumo means "nothing is
  wrong", and there a filter is wrong and there is a fix two lines below it.
- Two new gates (`ink.test.ts`, `Ink.test.tsx`), both reading off disk, both listed in §M.7.
- Nothing new animates. U9's decorative one-shot budget is unchanged and still spent by the fūrin.

## Alternatives rejected

- **A hardcoded `rgba()` per motif.** The status quo, and the reason this ADR exists. No gate sees
  it (it is not a state hue), and it is wrong the moment the theme flips.
- **An `<img>` or a second inlined `<svg>` per motif.** An `<img>` cannot take `currentColor`, so it
  needs one file per theme; an inlined `<svg>` occupies a slot in the layout, which is the one thing
  decoration must not do. `Logo.tsx` is 72 KB of path data and inlining it a second time for the
  login wash would have doubled that for a shape nobody reads.
- **A `motif` prop on the shared `EmptyState`.** Rejected on arithmetic: eighteen call sites, six of
  them sub-panels on one page.
- **Choosing an opacity low enough to look safe, instead of a carve-out.** Rejected with a number —
  see decision 4. It is not that a lower opacity could not be found; it is that nothing in this
  repository could prove one had been.
- **One stretched asset for both heading shapes.** A mask has no fixed aspect, so it is possible.
  Stretching the thin tapered rule to heading height produces a shape that reads as a wave rather
  than as a stroke.
- **Kumo as a group boundary.** Built twice and rejected twice; recorded in §M.9 and in git-bug
  `2a64686` so it is not re-derived. It is a taste decision rather than a rules one — the band is
  Tier A and carries no fact — and nothing here forbids a third attempt that starts from what the
  first two learned.
