# 0030 — Tier A repainted as Washi & Ink / Konshi Sutra; accent moves to beni

**Status:** Accepted · 2026-08-16 · supersedes the accent clause of ADR 0012

## Context
ADR 0012 fixed the two-tier system — pastel chrome, saturated colour reserved exclusively for
state — and picked a periwinkle (`#5B54D6` light / `#A6A0FF` dark) for the Tier A brand accent,
chosen because it sat far from every Tier B state hue.

Separately, the product's marketing surfaces adopted a Japanese-Zen visual language: **Washi &
Ink** for light (sumi ink on cream washi paper) and **Konshi Sutra** for dark (indigo manuscript
paper written on in gold and silver ink), with sakura-branch and kumo-cloud motifs. Bringing the
product UI's chrome into that language means repainting Tier A's neutrals (bg/surface/border/text)
from a cool periwinkle-tinted grey to a warm washi/konshi one, and choosing a new accent to replace
the periwinkle.

The obvious move — reuse the marketing palette's gold (kincha `#B8935A` light / kindei `#C9A668`
dark) as the product accent — does not survive contact with Tier B. Gold sits at ~36-38° hue,
which is the *same* hue lane as `--oto-state-acked-solid` (~36°) and close to
`--oto-state-expired-solid` (~32°). ADR 0012's whole argument for the periwinkle was "not adjacent
to any state hue"; an accent that reads as amber in an alerting tool risks being misread as the
`acked` state itself. Gold works for a marketing hero; it fails Tier A's actual job.

## Decision
Two changes, kept separate:

**1. Tier A neutrals repaint warm.** `--oto-bg`, `--oto-surface`, `--oto-surface-raised`,
`--oto-surface-sunken`, `--oto-border`, `--oto-border-strong`, `--oto-text`, `--oto-text-muted`,
`--oto-text-subtle` move from a cool periwinkle-grey cast to a warm washi-paper cast (light) / a
konshi-indigo cast (dark). `--oto-chart-grid` moves with `--oto-border`, as before (it has always
shared that value). Tier B is untouched — state fills/borders/text/solids keep their exact hex
values in both themes; only the chrome around them changed colour temperature.

**2. The accent becomes beni, not gold.** `--oto-accent` (and `-hover`/`-fill`/`-border`/`-focus`)
move to a beni crimson-pink — `#B5305C` light, `#EC8CA6` dark — sitting at ~340° hue. That is the
widest open lane on the hue wheel relative to all six state hues (nearest neighbour is firing red
at ~3°, a ~25° gap; every other state sits 60-300° away). It keeps the sakura/beni family that
carries the rest of the Zen language, without landing in acked's or expired's territory the way
gold would have. Gold stays exactly where it already worked: the marketing site's hero, wordmark,
and spotlight glow, which have no Tier B system to collide with.

Every replacement value clears its §M.4/§M.5 contrast obligation with the same or better margin
than the token it replaced — see the updated tables. `--oto-state-*-solid` vs `--oto-bg` rows were
recomputed (foreground unchanged, background's luminance shifted) and all still clear ≥ 3:1; the
tightest, `acked-solid` on the new light `--oto-bg`, holds the same 3.2:1 margin the periwinkle
palette had.

## Consequences
- The product UI and the marketing site now share a coherent visual language (washi/konshi neutrals,
  sakura-family accent) without sharing literal token values — the product's beni is more saturated
  and hue-shifted from the marketing gold specifically because Tier A here has a job (staying clear
  of Tier B) that the marketing site does not.
- Every Tier A hex in `tokens.css` changed in both themes; every measured pair in SPEC §M.4/§M.5
  that touches a Tier A token was re-measured, not just re-stated. `contrast.test.ts` and
  `tokens.test.ts` are the gates that would have caught a mismatch, and both were run green before
  this landed.
- `--oto-state-*` (Tier B), `--oto-chart-1…6`, `--oto-row-h`, and the §M.8 type/radius scales are
  unchanged — this is a chrome-only repaint.
- ADR 0012's two-tier architecture and its "chrome never uses a state hue" rule are unchanged and,
  if anything, are the reason gold was rejected for Tier A in the first place.

## Alternatives rejected
- **Reuse the marketing gold (kincha/kindei) as the product accent.** Rejected: ~36° hue lands on
  top of `acked` (~36°) and near `expired` (~32°). An accent that can be mistaken for a state colour
  defeats the entire premise of ADR 0012.
- **Keep the periwinkle accent, repaint only the neutrals.** Considered, but leaves the product
  visually disconnected from the sakura/beni family that carries the rest of the Zen language on
  every other surface, for no reason stronger than "it already worked."
- **Invert Washi & Ink's light values to build the dark neutrals.** Rejected on the marketing side
  for the same reason it's rejected here: ink-on-paper depends on the paper being light, and a
  brightness-flip reads as chalk-on-slate, not indigo manuscript paper. Konshi Sutra's dark
  neutrals were tuned independently, the same way the marketing palette was.
