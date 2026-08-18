/**
 * The glyph alphabet for the alert screens — SPEC §0.3.
 *
 * ⭐ SHAPE ENCODES WHAT THE DATA ACTUALLY IS. There are two kinds of fact on an
 * alert row and they get two different visual variables:
 *
 *   - **Severity is ORDERED**, so it gets an ordered variable: the number of
 *     filled bars in an ascending ramp (`SeverityBars`). More ink = worse.
 *   - **Lifecycle state is NOMINAL**, so it gets a *nominal* variable: six
 *     unrelated shapes with no progression between them (`StateGlyph`).
 *
 * ⛔ THE PROTOTYPE'S MONOTONIC CIRCLE-FILL RAMP IS NOT PORTED. A ramp asserts an
 * order, and alert state has none: `suppressed` is orthogonal to `firing` (a
 * suppressed alert is still firing, we are simply not delivering it), `expired`
 * is terminal-by-timeout rather than terminal-by-fix, and `acked` is a human
 * annotation laid *on top of* a state rather than a step past it. Drawn as a
 * half-filled disc, "suppressed" reads "half resolved" — which is the single
 * most dangerous sentence this UI could say at 3am. So no two marks below are
 * the same shape at different fill fractions, and in particular none of the six
 * belongs to a family with more than one other member.
 *
 * Geometry is Bauhaus and uniform: a 14px box, every glyph inking the same
 * 1..13 extent so a column of them cannot appear to jitter as you scan it;
 * 1.5px strokes, butt caps, miter joins, no gradients, no corner radius.
 *
 * Colour comes only from Tier B (`--oto-state-*` via the `*-solid` utilities),
 * because a saturated colour in oto means exactly one thing: this is the state
 * of an alert.
 */
import { For, Match, Switch, type Component, type JSX } from "solid-js";

import { cn } from "~/lib/cn";

/* -------------------------------------------------------------------------- */
/* Severity — the one genuinely ordinal glyph                                 */
/* -------------------------------------------------------------------------- */

/**
 * Three bars on a shared baseline at y=13, ascending 5 / 8.5 / 12 tall, width 3
 * with a 1.5 gap. The run spans x=1..13, the same extent every state mark inks,
 * so the two columns optically agree.
 */
const BARS: readonly { readonly x: number; readonly y: number; readonly h: number }[] = [
  { x: 1, y: 8, h: 5 },
  { x: 5.5, y: 4.5, h: 8.5 },
  { x: 10, y: 1, h: 12 },
];

const BAR_W = 3;

/**
 * Unfilled bars are drawn faint rather than omitted: the ink bounding box is
 * then identical for every severity, so the column reads as one ruler instead
 * of three differently-sized marks.
 */
const UNFILLED_OPACITY = 0.3;

export interface SeverityBarsProps {
  /** How many of the three bars are inked: 0 (unknown) … 3 (critical). */
  readonly filled: number;
  /** Accessible name. Omit when adjacent text already says it — then the glyph is hidden. */
  readonly label?: string;
  readonly class?: string;
}

export const SeverityBars: Component<SeverityBarsProps> = (props) => (
  <svg
    viewBox="0 0 14 14"
    class={cn("size-3.5 shrink-0", props.class)}
    fill="none"
    stroke-linecap="butt"
    stroke-linejoin="miter"
    role={props.label === undefined ? undefined : "img"}
    aria-label={props.label}
    aria-hidden={props.label === undefined ? "true" : undefined}
  >
    <For each={BARS}>
      {(bar, index): JSX.Element => (
        <rect
          x={bar.x}
          y={bar.y}
          width={BAR_W}
          height={bar.h}
          fill="currentColor"
          opacity={index() < props.filled ? 1 : UNFILLED_OPACITY}
        />
      )}
    </For>
  </svg>
);

/* -------------------------------------------------------------------------- */
/* State — six shapes, no ramp                                                */
/* -------------------------------------------------------------------------- */

/**
 * The six marks the alert screens can draw. This is deliberately wider than the
 * `State` enum: `acked` is an annotation and `info` a derived-signal tone, and
 * both need a mark from the same alphabet so the row reads as one system.
 */
export type GlyphState = "firing" | "acked" | "suppressed" | "resolved" | "expired" | "info";

/** Tier B, and only Tier B. Never a raw hex, never a foreign palette. */
const GLYPH_COLOUR: Record<GlyphState, string> = {
  firing: "text-firing-solid",
  acked: "text-acked-solid",
  suppressed: "text-suppressed-solid",
  resolved: "text-resolved-solid",
  expired: "text-expired-solid",
  info: "text-info-solid",
};

const STROKE = 1.5;

/**
 * Apex-up triangle, x 1..13 — the one unstable shape, and genuinely the
 * loudest: 72px² of ink, ahead of every other mark (resolved's hexagon below
 * included, at 52px²). The state a reader least needs to notice must never
 * outweigh the state that means "look now".
 */
const FIRING_TRIANGLE = "M7 1 L13 13 L1 13 Z";

/** An open check: no enclosing form, because an ack annotates rather than contains. */
const ACKED_CHECK = "M2 7.3 L5.6 10.9 L12 3.5";

/** Slash across the muted square, corner to corner inside its stroke. */
const SUPPRESSED_SLASH = "M3.2 10.8 L10.8 3.2";

/**
 * Circumference at r=5.25 is 2π·5.25 ≈ 32.99, so a 1.65/1.65 dash lays exactly
 * ten even ticks with no seam where the path closes: a signal arriving in gaps,
 * then not at all.
 */
const EXPIRED_DASH = "1.65 1.65";

/**
 * Resolved — a filled hexagon, deliberately outside the circle family so it
 * can never pair with expired's ring as "one shape, two fill levels". Six
 * straight sides read as a sealed, closed-case badge rather than a step on
 * any scale. Vertices (7,2.5) (11,5) (11,9) (7,11.5) (3,9) (3,5) shoelace out
 * to 52px² — comfortably under the firing triangle's 72px², so the state you
 * least need to notice is no longer the loudest mark in the column.
 */
const RESOLVED_HEXAGON = "M7 2.5 L11 5 L11 9 L7 11.5 L3 9 L3 5 Z";

export interface StateGlyphProps {
  readonly state: GlyphState;
  /**
   * `solid` paints the Tier B `*-solid` token; `inherit` takes the surrounding
   * `currentColor`, which is what a chip wants — the chip's own `*-text` token
   * is already contrast-checked against its fill.
   */
  readonly tone?: "solid" | "inherit";
  /** Accessible name. Omit when adjacent text already says it — then the glyph is hidden. */
  readonly label?: string;
  readonly class?: string;
}

export const StateGlyph: Component<StateGlyphProps> = (props) => (
  <svg
    viewBox="0 0 14 14"
    class={cn(
      "size-3.5 shrink-0",
      props.tone === "inherit" ? "" : GLYPH_COLOUR[props.state],
      props.class,
    )}
    fill="none"
    stroke-linecap="butt"
    stroke-linejoin="miter"
    role={props.label === undefined ? undefined : "img"}
    aria-label={props.label}
    aria-hidden={props.label === undefined ? "true" : undefined}
  >
    <Switch>
      {/* Firing — a filled triangle. Unstable, apex-up, maximum ink. */}
      <Match when={props.state === "firing"}>
        <path d={FIRING_TRIANGLE} fill="currentColor" />
      </Match>

      {/* Acked — a bare check. A human mark, not a container. */}
      <Match when={props.state === "acked"}>
        <path d={ACKED_CHECK} stroke="currentColor" stroke-width={STROKE} />
      </Match>

      {/* Suppressed — an outlined square struck through: present, not delivered. */}
      <Match when={props.state === "suppressed"}>
        <rect
          x={1.75}
          y={1.75}
          width={10.5}
          height={10.5}
          stroke="currentColor"
          stroke-width={STROKE}
        />
        <path d={SUPPRESSED_SLASH} stroke="currentColor" stroke-width={STROKE} />
      </Match>

      {/* Resolved — a filled hexagon. Closed, settled, sealed shut — not a ring's twin. */}
      <Match when={props.state === "resolved"}>
        <path d={RESOLVED_HEXAGON} fill="currentColor" />
      </Match>

      {/* Expired — a dashed ring. The signal came in gaps, then stopped. */}
      <Match when={props.state === "expired"}>
        <circle
          cx={7}
          cy={7}
          r={5.25}
          stroke="currentColor"
          stroke-width={STROKE}
          stroke-dasharray={EXPIRED_DASH}
        />
      </Match>

      {/* Info — a small filled square. Quiet, orthogonal, deliberately minor. */}
      <Match when={props.state === "info"}>
        <rect x={4.25} y={4.25} width={5.5} height={5.5} fill="currentColor" />
      </Match>
    </Switch>
  </svg>
);

/* -------------------------------------------------------------------------- */
/* Derived signals                                                            */
/* -------------------------------------------------------------------------- */

/*
 * ⛔ THE ALPHABET HAS NO MARK FOR A DERIVED SIGNAL ANY MORE. `WaveGlyph` — a
 * square wave, deliberately outside the state alphabet because flapping is not a
 * state — had exactly one subject, and the flap detector no longer sees it: an
 * episode damped by the case retention window W appends none of the `case.*`
 * events `flap_score` counts, so `is_flapping` reads false precisely when an
 * alert oscillates (ADR 0041 Amendment 1). Nothing in the UI presents flapping
 * as a live signal, so nothing needs the mark. The path was
 * `M1 11 V3 H4.5 V11 H8 V3 H11.5 V11 H13`, on the same 1..13 box and the same
 * stroke as every glyph above, if a derived signal ever earns one again.
 */
