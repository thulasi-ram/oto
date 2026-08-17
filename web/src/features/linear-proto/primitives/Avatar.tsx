/**
 * Avatars are circles — a Bauhaus primitive, not a rounded rectangle
 * (SPEC.md §2). `rounded-full` here is one of only two exemptions from the
 * radius-0 rule; the other is the 6px label dot.
 *
 * The unassigned state keeps the same 20px footprint as a filled avatar so the
 * column holds its grid whether or not anyone owns the issue.
 */
import type { Component } from "solid-js";
import { Show } from "solid-js";

import { cn } from "~/lib/cn";
import { AVATAR_COLORS } from "~/features/linear-proto/types";
import type { Assignee } from "~/features/linear-proto/types";

const FALLBACK_COLOR = "#62666d";

function avatarColor(colorIndex: number): string {
  return AVATAR_COLORS[colorIndex % AVATAR_COLORS.length] ?? FALLBACK_COLOR;
}

export const Avatar: Component<{ assignee: Assignee | null; class?: string }> = (props) => (
  <Show
    when={props.assignee}
    fallback={
      <span
        class={cn(
          "w-5 h-5 shrink-0 inline-block rounded-full border border-dashed",
          "border-[var(--lp-text-3)]",
          props.class,
        )}
        title="Unassigned"
        aria-hidden="true"
      />
    }
  >
    {(assignee) => (
      <span
        class={cn(
          "w-5 h-5 shrink-0 inline-flex items-center justify-center rounded-full",
          // The 10px/500 caps step (SPEC.md §4); initials are already capitals,
          // so `uppercase` is a no-op here and the tracking only opens the pair
          // up inside a 20px disc.
          "text-micro font-medium uppercase tracking-[0.08em] leading-none text-white select-none",
          props.class,
        )}
        style={{ "background-color": avatarColor(assignee().colorIndex) }}
        title={assignee().name}
      >
        {assignee().initials}
      </span>
    )}
  </Show>
);
