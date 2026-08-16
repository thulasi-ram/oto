/**
 * solid-ui's Popover (https://solid-ui.com/docs/components/popover), ported
 * onto `@kobalte/core/popover`.
 *
 * Retheme notes:
 *   - `bg-popover`/`text-popover-foreground` -> `bg-surface`/`text-ink` (a
 *     floating panel is Tier A chrome, same as `Panel` in `primitives.tsx`).
 *   - `rounded-md` -> `rounded-surface` (this is a panel, not a control —
 *     see the radius-tier comment in `design/tokens.css` §M.8).
 *   - The upstream `data-[expanded]:animate-in data-[closed]:animate-out
 *     fade-in-0/fade-out-0 zoom-in-95/zoom-out-95` soup depends on the
 *     `tailwindcss-animate` plugin, which this app does not install (motion
 *     is a closed, enumerated vocabulary here — U9, see `index.css`). Swapped
 *     for the app's own one-shot entrance, `oto-enter`, whose reduced-motion
 *     suppression comes from `index.css`'s global `*` sweep rather than a
 *     component-local `motion-safe:` — the same choice `Dialog.tsx` makes.
 */
import type { Component, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import * as PopoverPrimitive from "@kobalte/core/popover";

import { cn } from "~/lib/cn";

export const PopoverTrigger = PopoverPrimitive.Trigger;

export const Popover: Component<PopoverPrimitive.PopoverRootProps> = (props) => {
  return <PopoverPrimitive.Root gutter={4} {...props} />;
};

type PopoverContentProps<T extends ValidComponent = "div"> =
  PopoverPrimitive.PopoverContentProps<T> & { class?: string | undefined };

export const PopoverContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, PopoverContentProps<T>>,
) => {
  const [local, others] = splitProps(props as PopoverContentProps, ["class"]);
  return (
    <PopoverPrimitive.Portal>
      <PopoverPrimitive.Content
        class={cn(
          "z-50 w-72 origin-[var(--kb-popover-content-transform-origin)] rounded-surface border " +
            "border-line bg-surface p-4 text-ink shadow-md outline-none data-[expanded]:oto-enter",
          local.class,
        )}
        {...others}
      />
    </PopoverPrimitive.Portal>
  );
};
