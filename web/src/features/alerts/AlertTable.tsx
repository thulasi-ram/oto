/**
 * The alert table. The screen oto is judged on.
 *
 * The design brief is one sentence: **a tired on-call engineer at 3am must be
 * able to scan it.** Everything here follows from that.
 *
 *   - It is a real `<table>`. Screen readers, in-page find and "copy the row"
 *     all work for free, and none of them work in a div soup.
 *   - Row height is `--oto-row-h` exactly (U6), which is what makes the
 *     virtualiser exact and what stops the table reflowing when data lands.
 *   - The 3 px left bar is the state (§M.2): pastel fill, saturated bar. Calm
 *     at a distance, unmistakable at a glance.
 *   - Severity is the glyph, state is the colour (U8), and both always have a
 *     word next to them (U1).
 *   - Keyboard first: `j`/`k` or arrows move, `Enter` opens, and the focused
 *     row is scrolled into view rather than left behind.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createMemo,
  createSignal,
  createUniqueId,
  type Component,
  type JSX,
} from "solid-js";
import { A } from "@solidjs/router";

import type { Alert, RuleSnapshot } from "~/api/types";
import { RelativeTime, Elapsed } from "~/components/Time";
import {
  AckChip,
  FlappingChip,
  SeverityMark,
  STATE_BAR,
  StateChip,
  normaliseSeverity,
} from "~/components/StateChip";
import { SnoozeChipUnknownUntil } from "~/components/SnoozeChip";
import { Chip, cx } from "~/components/ui/primitives";
import { count as fmtCount, duration as fmtDuration, truncate } from "~/lib/format";
import { createVirtualiser, readRowHeight } from "~/lib/virtual";

export interface AlertTableProps {
  readonly alerts: readonly Alert[];
  /**
   * The `?snoozed=` filter the rows were fetched under, when there was one.
   *
   * `AlertDTO` carries no `snooze` field, so a row's snooze state is only
   * knowable when the query pinned it. `true` means every row here is certainly
   * snoozed; `null` means unknown, and an unknown is left unsaid rather than
   * guessed at.
   */
  readonly snoozedKnown?: boolean | null;
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
  /** Clicking a label chip filters by it — the fastest drill-down there is. */
  readonly onFilterLabel: (name: string, value: string) => void;
  /** Rendered after the last row: the "load more" affordance or a total. */
  readonly footer?: JSX.Element;
}

/** The columns, so the header and the rows can never drift apart. */
const COLUMNS = [
  { key: "severity", label: "Severity", class: "w-[7.5rem]" },
  { key: "alert", label: "Alert", class: "" },
  // The differentiator, on the screen it is for. It sits beside the alert name
  // because "what fired" and "what the rule said when it fired" are one thought.
  { key: "rule", label: "Rule", class: "w-[15rem]" },
  { key: "state", label: "State", class: "w-[11rem]" },
  { key: "cluster", label: "Cluster", class: "w-[9rem]" },
  { key: "firing", label: "Firing for", class: "w-[6rem] text-right" },
  { key: "count", label: "Episodes", class: "w-[5.5rem] text-right" },
  { key: "seen", label: "Last seen", class: "w-[6rem] text-right" },
] as const;

/** The empty lookup, so a table rendered without rules is not a special case. */
const NO_RULES: ReadonlyMap<string, RuleSnapshot> = new Map();

export const AlertTable: Component<AlertTableProps> = (props) => {
  /**
   * ⛔ The Rule column exists only when the caller can fill it.
   *
   * A caller that did not ask for `include=rule` has no snapshot ids, and a
   * column of dashes would read as "oto captured no rule" when the truth is
   * "nobody asked". That is the same class of lie as a silently dropped filter,
   * so the column is absent rather than empty.
   */
  const showRule = (): boolean => props.rules !== undefined;
  const columns = createMemo(() =>
    showRule() ? COLUMNS : COLUMNS.filter((c) => c.key !== "rule"),
  );

  const [focusIndex, setFocusIndex] = createSignal(-1);
  const [rowHeight, setRowHeight] = createSignal(readRowHeight());

  /** Names the scroll region below with the caption the table already carries. */
  const captionId = createUniqueId();

  let scroller: HTMLDivElement | undefined;

  const virt = createVirtualiser({
    count: () => props.alerts.length,
    rowHeight,
  });

  const win = createMemo(() => virt.window());

  const move = (delta: number): void => {
    const next = Math.min(
      props.alerts.length - 1,
      Math.max(0, (focusIndex() < 0 ? -1 : focusIndex()) + delta),
    );
    setFocusIndex(next);
    // The row may not be mounted yet when virtualised, so scroll the container
    // by arithmetic rather than relying on `scrollIntoView` finding an element.
    const el = scroller;
    if (!el) return;
    const top = next * rowHeight();
    if (top < el.scrollTop) el.scrollTop = top;
    else if (top + rowHeight() > el.scrollTop + el.clientHeight) {
      el.scrollTop = top + rowHeight() - el.clientHeight;
    }
    queueMicrotask(() => {
      el.querySelector<HTMLElement>(`[data-row-index="${next}"] a`)?.focus();
    });
  };

  const onKeyDown = (e: KeyboardEvent): void => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "j" || e.key === "ArrowDown") {
      e.preventDefault();
      move(1);
    } else if (e.key === "k" || e.key === "ArrowUp") {
      e.preventDefault();
      move(-1);
    } else if (e.key === "Home") {
      e.preventDefault();
      setFocusIndex(-1);
      move(1);
    }
  };

  return (
    <div
      ref={(el) => {
        scroller = el;
        virt.attach(el);
        // Density can change at runtime; re-read on the next frame so the
        // virtualiser's arithmetic follows the CSS rather than a stale copy.
        requestAnimationFrame(() => setRowHeight(readRowHeight()));
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
      // It adds one tab stop between the filter bar and the first row, and that
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
      <table class="w-full border-collapse text-[13px]">
        <caption id={captionId} class="sr-only-focusable">
          Alerts, newest activity first. Use j and k or the arrow keys to move between rows, Enter
          to open one.
        </caption>
        <thead class="sticky top-0 z-10">
          <tr class="bg-raised">
            {/* The status-bar gutter has no header text; it repeats the state
                column, which does. */}
            <th class="w-[3px] p-0" aria-hidden="true" />
            <For each={columns()}>
              {(col) => (
                <th
                  scope="col"
                  class={cx(
                    "border-b border-line px-2 py-1.5 text-left align-middle",
                    "text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted",
                    col.class,
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
                focused={win().start + i() === focusIndex()}
                snoozed={props.snoozedKnown === true}
                showRule={showRule()}
                rules={props.rules ?? NO_RULES}
                rulesPending={props.rulesPending === true}
                onFocus={() => setFocusIndex(win().start + i())}
                onFilterLabel={props.onFilterLabel}
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
  /** Certainly snoozed, because the query pinned `?snoozed=true`. */
  readonly snoozed: boolean;
  /** False when the caller cannot fill the column, so it is not drawn at all. */
  readonly showRule: boolean;
  readonly rules: ReadonlyMap<string, RuleSnapshot>;
  readonly rulesPending: boolean;
  readonly onFocus: () => void;
  readonly onFilterLabel: (name: string, value: string) => void;
}

const AlertRow: Component<AlertRowProps> = (props) => {
  const occ = (): Alert["current_occurrence"] => props.alert.current_occurrence ?? null;

  /**
   * U4: the only urgency motion in the product, and it is spent on exactly one
   * thing — a critical nobody has acknowledged yet. Everything else is still.
   */
  const urgent = (): boolean =>
    props.alert.state === "firing" &&
    props.alert.ack_state === "unacked" &&
    normaliseSeverity(props.alert.severity) === "critical";

  /** Two or three labels beyond the promoted ones, for orientation. */
  const extraLabels = createMemo(() => {
    const promoted = new Set(["alertname", "severity", "namespace", "service", "cluster"]);
    return Object.entries(props.alert.labels)
      .filter(([k]) => !promoted.has(k))
      .sort(([a], [b]) => a.localeCompare(b))
      .slice(0, 3);
  });

  const summary = (): string | null => {
    const a = props.alert.annotations;
    return a["summary"] ?? a["description"] ?? null;
  };

  return (
    <tr
      data-row-index={props.index}
      onFocusIn={props.onFocus}
      class={cx(
        "group border-b border-line",
        // The row tint is the state's pastel fill — Tier A brightness, Tier B
        // meaning. `firing` gets the tint; a resolved row stays neutral so the
        // live ones are what your eye lands on.
        props.alert.state === "firing" ? "bg-firing-fill/40" : "bg-surface",
        props.focused ? "bg-accent-fill" : "hover:bg-raised",
      )}
      style={{ height: "var(--oto-row-h)" }}
    >
      {/* The 3 px status bar (§M.2). Saturated, and the only saturated thing in
          the row's chrome. */}
      <td class="w-[3px] p-0">
        <div class={cx("h-full w-[3px]", STATE_BAR[props.alert.state])} aria-hidden="true" />
      </td>

      <td class="px-2 align-middle">
        <SeverityMark severity={props.alert.severity} withLabel />
      </td>

      <td class="min-w-0 px-2 align-middle">
        <div class="flex min-w-0 items-baseline gap-2">
          <A
            href={`/alerts/${props.alert.id}`}
            class="shrink-0 truncate font-medium text-ink hover:text-accent hover:underline"
            title={props.alert.alertname}
          >
            {props.alert.alertname}
          </A>
          <Show when={props.alert.namespace ?? props.alert.service}>
            <span class="shrink-0 truncate font-mono text-[11px] text-ink-subtle">
              {props.alert.namespace}
              {props.alert.namespace && props.alert.service ? "/" : ""}
              {props.alert.service}
            </span>
          </Show>
          <Show when={summary()}>
            {(text) => (
              <span class="min-w-0 truncate text-[12px] text-ink-muted" title={text()}>
                {truncate(text(), 120)}
              </span>
            )}
          </Show>
          <For each={extraLabels()}>
            {([k, v]) => (
              <button
                type="button"
                class="hidden shrink-0 rounded-[3px] border border-line bg-raised px-1 font-mono text-[10px] leading-4 text-ink-subtle hover:border-accent-border hover:text-ink group-hover:inline-flex"
                title={`Filter by ${k}="${v}"`}
                onClick={(e) => {
                  e.stopPropagation();
                  props.onFilterLabel(k, v);
                }}
              >
                {k}={truncate(v, 16)}
              </button>
            )}
          </For>
        </div>
      </td>

      {/* **What the rule said at that moment** — the first promise in the
          README, on the screen it was missing from. */}
      <Show when={props.showRule}>
        <td class="min-w-0 px-2 align-middle">
          <RuleCell
            snapshotId={props.alert.rule?.id ?? null}
            snapshot={props.alert.rule ? (props.rules.get(props.alert.rule.id) ?? null) : null}
            pending={props.rulesPending}
          />
        </td>
      </Show>

      <td class="px-2 align-middle">
        <div class="flex items-center gap-1">
          <StateChip state={props.alert.state} size="sm" urgent={urgent()} />
          <AckChip ackState={props.alert.ack_state} />
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

      <td class="px-2 align-middle">
        <Chip mono title={`Cluster ${props.alert.cluster_key}`}>
          {truncate(props.alert.cluster_key, 18)}
        </Chip>
      </td>

      {/* "Firing duration", never MTTR — oto measures the signal, not anyone's
          response (SCOPE-BOUNDARY). */}
      <td class="px-2 text-right align-middle text-ink-muted">
        <Show when={occ()} fallback={<span class="text-ink-subtle">—</span>}>
          {(o) => <Elapsed from={o().started_at} to={o().ended_at ?? null} />}
        </Show>
      </td>

      <td class="px-2 text-right align-middle tabular-nums text-ink-muted">
        <span title={`${props.alert.total_occurrences} firing episodes since first seen`}>
          {fmtCount(props.alert.total_occurrences)}
        </span>
      </td>

      <td class="px-2 text-right align-middle text-ink-muted">
        <RelativeTime value={props.alert.last_seen_at} label="Last seen" />
      </td>
    </tr>
  );
};

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
          title="No rule definition was captured for this episode."
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
            class="block truncate font-mono text-[11px] leading-4 text-ink-muted"
            title={tooltip(snapshot())}
          >
            {truncate(snapshot().expr, 64)}
          </code>
        )}
      </Match>
    </Switch>
  );
};
