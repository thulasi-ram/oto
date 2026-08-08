/**
 * Every firing episode of this identity.
 *
 * This is the history Alertmanager does not keep: an alert that has fired forty
 * times has forty rows here, each with its own duration, its own acknowledgement
 * and its own reason for ending. "Has this happened before?" is a question with
 * an answer, and it is on this panel.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";

import type { Occurrence, ResolveReason } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { STATE_BAR, StateChip } from "~/components/StateChip";
import { Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { absoluteTime, duration } from "~/lib/format";

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

const SUPPRESSION_NOTE: Record<string, string> = {
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
    <PanelHeader>
      <PanelTitle>Firing episodes</PanelTitle>
      <Show when={props.occurrences.length > 0}>
        <span class="text-[11px] text-ink-subtle">{props.occurrences.length} shown</span>
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
                class={cx(
                  "flex items-start gap-2.5 border-b border-line px-3 py-2 last:border-b-0",
                  occ.id === props.currentId ? "bg-accent-fill" : "",
                )}
              >
                <span
                  aria-hidden="true"
                  class={cx("mt-1 h-6 w-[3px] shrink-0 rounded-full", STATE_BAR[occ.state])}
                />

                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span class="font-mono text-[12px] font-medium text-ink">#{occ.seq}</span>
                    <StateChip state={occ.state} size="sm" />
                    <Show when={occ.ack_state === "acked"}>
                      <span
                        class="text-[11px] text-ink-muted"
                        title="A receipt on the signal. It was still firing when this was recorded."
                      >
                        seen by {occ.acked_by_label ?? "someone"}
                      </span>
                    </Show>
                    <span class="ml-auto text-[11px] text-ink-subtle" title={absoluteTime(occ.started_at)}>
                      <RelativeTime value={occ.started_at} label="Started" /> ago
                    </span>
                  </div>

                  <div class="mt-0.5 flex flex-wrap items-center gap-x-3 text-[11px] text-ink-muted">
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
                      <p class="mt-1 border-l-2 border-line-strong pl-2 text-[11px] leading-snug text-ink-muted">
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
