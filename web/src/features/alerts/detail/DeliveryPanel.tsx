/**
 * Was anyone actually told?
 *
 * > **oto's silence must never be indistinguishable from "no alert".**
 *
 * That single sentence is why this panel exists and why a non-zero `dead` count
 * is presented as a headline rather than a footnote. A revoked Slack token that
 * has been swallowing notifications for three days is the most expensive
 * invisible failure an alerting tool can have, and this is where it stops being
 * invisible.
 *
 * The counts are Tier A. A delivery failure is serious but it is **not the
 * state of an alert**, and borrowing a state hue for it would spend exactly the
 * scarcity that makes a firing row loud (§M.2). It signals with weight, a
 * strong border and unambiguous words instead.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";

import type { DeliverySummary, Notification } from "~/api/types";
import { DeadDeliveries } from "~/features/notifications/DeadDeliveries";
import { describeSuppression, REASON_LABEL } from "~/features/notifications/vocabulary";
import { RelativeTime } from "~/components/Time";
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime, shortId } from "~/lib/format";
import { PANEL_BODY, PANEL_HEADER, PANEL_ROW } from "./rhythm";

/*
 * The words for a reason and for a suppression are NOT declared here any more.
 * They live in `~/features/notifications/vocabulary`, because the org-wide
 * activity log renders the same rows and a second copy of an enum-keyed map is
 * how the first one lost `snoozed`. That module's header carries the argument.
 */

export interface DeliveryPanelProps {
  readonly notifications: readonly Notification[];
  readonly summary: DeliverySummary | null;
  readonly loading: boolean;
  readonly error: unknown;
}

export const DeliveryPanel: Component<DeliveryPanelProps> = (props) => (
  <Panel>
    <PanelHeader class={PANEL_HEADER}>
      <PanelTitle>Who was told</PanelTitle>
    </PanelHeader>

    <Show when={props.summary}>
      {(s) => (
        <div class={cn("border-b border-line", PANEL_BODY)}>
          <div class="flex flex-wrap items-center gap-xs">
            <Stat label="sent" value={s().sent} />
            <Stat label="pending" value={s().pending} />
            <Stat label="skipped" value={s().skipped} />
            <Stat label="failed" value={s().failed} emphasis={s().failed > 0} />
            <Stat label="gave up" value={s().dead} emphasis={s().dead > 0} />
            <Show when={s().last_sent_at}>
              <span class="ml-auto text-meta text-ink-subtle">
                last sent <RelativeTime value={s().last_sent_at} label="Last sent" /> ago
              </span>
            </Show>
          </div>

          {/* The headline case. Stated as a sentence, because a number alone
              does not communicate "nobody knows about this". */}
          <Show when={s().dead > 0}>
            <p class="mt-md rounded-control border border-line-strong border-l-2 border-l-ink bg-sunken px-md py-sm text-body font-medium leading-snug text-ink">
              {s().dead} {s().dead === 1 ? "delivery" : "deliveries"} gave up permanently. Nobody was
              told through {s().dead === 1 ? "that channel" : "those channels"}.
              {s().last_error_class ? ` Last error class: ${s().last_error_class}.` : ""}
            </p>
          </Show>

          <Show when={s().total === 0}>
            <p class="mt-md text-body leading-snug text-ink-muted">
              No notification was even attempted for this alert. That usually means no policy
              matched it — which is worth knowing, because it is indistinguishable from silence.
            </p>
          </Show>
        </div>
      )}
    </Show>

    <Switch>
      <Match when={props.loading}>
        <LoadingLine />
      </Match>
      <Match when={props.error !== null && props.error !== undefined}>
        <ErrorState error={props.error} />
      </Match>
      <Match when={props.notifications.length === 0}>
        <EmptyState
          title="No notifications recorded."
          body="oto records an intent to communicate even when it decides not to send, so an empty list here means nothing was ever computed for this alert."
        />
      </Match>
      <Match when={true}>
        <ul>
          <For each={props.notifications}>
            {(n) => (
              <li class={cn("border-b border-line last:border-b-0", PANEL_ROW)}>
                <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
                  <span class="text-body font-medium text-ink">
                    {REASON_LABEL[n.reason] ?? n.reason}
                  </span>
                  <Chip title={`Notification status: ${n.status}`}>{n.status}</Chip>
                  {/* ⛔ A NULL `policy_id` HAS TWO CAUSES AND THEY ARE NOT THE
                      SAME FACT. A policy that matched and has since been
                      deleted is "oto can no longer name what routed this";
                      `no_policy` is "nothing routed it at all", and there was
                      never a policy to name. This chip read "policy deleted"
                      for both, inventing a deletion that never happened on the
                      commonest row an org without policies has — the
                      suppression sentence below already says it properly. */}
                  <Show when={n.policy_id ?? (n.suppressed_reason !== "no_policy" ? "deleted" : null)}>
                    <Chip
                      mono={n.policy_id !== null && n.policy_id !== undefined}
                      title={
                        n.policy_id
                          ? "The notification policy that matched this fact."
                          : "The policy that matched has since been deleted, so oto can no longer name it."
                      }
                    >
                      policy {n.policy_id ? shortId(n.policy_id) : "deleted"}
                    </Chip>
                  </Show>
                  <span class="ml-auto text-meta text-ink-subtle">
                    <RelativeTime value={n.created_at} label="Created" /> ago
                  </span>
                </div>

                {/* `updated_at` is nullable and no longer echoes `created_at`.
                    Null means "this read model does not track one" — unknown,
                    which is not the same as "never changed", so it is rendered
                    as an em dash rather than silently repeating the creation
                    time and making the two indistinguishable. */}
                <p class="mt-2xs text-meta text-ink-subtle">
                  last moved{" "}
                  <Show
                    when={n.updated_at}
                    fallback={<span title="This read model does not track an update time for notifications. Unknown, not unchanged.">—</span>}
                  >
                    {(at) => (
                      <span title={absoluteTime(at())}>
                        <RelativeTime value={at()} label="Last moved" /> ago
                      </span>
                    )}
                  </Show>
                </p>

                <Show when={n.suppressed_reason}>
                  {(reason) => (
                    <p class="mt-2xs text-meta leading-snug text-ink-muted">
                      Not sent — {describeSuppression(reason())}. Recorded rather than dropped, so
                      the audit trail is complete.
                    </p>
                  )}
                </Show>

                <Show when={n.delivery_summary}>
                  {(s) => (
                    <div class="mt-sm flex flex-wrap items-center gap-2xs">
                      <Stat label="sent" value={s().sent} small />
                      <Show when={s().pending > 0}>
                        <Stat label="pending" value={s().pending} small />
                      </Show>
                      {/* A skipped delivery means the destination already shows
                          exactly this content — a healthy quiet thread, not a
                          failure — so it is counted plainly and never emphasised. */}
                      <Show when={s().skipped > 0}>
                        <Stat label="skipped" value={s().skipped} small />
                      </Show>
                      <Show when={s().failed > 0}>
                        <Stat label="failed" value={s().failed} emphasis small />
                      </Show>
                      <Show when={s().dead > 0}>
                        <Stat label="gave up" value={s().dead} emphasis small />
                      </Show>
                    </div>
                  )}
                </Show>

                {/* ⭐ THE ONE ACTIONABLE THING ON THIS PANEL, AND IT IS GATED ON
                    THE COUNT RATHER THAN ALWAYS MOUNTED. The retry endpoint takes
                    a DELIVERY id and this row only carries a roll-up, so the ids
                    cost a request — one per intent that actually gave up, which
                    on almost every alert is none. The org-wide log stays a log
                    (`features/notifications/ActivitySection`); this is the screen
                    that has the context to judge a retry. */}
                <Show when={(n.delivery_summary?.dead ?? 0) > 0}>
                  <DeadDeliveries notificationId={n.id} />
                </Show>
              </li>
            )}
          </For>
        </ul>
      </Match>
    </Switch>
  </Panel>
);

const Stat: Component<{
  readonly label: string;
  readonly value: number;
  readonly emphasis?: boolean;
  readonly small?: boolean;
}> = (props) => (
  <span
    class={cn(
      "inline-flex items-center gap-1 rounded-chip border px-1.5 leading-5 tabular-nums",
      props.small === true ? "text-micro" : "text-meta",
      props.emphasis === true
        ? "border-line-strong bg-raised font-semibold text-ink"
        : "border-line bg-surface text-ink-muted",
    )}
  >
    <span class={props.value === 0 ? "text-ink-subtle" : ""}>{props.value}</span>
    {props.label}
  </span>
);
