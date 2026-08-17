# Decorative motifs

The art behind §M.9's decorative ink. Every file here is a **mask**, never an image: it is
referenced from CSS as `mask-image: url(/motifs/<name>.svg)` over a flat `--oto-wash` fill, so one
asset is correct in both themes and the tint comes from the palette rather than from the file.
`Ink.tsx` is the only intended way in — it owns the URL and the `aria-hidden` that no stylesheet
can set.

| File | Motif | Where it is spent |
|---|---|---|
| `enso.svg` | the ensō lockup — `assets/logo/oto-logo-mono.svg`, verbatim | `/login`, bleeding off two corners behind the form |
| `swipe.svg` | two overlapping brush passes, seam visible | behind a page heading (alert detail) |
| `rule.svg` | one smooth tapered pass | under a page heading (group detail) |
| `kumo.svg` | suyari-gumo, the trailing mist band — stillness | a full-page empty state that is quiet |
| `sakura.svg` | one fallen petal — *mono no aware*, transience | the `expired` empty state, and nowhere else |

## Three things that are easy to get wrong here

**⛔ Every file carries `preserveAspectRatio="none"`, and it is load-bearing.** The SVG default is
`xMidYMid meet`, which letterboxes the art inside the mask box — and the mask is then *transparent*
at the edges. That does not present as a scaling bug. It presents as the content having vanished,
which is the hardest possible symptom to trace back to an attribute nobody set.

**⛔ The fill must be opaque black, not `currentColor`.** A mask referenced by `url()` is an
isolated document: it inherits nothing from the page, so `currentColor` resolves against the
asset's own root and not against the theme. `mask-mode: match-source` reads **alpha** from an SVG
image, so black-on-transparent is the whole contract — the colour of the ink is `--oto-ink-tint` at
the call site. (`enso.svg` is the traced source verbatim, which sets `color` on its own root for
exactly this reason.)

**⛔ `swipe.svg` and `rule.svg` are two assets and must stay two.** A mask has no fixed aspect, so
stretching the thin rule to heading height is possible and produces a shape that reads as a wave
rather than as a stroke. The two shapes are drawn for the two boxes they go in.
