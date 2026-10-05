/**
 * ADR 0056 §1 on screen: when upstream last spoke about a Case, and whether it
 * can expire. See `./expiry` for the rule; this file only says it.
 *
 * ⛔ TIER A ONLY (§M.2). Staleness is not an alert's state, so nothing here
 * spends a state hue. A Case that cannot expire is marked the way this product
 * marks every standing warning that is not a state — a strong left rule and ink
 * one tier up — and the sentence says why.
 */
import { Show, type Component } from "solid-js";

import { tickingNow } from "~/components/Time";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";
import { ago, caseExpiry, expiryNote, isHeld, type StalenessInput } from "./expiry";

/** `last heard from upstream 3h ago`, with the instant behind a hover. */
export const LastHeard: Component<{ readonly at: string; readonly class?: string }> = (props) => (
  <span
    class={props.class}
    title={`Upstream last said anything about this firing at ${absoluteTime(props.at)} — a webhook or a reconcile pass that saw it.`}
  >
    <span class="text-ink-subtle">last heard from upstream</span>{" "}
    <time datetime={props.at} class="tabular-nums">
      {ago(props.at, tickingNow())}
    </time>
  </span>
);

/**
 * The Case screen's sentence about expiry, under its header. Nothing for an
 * ended Case (its chip names the expiry) or one whose sources were not read.
 */
export const ExpiryNote: Component<{ readonly case: StalenessInput; readonly class?: string }> = (
  props,
) => {
  const expiry = () => caseExpiry(props.case);
  const note = () => expiryNote(expiry(), tickingNow());
  return (
    <Show when={note()}>
      {(n) => (
        <p
          data-expiry={expiry()?.kind}
          class={cn(
            "text-body leading-snug",
            isHeld(expiry())
              ? "border-l-2 border-line-strong pl-2 font-medium text-ink"
              : "text-ink-muted",
            props.class,
          )}
        >
          {n().long}
        </p>
      )}
    </Show>
  );
};

/** A list row's short form of the same sentence; the long one is its tooltip. */
export const ExpiryMeta: Component<{ readonly case: StalenessInput }> = (props) => {
  const expiry = () => caseExpiry(props.case);
  const note = () => expiryNote(expiry(), tickingNow());
  return (
    <Show when={note()}>
      {(n) => (
        <span
          data-expiry={expiry()?.kind}
          class={isHeld(expiry()) ? "font-medium text-ink" : undefined}
          title={n().long}
        >
          {n().short}
        </span>
      )}
    </Show>
  );
};
