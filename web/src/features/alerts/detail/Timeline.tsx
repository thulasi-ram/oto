/**
 * The lifecycle as an event stream. **The product's differentiator.**
 *
 * What makes it worth building rather than a table of rows:
 *
 *   - **Two timestamps, both honest.** `occurred_at` is the upstream's claim and
 *     is what the gutter displays; `recorded_at` is oto's clock and is what the
 *     list is ordered by. Where they diverge a skew badge says so, because that
 *     divergence is a true fact about the cluster and normalising it away would
 *     destroy the only evidence of a wrong clock.
 *   - **Attribution on every row**, and a human actor is visually distinct from
 *     a machine one — "the reaper expired this" and "Priya expired this" are
 *     very different sentences.
 *   - **Unknown event types still render.** The server pre-computes a one-line
 *     `summary` at write time precisely so a client can display an event it has
 *     never heard of. Forward compatibility is a feature, not a fallback.
 *   - **Nothing is editable.** Events are immutable; the timeline is the record.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";

import type { AlertEvent } from "~/api/types";
import { ClockSkewBadge, ClockTime, RelativeTime } from "~/components/Time";
import { Button, ToggleGroup, cx } from "~/components/ui/primitives";
import { EmptyState } from "~/components/ui/states";
import { calendarDay, differentDay } from "~/lib/format";
import {
  ALL_CATEGORIES,
  CATEGORY_LABEL,
  describeActor,
  isHuman,
  kindOf,
  type EventCategory,
  type MarkerShape,
} from "./eventKinds";
import { RuleChangePayload, isRuleChangePayload } from "./RuleDrift";

export interface TimelineProps {
  readonly events: readonly AlertEvent[];
  readonly categories: readonly EventCategory[];
  readonly onCategoriesChange: (next: readonly EventCategory[]) => void;
  readonly order: "asc" | "desc";
  readonly onOrderChange: (next: "asc" | "desc") => void;
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly onLoadMore: () => void;
}

export const Timeline: Component<TimelineProps> = (props) => (
  <div class="flex min-h-0 flex-col">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-line bg-raised px-3 py-2">
      <ToggleGroup<EventCategory>
        legend="Event kinds"
        options={ALL_CATEGORIES.map((c) => ({ value: c, label: CATEGORY_LABEL[c] }))}
        selected={props.categories}
        onChange={props.onCategoriesChange}
      />
      <div class="ml-auto flex items-center gap-2">
        <Show when={props.categories.length > 0}>
          <Button size="sm" variant="ghost" onClick={() => props.onCategoriesChange([])}>
            Show all
          </Button>
        </Show>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => props.onOrderChange(props.order === "desc" ? "asc" : "desc")}
          title="Ordering is always by oto's own clock, so a skewed upstream can never reorder the timeline."
        >
          {props.order === "desc" ? "Newest first" : "Oldest first"}
        </Button>
      </div>
    </div>

    <Show
      when={props.events.length > 0}
      fallback={
        <EmptyState
          title="No events of these kinds."
          body="Every alert has at least one event. If this is empty, the filter above is hiding them — not the timeline."
        />
      }
    >
      <ol class="min-h-0 flex-1 overflow-auto px-3 py-2">
        <For each={props.events}>
          {(event, i) => (
            <TimelineRow
              event={event}
              previous={props.events[i() - 1]}
              first={i() === 0}
              order={props.order}
            />
          )}
        </For>
      </ol>
    </Show>

    <Show when={props.hasMore}>
      <div class="border-t border-line px-3 py-2 text-center">
        <Button size="sm" busy={props.loading} onClick={props.onLoadMore}>
          Load earlier events
        </Button>
      </div>
    </Show>
  </div>
);

/* -------------------------------------------------------------------------- */
/* One event                                                                  */
/* -------------------------------------------------------------------------- */

const TimelineRow: Component<{
  readonly event: AlertEvent;
  readonly previous: AlertEvent | undefined;
  readonly first: boolean;
  readonly order: "asc" | "desc";
}> = (props) => {
  const [open, setOpen] = createSignal(false);
  const kind = createMemo(() => kindOf(props.event.type));

  /**
   * Day separators are computed against `occurred_at`, which is what is on
   * screen. Using `recorded_at` here would put the separator in a place the eye
   * cannot justify from the visible times.
   */
  const newDay = (): boolean =>
    props.first || differentDay(props.event.occurred_at, props.previous?.occurred_at);

  const payload = (): Record<string, unknown> => props.event.payload ?? {};
  const hasPayload = (): boolean => Object.keys(payload()).length > 0;

  return (
    <>
      <Show when={newDay()}>
        <li class="flex items-center gap-2 pb-1 pt-3 first:pt-0" aria-hidden="true">
          <span class="text-meta font-semibold uppercase tracking-[0.06em] text-ink-subtle">
            {calendarDay(props.event.occurred_at)}
          </span>
          <span class="h-px flex-1 bg-line" />
        </li>
      </Show>

      <li class="group relative flex gap-3 pl-1">
        {/* The rail. It is a background element, so it must never be the only
            thing carrying meaning — the marker glyph and the label do that. */}
        <div class="relative flex w-4 shrink-0 justify-center" aria-hidden="true">
          <span class="absolute inset-y-0 w-px bg-line" />
          <span class={cx("relative mt-1.5 bg-surface", kind().tone)}>
            <Marker shape={kind().shape} />
          </span>
        </div>

        <div class="min-w-0 flex-1 pb-3">
          <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
            <ClockTime value={props.event.occurred_at} class="shrink-0 text-ink-subtle" />

            <span class="shrink-0 text-body font-medium text-ink">{kind().label}</span>

            {/* Attribution. A human is named plainly; a machine is named as the
                machine it is, so the two are never confusable. */}
            <span
              class={cx(
                "shrink-0 text-meta",
                isHuman(props.event.actor_kind) ? "font-medium text-ink-muted" : "text-ink-subtle",
              )}
              title={`Recorded by the ${props.event.actor_kind} actor`}
            >
              {isHuman(props.event.actor_kind) ? "by " : "· "}
              {describeActor(props.event.actor_kind, props.event.actor_label)}
            </span>

            {/* The two clocks disagreed. That is data, not noise. */}
            <ClockSkewBadge
              occurredAt={props.event.occurred_at}
              recordedAt={props.event.recorded_at}
            />

            <span class="ml-auto shrink-0 text-meta text-ink-subtle">
              <RelativeTime value={props.event.occurred_at} label="Occurred" />
            </span>
          </div>

          {/* The server's pre-rendered one-liner. Rendered verbatim, which is
              what lets an older UI display a newer event type correctly. */}
          <p class="mt-0.5 text-item leading-snug text-ink">{props.event.summary}</p>

          <Show when={kind().note}>
            <p class="mt-0.5 text-meta leading-snug text-ink-subtle">{kind().note}</p>
          </Show>

          {/* A rule change is the one payload that gets a first-class rendering,
              because "the threshold moved under you" is the single most valuable
              thing this timeline can tell anyone. */}
          <Show when={isRuleChangePayload(props.event.type, payload())}>
            <RuleChangePayload payload={payload()} class="mt-2" />
          </Show>

          <Show when={hasPayload() && !isRuleChangePayload(props.event.type, payload())}>
            <div class="mt-1">
              <button
                type="button"
                class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
                aria-expanded={open()}
                onClick={() => setOpen(!open())}
              >
                {open() ? "Hide" : "Show"} detail
              </button>
              <Show when={open()}>
                <PayloadTable payload={payload()} />
              </Show>
            </div>
          </Show>
        </div>
      </li>
    </>
  );
};

/**
 * Structured payload as a definition list rather than a JSON blob.
 *
 * Nested values fall back to JSON, but a flat scalar map — which is what most
 * event payloads are — reads as English instead of as a code block.
 */
const PayloadTable: Component<{ readonly payload: Record<string, unknown> }> = (props) => (
  <dl class="mt-1 grid grid-cols-[minmax(0,9rem)_minmax(0,1fr)] gap-x-3 gap-y-0.5 rounded-control border border-line bg-sunken px-2 py-1.5">
    <For each={Object.entries(props.payload)}>
      {([key, value]) => (
        <>
          <dt class="truncate font-mono text-meta text-ink-subtle" title={key}>
            {key}
          </dt>
          <dd class="min-w-0 break-words font-mono text-meta text-ink">
            {typeof value === "string" || typeof value === "number" || typeof value === "boolean"
              ? String(value)
              : JSON.stringify(value)}
          </dd>
        </>
      )}
    </For>
  </dl>
);

/**
 * Five distinct marker shapes.
 *
 * Shape, not colour, is what separates a resolve from an expiry for anyone with
 * a colour-vision deficiency or a bad monitor at 3am (U1). The colour is a
 * second channel, never the only one.
 */
const Marker: Component<{ readonly shape: MarkerShape }> = (props) => {
  const body = (): string => {
    switch (props.shape) {
      case "dot":
        return "M5 1.6a3.4 3.4 0 1 0 0 6.8 3.4 3.4 0 0 0 0-6.8Z";
      case "diamond":
        return "M5 0.8 9.2 5 5 9.2 0.8 5Z";
      case "square":
        return "M1.8 1.8h6.4v6.4H1.8Z";
      case "bar":
        return "M0.6 3.6h8.8v2.8H0.6Z";
      case "quote":
        return "M1.4 1.8h7.2v4.6H4.6L2.4 8.6V6.4H1.4Z";
      default:
        return "";
    }
  };

  return (
    <svg viewBox="0 0 10 10" class="size-2.5" aria-hidden="true">
      <Show
        when={props.shape !== "ring"}
        fallback={<circle cx="5" cy="5" r="3.2" fill="none" stroke="currentColor" stroke-width="1.8" />}
      >
        <path d={body()} fill="currentColor" />
      </Show>
    </svg>
  );
};
