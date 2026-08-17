/**
 * Project chip.
 *
 * Deliberately the quietest badge in the row: no glyph, no colour, and — unlike
 * `LabelBadge` — no box either. SPEC.md §2: "if every element shouts geometry,
 * the alphabet stops meaning anything". Project identity is carried by text
 * alone, in the secondary ink. A border plus padding here would give a 12px
 * chip more edge contrast than the 13px/500 issue title beside it, so the
 * quietest field in the row would read as the loudest.
 */
import type { Component } from "solid-js";

import { cn } from "~/lib/cn";

export const ProjectBadge: Component<{ name: string; class?: string }> = (props) => (
  <span
    class={cn(
      "inline-flex h-5 max-w-full shrink-0 items-center overflow-hidden",
      // Same 12px step as `LabelBadge`, so the two sit on one baseline.
      "text-body text-[var(--lp-text-2)] truncate",
      props.class,
    )}
    title={props.name}
  >
    <span class="min-w-0 truncate">{props.name}</span>
  </span>
);
