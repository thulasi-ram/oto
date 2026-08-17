/**
 * Bespoke layout surfaces with no solid-ui counterpart, relocated verbatim
 * (in behaviour) out of `primitives.tsx` — `Panel`, `PanelHeader`, `PanelTitle`,
 * `Chip`, `DataRow`. solid-ui doesn't ship a "Panel" or "DataRow" concept, so
 * there is nothing to unify these against; they move here only because
 * `primitives.tsx` itself is being retired once every other export
 * (Button/Input/Textarea/Select/Field/Checkbox/ToggleGroup/Spinner/cx) has
 * been migrated off it by other agents in a later phase.
 *
 * The one change from the original: `cn()` from `~/lib/cn` in place of
 * `primitives.tsx`'s local `cx()` helper. `cx()` is not re-exported from
 * here or anywhere else — every future consumer imports `cn` from `~/lib/cn`
 * directly, the same merger every other solid-ui-derived component in
 * `components/ui/` already uses at its `class` prop boundary.
 */
import type { ParentComponent } from "solid-js";

import { Ink } from "./Ink";
import { cn } from "~/lib/cn";

/**
 * The page heading, and the one place in the product a brush is allowed to
 * touch text — SPEC §M.9, ADR 0035.
 *
 * ADR 0031 took the interface to sharp corners and a crisp technical grid, which
 * is the right call for a table an operator reads at 3am and which left the
 * product with no surviving trace of the ink-painting language ADR 0030 chose.
 * A page heading is where it comes back: it is singular, it appears once per
 * screen, and it is the only text in the product with the contrast headroom to
 * wear ink.
 *
 * ⭐ THE HEADROOM IS THE WHOLE ARGUMENT, AND IT IS MEASURED. `--oto-text` is
 * 14.24:1 on `--oto-bg` in light and 14.07:1 in dark, so a 12% heading tint
 * behind it lands at 12.95:1 (light) and 10.89:1 (dark) — against a 4.5:1 floor,
 * because 18 px at weight 600 is *not* WCAG "large text" (that starts at
 * 18.66 px bold). §M.9 tabulates all four hue/theme combinations.
 *
 * ⛔ INK GOES BEHIND A PAGE HEADING AND NOWHERE ELSE. The same treatment behind
 * `--oto-text-muted` body copy would be a defect and behind `--oto-text-subtle`
 * an outright failure — subtle is 4.90:1 and drops to 4.37:1 under a wash a
 * third this weight. And `PanelHeader`/`PanelTitle`/`SECTION_LABEL` below do not
 * get it for a second reason that has nothing to do with contrast: alert detail
 * stacks six panels, and at six a gesture becomes a texture. That is the failure
 * mode the whole study was trying to avoid, so the two live in one file with
 * this paragraph between them.
 *
 * ⛔ THE TWO SHAPES ARE TWO ASSETS AND MUST STAY TWO. `swipe` is the background —
 * two overlapping passes with the seam left visible; `rule` is the underline — a
 * single smooth tapered pass. A mask has no fixed aspect, so stretching the thin
 * rule to heading height is possible, and what it produces reads as a wave
 * rather than as a stroke.
 *
 * The brush also overshoots the word, asymmetrically: a brush enters just before
 * the first letter and lifts off well after the last, and matching the two makes
 * it read as a box rule again. `xs` in and `xl` out — 6 px and 24 px — which is
 * the §M.8 spacing scale's nearest pair to the 7/22 the drawings were made at.
 */
export type Brush = "swipe" | "rule";

/**
 * Both are Tier A under §M.2, so neither can be mistaken for a state.
 *
 * ⚠️ They are clearly distinct in light — kincha gold against beni crimson — and
 * nearly collapse in dark, where ADR 0032 put the accent at ~233° next to a
 * blue-grey border token. `muted` is what both shipping headings spend;
 * `accent` is offered because §M.9 permits it, and is unspent today.
 */
export type BrushHue = "muted" | "accent";

export const PageHeading: ParentComponent<{
  readonly brush: Brush;
  readonly hue?: BrushHue;
  readonly class?: string;
}> = (props) => (
  <h1 class={cn("min-w-0 text-page font-semibold tracking-tight text-ink", props.class)}>
    {/* Three elements rather than one, and each of the three earns its place.
        The inline-block shrink-wraps to the words, so the brush is as long as
        the heading rather than as long as the column. `truncate` moves down onto
        the text alone — left on the wrapper, its `overflow: hidden` would clip
        exactly the lead and tail the brush exists to have. And the text carries
        `relative` so that two positioned siblings at `z-index: auto` paint in
        DOM order: ink first, words on top, no stacking context invented. */}
    <span class="relative inline-block max-w-full align-bottom">
      {/* The shape picks the tint step as well as the asset, and it must: a
          swipe sits UNDER the words and a rule sits BESIDE them, so the two owe
          completely different things to contrast. §M.9 has the numbers. */}
      <Ink
        motif={props.brush}
        tint={
          props.brush === "swipe"
            ? props.hue === "accent"
              ? "heading-accent"
              : "heading"
            : props.hue === "accent"
              ? "rule-accent"
              : "rule"
        }
        class={cn(
          "absolute -left-xs -right-xl",
          props.brush === "swipe" ? "-inset-y-0.5" : "-bottom-0.5 h-2",
        )}
      />
      <span class="relative block truncate">{props.children}</span>
    </span>
  </h1>
);

/**
 * The one uppercase section-label recipe. Three near-identical variants grew
 * up independently (`.08em`/`text-meta`/semibold here, `.06em`/`text-meta`/
 * semibold in the alert tables, `.08em`/`text-micro`/medium in the side
 * panel) before this was hoisted; the side-panel's is the one PORTING-SPEC §4
 * mandates, so it is the one that survives. Every uppercase section label in
 * the app should read from this constant rather than re-deriving its own.
 */
export const SECTION_LABEL = "text-micro font-medium uppercase tracking-[0.08em]";

export const Panel: ParentComponent<{ readonly class?: string }> = (props) => (
  <section class={cn("rounded-surface border border-line bg-surface", props.class)}>
    {props.children}
  </section>
);

export const PanelHeader: ParentComponent<{ readonly class?: string }> = (props) => (
  <header
    class={cn(
      "flex items-center justify-between gap-3 border-b border-line bg-raised px-3 py-2",
      "rounded-t-surface",
      props.class,
    )}
  >
    {props.children}
  </header>
);

export const PanelTitle: ParentComponent<{ readonly class?: string }> = (props) => (
  <h2 class={cn(SECTION_LABEL, "text-ink-muted", props.class)}>{props.children}</h2>
);

/** A neutral chip. Tier A only — never used to carry a state. */
export const Chip: ParentComponent<{
  readonly class?: string;
  readonly title?: string;
  readonly mono?: boolean;
}> = (props) => (
  <span
    title={props.title}
    class={cn(
      "inline-flex max-w-full items-center gap-2xs rounded-chip border border-line bg-raised",
      "px-2xs py-0.5 text-meta leading-4 text-ink-muted",
      props.mono === true ? "font-mono" : "",
      props.class,
    )}
  >
    {props.children}
  </span>
);

/** A definition row: fixed-width term, wrapping value. Used all over detail. */
export const DataRow: ParentComponent<{ readonly term: string; readonly class?: string }> = (
  props,
) => (
  <div class={cn("grid grid-cols-[minmax(0,7.5rem)_minmax(0,1fr)] gap-x-3 gap-y-0.5", props.class)}>
    <dt class="truncate pt-px text-body text-ink-subtle" title={props.term}>
      {props.term}
    </dt>
    <dd class="min-w-0 text-item text-ink">{props.children}</dd>
  </div>
);
