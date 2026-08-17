/**
 * A value oto will never show again, and the affordance for carrying it away.
 *
 * Three flows hand the operator a secret exactly once — registering a source,
 * rotating a source's ingest token, and minting a personal access token — and
 * each of them had, or was about to have, its own monospace box and its own copy
 * button. That is the shape of drift `rhythm.ts` exists to prevent one level up:
 * three boxes written at three moments read as three products, and the one that
 * matters here is not decoration. A secret the operator fails to copy is not a
 * cosmetic loss, so the copying is one object with one behaviour.
 *
 * ⛔ THE COPY BUTTON REPORTS WHAT HAPPENED, and that is the whole reason it is a
 * component rather than an `onClick`. `navigator.clipboard` is undefined outside
 * a secure context and its promise rejects when the document is not focused, so
 * `void navigator.clipboard?.writeText(…)` — which is what the source dialog
 * shipped — is silent in exactly the two cases where the operator most needs to
 * know: the button depresses, nothing reaches the clipboard, and the dialog is
 * then closed on a secret that no longer exists anywhere. Here a failure says so
 * and leaves the value on screen to select by hand.
 */
import { Show, createSignal, type Component } from "solid-js";

import { Button } from "~/components/ui/Button";
import { cn } from "~/lib/cn";

import { HELP, LABEL } from "./rhythm";

/** Long enough to be read, short enough not to outlive the glance. */
const CONFIRMATION_MS = 2000;

export const OneTimeSecret: Component<{
  readonly label: string;
  readonly value: string;
  /** What this value is for, in one line. */
  readonly help?: string;
  /**
   * Whether the value is the secret itself. A secret gets the stronger frame;
   * the webhook URL beside it is public and gets the quieter one, so the two do
   * not read as equally dangerous to paste into a ticket.
   */
  readonly secret?: boolean;
}> = (props) => {
  const [state, setState] = createSignal<"idle" | "copied" | "failed">("idle");

  const copy = async (): Promise<void> => {
    try {
      // `?.` would make an absent clipboard indistinguishable from a successful
      // copy — the exact silence this component exists to end.
      if (navigator.clipboard === undefined) throw new Error("no clipboard");
      await navigator.clipboard.writeText(props.value);
      setState("copied");
    } catch {
      setState("failed");
    }
    setTimeout(() => setState("idle"), CONFIRMATION_MS);
  };

  return (
    <div class="flex flex-col gap-xs">
      <span class={LABEL}>{props.label}</span>
      <div
        class={cn(
          "rounded-control border bg-sunken px-sm py-sm",
          props.secret ? "border-line-strong" : "border-line",
        )}
      >
        {/* `select-all` so a failed copy is still one click away from the
            clipboard, which is the fallback the failure message promises. */}
        <code class="block select-all break-all font-mono text-body text-ink">{props.value}</code>
      </div>
      <div class="flex items-center gap-sm">
        <Button size="sm" variant="secondary" onClick={() => void copy()}>
          Copy
        </Button>
        <Show when={state() !== "idle"}>
          <span
            class={cn("text-body", state() === "copied" ? "text-ink-muted" : "font-medium text-ink")}
            role="status"
          >
            {state() === "copied"
              ? "Copied."
              : "Could not reach the clipboard — select the value above and copy it by hand."}
          </span>
        </Show>
      </div>
      <Show when={props.help}>{(help) => <p class={HELP}>{help()}</p>}</Show>
    </div>
  );
};
