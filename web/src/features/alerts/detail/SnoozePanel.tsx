/**
 * Every quiet period this alert has ever had.
 *
 * > **Membership of a snooze is history, not a boolean.**
 *
 * This panel is the counterweight that makes the feature safe to ship. An ended
 * snooze keeps its row — who asked for it, until when, and how it finished — so
 * a period during which oto deliberately said nothing can be reviewed
 * afterwards. A quiet period nobody can be asked about is a quiet period nobody
 * is accountable for.
 *
 * `ended_reason` separates three endings that look identical from outside and
 * are not, so each gets its own sentence rather than a shared "ended".
 *
 * Colour discipline (§M.2): a snooze is **not a state**, so nothing here reaches
 * for a Tier-B hue. It signals with words and a neutral rule.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";

import type { SnoozeEndReason, SnoozeHistoryEntry } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";
import { PANEL_HEADER, PANEL_ROW } from "./rhythm";

const END_REASON: Record<NonNullable<SnoozeEndReason>, string> = {
  manual: "a person woke it early",
  expired: "the clock ran out, which is how every snooze ends unless something intervenes",
  superseded: "a new snooze on the same alert replaced it",
};

export interface SnoozePanelProps {
  readonly history: readonly SnoozeHistoryEntry[];
  readonly loading: boolean;
  readonly error: unknown;
}

export const SnoozePanel: Component<SnoozePanelProps> = (props) => (
  <Panel>
    <PanelHeader class={PANEL_HEADER}>
      <PanelTitle>Quiet periods</PanelTitle>
      <span class="text-meta text-ink-subtle">oto's notifications, not the signal</span>
    </PanelHeader>

    <Switch>
      <Match when={props.loading}>
        <LoadingLine />
      </Match>
      <Match when={props.error !== null && props.error !== undefined}>
        <ErrorState error={props.error} />
      </Match>
      <Match when={props.history.length === 0}>
        <EmptyState
          title="oto has never been asked to hold its notifications for this alert."
          body="A snooze suppresses oto's own messages until a fixed time. It writes nothing into Alertmanager and changes nothing about the alert."
        />
      </Match>
      <Match when={true}>
        <ul>
          <For each={props.history}>{(row) => <SnoozeRow row={row} />}</For>
        </ul>
      </Match>
    </Switch>
  </Panel>
);

const SnoozeRow: Component<{ readonly row: SnoozeHistoryEntry }> = (props) => {
  const r = (): SnoozeHistoryEntry => props.row;

  return (
    <li
      class={cn(
        "border-b border-line last:border-b-0",
        PANEL_ROW,
        r().active ? "border-l-2 border-l-ink-muted bg-sunken" : "",
      )}
    >
      <div class="flex flex-wrap items-baseline gap-x-sm gap-y-2xs">
        <Show when={r().active}>
          <Chip title="This is the snooze currently in force. The alert is still firing.">
            in force
          </Chip>
        </Show>
        <span class="text-body font-medium text-ink">{r().snoozed_by_label}</span>
        <span class="text-body text-ink-muted">
          asked for quiet;{" "}
          {r().active ? "notifications resume " : "notifications resumed "}
          <span title={absoluteTime(r().snoozed_until)}>
            <RelativeTime
              value={r().snoozed_until}
              label={r().active ? "Notifications resume" : "Notifications resumed"}
            />
          </span>
          {r().active ? "" : " ago"}
        </span>
        <span class="ml-auto text-meta text-ink-subtle" title={absoluteTime(r().snoozed_at)}>
          started <RelativeTime value={r().snoozed_at} label="Snooze started" /> ago
        </span>
      </div>

      <Show when={r().note}>
        {(note) => (
          <p class="mt-sm border-l-2 border-line-strong pl-sm text-body leading-snug text-ink">
            {note()}
          </p>
        )}
      </Show>

      <Show when={r().ended_reason}>
        {(reason) => (
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            Ended — {END_REASON[reason()] ?? reason()}
            {r().ended_by_label ? `, by ${r().ended_by_label}` : ""}.
          </p>
        )}
      </Show>
    </li>
  );
};
