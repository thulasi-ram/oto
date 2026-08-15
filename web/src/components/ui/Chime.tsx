/**
 * The fūrin — 音. The product's own mark, and the only decorative art in oto.
 *
 * This file exists because the glyph used to be two hand-copied SVGs, one in the
 * header and one in the empty states. Consolidating them is safe only if you
 * understand why they differed, so that reason is recorded here rather than in a
 * commit message nobody will read again.
 *
 * # The stroke is per-size on purpose (optical compensation)
 *
 * A hairline is a *device-pixel* fact, not an SVG-unit one. The two copies were
 * authored in different boxes and each was tuned so it landed on the same ~1.25px
 * hairline once rendered:
 *
 *   header  viewBox 0 0 20 20 at 16px → scale 16/20 = 0.8 → 1.5 × 0.8 = 1.20 px
 *   states  viewBox 0 0 40 40 at 32px → scale 32/40 = 0.8 → 1.6 × 0.8 = 1.28 px
 *
 * One shared box therefore cannot have one shared `stroke-width`. This component
 * standardises on the 40-unit box, which changes the *mark's* scale:
 *
 *   mark   40-unit box at 16px → scale 16/40 = 0.4 → 1.20 / 0.4 = 3.0
 *   glyph  40-unit box at 32px → scale 32/40 = 0.8 → 1.28 / 0.8 = 1.6
 *
 * ⛔ DO NOT COLLAPSE THESE TWO NUMBERS INTO ONE. `3.0` looks like a typo next to
 * `1.6` and it is not: a single value here halves the header's stroke (1.6 × 0.4
 * = 0.64px, which a display rounds to a ghost) or triples the empty state's.
 * `Chime.test.tsx` pins both, and it will fail loudly rather than let the glyph
 * quietly thin out.
 *
 * # Two things about the shape DID change, and this is the record of it
 *
 * The stroke was preserved exactly; the geometry was not, and pretending
 * otherwise is how the next reader gets misled.
 *
 *   1. The single source is the *states* path, so the header mark's dome now
 *      spans x 8–32 in 40-unit terms where its own hand-drawn path spanned
 *      10.8–29.2. At 16px that is a difference of at most 0.16 device px — under
 *      a fifth of a pixel, and the price of the two copies becoming one.
 *   2. The `glyph` size gained a zetsu (the clapper) and its bead, so the empty
 *      state renders three strokes plus a circle where it used to render two
 *      paths. That is an intentional addition: without a clapper the silhouette
 *      reads as a lampshade rather than a bell. It is `glyph`-only because at
 *      16px the bead is 0.88px across and the stroke 0.48px — sub-pixel mush.
 *
 * # The mark swings once, and the trigger is the document, not the connection
 *
 * The bell greets the operator when oto opens. That is a *page-load* fact, and
 * the only honest page-load fact available to a component is its own first mount
 * in a document — which is what this file uses (see `hasRung` below).
 *
 * ⛔ IT MUST NEVER BE DRIVEN BY `stream.ts`. The obvious trigger — the
 * connection reaching `live` for the first time — is not the signal it reads as.
 * `stream.ts` picks `connecting` vs `reconnecting` from whether `sessionStorage`
 * holds a resume point (`readResumePoint`), so `connecting` means "this tab has
 * never received a sequenced frame", not "this page just loaded":
 *
 *   - Reload a tab that has ever seen a frame and the resume point survives, so
 *     the connection goes straight to `reconnecting`. The bell would stay silent
 *     for the returning operator — exactly the person the mark is for.
 *   - Open a second tab, or clear site data, and the same running install is
 *     suddenly "connecting" again with no page-load in sight.
 *   - On a quiet install no frame is ever sequenced, so the resume point stays
 *     empty forever and *every* reconnect is a `connecting` — on `online`, on
 *     tab focus, on the 50 s stall check, on a clean end-of-stream, on every
 *     deploy. The bell would ring all day on the install with the least going on.
 *
 * And it would be wrong even if it were reliable: motion tied to a connection
 * transition is a *silent* channel carrying a state fact, which U1 forbids and
 * §M.3 U9 forbids again. Nothing about connection health, severity or count
 * reaches this component, and nothing should.
 *
 * The rule the swing obeys is §M.3 U9 (ADR 0028): one-shot, 180 ms, transform
 * only, at most once per document, absent under `prefers-reduced-motion`. The
 * animation itself and the reason it rotates the `<svg>` rather than an inner
 * `<g>` are in `web/src/index.css`.
 *
 * # What this component deliberately does not do
 *
 * No connection-state coupling, and no colour prop: colour stays at the call
 * site (`text-accent` in the chrome, `text-line-strong` in the quiet states) so
 * this file never has an opinion about severity, and no new token or hex literal
 * enters `web/src`.
 */
import { Show, type Component } from "solid-js";

import { cx } from "./primitives";

export type ChimeSize = "mark" | "glyph";

export interface ChimeProps {
  /** `mark` is the 16px chrome lockup; `glyph` is the 32px quiet state. */
  readonly size: ChimeSize;
  /** Call-site colour and layout. The component sets no colour of its own. */
  readonly class?: string;
}

/** The rendered box per size. Both are Tailwind sizes, not arbitrary values. */
const BOX: Readonly<Record<ChimeSize, string>> = {
  mark: "size-4",
  glyph: "size-8",
};

/**
 * The per-size stroke, derived above. Each value reproduces exactly the device
 * pixels its call site rendered before this component existed.
 */
const STROKE: Readonly<Record<ChimeSize, number>> = {
  mark: 3, // 3.0 × (16/40) = 1.20px — the header's old 1.5 in a 20-unit box
  glyph: 1.6, // 1.6 × (32/40) = 1.28px — the empty state's box is unchanged
};

/** The dome: shoulders, straight sides, and the flared mouth. */
const DOME = "M20 6c-5 0-9 4-9 9v7l-3 5h24l-3-5v-7c0-5-4-9-9-9Z";

/** The mouth arc, hanging just below the rim. */
const MOUTH = "M17 31a3 3 0 0 0 6 0";

/**
 * The zetsu — the clapper, hanging from the dome interior down through the mouth.
 *
 * ⛔ GLYPH ONLY. At `mark` size this is 0.4px of travel per unit: the hairline
 * lands sub-pixel against the mouth arc and reads as a smudge rather than a
 * clapper. A tanzaku (paper strip) is omitted at *both* sizes for the same
 * reason — at 16px it is a 2×4.4px grey blot, and it is not worth a fourth
 * stroke at 32px alone.
 */
const ZETSU = "M20 16.5V28.4";

/**
 * The `data-*` key on `<html>` recording that the mark has already rung here.
 *
 * The latch lives on the **document** rather than in a module variable or in
 * storage, because "once per document" is precisely what the swing means and the
 * document is the thing that dies when the page does. A reload gets a fresh
 * `<html>` and the bell rings again — the returning operator is greeted, which
 * is the case the connection trigger got backwards. A soft navigation that
 * unmounts and remounts the shell gets the same `<html>` and stays silent.
 *
 * ⛔ NEVER `sessionStorage`. Storage OUTLIVES a reload; a document does not.
 * Keeping the latch there would silence the bell for every visit after the first
 * and make "did it ring" depend on the user's storage settings — which is the
 * same category of mistake as reading `connecting` off a resume point.
 */
const RUNG = "otoChimeRung";

/**
 * The swing class if this render is the mark's first in this document, else
 * nothing. Reads and takes the latch in one step, so two marks in one document
 * cannot both claim it.
 *
 * `motion-safe:` is the first of the two reduced-motion guards; the second is
 * the `*` sweep in `index.css`, which needs no help from here.
 */
function swingClass(size: ChimeSize): string {
  // The glyph hangs in a quiet state and in the door. It does not greet, and at
  // 32px in the middle of an empty panel a rotation would read as an error.
  if (size !== "mark") return "";

  const root = document.documentElement;
  if (root.dataset[RUNG] !== undefined) return "";
  root.dataset[RUNG] = "";
  return "motion-safe:oto-chime-swing";
}

export const Chime: Component<ChimeProps> = (props) => {
  // Taken once, in the component body — that IS the mount, and the mount is the
  // trigger. Inlined into the `class` expression below it would instead be a
  // reactive read, re-run on any prop change, and the second run would strip the
  // animation mid-swing because the latch is already taken.
  const swing = swingClass(props.size);

  return (
    <svg viewBox="0 0 40 40" class={cx(BOX[props.size], swing, props.class)} aria-hidden="true">
      <path
        d={DOME}
        fill="none"
        stroke="currentColor"
        stroke-width={STROKE[props.size]}
        stroke-linejoin="round"
      />
      <path
        d={MOUTH}
        fill="none"
        stroke="currentColor"
        stroke-width={STROKE[props.size]}
        stroke-linecap="round"
      />
      <Show when={props.size === "glyph"}>
        <path
          d={ZETSU}
          fill="none"
          stroke="currentColor"
          stroke-width={STROKE[props.size]}
          stroke-linecap="round"
        />
        <circle cx="20" cy="29.6" r="1.3" fill="currentColor" />
      </Show>
    </svg>
  );
};
