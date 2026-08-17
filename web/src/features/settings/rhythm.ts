/**
 * One vertical rhythm for every form on the settings screen.
 *
 * Sources, Channels, Policies, Tuning and `SchemaForm` were each written at a
 * different moment and each picked their own `gap-1`/`gap-2`/`gap-3` and their
 * own help-text colour, so four panels stacked in one scroll column read as four
 * different products. The values live here, once, rather than being re-decided
 * per call site — a control that wants to look like the others imports the
 * constant instead of guessing the number.
 *
 * These are class-name strings, not components, deliberately: the controls
 * themselves stay Kobalte's (`TextField`, `Select`), so label/description ids
 * and `aria-describedby` keep being wired by the primitive rather than by a
 * wrapper here. `cn()` resolves the collision in favour of the value passed at
 * the call site, so handing `FIELD` to a `TextField` root simply replaces the
 * root's own default gap.
 *
 * SPEC §2: liberal. Settings is a reading-and-editing screen; hierarchy comes
 * from weight and colour, never from size inflation.
 */

/**
 * A single field: label, control, help, error — stacked tight enough to read as
 * one object. 6px, one step under the gap between fields.
 */
export const FIELD = "flex flex-col gap-xs";

/** Between fields inside one form. 12px — twice the within-field gap. */
export const FORM = "flex flex-col gap-md";

/**
 * Fields side by side on one line — "cluster key + display name", "which alert +
 * which reason".
 *
 * ⛔ `items-start`, NEVER `items-end`, AND THAT IS THE WHOLE POINT OF THE
 * CONSTANT. Ending a row lines up the *bottoms* of the fields, which is only the
 * control line while every field in the row is the same height. The moment one
 * of them carries a `TextFieldDescription` and its neighbour does not, the taller
 * field pushes its own label and input UP and the row renders two staggered
 * controls: the clusters bar shipped exactly that, with 39px between two inputs
 * that read as one line. Starting the row pins every label to one baseline, so
 * every control lands on the next one, and help text hangs below without moving
 * anything.
 *
 * A `Button` on this line therefore needs `self-end` and only earns it when the
 * fields beside it are genuinely equal height; otherwise it belongs on a line of
 * its own under the row.
 */
export const FIELD_ROW = "flex flex-wrap items-start gap-md";

/** Between whole panels inside one section. */
export const SECTION = "flex flex-col gap-lg";

/**
 * Help text under a control. `text-body text-ink-muted` everywhere — the
 * primitives default to `text-meta text-ink-subtle`, which is legible on a dense
 * screen and not on this one.
 */
export const HELP = "text-body leading-snug text-ink-muted";

/**
 * A field label written by hand, where no Kobalte primitive owns it — the
 * matcher box, a `<legend>` that labels one control group.
 *
 * The string is `TextFieldLabel`/`SelectLabel`'s own resolved typography, so a
 * hand-written label and a primitive's label are the same object rather than two
 * that happen to look alike. Three copies of it had already drifted apart across
 * `PoliciesSection` and `ChannelsSection` before this existed.
 */
export const LABEL = "block text-body font-medium leading-none text-ink-muted";

/**
 * The heading of a named group of fields inside one form — "Provider
 * configuration", "Dry run".
 *
 * ⛔ THE GROUP IS BORDERLESS, AND THAT IS DELIBERATE. Both of these used to be a
 * `rounded-control border px-lg py-md` box, which indented everything inside it
 * by 17px and narrowed it by 34px: the channel dialog rendered Name, Credential
 * and Verbosity on one left edge and every provider field on another, inside one
 * form. A group needs to be *named*, not boxed — and small caps names it in the
 * same voice `PanelTitle` uses one level up, at the same left edge as every
 * label under it.
 *
 * A `<legend>` is taken out of its fieldset's flow, so no `gap` reaches it and
 * the space below it is stated here.
 */
export const LEGEND =
  "mb-md text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted";

/**
 * A checkbox and the words beside it.
 *
 * `min-h-6` is the same 24px floor the `Checkbox` control occupies, so a row of
 * them keeps its rhythm whether or not any one label wraps, and a checkbox row
 * is exactly as tall as a chip row beside it. Four sections had written this
 * four ways (`flex items-center gap-xs`, `inline-flex …`, with and without the
 * floor), which is why a stack of them read as uneven.
 */
export const CHECK_ROW = "flex min-h-6 items-center gap-xs";

/**
 * Its label. `text-item`, like every other interactive label on the screen —
 * `TuningSection` had these at `text-body`, one step smaller than the identical
 * control in the channel and policy dialogs.
 */
export const CHECK_LABEL = "cursor-pointer select-none text-item text-ink";

/**
 * Panel body/header padding (§2: `px-3 py-2` → `px-lg py-md` / `px-lg py-sm`).
 * Shared verbatim with the alert detail screen's panel rhythm, so both live
 * in `~/components/ui/panel-rhythm.ts` and are re-exported here rather than
 * kept as a second copy.
 */
export { PANEL_BODY, PANEL_HEADER } from "~/components/ui/panel-rhythm";

/**
 * A row in a panel's list. Hairline separator, liberal padding, and a real
 * minimum height so a one-line row is not shorter than a two-line one by half.
 * 48px matches the comfortable alert-row height in §2 — settings rows are not
 * virtualised, so this is a floor rather than a measured constant.
 */
export const ROW = "border-b border-line px-lg py-md last:border-b-0";

/** `ROW`, for the common case of a single line of content that fills the width. */
export const ROW_SINGLE = `${ROW} flex min-h-12 flex-wrap items-center gap-sm`;
