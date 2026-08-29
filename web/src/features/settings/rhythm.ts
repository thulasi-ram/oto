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

/* -------------------------------------------------------------------------- */
/* A section that is a LIST OF THINGS rather than a form                      */
/* -------------------------------------------------------------------------- */

/**
 * The four recipes below are one layout, and they exist because `PanelHeader`
 * was the wrong container for a screen whose content is a collection.
 *
 * ⭐⭐ WHAT WAS WRONG. Policies and Templates each wrapped their WHOLE section in
 * one `Panel`: the title, the add button and every row lived inside a single
 * bordered box, with the rows separated by hairlines. That box drew a frame
 * around a heading, which is not a thing a heading needs, and it made every
 * policy a BAND rather than an object — a policy is a rule with six or seven
 * facts stacked under it, and a hairline is not enough separation for that. The
 * templates screen showed the second half of the same mistake: the explanatory
 * sentence under its header sat directly on the panel's left border, because
 * `HELP` carries no inset of its own and there was no `PANEL_BODY` around it.
 * Inside a box, every child owes the box a margin, and one of them had forgotten.
 *
 * ⭐ SO THE BOX MOVES DOWN A LEVEL. The heading and its sentence are page
 * furniture and wear no border; the add control is a full-width affordance under
 * them; and each POLICY or TEMPLATE gets the `Panel` — which is where a border
 * was always earning its keep, because a bordered card is how a reader knows
 * where one rule stops and the next begins.
 */

/** The heading block: the section's name, and one sentence saying what it is. */
export const SECTION_HEAD = "flex flex-col gap-xs";

/**
 * That sentence. Wider measure than `HELP` and one step up in size, because it
 * is a lede under a page heading rather than a note under a control — and, since
 * it no longer lives inside a panel, it needs no inset to keep it off a border.
 *
 * ⛔ `max-w-144` (36rem) AND NOT `max-w-prose`, for the reason `Modal.tsx` spells
 * out at length: a NAMED width key resolves against the spacing namespace in this
 * theme, so `max-w-sm` compiles to 12px and `max-w-prose` to nothing at all. The
 * numeric steps are the ones that work, which is why `EmptyState` reads
 * `max-w-96` and `ErrorState` reads `max-w-112`.
 */
export const SECTION_LEDE = "max-w-144 text-item leading-relaxed text-ink-muted";

/**
 * The full-width add control that closes a list.
 *
 * ⭐ DASHED, AND THAT IS THE ONE PLACE IN THIS PRODUCT A DASHED BORDER IS RIGHT.
 * Every solid border here encloses something that EXISTS; this one encloses the
 * space where the next one would go, and the broken line is the oldest way to
 * draw "not yet". It also keeps the control from reading as another card in the
 * stack, which a solid full-width button immediately would.
 *
 * ⛔ IT SITS BELOW THE LIST AND NOT ABOVE IT. A policy list is read top to
 * bottom in priority order and an add control at the top pushes the thing being
 * read down the screen every time it is looked at. Below, it is where the list
 * ends, which is also where the next one goes.
 */
export const ADD_FULL =
  "h-11 w-full border-dashed text-ink-muted hover:border-line-strong hover:text-ink";

/** The stack of cards a list section renders. */
export const CARD_LIST = "flex flex-col gap-md";
