/**
 * solid-ui's Dialog (https://solid-ui.com/docs/components/dialog), ported
 * onto `@kobalte/core/dialog`.
 *
 * Named `Modal` rather than `Dialog` on purpose. This filesystem is
 * case-insensitive (macOS/APFS default) and `components/ui/Dialog.tsx`
 * already exists — the app's hand-rolled modal built on native
 * `<dialog showModal()>` (see that file's own header comment for why: free
 * focus trap, inert background, top layer, Escape-to-dismiss). The two now
 * split by area rather than by "standalone vs. nested": `Dialog.tsx` remains
 * the native-`<dialog>` implementation existing settings screens use, and
 * `Modal` is the solid-ui/Kobalte-based dialog primitive the alerts feature
 * area has standardized on — including its standalone confirm/edit modals
 * (SnoozeDialog, AckDialog, CommentDialog), none of which nest inside another
 * Kobalte overlay. Reach for `Modal` in the alerts area, or any new
 * solid-ui-based surface adopting the same conventions; leave existing
 * `Dialog.tsx` call sites alone rather than migrating them piecemeal.
 * Exported names are renamed in step — `Modal`, `ModalTrigger`,
 * `ModalContent`, `ModalHeader`, `ModalFooter`, `ModalTitle`,
 * `ModalDescription` — so importing both in the same file is never
 * ambiguous.
 *
 * Retheme notes (see `Button.tsx`/`Popover.tsx` for the fuller rationale):
 *   - `bg-background/80` overlay -> `bg-black/40`, matching the backdrop the
 *     native `Dialog.tsx` already draws via `::backdrop`.
 *   - `bg-background`/`border` panel -> `bg-surface`/`border-line`;
 *     `sm:rounded-lg` -> `rounded-surface` (unconditional, as `Dialog.tsx` does).
 *   - `text-lg` (title) -> `text-title`; `text-sm` (description) -> `text-body`.
 *   - The `animate-in`/`zoom-in-95`/`slide-in-from-*` soup depends on plugins
 *     this app doesn't install; swapped for `oto-enter` (see `Popover.tsx`).
 *   - `focus:ring-2 ring-ring ring-offset-2` on the close button dropped —
 *     the global `:focus-visible` rule already rings it (U7).
 *
 * ⛔ NO `max-w-lg`/`max-w-sm`/`max-w-md`/`max-w-xl` ANYWHERE IN THIS FILE, AND
 * NOT IN A CALLER'S `class` EITHER. `index.css`'s `@theme inline` block names
 * six spacing steps `2xs…xl` (`--spacing-sm`, `--spacing-lg`, …), and in
 * Tailwind v4 the `max-w-*` utility resolves a *named* key against the spacing
 * namespace before the container namespace. Registering the steps therefore
 * silently redefined the whole t-shirt width ladder: `max-w-lg` stopped being
 * `--container-lg` (32rem) and became `--oto-space-lg` — 16px — so this panel
 * compiled to a 50px-wide sliver with its text spilling out of both sides. The
 * width is stated as a spacing multiple (`max-w-128` == 32rem, the width this
 * panel has always had) because a multiple cannot be shadowed by a named step.
 *
 * Layout note, same incident: the panel is centred by the portal's flex box
 * rather than by `left-1/2 top-1/2 -translate-1/2` on the panel itself. That
 * lets the portal own the viewport gutter (`p-lg`), which `max-h-screen` on a
 * self-positioned panel could never leave, so a modal taller than the viewport
 * now scrolls *inside* a panel that stops short of both edges instead of
 * running flush off the top and bottom of the screen.
 */
import type { Component, ComponentProps, JSX, ValidComponent } from "solid-js";
import { splitProps } from "solid-js";

import * as DialogPrimitive from "@kobalte/core/dialog";
import type { PolymorphicProps } from "@kobalte/core/polymorphic";

import { cn } from "~/lib/cn";

export const Modal = DialogPrimitive.Root;
export const ModalTrigger = DialogPrimitive.Trigger;

const ModalPortal: Component<DialogPrimitive.DialogPortalProps> = (props) => {
  const [, rest] = splitProps(props, ["children"]);
  return (
    <DialogPrimitive.Portal {...rest}>
      <div class="fixed inset-0 z-50 flex items-start justify-center overflow-hidden p-lg sm:items-center">
        {props.children}
      </div>
    </DialogPrimitive.Portal>
  );
};

type ModalOverlayProps<T extends ValidComponent = "div"> = DialogPrimitive.DialogOverlayProps<T> & {
  class?: string | undefined;
};

const ModalOverlay = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ModalOverlayProps<T>>,
) => {
  const [local, rest] = splitProps(props as ModalOverlayProps, ["class"]);
  return <DialogPrimitive.Overlay class={cn("fixed inset-0 z-50 bg-black/40", local.class)} {...rest} />;
};

type ModalContentProps<T extends ValidComponent = "div"> = DialogPrimitive.DialogContentProps<T> & {
  class?: string | undefined;
  children?: JSX.Element;
};

export const ModalContent = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ModalContentProps<T>>,
) => {
  const [local, rest] = splitProps(props as ModalContentProps, ["class", "children"]);
  return (
    <ModalPortal>
      <ModalOverlay />
      <DialogPrimitive.Content
        class={cn(
          "relative z-50 grid max-h-full w-full max-w-128 gap-lg overflow-y-auto rounded-surface " +
            "border border-line bg-surface px-xl py-lg text-ink shadow-lg data-[expanded]:oto-enter",
          local.class,
        )}
        {...rest}
      >
        {local.children}
        <DialogPrimitive.CloseButton class="absolute right-lg top-lg rounded-control p-2xs opacity-70 transition-opacity hover:bg-raised hover:opacity-100 disabled:pointer-events-none">
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
            <path d="M18 6l-12 12" />
            <path d="M6 6l12 12" />
          </svg>
          <span class="sr-only">Close</span>
        </DialogPrimitive.CloseButton>
      </DialogPrimitive.Content>
    </ModalPortal>
  );
};

export const ModalHeader: Component<ComponentProps<"div">> = (props) => {
  const [local, rest] = splitProps(props, ["class"]);
  /* `pr-xl` is the close button's berth, not decoration: it sits `right-lg`
     inside the panel's `px-xl`, so a title long enough to reach it would run
     underneath the glyph without it. */
  return (
    <div class={cn("flex flex-col gap-xs pr-xl text-center sm:text-left", local.class)} {...rest} />
  );
};

export const ModalFooter: Component<ComponentProps<"div">> = (props) => {
  const [local, rest] = splitProps(props, ["class"]);
  return (
    <div
      class={cn("flex flex-col-reverse gap-sm sm:flex-row sm:justify-end", local.class)}
      {...rest}
    />
  );
};

type ModalTitleProps<T extends ValidComponent = "h2"> = DialogPrimitive.DialogTitleProps<T> & {
  class?: string | undefined;
};

export const ModalTitle = <T extends ValidComponent = "h2">(
  props: PolymorphicProps<T, ModalTitleProps<T>>,
) => {
  const [local, rest] = splitProps(props as ModalTitleProps, ["class"]);
  return (
    <DialogPrimitive.Title class={cn("text-title font-semibold leading-none text-ink", local.class)} {...rest} />
  );
};

type ModalDescriptionProps<T extends ValidComponent = "p"> =
  DialogPrimitive.DialogDescriptionProps<T> & {
    class?: string | undefined;
  };

export const ModalDescription = <T extends ValidComponent = "p">(
  props: PolymorphicProps<T, ModalDescriptionProps<T>>,
) => {
  const [local, rest] = splitProps(props as ModalDescriptionProps, ["class"]);
  return <DialogPrimitive.Description class={cn("text-body text-ink-muted", local.class)} {...rest} />;
};
