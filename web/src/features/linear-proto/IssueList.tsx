/**
 * The issue list: column header, grouped rows, and the keyboard model (SPEC.md §6).
 *
 * Nothing is derived here. Filtering, sorting, grouping and the flat row order all
 * come from `useIssueView()`; this file only decides what is on screen, what is
 * focused, and what the keyboard does about it.
 *
 * Two indexes exist and must not be confused:
 *   - the FLAT index — position in `flatIssues()`, the store's canonical row order.
 *     Focus, selection and ↑/↓ all speak this one, which is why arrow keys cross
 *     group boundaries without any special case at the seam.
 *   - the DOM position — position among rendered rows. It diverges from the flat
 *     index the moment a group is collapsed, so it is only ever computed as a
 *     fallback for locating an element (see `rowElement`).
 */
import {
  createEffect,
  createMemo,
  createSignal,
  For,
  on,
  Show,
  untrack,
  type JSX,
} from "solid-js";

import { cn } from "~/lib/cn";
import { EmptyState } from "./EmptyState";
import { GroupHeader } from "./GroupHeader";
import { IssueRow } from "./IssueRow";
import { COL, GROUP_HEADER_H, GUTTER, ROW_GAP } from "./layout";
import { useIssueView } from "./store";

/**
 * Anything that swallows typing. `closest()` rather than a tag test, so a key
 * pressed inside a wrapper around an input still counts as typing.
 */
const TYPING_SELECTOR =
  "input, textarea, select, [contenteditable=''], [contenteditable='true']";

/**
 * Anything that means "an overlay owns the keyboard right now". Kobalte stamps
 * `data-expanded` on an open trigger and its content, and portals that content
 * to `document.body` — outside this component — so the probe is document-wide.
 * `role="dialog"` / `role="menu"` catch popovers and menus rendered by wrappers
 * that do not set the data attribute.
 */
const OVERLAY_SELECTOR =
  "[data-kb-expanded], [data-expanded], [role='dialog'], [role='menu']";

function isTypingContext(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest(TYPING_SELECTOR) !== null;
}

/**
 * True while any Kobalte overlay (menu, popover, dialog, command palette) is
 * open anywhere on the page.
 *
 * Exported because the guard is not the list's alone: the route's `[` shortcut
 * needs the same answer, and two hand-rolled probes would drift apart the first
 * time a wrapper stopped stamping one of these attributes.
 */
export function isOverlayOpen(): boolean {
  return document.querySelector(OVERLAY_SELECTOR) !== null;
}

export function IssueList(): JSX.Element {
  const view = useIssueView();
  const [collapsedKeys, setCollapsedKeys] = createSignal<ReadonlySet<string>>(new Set());

  let listEl: HTMLDivElement | undefined;

  /**
   * Whether the list owned DOM focus the last time anything reported focus.
   *
   * Plain `let`, not a signal: it is only ever read imperatively, and making it
   * reactive would re-run the reveal effect every time focus crossed the list
   * boundary. It exists because `document.activeElement` alone cannot answer the
   * question — when the focused row is unmounted (a filter narrows the list) the
   * browser silently drops focus to `<body>` without firing `focusout`, so the
   * only record that the list *had* focus is this flag.
   */
  let listHadFocus = false;

  /**
   * May the list move DOM focus right now? Only if it already owns it. Focus
   * legitimately parked in the side panel or inside a portalled overlay must
   * never be yanked back into the rows.
   */
  const listOwnsFocus = (): boolean => {
    if (isOverlayOpen()) return false;
    const active = document.activeElement;
    // Focus is somewhere concrete: that element is the authority.
    if (active !== null && active !== document.body) {
      return listEl?.contains(active) === true;
    }
    // Focus is nowhere. Ours to reclaim only if it was ours when it vanished.
    return listHadFocus;
  };

  /** Flat index -> owning group key. Same traversal order as `flatIssues()`. */
  const rowGroupKeys = createMemo<string[]>(() =>
    view.groups().flatMap((group) => group.issues.map(() => group.key)),
  );

  /** Flat index of each group's first row, so rows can label themselves. */
  const groupOffsets = createMemo<number[]>(() => {
    const offsets: number[] = [];
    let running = 0;
    for (const group of view.groups()) {
      offsets.push(running);
      running += group.issues.length;
    }
    return offsets;
  });

  const isCollapsed = (key: string): boolean => collapsedKeys().has(key);

  const isRowVisible = (flatIndex: number): boolean => {
    const key = rowGroupKeys()[flatIndex];
    return key !== undefined && !isCollapsed(key);
  };

  /**
   * Keep focus inside the list when filters shrink it under the focused row.
   * Read `focusedIndex` untracked: this effect corrects the index, it must not
   * re-run because of its own write.
   */
  createEffect(() => {
    const count = view.visibleCount();
    const current = untrack(view.focusedIndex);
    if (count > 0 && current > count - 1) view.setFocusedIndex(count - 1);
  });

  /** DOM position of a flat index, counting only rows in expanded groups. */
  const domPosition = (flatIndex: number): number => {
    let position = 0;
    for (let i = 0; i < flatIndex; i += 1) if (isRowVisible(i)) position += 1;
    return position;
  };

  /**
   * The row element for a flat index. `data-row-index` is the contract; the
   * positional lookup is a belt-and-braces fallback so keyboard scrolling still
   * works if a row stops stamping the attribute.
   */
  const rowElement = (flatIndex: number): HTMLElement | null => {
    if (!listEl) return null;
    const tagged = listEl.querySelector<HTMLElement>(`[data-row-index="${flatIndex}"]`);
    if (tagged) return tagged;
    const rendered = listEl.querySelectorAll<HTMLElement>("[role='option']");
    return rendered.item(domPosition(flatIndex)) ?? null;
  };

  /**
   * Scroll-and-focus. `preventScroll` on focus() because the explicit
   * `block: "nearest"` above it is the scroll we want — the browser's default
   * focus scroll would centre the row and make ↓ feel like it jumps.
   */
  const revealRow = (flatIndex: number): void => {
    const el = rowElement(flatIndex);
    if (!el) return;
    el.scrollIntoView({ block: "nearest" });
    const focusTarget = el.matches("[tabindex]")
      ? el
      : el.querySelector<HTMLElement>("[tabindex]");
    focusTarget?.focus({ preventScroll: true });
  };

  /**
   * DOM focus follows the focus index, wherever the index came from.
   *
   * `moveFocus` used to be the only path that revealed, so every other writer —
   * the clamp above, `toggleGroup`, the palette — left focus on a stale (often
   * unmounted) node and the row keys went dead until the next click. One effect
   * downstream of the signal covers all of them, including writers added later.
   *
   * Guarded by `listOwnsFocus()` so it can only ever *return* focus to the list,
   * never take it from an overlay, the side panel, or the page on first paint.
   * `queueMicrotask` so the read happens after the render this write caused —
   * before that, the row for the new index may not exist yet.
   */
  createEffect(() => {
    const index = view.focusedIndex();
    if (view.visibleCount() === 0) return;
    queueMicrotask(() => {
      if (!listOwnsFocus()) return;
      revealRow(index);
    });
  });

  /**
   * Collapsed state is keyed by group key, and the key space changes completely
   * when the grouping does ("done" under Status, "u-2" under Assignee). Keeping
   * stale keys would silently re-collapse an unrelated group on the way back.
   * Deferred so the initial grouping does not pay for a reset.
   */
  createEffect(on(view.groupBy, () => setCollapsedKeys(new Set<string>()), { defer: true }));

  const expandGroupAt = (flatIndex: number): void => {
    const key = rowGroupKeys()[flatIndex];
    if (key === undefined || !isCollapsed(key)) return;
    setCollapsedKeys((prev) => {
      const next = new Set(prev);
      next.delete(key);
      return next;
    });
  };

  /**
   * Move focus along the FLAT order. Clamped at both ends — a list that wraps
   * makes "am I at the bottom?" unanswerable without looking away from the row
   * you are reading.
   */
  const moveFocus = (target: number): void => {
    const count = view.visibleCount();
    if (count === 0) return;
    const next = Math.max(0, Math.min(count - 1, target));
    expandGroupAt(next);
    view.setFocusedIndex(next);
    // Unconditional, and not left to the effect above: an arrow key is proof the
    // list owns focus, and `next` is often unchanged (clamped at an end, or a
    // collapsed group just expanded under the current index) — no index change
    // means no effect run, but the row still has to be focused and scrolled to.
    queueMicrotask(() => revealRow(next));
  };

  const toggleGroup = (key: string): void => {
    setCollapsedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

    // Collapsing the focused row's group would leave no row tabbable at all,
    // breaking the roving tabindex. Land on the nearest still-visible row.
    if (!isCollapsed(key)) return;
    const focused = view.focusedIndex();
    if (rowGroupKeys()[focused] !== key) return;

    const total = rowGroupKeys().length;
    for (let i = focused + 1; i < total; i += 1) {
      if (isRowVisible(i)) {
        view.setFocusedIndex(i);
        return;
      }
    }
    for (let i = focused - 1; i >= 0; i -= 1) {
      if (isRowVisible(i)) {
        view.setFocusedIndex(i);
        return;
      }
    }
  };

  const onKeyDown = (event: KeyboardEvent): void => {
    // Suppression first, and from both ends: the event target covers keys typed
    // into a field inside this list, `document.activeElement` covers focus that
    // has been moved into a portalled overlay while the event still bubbles here.
    if (isTypingContext(event.target) || isTypingContext(document.activeElement)) return;
    if (isOverlayOpen()) return;

    const count = view.visibleCount();
    const focused = view.focusedIndex();

    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "a") {
      event.preventDefault();
      view.selectAll();
      return;
    }
    // Every other chord belongs to the browser or the OS, not to this list.
    if (event.metaKey || event.ctrlKey || event.altKey) return;

    switch (event.key) {
      case "ArrowDown":
        if (count === 0) return;
        event.preventDefault();
        moveFocus(focused + 1);
        return;
      case "ArrowUp":
        if (count === 0) return;
        event.preventDefault();
        moveFocus(focused - 1);
        return;
      case "Home":
        if (count === 0) return;
        event.preventDefault();
        moveFocus(0);
        return;
      case "End":
        if (count === 0) return;
        event.preventDefault();
        moveFocus(count - 1);
        return;
      case "Escape":
        // Nothing selected means Esc is not ours — let it reach whatever owns it.
        if (view.selected().size === 0) return;
        event.preventDefault();
        view.clearSelection();
        return;
      case "x":
      case "X": {
        const issue = view.flatIssues()[focused];
        if (!issue) return;
        event.preventDefault();
        view.toggleSelected(issue.id);
        return;
      }
      default:
        // Anything else falls through untouched. Never swallow a key we do not own.
        return;
    }
  };

  const headerCell = "text-micro font-medium uppercase tracking-[0.08em] font-sans truncate";

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      {/* Header cells import the same COL constants as the body rows, so the two
          cannot drift; the transparent left border mirrors the row focus rail.

          It is held in place by the flex column, NOT by `sticky`: it is a
          shrink-0 sibling *above* the scrollport, so nothing ever scrolls under
          it. A `sticky top-0` here would be an inert no-op — the element is not
          inside a scroll container — so it is not written. `z-20` still applies
          (z-index works on a flex item at any position) and keeps it above the
          group headers' own stacking. */}
      <div
        class={cn(
          GROUP_HEADER_H,
          GUTTER,
          ROW_GAP,
          "z-20 flex shrink-0 items-center",
          "border-b border-[var(--lp-border)] border-l-2 border-l-transparent",
          "text-micro font-medium uppercase tracking-[0.08em] bg-[var(--lp-surface)]",
          "text-[var(--lp-text-3)] select-none",
        )}
        aria-hidden="true"
      >
        <span class={COL.select} />
        <span class={COL.priority} />
        <span class={COL.status} />
        <span class={cn(COL.key, headerCell)}>ID</span>
        <span class={cn(COL.title, headerCell)}>Title</span>
        <span class={cn(COL.labels, headerCell)}>Labels</span>
        <span class={cn(COL.project, headerCell)}>Project</span>
        <span class={COL.avatar} />
        <span class={cn(COL.trailing, headerCell, "flex items-center justify-end")}>
          Target
        </span>
      </div>

      <div
        ref={listEl}
        role="listbox"
        aria-label="Issues"
        aria-multiselectable="true"
        tabindex={-1}
        onKeyDown={onKeyDown}
        onFocusIn={() => {
          listHadFocus = true;
        }}
        onFocusOut={(event) => {
          const next = event.relatedTarget;
          listHadFocus = next instanceof Node && listEl?.contains(next) === true;
        }}
        // THE scrollport for this screen: the only `overflow-y-auto` in the
        // subtree, which is what lets the group headers' `sticky top-0` pin.
        // `scroll-mt-9` on the rows (one group-header height) is the price of
        // that: `scrollIntoView({ block: "nearest" })` would otherwise park a
        // keyboard-focused row flush with the top edge, underneath the pinned
        // header that now sits there.
        class="min-h-0 flex-1 overflow-y-auto outline-none [&_[role='option']]:scroll-mt-9"
      >
        <Show
          when={view.visibleCount() > 0}
          fallback={
            <EmptyState
              // SPEC §7: the escape hatch appears only when there is something
              // to escape from. An empty list with no filters set is just empty.
              // Spread rather than `undefined`: `exactOptionalPropertyTypes` is
              // on, so an absent prop and an explicitly-undefined one differ.
              {...(view.activeFilterCount() > 0
                ? { onClearFilters: () => view.clearAllFilters() }
                : {})}
            />
          }
        >
          <For each={view.groups()}>
            {(group, groupIndex) => {
              const offset = (): number => groupOffsets()[groupIndex()] ?? 0;
              const collapsed = (): boolean => isCollapsed(group.key);
              return (
                <div role="group" aria-label={group.label} class="relative">
                  <GroupHeader
                    group={group}
                    collapsed={collapsed()}
                    onToggle={() => toggleGroup(group.key)}
                  />
                  <Show when={!collapsed()}>
                    <For each={group.issues}>
                      {(issue, issueIndex) => {
                        const flatIndex = (): number => offset() + issueIndex();
                        return (
                          <IssueRow
                            issue={issue}
                            index={flatIndex()}
                            focused={view.focusedIndex() === flatIndex()}
                            selected={view.selected().has(issue.id)}
                            anySelected={view.selected().size > 0}
                            density={view.density()}
                            onFocus={() => view.setFocusedIndex(flatIndex())}
                          />
                        );
                      }}
                    </For>
                  </Show>
                </div>
              );
            }}
          </For>
        </Show>
      </div>
    </div>
  );
}
