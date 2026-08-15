/**
 * A modal dialog built on the platform's own `<dialog showModal()>`.
 *
 * Using the native element rather than a div is a deliberate accessibility
 * decision: the browser gives us the focus trap, the inert background, the top
 * layer, `Escape` to dismiss and the `aria-modal` semantics — all of which are
 * easy to implement badly and hard to implement well. We add only what the
 * platform does not: a labelled heading, a light-dismiss backdrop, and a
 * guarantee that focus returns to whatever opened it.
 */
import {
  createEffect,
  createUniqueId,
  onCleanup,
  Show,
  type Component,
  type JSX,
  type ParentComponent,
} from "solid-js";

import { Button, cx } from "./primitives";

export interface DialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly title: string;
  /** Rendered under the title in a muted voice. Keep it to one sentence. */
  readonly description?: string;
  readonly footer?: JSX.Element;
  readonly width?: "sm" | "md" | "lg";
  readonly children: JSX.Element;
}

const WIDTH: Record<NonNullable<DialogProps["width"]>, string> = {
  sm: "max-w-sm",
  md: "max-w-lg",
  lg: "max-w-3xl",
};

export const Dialog: Component<DialogProps> = (props) => {
  let el: HTMLDialogElement | undefined;
  let opener: Element | null = null;

  /**
   * ⛔ THE LABEL IDS ARE PER-INSTANCE, and hardcoding them was a wrong label on
   * every screen that offers more than one action.
   *
   * The `<dialog>` below is not wrapped in a `<Show>`: it renders whether or not
   * it is open, because the platform's `showModal()`/`close()` is what opens it
   * and there has to be an element to call that on. So a screen that mounts
   * several dialogs side by side — `alerts/detail/Actions.tsx` mounts ack,
   * comment and snooze together — had three `#oto-dialog-title` nodes in the
   * document at once. IDREF resolution takes the FIRST match in document order,
   * so `aria-labelledby` on the dialog the operator actually opened resolved to
   * the ack dialog's heading, and a screen reader announced "Acknowledge this
   * alert" over the comment form. `aria-describedby` was the same, one sentence
   * further down.
   *
   * `createUniqueId` is Solid's own answer and it is stable across hydration, so
   * the ids do not have to be threaded through props by every caller.
   */
  const titleId = createUniqueId();
  const descId = createUniqueId();

  createEffect(() => {
    const dialog = el;
    if (!dialog) return;
    if (props.open) {
      if (!dialog.open) {
        opener = document.activeElement;
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  });

  onCleanup(() => {
    if (el?.open) el.close();
  });

  const restoreFocus = (): void => {
    if (opener instanceof HTMLElement) opener.focus();
    opener = null;
  };

  return (
    <dialog
      ref={el}
      // `close` fires for Escape too, so this is the single dismissal path.
      onClose={() => {
        restoreFocus();
        props.onClose();
      }}
      onCancel={(e) => {
        e.preventDefault();
        el?.close();
      }}
      // Light dismiss: a click that lands on the dialog element itself is a
      // click on the backdrop, because the panel below fills the content box.
      onClick={(e) => {
        if (e.target === el) el?.close();
      }}
      aria-labelledby={titleId}
      aria-describedby={props.description ? descId : undefined}
      class={cx(
        "m-auto w-[calc(100vw-2rem)] rounded-surface border border-line bg-surface p-0 text-ink",
        "shadow-[0_16px_48px_-12px_rgb(0_0_0_/_0.35)] backdrop:bg-black/40",
        "open:oto-enter",
        WIDTH[props.width ?? "md"],
      )}
    >
      <div class="flex items-start justify-between gap-4 border-b border-line px-4 py-3">
        <div class="min-w-0">
          <h2 id={titleId} class="text-title font-semibold text-ink">
            {props.title}
          </h2>
          <Show when={props.description}>
            <p id={descId} class="mt-0.5 text-body leading-snug text-ink-muted">
              {props.description}
            </p>
          </Show>
        </div>
        <Button
          variant="ghost"
          size="sm"
          aria-label="Close"
          onClick={() => el?.close()}
          class="-mr-1 -mt-1 shrink-0"
        >
          <svg viewBox="0 0 12 12" class="size-3" aria-hidden="true">
            <path
              d="m2.5 2.5 7 7m0-7-7 7"
              stroke="currentColor"
              stroke-width="1.6"
              stroke-linecap="round"
            />
          </svg>
        </Button>
      </div>

      <div class="max-h-[70vh] overflow-y-auto px-4 py-3">{props.children}</div>

      <Show when={props.footer}>
        <div class="flex items-center justify-end gap-2 border-t border-line bg-raised px-4 py-2.5">
          {props.footer}
        </div>
      </Show>
    </dialog>
  );
};

/** Body text inside a dialog, at the reading measure the rest of the app uses. */
export const DialogBody: ParentComponent<{ readonly class?: string }> = (props) => (
  <div class={cx("flex flex-col gap-3 text-item leading-relaxed text-ink", props.class)}>
    {props.children}
  </div>
);
