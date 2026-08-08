/**
 * "oto is holding its tongue about this until T."
 *
 * Snooze is **not a state** (§B.8.1). It is a third orthogonal axis alongside
 * `state` and `ack_state`, so this chip is deliberately **Tier A** — no
 * `--oto-state-*` token, no dimming, no calming. A snoozed critical alert is
 * still critical and still firing, and the row beside this chip keeps its full
 * firing tint and its true severity glyph. Colouring a snoozed alert calm would
 * be exactly the lie §M.2's scarcity rule exists to prevent.
 *
 * It always carries the countdown, because the one thing that makes a quiet
 * period safe is that everyone can see when it ends.
 */
import { Show, type Component } from "solid-js";

import { RelativeTime } from "~/components/Time";
import { cx } from "~/components/ui/primitives";
import type { Snooze } from "~/api/types";

export interface SnoozeChipProps {
  /** The snooze in force. `null` renders nothing. */
  readonly snooze: Snooze | null | undefined;
  readonly class?: string;
}

export const SnoozeChip: Component<SnoozeChipProps> = (props) => (
  <Show when={props.snooze}>
    {(s) => (
      <span
        class={cx(
          "inline-flex shrink-0 items-center gap-1 rounded-[3px] border border-line-strong",
          "bg-sunken px-1 py-px text-[11px] font-medium leading-4 text-ink",
          props.class,
        )}
        title={`oto is not notifying about this alert until it wakes. Asked for by ${s().snoozed_by_label}${s().note ? ` — "${s().note}"` : ""}. The alert itself is unchanged: still firing, still whatever severity it was.`}
      >
        <ZzzGlyph />
        Notifications held ·{" "}
        <RelativeTime value={s().snoozed_until} label="Notifications resume" />
      </span>
    )}
  </Show>
);

/**
 * A "known snoozed but we do not have the row" chip.
 *
 * `AlertDTO` carries no `snooze`, so a list row can only know its snooze state
 * when the query pinned it — `?snoozed=true`. Then the fact is certain but the
 * expiry is not, so this says exactly that much and no more rather than
 * inventing a countdown.
 */
export const SnoozeChipUnknownUntil: Component<{ readonly class?: string }> = (props) => (
  <span
    class={cx(
      "inline-flex shrink-0 items-center gap-1 rounded-[3px] border border-line-strong",
      "bg-sunken px-1 py-px text-[11px] font-medium leading-4 text-ink",
      props.class,
    )}
    title="This list is filtered to alerts whose notifications oto is currently holding. Open the alert to see who asked, why, and when it wakes. It is still firing."
  >
    <ZzzGlyph />
    Notifications held
  </span>
);

const ZzzGlyph: Component = () => (
  <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
    <path
      d="M2.2 2.4h4L2.2 6.4h4M6.6 6.2h3.2L6.6 9.4h3.2"
      fill="none"
      stroke="currentColor"
      stroke-width="1.2"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);
