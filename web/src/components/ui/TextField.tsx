/**
 * solid-ui's TextField (https://solid-ui.com/docs/components/input), ported
 * onto `@kobalte/core/text-field`.
 *
 * Retheme notes (see `Button.tsx` for the same rationale, condensed here):
 *   - `border-input`/`bg-background`/`bg-transparent` -> `border-line-strong`/`bg-surface`,
 *     matching the hand-rolled `Input`/`Textarea` in `primitives.tsx`.
 *   - `text-sm` -> `text-item`; `text-xs` -> `text-meta`; `rounded-md` -> `rounded-control`.
 *   - `placeholder:text-muted-foreground` -> `placeholder:text-ink-subtle`.
 *   - `focus-visible:ring-*` dropped — the global `:focus-visible` rule owns focus (U7).
 *   - `data-[invalid]:border-error-foreground data-[invalid]:text-error-foreground` ->
 *     `data-[invalid]:border-line-strong data-[invalid]:ring-1 data-[invalid]:ring-accent-border`,
 *     i.e. exactly how `primitives.tsx`'s `Input` signals `aria-invalid` today. A
 *     validation error is not an alert *state* (§M.2 Tier B is state-only), so it
 *     stays inside Tier A rather than borrowing a red.
 *   - `min-h-[80px]` (a bracket, banned by `design/scales.test.ts`) -> `min-h-16`,
 *     the same textarea floor `primitives.tsx` already uses.
 *
 * This is a distinct component from `primitives.tsx`'s `Input`/`Textarea`/`Field` —
 * both now exist side by side. Prefer this one where Kobalte's compound
 * `TextField` semantics (built-in `data-[invalid]`, `TextFieldErrorMessage`,
 * paired label/description ids) are worth the extra pieces; prefer the
 * existing `Field` wrapper for a plain native input with a hand-managed error.
 */
import type { ValidComponent } from "solid-js";
import { mergeProps, splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core";
import * as TextFieldPrimitive from "@kobalte/core/text-field";
import { cva } from "class-variance-authority";

import { cn } from "~/lib/cn";

type TextFieldRootProps<T extends ValidComponent = "div"> =
  TextFieldPrimitive.TextFieldRootProps<T> & {
    class?: string | undefined;
  };

export const TextField = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TextFieldRootProps<T>>,
) => {
  const [local, others] = splitProps(props as TextFieldRootProps, ["class"]);
  return <TextFieldPrimitive.Root class={cn("flex flex-col gap-1", local.class)} {...others} />;
};

type TextFieldInputProps<T extends ValidComponent = "input"> =
  TextFieldPrimitive.TextFieldInputProps<T> & {
    class?: string | undefined;
    type?:
      | "button"
      | "checkbox"
      | "color"
      | "date"
      | "datetime-local"
      | "email"
      | "file"
      | "hidden"
      | "image"
      | "month"
      | "number"
      | "password"
      | "radio"
      | "range"
      | "reset"
      | "search"
      | "submit"
      | "tel"
      | "text"
      | "time"
      | "url"
      | "week";
  };

export const TextFieldInput = <T extends ValidComponent = "input">(
  rawProps: PolymorphicProps<T, TextFieldInputProps<T>>,
) => {
  const props = mergeProps<TextFieldInputProps<T>[]>({ type: "text" }, rawProps);
  const [local, others] = splitProps(props as TextFieldInputProps, ["type", "class"]);
  return (
    <TextFieldPrimitive.Input
      type={local.type}
      class={cn(
        "flex h-8 w-full rounded-control border border-line-strong bg-surface px-2 text-item text-ink " +
          "file:border-0 file:bg-transparent file:text-item file:font-medium " +
          "placeholder:text-ink-subtle " +
          "disabled:cursor-not-allowed disabled:bg-sunken disabled:opacity-60 " +
          "data-[invalid]:border-line-strong data-[invalid]:ring-1 data-[invalid]:ring-accent-border",
        local.class,
      )}
      {...others}
    />
  );
};

type TextFieldTextAreaProps<T extends ValidComponent = "textarea"> =
  TextFieldPrimitive.TextFieldTextAreaProps<T> & { class?: string | undefined };

export const TextFieldTextArea = <T extends ValidComponent = "textarea">(
  props: PolymorphicProps<T, TextFieldTextAreaProps<T>>,
) => {
  const [local, others] = splitProps(props as TextFieldTextAreaProps, ["class"]);
  return (
    <TextFieldPrimitive.TextArea
      class={cn(
        "flex min-h-16 w-full resize-y rounded-control border border-line-strong bg-surface px-2 py-1.5 " +
          "text-item leading-relaxed text-ink placeholder:text-ink-subtle " +
          "disabled:cursor-not-allowed disabled:bg-sunken disabled:opacity-60 " +
          "data-[invalid]:border-line-strong data-[invalid]:ring-1 data-[invalid]:ring-accent-border",
        local.class,
      )}
      {...others}
    />
  );
};

const labelVariants = cva("text-body font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70", {
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
});

type TextFieldLabelProps<T extends ValidComponent = "label"> =
  TextFieldPrimitive.TextFieldLabelProps<T> & { class?: string | undefined };

export const TextFieldLabel = <T extends ValidComponent = "label">(
  props: PolymorphicProps<T, TextFieldLabelProps<T>>,
) => {
  const [local, others] = splitProps(props as TextFieldLabelProps, ["class"]);
  return <TextFieldPrimitive.Label class={cn(labelVariants(), local.class)} {...others} />;
};

type TextFieldDescriptionProps<T extends ValidComponent = "div"> =
  TextFieldPrimitive.TextFieldDescriptionProps<T> & {
    class?: string | undefined;
  };

export const TextFieldDescription = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TextFieldDescriptionProps<T>>,
) => {
  const [local, others] = splitProps(props as TextFieldDescriptionProps, ["class"]);
  return (
    <TextFieldPrimitive.Description
      class={cn(labelVariants({ variant: "description" }), local.class)}
      {...others}
    />
  );
};

type TextFieldErrorMessageProps<T extends ValidComponent = "div"> =
  TextFieldPrimitive.TextFieldErrorMessageProps<T> & {
    class?: string | undefined;
  };

export const TextFieldErrorMessage = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, TextFieldErrorMessageProps<T>>,
) => {
  const [local, others] = splitProps(props as TextFieldErrorMessageProps, ["class"]);
  return (
    <TextFieldPrimitive.ErrorMessage
      class={cn(labelVariants({ variant: "error" }), local.class)}
      {...others}
    />
  );
};
