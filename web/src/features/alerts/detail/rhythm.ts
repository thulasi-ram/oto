/**
 * The one rhythm every panel on the alert detail screen shares (§2).
 *
 * These are strings and not a component because the panel chrome itself is
 * already shared — `Panel`/`PanelHeader`/`PanelTitle` in
 * `~/components/ui/surfaces.tsx` — and wrapping it again would fork it. What is
 * NOT shared by that component is the liberal inset this screen wants, so the
 * inset lives here, once, and every panel on the screen spreads the same one.
 *
 * `PANEL_HEADER` and `PANEL_BODY` are settings' rhythm too, so both live in
 * `~/components/ui/panel-rhythm.ts` and are re-exported below rather than
 * kept as two verbatim copies. `PANEL_ROW` is genuinely specific to this
 * screen's panels (delivery, enrichment, case lists) and stays local.
 *
 * Written as oto's own spacing names (`px-lg`, `py-md`) rather than Tailwind
 * numerics: `~/lib/cn`'s `cn()` teaches `tailwind-merge` oto's named spacing
 * steps as the same axis as their numeric equivalents, so `cn("px-3 py-2",
 * PANEL_HEADER)` resolves as a normal same-axis collision — the named step
 * passed later wins outright, not by emission-order luck. That is what makes
 * a named step safe to hand to `PanelHeader`'s `class` prop in place of its
 * own default: the override lands cleanly instead of leaving both classes on
 * the element for the cascade to arbitrate.
 *
 * The values themselves: header inset is tighter vertically than the body it
 * caps, so a stack of panels reads as titled boxes rather than as a ladder of
 * equal bands. Hairline borders and zero radius come from `Panel` itself.
 */
export { PANEL_HEADER, PANEL_BODY } from "~/components/ui/panel-rhythm";

/** One row inside a panel that renders a list. Matches `PANEL_BODY`'s inset. */
export const PANEL_ROW = "px-lg py-md";
