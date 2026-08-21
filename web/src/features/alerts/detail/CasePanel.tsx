/**
 * Every **case** of this identity — one row per firing episode.
 *
 * This is the history Alertmanager does not keep: an alert that has fired forty
 * times has forty rows here, each with its own duration, its own acknowledgement
 * and its own reason for ending. "Has this happened before?" is a question with
 * an answer, and it is on this panel.
 *
 * ⛔ A ROW HERE IS A CASE AND EVERY LABEL SAYS SO. One episode of one alert —
 * never a batch, never a correlation and never an incident. Each row links to
 * its own case screen, which is where the receipt is written.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";
import { A } from "@solidjs/router";

import type { Case, ResolveReason, SuppressionReason } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { CaseStateChip } from "~/components/StateChip";
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
const suppressors = (by: Case["suppressed_by"]): string | undefined => {
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

export interface CasePanelProps {
  readonly cases: readonly Case[];
  readonly loading: boolean;
  readonly error: unknown;
  readonly currentId: string | null;
}

export const CasePanel: Component<CasePanelProps> = (props) => (
  <Panel>
    <PanelHeader class={PANEL_HEADER}>
      <PanelTitle>Cases</PanelTitle>
      <Show when={props.cases.length > 0}>
        <span class="shrink-0 text-meta text-ink-subtle">{props.cases.length} shown</span>
      </Show>
    </PanelHeader>

    <Switch>
      <Match when={props.loading}>
        <LoadingLine />
      </Match>
      <Match when={props.error !== null && props.error !== undefined}>
        <ErrorState error={props.error} />
      </Match>
      <Match when={props.cases.length === 0}>
        <EmptyState title="No cases recorded — this alert has never fired." />
      </Match>
      <Match when={true}>
        <ol>
          <For each={props.cases}>
            {(ac) => (
              <li
                class={cn(
                  "flex items-start gap-md border-b border-line last:border-b-0",
                  PANEL_ROW,
                  // §0.6: "this is the case you are looking at" is chrome, not
                  // state, so it is said with a neutral tone shift rather than
                  // with the accent tint that used to sit here — the same idiom
                  // `SnoozePanel` already uses for the snooze in force.
                  ac.id === props.currentId ? "border-l-2 border-l-ink-muted bg-sunken" : "",
                )}
              >
                {/* ⛔⛔ THE STATE RAIL WAS HERE AND IS DELETED, AND THIS ROW IS THE
                    CLEAREST CASE FOR WHY. It was a 2 px neutral bar keyed on
                    `ac.state` — inked when open, drawn in the border tone when
                    ended — sitting immediately left of a `CaseStateChip` that
                    said `Open` or `Ended` in words. Two channels for one boolean
                    is merely redundant; what made it a defect is the line above,
                    where the CURRENT case is marked with `border-l-2
                    border-l-ink-muted`. That put two neutral left-edge marks in
                    the same 6 px of gutter, meaning two unrelated things — "this
                    is the one you are reading" and "this one is still running" —
                    with nothing to tell a reader which was which.
                    One left edge, one meaning. The state is the chip's. */}
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
                    <A
                      href={`/cases/${ac.id}`}
                      class="font-mono text-body font-medium text-ink underline decoration-line-strong underline-offset-2 hover:text-ink-muted"
                      title="Open this case — where it is acknowledged, and what happened during it."
                    >
                      #{ac.seq}
                    </A>
                    {/* No `resolveReason` here on purpose: the meta line below
                        already says "ended because oto stopped hearing about it"
                        in full, and a chip repeating it as "timed out" would be
                        the same fact in two vocabularies two lines apart. */}
                    <CaseStateChip state={ac.state} size="sm" />
                    <Show when={ac.ack_state === "acked"}>
                      <span
                        class="text-meta text-ink-muted"
                        title="A receipt on the signal. It was still firing when this was recorded."
                      >
                        seen by {ac.acked_by_label ?? "someone"}
                      </span>
                    </Show>
                    <span class="ml-auto text-meta text-ink-subtle" title={absoluteTime(ac.started_at)}>
                      <RelativeTime value={ac.started_at} label="Started" /> ago
                    </span>
                  </div>

                  <div class="mt-2xs flex flex-wrap items-center gap-x-md gap-y-2xs text-meta text-ink-muted">
                    <span title="How long this case was firing. oto times the signal, not anyone's response.">
                      fired for {duration(ac.duration_seconds)}
                      {ac.state === "open" ? " so far" : ""}
                    </span>

                    <Show when={ac.resolve_reason}>
                      {(reason) => <span>ended because {RESOLVE_REASON[reason()]}</span>}
                    </Show>

                    <Show when={ac.suppression_reason}>
                      {(reason) => (
                        <span>suppressed by {SUPPRESSION_NOTE[reason()] ?? reason()}</span>
                      )}
                    </Show>

                    <Show when={suppressors(ac.suppressed_by)}>
                      {(ids) => (
                        <span
                          class="font-mono"
                          title="The Alertmanager objects doing the suppressing, as Alertmanager named them: silence ids, inhibiting alert fingerprints, mute intervals."
                        >
                          {ids()}
                        </span>
                      )}
                    </Show>

                    <Show when={ac.value !== null && ac.value !== undefined}>
                      <span class="font-mono" title="The sample value upstream reported">
                        value {ac.value}
                      </span>
                    </Show>
                  </div>

                  <Show when={ac.ack_note}>
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
