/**
 * Keycap (SPEC.md §5).
 *
 * The brief called for a rounded keycap; the Bauhaus instruction, SPEC.md §0 and
 * the repo's own radius-0 convention all override it, so this is `rounded-none`.
 *
 * Type is the key step — `text-meta font-medium font-mono tabular-nums` (SPEC.md §4), so
 * `⌘K` and `⇧⌘P` are the same width per glyph and a column of keycaps in a menu
 * has a straight left edge. `whitespace-nowrap` because a keycap that wrapped
 * would stop being a key.
 */
import type { Component, JSX } from "solid-js";

import { cn } from "~/lib/cn";

export const Keycap: Component<{ children: JSX.Element; class?: string }> = (props) => (
  <kbd
    class={cn(
      "inline-flex items-center justify-center rounded-none",
      "border border-white/[0.1] bg-white/[0.08]",
      "text-meta font-medium font-mono tabular-nums",
      "whitespace-nowrap px-1.5 py-0.5 leading-none",
      "text-[var(--lp-text-2)] select-none",
      props.class,
    )}
  >
    {props.children}
  </kbd>
);
