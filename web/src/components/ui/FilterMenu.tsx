/**
 * One filter axis, behind one dropdown — the shape every enumerable filter in
 * the product now takes.
 *
 * ⭐ WHY THIS REPLACED THE SEGMENTED STRIPS. `ToggleGroup` put every value of
 * every axis on the bar at once, which is the honest thing to do when an axis
 * has two values and the wrong thing at four: `/cases` spent two thirds of its
 * filter row on six chips, the alert Timeline spent a whole row on nine, and
 * both read as TAB STRIPS — a row of pills where exactly one looks chosen. A tab
 * strip says *"you are in one of these"*. A filter says *"the list is narrowed to
 * these"*, which is a different sentence and needs a different control, because
 * the two disagree the moment more than one value is on.
 *
 * So an axis is a trigger plus a checkbox list, and the two halves split the job:
 *
 *   - **The trigger says what the axis is doing, at rest.** `Event kinds` when it
 *     is narrowing nothing; `Event kinds Lifecycle +2` when it is. That is the
 *     whole price of a popover being paid back — see `FilterBar.tsx`'s header,
 *     which argues it at length and where this recipe came from.
 *   - **The list is checkboxes, because the axis is a set.** A checkbox is the
 *     one control that says "several of these at once" without being read; a
 *     pressed pill in a strip of pills does not.
 *
 * ⭐ NOTHING CHECKED MEANS EVERYTHING, AND THE LIST SAYS SO. Every axis here is
 * a narrowing: an empty set narrows nothing, so it means *all*, which is also
 * what the server does with an omitted parameter. `CheckList` therefore draws
 * its `All` row in the checked state whenever nothing else is, so the control
 * never sits with every box empty looking like a list that can return nothing.
 * Unchecking the last value falls back to `All` rather than to a filter matching
 * nothing.
 *
 * ⛔ IT SPENDS NO ACCENT AND NO STATE HUE. An active trigger lifts with neutrals
 * — a raised surface, the stronger hairline, and `text-ink-muted` → `text-ink` —
 * so a band of these reads correctly in greyscale and costs the severity column
 * nothing (§0.6, §M.2). Glyphs passed in as `icon`/`leading` are drawn in Tier A
 * ink by their call site (`tone="inherit"`); the SHAPE is the vocabulary chrome
 * may borrow, never the hue.
 */
import { For, Show, type Component, type JSX, type ParentComponent } from "solid-js";

import { Popover, PopoverContent, PopoverTrigger } from "~/components/ui/Popover";
import { SECTION_LABEL } from "~/components/ui/surfaces";
import { Checkbox } from "~/components/ui/Checkbox";
import { cn } from "~/lib/cn";

/* -------------------------------------------------------------------------- */
/* The one control recipe                                                     */
/* -------------------------------------------------------------------------- */

/**
 * Every filter trigger in the product wears exactly this, so a band of them
 * reads as one set of instruments rather than as whatever each screen reached
 * for. Exported because `/alerts` puts real Kobalte `Select`s in the same band
 * and they have to match.
 *
 * No border. Depth instead comes from one hue at three strengths, so a
 * trigger's state reads as the same fill going deeper rather than as a
 * different object switching on:
 *
 *   rest 35 % → hover 60 % → narrowing 100 % + `font-medium`
 *
 * A flat `bg-sunken` at rest was tried and dropped: it sits only one step
 * off `bg-raised` (`#e9ddc0` vs `#f1e9d5`) as an *opaque* fill, so every
 * trigger read as the same solid beige box whether it was narrowing anything
 * or not — a second, competing chip language, and a harder call to make than
 * a border was. A translucent low percentage of the same token gives every
 * trigger a barely-there presence (so the bar reads as a row of controls, not
 * bare words floating in space) while keeping the narrowing one unmistakably
 * the deepest fill in the row — the eye still has exactly one place to land.
 *
 * `border-0` is explicit, not just an omission: `selectClass` in
 * `features/alerts/FilterBar.tsx` lays this recipe over Kobalte's
 * `SelectTrigger`, whose own base class hardcodes `border border-line` and a
 * `bg-surface` fill (`components/ui/Select.tsx`) — a plain absence here would
 * leave that default standing, and `Cluster`/`Since` would grow the border
 * and the wrong opaque fill back while `Status`/`Severity`/`Group` sat on
 * the translucent one.
 */
export const FILTER_TRIGGER =
  "inline-flex h-7 shrink-0 cursor-pointer items-center gap-2xs rounded-control border-0 px-xs " +
  "text-item transition-colors duration-100";
export const FILTER_TRIGGER_OFF =
  "bg-raised/35 text-ink-muted hover:bg-raised/60 hover:text-ink";
export const FILTER_TRIGGER_ON = "bg-raised font-medium text-ink";

/**
 * A full-width control row inside a menu.
 *
 * ⛔ `cursor-pointer` IS NOT DECORATION AND IT IS NOT REDUNDANT. Tailwind v4's
 * preflight sets `button { cursor: default }` — reversing v3 — so a row that
 * lights up under the cursor was still showing the arrow, which says "this
 * highlight is not a click target" one pixel above a checkbox that is. The rows
 * in `DropdownMenu`/`Select`/`Combobox` keep `cursor-default` on purpose: those
 * are real listbox and menu items and they follow the platform's own menus.
 * These are checkbox and choice rows inside a plain popover panel — the same
 * thing `ToggleGroup` was before ADR 0033, and it has carried `cursor-pointer`
 * all along.
 */
export const MENU_ROW =
  "flex min-h-8 w-full cursor-pointer items-center gap-xs rounded-control px-xs text-item " +
  "transition-colors duration-100";
export const MENU_ROW_QUIET = "text-ink-muted hover:bg-raised hover:text-ink";
/** The one accent a menu may spend: the single-choice row you are currently in. */
export const MENU_ROW_ACTIVE = "bg-accent-fill text-ink";

/**
 * The menu caret. Points down at rest and flips when the menu is open, so the
 * shape says which way the panel will move — read off the trigger's own
 * `data-expanded`, never off a second copy of the state.
 */
export const CaretGlyph: Component = () => (
  <svg
    viewBox="0 0 12 12"
    class={
      "size-3 shrink-0 text-ink-subtle transition-transform duration-100 " +
      "group-data-[expanded]:rotate-180"
    }
    aria-hidden="true"
  >
    <path
      d="M2.6 4.4 6 7.8l3.4-3.4"
      fill="none"
      stroke="currentColor"
      stroke-width="1.4"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);

/* -------------------------------------------------------------------------- */
/* The trigger                                                                */
/* -------------------------------------------------------------------------- */

export interface FilterMenuProps {
  readonly label: string;
  /** What this axis is currently narrowing by, or `undefined` when it is not. */
  readonly value?: string | undefined;
  /** A glyph from the §0.3 alphabet, drawn in Tier A ink. Optional. */
  readonly leading?: JSX.Element;
  /** The long form, for a hover. The trigger itself can only afford a summary. */
  readonly title?: string | undefined;
  /** Panel width. Defaults to `w-72`, which holds a label and a value comfortably. */
  readonly width?: string;
}

/**
 * One axis, behind a menu — and saying on its face what it is doing.
 *
 * `value` is the whole design: `undefined` means the axis is not narrowing
 * anything and the trigger shows only its name, quietly; a string means it is,
 * and the trigger lifts and says what to. The axis name stays `text-ink-subtle`
 * in both cases, so the *value* is what the eye lands on — hierarchy from weight
 * and tone, at one size, because there is only one size a 28 px control can hold.
 *
 * The trigger's own text is its accessible name. No `aria-label` is added on
 * top: it would have to repeat the same two words, and a label that drifts from
 * the text beside it is worse than no label at all.
 */
export const FilterMenu: ParentComponent<FilterMenuProps> = (props) => {
  // ⛔ FOCUS LANDS ON THE PANEL, NOT ON ITS FIRST ROW. Kobalte's default
  // `onOpenAutoFocus` moves focus straight to the first tabbable descendant —
  // here, the `All`/currently-checked row — so the very act of clicking the
  // trigger leaves a keyboard user's element focused, and the browser's own
  // `:focus-visible` heuristic (see `index.css`'s U7 rule) reads a script-moved
  // focus as keyboard-worthy and rings it, on a plain mouse open. Focusing the
  // panel itself instead — `tabIndex={-1}`, already `outline-none` in
  // `Popover.tsx` — gives the popover a focus target with nothing to ring, and
  // a keyboard user who then presses Tab still reaches the first row and gets
  // a real, deserved ring.
  let panel: HTMLDivElement | undefined;
  return (
    <Popover>
      <PopoverTrigger
        class={cn(
          "group",
          FILTER_TRIGGER,
          props.value === undefined ? FILTER_TRIGGER_OFF : FILTER_TRIGGER_ON,
        )}
        title={props.title}
      >
        {props.leading}
        <span class={cn("font-normal", props.value === undefined ? "" : "text-ink-subtle")}>
          {props.label}
        </span>
        {/* ⛔ A REAL SPACE, NOT THE FLEX GAP. `gap-2xs` separates these two spans
            on screen and not at all in the accessibility tree, where the name
            would concatenate to "Severitycritical +1". A whitespace-only text run
            is not rendered as a flex item (CSS Flexbox §4), so this costs no
            layout and buys the trigger a name a screen reader can say. */}
        {" "}
        <Show when={props.value}>
          {(value) => <span class="max-w-[9rem] truncate">{value()}</span>}
        </Show>
        <CaretGlyph />
      </PopoverTrigger>
      {/* `p-md` and a column of `gap-md` bands: a menu holding three axes needs
          the axes to read as three things, and the only separator used here is
          space — a hairline between every pair would draw more ink than the
          controls do. */}
      <PopoverContent
        ref={panel}
        tabIndex={-1}
        class={cn(props.width ?? "w-72", "p-md")}
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          panel?.focus();
        }}
      >
        <div class="flex flex-col gap-md">{props.children}</div>
      </PopoverContent>
    </Popover>
  );
};

/**
 * A labelled band inside a menu.
 *
 * The label is the quietest type in the app (`text-micro`, `text-ink-subtle`)
 * on purpose: each axis is named exactly once, above the control that narrows
 * it, so nothing inside has to repeat the word.
 */
export const MenuSection: ParentComponent<{ readonly label: string }> = (props) => (
  <div class="flex min-w-0 flex-col gap-2xs">
    <h3 class={cn(SECTION_LABEL, "flex items-center gap-2xs text-ink-subtle")}>{props.label}</h3>
    {props.children}
  </div>
);

/* -------------------------------------------------------------------------- */
/* The set of values — checkboxes                                             */
/* -------------------------------------------------------------------------- */

export interface CheckOption<T extends string> {
  readonly value: T;
  readonly label: string;
  /** A glyph from the §0.3 alphabet, drawn `tone="inherit"`. Optional. */
  readonly icon?: JSX.Element;
  readonly title?: string | undefined;
}

export interface CheckListProps<T extends string> {
  /** The group's accessible name. Required: an unlabelled group is a guessing game. */
  readonly legend: string;
  readonly options: readonly CheckOption<T>[];
  readonly value: readonly T[];
  readonly onChange: (next: readonly T[]) => void;
  /**
   * The word for the empty set. Defaults to `All` — override where the screen
   * has a truer sentence for it (`Any time`, `Both`, `Everything oto has seen`).
   */
  readonly allLabel?: string;
  readonly allTitle?: string | undefined;
  /**
   * What the `All` row emits, for the axis where the empty set is NOT "all".
   *
   * ⛔ ONE AXIS IN THE PRODUCT GENUINELY NEEDS THIS AND IT IS `/cases`'s EPISODE
   * STATE. That screen reads an absent `?state=` as the QUEUE — open episodes
   * only — because a list of every episode since the beginning of retention
   * answers a question nobody asked. So its "everything" is `open,closed` spelled
   * out, not the empty set, and clearing it lands back on the queue. Leave this
   * unset everywhere the empty set really does mean all, which is everywhere else.
   */
  readonly allValue?: readonly T[];
}

/**
 * A column of checkboxes, one per value, with the empty set drawn as its own
 * checked row at the top.
 *
 * ⭐ `All` IS A REAL ROW AND IT IS CHECKED WHEN NOTHING ELSE IS. The alternative
 * — leaving every box empty to mean "everything" — is a control whose resting
 * state is indistinguishable from a filter that can return nothing, and it is
 * the state an operator lands in by unchecking the last box. Clicking `All`
 * clears the axis; unchecking the last value lands on `All` rather than on a set
 * that matches nothing.
 *
 * ⛔ THE ROW IS A `<button>` WRAPPING A NON-INTERACTIVE `Checkbox`, not a label
 * beside one. Kobalte's `Checkbox` owns a visually-hidden `<input>`, so nesting
 * it inside a button would put a control inside a control; `readOnly` keeps it
 * as the indicator it is here and the button is the one focus stop. The row's
 * text is its accessible name, and `aria-pressed` is what says checked.
 */
export const CheckList = <T extends string>(props: CheckListProps<T>): JSX.Element => {
  const on = (v: T): boolean => props.value.includes(v);
  /** The set the `All` row stands for — empty unless the axis says otherwise. */
  const allSet = (): readonly T[] => props.allValue ?? [];
  /** Values are unique within an axis, so agreeing on size is agreeing. */
  const none = (): boolean => props.value.length === allSet().length;

  const toggle = (v: T): void => {
    props.onChange(on(v) ? props.value.filter((x) => x !== v) : [...props.value, v]);
  };

  return (
    <div role="group" aria-label={props.legend} class="-mx-xs flex flex-col">
      <button
        type="button"
        aria-pressed={none()}
        title={props.allTitle}
        class={cn(MENU_ROW, MENU_ROW_QUIET, none() && "font-medium text-ink")}
        onClick={() => props.onChange(allSet())}
      >
        <Checkbox checked={none()} readOnly class="pointer-events-none shrink-0" />
        <span class="min-w-0 flex-1 truncate text-left">{props.allLabel ?? "All"}</span>
      </button>

      <For each={props.options}>
        {(opt) => (
          <button
            type="button"
            aria-pressed={on(opt.value)}
            title={opt.title}
            class={cn(MENU_ROW, MENU_ROW_QUIET, on(opt.value) && "font-medium text-ink")}
            onClick={() => toggle(opt.value)}
          >
            <Checkbox
              checked={on(opt.value)}
              readOnly
              class="pointer-events-none shrink-0"
            />
            {opt.icon}
            <span class="min-w-0 flex-1 truncate text-left">{opt.label}</span>
          </button>
        )}
      </For>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* The one-of-many axis — radio-shaped, but a button list                      */
/* -------------------------------------------------------------------------- */

export interface ChoiceOption<T extends string> {
  readonly value: T;
  readonly label: string;
  readonly icon?: JSX.Element;
  readonly title?: string | undefined;
  /** A count or hint, right-aligned and quiet. */
  readonly hint?: string | undefined;
}

export interface ChoiceListProps<T extends string> {
  readonly legend: string;
  readonly options: readonly ChoiceOption<T>[];
  readonly value: T;
  readonly onChange: (next: T) => void;
}

/**
 * One of several, as a column of rows with `aria-current` on the live one.
 *
 * ⛔ DELIBERATELY NOT A `radiogroup`, AND DELIBERATELY NOT TABS. Both move the
 * selection with the arrow keys, and every axis this renders fires a different
 * REQUEST when it changes — so roving-focus-plus-activation would send one query
 * per arrow key press while somebody merely walks the list. A column of ordinary
 * buttons has one focus stop per row, activates on Enter/Space/click only, and
 * is the same idiom the view presets beside it already use.
 */
export const ChoiceList = <T extends string>(props: ChoiceListProps<T>): JSX.Element => (
  <div role="group" aria-label={props.legend} class="-mx-xs flex flex-col">
    <For each={props.options}>
      {(opt) => (
        <button
          type="button"
          aria-current={props.value === opt.value ? "true" : undefined}
          title={opt.title}
          class={cn(MENU_ROW, props.value === opt.value ? MENU_ROW_ACTIVE : MENU_ROW_QUIET)}
          onClick={() => props.onChange(opt.value)}
        >
          {opt.icon}
          <span class="min-w-0 flex-1 truncate text-left">{opt.label}</span>
          <Show when={opt.hint}>
            {(hint) => (
              <span class="shrink-0 tabular-nums text-meta text-ink-subtle">{hint()}</span>
            )}
          </Show>
        </button>
      )}
    </For>
  </div>
);

/* -------------------------------------------------------------------------- */
/* Trigger copy                                                               */
/* -------------------------------------------------------------------------- */

/** `first +n`, the only summary a 32 px trigger can hold. */
export function summarise(parts: readonly string[]): string | undefined {
  const [first, ...rest] = parts;
  if (first === undefined) return undefined;
  return rest.length === 0 ? first : `${first} +${rest.length}`;
}

/* -------------------------------------------------------------------------- */
/* Sort — the one axis that is a direction rather than a set                   */
/* -------------------------------------------------------------------------- */

/**
 * The two sort directions, as a glyph.
 *
 * ⭐ IT IS AN ARROW THROUGH A STACK, NOT A BARE CHEVRON. A chevron beside the
 * word "Newest first" is the same shape as the caret that opens the menu it sits
 * in, one line apart, meaning two different things. The three rules say "a list"
 * and the arrow says which way it runs, which is a sentence a 14 px box can hold
 * without ambiguity.
 */
export const SortGlyph: Component<{
  readonly direction: "desc" | "asc";
  readonly class?: string;
}> = (props) => (
  <svg
    viewBox="0 0 14 14"
    class={cn("size-3.5 shrink-0", props.class)}
    fill="none"
    stroke="currentColor"
    stroke-width="1.4"
    stroke-linecap="round"
    aria-hidden="true"
  >
    {/* The stack: three rules, widest at the top when descending. */}
    <path d={props.direction === "desc" ? "M1 3h6M1 7h4.5M1 11h3" : "M1 3h3M1 7h4.5M1 11h6"} />
    {/* The arrow, on the right, pointing the way the list runs. */}
    <path d={props.direction === "desc" ? "M11 2v10M8.8 9.6 11 12l2.2-2.4" : "M11 12V2M8.8 4.4 11 2l2.2 2.4"} />
  </svg>
);
