---
title: 0032 — Dark-theme accent moves from beni to plum-blossom blue-and-white; light untouched
---
**Status:** Accepted · 2026-08-16 · amends the dark-theme accent clause of ADR 0030; the light
theme's beni crimson-pink, both themes' neutrals, and every Tier B token are unchanged

## Context
The owner asked for the dark theme's accent to move from beni crimson-pink to a "plum blossom
blue and white" combination — the sometsuke (blue-and-white porcelain) register: white ume blossoms
against a deep indigo ground, which is also literally what Konshi Sutra's dark background already
is. Scoped to dark only; Washi & Ink's light-theme beni accent (`#B5305C`) is untouched.

This is the same kind of decision ADR 0012 and ADR 0030 made — pick a hue for the scarce Tier A
accent — with a materially worse hand of cards. ADR 0012's periwinkle and ADR 0030's beni both
landed ≥25° from every Tier B state hue. A genuine *blue* accent cannot: Tier B already owns both
ends of the blue lane — `info` at ~214° and `suppressed` at ~252° — leaving only a 41°-wide gap
between them. The best a blue accent can do is sit at the midpoint, ~233°, which is ~19° from each
neighbour. Every other accent this system has shipped cleared ≥25°; this one cannot, because the
colour family the owner asked for is the one Tier B has boxed in on both sides.

## Decision
**Dark accent moves to `#949DE0`, hue ~233°**, accepting the ~19° gap as a deliberate, documented
exception rather than reopening the request. `-hover` moves to `#CDD4EA` — pale, low-chroma, the
"white" half of the combination, so hovering an accent element visibly pales toward blossom-white
rather than just darkening/lightening the same hue. `-fill` (`#141734`) and `-border` (`#465291`)
are a dark blue wash and a mid blue-white border, for badge-style and outlined uses of the accent.
`--oto-focus` follows `--oto-accent`, as it always has.

Both measured pairs clear their 4.5:1 obligation with a normal margin — `--oto-accent` on
`--oto-surface` is 5.7:1, `--oto-text-inverse` on `--oto-accent` is 6.5:1 — comparable to every
prior accent's margin. The risk here is not contrast; it is that a user can, at a glance, mistake
the accent for the `info` or `suppressed` state badge, which is exactly the harm ADR 0012's
"chrome never uses a state hue" rule exists to prevent. Two things limit that risk in practice:
`info` and `suppressed` are always rendered as a four-token badge (fill + border + text + icon,
per §M.2), never as a bare swatch, so context disambiguates them from an accent used on a link,
button or focus ring; and U1 already requires state to carry ≥2 non-colour channels (icon, text
label), so no state reading depends on colour alone.

Light theme is unaffected: `--oto-accent` stays `#B5305C` (beni), because the owner's request and
the reasoning above are dark-only.

## Consequences
- `--oto-accent`/`-hover`/`-fill`/`-border`/`-focus` change in the dark theme only. Light theme, both
  themes' neutrals, all Tier B state tokens, and the chart ramp are byte-identical to what ADR 0030
  and ADR 0031 shipped.
- §M.5's two measured accent rows were recomputed against the new hex and both still clear 4.5:1.
  `contrast.test.ts` and `tokens.test.ts` were run green before this landed.
- This is the first accent in the system's history shipped with a hue gap tighter than 25° from a
  Tier B state. That is a conscious exception, not a new precedent — the next accent change should
  not treat ~19° as an acceptable target, only as what a "true blue" costs specifically, given where
  `info` and `suppressed` already sit.
- If the `info`/`suppressed` badge-vs-accent ambiguity turns out to matter in practice (an axe/user
  report, not a hypothetical), the cheapest fix is moving `info`'s hue rather than the accent's — but
  that is a Tier B change gated by its own contrast obligations and is out of scope here.

## Alternatives rejected
- **A teal/cyan accent (~180°), as ADR 0031's reverted first pass used.** Would have cleared ≥30°
  from every state hue, comfortably inside precedent — but is not blue, and does not read as
  sometsuke plum-blossom blue-and-white, which is what was asked for.
- **Push the hue further from `info` toward `suppressed`, or vice versa.** Rejected: 233° is the
  midpoint and therefore already the best available compromise; moving either direction only trades
  one collision risk for the other, worse.
- **Desaturate toward an achromatic blue-gray to reduce collision risk.** Rejected: a properly
  desaturated "blue" reads as slate, not as sometsuke cobalt — it would solve the hue-adjacency
  problem by quietly deleting the colour the owner asked to see.
