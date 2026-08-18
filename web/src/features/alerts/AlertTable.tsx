/**
 * The alert table. The screen oto is judged on.
 *
 * The design brief is one sentence: **a tired on-call engineer at 3am must be
 * able to scan it.** Everything here follows from that.
 *
 *   - It is a real `<table>`. Screen readers, in-page find and "copy the row"
 *     all work for free, and none of them work in a div soup.
 *   - Row height is `--oto-row-h` exactly (U6), which is what makes the
 *     virtualiser exact and what stops the table reflowing when data lands. The
 *     token moved up to a liberal 48/36 px; nothing here hardcodes a height, so
 *     the row got taller without a single call site learning the number. That
 *     height is *spent*, not merely declared: the alert name sits on the first
 *     line and its namespace/service and summary on a second beneath it.
 *   - Every fixed column is budgeted against 976 px — the width this table
 *     actually gets at a 1280 px viewport — so the elastic alert column keeps a
 *     real width there instead of being crushed to nothing. See `TRACK`.
 *   - The gutter is two bars, not one: a 2 px focus rail that is *always*
 *     present (transparent at rest, accent when focused, so focus can never
 *     shift the row sideways) and then the 3 px state bar (§M.2) — pastel fill,
 *     saturated bar. Calm at a distance, unmistakable at a glance.
 *   - Severity is the glyph, state is the colour (U8), and both always have a
 *     word next to them (U1). They are also the **only** saturated things in
 *     the row: the accent appears on the focus rail and nowhere else, so
 *     nothing in the chrome competes with the signal.
 *   - Keyboard first: `j`/`k` or arrows move, `Home`/`End` jump, `Enter` opens,
 *     and the focused row is scrolled clear of the sticky header rather than
 *     left underneath it.
 *   - **Focus is keyed to the alert's id, never to its row index.** Alerts
 *     arrive over SSE while the operator is reading, and an index-keyed cursor
 *     silently changes which alert is selected the moment one is inserted above
 *     it. That is how the wrong alert gets acknowledged at 3am.
 *   - No hover-revealed controls. Anything the row can do is either visible or
 *     behind an explicit, always-present menu trigger. A control that appears
 *     under the cursor on a list that re-sorts as alerts fire is a misclick
 *     generator, and it is unreachable by keyboard until it is too late.
 *   - ⛔ **THE ROW CARRIES NO ACKNOWLEDGE AND NO SNOOZE**, and that is the
 *     decision, not an omission. A row in this list is an **Alert** — the
 *     identity of a label set, which outlives every one of its firings — while
 *     acknowledging is a receipt on ONE firing episode, so it lives on the
 *     **Case** surface (`/cases`, `/cases/:id`). Snooze *is* alert-scoped but is
 *     a dialog about one alert, so it is offered from that alert's own screen
 *     rather than from a hundred rows at once.
 *   - ⭐ **THE ONE EXCEPTION IS `Resume`, ON THE QUIET TAB ONLY** (see `quiet`).
 *     That tab is the list of alerts oto is currently holding its tongue about,
 *     which makes it the one screen where waking one is a gesture whose subject
 *     *and whose whole set* are both on screen. It is the undo of a snooze and
 *     nothing else: there is no bulk snooze and never will be.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  createUniqueId,
  on,
  onCleanup,
  type Component,
  type JSX,
} from "solid-js";
import { A } from "@solidjs/router";

import type { Alert, RuleSnapshot } from "~/api/types";
import { RelativeTime, Elapsed } from "~/components/Time";
import {
  FlappingChip,
  SeverityMark,
  STATE_BAR,
  StateChip,
  normaliseSeverity,
} from "~/components/StateChip";
import { SnoozeChipUnknownUntil } from "~/components/SnoozeChip";
import { Button } from "~/components/ui/Button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "~/components/ui/DropdownMenu";
import { Chip, SECTION_LABEL } from "~/components/ui/surfaces";
import { density } from "~/design/theme";
import { cn } from "~/lib/cn";
import { count as fmtCount, duration as fmtDuration, truncate } from "~/lib/format";
import { createVirtualiser, readRowHeight } from "~/lib/virtual";

export interface AlertTableProps {
  readonly alerts: readonly Alert[];
  /**
   * Whether these rows came from the **Quiet** tab.
   *
   * ⭐ IT IS NO LONGER A `boolean | null`, AND THE MISSING THIRD STATE IS THE
   * IMPROVEMENT. `AlertDTO` carries no `snooze` field, so a row's snooze state is
   * only knowable from the query that fetched it — and the list used to be able
   * to run with `?snoozed=` unset, which made the answer "unknown" and left the
   * chip unsaid on rows that were in fact quiet. The list is now always on one
   * tab or the other, so the answer is always known.
   */
  readonly quiet?: boolean;
  /**
   * **What the rule said**, keyed by snapshot id (ADR 0025).
   *
   * The row itself carries only `rule.id` — `alerts/api` may not name the rules
   * module's types — so the screen resolves a whole page of those ids in ONE
   * call and hands the result down here. An id that is not in the map yet is
   * *loading*; an id that will never be in it is *unreadable*, and the cell says
   * which rather than showing an empty space for both.
   */
  readonly rules?: ReadonlyMap<string, RuleSnapshot>;
  /** True while the batch above is in flight, so a blank cell can say why. */
  readonly rulesPending?: boolean;
  /** Picking a label filters by it — the fastest drill-down there is. */
  readonly onFilterLabel: (name: string, value: string) => void;
  /**
   * End the quiet period on these alerts. **Given only on the Quiet tab**, which
   * is what puts the `Resume` column on the row at all.
   *
   * ⭐ IT TAKES A LIST OF IDS FROM THE FIRST DAY IT EXISTS, AND IT IS ONE ROW'S
   * BUTTON THAT CALLS IT WITH ONE. §B.8 offers a bulk wake (`POST
   * /alerts/unsnooze`, an account rather than a bare count) precisely because
   * "resume these nine" is the gesture this tab is for; a `(id: string) => void`
   * here would have to be widened, and every call site rewritten, the day the
   * checkbox column lands. The route decides which endpoint a given length
   * deserves — one id is the single-alert `unsnooze`, which is the only form that
   * can answer `412 not_snoozed` about a specific alert.
   */
  readonly onResume?: ((alertIds: readonly string[]) => void) | undefined;
  /** The alert ids whose resume is in flight, so a row can say it is working. */
  readonly resuming?: ReadonlySet<string>;
  /** Rendered after the last row: the "load more" affordance or a total. */
  readonly footer?: JSX.Element;
}

/* -------------------------------------------------------------------------- */
/* The column model — ONE source of truth                                     */
/* -------------------------------------------------------------------------- */

/**
 * ⭐ EVERY FIXED TRACK IN THE ROW, AS A RAW CSS LENGTH, IN ONE PLACE.
 *
 * The header, the `<colgroup>` and the body cells all resolve their width from
 * here — via a single `<col>` per column, which is the one mechanism a table
 * has for sizing a header and its body cells from the same declaration. Two
 * lists of widths (one on `<th>`, one on `<td>`) is how a table quietly stops
 * lining up; there is no second list to drift from.
 *
 * `GroupedAlerts` imports this too, so the grouped view's grid lands on the
 * same gutter, the same severity track and the same trailing "last seen" edge
 * as the list. The two views are then legibly one system rather than two
 * screens that happen to show alerts.
 *
 * Exactly ONE column is absent from this table: `alert`. It is the elastic,
 * truncating column, and it takes whatever is left (see `ELASTIC` below).
 *
 * ⛔ THESE ARE A BUDGET, NOT A WISH LIST, AND THE BUDGET IS 976 px.
 *
 * The narrowest supported viewport is 1280 px, and the table never sees all of
 * it: 32 px of shell gutter, the 256 px filter rail and the 16 px inset between
 * them leave **976 px**. The previous widths summed to 1093 px of *fixed*
 * track before the elastic column asked for a single pixel — so the alert name,
 * the most important thing on this screen, was allotted nothing and the whole
 * table fell into horizontal scroll inside the virtualiser's scroller.
 *
 * ⭐ AND A BUDGET IS ONLY A BUDGET UNDER `table-layout: fixed` (see the `<table>`).
 *
 * This whole object was inert for exactly as long as the table laid out `auto`.
 * `<col width>` under auto layout is a *preference* the browser is free to
 * ignore in favour of each column's min-content width, and it did: measured at
 * 1280 px the `Rule` track took 448 px against the 120 px declared here, `State`
 * took 218 against 144, and the table came out 1774 px wide inside a 1008 px
 * scroller — 766 px of horizontal overflow, with everything from `State`
 * rightward off screen. Under `fixed` these lengths are the layout, the elastic
 * column is the only one that moves, and every cell clips instead of pushing.
 *
 * Every width below is derived from what the cell actually renders (and, for
 * the numeric columns, from the header word, which is the wider of the two and
 * therefore the real floor) — all of them re-measured in the browser rather
 * than guessed. The fixed tracks total **645 px** at the narrow breakpoint,
 * which leaves the alert name **331 px** at 1280 px and 363 px in the design
 * preview's slightly wider frame. Anything that grows a track here comes
 * straight out of the name, so a change to this object is a change to the alert
 * name's width — say so.
 */
export const TRACK = {
  /** The 2 px focus rail plus the 3 px state bar, flush to the row's edge. */
  gutter: "5px",
  /**
   * Bars (14) + gap + the label. Measured, not guessed: the widest label the
   * component can produce is not "critical" (57 px) but the "no severity"
   * fallback (82 px), and severities are raw operator strings anyway, so the
   * track is sized for 88 px of content. Under `fixed` a short track would not
   * quietly widen — it would clip the word off a glyph U1 says must never be
   * alone.
   */
  severity: "7rem",
  /** A truncating expression with the whole of it in the tooltip. Wide only. */
  rule: "7.5rem",
  /**
   * `StateChip` plus whichever derived chip is beside it, measured:
   * `Firing`+`Flapping` is 131 px and `Firing`+`Notifications held` is wider
   * still, so 144 px of content clears the pair that actually occurs on a busy
   * list. The three-chip row (194 px) clips its last chip, which is the
   * deliberate part — a third chip must never be able to shove `Last seen`
   * sideways.
   */
  state: "10.5rem",
  /** The cluster chip measures 76 px and the header word 51; 72 px of content. */
  cluster: "6rem",
  /** Header-bound: "FIRING FOR" (64 px) is wider than any duration it labels. */
  firing: "6rem",
  /** Header-bound: "EPISODES" is wider than any count it labels. Wide only. */
  count: "5.5rem",
  /** Header-bound: "LAST SEEN" measures 62 px, so 88 px is the honest floor. */
  seen: "5.5rem",
  /**
   * The `Resume` button, and it is spent out of NOBODY's budget on the main tab
   * because the column does not exist there (see `ORDER`). On the Quiet tab the
   * alert name gives up 88 px, which is the one screen where that is the right
   * trade: every row on it is being held back, and ending the hold is the reason
   * an operator opened the tab.
   */
  resume: "5.5rem",
} as const;

/**
 * The elastic column's `<col>` width.
 *
 * ⛔ `auto`, NOT `100%`. Under `table-layout: fixed` a percentage `<col>` is
 * resolved against the table's own width, so `100%` here asks for the entire
 * table and leaves the fixed tracks to be scaled down proportionally — which is
 * the exact opposite of "fixed anchors, one elastic column". A column with no
 * declared width is the one thing the fixed algorithm hands the *remainder* to,
 * and with exactly one such column in the table it takes all of the slack.
 */
const ELASTIC = "auto";

/**
 * ⭐ THE VIEWPORT AT WHICH THE SECOND-TIER COLUMNS ARE AFFORDABLE AGAIN.
 *
 * `Rule` and `Episodes` are real facts, but at 1280 px they cost 208 px that the
 * alert name needs more: with both of them present the primary column of this
 * screen came out at 275 px, and `Rule` in particular is a raw PromQL expression
 * that is unreadable at any width this table can spare and is already shown in
 * full — unwrapped and copyable — on the alert's own screen. So they are not
 * deleted, they are *deferred*: below this breakpoint they are absent (not
 * blank, which would read as "oto captured nothing"), and above it there is room
 * for them and for a 400 px alert name at the same time.
 *
 * 1536 px is chosen so the narrowest viewport that shows them still clears the
 * 320 px floor on the name with the filter rail deducted: 1232 px of table,
 * 829 px of fixed track, 403 px of name.
 */
const WIDE_QUERY = "(min-width: 1536px)";

type ColumnKey =
  | "severity"
  | "alert"
  | "rule"
  | "state"
  | "cluster"
  | "firing"
  | "count"
  | "seen"
  | "resume";

interface Column {
  readonly key: ColumnKey;
  readonly label: string;
  readonly width: string;
  /**
   * ⭐ THE FLOOR THAT MAKES OVERFLOW HONEST.
   *
   * Under `table-layout: fixed` nothing can widen a column, so the failure mode
   * inverts: instead of the fixed tracks silently eating the elastic one, the
   * elastic one silently shrinks towards zero as the window narrows. These
   * floors are summed into a `min-width` on the **table** (`tableMin` below),
   * which turns that into the visible failure instead — below the sum the
   * scroller scrolls, which a reader can see and act on, rather than the alert
   * name quietly becoming an ellipsis.
   *
   * The floor lives on the table rather than on the header cells because the
   * fixed algorithm reads `width` off `<col>` and the first row and nothing
   * else: a `min-width` on a `<th>` is simply not consulted, so the guard has to
   * be somewhere the algorithm cannot ignore.
   */
  readonly min: string;
  /** Worn by the header cell AND the body cell, so the two cannot disagree. */
  readonly cell: string;
}

/**
 * Liberal spacing (§0.2): `px-md`, not the dense `px-sm` this table used to run.
 *
 * The vertical half is density-aware and lives in `cellY()` below. It cannot be
 * a constant: `py-sm` twice against the compact row's 36 px leaves exactly 20 px
 * for a 20 px `StateChip`, and CSS `height` on a `<tr>` is a *minimum* — so the
 * first cell that overflows by a pixel silently desynchronises every row from
 * the virtualiser's arithmetic.
 */
/**
 * ⛔ `overflow-hidden` IS NOT DECORATION HERE, IT IS THE OTHER HALF OF `fixed`.
 *
 * `table-layout: fixed` stops a cell from *widening* its column; it does not
 * stop the cell's content from painting outside it. Without a clip on every
 * cell the table stays exactly 100 % wide — which is what the scroll check
 * measures — while chips, cluster names and expressions spill across their
 * neighbours, which is a worse lie than the scrollbar was. So the clip is part
 * of `CELL`/`NUMERIC` and every column inherits it rather than remembering it.
 */
const CELL = "px-md text-left align-middle overflow-hidden";
const NUMERIC = "px-md text-right align-middle tabular-nums overflow-hidden whitespace-nowrap";

/** The vertical half of the cell inset, with slack left at both densities. */
const cellY = (): string => (density() === "compact" ? "py-xs" : "py-sm");

/** A body cell's classes: the density inset first, so a column can override it. */
const td = (col: Column): string => cn(cellY(), col.cell);

const COLUMN: Record<ColumnKey, Column> = {
  severity: {
    key: "severity",
    label: "Severity",
    width: TRACK.severity,
    min: "6rem",
    cell: cn(CELL, "min-w-0"),
  },
  // Two lines, and `py-2xs` so both of them fit: the name on top, the
  // namespace/service and summary demoted beneath it. See `AlertRow`.
  // ⭐ The floor is 20rem, and that is the whole point of the re-budget: the
  // primary column of the primary screen is guaranteed 320 px or the table
  // admits it cannot fit. Nothing else here is allowed to grow past it.
  alert: {
    key: "alert",
    label: "Alert",
    width: ELASTIC,
    min: "20rem",
    cell: cn(CELL, "min-w-0 py-2xs"),
  },
  // The differentiator, on the screen it is for. It sits beside the alert name
  // because "what fired" and "what the rule said when it fired" are one thought
  // — but only where both fit at once (`WIDE_QUERY`).
  rule: { key: "rule", label: "Rule", width: TRACK.rule, min: "5rem", cell: cn(CELL, "min-w-0") },
  state: { key: "state", label: "State", width: TRACK.state, min: "7rem", cell: cn(CELL, "min-w-0") },
  cluster: {
    key: "cluster",
    label: "Cluster",
    width: TRACK.cluster,
    min: "5rem",
    cell: cn(CELL, "min-w-0"),
  },
  firing: { key: "firing", label: "Firing for", width: TRACK.firing, min: "5.5rem", cell: NUMERIC },
  count: { key: "count", label: "Episodes", width: TRACK.count, min: "5.5rem", cell: NUMERIC },
  seen: { key: "seen", label: "Last seen", width: TRACK.seen, min: "5.5rem", cell: NUMERIC },
  // The trailing anchor, and it is anchored on purpose: a control that changes
  // column as the window resizes is a control you have to look for.
  resume: {
    key: "resume",
    label: "Resume",
    width: TRACK.resume,
    min: "5rem",
    cell: cn(CELL, "min-w-0 text-right"),
  },
};

/**
 * ⛔ THERE IS NO `actions` TRACK, AND THE 80 px IT USED TO HOLD WENT BACK TO THE
 * ALERT NAME. The trailing anchor existed for a per-row Acknowledge and Snooze:
 * the first is addressed to a **Case** — one firing episode — and not to the
 * Alert this row renders, and the second is a dialog about one alert. An empty
 * 5 rem column under an "Actions" header would be a promise the row cannot keep,
 * so the column is gone rather than blank.
 *
 * ⭐ `resume` IS THE ONE COLUMN THAT COMES BACK, AND ONLY WHERE ITS SUBJECT IS
 * CERTAIN. It is filtered out unless the rows came from the Quiet tab (see
 * `columns()`), where every row is by construction an alert oto is holding — so
 * the button can never be aimed at a guess, and the main tab's budget is
 * untouched.
 */
const ORDER: readonly ColumnKey[] = [
  "severity",
  "alert",
  "rule",
  "state",
  "cluster",
  "firing",
  "count",
  "seen",
  "resume",
];

const COLUMNS: readonly Column[] = ORDER.map((key) => COLUMN[key]);

/** The empty lookup, so a table rendered without rules is not a special case. */
const NO_RULES: ReadonlyMap<string, RuleSnapshot> = new Map();

/** Anything that swallows a keystroke: the shortcuts below must never eat one. */
const TYPING_SELECTOR =
  "input, textarea, select, [contenteditable=''], [contenteditable='true']";

/** Kobalte stamps this on an open trigger and its content; a menu owns the keys. */
const OVERLAY_SELECTOR = "[data-expanded], [role='menu'], [role='dialog']";

function isTypingContext(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest(TYPING_SELECTOR) !== null;
}

export const AlertTable: Component<AlertTableProps> = (props) => {
  /**
   * ⛔ The Rule column exists only when the caller can fill it.
   *
   * A caller that did not ask for `include=rule` has no snapshot ids, and a
   * column of dashes would read as "oto captured no rule" when the truth is
   * "nobody asked". That is the same class of lie as a silently dropped filter,
   * so the column is absent rather than empty.
   */
  const hasRules = (): boolean => props.rules !== undefined;

  /**
   * True once the window is wide enough to afford the second-tier columns.
   *
   * A media *query* rather than a CSS breakpoint class because the column set
   * is data here, not styling: the `<colgroup>`, the header cells, the body
   * cells and the pad rows' `colSpan` all have to agree on how many columns
   * exist, and a `hidden lg:table-cell` on some of them would leave the others
   * counting a column that is not there.
   */
  const [wide, setWide] = createSignal(false);
  if (typeof window !== "undefined" && window.matchMedia) {
    const mql = window.matchMedia(WIDE_QUERY);
    setWide(mql.matches);
    const onChange = (e: MediaQueryListEvent): void => {
      setWide(e.matches);
    };
    mql.addEventListener("change", onChange);
    onCleanup(() => mql.removeEventListener("change", onChange));
  }

  const showRule = (): boolean => hasRules() && wide();
  const showCount = (): boolean => wide();

  /**
   * ⛔ BOTH HALVES, NEVER EITHER ONE. The column is a promise that the row can end
   * this alert's quiet period, and it takes a caller who wired the mutation
   * (`onResume`) AND a tab where every row is certainly snoozed (`quiet`). Gating
   * on `quiet` alone would render a button with nothing behind it; gating on
   * `onResume` alone would offer to wake an alert that was never asleep.
   */
  const showResume = (): boolean => props.quiet === true && props.onResume !== undefined;

  const columns = createMemo(() =>
    COLUMNS.filter((c) => {
      if (c.key === "rule") return showRule();
      if (c.key === "count") return showCount();
      if (c.key === "resume") return showResume();
      return true;
    }),
  );

  /**
   * The sum of every rendered column's floor, as a `calc()` so the rem units
   * stay rem units and follow the root font size rather than being frozen into
   * a pixel number at build time. See `Column.min`.
   */
  const tableMin = createMemo(
    () => `calc(${[TRACK.gutter, ...columns().map((c) => c.min)].join(" + ")})`,
  );

  /**
   * ⭐ THE CURSOR IS AN ALERT ID, NOT A ROW NUMBER (§0.5).
   *
   * This list is fed by SSE. Rows are inserted above the viewport while the
   * operator is reading, and every one of them shifts each existing row's
   * index by one. A `focusIndex` signal would therefore keep pointing at
   * "row 7" while row 7 became a different alert underneath it — and the next
   * `Enter` would open, and the next ack would acknowledge, whatever had
   * slid into that slot.
   *
   * Holding the id and deriving the position means an insert above the cursor
   * moves the *number* and leaves the *selection* exactly where the operator
   * put it. `-1` means "nothing focused", which is also what an id that has
   * left the list resolves to — a filter that removes the focused alert parks
   * the cursor rather than silently re-pointing it at a neighbour.
   */
  const [focusId, setFocusId] = createSignal<string | null>(null);
  const focusIndex = createMemo(() => {
    const id = focusId();
    if (id === null) return -1;
    return props.alerts.findIndex((a) => a.id === id);
  });

  /**
   * ⛔ THE ROW HEIGHT FOLLOWS THE DENSITY SWITCH, OR THE VIRTUALISER LIES.
   *
   * This used to be read once, inside the scroller's `ref` callback. Nothing
   * tracked `density`, so switching Display → Compact over 150 alerts left this
   * at 48 while the CSS rows were 36: `padTop`/`padBottom` came out 33 % too
   * tall, the visible slice was chosen from the wrong offset, and the list
   * showed blank bands and jumped to the wrong row on `j`/`k`.
   *
   * `readRowHeight()` is still the single source — this only decides *when* to
   * ask it. The second read on the next frame is for the first run, where the
   * stylesheet (or `installThemeEffect`'s `data-density`) may not have landed
   * yet; both reads go through the same function, so there is no second copy of
   * the number to drift.
   */
  const [rowHeight, setRowHeight] = createSignal(readRowHeight());
  createEffect(
    on(density, () => {
      setRowHeight(readRowHeight());
      requestAnimationFrame(() => setRowHeight(readRowHeight()));
    }),
  );

  /** Names the scroll region below with the caption the table already carries. */
  const captionId = createUniqueId();

  let scroller: HTMLDivElement | undefined;

  const virt = createVirtualiser({
    count: () => props.alerts.length,
    rowHeight,
  });

  const win = createMemo(() => virt.window());

  /**
   * How much of the scrollport the pinned header is sitting on top of.
   *
   * ⛔ MEASURED WHEN IT CHANGES, NEVER PER KEYSTROKE. `getBoundingClientRect()`
   * forces a synchronous layout, and this is read inside `reveal()` — so every
   * single `j`/`k` used to flush layout on a table with a thousand rows in it.
   * A `ResizeObserver` costs that once, when the header's height actually
   * changes, which is the only time the answer is different.
   */
  let headerHeight = 0;
  const measureHead = (el: HTMLTableSectionElement): void => {
    headerHeight = el.getBoundingClientRect().height;
    const ro = new ResizeObserver(() => {
      headerHeight = el.getBoundingClientRect().height;
    });
    ro.observe(el);
    onCleanup(() => ro.disconnect());
  };

  /**
   * Scroll a row into view and hand it DOM focus.
   *
   * The row may not be mounted yet when virtualised, so the container is
   * scrolled by arithmetic rather than by asking an element that does not
   * exist to scroll itself — `--oto-row-h` (read once into `rowHeight`, and
   * re-read when density changes) is what makes that arithmetic exact.
   *
   * The sticky `<thead>` is subtracted on the way up: without it, `k` at the
   * top of the viewport parks the row *underneath* the header, which reads as
   * the keystroke having done nothing.
   */
  const reveal = (index: number): void => {
    const el = scroller;
    if (!el) return;
    const h = rowHeight();
    const inset = headerHeight;
    const top = inset + index * h;
    if (top - inset < el.scrollTop) el.scrollTop = top - inset;
    else if (top + h > el.scrollTop + el.clientHeight) {
      el.scrollTop = top + h - el.clientHeight;
    }
    /*
     * ⛔ PUSH THE VIRTUALISER *BEFORE* REACHING FOR THE ROW.
     *
     * The virtualiser's window only moves when its `scroll` listener runs, and
     * the browser dispatches that as a task — later than a microtask. `End` on
     * a 200-row list therefore set `scrollTop`, then looked for
     * `[data-row-index="199"]` in a `queueMicrotask` while the DOM still held
     * rows 0…40, found nothing, and dropped keyboard focus on the floor.
     *
     * Dispatching the event ourselves runs that same listener synchronously —
     * Solid's update is synchronous too, so the row exists by the next line.
     * The browser's own event still arrives afterwards and is a no-op: the
     * signal is already at `el.scrollTop`.
     */
    el.dispatchEvent(new Event("scroll"));
    el.querySelector<HTMLElement>(`[data-row-index="${index}"] a`)?.focus();
  };

  const focusAt = (index: number): void => {
    const target = props.alerts[index];
    if (target === undefined) return;
    setFocusId(target.id);
    reveal(index);
  };

  const move = (delta: number): void => {
    const total = props.alerts.length;
    if (total === 0) return;
    const from = focusIndex();
    focusAt(Math.min(total - 1, Math.max(0, (from < 0 ? -1 : from) + delta)));
  };

  const onKeyDown = (e: KeyboardEvent): void => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    // Never take a key from a text field or from an open menu. The label menu
    // below lives inside a row, so its typeahead would otherwise reach here.
    if (isTypingContext(e.target) || isTypingContext(document.activeElement)) return;
    // ⛔ SCOPED TO THIS SCROLLER, NEVER THE DOCUMENT. Asked document-wide, this
    // said "a menu is open" for *any* open Kobalte overlay on the screen — the
    // filter rail's `Select` included — so opening a filter silently switched
    // `j`/`k`/`Home`/`End` off until it was closed again. The only overlay that
    // legitimately owns these keys is one opened from a row, and a row's
    // trigger carries `data-expanded` inside this element.
    if ((scroller?.querySelector(OVERLAY_SELECTOR) ?? null) !== null) return;

    if (e.key === "j" || e.key === "ArrowDown") {
      e.preventDefault();
      move(1);
    } else if (e.key === "k" || e.key === "ArrowUp") {
      e.preventDefault();
      move(-1);
    } else if (e.key === "Home") {
      e.preventDefault();
      focusAt(0);
    } else if (e.key === "End") {
      e.preventDefault();
      focusAt(props.alerts.length - 1);
    }
  };

  return (
    <div
      ref={(el) => {
        scroller = el;
        virt.attach(el);
      }}
      class="min-h-0 flex-1 overflow-auto"
      // ⛔ THE KEYBOARD CONTRACT NEEDS SOMETHING THAT CAN HOLD FOCUS, and the
      // caption below was promising one that did not exist. `onKeyDown` is bound
      // here, but a scrollable `<div>` is not focusable in Chrome or Safari, and
      // the global handler on the alerts route binds only `/` and `f`. So `j`,
      // `k`, the arrows and `Home` did nothing at all until the operator had
      // already Tabbed into a row link — which is the person who least needs the
      // shortcut. `tabindex` is also what makes this scroller reachable at all:
      // a scroll container that cannot take focus cannot be scrolled from the
      // keyboard (WCAG 2.1.1), and on this screen everything scrolls in here.
      //
      // It adds one tab stop between the filter panel and the first row, and that
      // stop is the point rather than the cost: `role="region"` named by the
      // caption means landing here announces "Alerts, newest activity first. Use
      // j and k or the arrow keys…", so the shortcut is *told to* the keyboard
      // user instead of being described to a mouse user. Nothing traps focus —
      // the region is a plain element in source order, Tab leaves it for the
      // first row link, and `move()` hands focus to the rows on the first `j`.
      tabindex="0"
      role="region"
      aria-labelledby={captionId}
      onKeyDown={onKeyDown}
    >
      {/*
        ⭐ `table-layout: fixed` IS WHAT MAKES `TRACK` REAL.

        Everything in this file's column model — the `<colgroup>`, the budget,
        "one elastic column between fixed anchors" — was written against a table
        that laid out `auto`, and under `auto` the browser sizes each column to
        its content and treats the declared widths as a suggestion. Measured at
        1280 px it discarded them wholesale: 1774 px of table inside a 1008 px
        scroller, `Rule` at 448 px against 120 declared, and `State` onwards off
        the right-hand edge. With `fixed`, the `<col>` lengths *are* the layout,
        the one undeclared column absorbs the remainder, and a cell that does not
        fit clips (every one of them carries `overflow-hidden`) instead of
        pushing its neighbours into the horizontal scroll.

        `min-width` is the escape hatch that keeps the failure honest at genuinely
        small windows: below the sum of the floors the scroller scrolls, visibly,
        rather than the alert name being ground down to nothing in silence.
      */}
      <table
        class="w-full border-collapse text-item"
        style={{ "table-layout": "fixed", "min-width": tableMin() }}
      >
        <caption id={captionId} class="sr-only-focusable">
          Alerts, newest activity first. Use j and k or the arrow keys to move between rows, Home
          and End to jump to either end, Enter to open one. New alerts arriving above the cursor
          never move the selection.
        </caption>

        {/* ⭐ The single declaration of every column's width. A `<col>` sizes the
            header cell and every body cell in that column at once, which is the
            only arrangement in which the two literally cannot drift. */}
        <colgroup>
          <col style={{ width: TRACK.gutter }} />
          <For each={columns()}>{(col) => <col style={{ width: col.width }} />}</For>
        </colgroup>

        <thead ref={measureHead} class="sticky top-0 z-10">
          <tr class="bg-raised">
            {/* The gutter has no header text; it repeats the state column, which
                does, and carries the focus rail, which is not data. */}
            <th class="p-0" aria-hidden="true" />
            <For each={columns()}>
              {(col) => (
                <th
                  scope="col"
                  // No `min-width` here: the fixed algorithm does not consult it,
                  // and the floors are summed onto the table instead (§ `Column.min`).
                  class={cn(
                    col.cell,
                    cellY(),
                    "whitespace-nowrap text-ellipsis border-b border-line",
                    SECTION_LABEL,
                    "text-ink-muted",
                  )}
                >
                  {col.label}
                </th>
              )}
            </For>
          </tr>
        </thead>

        <tbody>
          <Show when={win().padTop > 0}>
            <tr aria-hidden="true" style={{ height: `${win().padTop}px` }}>
              <td colSpan={columns().length + 1} class="p-0" />
            </tr>
          </Show>

          <For each={props.alerts.slice(win().start, win().end)}>
            {(alert, i) => (
              <AlertRow
                alert={alert}
                index={win().start + i()}
                focused={alert.id === focusId()}
                snoozed={props.quiet === true}
                showRule={showRule()}
                showCount={showCount()}
                showResume={showResume()}
                resuming={props.resuming?.has(alert.id) === true}
                rules={props.rules ?? NO_RULES}
                rulesPending={props.rulesPending === true}
                onFocus={() => setFocusId(alert.id)}
                onFilterLabel={props.onFilterLabel}
                onResume={props.onResume}
              />
            )}
          </For>

          <Show when={win().padBottom > 0}>
            <tr aria-hidden="true" style={{ height: `${win().padBottom}px` }}>
              <td colSpan={columns().length + 1} class="p-0" />
            </tr>
          </Show>
        </tbody>
      </table>

      {props.footer}
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* One row                                                                    */
/* -------------------------------------------------------------------------- */

interface AlertRowProps {
  readonly alert: Alert;
  readonly index: number;
  readonly focused: boolean;
  /** Certainly snoozed, because the row came from the Quiet tab. */
  readonly snoozed: boolean;
  /**
   * False when the caller cannot fill the column, or when the window is too
   * narrow to spend 120 px on a PromQL expression, so it is not drawn at all.
   */
  readonly showRule: boolean;
  /** False below `WIDE_QUERY`, where the alert name needs the 88 px more. */
  readonly showCount: boolean;
  /** True on the Quiet tab, where the row may end this alert's quiet period. */
  readonly showResume: boolean;
  /** This alert's own resume is in flight. */
  readonly resuming: boolean;
  readonly rules: ReadonlyMap<string, RuleSnapshot>;
  readonly rulesPending: boolean;
  readonly onFocus: () => void;
  readonly onFilterLabel: (name: string, value: string) => void;
  /** Plural for the same reason the table's prop is — see `AlertTableProps`. */
  readonly onResume?: ((alertIds: readonly string[]) => void) | undefined;
}

const AlertRow: Component<AlertRowProps> = (props) => {
  const ac = (): Alert["current_case"] => props.alert.current_case ?? null;

  /**
   * U4: the only urgency motion in the product, and it is spent on exactly one
   * thing — a critical nobody has acknowledged yet. Everything else is still.
   */
  const urgent = (): boolean =>
    props.alert.state === "firing" &&
    (ac()?.ack_state ?? "unacked") === "unacked" &&
    normaliseSeverity(props.alert.severity) === "critical";

  /** A few labels beyond the promoted ones, for orientation and drill-down. */
  const extraLabels = createMemo(() => {
    const promoted = new Set(["alertname", "severity", "namespace", "service", "cluster"]);
    return Object.entries(props.alert.labels)
      .filter(([k]) => !promoted.has(k))
      .sort(([a], [b]) => a.localeCompare(b))
      .slice(0, 6);
  });

  const summary = (): string | null => {
    const a = props.alert.annotations;
    return a["summary"] ?? a["description"] ?? null;
  };

  /** `namespace/service`, whichever of the two the alert actually carries. */
  const place = (): string | null => {
    const ns = props.alert.namespace ?? "";
    const svc = props.alert.service ?? "";
    if (ns === "" && svc === "") return null;
    return ns !== "" && svc !== "" ? `${ns}/${svc}` : `${ns}${svc}`;
  };

  /**
   * Everything the second line says, also said to a hover and to a tooltip.
   *
   * Compact density drops that line — two lines do not fit in 36 px and a
   * half-clipped sentence is worse than an absent one — so this is the only
   * place the supporting facts survive at that density.
   */
  const nameTitle = (): string =>
    [props.alert.alertname, place(), summary()].filter((s) => s !== null).join("\n");

  const firing = (): boolean => props.alert.state === "firing";

  return (
    <tr
      data-row-index={props.index}
      onFocusIn={props.onFocus}
      class={cn(
        // A quarter-strength hairline, not a full rule: enough to separate two
        // quiet rows, not enough to read as a spreadsheet grid. The row's own
        // background tint (below) and the extra height carry the rest of the
        // separation — hierarchy from tone and space, never from heavy rules.
        "group border-b border-line/50",
        // The row tint is the state's pastel fill — Tier A brightness, Tier B
        // meaning. `firing` gets the tint; a resolved row stays neutral so the
        // live ones are what your eye lands on.
        //
        // ⭐ THE ALPHA IS THE DECISION, NOT THE TOKEN (§0.6). At `/40` a firing
        // row sat *under* the filter rail's `bg-accent-fill` selection: the
        // chrome was reading louder than the signal, which is precisely the
        // inversion §0.6 forbids. `/70` puts it back on top. `GroupedAlerts`
        // was separately on `/30`; both views now say the same thing at the
        // same strength. Raising `--oto-state-firing-fill` itself would have
        // been the wrong lever — that is a SPEC amendment plus a contrast-table
        // update, for a decision that belongs to this one call site.
        firing() ? "bg-firing-fill/70 hover:bg-firing-fill/85" : "bg-surface hover:bg-raised",
        // ⛔ FOCUS IS THE RAIL, NOT A WASH OF ACCENT (§0.6). The row used to go
        // `bg-accent-fill` when focused, which put brand colour on the single
        // widest object on the screen and left the severity glyph competing
        // with it. The rail says the same thing in 2 px, and a focused firing
        // row keeps its firing tint instead of trading it for the accent.
        props.focused && (firing() ? "bg-firing-fill/85" : "bg-raised"),
      )}
      style={{ height: "var(--oto-row-h)" }}
    >
      {/*
        The gutter: focus rail then state bar (§M.2).

        The rail is a *recolour*, never an addition — it occupies its 2 px on
        every row and only changes colour — so focusing a row cannot nudge its
        contents sideways. The 3 px state bar beside it is saturated, and with
        the accent now confined to the rail those two bars and the severity
        glyph are the only saturated things the row contains.
      */}
      <td class="p-0">
        <div class="flex h-full" aria-hidden="true">
          <div class={cn("w-0.5 shrink-0", props.focused ? "bg-accent" : "bg-transparent")} />
          <div class={cn("w-[3px] shrink-0", STATE_BAR[props.alert.state])} />
        </div>
      </td>

      <td class={td(COLUMN.severity)}>
        <SeverityMark severity={props.alert.severity} withLabel />
      </td>

      {/*
        ⭐ TWO LINES, BECAUSE THE ROW IS 48 px AND WAS SPENDING NONE OF IT.

        The name, the namespace/service and the summary used to share ONE
        baseline-aligned line inside a row with ~12 px of vertical slack going
        spare — three competing claims on the single column that has no width to
        give, in a table whose fixed tracks had already spent the viewport. The
        supporting facts move down onto the empty line instead: they cost the
        name nothing horizontally, they finally justify the taller row, and the
        hierarchy is now legible (name first, context beneath) rather than
        implied by reading order alone.
      */}
      <td class={td(COLUMN.alert)}>
        <div class="min-w-0 overflow-hidden">
          <div class="flex min-w-0 items-center gap-sm overflow-hidden">
            {/* The one elastic, truncating column. The name shrinks and ellipses
                rather than widening the table, which is what makes every fixed
                column to its right a genuine anchor. */}
            <A
              href={`/alerts/${props.alert.id}`}
              class="min-w-0 truncate font-medium leading-4 text-ink hover:underline"
              title={nameTitle()}
            >
              {props.alert.alertname}
            </A>
            <Show when={extraLabels().length > 0}>
              <LabelFilterMenu
                alertname={props.alert.alertname}
                labels={extraLabels()}
                onFilterLabel={props.onFilterLabel}
              />
            </Show>
          </div>

          {/* Compact density is one line per alert, by definition: 36 px cannot
              hold two and the facts below stay one hover away in `nameTitle()`. */}
          <Show when={density() !== "compact" && (place() !== null || summary() !== null)}>
            {/* ⛔ BOTH LINES TRUNCATE, NOT JUST THE NAME. `place` used to be
                `shrink-0`, which under the old auto layout let it widen the
                column and under fixed layout would simply paint over `State`.
                Both spans are now shrinkable and both clip, so flexbox takes the
                room out of whichever of the two is actually long — and the
                overflow-hidden on the wrapper is the backstop for the case where
                even the shrunken pair does not fit. */}
            <div class="flex min-w-0 items-baseline gap-xs overflow-hidden text-meta leading-4 text-ink-subtle">
              <Show when={place()}>
                {(p) => <span class="min-w-0 truncate font-mono">{p()}</span>}
              </Show>
              <Show when={summary()}>
                {(text) => (
                  <span class="min-w-0 truncate" title={text()}>
                    {truncate(text(), 160)}
                  </span>
                )}
              </Show>
            </div>
          </Show>
        </div>
      </td>

      {/* **What the rule said at that moment** — the first promise in the
          README, on the screen it was missing from. */}
      <Show when={props.showRule}>
        <td class={td(COLUMN.rule)}>
          <RuleCell
            snapshotId={props.alert.rule?.id ?? null}
            snapshot={props.alert.rule ? (props.rules.get(props.alert.rule.id) ?? null) : null}
            pending={props.rulesPending}
          />
        </td>
      </Show>

      <td class={td(COLUMN.state)}>
        {/* `overflow-hidden` so the rarer third and fourth chip clip at the
            column's edge instead of widening the track — under `table-layout:
            auto` every pixel they took came out of the alert name. */}
        {/* ⛔ NO `AckChip` HERE, AND ITS ABSENCE IS THE SAME DECISION AS THE
            MISSING ACKNOWLEDGE BUTTON. `acked` is not a state an Alert can be
            in: a receipt is written on ONE firing, and an identity outlives its
            firings — so an "Acked" chip on this row claimed a property of the
            identity from a fact about whichever episode happened to be open. The
            receipt is shown where it was written, on `/cases` and `/cases/:id`.
            The unacked-critical pulse below is a different thing: it reads the
            open episode to decide how loudly to say `firing`, and it never
            reports "acknowledged" as something the alert is. */}
        <div class="flex min-w-0 items-center gap-2xs overflow-hidden">
          <StateChip state={props.alert.state} size="sm" urgent={urgent()} />
          <Show when={props.alert.is_flapping}>
            <FlappingChip />
          </Show>
          {/* Tier A, and beside the state chip rather than instead of it: the
              row keeps its firing tint and its true severity. Snoozing holds
              oto's notifications, it does not make the alert less serious. */}
          <Show when={props.snoozed}>
            <SnoozeChipUnknownUntil />
          </Show>
        </div>
      </td>

      <td class={td(COLUMN.cluster)}>
        <Chip mono title={`Cluster ${props.alert.cluster_key}`}>
          <span class="min-w-0 truncate">{truncate(props.alert.cluster_key, 11)}</span>
        </Chip>
      </td>

      {/* "Firing duration", never MTTR — oto measures the signal, not anyone's
          response (SCOPE-BOUNDARY). */}
      <td class={cn(td(COLUMN.firing), "text-ink-muted")}>
        <Show when={ac()} fallback={<span class="text-ink-subtle">—</span>}>
          {(o) => <Elapsed from={o().started_at} to={o().ended_at ?? null} />}
        </Show>
      </td>

      <Show when={props.showCount}>
        <td class={cn(td(COLUMN.count), "text-ink-muted")}>
          <span title={`${props.alert.total_cases} cases — firing episodes — since first seen`}>
            {fmtCount(props.alert.total_cases)}
          </span>
        </td>
      </Show>

      <td class={cn(td(COLUMN.seen), "text-ink-muted")}>
        <RelativeTime value={props.alert.last_seen_at} label="Last seen" />
      </td>

      {/* ⛔ VISIBLE ON EVERY ROW OF THIS TAB, ON EVERY PAINT, WITH OR WITHOUT A
          POINTER (§0.4) — the same rule the label menu above obeys. A wake
          control that materialises under the cursor, on a list that re-sorts as
          snoozes expire, is a 3am misclick generator, and what it would misclick
          is a channel starting to notify again. */}
      <Show when={props.showResume}>
        <td class={td(COLUMN.resume)}>
          <Button
            variant="secondary"
            size="sm"
            busy={props.resuming}
            // ⛔ THE ROW HANDS ITS ID TO THE CALLER AND MINTS NOTHING. The
            // idempotency key belongs to the gesture, and the gesture is the
            // route's mutation — a key minted here would be re-minted on every
            // re-render of a virtualised row.
            onClick={() => props.onResume?.([props.alert.id])}
            aria-label={`Resume notifications for ${props.alert.alertname}`}
            title="Ends the quiet period on this alert now. oto starts notifying about it again, and the wake announces itself in the channel."
          >
            Resume
          </Button>
        </td>
      </Show>
    </tr>
  );
};

/* -------------------------------------------------------------------------- */
/* Drill down by label                                                        */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ AN EXPLICIT MENU, BECAUSE HOVER-REVEALED ROW CONTROLS ARE A 3AM HAZARD.
 *
 * These label chips used to be `hidden … group-hover:inline-flex`: buttons that
 * materialised under the pointer, on a list that re-sorts itself as alerts fire
 * and resolve. Whatever the button did, it was one insert away from doing it to
 * the wrong alert, and a keyboard user could not see it at all until they had
 * already tabbed into the row. The `Resume` control on the Quiet tab is held to
 * the same rule for the same reason.
 *
 * So the trigger is *always* there, at a constant size and a constant place,
 * and the labels live behind it. One tab stop per row instead of up to six, no
 * reflow when the pointer crosses a row, and nothing in the row moves or
 * appears in response to the cursor.
 */
const LabelFilterMenu: Component<{
  readonly alertname: string;
  readonly labels: readonly (readonly [string, string])[];
  readonly onFilterLabel: (name: string, value: string) => void;
}> = (props) => (
  <DropdownMenu placement="bottom-start">
    <DropdownMenuTrigger
      class={cn(
        "inline-flex shrink-0 items-center gap-2xs rounded-chip border border-line bg-raised",
        "px-2xs py-px font-mono text-micro leading-4 text-ink-subtle",
        // Tier A hover only: a neutral lift, never the accent (§0.6).
        "hover:border-line-strong hover:text-ink",
      )}
      title="Filter this list by one of this alert's labels"
      aria-label={`Filter by a label on ${props.alertname}`}
    >
      <TagGlyph />
      {props.labels.length}
    </DropdownMenuTrigger>

    <DropdownMenuContent class="max-w-96">
      <DropdownMenuLabel class="text-meta font-normal text-ink-subtle">
        Filter by label
      </DropdownMenuLabel>
      <For each={props.labels}>
        {([name, value]) => (
          <DropdownMenuItem
            class="font-mono text-meta"
            onSelect={() => props.onFilterLabel(name, value)}
          >
            <span class="truncate">
              {name}={value}
            </span>
          </DropdownMenuItem>
        )}
      </For>
    </DropdownMenuContent>
  </DropdownMenu>
);

const TagGlyph: Component = () => (
  <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
    <path
      d="M1.6 1.6h3.6L10.4 6.8 6.8 10.4 1.6 5.2z"
      fill="none"
      stroke="currentColor"
      stroke-width="1.2"
      stroke-linejoin="round"
    />
    <circle cx="3.6" cy="3.6" r="0.8" fill="currentColor" />
  </svg>
);

/* -------------------------------------------------------------------------- */
/* The rule cell                                                              */
/* -------------------------------------------------------------------------- */

interface RuleCellProps {
  /** The snapshot bound to this alert's current episode, or null if none was. */
  readonly snapshotId: string | null;
  /** The resolved snapshot, or null while it is unresolved. */
  readonly snapshot: RuleSnapshot | null;
  /** True while the page's batch is in flight. */
  readonly pending: boolean;
}

/**
 * `expr` on the row — Tier A chrome throughout (§M): mono, muted, no state hue.
 *
 * The point of this cell is that **four different silences look different**, and
 * that is the whole design:
 *
 *   - *no snapshot id*        — nothing was ever captured for this episode;
 *   - *not resolved yet*      — the batch is in flight, so the cell says so;
 *   - *resolved to nothing*   — we hold an id whose snapshot we could not read;
 *   - *captured as empty*     — a snapshot exists and honestly records that the
 *     definition could not be recovered (`origin: unavailable`, ADR 0009).
 *
 * Rendering all four as an empty cell would be the "oto's silence is
 * indistinguishable from no alert" failure, in miniature. The expression itself
 * is never reformatted, wrapped or prettified: it is the text that fired, and it
 * is copyable in full from the tooltip.
 */
const RuleCell: Component<RuleCellProps> = (props) => {
  const tooltip = (s: RuleSnapshot): string => {
    const parts = [s.expr === "" ? "(no expression captured)" : s.expr];
    if (s.for_seconds > 0) parts.push(`for: ${fmtDuration(s.for_seconds)}`);
    if (s.match_confidence === "ambiguous") {
      // ADR 0009: an ambiguous match is surfaced, never silently resolved.
      parts.push("match: ambiguous — more than one rule fits this alert");
    }
    return parts.join("\n");
  };

  return (
    <Switch>
      <Match when={props.snapshotId === null}>
        <span
          class="text-ink-subtle"
          title="No rule definition was captured for this case."
        >
          —
        </span>
      </Match>

      <Match when={props.snapshot === null && props.pending}>
        <span class="text-ink-subtle" aria-label="Loading the rule">
          ·&#8202;·&#8202;·
        </span>
      </Match>

      <Match when={props.snapshot === null}>
        <span
          class="text-ink-subtle"
          title="oto holds a snapshot id for this episode but could not read the definition back."
        >
          unreadable
        </span>
      </Match>

      <Match when={props.snapshot !== null && props.snapshot.expr === ""}>
        {/* A snapshot that honestly records "we could not see the rule" is a
            different fact from having no snapshot at all, and it says so. */}
        <span
          class="text-ink-subtle italic"
          title="The capture is recorded as unavailable. oto never fabricates a rule it could not read."
        >
          unavailable
        </span>
      </Match>

      <Match when={props.snapshot}>
        {(snapshot) => (
          <code
            class="block truncate font-mono text-meta leading-4 text-ink-muted"
            title={tooltip(snapshot())}
          >
            {truncate(snapshot().expr, 64)}
          </code>
        )}
      </Match>
    </Switch>
  );
};
