---
title: 0012 — Pastel product chrome, saturated colour reserved exclusively for state
---
**Status:** Accepted · 2026-08-08 · accent choice superseded by ADR 0030 (2026-08-16); the
two-tier architecture below is unchanged

## Context
The owner named the product **oto** (音, Japanese for *sound* — a chime) and requires a light,
pastel palette. That is a real and defensible position for an alerting tool: this category's
interfaces are uniformly loud, and loudness at rest is how people learn to ignore a channel.

But pastel and alerting are in genuine conflict, and the conflict cannot be dodged:

- A pastel red at ~3:1 against a white surface fails WCAG AA (4.5:1) for body text.
- Even where it technically passes, low-chroma red carries no urgency. A `critical` state that
  reads as "gentle pink" is a safety failure in a product whose output is 3am interruptions.
- ~8 % of men have a red/green colour vision deficiency. Low-saturation state colours collapse
  toward each other far faster than saturated ones.

Resolving this by darkening everything destroys the calm. Resolving it by keeping everything
pastel makes oto unsafe. Neither is acceptable.

## Decision
A strict **two-tier** colour system.

**Tier A — chrome is pastel.** Backgrounds, surfaces, borders, navigation, tables, panels, form
controls, charts' gridlines. Low chroma, high lightness (light) / low lightness (dark). Chrome
never uses a state hue. The brand accent is a periwinkle (`#5B54D6` / `#A6A0FF`) chosen
specifically because it is not adjacent to any state hue. *(2026-08-16: the accent is now a beni
crimson-pink — see ADR 0030 — for the same reason, not a different one.)*

**Tier B — saturated colour means state, and nothing else.** No decorative accent, no chart
series, no hover effect may use a Tier-B hue. Scarcity is what makes it loud: when a saturated
colour appears, it means exactly one thing.

Each state ships **four** tokens rather than one:

| Token | Role | Obligation |
|---|---|---|
| `-fill` | pastel tinted surface (row background, badge) | — |
| `-border` | hairline / 3 px status bar | ≥ 3:1 vs page background |
| `-text` | dark (light mode) / pale (dark mode) text **on that fill** | ≥ 4.5:1 vs its own fill |
| `-solid` | saturated accent: severity dot, status bar, chart mark | ≥ 3:1 vs page background |

So a critical row is a **pastel fill with a saturated 3 px left bar and dark red text** — calm at
a distance, unmistakable at a glance, legible for everyone.

Supporting rules: colour is never the only channel (every state carries ≥ 2 of colour / icon /
text label); severity is carried by the **icon**, state by the **colour**; dark mode is the
default; no flashing ever, and the single 2 s pulse on unacked-critical is removed under
`prefers-reduced-motion`.

Full token sets with **measured contrast ratios for every text-on-surface pair**, light and dark,
are in SPEC §M.4–M.5, and are asserted by `web/src/design/contrast.test.ts` plus an axe-core
Playwright run in both themes.

**The Slack Block Kit palette (§H.2) is a separate, unchanged system.** The Grafana OnCall values
(`#a30200`, `#daa038`, `#dddddd`, `#2eb886`) stay exactly as they are.

## Consequences
- Every state needs four values instead of one, and every new state costs a contrast measurement.
  That is the price of being both calm and safe, and it is paid once.
- Charts get a dedicated neutral/brand ramp and may never plot with state hues, which costs some
  expressiveness in dashboards.
- Designers cannot reach for "a nice red" for anything decorative. This will feel restrictive and
  is the point.
- The two colour systems (UI and Slack) will look different side by side in a screenshot. That is
  accepted and documented, so nobody "fixes" it later.

## Alternatives rejected
- **Pastel everywhere including state:** fails WCAG AA for text on tinted rows and blunts urgency.
  Unsafe.
- **Saturated everywhere (the category default):** loses the brand and, more importantly, loses
  the signal — when everything is loud, nothing is.
- **Pastel fills with pastel text, relying on the icon for legibility:** shifts the entire
  accessibility burden onto iconography and fails for anyone using a screen magnifier or a
  low-quality display.
- **Harmonising the Slack palette with the UI palette:** would replace the best-tested open-source
  alert palette in existence with untested pastels, on a substrate (someone else's Slack theme)
  oto does not control and cannot measure. Trading correctness for coherence is the wrong trade
  in this product.
