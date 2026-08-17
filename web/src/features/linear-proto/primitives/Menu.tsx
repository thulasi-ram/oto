/**
 * Dropdown menu on `@kobalte/core/dropdown-menu`, styled per SPEC.md §5:
 * `--lp-overlay` fill, 1px `--lp-border`, radius 0, 4px of internal padding,
 * `h-7 px-2 text-item` items, right-aligned keycap for shortcuts.
 *
 * Elevation is a single 1px ring and nothing else — no cast shadow anywhere in
 * the prototype. On a `#0f0f11` canvas a blur reads as smudge, not as height;
 * the edge is what separates the surface from the page.
 *
 * This is the single overlay-with-actions primitive for the prototype — it
 * absorbs the roles the old `DropdownMenu.tsx` and `Select.tsx` wrappers split
 * between them, so feature code never touches Kobalte directly (SPEC.md §9).
 *
 * `lp-portal` on the content is load-bearing: Kobalte portals overlays to
 * `document.body`, outside the `.linear-proto` shell, so the `--lp-*` vars have
 * to be re-declared on the content element or every colour resolves to nothing.
 */
import type { Component, JSX } from "solid-js";
import { Show } from "solid-js";

import * as DropdownMenuPrimitive from "@kobalte/core/dropdown-menu";

import { cn } from "~/lib/cn";
import { Keycap } from "~/features/linear-proto/primitives/Keycap";

export const Menu: Component<{
  trigger: JSX.Element;
  children: JSX.Element;
  open?: boolean;
  onOpenChange?: (v: boolean) => void;
  placement?: "bottom-start" | "bottom-end";
  class?: string;
  /**
   * Roving-tabindex passthrough (SPEC.md §6). The trigger renders a real
   * `<button>`, so a caller that owns one tab stop for a whole list — three
   * menus per row, sixty rows — needs to be able to take it out of the
   * sequence and hand it back only to the focused row.
   */
  triggerTabIndex?: number;
}> = (props) => {
  /*
   * `exactOptionalPropertyTypes` forbids handing Kobalte an explicit
   * `open={undefined}`, and an undefined `open` is exactly how it decides the
   * menu is uncontrolled — so the optional keys are omitted rather than passed.
   * Spreading a call expression keeps Solid's reactivity (the compiler wraps it
   * in a thunk for `mergeProps`).
   */
  const rootProps = (): DropdownMenuPrimitive.DropdownMenuRootProps => {
    const base: DropdownMenuPrimitive.DropdownMenuRootProps = {
      placement: props.placement ?? "bottom-start",
      gutter: 4,
    };
    if (props.open !== undefined) base.open = props.open;
    if (props.onOpenChange !== undefined) base.onOpenChange = props.onOpenChange;
    return base;
  };

  return (
    <DropdownMenuPrimitive.Root {...rootProps()}>
      <DropdownMenuPrimitive.Trigger
        tabindex={props.triggerTabIndex}
        class={cn(
          "inline-flex items-center justify-center outline-none",
          // Named properties, never `transition-all`: `all` would also animate
          // the layout box, and these triggers live inside a fixed-width cell.
          "transition-[color,transform] duration-150 ease-[var(--lp-ease)]",
          "motion-reduce:transition-none",
          "text-[var(--lp-text-2)]",
          "hover:text-[var(--lp-text)] data-[expanded]:text-[var(--lp-text)]",
          // Press feedback. No hit-area `::before` here: the trigger's box is
          // supplied by the caller, and `IssueRow` already cushions its own
          // action buttons to the midpoint of their 4px gap. A second cushion
          // here would make adjacent buttons overlap.
          "active:scale-[0.97]",
        )}
      >
        {props.trigger}
      </DropdownMenuPrimitive.Trigger>
      <DropdownMenuPrimitive.Portal>
        <DropdownMenuPrimitive.Content
          class={cn(
            "lp-portal z-50 min-w-44 rounded-none p-1 outline-none",
            /*
             * Depth is borders-only. This ONE hairline is the whole of
             * dark-mode elevation — no blur, no offset, nothing that casts, and
             * no ring shadow doubling it into a 2px edge.
             */
            "border border-[var(--lp-border)] bg-[var(--lp-overlay)]",
            "text-item text-[var(--lp-text)]",
            /*
             * Origin-aware: Kobalte writes the trigger-relative origin onto the
             * content element, so the menu unfolds out of the button that owns
             * it instead of pulsing from its own centre. Enter 150ms, exit 100ms
             * — leaving should feel like a dismissal, not a second animation.
             */
            "origin-[var(--kb-menu-content-transform-origin)]",
            "data-[expanded]:animate-[lp-overlay-in_150ms_var(--lp-ease)]",
            "data-[closed]:animate-[lp-overlay-out_100ms_var(--lp-ease)]",
            "motion-reduce:animate-none",
            props.class,
          )}
        >
          {props.children}
        </DropdownMenuPrimitive.Content>
      </DropdownMenuPrimitive.Portal>
    </DropdownMenuPrimitive.Root>
  );
};

export const MenuItem: Component<{
  children: JSX.Element;
  onSelect?: () => void;
  shortcut?: string;
  selected?: boolean;
  class?: string;
}> = (props) => (
  <DropdownMenuPrimitive.Item
    class={cn(
      "flex h-7 cursor-default select-none items-center gap-2 rounded-none px-2",
      "text-item text-[var(--lp-text)] outline-none",
      /*
       * No `active:scale` on items, unlike the trigger: a 176px-wide row
       * shrinking 3% is a 5px wobble across the whole menu, and the item
       * already answers the press with its highlight. Press feedback is for
       * controls small enough that the scale reads as a push.
       */
      "transition-colors duration-150 ease-[var(--lp-ease)] motion-reduce:transition-none",
      "focus:bg-white/[0.06] data-[highlighted]:bg-white/[0.06]",
      "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
      props.class,
    )}
    onSelect={() => props.onSelect?.()}
  >
    <span class="flex min-w-0 flex-1 items-center gap-2 truncate">{props.children}</span>
    {/*
      Selection is a filled 6px accent square, never a checkmark — the check
      belongs to the Done status glyph and nowhere else (SPEC.md §7).
    */}
    <Show when={props.selected}>
      <span class="h-1.5 w-1.5 shrink-0 bg-[var(--lp-accent)]" aria-hidden="true" />
    </Show>
    <Show when={props.shortcut}>
      {(shortcut) => <Keycap class="ml-auto shrink-0">{shortcut()}</Keycap>}
    </Show>
  </DropdownMenuPrimitive.Item>
);

export const MenuLabel: Component<{ children: JSX.Element }> = (props) => (
  <div class="text-micro font-medium uppercase tracking-[0.08em] flex h-7 items-center px-2 text-[var(--lp-text-3)] select-none">
    {props.children}
  </div>
);

export const MenuSeparator: Component = () => (
  <DropdownMenuPrimitive.Separator class="my-1 h-px border-0 bg-[var(--lp-border)]" />
);
