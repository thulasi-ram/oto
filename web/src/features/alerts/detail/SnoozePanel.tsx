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
import { Chip, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { absoluteTime } from "~/lib/format";

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
    <PanelHeader>
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
      class={cx(
        "border-b border-line px-3 py-2 last:border-b-0",
        r().active ? "border-l-[3px] border-l-ink-muted bg-sunken" : "",
      )}
    >
      <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <Show when={r().active}>
          <Chip title="This is the snooze currently in force. The alert is still firing.">
            in force
          </Chip>
        </Show>
        <span class="text-body font-medium text-ink">{r().snoozed_by_label}</span>
        <span class="text-body text-ink-muted">
          asked for quiet until{" "}
          <span title={absoluteTime(r().snoozed_until)}>
            <RelativeTime value={r().snoozed_until} label="Notifications resume" />
          </span>
        </span>
        <span class="ml-auto text-meta text-ink-subtle" title={absoluteTime(r().snoozed_at)}>
          started <RelativeTime value={r().snoozed_at} label="Snooze started" /> ago
        </span>
      </div>

      <Show when={r().note}>
        {(note) => (
          <p class="mt-0.5 border-l-2 border-line-strong pl-2 text-body leading-snug text-ink">
            {note()}
          </p>
        )}
      </Show>

      <Show when={r().ended_reason}>
        {(reason) => (
          <p class="mt-0.5 text-meta leading-snug text-ink-subtle">
            Ended — {END_REASON[reason()] ?? reason()}
            {r().ended_by_label ? `, by ${r().ended_by_label}` : ""}.
          </p>
        )}
      </Show>
    </li>
  );
};
