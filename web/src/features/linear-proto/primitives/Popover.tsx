/**
 * Popover on `@kobalte/core/popover`, styled per SPEC.md §5 — the container for
 * arbitrary content (filter multi-selects, date pickers) as opposed to `Menu`,
 * which owns lists of actions.
 *
 * Same portal caveat as `Menu`: `lp-portal` re-declares the `--lp-*` vars on the
 * content because Kobalte mounts it at `document.body`, outside the shell.
 */
import type { Component, JSX } from "solid-js";

import * as PopoverPrimitive from "@kobalte/core/popover";

import { cn } from "~/lib/cn";

export const Popover: Component<{
  trigger: JSX.Element;
  children: JSX.Element;
  open?: boolean;
  onOpenChange?: (v: boolean) => void;
  placement?: "bottom-start" | "bottom-end";
  class?: string;
}> = (props) => {
  /*
   * Optional keys are omitted rather than passed as `undefined`: that is both
   * what `exactOptionalPropertyTypes` demands and how Kobalte detects that the
   * popover is uncontrolled. Spreading a call expression keeps reactivity.
   */
  const rootProps = (): PopoverPrimitive.PopoverRootProps => {
    const base: PopoverPrimitive.PopoverRootProps = {
      placement: props.placement ?? "bottom-start",
      gutter: 4,
    };
    if (props.open !== undefined) base.open = props.open;
    if (props.onOpenChange !== undefined) base.onOpenChange = props.onOpenChange;
    return base;
  };

  return (
    <PopoverPrimitive.Root {...rootProps()}>
      <PopoverPrimitive.Trigger
        class={cn(
          "inline-flex items-center outline-none",
          // Named properties only — `transition-all` on a trigger that the side
          // panel stretches to full width would animate its geometry too.
          "transition-colors duration-150 ease-[var(--lp-ease)]",
          "motion-reduce:transition-none",
          "text-[var(--lp-text-2)]",
          "hover:text-[var(--lp-text)] data-[expanded]:text-[var(--lp-text)]",
          /*
           * No `active:scale` and no hit-area `::before` here, unlike `Menu`:
           * this trigger's callers are the side panel's full-width `h-8`
           * disclosure rows. A 3% scale there is a 7px wobble that breaks the
           * panel's edge alignment, and a vertical cushion would reach into the
           * row above and below. Both corrections apply to small glyph-sized
           * controls; this one is neither.
           */
        )}
      >
        {props.trigger}
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        <PopoverPrimitive.Content
          class={cn(
            "lp-portal z-50 min-w-44 rounded-none p-1 outline-none",
            // Depth is borders-only: this ONE hairline, no cast shadow and no
            // ring shadow behind it doubling the edge to 2px.
            "border border-[var(--lp-border)] bg-[var(--lp-overlay)]",
            "text-item text-[var(--lp-text)]",
            // Origin-aware, so the panel unfolds out of its disclosure row.
            "origin-[var(--kb-popover-content-transform-origin)]",
            "data-[expanded]:animate-[lp-overlay-in_150ms_var(--lp-ease)]",
            "data-[closed]:animate-[lp-overlay-out_100ms_var(--lp-ease)]",
            "motion-reduce:animate-none",
            props.class,
          )}
        >
          {props.children}
        </PopoverPrimitive.Content>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  );
};
