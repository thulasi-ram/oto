/**
 * Label chip: a 6px identity dot plus the name (SPEC.md §2).
 *
 * The dot is one of the two `rounded-full` exemptions and one of the two places
 * non-semantic colour is allowed, because label colour is *data*, not decoration.
 * The chip itself is a square-cornered hairline box — it must never compete with
 * the status/priority alphabet to its left.
 */
import type { Component } from "solid-js";

import { cn } from "~/lib/cn";
import type { Label } from "~/features/linear-proto/types";

export const LabelBadge: Component<{ label: Label; class?: string }> = (props) => (
  <span
    class={cn(
      "inline-flex h-5 max-w-full shrink-0 items-center gap-1 overflow-hidden",
      "rounded-none border border-[var(--lp-border)] px-1.5",
      // `text-body` is the 12px metadata step (SPEC.md §4). The chip is a fixed
      // `h-5` flex box that centres its own line, so no `leading-*` is needed.
      "text-body text-[var(--lp-text-2)]",
      props.class,
    )}
    title={props.label.name}
  >
    <span
      class="h-1.5 w-1.5 shrink-0 rounded-full"
      style={{ "background-color": props.label.color }}
      aria-hidden="true"
    />
    <span class="min-w-0 truncate">{props.label.name}</span>
  </span>
);
