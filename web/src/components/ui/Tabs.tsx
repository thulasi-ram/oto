/**
 * solid-ui's Tabs (https://solid-ui.com/docs/components/tabs), ported onto
 * `@kobalte/core/tabs`.
 *
 * Retheme notes:
 *   - `bg-muted`/`text-muted-foreground` (track) -> `bg-sunken`/`text-ink-muted`.
 *   - `data-[selected]:bg-background data-[selected]:text-foreground` -> `bg-surface`/`text-ink`.
 *   - `rounded-md`/`rounded-sm` -> `rounded-control`; `text-sm` -> `text-item`
 *     (see `Button.tsx` for why the Tailwind builtin ladder is off-limits here).
 *   - `focus-visible:ring-*`/`ring-offset-background` dropped — the global
 *     `:focus-visible` rule already rings every focusable control (U7).
 *   - `TabsIndicator`'s upstream class list carries a `duration-250ms` token,
 *     which is not a real Tailwind utility (should have been an arbitrary
 *     value or a step off the `duration-*` scale) and was dropped rather than
 *     carried over as dead weight.
 */
import type { ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import * as TabsPrimitive from "@kobalte/core/tabs";

import { cn } from "~/lib/cn";

export const Tabs = TabsPrimitive.Root;

type TabsListProps<T extends ValidComponent = "div"> = TabsPrimitive.TabsListProps<T> & {
  class?: string | undefined;
};

export const TabsList = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TabsListProps<T>>,
) => {
  const [local, others] = splitProps(props as TabsListProps, ["class"]);
  return (
    <TabsPrimitive.List
      class={cn(
        "inline-flex h-8 items-center justify-center gap-0.5 rounded-control bg-sunken p-1 text-ink-muted",
        local.class,
      )}
      {...others}
    />
  );
};

type TabsTriggerProps<T extends ValidComponent = "button"> = TabsPrimitive.TabsTriggerProps<T> & {
  class?: string | undefined;
};

export const TabsTrigger = <T extends ValidComponent = "button">(
  props: PolymorphicProps<T, TabsTriggerProps<T>>,
) => {
  const [local, others] = splitProps(props as TabsTriggerProps, ["class"]);
  return (
    <TabsPrimitive.Trigger
      class={cn(
        "inline-flex items-center justify-center whitespace-nowrap rounded-control px-3 py-1 " +
          "text-item font-medium transition-all disabled:pointer-events-none disabled:opacity-45 " +
          "data-[selected]:bg-surface data-[selected]:text-ink data-[selected]:shadow-sm",
        local.class,
      )}
      {...others}
    />
  );
};

type TabsContentProps<T extends ValidComponent = "div"> = TabsPrimitive.TabsContentProps<T> & {
  class?: string | undefined;
};

export const TabsContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TabsContentProps<T>>,
) => {
  const [local, others] = splitProps(props as TabsContentProps, ["class"]);
  return <TabsPrimitive.Content class={cn("mt-2", local.class)} {...others} />;
};

type TabsIndicatorProps<T extends ValidComponent = "div"> = TabsPrimitive.TabsIndicatorProps<T> & {
  class?: string | undefined;
};

export const TabsIndicator = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TabsIndicatorProps<T>>,
) => {
  const [local, others] = splitProps(props as TabsIndicatorProps, ["class"]);
  return (
    <TabsPrimitive.Indicator
      class={cn(
        "absolute transition-all " +
          "data-[orientation=horizontal]:-bottom-px data-[orientation=horizontal]:h-[2px] " +
          "data-[orientation=vertical]:-right-px data-[orientation=vertical]:w-[2px] " +
          "bg-accent",
        local.class,
      )}
      {...others}
    />
  );
};
