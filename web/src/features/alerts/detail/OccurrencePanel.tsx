/**
 * Every firing episode of this identity.
 *
 * This is the history Alertmanager does not keep: an alert that has fired forty
 * times has forty rows here, each with its own duration, its own acknowledgement
 * and its own reason for ending. "Has this happened before?" is a question with
 * an answer, and it is on this panel.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";

import type { Occurrence, ResolveReason, SuppressionReason } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { STATE_BAR, StateChip } from "~/components/StateChip";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime, duration } from "~/lib/format";
import { PANEL_HEADER, PANEL_ROW } from "./rhythm";

/**
 * `resolve_reason` distinguishes "the upstream told us it ended" from "we
 * stopped hearing about it and timed it out". Collapsing them into "resolved"
 * would be the exact overstatement §M.1 forbids.
 */
const RESOLVE_REASON: Record<NonNullable<ResolveReason>, string> = {
  upstream: "the upstream said it ended",
  timeout: "oto stopped hearing about it",
};

/**
 * The upstream objects doing the suppressing, joined for display.
 *
 * `suppression_reason` says a silence is muting the alert; `suppressed_by` says
 * WHICH one, and that is the half an operator can act on — it is the id you
 * paste into Alertmanager to find who silenced it and until when. It is
 * populated only while the episode is suppressed, and it returns undefined
 * rather than an empty array so `<Show>` treats "nobody named" as absent (an
 * empty array is truthy).
 */
const suppressors = (by: Occurrence["suppressed_by"]): string | undefined => {
  const ids = [...(by?.silenced_by ?? []), ...(by?.inhibited_by ?? []), ...(by?.muted_by ?? [])];
  return ids.length > 0 ? ids.join(", ") : undefined;
};

/**
 * What is doing the suppressing, in words.
 *
 * ⛔ Keyed by the contract's own `SuppressionReason` rather than by `string`, so
 * an upstream suppression kind oto learns to report is a build failure here
 * instead of "suppressed by mute_time_interval" appearing in a sentence.
 */
const SUPPRESSION_NOTE: Record<NonNullable<SuppressionReason>, string> = {
  silence: "an Alertmanager silence",
  inhibition: "an inhibition rule",
  mute_time_interval: "a mute time interval",
  active_time_interval: "an active time interval",
};

export interface OccurrencePanelProps {
  readonly occurrences: readonly Occurrence[];
  readonly loading: boolean;
  readonly error: unknown;
  readonly currentId: string | null;
}

export const OccurrencePanel: Component<OccurrencePanelProps> = (props) => (
  <Panel>
    <PanelHeader class={PANEL_HEADER}>
      <PanelTitle>Firing episodes</PanelTitle>
      <Show when={props.occurrences.length > 0}>
        <span class="shrink-0 text-meta text-ink-subtle">{props.occurrences.length} shown</span>
      </Show>
    </PanelHeader>

    <Switch>
      <Match when={props.loading}>
        <LoadingLine />
      </Match>
      <Match when={props.error !== null && props.error !== undefined}>
        <ErrorState error={props.error} />
      </Match>
      <Match when={props.occurrences.length === 0}>
        <EmptyState title="No episodes recorded." />
      </Match>
      <Match when={true}>
        <ol>
          <For each={props.occurrences}>
            {(occ) => (
              <li
                class={cn(
                  "flex items-start gap-md border-b border-line last:border-b-0",
                  PANEL_ROW,
                  // §0.6: "this is the episode you are looking at" is chrome, not
                  // state, so it is said with a neutral tone shift rather than
                  // with the accent tint that used to sit here — the same idiom
                  // `SnoozePanel` already uses for the snooze in force.
                  occ.id === props.currentId ? "border-l-2 border-l-ink-muted bg-sunken" : "",
                )}
              >
                <span
                  aria-hidden="true"
                  class={cn("mt-2xs h-6 w-2xs shrink-0 rounded-full", STATE_BAR[occ.state])}
                />

                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
                    <span class="font-mono text-body font-medium text-ink">#{occ.seq}</span>
                    <StateChip state={occ.state} size="sm" />
                    <Show when={occ.ack_state === "acked"}>
                      <span
                        class="text-meta text-ink-muted"
                        title="A receipt on the signal. It was still firing when this was recorded."
                      >
                        seen by {occ.acked_by_label ?? "someone"}
                      </span>
                    </Show>
                    <span class="ml-auto text-meta text-ink-subtle" title={absoluteTime(occ.started_at)}>
                      <RelativeTime value={occ.started_at} label="Started" /> ago
                    </span>
                  </div>

                  <div class="mt-2xs flex flex-wrap items-center gap-x-md gap-y-2xs text-meta text-ink-muted">
                    <span title="How long this episode was firing. oto times the signal, not anyone's response.">
                      fired for {duration(occ.duration_seconds)}
                      {occ.ended_at === null || occ.ended_at === undefined ? " so far" : ""}
                    </span>

                    <Show when={occ.resolve_reason}>
                      {(reason) => <span>ended because {RESOLVE_REASON[reason()]}</span>}
                    </Show>

                    <Show when={occ.suppression_reason}>
                      {(reason) => (
                        <span>suppressed by {SUPPRESSION_NOTE[reason()] ?? reason()}</span>
                      )}
                    </Show>

                    <Show when={suppressors(occ.suppressed_by)}>
                      {(ids) => (
                        <span
                          class="font-mono"
                          title="The Alertmanager objects doing the suppressing, as Alertmanager named them: silence ids, inhibiting alert fingerprints, mute intervals."
                        >
                          {ids()}
                        </span>
                      )}
                    </Show>

                    <Show when={occ.reopen_count > 0}>
                      <span title="Re-fires inside the grace window reopen the same episode rather than starting a new one.">
                        reopened {occ.reopen_count}×
                      </span>
                    </Show>

                    <Show when={occ.value !== null && occ.value !== undefined}>
                      <span class="font-mono" title="The sample value upstream reported">
                        value {occ.value}
                      </span>
                    </Show>
                  </div>

                  <Show when={occ.ack_note}>
                    {(note) => (
                      <p class="mt-sm border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink-muted">
                        {note()}
                      </p>
                    )}
                  </Show>
                </div>
              </li>
            )}
          </For>
        </ol>
      </Match>
    </Switch>
  </Panel>
);
