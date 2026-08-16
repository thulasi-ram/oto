/**
 * solid-ui's Select (https://solid-ui.com/docs/components/select), ported
 * onto `@kobalte/core/select`.
 *
 * This is a full Kobalte listbox/combobox, not a native `<select>` — a
 * deliberate choice here, distinct from `primitives.tsx`'s native-`<select>`
 * `Select` (which stays put; see that file's own header comment on why a
 * native element wins when one exists). Reach for this one where Kobalte's
 * compound listbox semantics (custom item rendering, `SelectValue` render
 * props, built-in `data-[invalid]`) are worth the extra pieces.
 *
 * No renaming was needed the way `Modal.tsx` renamed Dialog's parts: nothing
 * else under `components/ui/` is named `Select`/`SelectTrigger`/`SelectContent`/etc.
 * — this file and `primitives.tsx` simply live at different import paths.
 *
 * Retheme notes (see `Button.tsx`/`TextField.tsx`/`DropdownMenu.tsx` for the
 * fuller rationale, condensed here):
 *   - `border-input`/`bg-transparent` (trigger) -> `border-line-strong`/`bg-surface`,
 *     matching `TextFieldInput`.
 *   - `h-10` -> `h-8`, the control height every other port in this app uses.
 *   - `text-sm`/`text-xs` -> `text-item`/`text-meta`; `rounded-md` -> `rounded-control`
 *     (trigger, item) / `rounded-surface` (content — a panel, not a control).
 *   - `ring-offset-background focus:ring-2 ring-ring ring-offset-2` dropped —
 *     the global `:focus-visible` rule already rings the trigger (U7).
 *   - `disabled:opacity-50` -> `disabled:opacity-45`, `disabled:bg-sunken` added,
 *     matching `TextFieldInput`'s disabled treatment.
 *   - `bg-popover text-popover-foreground` -> `bg-surface text-ink`.
 *   - `animate-in fade-in-80` dropped (depends on a plugin this app doesn't
 *     install) -> `data-[expanded]:oto-enter`, the same one-shot entrance
 *     `Popover.tsx`/`DropdownMenu.tsx`/`Modal.tsx` use.
 *   - item: `focus:bg-accent focus:text-accent-foreground` (shadcn's neutral
 *     hover, name-collides with oto's *brand* `accent`) -> `focus:bg-raised focus:text-ink`;
 *     `data-[disabled]:opacity-50` -> `opacity-45`.
 *   - label variants: `text-sm`/`text-xs` -> `text-body`/`text-meta`;
 *     `data-[invalid]:text-destructive` / `text-destructive` (error) -> both
 *     stay `text-ink` — a validation error is not an alert *state* (§M.2 Tier B
 *     is state-only), exactly `TextField.tsx`'s reasoning.
 */
import type { JSX, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import * as SelectPrimitive from "@kobalte/core/select";
import { cva } from "class-variance-authority";

import { cn } from "~/lib/cn";

export const Select = SelectPrimitive.Root;
export const SelectValue = SelectPrimitive.Value;
export const SelectHiddenSelect = SelectPrimitive.HiddenSelect;

type SelectTriggerProps<T extends ValidComponent = "button"> =
  SelectPrimitive.SelectTriggerProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const SelectTrigger = <T extends ValidComponent = "button">(
  props: PolymorphicProps<T, SelectTriggerProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectTriggerProps, ["class", "children"]);
  return (
    <SelectPrimitive.Trigger
      class={cn(
        "flex h-8 w-full items-center justify-between rounded-control border border-line " +
          "bg-surface px-sm text-item text-ink " +
          "disabled:cursor-not-allowed disabled:bg-sunken disabled:opacity-45 " +
          "data-[invalid]:border-line-strong data-[invalid]:ring-1 data-[invalid]:ring-accent-border",
        local.class,
      )}
      {...others}
    >
      {local.children}
      <SelectPrimitive.Icon
        as="svg"
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
        class="size-4 shrink-0 opacity-50"
      >
        <path d="M8 9l4 -4l4 4" />
        <path d="M16 15l-4 4l-4 -4" />
      </SelectPrimitive.Icon>
    </SelectPrimitive.Trigger>
  );
};

type SelectContentProps<T extends ValidComponent = "div"> = SelectPrimitive.SelectContentProps<T> & {
  class?: string | undefined;
};

export const SelectContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, SelectContentProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectContentProps, ["class"]);
  return (
    <SelectPrimitive.Portal>
      <SelectPrimitive.Content
        class={cn(
          "relative z-50 min-w-32 overflow-hidden rounded-surface border border-line bg-surface " +
            "text-ink shadow-md data-[expanded]:oto-enter",
          local.class,
        )}
        {...others}
      >
        <SelectPrimitive.Listbox class="m-0 p-1" />
      </SelectPrimitive.Content>
    </SelectPrimitive.Portal>
  );
};

type SelectItemProps<T extends ValidComponent = "li"> = SelectPrimitive.SelectItemProps<T> & {
  class?: string | undefined;
  children?: JSX.Element;
};

export const SelectItem = <T extends ValidComponent = "li">(
  props: PolymorphicProps<T, SelectItemProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectItemProps, ["class", "children"]);
  return (
    <SelectPrimitive.Item
      class={cn(
        "relative mt-0 flex w-full cursor-default select-none items-center rounded-control py-1.5 " +
          "pl-2 pr-8 text-item text-ink outline-none transition-colors " +
          "focus:bg-raised focus:text-ink " +
          "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
        local.class,
      )}
      {...others}
    >
      <SelectPrimitive.ItemIndicator class="absolute right-2 flex size-3.5 items-center justify-center">
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
      </SelectPrimitive.ItemIndicator>
      <SelectPrimitive.ItemLabel>{local.children}</SelectPrimitive.ItemLabel>
    </SelectPrimitive.Item>
  );
};

const labelVariants = cva(
  "text-body font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70",
  {
    variants: {
      variant: {
        label: "text-ink-muted data-[invalid]:text-ink",
        description: "font-normal text-ink-subtle text-meta",
        error: "text-meta font-medium text-ink",
      },
    },
    defaultVariants: {
      variant: "label",
    },
  },
);

type SelectLabelProps<T extends ValidComponent = "label"> = SelectPrimitive.SelectLabelProps<T> & {
  class?: string | undefined;
};

export const SelectLabel = <T extends ValidComponent = "label">(
  props: PolymorphicProps<T, SelectLabelProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectLabelProps, ["class"]);
  return <SelectPrimitive.Label class={cn(labelVariants(), local.class)} {...others} />;
};

type SelectDescriptionProps<T extends ValidComponent = "div"> =
  SelectPrimitive.SelectDescriptionProps<T> & {
    class?: string | undefined;
  };

export const SelectDescription = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, SelectDescriptionProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectDescriptionProps, ["class"]);
  return (
    <SelectPrimitive.Description
      class={cn(labelVariants({ variant: "description" }), local.class)}
      {...others}
    />
  );
};

type SelectErrorMessageProps<T extends ValidComponent = "div"> =
  SelectPrimitive.SelectErrorMessageProps<T> & {
    class?: string | undefined;
  };

export const SelectErrorMessage = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, SelectErrorMessageProps<T>>,
) => {
  const [local, others] = splitProps(props as SelectErrorMessageProps, ["class"]);
  return (
    <SelectPrimitive.ErrorMessage
      class={cn(labelVariants({ variant: "error" }), local.class)}
      {...others}
    />
  );
};
