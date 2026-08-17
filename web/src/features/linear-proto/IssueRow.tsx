import { createSignal, For, onCleanup, Show } from "solid-js";
import type { JSX } from "solid-js";

import { cn } from "~/lib/cn";
import { COL, COMPACT_ROW_H, GUTTER, ROW_GAP, ROW_H } from "./layout";
import { useIssueView, type Density } from "./store";
import {
  PRIORITY_LABEL,
  PRIORITY_ORDER,
  STATUS_LABEL,
  STATUS_ORDER,
  type Issue,
  type Priority,
} from "./types";
import { Avatar } from "./primitives/Avatar";
import { Checkbox } from "./primitives/Checkbox";
import { LabelBadge } from "./primitives/LabelBadge";
import { Menu, MenuItem, MenuLabel, MenuSeparator } from "./primitives/Menu";
import { PriorityIcon } from "./primitives/PriorityIcon";
import { ProjectBadge } from "./primitives/ProjectBadge";
import { StatusIcon } from "./primitives/StatusIcon";

/**
 * One issue row.
 *
 * The whole row is a single flex line whose column widths come exclusively from
 * `COL` in layout.ts, so the view header and the body can never drift apart.
 *
 * Two invariants are load-bearing and easy to break by accident:
 *
 * 1. The focus rail is a *recolour*, not an addition. Every row carries
 *    `border-l-2 border-l-transparent` at rest; focus only swaps the colour, so
 *    focusing a row can never shove its content 2px to the right.
 * 2. The trailing slot is a fixed `w-24 h-5` box with the target date and the
 *    quick-action bar stacked absolutely at `inset-0`. They cross-fade; neither
 *    ever participates in layout, so the hover swap costs zero reflow.
 */

/** Urgent-first, which is how a priority picker is read. */
const PRIORITY_MENU_ORDER: readonly Priority[] = [...PRIORITY_ORDER].reverse();

/** Labels beyond this collapse into a `+N` chip; the column never wraps. */
const MAX_VISIBLE_LABELS = 2;

/** Which quick-action menu is open. At most one at a time, by construction. */
type QuickMenu = "assignee" | "priority" | "status" | "more";

/**
 * SPEC §6: `a` / `p` / `s` open a menu on the focused row.
 *
 * `s` binds to a real status menu, not to "More actions". A shortcut whose
 * documented meaning is "open the status menu" has to land on a menu that is
 * about status, or the table in the spec is describing a different program.
 */
const SHORTCUT_MENUS: Record<string, QuickMenu> = {
  a: "assignee",
  p: "priority",
  s: "status",
};

/**
 * Anything that means "an overlay owns the keyboard right now" — the same probe
 * `IssueList` exports, deliberately re-declared rather than imported: `IssueList`
 * imports `IssueRow`, and reaching back the other way would close an import
 * cycle through a module that runs at import time.
 */
const OVERLAY_SELECTOR =
  "[data-kb-expanded], [data-expanded], [role='dialog'], [role='menu']";

/**
 * Marks the trailing quick-action bar. The rescue below needs to tell "focus is
 * parked on a trigger that has just gone `opacity-0 pointer-events-none`" apart
 * from "focus is on the row's checkbox", and a class test would be brittle.
 */
const QUICK_ACTIONS_ATTR = "data-lp-quick-actions";

/**
 * How long after a menu closes we keep re-asserting focus.
 *
 * Not a guess — measured in Chrome against the two close paths, which restore
 * focus at wildly different times (see `reclaimFocus`): ~17ms for a selection,
 * ~127ms for Escape/click-outside, the latter being the 100ms `lp-overlay-out`
 * exit animation plus Kobalte's own `setTimeout(…, 0)`. 300ms clears both with
 * room for a slow frame, and every tick is a no-op once focus has settled.
 */
const RECLAIM_WINDOW_MS = 300;

/**
 * The close baton, at module scope because the row that opened the menu is
 * frequently NOT the row that has to finish the job.
 *
 * Selecting a value calls `patchIssue`, which puts a NEW `Issue` object in the
 * store; `IssueList` renders rows through a `<For>` keyed by object identity, so
 * the whole `IssueRow` — its ref, its signals, its `onCleanup` — is disposed and
 * rebuilt mid-close. Anything held on the instance dies with it (and its
 * `onCleanup` dutifully cancels the very frame that would have done the rescue).
 * This hands the pending close to whichever row takes its place at that index.
 */
let closingRowIndex: number | null = null;
let closingArmedAt = 0;

function armReclaim(index: number): void {
  closingRowIndex = index;
  closingArmedAt = performance.now();
}

/** Did a menu on this index close just now, in a row that did not survive it? */
function reclaimArmedFor(index: number): boolean {
  return (
    closingRowIndex === index &&
    performance.now() - closingArmedAt < RECLAIM_WINDOW_MS
  );
}

const MONTHS: readonly string[] = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

/** `2026-08-16` → `Aug 16`. Parsed by hand so no timezone can shift the day. */
function formatTargetDate(iso: string): string {
  const [year, month, day] = iso.split("-");
  const monthName = MONTHS[Number(month) - 1];
  if (year === undefined || monthName === undefined || day === undefined) return iso;
  return `${monthName} ${day.padStart(2, "0")}`;
}

/** Today as `yyyy-mm-dd`, so due-date comparison is a plain string compare. */
function todayIso(): string {
  const now = new Date();
  const month = String(now.getMonth() + 1).padStart(2, "0");
  const day = String(now.getDate()).padStart(2, "0");
  return `${now.getFullYear()}-${month}-${day}`;
}

/** A date only reads as "late" while the work is still live. */
function isPastDue(issue: Issue): boolean {
  if (issue.targetDate === null) return false;
  if (issue.status === "done" || issue.status === "canceled") return false;
  return issue.targetDate < todayIso();
}

/**
 * A 20px glyph is far below the 40px minimum hit area, so each quick action
 * grows an invisible `::before` cushion instead of growing the glyph.
 *
 * The cushion is asymmetric on purpose, and it is sized against the SHORTER
 * row, not the taller one. Vertically it expands by 6px a side — 20 + 12 = 32 =
 * `h-8`, the compact density — so it fills a compact row exactly and leaves 2px
 * of slack in a comfortable `h-9` one. Sizing it to 36px instead would look
 * correct at comfortable density and spill 2px into both neighbours at compact,
 * and because the cushion belongs to a row *descendant*, hovering that spill
 * puts `:hover` on the previous row: its action bar turns `pointer-events-auto`
 * and swallows a click aimed at the row below. Horizontally it expands by only
 * 2px a side, which is *half* the `gap-1` separating the buttons; two
 * neighbouring cushions therefore meet at the midpoint of the gap and can never
 * overlap, so no press is ever ambiguous about which action it hit.
 */
const QUICK_BUTTON =
  "w-5 h-5 flex items-center justify-center rounded-none text-[var(--lp-text-2)] " +
  "relative before:absolute before:content-[''] before:-inset-y-1.5 before:-inset-x-0.5 " +
  "hover:bg-white/[0.08] hover:text-[var(--lp-text)] active:scale-[0.97] " +
  "transition-[color,background-color,transform] duration-150 motion-reduce:transition-none";

/**
 * Trigger content for a quick action.
 *
 * The visible glyph is `aria-hidden` and the name is carried by an `sr-only`
 * span, so the control is named correctly whether the `Menu` primitive renders
 * this element as the button or wraps it in one.
 */
function TriggerBody(props: { label: string; children: JSX.Element }): JSX.Element {
  return (
    <span class={QUICK_BUTTON}>
      {props.children}
      <span class="sr-only">{props.label}</span>
    </span>
  );
}

function AssignGlyph(): JSX.Element {
  return (
    <svg viewBox="0 0 14 14" class="w-3.5 h-3.5" fill="none" aria-hidden="true">
      <circle cx="7" cy="4.6" r="2.3" stroke="currentColor" stroke-width="1.5" />
      <path d="M2.2 12.2c0-2.7 2.15-4.3 4.8-4.3s4.8 1.6 4.8 4.3" stroke="currentColor" stroke-width="1.5" />
    </svg>
  );
}

function MoreGlyph(): JSX.Element {
  return (
    <svg viewBox="0 0 14 14" class="w-3.5 h-3.5" fill="currentColor" aria-hidden="true">
      <rect x="1.6" y="6" width="2" height="2" />
      <rect x="6" y="6" width="2" height="2" />
      <rect x="10.4" y="6" width="2" height="2" />
    </svg>
  );
}

export function IssueRow(props: {
  issue: Issue;
  index: number;
  focused: boolean;
  selected: boolean;
  anySelected: boolean;
  density: Density;
  onFocus: () => void;
}): JSX.Element {
  const view = useIssueView();

  /**
   * THE hover lock. A popover is portaled out of the row, so the moment the
   * pointer travels into it the row loses `:hover`, the action bar fades out,
   * and the menu closes itself. Holding the open menu in a signal — and OR-ing
   * it into every hover-driven class below — keeps the row latched open until
   * the menu itself reports that it closed.
   */
  const [openMenu, setOpenMenu] = createSignal<QuickMenu | null>(null);
  const menuOpen = () => openMenu() !== null;

  const isMenuOpen = (id: QuickMenu) => openMenu() === id;

  /**
   * The row element. Held as a ref rather than looked up by `data-row-index`
   * because the rescue below must only ever target THIS instance's node — after
   * a selection the previous row's node is still reachable, but detached.
   */
  let rowEl: HTMLDivElement | undefined;

  /**
   * Where does DOM focus stand, from this row's point of view?
   *
   * This is the whole safety argument for the rescue, so it is a three-way
   * answer rather than a boolean. `"yield"` is the important one: it means focus
   * has been put somewhere deliberate, and this row must stand down for good —
   * without it, Kobalte's late restore-to-trigger (~127ms) would pull focus off
   * whatever the user had just moved to, and the rescue would then dutifully
   * carry that stolen focus onto the row. Measured: focusing a sidebar button
   * mid-close ended up on the row until this case was split out.
   */
  const focusVerdict = (): "reclaim" | "yield" | "wait" => {
    const active = document.activeElement;
    // Kobalte aimed at a trigger that had already been unmounted along with the
    // old row, so its focus() was a no-op and the document kept nothing.
    if (active === null || active === document.body) return "reclaim";
    if (!active.isConnected) return "reclaim";
    if (rowEl === undefined) return "wait";
    // Already ours — but keep watching, because the late restore is still coming.
    if (active === rowEl) return "wait";
    if (rowEl.contains(active)) {
      // Kobalte restored focus to our own trigger, which the closing menu has
      // just returned to `opacity-0 pointer-events-none`. Ours to take back —
      // but only from the action bar; the checkbox is a legitimate resting place.
      return active.closest(`[${QUICK_ACTIONS_ATTR}]`) !== null ? "reclaim" : "yield";
    }
    // Inside a live overlay — including our own menu still playing its 100ms exit
    // animation, which is where focus legitimately sits for the first few frames
    // of every close. Not a decision yet, in either direction.
    if (active.closest(OVERLAY_SELECTOR) !== null) return "wait";
    // A real element somewhere else entirely: another row, the side panel, the
    // command palette's input, anything the user reached for. Hands off.
    return "yield";
  };

  let frame: number | undefined;
  let reclaimUntil = 0;

  const stopReclaim = (): void => {
    if (frame !== undefined) cancelAnimationFrame(frame);
    frame = undefined;
  };

  /**
   * Re-assert focus every frame until the window closes or the row stands down.
   *
   * A single deferred tick cannot work here, which is why this is a loop and not
   * a one-liner: measured in Chrome, the two close paths restore focus 17ms
   * (selection, which rebuilds the row) and 127ms (Escape / click-outside, which
   * waits out the exit animation) after the key. Any one primitive —
   * `queueMicrotask`, one `requestAnimationFrame`, `setTimeout(…, 0)` — is
   * either far too early for the second path or useless for the first.
   *
   * `requestAnimationFrame` rather than a timer chain because it is paint-aligned
   * and self-throttles in a background tab, and each tick is two cheap DOM reads
   * that stop the moment focus settles.
   */
  const startReclaim = (): void => {
    reclaimUntil = performance.now() + RECLAIM_WINDOW_MS;
    if (frame !== undefined) return;
    const tick = (): void => {
      frame = undefined;
      // The index moved on (a filter, a re-group, an arrow key): not ours to hold.
      if (!props.focused) return;
      if (rowEl === undefined || !rowEl.isConnected) return;
      const verdict = focusVerdict();
      if (verdict === "yield") return;
      if (verdict === "reclaim") {
        // Belt and braces over the verdict: never move focus while anything with
        // a keyboard claim is open anywhere on the page.
        if (document.querySelector(OVERLAY_SELECTOR) === null) {
          // `preventScroll`: the row is already on screen, and the browser's
          // default focus scroll would centre it and read as a jump.
          rowEl.focus({ preventScroll: true });
        }
      }
      if (performance.now() < reclaimUntil) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
  };

  /** A row unmounting mid-close must never refocus a dead node. */
  onCleanup(() => {
    stopReclaim();
    // Disposed while a menu was still open or closing — the select path, where
    // `patchIssue` swaps the Issue identity out from under `<For>`. Pass the
    // close to the replacement row; this instance can no longer act on it.
    if (openMenu() !== null) armReclaim(props.index);
  });

  // Mounting into a close that the previous occupant of this index could not
  // finish. Gated on the baton, never on `props.focused` alone: on first paint
  // row 0 is focused and the document is on `<body>`, and an ungated rescue
  // would yank focus off the page the moment the list rendered.
  if (reclaimArmedFor(props.index)) {
    closingRowIndex = null;
    startReclaim();
  }

  /**
   * Controlled close that ignores a stale close report from another menu.
   *
   * Every close path Kobalte has — Escape, Enter/click selection, click-outside
   * — funnels through `onOpenChange(false)`, so arming here covers all three.
   * It is armed at module scope as well as started locally because this handler
   * may be the last thing this row instance ever runs.
   */
  const onMenuOpenChange = (id: QuickMenu) => (next: boolean) => {
    setOpenMenu((current) => (next ? id : current === id ? null : current));
    if (!next && openMenu() === null) {
      armReclaim(props.index);
      startReclaim();
    }
  };

  /**
   * Hoisted, not per-row. Deriving the roster here re-ran the whole scan in
   * every one of ~60 rows, and again in all of them on every `patchIssue`.
   */
  const roster = view.assigneeRoster;

  /** One tab stop for the entire list (SPEC §6): the focused row owns it, and
   * every control it contains borrows it. Without this each row contributes a
   * checkbox and four menu triggers, so Tab walks ~300 stops down the list. */
  const rovingTabIndex = () => (props.focused ? 0 : -1);

  const visibleLabels = () => props.issue.labels.slice(0, MAX_VISIBLE_LABELS);
  const hiddenLabelCount = () =>
    Math.max(0, props.issue.labels.length - MAX_VISIBLE_LABELS);

  /** Hover, keyboard focus, an active selection or an open menu all reveal chrome. */
  const revealed = () =>
    props.focused || props.selected || props.anySelected || menuOpen();

  const rowLabel = () => {
    const issue = props.issue;
    const who = issue.assignee === null ? "Unassigned" : issue.assignee.name;
    return `${issue.key}. ${issue.title}. ${STATUS_LABEL[issue.status]}, ${
      PRIORITY_LABEL[issue.priority]
    }, ${who}`;
  };

  return (
    <div
      ref={rowEl}
      role="option"
      aria-selected={props.selected}
      aria-label={rowLabel()}
      tabindex={props.focused ? 0 : -1}
      data-row-index={props.index}
      class={cn(
        "group flex items-center outline-none",
        // The rail exists at every moment; only its colour changes. This is the
        // single reason focus cannot nudge the row 2px sideways.
        "border-l-2 border-l-transparent",
        "transition-colors duration-150 motion-reduce:transition-none",
        "hover:bg-white/[0.04]",
        GUTTER,
        ROW_GAP,
        props.density === "compact" ? COMPACT_ROW_H : ROW_H,
        // Pointer is inside the portaled popover: paint the row as if hovered.
        menuOpen() && "bg-white/[0.04]",
        // SPEC §1: selection is the accent rail plus `/10`, the same pair as
        // focus. A tint alone left selection legible only via the checkbox,
        // which is hidden until hover on every unselected row.
        props.selected &&
          "border-l-[var(--lp-accent)] bg-[#5e6ad2]/10 hover:bg-[#5e6ad2]/[0.14]",
        // Focus stays LAST so twMerge lets it win the conflict with the
        // selected background: a focused row inside a selection reads as
        // focused, not as one more selected row.
        props.focused &&
          "border-l-[var(--lp-accent)] bg-[#5e6ad2]/10 hover:bg-[#5e6ad2]/10",
      )}
      onFocus={() => props.onFocus()}
      onClick={(event) => {
        props.onFocus();
        event.currentTarget.focus();
      }}
      onKeyDown={(event) => {
        // Never steal a key from a text field or from an open overlay: only the
        // row element itself, with no modifier held, arms these shortcuts.
        if (event.target !== event.currentTarget) return;
        if (event.metaKey || event.ctrlKey || event.altKey) return;
        const menu = SHORTCUT_MENUS[event.key.toLowerCase()];
        if (menu === undefined) return;
        event.preventDefault();
        event.stopPropagation();
        setOpenMenu(menu);
      }}
    >
      {/* 1. Select — always occupies its w-5; only opacity changes. */}
      <div
        class={COL.select}
        onClick={(event) => event.stopPropagation()}
      >
        <Checkbox
          checked={props.selected}
          onChange={() => view.toggleSelected(props.issue.id)}
          label={`Select ${props.issue.key}`}
          // A visible label would push the 14px square off the centre of this
          // 20px column and out of line with the priority and status glyphs.
          labelHidden
          tabIndex={rovingTabIndex()}
          class={cn(
            // Same cushion argument as QUICK_BUTTON, and the same compact-first
            // arithmetic as the one inside the primitive: 8px vertically on a
            // 14px box is 30px, which still clears the `h-8` compact row rather
            // than spilling into the neighbours the way an `h-9`-sized cushion
            // would. Horizontally 4px is half the `gap-2` to the priority
            // column — and that column holds no control, so nothing can contend
            // for the press.
            "relative before:absolute before:content-[''] before:-inset-y-2 before:-inset-x-1",
            "opacity-0 transition-opacity duration-150 motion-reduce:transition-none",
            "group-hover:opacity-100 group-focus-within:opacity-100",
            revealed() && "opacity-100",
          )}
        />
      </div>

      {/* 2. Priority */}
      <div class={COL.priority}>
        <PriorityIcon priority={props.issue.priority} />
      </div>

      {/* 3. Status */}
      <div class={COL.status}>
        <StatusIcon status={props.issue.status} />
      </div>

      {/* 4. Key */}
      <span class={cn(COL.key, "text-meta font-medium font-mono tabular-nums text-[var(--lp-text-3)]")}>
        {props.issue.key}
      </span>

      {/* 5. Title — the only elastic column. */}
      <span class={cn(COL.title, "text-item font-medium text-[var(--lp-text)]")}>
        {props.issue.title}
      </span>

      {/* 6. Labels — clipped, never wrapped, with a +N chip on overflow. */}
      <Show when={props.issue.labels.length > 0}>
        <div class={cn(COL.labels, "whitespace-nowrap")}>
          <For each={visibleLabels()}>
            {(label) => <LabelBadge label={label} class="min-w-0" />}
          </For>
          <Show when={hiddenLabelCount() > 0}>
            <span
              class="text-meta font-medium font-mono tabular-nums shrink-0 px-1 text-[var(--lp-text-3)]"
              title={`${hiddenLabelCount()} more label${hiddenLabelCount() === 1 ? "" : "s"}`}
            >
              +{hiddenLabelCount()}
            </span>
          </Show>
        </div>
      </Show>

      {/* 7. Project */}
      <Show when={props.issue.project}>
        {(name) => <ProjectBadge name={name()} class={COL.project} />}
      </Show>

      {/* 8. Assignee */}
      <div class={COL.avatar}>
        <Avatar assignee={props.issue.assignee} />
      </div>

      {/*
        9. Trailing — the no-shift swap.

        A fixed w-24 h-5 box. Both children are absolutely positioned at inset-0,
        so neither has any effect on the flex line: the date and the action bar
        occupy the exact same rectangle and cross-fade in place. Nothing reflows,
        and the row's other columns never learn that anything happened.
      */}
      <div class={COL.trailing} onClick={(event) => event.stopPropagation()}>
        <div
          class={cn(
            "absolute inset-0 flex items-center justify-end",
            "transition-opacity duration-150 motion-reduce:transition-none",
            "opacity-100 group-hover:opacity-0 group-focus-within:opacity-0",
            menuOpen() && "opacity-0",
          )}
        >
          <Show
            when={props.issue.targetDate}
            fallback={
              <span class="text-meta font-medium font-mono tabular-nums text-[var(--lp-text-3)]">—</span>
            }
          >
            {(date) => (
              <time
                datetime={date()}
                class={cn(
                  "text-meta font-medium font-mono tabular-nums",
                  isPastDue(props.issue)
                    ? "text-[var(--lp-red)]"
                    : "text-[var(--lp-text-2)]",
                )}
              >
                {formatTargetDate(date())}
              </time>
            )}
          </Show>
        </div>

        <div
          {...{ [QUICK_ACTIONS_ATTR]: "" }}
          class={cn(
            // gap-1, not gap-0.5: the buttons' 2px hit cushions meet exactly at
            // the midpoint of a 4px gap. A 2px gap would make them overlap.
            // Four 20px buttons and three 4px gaps is 92px, inside the fixed
            // `w-24` (96px) trailing box, so the bar still cannot reflow.
            "absolute inset-0 flex items-center justify-end gap-1",
            "transition-opacity duration-150 motion-reduce:transition-none",
            "opacity-0 pointer-events-none",
            "group-hover:opacity-100 group-hover:pointer-events-auto",
            "group-focus-within:opacity-100 group-focus-within:pointer-events-auto",
            // The latch: survives the pointer leaving the row for the popover.
            menuOpen() && "opacity-100 pointer-events-auto",
          )}
        >
          <Menu
            placement="bottom-end"
            open={isMenuOpen("assignee")}
            onOpenChange={onMenuOpenChange("assignee")}
            triggerTabIndex={rovingTabIndex()}
            trigger={
              <TriggerBody label={`Assign ${props.issue.key}`}>
                <AssignGlyph />
              </TriggerBody>
            }
          >
            <MenuLabel>Assign to</MenuLabel>
            <For each={roster()}>
              {(assignee) => (
                <MenuItem
                  selected={props.issue.assignee?.id === assignee.id}
                  onSelect={() => view.patchIssue(props.issue.id, { assignee })}
                >
                  <span class="flex min-w-0 items-center gap-2">
                    <Avatar assignee={assignee} class="w-4 h-4" />
                    <span class="truncate">{assignee.name}</span>
                  </span>
                </MenuItem>
              )}
            </For>
            <MenuSeparator />
            <MenuItem
              selected={props.issue.assignee === null}
              onSelect={() => view.patchIssue(props.issue.id, { assignee: null })}
            >
              No assignee
            </MenuItem>
          </Menu>

          <Menu
            placement="bottom-end"
            open={isMenuOpen("priority")}
            onOpenChange={onMenuOpenChange("priority")}
            triggerTabIndex={rovingTabIndex()}
            trigger={
              <TriggerBody label={`Set priority of ${props.issue.key}`}>
                <PriorityIcon priority={props.issue.priority} />
              </TriggerBody>
            }
          >
            <MenuLabel>Priority</MenuLabel>
            <For each={PRIORITY_MENU_ORDER}>
              {(priority) => (
                <MenuItem
                  selected={props.issue.priority === priority}
                  onSelect={() => view.patchIssue(props.issue.id, { priority })}
                >
                  <span class="flex items-center gap-2">
                    <PriorityIcon priority={priority} />
                    {PRIORITY_LABEL[priority]}
                  </span>
                </MenuItem>
              )}
            </For>
          </Menu>

          {/*
            Status is its own quick action, not a section buried in "More".
            SPEC §6 binds `s` to a status menu, and a shortcut has to be able to
            point at something the pointer can reach the same way.
          */}
          <Menu
            placement="bottom-end"
            open={isMenuOpen("status")}
            onOpenChange={onMenuOpenChange("status")}
            triggerTabIndex={rovingTabIndex()}
            trigger={
              <TriggerBody label={`Set status of ${props.issue.key}`}>
                <StatusIcon status={props.issue.status} />
              </TriggerBody>
            }
          >
            <MenuLabel>Status</MenuLabel>
            <For each={STATUS_ORDER}>
              {(status) => (
                <MenuItem
                  selected={props.issue.status === status}
                  onSelect={() => view.patchIssue(props.issue.id, { status })}
                >
                  <span class="flex items-center gap-2">
                    <StatusIcon status={status} />
                    {STATUS_LABEL[status]}
                  </span>
                </MenuItem>
              )}
            </For>
          </Menu>

          <Menu
            placement="bottom-end"
            open={isMenuOpen("more")}
            onOpenChange={onMenuOpenChange("more")}
            triggerTabIndex={rovingTabIndex()}
            trigger={
              <TriggerBody label={`More actions for ${props.issue.key}`}>
                <MoreGlyph />
              </TriggerBody>
            }
          >
            <MenuLabel>More actions</MenuLabel>
            <MenuItem
              onSelect={() => view.patchIssue(props.issue.id, { assignee: null })}
            >
              Remove assignee
            </MenuItem>
            <Show when={props.issue.targetDate !== null}>
              <MenuItem
                onSelect={() => view.patchIssue(props.issue.id, { targetDate: null })}
              >
                Clear target date
              </MenuItem>
            </Show>
          </Menu>
        </div>
      </div>
    </div>
  );
}
