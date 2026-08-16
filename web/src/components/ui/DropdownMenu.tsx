/**
 * solid-ui's DropdownMenu (https://solid-ui.com/docs/components/dropdown-menu),
 * ported onto `@kobalte/core/dropdown-menu`.
 *
 * Retheme notes (see `Button.tsx`/`Popover.tsx` for the fuller rationale):
 *   - `bg-popover`/`text-popover-foreground` -> `bg-surface`/`text-ink`.
 *   - `rounded-md`/`rounded-sm` -> `rounded-surface` (menu panel) /
 *     `rounded-control` (item) per the radius tiers in `design/tokens.css`.
 *   - `text-sm` -> `text-item`; `text-xs` (shortcut) -> `text-meta`.
 *   - `focus:bg-accent focus:text-accent-foreground` (shadcn's neutral hover,
 *     name-collides with oto's brand `accent`) -> `focus:bg-raised focus:text-ink`.
 *   - `bg-muted` (separator) -> `bg-line`.
 *   - The `animate-in`/`animate-content-show`/`animate-content-hide` classes
 *     depend on plugins this app doesn't install; swapped for the app's own
 *     `oto-enter` one-shot (see `Popover.tsx`).
 */
import type { Component, ComponentProps, JSX, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import * as DropdownMenuPrimitive from "@kobalte/core/dropdown-menu";
import type { PolymorphicProps } from "@kobalte/core/polymorphic";

import { cn } from "~/lib/cn";

export const DropdownMenuTrigger = DropdownMenuPrimitive.Trigger;
export const DropdownMenuPortal = DropdownMenuPrimitive.Portal;
export const DropdownMenuSub = DropdownMenuPrimitive.Sub;
export const DropdownMenuGroup = DropdownMenuPrimitive.Group;
export const DropdownMenuRadioGroup = DropdownMenuPrimitive.RadioGroup;

export const DropdownMenu: Component<DropdownMenuPrimitive.DropdownMenuRootProps> = (props) => {
  return <DropdownMenuPrimitive.Root gutter={4} {...props} />;
};

type DropdownMenuContentProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuContentProps<T> & {
    class?: string | undefined;
  };

export const DropdownMenuContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuContentProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuContentProps, ["class"]);
  return (
    <DropdownMenuPrimitive.Portal>
      <DropdownMenuPrimitive.Content
        class={cn(
          "z-50 min-w-32 origin-[var(--kb-menu-content-transform-origin)] overflow-hidden " +
            "rounded-surface border border-line bg-surface p-1 text-ink shadow-md " +
            "data-[expanded]:oto-enter",
          local.class,
        )}
        {...others}
      />
    </DropdownMenuPrimitive.Portal>
  );
};

type DropdownMenuItemProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuItemProps<T> & {
    class?: string | undefined;
  };

export const DropdownMenuItem = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuItemProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuItemProps, ["class"]);
  return (
    <DropdownMenuPrimitive.Item
      class={cn(
        "relative flex cursor-default select-none items-center gap-2 rounded-control px-2 py-1.5 " +
          "text-item text-ink outline-none transition-colors " +
          "focus:bg-raised focus:text-ink " +
          "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
        local.class,
      )}
      {...others}
    />
  );
};

export const DropdownMenuShortcut: Component<ComponentProps<"span">> = (props) => {
  const [local, others] = splitProps(props, ["class"]);
  return <span class={cn("ml-auto text-meta tracking-widest opacity-60", local.class)} {...others} />;
};

export const DropdownMenuLabel: Component<ComponentProps<"div"> & { inset?: boolean }> = (
  props,
) => {
  const [local, others] = splitProps(props, ["class", "inset"]);
  return (
    <div
      class={cn(
        "px-2 py-1.5 text-item font-semibold text-ink-muted",
        local.inset === true ? "pl-8" : "",
        local.class,
      )}
      {...others}
    />
  );
};

type DropdownMenuSeparatorProps<T extends ValidComponent = "hr"> =
  DropdownMenuPrimitive.DropdownMenuSeparatorProps<T> & {
    class?: string | undefined;
  };

export const DropdownMenuSeparator = <T extends ValidComponent = "hr">(
  props: PolymorphicProps<T, DropdownMenuSeparatorProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuSeparatorProps, ["class"]);
  return <DropdownMenuPrimitive.Separator class={cn("-mx-1 my-1 h-px bg-line", local.class)} {...others} />;
};

type DropdownMenuSubTriggerProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuSubTriggerProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const DropdownMenuSubTrigger = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuSubTriggerProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuSubTriggerProps, ["class", "children"]);
  return (
    <DropdownMenuPrimitive.SubTrigger
      class={cn(
        "flex cursor-default select-none items-center rounded-control px-2 py-1.5 text-item text-ink " +
          "outline-none focus:bg-raised data-[state=open]:bg-raised",
        local.class,
      )}
      {...others}
    >
      {local.children}
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
        class="ml-auto size-4"
      >
        <path d="M9 6l6 6l-6 6" />
      </svg>
    </DropdownMenuPrimitive.SubTrigger>
  );
};

type DropdownMenuSubContentProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuSubContentProps<T> & {
    class?: string | undefined;
  };

export const DropdownMenuSubContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuSubContentProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuSubContentProps, ["class"]);
  return (
    <DropdownMenuPrimitive.SubContent
      class={cn(
        "z-50 min-w-32 origin-[var(--kb-menu-content-transform-origin)] overflow-hidden " +
          "rounded-surface border border-line bg-surface p-1 text-ink shadow-md data-[expanded]:oto-enter",
        local.class,
      )}
      {...others}
    />
  );
};

type DropdownMenuCheckboxItemProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuCheckboxItemProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const DropdownMenuCheckboxItem = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuCheckboxItemProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuCheckboxItemProps, ["class", "children"]);
  return (
    <DropdownMenuPrimitive.CheckboxItem
      class={cn(
        "relative flex cursor-default select-none items-center rounded-control py-1.5 pl-8 pr-2 " +
          "text-item text-ink outline-none transition-colors " +
          "focus:bg-raised focus:text-ink " +
          "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
        local.class,
      )}
      {...others}
    >
      <span class="absolute left-2 flex size-3.5 items-center justify-center">
        <DropdownMenuPrimitive.ItemIndicator>
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
            class="size-4"
          >
            <path d="M5 12l5 5l10 -10" />
          </svg>
        </DropdownMenuPrimitive.ItemIndicator>
      </span>
      {local.children}
    </DropdownMenuPrimitive.CheckboxItem>
  );
};

type DropdownMenuGroupLabelProps<T extends ValidComponent = "span"> =
  DropdownMenuPrimitive.DropdownMenuGroupLabelProps<T> & {
    class?: string | undefined;
  };

export const DropdownMenuGroupLabel = <T extends ValidComponent = "span">(
  props: PolymorphicProps<T, DropdownMenuGroupLabelProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuGroupLabelProps, ["class"]);
  return (
    <DropdownMenuPrimitive.GroupLabel
      class={cn("px-2 py-1.5 text-item font-semibold text-ink-muted", local.class)}
      {...others}
    />
  );
};

type DropdownMenuRadioItemProps<T extends ValidComponent = "div"> =
  DropdownMenuPrimitive.DropdownMenuRadioItemProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const DropdownMenuRadioItem = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, DropdownMenuRadioItemProps<T>>,
) => {
  const [local, others] = splitProps(props as DropdownMenuRadioItemProps, ["class", "children"]);
  return (
    <DropdownMenuPrimitive.RadioItem
      class={cn(
        "relative flex cursor-default select-none items-center rounded-control py-1.5 pl-8 pr-2 " +
          "text-item text-ink outline-none transition-colors " +
          "focus:bg-raised focus:text-ink " +
          "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
        local.class,
      )}
      {...others}
    >
      <span class="absolute left-2 flex size-3.5 items-center justify-center">
        <DropdownMenuPrimitive.ItemIndicator>
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
            class="size-2 fill-current"
          >
            <path d="M12 12m-9 0a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />
          </svg>
        </DropdownMenuPrimitive.ItemIndicator>
      </span>
      {local.children}
    </DropdownMenuPrimitive.RadioItem>
  );
};
