---
title: 0029 — Type and radius scales, read off the product rather than drawn for it
---
**Status:** Accepted · 2026-08-14 · radius VALUES (not the three-tier structure) overridden by
ADR 0031 (2026-08-16) — see *How to overturn this* for why the census below still stands as history
**Decided WITHOUT the owner.** See *How to overturn this*, below. The ticket set the shape of the
answer and it is honoured rather than relitigated: derive the scale from what is in use, resolve the
stragglers deliberately, and land a gate — *"a half-migration that leaves both spellings live is the
worst outcome"*.
**Relates to:** [0012](/adr/0012-pastel-chrome-saturated-state/) (the two-tier palette — the axis that
had names and a gate from the first commit, and the model this follows)
**Amends:** SPEC §M.3 (new rule **U10**), §M.7 (a ninth row), and adds §M.8
**Resolves:** git-bug `fe858fe` — *"The token system covers exactly one axis: 285 font sizes and 57
radii are bracket literals with no token behind them"*

## Context

`index.css`'s `@theme inline` block mapped every colour the design system owns — 15 chrome tokens,
24 state tokens, 7 chart tokens — and defined no `--text-*` and no `--radius-*`. Font size and
corner radius were the two axes with no vocabulary, so every component invented a px literal in a
bracket. At the time the ticket was filed: **285** `text-[Npx]` across 32 files and **57**
`rounded-[Npx]`. Two days later, when it was implemented: **309** and **59**. That growth rate is
the finding — an axis nothing watches does not stay still.

Not one named size utility (`text-xs`, `text-sm`, `text-base`, …) appeared anywhere in `web/src`,
so this was not a system with an escape hatch. It was 342 independent decisions with no system.

**The histogram is what decided the shape of the fix.** A long tail of one-off values would have
been a different problem — an argument about what the scale should be. The census says otherwise:

| Axis | Values in use | Concentration |
|---|---|---|
| font size | 11 px ×183, 12 px ×75, 13 px ×23, 10 px ×20, 14 px ×4, 15 px ×2, 18 px ×2 | four values carry 301 of 309 |
| radius | 4 px ×28, 3 px ×26, 6 px ×3, 8 px ×1, 2 px ×1 | two values carry 54 of 59 |

An unnamed scale already existed and was being followed. What was missing was the name, and with it
any way to tell a deliberate value from a typo: `text-[15px]` beside 183 uses of `text-[11px]` reads
as neither a decision nor a mistake, because there was nothing for it to be off.

**The one thing the ticket asserted that the evidence did not support** is the 3 px/4 px split,
described there as *"one tier written two ways"*. Reading all 54 call sites, they are two tiers and
every one of them is already on the correct side: 3 px sits on inline things that live inside a line
of text (badges, chips, code spans, skeleton bars), 4 px on things you operate or that hold
something (buttons, inputs, wells, nav items, bordered boxes). Collapsing them would have been a
visible regression justified by a sentence, so they are kept as two named steps and the rule that
was implicit is now written down.

## Decision

**1. Six type steps and three radius steps, in `tokens.css`, republished through `@theme inline`.**
The same two-file shape the colour axis uses: `--oto-*` holds the value, `@theme inline` publishes
the Tailwind namespace. §M.8 tabulates both scales with what each step is for.

**2. They are NOT theme tokens.** They sit in a bare `:root`, outside every `[data-theme]` block, so
`tokens.test.ts` — which reads only theme-prefixed rules — does not ask dark mode for its own 11 px.
A palette is a property of the theme; a type step is not. `--oto-row-h` has had this arrangement
since U6.

**3. The four straggler values are resolved, not preserved.** Each was one or two occurrences:

| Value | Where | Resolution |
|---|---|---|
| `15px` ×2 | the wordmark, the login heading | → `text-title` (14 px). Neither is a size the other 307 call sites knew about |
| `8px` ×1 | the dialog | → `rounded-surface` (6 px). A modal and a panel are the same kind of corner, and n=1 is not a tier |
| `2px` ×1 | an icon button inside a chip | → `rounded-chip` (3 px). It was inside a 3 px chip |
| `18px` ×2 | the two detail-page `h1`s | **kept** as `text-page`. Two occurrences, but it is the only page-title size and the tier below it is 4 px away |

**4. `index.css`'s `:focus-visible` reads `var(--oto-radius-control)`.** It carried a hand-written
`border-radius: 3px` — a third instance of a decision the scale now owns, agreeing with the chip
step by coincidence and with the controls it actually draws around by nothing at all. It moves to
4 px, which is the tier of everything that takes focus.

**5. All 368 literals are replaced in the same commit, and a gate stops the next one.**
`web/src/design/scales.test.ts` rejects a bracket, rejects Tailwind's own `text-sm`/`rounded-md`
ladder beside ours, rejects a raw `font-size`/`border-radius` in a stylesheet, asserts §M.8 and
`tokens.css` agree step for step, and asserts every declared step has a call site.

## Consequences

- The two axes now have what colour has had since the first commit: a vocabulary, a place the values
  are written once, and a test that fails on the day someone writes a new literal.
- **Three visual changes ship with this**, all deliberate and all listed above: the wordmark and
  login heading go 15 → 14 px, the dialog corner 8 → 6 px, the focus ring 3 → 4 px. Nothing else
  moves; the other 364 replacements are exact.
- **Tailwind's built-in ladder is banned rather than removed.** Setting `--text-*: initial` would
  have deleted `text-sm` from the namespace, and a deleted utility fails *silently* — the class
  stays in the markup and no rule is generated, which `index.css.test.ts` exists to catch elsewhere.
  A test that names the offending file is the louder failure.
- **The scale may not grow ahead of the product.** The gate fails on a declared step with no call
  site. This is the rule that keeps §M.8 six and three steps long instead of becoming a general
  purpose type ramp with a `5xl` nobody has ever rendered.
- A component that genuinely needs a size outside the scale must amend §M.8 under §N. That is
  friction on purpose, and it is the friction that was missing for 342 literals.

## Alternatives rejected

**Collapse 3 px and 4 px into one radius tier**, as the ticket proposed. Rejected on the evidence:
the split is consistent across all 54 call sites and describes a real distinction (inline vs
operable). Collapsing would have changed 26 or 28 corners to prove a sentence.

**Name the steps after their sizes (`text-11`, `rounded-4`).** Rejected: it is the bracket with
extra steps. The name would still carry the value, so changing 11 px to 11.5 px would mean touching
183 call sites, and nothing would be learned about *why* a call site picked it.

**Use Tailwind's own names (`text-xs`, `rounded-md`).** Rejected: the built-in `text-xs` is 12 px
and `text-sm` is 14 px, so the four steps this product actually leans on (10–13 px) have no honest
names in that ladder, and two of ours would have had to be brackets anyway. A dense ops table is not
the density that ladder was drawn for.

**Land the scale plus the highest-traffic replacements, and leave the rest.** Rejected because the
ticket named the failure mode exactly: both spellings live at once is worse than either alone. The
migration is mechanical — seven substitutions on 34 files — and the gate is only honest if the tree
it guards is already clean.

**Write the gate in Go, beside `test/design/boundary_test.go`.** Both shapes fit and the Go one
already walks `web/`. TypeScript won on two counts: it can read the scale out of `index.css` and
name the exact utility to use in the failure message, and it sits beside `tokens.test.ts` and
`contrast.test.ts`, which is where somebody changing the design system is already looking.

## How to overturn this

Open an ADR that supersedes this one. The cheapest thing to get wrong is step **3** — a straggler
resolved the wrong way is a visible regression, and the four are listed above precisely so a
disagreement has something to point at. Reverting a resolution costs one line in `tokens.css` and a
row in §M.8; reverting the *scales* means reintroducing 342 literals, and the census in §M.8 is the
argument against that.
