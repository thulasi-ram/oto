/**
 * Decorative ink — SPEC §M.9, ADR 0035.
 *
 * A mark that sits in the DOM as *content* is an inline `<svg>` taking
 * `currentColor`; that is `Wordmark.tsx` and `Logo.tsx` and they are not
 * changing. Decoration is the other thing: a stroke that has to sit **behind**
 * or **beside** something without occupying a slot in the layout, which an
 * inline element cannot do and an `<img>` cannot theme. So the art arrives as a
 * `mask-image` over a flat `--oto-wash` fill, and this component is the only
 * intended way to take it.
 *
 * ⭐ IT EXISTS SO THE CONTRACT CANNOT BE TAKEN APART. `oto-ink` in `index.css`
 * carries the three CSS halves — `pointer-events: none`, `user-select: none`,
 * and `display: none` under `forced-colors: active`, where the OS replaces a 6%
 * wash with a system colour at full strength and it becomes an opaque slab. This
 * file carries the fourth, `aria-hidden`, which no stylesheet can set, plus the
 * `/motifs/` URL, which a call site can misspell into silence. Four motifs are
 * spent through it and none of them repeats any of that.
 *
 * ⛔ NOTHING HERE ANIMATES, AND NOTHING HERE MAY. U9's decorative one-shot
 * budget is a whole document's worth and the fūrin's 180 ms greeting already
 * owns it (ADR 0028) — a fading wash or a drifting cloud would be a second one.
 * The ink is static, which is also what makes it safe to render behind text.
 *
 * ⛔ IT IS NEVER THE ONLY CHANNEL (U1). A motif is a second reading of a fact
 * the copy already states in full. Delete every `<Ink>` on a screen and the
 * screen must still say everything it said before.
 */
import type { Component, JSX } from "solid-js";

import { cn } from "~/lib/cn";

/**
 * The art in `web/public/motifs/`. A union rather than a string, so a typo is a
 * compile error instead of a mask that resolves to nothing and paints a flat
 * rectangle of wash across whatever it was positioned over.
 */
export type Motif = "enso" | "swipe" | "rule" | "kumo" | "sakura";

/**
 * The §M.9 tint steps. `wash` is the ambient 6%; the `heading` pair is 12%,
 * because 6% behind an 18 px page heading is invisible; the `rule` pair is 48%.
 *
 * ⭐ THE FOURFOLD JUMP IS OCCUPANCY, NOT TASTE. `heading` goes UNDER text and
 * spends every percent it gains against that text's contrast. `rule` goes BESIDE
 * it, on empty canvas, and owes nothing to anything — a 4 px tapered stroke at
 * 12% is not a quiet brush, it is an invisible one, and what survives of it
 * reads as a `border-bottom`.
 *
 * All four hues are Tier A (§M.2), so none can be mistaken for a state — see
 * §M.9 for why they read as two hues in light and nearly as one in dark.
 */
export type Tint = "wash" | "heading" | "heading-accent" | "rule" | "rule-accent";

const TINT: Readonly<Record<Tint, string>> = {
  wash: "var(--oto-wash)",
  heading: "var(--oto-wash-heading)",
  "heading-accent": "var(--oto-wash-heading-accent)",
  rule: "var(--oto-wash-rule)",
  "rule-accent": "var(--oto-wash-rule-accent)",
};

export interface InkProps {
  readonly motif: Motif;
  /** Defaults to the ambient `wash`. */
  readonly tint?: Tint;
  /** `mask-size`, per layer. Defaults to filling the box (`100% 100%`). */
  readonly size?: string;
  /** `mask-position`, per layer. Defaults to `center`. */
  readonly position?: string;
  /**
   * An extra mask layer intersected with the art — a gradient that clears a
   * column the ink may not enter.
   *
   * ⭐ THIS IS THE PART WORTH TAKING SERIOUSLY. A carve-out makes the ink
   * *geometrically incapable* of reaching the text it sits behind: nothing to
   * tune, nothing to eyeball, and it holds at every breakpoint without a media
   * query. The alternative — picking an opacity low enough to look safe — is how
   * §M.4's measured pairs quietly stop holding, and nothing in CI would see it:
   * `contrast.test.ts` measures token pairs, not composites, and the axe row
   * that would is the one UNWRITTEN entry in §M.7.
   *
   * When this is set, `size` and `position` must name a value for BOTH layers.
   */
  readonly carve?: string;
  /** Position and box. The component sets neither — it has no opinion on either. */
  readonly class?: string;
}

export const Ink: Component<InkProps> = (props) => {
  const style = (): JSX.CSSProperties => {
    const art = `url(/motifs/${props.motif}.svg)`;
    return {
      "--oto-ink-motif": props.carve === undefined ? art : `${art}, ${props.carve}`,
      ...(props.tint === undefined ? {} : { "--oto-ink-tint": TINT[props.tint] }),
      ...(props.size === undefined ? {} : { "--oto-ink-size": props.size }),
      ...(props.position === undefined ? {} : { "--oto-ink-position": props.position }),
      ...(props.carve === undefined ? {} : { "--oto-ink-composite": "intersect" }),
    };
  };

  return <span aria-hidden="true" class={cn("oto-ink", props.class)} style={style()} />;
};

/**
 * A carve-out that clears a centred column of `width`, at any viewport width.
 *
 * `50%` is the *element's* middle, so this is only a guarantee when the element
 * it is applied to spans the same box the column is centred in — which is why
 * every call site stretches its `<Ink>` to `inset-0` of the centring wrapper
 * rather than sizing it to the art.
 */
export const clearColumn = (width: string): string =>
  `linear-gradient(to right, #000 0, #000 calc(50% - ${width} / 2), ` +
  `transparent calc(50% - ${width} / 2), transparent calc(50% + ${width} / 2), ` +
  `#000 calc(50% + ${width} / 2), #000 100%)`;
