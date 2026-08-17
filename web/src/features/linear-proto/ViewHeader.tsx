import { Show } from "solid-js";

import { cn } from "~/lib/cn";

import { GUTTER, HEADER_H } from "./layout";
import { Keycap } from "./primitives/Keycap";
import { useIssueView } from "./store";

/**
 * The top strip — SPEC §7.
 *
 * Breadcrumb, count and a shortcut hint. Deliberately holds NO filter, sort or
 * group control: all of that lives in <SidePanel>, and the whole point of the
 * side panel is that this bar stops being a dumping ground for uncategorised
 * controls. The only button here is the escape hatch that brings the panel
 * back once it has been collapsed to w-0 (its own collapse control goes with
 * it, so without this the panel could not be reopened by pointer).
 */
export function ViewHeader() {
  const view = useIssueView();

  const isFiltered = () => view.visibleCount() !== view.totalCount();

  return (
    <header
      class={cn(
        // Height and gutter come from layout.ts, which is the whole reason it
        // exists: the header, the group headers and the rows share one gutter,
        // and a hand-written `px-4` here is exactly how that alignment drifts.
        HEADER_H,
        GUTTER,
        "shrink-0 flex items-center gap-3",
        "border-b border-[var(--lp-border)] bg-[var(--lp-canvas)]",
      )}
    >
      <Show when={view.panelCollapsed()}>
        <button
          type="button"
          onClick={() => view.togglePanel()}
          aria-label="Expand side panel"
          title="Expand side panel"
          class={cn(
            "-ml-1 w-6 h-6 shrink-0 flex items-center justify-center",
            // 24px of paint, 40px of target: the ::before grows into the header's
            // own padding and the 12px gap before the breadcrumb, so it overlaps
            // nothing (the breadcrumb is text, and the header is h-11).
            "relative before:absolute before:content-[''] before:-inset-2",
            "text-[var(--lp-text-3)] outline-none active:scale-[0.97]",
            "transition-[color,background-color,transform] duration-150",
            "ease-[var(--lp-ease)] motion-reduce:transition-none",
            "hover:bg-white/[0.04] hover:text-[var(--lp-text)]",
            "focus-visible:bg-white/[0.06] focus-visible:text-[var(--lp-text)]",
          )}
        >
          <PanelGlyph />
        </button>
      </Show>

      <nav aria-label="Breadcrumb" class="min-w-0 flex items-baseline gap-1.5">
        <span class="text-item text-[var(--lp-text-3)] truncate">Workspace</span>
        <span aria-hidden="true" class="text-item text-[var(--lp-text-3)]">
          /
        </span>
        {/* Same 13px as the crumb before it: the leaf separates by weight and
            colour, not by size (SPEC §4). */}
        <span class="text-item font-medium text-[var(--lp-text)] truncate">Issues</span>
      </nav>

      <span aria-hidden="true" class="w-px h-3.5 shrink-0 bg-[var(--lp-border-strong)]" />

      {/* A count wears the same treatment everywhere in the prototype — the
          `text-micro` caps step with `tabular-nums`, as `GroupHeader` renders
          its own count. One role, one type style; the breadcrumb beside it is
          what carries reading size here. */}
      <p class="text-micro font-medium uppercase tracking-[0.08em] tabular-nums shrink-0 text-[var(--lp-text-2)]">
        <Show when={isFiltered()} fallback={<span>{view.totalCount()}</span>}>
          <span>{view.visibleCount()}</span>
          <span aria-hidden="true" class="mx-1 text-[var(--lp-text-3)]">
            /
          </span>
          <span class="text-[var(--lp-text-3)]">{view.totalCount()}</span>
        </Show>
        <span class="ml-1.5 text-[var(--lp-text-3)]">issues</span>
      </p>

      <div class="flex-1" />

      <span class="text-meta shrink-0 flex items-center gap-1.5 text-[var(--lp-text-3)]">
        <span>Search</span>
        <Keycap>⌘K</Keycap>
      </span>
    </header>
  );
}

/** Side-panel affordance: a rectangle divided by its own left rail. */
function PanelGlyph() {
  return (
    <svg
      viewBox="0 0 14 14"
      aria-hidden="true"
      class="w-3.5 h-3.5"
      fill="none"
      stroke="currentColor"
      stroke-width="1.5"
    >
      <rect x="1.75" y="2.5" width="10.5" height="9" />
      <path d="M5.5 2.5v9" />
    </svg>
  );
}
