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

import type {
  DeliverySummary,
  Notification,
  NotificationReason,
  NotificationSuppressedReason,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Chip, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { absoluteTime, shortId } from "~/lib/format";

/**
 * Every reason a notification can be suppressed, in plain language.
 *
 * A suppressed notification is **recorded with a reason, never silently
 * dropped**, so every one of these has to be sayable.
 */
const SUPPRESSED_REASON: Record<NonNullable<NotificationSuppressedReason>, string> = {
  no_policy: "no notification policy matched, so nobody was told",
  // §B.8.2 ranks a snooze ahead of every automatic damper: it is a deliberate
  // human act and therefore the most actionable explanation of a silence.
  snoozed:
    "a person asked oto to hold its notifications for this alert until a fixed time — the alert itself kept firing",
  throttled: "the per-subject throttle was already spent",
  storm: "the group was in storm mode — one message with a count was posted instead",
  flapping: "this alert is damped as flapping, so updates are digested rather than sent one by one",
  verbosity: "the channel's verbosity setting does not carry this kind of update",
  channel_disabled: "every matching channel is disabled",
  duplicate_render: "the message would have been byte-identical to the one already posted",
};

/**
 * A reason oto has never heard of renders as its raw wire value rather than as
 * nothing. The published enum is closed, so this only fires when the server is
 * ahead of the client — which gate G3 (`npm run generate:check`, in CI) exists
 * to make a build failure rather than a blank in the UI.
 */
function describeSuppression(reason: string): string {
  return SUPPRESSED_REASON[reason as NonNullable<NotificationSuppressedReason>] ?? reason;
}

/**
 * Every reason a notification carries, in plain language.
 *
 * ⛔ TYPED AGAINST `NotificationReason`, not `Record<string, string>`. The
 * suppression map above already learned this the hard way — it lost `snoozed`,
 * the compiler had nothing to check it against, and a wire token was rendered
 * where a sentence belongs. This map has the same job and now has the same
 * guarantee: a reason the server adds is a build failure here.
 */
const REASON_LABEL: Record<NotificationReason, string> = {
  fired: "started firing",
  new_alerts: "new alerts joined",
  some_resolved: "some resolved",
  all_resolved: "all resolved",
  repeat: "repeat",
  suppressed: "suppressed upstream",
  unsuppressed: "no longer suppressed",
  expired: "expired",
  refired: "fired again",
  acked: "acknowledged",
  unacked: "acknowledgement withdrawn",
  enriched: "enrichment arrived",
  rule_changed: "the rule changed",
  comment: "a comment was added",
  snoozed: "snoozed",
  unsnoozed: "snooze ended",
  unacked_reminder: "still firing and unacknowledged",
  storm: "storm",
};

export interface DeliveryPanelProps {
  readonly notifications: readonly Notification[];
  readonly summary: DeliverySummary | null;
  readonly loading: boolean;
  readonly error: unknown;
}

export const DeliveryPanel: Component<DeliveryPanelProps> = (props) => (
  <Panel>
    <PanelHeader>
      <PanelTitle>Who was told</PanelTitle>
    </PanelHeader>

    <Show when={props.summary}>
      {(s) => (
        <div class="border-b border-line px-3 py-2">
          <div class="flex flex-wrap items-center gap-1.5">
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
            <p class="mt-2 rounded-control border border-line-strong border-l-[3px] border-l-ink bg-sunken px-2 py-1.5 text-body font-medium leading-snug text-ink">
              {s().dead} {s().dead === 1 ? "delivery" : "deliveries"} gave up permanently. Nobody was
              told through {s().dead === 1 ? "that channel" : "those channels"}.
              {s().last_error_class ? ` Last error class: ${s().last_error_class}.` : ""}
            </p>
          </Show>

          <Show when={s().total === 0}>
            <p class="mt-2 text-body leading-snug text-ink-muted">
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
              <li class="border-b border-line px-3 py-2 last:border-b-0">
                <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span class="text-body font-medium text-ink">
                    {REASON_LABEL[n.reason] ?? n.reason}
                  </span>
                  <Chip title={`Notification status: ${n.status}`}>{n.status}</Chip>
                  {/* Which policy matched. `null` means the policy has since
                      been deleted, which is a different fact from "no policy
                      matched" — that one shows as a suppression reason. */}
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
                  <span class="ml-auto text-meta text-ink-subtle">
                    <RelativeTime value={n.created_at} label="Created" /> ago
                  </span>
                </div>

                {/* `updated_at` is nullable and no longer echoes `created_at`.
                    Null means "this read model does not track one" — unknown,
                    which is not the same as "never changed", so it is rendered
                    as an em dash rather than silently repeating the creation
                    time and making the two indistinguishable. */}
                <p class="mt-0.5 text-meta text-ink-subtle">
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
                    <p class="mt-0.5 text-meta leading-snug text-ink-muted">
                      Not sent — {describeSuppression(reason())}. Recorded rather than dropped, so
                      the audit trail is complete.
                    </p>
                  )}
                </Show>

                <Show when={n.delivery_summary}>
                  {(s) => (
                    <div class="mt-1 flex flex-wrap items-center gap-1">
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
    class={cx(
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
