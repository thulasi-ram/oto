/**
 * The ⌘K command palette.
 *
 * Every entry drives a real `useIssueView()` action — there are no decorative
 * rows. The palette is therefore a second, keyboard-only face of the side panel
 * (SPEC.md §7) rather than a separate feature: change grouping, change sort,
 * flip density, drop filters, collapse the panel.
 *
 * Two things are worth knowing before editing this file:
 *
 * 1. Kobalte portals content to `document.body`, which is OUTSIDE the route's
 *    `.linear-proto` wrapper — so every `--lp-*` variable would resolve to
 *    nothing. The portalled positioning layer re-declares them by carrying the
 *    `linear-proto` class itself, and neutralises that class's own `background`
 *    with an inline style so the backdrop stays visible through it.
 * 2. `.linear-proto` and a Tailwind `bg-*` utility are both single-class
 *    selectors, so a class alone could not reliably beat it. The inline style is
 *    what makes the layer's transparency deterministic.
 * 3. There is NO enter animation here, and that is the design, not an omission.
 *    This is a ⌘K surface a user opens dozens of times a day; every millisecond
 *    between the keystroke and a focusable input is felt as lag, and a scale or
 *    slide on the content makes a fast palette feel slow. Depth is one hairline
 *    ring over a `bg-black/60` backdrop — no shadow stack either.
 *
 * SPEC.md §9 routes overlays through `primitives/`; there is no local Dialog
 * wrapper and this is the only dialog in the prototype, so Kobalte is imported
 * directly here rather than adding a one-caller indirection.
 */
import { createEffect, createMemo, createSignal, For, Show, type JSX } from "solid-js";
import * as DialogPrimitive from "@kobalte/core/dialog";

import { cn } from "~/lib/cn";
import { Keycap } from "./primitives/Keycap";
import { GROUP_BY_OPTIONS, SORT_BY_OPTIONS, useIssueView, type IssueViewStore } from "./store";

interface PaletteCommand {
  /** Stable, DOM-id-safe. Used for `<For>` keying and `aria-activedescendant`. */
  id: string;
  /** The only string the query matches against. */
  label: string;
  /** Right-aligned keycap, present only where a real global shortcut exists. */
  keys?: string;
  /** Marks the command as the view's current setting (radio-style entries). */
  isCurrent?: () => boolean;
  run: () => void;
}

interface PaletteSection {
  title: string;
  commands: PaletteCommand[];
}

/**
 * Built once per mount. Nothing reactive is read here — the closures are what
 * carry reactivity, so the shape of the palette is constant and only its
 * contents change.
 */
function buildSections(view: IssueViewStore): PaletteSection[] {
  return [
    {
      title: "View",
      commands: [
        {
          id: "view-clear-filters",
          label: "Clear all filters",
          run: () => view.clearAllFilters(),
        },
        {
          id: "view-toggle-panel",
          label: "Toggle side panel",
          keys: "[",
          run: () => view.togglePanel(),
        },
        {
          id: "view-select-all",
          label: "Select all issues",
          run: () => view.selectAll(),
        },
        {
          id: "view-clear-selection",
          label: "Clear selection",
          run: () => view.clearSelection(),
        },
      ],
    },
    {
      title: "Group by",
      commands: GROUP_BY_OPTIONS.map((option) => ({
        id: `group-${option.value}`,
        label: `Group by ${option.label.toLowerCase()}`,
        isCurrent: () => view.groupBy() === option.value,
        run: () => view.setGroupBy(option.value),
      })),
    },
    {
      title: "Sort by",
      commands: [
        ...SORT_BY_OPTIONS.map((option) => ({
          id: `sort-${option.value}`,
          label: `Sort by ${option.label.toLowerCase()}`,
          isCurrent: () => view.sortBy() === option.value,
          run: () => view.setSortBy(option.value),
        })),
        {
          id: "sort-direction",
          label: "Toggle sort direction",
          run: () => view.toggleSortDir(),
        },
      ],
    },
    {
      title: "Display",
      commands: [
        {
          id: "display-density",
          label: "Toggle density",
          isCurrent: () => view.density() === "compact",
          run: () => view.setDensity(view.density() === "compact" ? "comfortable" : "compact"),
        },
        {
          id: "display-empty-groups",
          label: "Toggle empty groups",
          isCurrent: () => view.showEmptyGroups(),
          run: () => view.setShowEmptyGroups((value) => !value),
        },
      ],
    },
  ];
}

export function CommandPalette(): JSX.Element {
  const view = useIssueView();
  const sections = buildSections(view);

  const [query, setQuery] = createSignal("");
  const [active, setActive] = createSignal(0);

  /**
   * Sections filtered by the query, with each surviving command carrying its
   * position in the flattened order. Numbering here (rather than at render
   * time) is what lets ↑/↓ cross section boundaries without the sections
   * needing to know about each other.
   */
  const visible = createMemo(() => {
    const needle = query().trim().toLowerCase();
    let cursor = 0;
    const result: { title: string; items: { command: PaletteCommand; index: number }[] }[] = [];
    for (const section of sections) {
      const items = section.commands
        .filter((command) => needle === "" || command.label.toLowerCase().includes(needle))
        .map((command) => ({ command, index: cursor++ }));
      if (items.length > 0) result.push({ title: section.title, items });
    }
    return result;
  });

  const flat = createMemo(() => visible().flatMap((section) => section.items));

  /**
   * Clamped at read time rather than corrected by an effect, so a shrinking
   * result list can never leave the highlight pointing past the end.
   */
  const activeIndex = createMemo(() => Math.min(active(), Math.max(flat().length - 1, 0)));

  const activeCommand = createMemo(() => flat()[activeIndex()]?.command);

  const move = (delta: number): void => {
    const count = flat().length;
    if (count === 0) return;
    setActive((current) => (Math.min(current, count - 1) + delta + count) % count);
  };

  const close = (): void => {
    view.setPaletteOpen(false);
  };

  const runActive = (): void => {
    const command = activeCommand();
    if (!command) return;
    command.run();
    close();
  };

  const onOpenChange = (open: boolean): void => {
    view.setPaletteOpen(open);
    // Reset on close, not on open: the palette should never reopen showing the
    // tail of a previous search.
    if (!open) {
      setQuery("");
      setActive(0);
    }
  };

  const onKeyDown = (event: KeyboardEvent): void => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      move(1);
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      move(-1);
    } else if (event.key === "Enter") {
      event.preventDefault();
      runActive();
    }
    // Escape is Kobalte's; it closes the dialog and `onOpenChange` clears state.
  };

  return (
    <DialogPrimitive.Root open={view.paletteOpen()} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay class="fixed inset-0 z-50 bg-black/60" />
        <div
          class="linear-proto pointer-events-none fixed inset-0 z-50 flex items-center justify-center p-4"
          style={{ background: "transparent" }}
        >
          <DialogPrimitive.Content
            class={cn(
              "pointer-events-auto flex w-[560px] max-w-[90vw] flex-col rounded-none outline-none",
              // One hairline, drawn once. The border and a 1px spread-only
              // shadow of the SAME colour used to be stacked here, which just
              // painted the edge twice and made it read 2px thick.
              "border border-[var(--lp-border)] bg-[var(--lp-overlay)] text-[var(--lp-text)]",
            )}
            onKeyDown={onKeyDown}
          >
            <DialogPrimitive.Title class="sr-only">Command palette</DialogPrimitive.Title>

            <input
              type="text"
              role="combobox"
              aria-expanded="true"
              aria-controls="lp-command-list"
              aria-activedescendant={activeCommand() ? `lp-cmd-${activeCommand()?.id}` : undefined}
              autocomplete="off"
              autocapitalize="off"
              spellcheck={false}
              placeholder="Type a command…"
              value={query()}
              onInput={(event) => {
                setQuery(event.currentTarget.value);
                setActive(0);
              }}
              class={cn(
                "text-item h-11 w-full shrink-0 border-0 bg-transparent px-4",
                "text-[var(--lp-text)] placeholder:text-[var(--lp-text-3)]",
                "outline-none focus:outline-none focus:ring-0",
              )}
            />

            <div class="h-px shrink-0 bg-[var(--lp-border)]" />

            <div
              id="lp-command-list"
              role="listbox"
              aria-label="Commands"
              class="max-h-[320px] overflow-y-auto py-1"
            >
              <For each={visible()}>
                {(section) => (
                  // `role="group"` is load-bearing, not decoration. A listbox
                  // only owns options that are its children or inside a group;
                  // wrapping them in a plain <div> orphaned every option and
                  // screen readers announced the palette as an empty list. The
                  // group carries the section name, so the visible heading is
                  // `aria-hidden` — otherwise it is read out twice per section.
                  <div role="group" aria-label={section.title}>
                    <div
                      aria-hidden="true"
                      class="text-micro font-medium uppercase tracking-[0.08em] flex h-7 items-center px-4 text-[var(--lp-text-3)]"
                    >
                      {section.title}
                    </div>
                    <For each={section.items}>
                      {(item) => (
                        <CommandRow
                          command={item.command}
                          active={item.index === activeIndex()}
                          onActivate={() => setActive(item.index)}
                          onRun={() => {
                            item.command.run();
                            close();
                          }}
                        />
                      )}
                    </For>
                  </div>
                )}
              </For>
              <Show when={flat().length === 0}>
                <div class="text-item flex h-8 items-center px-4 text-[var(--lp-text-3)]">
                  No matching commands
                </div>
              </Show>
            </div>

            <div class="text-meta flex h-8 shrink-0 items-center gap-3 border-t border-[var(--lp-border)] px-4 text-[var(--lp-text-3)]">
              <span class="flex items-center gap-1.5">
                <Keycap>↑</Keycap>
                <Keycap>↓</Keycap>
                navigate
              </span>
              <span class="flex items-center gap-1.5">
                <Keycap>↵</Keycap>
                run
              </span>
              <span class="flex items-center gap-1.5">
                <Keycap>esc</Keycap>
                close
              </span>
            </div>
          </DialogPrimitive.Content>
        </div>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

/**
 * One command row.
 *
 * Split out so each row can own the `scrollIntoView` effect that keeps the
 * highlight visible while ↑/↓ walk past the fold.
 *
 * Focus never leaves the search input, so this is an ARIA `option` driven by
 * `aria-activedescendant`, not a button. The 2px left rail is present at rest in
 * `transparent` for the same reason list rows carry one (§1): the highlight must
 * not shift the label sideways. The 6px accent square marks the current setting
 * (§7 uses a filled square, never a checkmark) and its slot is always reserved,
 * so labels align whether or not a row is a radio entry.
 */
function CommandRow(props: {
  command: PaletteCommand;
  active: boolean;
  onActivate: () => void;
  onRun: () => void;
}): JSX.Element {
  let el: HTMLDivElement | undefined;

  createEffect(() => {
    if (props.active) el?.scrollIntoView({ block: "nearest" });
  });

  return (
    <div
      ref={el}
      id={`lp-cmd-${props.command.id}`}
      role="option"
      aria-selected={props.active}
      onMouseMove={() => props.onActivate()}
      onClick={() => props.onRun()}
      class={cn(
        "text-item flex h-8 cursor-pointer select-none items-center gap-2 border-l-2 px-4",
        "transition-colors duration-150 motion-reduce:transition-none",
        props.active
          ? "border-l-[var(--lp-accent)] bg-[#5e6ad2]/10 text-[var(--lp-text)]"
          : "border-l-transparent text-[var(--lp-text-2)]",
      )}
    >
      <span class="flex h-1.5 w-1.5 shrink-0 items-center justify-center">
        <Show when={props.command.isCurrent?.()}>
          <span class="h-1.5 w-1.5 bg-[var(--lp-accent)]" aria-hidden="true" />
        </Show>
      </span>
      <span class="min-w-0 flex-1 truncate">{props.command.label}</span>
      <Show when={props.command.keys}>
        <Keycap>{props.command.keys}</Keycap>
      </Show>
    </div>
  );
}
