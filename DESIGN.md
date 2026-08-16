---
version: alpha
name: zen-ink-design-system
description: A developer-tools brand recast around Japanese ink-painting tradition instead of a tech-voltage accent. Two named palettes carry the whole system — **Washi & Ink** for light (cream washi paper, sumi ink, a kincha-gold accent) and **Konshi Sutra** for dark (indigo-dyed manuscript paper written on in kindei gold and gindei silver, the technique used for Buddhist sutra copying). The electric-blue voltage from the system's original Composio-analysis scaffolding has been fully retired — gold now carries every CTA, wordmark, and spotlight glow. A quiet sakura (cherry-blossom) and kumo (cloud) motif layer runs underneath in both themes: branch silhouettes trail from hero/CTA corners, cloud linework marks section dividers — both corner-anchored and low-opacity so the gold accent and the 2×2 terminal-mockup grid stay the load-bearing signature.

palette-names:
  light: "Washi & Ink"
  dark: "Konshi Sutra"

colors:
  light:
    canvas: "#f3ede2"
    canvas-deep: "#2b2825"
    surface-card: "#f8f4ec"
    surface-card-elevated: "#fdfbf6"
    surface-strong: "#ffffff"
    hairline: "#ded3c0"
    hairline-soft: "#e8dfd0"
    hairline-strong: "#c2b49b"
    ink: "#2b2825"
    body: "#7d7565"
    body-strong: "#2b2825"
    muted: "#978f7e"
    muted-soft: "#b3a996"
    primary: "#b8935a"
    primary-active: "#97753f"
    primary-glow: "#d9b876"
    on-primary: "#2b2825"
    on-dark: "#f3ede2"
    sakura: "#e8a7bb"
    sakura-deep: "#cf7e97"
    sakura-glow: "#f7dde5"
    kumo: "#cdc6b6"
    kumo-line: "#a89f8a"
    semantic-error: "#b6503f"
    semantic-success: "#5a7a4a"
  dark:
    canvas: "#1b2333"
    canvas-deep: "#11161f"
    surface-card: "#232d40"
    surface-card-elevated: "#2c3750"
    surface-strong: "#374363"
    hairline: "#2c3750"
    hairline-soft: "#232d40"
    hairline-strong: "#3d4a6b"
    ink: "#efece3"
    body: "#b8bcc2"
    body-strong: "#efece3"
    muted: "#8992a1"
    muted-soft: "#5f6878"
    primary: "#c9a668"
    primary-active: "#e0bd82"
    primary-glow: "#e6cf9a"
    on-primary: "#1b2333"
    on-dark: "#efece3"
    sakura: "#e6a2b7"
    sakura-deep: "#c97e96"
    sakura-glow: "#f2c7d4"
    kumo: "#3a4557"
    kumo-line: "#586a86"
    semantic-error: "#e07a68"
    semantic-success: "#7fb56a"

typography:
  display-mega:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 72px
    fontWeight: 500
    lineHeight: 1.05
    letterSpacing: -2.16px
  display-xl:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 56px
    fontWeight: 500
    lineHeight: 1.05
    letterSpacing: -1.68px
  display-lg:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 44px
    fontWeight: 500
    lineHeight: 1.1
    letterSpacing: -1.32px
  display-md:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 32px
    fontWeight: 500
    lineHeight: 1.15
    letterSpacing: -0.96px
  display-sm:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 24px
    fontWeight: 500
    lineHeight: 1.25
    letterSpacing: -0.5px
  title-md:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 18px
    fontWeight: 600
    lineHeight: 1.4
    letterSpacing: 0
  title-sm:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 16px
    fontWeight: 600
    lineHeight: 1.4
    letterSpacing: 0
  body-md:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 16px
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: 0
  body-sm:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 14px
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: 0
  caption:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 13px
    fontWeight: 400
    lineHeight: 1.4
    letterSpacing: 0
  caption-uppercase:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 11px
    fontWeight: 600
    lineHeight: 1.4
    letterSpacing: 0.88px
    textTransform: uppercase
  code:
    fontFamily: "'JetBrains Mono', 'Fira Code', monospace"
    fontSize: 13px
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: 0
  button:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 14px
    fontWeight: 500
    lineHeight: 1.0
    letterSpacing: 0
  nav-link:
    fontFamily: "'abcDiatype', ui-sans-serif, system-ui, sans-serif"
    fontSize: 14px
    fontWeight: 500
    lineHeight: 1.4
    letterSpacing: 0

rounded:
  none: 0px
  xs: 4px
  sm: 6px
  md: 8px
  lg: 12px
  xl: 16px
  pill: 9999px
  full: 9999px

spacing:
  xxs: 4px
  xs: 8px
  sm: 12px
  base: 16px
  md: 20px
  lg: 24px
  xl: 32px
  xxl: 48px
  section: 96px

components:
  top-nav:
    backgroundColor: "{colors.canvas}"
    textColor: "{colors.body-strong}"
    typography: "{typography.nav-link}"
    height: 64px
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.on-primary}"
    typography: "{typography.button}"
    rounded: "{rounded.md}"
    padding: 10px 18px
    height: 40px
  button-primary-active:
    backgroundColor: "{colors.primary-active}"
    textColor: "{colors.on-primary}"
    rounded: "{rounded.md}"
  button-secondary:
    backgroundColor: "{colors.surface-card-elevated}"
    textColor: "{colors.body-strong}"
    typography: "{typography.button}"
    rounded: "{rounded.md}"
    padding: 10px 18px
    height: 40px
  button-outline:
    backgroundColor: transparent
    textColor: "{colors.body-strong}"
    typography: "{typography.button}"
    rounded: "{rounded.md}"
    padding: 9px 17px
    height: 40px
  button-tertiary-text:
    backgroundColor: transparent
    textColor: "{colors.body}"
    typography: "{typography.button}"
  hero-band:
    backgroundColor: "{colors.canvas}"
    textColor: "{colors.body-strong}"
    typography: "{typography.display-mega}"
    padding: 96px
  terminal-mockup-grid:
    backgroundColor: "{colors.canvas-deep}"
    textColor: "{colors.on-dark}"
    typography: "{typography.code}"
    rounded: "{rounded.xl}"
    padding: 32px
  terminal-pane:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body}"
    typography: "{typography.code}"
    rounded: "{rounded.lg}"
    padding: 20px
  feature-card:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body}"
    typography: "{typography.title-md}"
    rounded: "{rounded.xl}"
    padding: 28px
  toolkit-card:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body-strong}"
    typography: "{typography.title-sm}"
    rounded: "{rounded.lg}"
    padding: 20px
  toolkit-icon:
    backgroundColor: "{colors.surface-card-elevated}"
    rounded: "{rounded.md}"
    size: 40px
  spotlight-glow-card:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body-strong}"
    typography: "{typography.display-md}"
    rounded: "{rounded.xl}"
    padding: 48px
  code-block:
    backgroundColor: "{colors.canvas-deep}"
    textColor: "{colors.on-dark}"
    typography: "{typography.code}"
    rounded: "{rounded.lg}"
    padding: 20px
  badge-pill:
    backgroundColor: "{colors.surface-card-elevated}"
    textColor: "{colors.body-strong}"
    typography: "{typography.caption-uppercase}"
    rounded: "{rounded.pill}"
    padding: 4px 10px
  text-input:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body-strong}"
    typography: "{typography.body-md}"
    rounded: "{rounded.md}"
    padding: 12px 16px
    height: 44px
  search-input:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body-strong}"
    typography: "{typography.body-md}"
    rounded: "{rounded.md}"
    padding: 10px 16px
    height: 40px
  cta-band-spotlight:
    backgroundColor: "{colors.canvas}"
    textColor: "{colors.body-strong}"
    typography: "{typography.display-lg}"
    padding: 96px
  testimonial-card:
    backgroundColor: "{colors.surface-card}"
    textColor: "{colors.body}"
    typography: "{typography.body-md}"
    rounded: "{rounded.lg}"
    padding: 24px
  footer:
    backgroundColor: "{colors.canvas}"
    textColor: "{colors.body}"
    typography: "{typography.body-sm}"
    padding: 64px 48px
  footer-link:
    backgroundColor: transparent
    textColor: "{colors.body}"
    typography: "{typography.body-sm}"
  sakura-branch-background:
    backgroundColor: "{colors.canvas}"
    accentColor: "{colors.sakura}"
    accentColorDeep: "{colors.sakura-deep}"
    glowColor: "{colors.sakura-glow}"
    opacity: 0.14
  kumo-motif:
    backgroundColor: transparent
    lineColor: "{colors.kumo-line}"
    fillColor: "{colors.kumo}"
    opacity: 0.2
---

## Overview

The system now runs as two named palettes rather than one dark brand: **Washi & Ink** for light and **Konshi Sutra** for dark — both drawn from real Japanese traditional color vocabulary (nihon no dentō-shoku), not a generic pastel-on-black treatment. The electric-blue voltage inherited from the original Composio-analysis scaffolding has been fully retired. The scarce accent is now a warm ink-brush gold — **kincha** in light, **kindei** in dark — carrying the same jobs the blue voltage used to (primary CTAs, wordmark, atmospheric spotlight glow), recast as gold leaf on manuscript paper rather than circuit-board voltage.

**Washi & Ink** (light) reads as a sumi-e ink wash on cream washi paper: warm off-white ground, charcoal sumi ink for type, sakura as the one bloom of living color, kincha gold for rare flourish. **Konshi Sutra** (dark) is that same ink-painting logic's honest dark counterpart, not an inversion of it — ink painting depends on the paper being *light*, so flipping brightness alone just produces pale marks on a slate wall. Konshi Sutra instead references **konshi**, indigo-dyed manuscript paper written on in **kindei** (gold) and **gindei** (silver) ink, the technique historically used to copy Buddhist sutras. Both themes carry the same sakura-branch and kumo-cloud motif layer, corner-anchored and low-opacity in both directions.

Structurally the page keeps its developer-tool bones — top nav, hero, 2×2 terminal-style mockup grid, toolkit cards, footer — restyled entirely around the new palette pair rather than rebuilt from scratch.

**Key Characteristics:**
- Two named palettes, not one dark brand: `{palette-names.light}` and `{palette-names.dark}` — switch by theme, never by inventing new tokens.
- Scarce accent is warm ink-brush gold (`{colors.primary}`: kincha in light, kindei in dark) — carries CTAs, wordmark, spotlight glow. Blue voltage fully retired.
- Sumi/gindei ink for display type; nezumi/gindei for muted body — no pure black-on-white or white-on-black.
- Terminal-mockup hero: the 2×2 grid stays the structural anchor, now an ink/manuscript panel in tone rather than a raw code terminal.
- Compact `{rounded.md}` (8px) CTA geometry retained in both themes — developer-tool dialect regardless of palette.
- Spotlight-glow backdrop: same radial-glow mechanic, now warm gold (`{colors.primary-glow}`) instead of electric blue.
- Zen accent layer: sakura-branch silhouettes trail from hero/CTA corners; kumo-cloud motif linework marks section dividers — corner-anchored and low-opacity in both themes.
- 96px section rhythm retained.

## Colors

### Accent
| Token | Light — Washi & Ink | Dark — Konshi Sutra | Use |
|---|---|---|---|
| `{colors.primary}` | Kincha `#b8935a` | Kindei `#c9a668` | Primary CTAs, wordmark, spotlight glow center |
| `{colors.primary-active}` | `#97753f` (deeper) | `#e0bd82` (brighter) | Press state |
| `{colors.primary-glow}` | `#d9b876` | `#e6cf9a` | Atmospheric spotlight glow |

Gold is the only brand accent in either theme — no secondary illustration colors (the old accent-cyan / accent-violet pair belonged to the retired blue-tech brand and has no replacement).

### Surface
| Token | Light — Washi & Ink | Dark — Konshi Sutra | Use |
|---|---|---|---|
| `{colors.canvas}` | Washi `#f3ede2` | Konshi `#1b2333` | Page floor |
| `{colors.canvas-deep}` | Sumi `#2b2825` | `#11161f` | Terminal mockup grid bg, code blocks — a recessed ink block in both themes |
| `{colors.surface-card}` | `#f8f4ec` | `#232d40` | Default content card |
| `{colors.surface-card-elevated}` | `#fdfbf6` | `#2c3750` | Terminal panes, secondary buttons |
| `{colors.surface-strong}` | `#ffffff` | `#374363` | Dropdown menus |

### Hairlines
| Token | Light | Dark |
|---|---|---|
| `{colors.hairline}` | `#ded3c0` | `#2c3750` |
| `{colors.hairline-soft}` | `#e8dfd0` | `#232d40` |
| `{colors.hairline-strong}` | `#c2b49b` | `#3d4a6b` |

### Text
| Token | Light | Dark | Use |
|---|---|---|---|
| `{colors.ink}` | Sumi `#2b2825` | Gindei-white `#efece3` | Display headlines |
| `{colors.body}` | Nezumi `#7d7565` | Gindei `#b8bcc2` | Default running text |
| `{colors.body-strong}` | `#2b2825` | `#efece3` | Same as ink |
| `{colors.muted}` | `#978f7e` | `#8992a1` | Sub-titles, breadcrumbs |
| `{colors.muted-soft}` | `#b3a996` | `#5f6878` | Disabled text |
| `{colors.on-primary}` | `#2b2825` | `#1b2333` | Ink-dark text on the gold CTA — in both themes the button reads dark-on-gold, not white-on-blue |
| `{colors.on-dark}` | `#f3ede2` | `#efece3` | Text on `{colors.canvas-deep}` ink/manuscript blocks |

### Semantic
| Token | Light | Dark | Use |
|---|---|---|---|
| `{colors.semantic-success}` | `#5a7a4a` | `#7fb56a` | "Online", "active" indicators |
| `{colors.semantic-error}` | `#b6503f` | `#e07a68` | Validation errors |

Both are muted natural pigments (moss green, vermillion) rather than saturated UI red/green — they stay in the same family as the rest of the palette.

### Zen Accent
| Token | Light | Dark | Use |
|---|---|---|---|
| `{colors.sakura}` | `#e8a7bb` | `#e6a2b7` | Cherry-blossom pink for branch-silhouette backgrounds. Decorative only — never a CTA or interactive color. |
| `{colors.sakura-deep}` | `#cf7e97` | `#c97e96` | Petal/branch linework contrast tone |
| `{colors.sakura-glow}` | `#f7dde5` | `#f2c7d4` | Palest wash at the fade-out edge of a branch silhouette |
| `{colors.kumo}` | `#cdc6b6` | `#3a4557` | Cloud (kumo) motif fill |
| `{colors.kumo-line}` | `#a89f8a` | `#586a86` | Cloud motif outline/linework |

## Typography

### Font Family
The system runs **abcDiatype** (Lineto) across every text role. Code blocks switch to **JetBrains Mono**. Fallback: `ui-sans-serif, system-ui, sans-serif`.

### Hierarchy

| Token | Size | Weight | Line Height | Letter Spacing | Use |
|---|---|---|---|---|---|
| `{typography.display-mega}` | 72px | 500 | 1.05 | -2.16px | Homepage hero h1 |
| `{typography.display-xl}` | 56px | 500 | 1.05 | -1.68px | Subsidiary heroes |
| `{typography.display-lg}` | 44px | 500 | 1.1 | -1.32px | Section heads |
| `{typography.display-md}` | 32px | 500 | 1.15 | -0.96px | Sub-section heads |
| `{typography.display-sm}` | 24px | 500 | 1.25 | -0.5px | Card group titles |
| `{typography.title-md}` | 18px | 600 | 1.4 | 0 | Component titles |
| `{typography.title-sm}` | 16px | 600 | 1.4 | 0 | Toolkit card titles |
| `{typography.body-md}` | 16px | 400 | 1.5 | 0 | Default body |
| `{typography.body-sm}` | 14px | 400 | 1.5 | 0 | Footer body |
| `{typography.caption}` | 13px | 400 | 1.4 | 0 | Photo captions |
| `{typography.caption-uppercase}` | 11px | 600 | 1.4 | 0.88px | Section labels, badge pills |
| `{typography.code}` | 13px | 400 | 1.5 | 0 | Code blocks — JetBrains Mono |
| `{typography.button}` | 14px | 500 | 1.0 | 0 | CTA pill labels |
| `{typography.nav-link}` | 14px | 500 | 1.4 | 0 | Top-nav menu |

### Principles
- **Display weight stays at 500.** Confident but not display-bold.
- **abcDiatype across every role.** No display/body family split.
- **JetBrains Mono on every code surface.**

### Note on Font Substitutes
abcDiatype is a Lineto licensed typeface. Open-source substitute: **Inter** at weight 500 with letter-spacing -1.5%.

## Layout

### Spacing System
- **Base unit:** 4px.
- **Tokens:** `{spacing.xxs}` 4px · `{spacing.xs}` 8px · `{spacing.sm}` 12px · `{spacing.base}` 16px · `{spacing.md}` 20px · `{spacing.lg}` 24px · `{spacing.xl}` 32px · `{spacing.xxl}` 48px · `{spacing.section}` 96px.
- **Section padding:** `{spacing.section}` (96px) for major bands.

### Grid & Container
- Max content width: ~1200px.
- Editorial body: 12-column grid.
- Terminal mockup grid: 2×2 equal-size panes.
- Toolkit grid: 4-up at desktop, 2-up tablet, 1-up mobile.
- Footer: 5-column at desktop.

### Whitespace Philosophy
Both themes hold their own depth without whitespace doing extra work — 96px between bands; 24px between cards inside a band.

## Elevation & Depth

The system uses **brightness-step elevation** in both themes: surfaces step toward the "raised" end of the ladder (lighter in light mode, lighter-indigo in dark mode) instead of casting drop shadows. Combined with a subtle radial gold glow, this creates a focused atmosphere in either direction.

| Level | Light — Washi & Ink | Dark — Konshi Sutra | Use |
|---|---|---|---|
| Flat (canvas) | `{colors.canvas}` `#f3ede2` | `{colors.canvas}` `#1b2333` | Body bands, footer |
| Recessed | `{colors.canvas-deep}` `#2b2825` | `{colors.canvas-deep}` `#11161f` | Terminal mockup grid bg, code blocks |
| Card | `{colors.surface-card}` `#f8f4ec` | `{colors.surface-card}` `#232d40` | Default content cards |
| Card elevated | `{colors.surface-card-elevated}` `#fdfbf6` | `{colors.surface-card-elevated}` `#2c3750` | Terminal panes, secondary buttons |
| Atmospheric glow | Radial gradient using `{colors.primary-glow}` | Radial gradient using `{colors.primary-glow}` | Hero spotlight backdrop |

### Decorative Depth
- **Spotlight glow backdrops** — radial gold gradient (kincha in light, kindei in dark) centered behind hero content.
- **Terminal-pane brightness ladder** — 2×2 mockup uses canvas-deep outer + surface-card-elevated panes, in both themes.
- **Sakura branch silhouettes** — a single trailing cherry-blossom branch, rendered in `{colors.sakura}` / `{colors.sakura-deep}` at ~14% opacity, anchored to one corner of a hero or CTA band and fading to `{colors.sakura-glow}` before it dissolves into `{colors.canvas}`. Never centered, never full-bleed — a corner flourish, not wallpaper.
- **Kumo cloud motifs** — traditional kumo swirl linework in `{colors.kumo}` / `{colors.kumo-line}` at ~20% opacity, used to mark section-band dividers and to soften the corners of `{component.footer}`. Stays quiet enough that it never competes with the gold spotlight glow.

## Shapes

### Border Radius Scale

| Token | Value | Use |
|---|---|---|
| `{rounded.none}` | 0px | Reserved |
| `{rounded.xs}` | 4px | Inline tags |
| `{rounded.sm}` | 6px | Compact rows |
| `{rounded.md}` | 8px | CTA buttons, form inputs |
| `{rounded.lg}` | 12px | Toolkit cards, code blocks, terminal panes |
| `{rounded.xl}` | 16px | Feature cards, terminal mockup grids |
| `{rounded.pill}` | 9999px | Section-label badges |
| `{rounded.full}` | 9999px | Avatar plates (rare) |

Compact developer-ergonomic radii — 8px CTAs, 12-16px cards. Signals "developer tool" rather than "consumer brand," even restyled around ink and paper.

## Components

### Top Navigation

**`top-nav`** — Default top nav, same in both themes. Background `{colors.canvas}`, text `{colors.body-strong}`, height 64px. Layout: wordmark left, primary horizontal menu (Product / Toolkits / Docs / Pricing / Customers / Blog), GitHub stars + Sign In + "Get started" right.

### Buttons

**`button-primary`** — The signature gold CTA (kincha in light, kindei in dark). Background `{colors.primary}`, text `{colors.on-primary}` (ink-dark, not white), type `{typography.button}` (14px / 500), padding 10px × 18px, height 40px, rounded `{rounded.md}` (8px).

**`button-primary-active`** — Press state. Background `{colors.primary-active}` — deeper gold in light, brighter gold in dark.

**`button-secondary`** — Surface-elevated secondary. Background `{colors.surface-card-elevated}`, text `{colors.body-strong}`.

**`button-outline`** — Transparent with 1px hairline-strong border.

**`button-tertiary-text`** — Inline text link.

### Hero & Atmospheric

**`hero-band`** — Homepage hero. Background `{colors.canvas}`, full-width display headline in `{typography.display-mega}` (72px / 500), subhead, two CTAs, and a spotlight-glow backdrop — now gold rather than blue — emanating from behind the centered terminal-mockup grid, with a sakura-branch silhouette anchored to one corner.

**`terminal-mockup-grid`** — The brand's structural signature. 2×2 grid of ink/manuscript-toned panels inside a `{rounded.xl}` (16px) container. Background `{colors.canvas-deep}` (sumi ink in light, deep indigo in dark), text `{colors.on-dark}`, padding 32px, gap 16px.

**`terminal-pane`** — Individual panel inside the mockup grid. Background `{colors.surface-card}`, text `{colors.body}` in `{typography.code}`, rounded `{rounded.lg}` (12px), padding 20px.

**`spotlight-glow-card`** — Large feature card with centered display headline and a radial gold glow behind it. Background `{colors.surface-card}`, text `{colors.body-strong}` in `{typography.display-md}`, rounded `{rounded.xl}`, padding 48px.

### Cards

**`feature-card`** — 3-up benefit grid. Background `{colors.surface-card}`, text `{colors.body}`, type `{typography.title-md}`, rounded `{rounded.xl}`, padding 28px.

**`toolkit-card`** — 4-up toolkit grid (Slack, GitHub, Stripe, Notion, Linear, etc.). Background `{colors.surface-card}`, text `{colors.body-strong}`, type `{typography.title-sm}`, rounded `{rounded.lg}`, padding 20px. 40px square `{component.toolkit-icon}` top, toolkit name, one-line description.

**`toolkit-icon`** — Square icon plate. Background `{colors.surface-card-elevated}`, rounded `{rounded.md}`, 40px size.

**`testimonial-card`** — Quote card. Background `{colors.surface-card}`, text `{colors.body}`, rounded `{rounded.lg}`, padding 24px.

### Code

**`code-block`** — Inline code/manuscript block. Background `{colors.canvas-deep}`, text `{colors.on-dark}` in `{typography.code}`, rounded `{rounded.lg}`, padding 20px.

### Forms

**`text-input`** — Background `{colors.surface-card}`, text `{colors.body-strong}`, rounded `{rounded.md}` (8px), padding 12px × 16px, height 44px.

**`search-input`** — Compact search field. Same surface and radius, smaller padding, 40px height.

### Tags & Badges

**`badge-pill`** — Small uppercase pill. Background `{colors.surface-card-elevated}`, text `{colors.body-strong}`, type `{typography.caption-uppercase}`, rounded `{rounded.pill}`, padding 4px × 10px.

### CTA / Footer

**`cta-band-spotlight`** — Pre-footer band. Background `{colors.canvas}` with centered radial gold spotlight glow. Display headline + single primary CTA pill. 96px padding.

**`footer`** — Closing footer, same structure in both themes. Background `{colors.canvas}`, text `{colors.body}`. 5-column link list. 64×48px padding.

**`footer-link`** — Background transparent, text `{colors.body}`, type `{typography.body-sm}`.

### Zen Motifs

**`sakura-branch-background`** — Corner-anchored decorative branch silhouette. Background `{colors.canvas}`, branch/petal rendered in `{colors.sakura}` with `{colors.sakura-deep}` accents, fading to `{colors.sakura-glow}` at the edge, ~14% opacity overall. Used behind hero and CTA bands as a quiet counterpoint to the gold spotlight glow — never as the primary atmospheric element.

**`kumo-motif`** — Cloud (kumo) swirl linework. Transparent background, `{colors.kumo}` fill with `{colors.kumo-line}` outline, ~20% opacity. Used as a section-divider ornament and to soften footer corners.

## Do's and Don'ts

### Do
- Reserve `{colors.primary}` (kincha in light, kindei in dark) for primary CTAs, wordmark, and spotlight glows.
- Use `{rounded.md}` (8px) for every CTA — not full pills.
- Use brightness-step ladder for elevation; avoid drop shadows.
- Pair every hero with a centered radial gold spotlight glow, using the active theme's `{colors.primary-glow}`.
- Render code, CLI commands in JetBrains Mono via `{typography.code}`.
- Use the 2×2 terminal-mockup grid as the homepage hero anchor.
- Keep `{colors.sakura}` branch silhouettes corner-anchored and low-opacity (~14%) — a quiet flourish, not a background pattern.
- Keep kumo motifs (`{colors.kumo}` / `{colors.kumo-line}`) to dividers and corner ornaments at ~20% opacity.
- Define every token for both `light` and `dark` — never ship a color that only exists in one theme.

### Don't
- Don't introduce a secondary brand accent beyond `{colors.primary}` gold and the sakura/kumo decorative pair.
- Don't use full pills on CTAs.
- Don't drop display weight to 400.
- Don't add drop shadow tiers.
- Don't use `{colors.canvas-deep}` outside terminal/code/manuscript-block surfaces.
- Don't extract a CTA color from a third-party widget (cookie consent, OneTrust). The brand's CTA color is what appears on actual page CTAs.
- Don't use `{colors.sakura}` on CTAs, links, or any interactive element — it's decorative-only and must never compete with `{colors.primary}`.
- Don't let a sakura branch or kumo motif go full-bleed or full-opacity — both stay faint background texture, never foreground content.
- Don't build Konshi Sutra by inverting Washi & Ink's values. Ink painting depends on the paper being light — Konshi Sutra is its own palette (indigo paper, gold/silver ink), not `{colors.light}` with brightness flipped.

## Responsive Behavior

### Breakpoints

| Name | Width | Key Changes |
|---|---|---|
| Mobile | < 640px | Hero h1 72→36px; terminal mockup grid collapses to single pane; toolkit grid 1-up; nav hamburger. |
| Tablet | 640–1024px | Hero h1 56px; terminal mockup grid stays 2×2; toolkit grid 2-up. |
| Desktop | 1024–1280px | Full hero h1 72px; full 2×2 terminal mockup; toolkit grid 4-up. |
| Wide | > 1280px | Content caps at 1200px. |

### Touch Targets
- Primary CTA at 40px height — at WCAG AA, padded for AAA.
- Search input at 40px.

### Collapsing Strategy
- Top nav switches to hamburger below 768px.
- Terminal mockup 2×2 grid collapses to a single pane on mobile.
- Toolkit grid: 4-up → 2-up → 1-up.
- Hero spotlight glow stays at every breakpoint.

## Iteration Guide

1. Focus on a single component at a time.
2. CTAs default to `{rounded.md}` (8px). Cards use `{rounded.lg}` or `{rounded.xl}`.
3. Variants live as separate entries inside `components:`.
4. Use `{token.refs}` everywhere — never inline hex.
5. Hover state never documented.
6. abcDiatype 500 for display, 400/600 for body. JetBrains Mono on every code surface.
7. Gold (`{colors.primary}` — kincha light / kindei dark) stays scarce — the same discipline the old Composio Blue had.
8. Sakura and kumo are atmosphere, not identity — `{colors.primary}` and the terminal-mockup grid stay the load-bearing signature.
9. Washi & Ink and Konshi Sutra are independent, hand-tuned palettes — never derive one theme's tokens by inverting the other's.

## Known Gaps

- abcDiatype is licensed; Inter is the substitute.
- Animation timings out of scope.
- In-product surfaces (toolkit dashboards, agent playground) are behind login walls.
- Form validation states beyond focus not visible on captured surfaces.
- Sakura-branch and kumo-motif artwork are described as color/opacity specs here, not shipped as SVG/canvas assets — an implementer still needs to draw the actual branch/cloud linework, per theme.
- Two other palette directions were explored and set aside during review: **Ai-zome Night** (plain dyed-indigo dark) and **Suibokuga Wash** / **Kachō-fūgetsu** (alternate light/dark pairings) — kept here as a note in case the gold accent turns out too warm against a given surface and Ai-zome's plainer indigo is worth revisiting for dark mode.
