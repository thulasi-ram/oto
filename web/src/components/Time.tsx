/**
 * Time, told honestly.
 *
 * oto carries **two** timestamps on every event and they are not
 * interchangeable (§E, `AlertEventDTO`):
 *
 *   - `occurred_at` — the upstream's claim. **This is what we display.**
 *   - `recorded_at` — oto's own clock. **This is what we order by.**
 *
 * When they diverge the divergence is a fact about the cluster, so it is
 * surfaced as a badge rather than corrected away. A timeline that silently
 * normalised skew would destroy the only evidence that a node's clock is wrong.
 */
import { createSignal, onCleanup, type Component } from "solid-js";

import { cn } from "~/lib/cn";
import {
  absoluteTime,
  clockTime,
  describeSkew,
  duration,
  relativeTime,
  skewIsNotable,
  skewMs,
} from "~/lib/format";

/**
 * A ticking clock shared by every relative timestamp on the page.
 *
 * One interval for the whole app rather than one per row: at a thousand rows
 * the difference is the difference between a smooth table and a warm laptop.
 * Ten seconds is under the resolution anything on screen actually shows.
 */
const [now, setNow] = createSignal(Date.now());
if (typeof window !== "undefined") {
  setInterval(() => setNow(Date.now()), 10_000);
}

export interface RelativeTimeProps {
  readonly value: string | null | undefined;
  /** Prefix for the accessible label, e.g. "Last seen". */
  readonly label?: string;
  readonly class?: string;
}

/**
 * `12m`, with the absolute value always one hover or one screen-reader stop
 * away. `<time datetime>` gives assistive tech the machine value for free.
 */
export const RelativeTime: Component<RelativeTimeProps> = (props) => (
  <time
    datetime={props.value ?? undefined}
    title={`${props.label ? `${props.label}: ` : ""}${absoluteTime(props.value)}`}
    class={cn("tabular-nums", props.class)}
  >
    {relativeTime(props.value, now())}
  </time>
);

/** `14:03:22` — for the timeline gutter, where the day is already established. */
export const ClockTime: Component<{ readonly value: string; readonly class?: string }> = (
  props,
) => (
  <time
    datetime={props.value}
    title={absoluteTime(props.value)}
    class={cn("font-mono text-meta tabular-nums", props.class)}
  >
    {clockTime(props.value)}
  </time>
);

/** A live-updating elapsed span, e.g. how long a case has been firing. */
export const Elapsed: Component<{
  readonly from: string | null | undefined;
  readonly to?: string | null | undefined;
  readonly class?: string;
}> = (props) => {
  const text = (): string => {
    if (!props.from) return "—";
    const start = new Date(props.from).getTime();
    if (Number.isNaN(start)) return "—";
    const end = props.to ? new Date(props.to).getTime() : now();
    return duration((end - start) / 1000);
  };
  return <span class={cn("tabular-nums", props.class)}>{text()}</span>;
};

/**
 * Shown only where the two clocks actually disagree by a visible margin.
 *
 * It is a Tier-A badge on purpose. Clock skew is a data-quality warning about
 * the cluster, not the state of an alert, and giving it a state colour would
 * spend the scarcity that makes state colour legible (§M.2).
 */
export const ClockSkewBadge: Component<{
  readonly occurredAt: string;
  readonly recordedAt: string;
  readonly class?: string;
}> = (props) => {
  if (!skewIsNotable(props.occurredAt, props.recordedAt)) return null;
  const ms = (): number => skewMs(props.occurredAt, props.recordedAt);

  return (
    <span
      class={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-chip border border-line-strong",
        "bg-sunken px-1 py-px font-mono text-micro leading-4 text-ink-muted",
        props.class,
      )}
      title={`${describeSkew(props.occurredAt, props.recordedAt)}. oto displays the upstream time and orders by its own, so the timeline stays in the right order regardless.`}
    >
      <svg viewBox="0 0 12 12" class="size-2.5" aria-hidden="true">
        <circle cx="6" cy="6" r="4.6" fill="none" stroke="currentColor" stroke-width="1.2" />
        <path d="M6 3.4V6l1.9 1.3" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
      </svg>
      skew {ms() > 0 ? "+" : "−"}
      {duration(Math.abs(ms()) / 1000)}
    </span>
  );
};

/** A countdown to a fixed instant, e.g. "retrying in 8s". */
export const Countdown: Component<{ readonly until: number | null; readonly class?: string }> = (
  props,
) => {
  const [tick, setTick] = createSignal(Date.now());
  const id = setInterval(() => setTick(Date.now()), 500);
  onCleanup(() => clearInterval(id));

  const seconds = (): number =>
    props.until === null ? 0 : Math.max(0, Math.ceil((props.until - tick()) / 1000));

  return <span class={cn("tabular-nums", props.class)}>{seconds()}s</span>;
};
