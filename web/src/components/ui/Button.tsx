/**
 * solid-ui's Button (https://solid-ui.com/docs/components/button), ported
 * onto `@kobalte/core/button` and re-themed onto oto's own token set.
 *
 * The structure — a polymorphic `Root` plus a `cva` variant map — is
 * unchanged from the registry. What changed is every class that named a
 * shadcn/Tailwind token with no equivalent here:
 *
 *   - `bg-primary`/`text-primary-foreground`      -> `bg-accent`/`text-ink-inverse`
 *   - `bg-secondary`/`text-secondary-foreground`  -> `bg-surface`/`text-ink` (Tier A)
 *   - `bg-destructive`/`text-destructive-foreground` -> the app's existing
 *     "danger" treatment (the old hand-rolled `Button`'s danger variant, since
 *     retired): weight and a strong border, never a Tier-B state hue, because
 *     §M.2 reserves saturated colour for alert state alone.
 *   - `hover:bg-accent hover:text-accent-foreground` (shadcn's neutral hover
 *     surface, name-collides with oto's *brand* `accent`) -> `hover:bg-raised`
 *   - `rounded-md` -> `rounded-control`; `text-sm`/`text-xs` -> `text-item`/`text-body`
 *     (index.css's `@theme` only publishes oto's own type/radius scale —
 *     `design/scales.test.ts` rejects Tailwind's builtin ladder outright)
 *   - `focus-visible:ring-2 ring-ring ring-offset-2` dropped entirely: every
 *     focusable element already gets a 2px ring from the global
 *     `:focus-visible` rule in `index.css` (U7), so a component-local ring
 *     would just double it up.
 *
 * `busy` is a deliberate app-specific extension beyond solid-ui's stock
 * Button, ported forward from `primitives.tsx`'s `Button` (see that file's
 * own header comment, lines ~60-92) so the many call sites depending on this
 * exact contract migrate safely: `busy` disables the button in addition to
 * any existing `disabled`, sets `aria-busy="true"`, and renders `Spinner`
 * before `children` without changing the button's width (the spinner is
 * inline content, not layout it reflows around). `Spinner` itself — a 12px
 * indeterminate spin with a reduced-motion fallback to a static dot — is
 * ported byte-for-byte from the same file, onto oto's own scale rather than
 * a raw Tailwind size (there was never a raw size to begin with: `size-3` is
 * already one of Tailwind's spacing steps, not the banned font/radius ladder).
 */
import type { Component, JSX, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import * as ButtonPrimitive from "@kobalte/core/button";
import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import type { VariantProps } from "class-variance-authority";
import { cva } from "class-variance-authority";

import { cn } from "~/lib/cn";

/** A 12 px indeterminate spinner. Under reduced motion it stops and shows a dot. */
export const Spinner: Component<{ readonly class?: string }> = (props) => (
  <span
    aria-hidden="true"
    class={cn(
      "inline-block size-3 shrink-0 rounded-full border-[1.5px] border-current",
      "border-r-transparent motion-safe:animate-spin motion-reduce:border-r-current",
      "motion-reduce:opacity-60",
      props.class,
    )}
  />
);

export const buttonVariants = cva(
  "inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-control border " +
    "font-medium transition-colors duration-100 " +
    "disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-45 " +
    "[&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "border-transparent bg-accent text-ink-inverse hover:bg-accent-hover",
        secondary: "border-line-strong bg-surface text-ink hover:bg-raised",
        destructive: "border-line-strong bg-surface text-ink hover:bg-sunken font-semibold",
        outline: "border-line-strong bg-transparent text-ink hover:bg-raised",
        ghost: "border-transparent bg-transparent text-ink-muted hover:bg-raised hover:text-ink",
        link: "border-transparent text-accent underline-offset-4 hover:underline",
      },
      size: {
        default: "h-8 px-3 text-item",
        sm: "h-7 px-2 text-body",
        lg: "h-9 px-4 text-item",
        icon: "size-8",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  },
);

export type ButtonProps<T extends ValidComponent = "button"> = ButtonPrimitive.ButtonRootProps<T> &
  VariantProps<typeof buttonVariants> & {
    class?: string | undefined;
    children?: JSX.Element;
    /** Renders a spinner and blocks the click without changing the button's width. */
    busy?: boolean;
  };

export const Button = <T extends ValidComponent = "button">(
  props: PolymorphicProps<T, ButtonProps<T>>,
) => {
  const [local, others] = splitProps(props as ButtonProps, [
    "variant",
    "size",
    "class",
    "busy",
    "disabled",
    "children",
  ]);
  return (
    <ButtonPrimitive.Root
      aria-busy={local.busy === true ? "true" : undefined}
      disabled={local.disabled === true || local.busy === true}
      class={cn(buttonVariants({ variant: local.variant, size: local.size }), local.class)}
      {...others}
    >
      {local.busy === true ? <Spinner /> : null}
      {local.children}
    </ButtonPrimitive.Root>
  );
};
