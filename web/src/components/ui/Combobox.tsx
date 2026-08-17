/**
 * A listbox you can type into — `@kobalte/core/combobox`, themed the way
 * `Select.tsx` themes `@kobalte/core/select`.
 *
 * ⭐ THE DIFFERENCE FROM `Select` IS THE SEARCH BOX, AND NOTHING ELSE. Kobalte
 * filters `options` itself (`defaultFilter`, `"contains"` by default) against
 * whatever is typed into `ComboboxInput`, so a picker whose list is short enough
 * to read at a glance should stay a `Select`; this one is for a list whose length
 * an operator does not control — every channel in the org, say — where the honest
 * interaction is "type the two characters you remember".
 *
 * Retheme notes are `Select.tsx`'s, applied to the parts Combobox has instead:
 *   - The *control* carries the border, height and `data-[invalid]` ring that
 *     `SelectTrigger` carries there, because here the trigger is a chevron button
 *     inside it rather than the whole surface. `h-8` is a MINIMUM (`min-h-8`) on
 *     the multiple-selection control, which grows to hold the chips of what is
 *     already picked; the single-selection one is the same 32px row as a
 *     `TextFieldInput`.
 *   - `ComboboxInput` is deliberately unbordered and transparent: it sits inside
 *     the control, and a second border around a box inside a box is the "boxed
 *     group" `rhythm.ts`'s LEGEND note argues against at a larger scale.
 *   - The global `:focus-visible` rule (U7) rings the input; nothing here adds a
 *     ring of its own. The control lifts its own hairline instead when focus is
 *     inside it, so the whole object reads as focused rather than just the 1px
 *     caret line.
 *   - Content, item, indicator and the three label variants are `Select.tsx`'s
 *     verbatim — the two pickers appear in the same dialog and must not read as
 *     two different products.
 *
 * ⛔ `ComboboxContent` PORTALS, exactly as `SelectContent` does, and the `Portal`
 * is not decoration: it is what carries Kobalte's `contentPresent()` gate. Drop
 * it and the listbox is in the document from first render — every option of every
 * closed picker permanently in the accessibility tree. The cost is that the
 * options are not descendants of the dialog that owns the picker, so a test finds
 * them from `screen` rather than from the dialog element; `PoliciesSection.test.tsx`
 * already does exactly that for the `Select` beside this one.
 */
import type { JSX, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import * as ComboboxPrimitive from "@kobalte/core/combobox";
import { cva } from "class-variance-authority";

import { cn } from "~/lib/cn";

export const Combobox = ComboboxPrimitive.Root;
export const ComboboxHiddenSelect = ComboboxPrimitive.HiddenSelect;

type ComboboxControlProps<Option, T extends ValidComponent = "div"> = ComboboxPrimitive.ComboboxControlProps<
  Option,
  T
> & {
  class?: string | undefined;
};

/**
 * The bordered surface: what is already chosen, the search box, the chevron.
 *
 * `flex-wrap` and `min-h-8` rather than a fixed height: with `multiple`, the
 * caller renders a chip per selection in here, and a control that clipped them
 * would hide the answer to "what is this policy sending to" behind a scroll.
 */
export const ComboboxControl = <Option, T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ComboboxControlProps<Option, T>>,
) => {
  const [local, others] = splitProps(props as ComboboxControlProps<Option>, ["class"]);
  return (
    <ComboboxPrimitive.Control<Option>
      class={cn(
        "flex min-h-8 w-full flex-wrap items-center gap-2xs rounded-control border border-line " +
          "bg-surface px-2xs py-2xs text-item text-ink " +
          "focus-within:border-line-strong " +
          "data-[disabled]:cursor-not-allowed data-[disabled]:bg-sunken data-[disabled]:opacity-45 " +
          "data-[invalid]:border-line-strong data-[invalid]:ring-1 data-[invalid]:ring-accent-border",
        local.class,
      )}
      {...(others as object)}
    />
  );
};

type ComboboxInputProps<T extends ValidComponent = "input"> = ComboboxPrimitive.ComboboxInputProps<T> & {
  class?: string | undefined;
};

export const ComboboxInput = <T extends ValidComponent = "input">(
  props: PolymorphicProps<T, ComboboxInputProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxInputProps, ["class"]);
  return (
    <ComboboxPrimitive.Input
      class={cn(
        "min-w-24 flex-1 bg-transparent px-2xs py-0.5 text-item text-ink outline-none " +
          "placeholder:text-ink-subtle disabled:cursor-not-allowed",
        local.class,
      )}
      {...others}
    />
  );
};

type ComboboxTriggerProps<T extends ValidComponent = "button"> =
  ComboboxPrimitive.ComboboxTriggerProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const ComboboxTrigger = <T extends ValidComponent = "button">(
  props: PolymorphicProps<T, ComboboxTriggerProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxTriggerProps, ["class", "children"]);
  return (
    <ComboboxPrimitive.Trigger
      class={cn("ml-auto flex size-6 shrink-0 items-center justify-center text-ink-muted", local.class)}
      {...others}
    >
      {local.children ?? (
        <ComboboxPrimitive.Icon
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
        </ComboboxPrimitive.Icon>
      )}
    </ComboboxPrimitive.Trigger>
  );
};

type ComboboxContentProps<T extends ValidComponent = "div"> =
  ComboboxPrimitive.ComboboxContentProps<T> & {
    class?: string | undefined;
  };

export const ComboboxContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ComboboxContentProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxContentProps, ["class"]);
  return (
    <ComboboxPrimitive.Portal>
      <ComboboxPrimitive.Content
        class={cn(
          "relative z-50 min-w-32 overflow-hidden rounded-surface border border-line bg-surface " +
            "text-ink shadow-md data-[expanded]:oto-enter",
          local.class,
        )}
        {...others}
      >
        {/* Capped and scrollable: the list is as long as the org's channel list,
            and a listbox taller than the dialog would push its own footer off. */}
        <ComboboxPrimitive.Listbox class="m-0 max-h-56 overflow-y-auto p-1" />
      </ComboboxPrimitive.Content>
    </ComboboxPrimitive.Portal>
  );
};

type ComboboxItemProps<T extends ValidComponent = "li"> = ComboboxPrimitive.ComboboxItemProps<T> & {
  class?: string | undefined;
  children?: JSX.Element;
};

export const ComboboxItem = <T extends ValidComponent = "li">(
  props: PolymorphicProps<T, ComboboxItemProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxItemProps, ["class", "children"]);
  return (
    <ComboboxPrimitive.Item
      class={cn(
        "relative mt-0 flex w-full cursor-default select-none items-center rounded-control py-1.5 " +
          "pl-2 pr-8 text-item text-ink outline-none transition-colors " +
          "focus:bg-raised focus:text-ink " +
          "data-[disabled]:pointer-events-none data-[disabled]:opacity-45",
        local.class,
      )}
      {...others}
    >
      <ComboboxPrimitive.ItemIndicator class="absolute right-2 flex size-3.5 items-center justify-center">
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
      </ComboboxPrimitive.ItemIndicator>
      <ComboboxPrimitive.ItemLabel>{local.children}</ComboboxPrimitive.ItemLabel>
    </ComboboxPrimitive.Item>
  );
};

/** `Select.tsx`'s label variants, shared verbatim so the two agree. */
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

type ComboboxLabelProps<T extends ValidComponent = "label"> =
  ComboboxPrimitive.ComboboxLabelProps<T> & {
    class?: string | undefined;
  };

export const ComboboxLabel = <T extends ValidComponent = "label">(
  props: PolymorphicProps<T, ComboboxLabelProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxLabelProps, ["class"]);
  return <ComboboxPrimitive.Label class={cn(labelVariants(), local.class)} {...others} />;
};

type ComboboxDescriptionProps<T extends ValidComponent = "div"> =
  ComboboxPrimitive.ComboboxDescriptionProps<T> & {
    class?: string | undefined;
  };

export const ComboboxDescription = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ComboboxDescriptionProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxDescriptionProps, ["class"]);
  return (
    <ComboboxPrimitive.Description
      class={cn(labelVariants({ variant: "description" }), local.class)}
      {...others}
    />
  );
};

type ComboboxErrorMessageProps<T extends ValidComponent = "div"> =
  ComboboxPrimitive.ComboboxErrorMessageProps<T> & {
    class?: string | undefined;
  };

export const ComboboxErrorMessage = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ComboboxErrorMessageProps<T>>,
) => {
  const [local, others] = splitProps(props as ComboboxErrorMessageProps, ["class"]);
  return (
    <ComboboxPrimitive.ErrorMessage
      class={cn(labelVariants({ variant: "error" }), local.class)}
      {...others}
    />
  );
};
