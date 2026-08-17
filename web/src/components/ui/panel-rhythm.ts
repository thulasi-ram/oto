/**
 * The panel inset shared by every screen that stacks `Panel`/`PanelHeader`
 * (`~/components/ui/surfaces.tsx`) into a scroll column: alert detail and
 * settings. Both feature areas independently arrived at the same two
 * numbers — `PANEL_HEADER` tighter vertically than `PANEL_BODY`, so a stack
 * of panels reads as titled boxes rather than a ladder of equal bands — so
 * they are hoisted here rather than kept as two verbatim copies.
 *
 * Each feature's own `rhythm.ts` re-exports these two constants for its
 * existing call sites and keeps everything that is genuinely feature-
 * specific (settings' `FIELD`/`FORM`/`SECTION`/`HELP`/`ROW`, alert detail's
 * `PANEL_ROW`) local to itself.
 */

/** `PanelHeader`'s inset wherever this rhythm applies. */
export const PANEL_HEADER = "px-lg py-sm";

/** A panel body that holds prose, a definition list, a diff or a form. */
export const PANEL_BODY = "px-lg py-md";
