/**
 * `/groups/:id` — one AlertGroup generation.
 *
 * ⛔ AN AlertGroup IS ALERTMANAGER'S NOTIFICATION GROUPING, AND NOTHING MORE. It
 * is derived from `(source, receiver, groupLabels)`, it owns exactly one chat
 * thread, and every fact on this screen is a fact about that batching. It is not
 * a Case — a Case is one firing episode of ONE alert, and is what a human
 * acknowledges — and it is not a correlation or an incident: oto did not decide
 * which alerts belong together, Alertmanager's `group_by` did, and oto mirrors
 * the decision.
 *
 * ⛔ ACKNOWLEDGING IS NOT A GESTURE ABOUT THIS OBJECT. There is no receipt on a
 * group; a receipt belongs to a firing episode, and the control below is a
 * FAN-OUT that writes one onto each member's open case. It is kept here because
 * a storm of forty is the case for it — and it is labelled as what it does
 * rather than as "acknowledge this group", because a group is not a thing anyone
 * can have seen.
 *
 * ⛔ SNOOZE IS NOT HERE, AND ITS ABSENCE IS THE DECISION. A snooze holds oto's
 * notifications for an ALERT — an identity, which outlives every group it is
 * ever batched into. Offering it from a screen about one generation of one
 * notification batch invited an operator to believe they had quieted the batch,
 * when what they had quieted was every identity in it, indefinitely past the
 * moment the batch closed. It lives on the alert and on the case, where the
 * subject is named.
 *
 * The same timeline component the alert detail uses, because a group's history
 * is the same kind of thing: an ordered stream of immutable, attributed events.
 * Reusing it means the two screens can never drift in how they present a clock
 * skew or an unknown event type.
 *
 * "Who was told" is the interesting extra. A generation is the thing oto
 * actually notifies about — the intents are keyed on it — so whether its fan-out
 * landed is a fact about THIS screen and nowhere else.
 *
 * That panel used to be "Where this is being narrated", listing chat threads from
 * `GroupDetailDTO.threads`. That field was in the contract, was rendered here, and
 * was emitted by no server code at all: there was no ChannelThreadDTO in oto's Go
 * tree, so the panel was permanently in its empty state and a group being actively
 * discussed in Slack looked identical to one nobody had been told about.
 * `delivery_summary` answers the question the panel was reaching for, from data
 * the module can actually see.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useQuery, useQueryClient } from "@tanstack/solid-query";

import {
  ackAlertGroup,
  getAlertGroup,
  getAlertGroupTimeline,
  listAlertGroupAlerts,
  unackAlertGroup,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, DeliverySummary, TimelineQuery } from "~/api/types";
import { AckDialog } from "~/features/alerts/AckDialog";
import { RelativeTime } from "~/components/Time";
import { SeverityMark, STATE_BAR, StateChip } from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { Chip, DataRow, PageHeading, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { EmptyState, ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { count as fmtCount } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";
import { Timeline } from "~/features/alerts/detail/Timeline";
import { typesForCategories, type EventCategory } from "~/features/alerts/detail/eventKinds";

export default function GroupDetailRoute() {
  const params = useParams<{ id: string }>();
  const client = useQueryClient();

  const group = useQuery(() => ({
    queryKey: qk.groups.detail(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => getAlertGroup(params.id, { signal }),
  }));

  const members = useQuery(() => ({
    queryKey: qk.groups.alerts(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertGroupAlerts(params.id, { limit: 100 }, { signal }),
  }));

  const [categories, setCategories] = createSignal<readonly EventCategory[]>([]);
  const [order, setOrder] = createSignal<"asc" | "desc">("desc");

  // Any change of direction or event-kind filter invalidates every cursor
  // minted under the old one (§E.3), so both are the fingerprint — see
  // `createKeysetFeed` for why the reset must be a pure-phase derivation. The
  // annotation cuts the type-inference loop the closure creates: the feed
  // reads the query's envelope, and the query's key carries the feed's cursor.
  const feed: KeysetFeed<AlertEvent> = createKeysetFeed({
    envelope: () => timeline.data,
    isPlaceholder: () => timeline.isPlaceholderData,
    keyOf: (e) => e.id,
    fingerprint: () => `${order()}|${[...categories()].sort().join(",")}`,
  });

  const timelineQuery = createMemo<TimelineQuery>(() => {
    const q: Record<string, unknown> = { limit: 100, order: order() };
    if (categories().length > 0) q["type"] = [...typesForCategories(categories())];
    if (feed.cursor() !== null) q["cursor"] = feed.cursor();
    return q as TimelineQuery;
  });

  const timeline = useQuery(() => ({
    queryKey: qk.groups.timeline(params.id, timelineQuery()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      getAlertGroupTimeline(params.id, timelineQuery(), { signal }),
    placeholderData: keepPrevious,
  }));

  /**
   * The fan-out touches three lists: it writes a receipt onto each member's open
   * case, so the case queue, the alert list and this group's own roll-up are all
   * as stale as each other afterwards.
   */
  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.cases.all() });
    void client.invalidateQueries({ queryKey: qk.groups.all() });
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  /**
   * Acknowledging every member is a **fan-out of the case-scoped primitive**,
   * not a new one: one receipt per member whose case is still open. Alerts
   * notified under this group later are not acknowledged, because a receipt is
   * never predictive — and the copy says so.
   *
   * ⛔ IT GOES THROUGH A DIALOG, AND THE DIALOG IS THE POINT. This is the widest
   * gesture in the product: one click writes a receipt on every member, and
   * everybody else then reads "a human has seen this" about all of them. It used
   * to fire straight from the button with no note field and no confirmation,
   * which meant the operator could not say *why* — and the note is what whoever
   * reads the timeline next actually needs. `AckDialog` is shared with the case
   * surface so that one copy of the sentence explaining that a receipt does not
   * change the signal is the only copy there is.
   */
  const [ackOpen, setAckOpen] = createSignal(false);

  /**
   * ⛔ THE WAY BACK, AND IT IS NOT OPTIONAL. When the fan-out arrived it arrived
   * without its withdrawal: `POST /alert-groups/{id}/ack` was reachable and
   * `.../unack` did not exist, so the widest gesture in the product was also the
   * only irreversible one — an operator who acknowledged forty alerts could only
   * undo it by visiting forty screens. A control that writes a record on forty
   * signals must be able to unwrite it from the same place.
   *
   * It goes through the SAME dialog, in withdrawing mode, because the note matters
   * more here than it does on the way in: "un-acking, it's back" is the sentence
   * whoever reads the timeline next actually needs, and it lands on each member's
   * case rather than on the group, which has nowhere to keep one.
   */
  const [withdrawOpen, setWithdrawOpen] = createSignal(false);

  return (
    <Switch>
      <Match when={group.isPending}>
        <div class="space-y-3 p-4">
          <Skeleton class="h-6 w-96" />
          <Skeleton class="h-64 w-full" />
        </div>
      </Match>
      <Match when={group.isError}>
        <ErrorState error={group.error} onRetry={() => void group.refetch()} />
      </Match>
      <Match when={group.data}>
        {(g) => (
          <div class="flex min-h-0 flex-1 flex-col">
            <header class="shrink-0 border-b border-line bg-surface px-4 pb-2 pt-3">
              <div class="flex flex-wrap items-start gap-3">
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    {/* The rule rather than the swipe (§M.9): this heading
                        shares a wrap row with four chips, and a background pass
                        under a title with a severity mark beside it is one
                        texture too many. An underline is the quieter of the two
                        shapes and it is what that row can carry. */}
                    <PageHeading brush="rule">{g().title}</PageHeading>
                    <SeverityMark severity={g().severity} withLabel />
                    <Chip title="A generation owns exactly one chat thread.">
                      generation {g().generation}
                    </Chip>
                    <Chip>{g().state}</Chip>
                  </div>

                  <p class="mt-1 text-meta text-ink-subtle">
                    Alertmanager batched these alerts into one notification. oto mirrors that
                    decision; it did not make it.
                  </p>

                  <div class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-body text-ink-muted">
                    <span class="tabular-nums">{fmtCount(g().total_count)} members</span>
                    <span>{g().firing_count} firing</span>
                    <span>{g().acked_count} acknowledged</span>
                    {/* `cluster_key` is a first-class field on the group. It used
                        to be dug out of `group_labels["cluster"]`, which was only
                        ever a guess: Alertmanager's group labels are whatever the
                        route grouped on, and `cluster` need not be among them. */}
                    <span>
                      <span class="text-ink-subtle">cluster</span>{" "}
                      <span class="font-mono">{g().cluster_key}</span>
                    </span>
                    <Show when={g().receiver !== ""}>
                      <span>
                        <span class="text-ink-subtle">receiver</span>{" "}
                        <span class="font-mono">{g().receiver}</span>
                      </span>
                    </Show>
                    <span>
                      <span class="text-ink-subtle">last activity</span>{" "}
                      <RelativeTime value={g().last_activity_at} label="Last activity" /> ago
                    </span>
                  </div>

                  <Show when={g().last_notification_reason}>
                    {(reason) => (
                      <p class="mt-1 text-meta text-ink-subtle">
                        Alertmanager last said: <span class="font-mono">{reason()}</span>
                      </p>
                    )}
                  </Show>
                </div>

                <div class="flex shrink-0 flex-wrap items-center gap-2">
                  {/* ⛔ THE LABEL NAMES THE FAN-OUT, NOT THE GROUP. There is no
                      receipt on a group — the receipt goes onto each member's own
                      open case — and a button reading "Acknowledge this group"
                      would be claiming an object that does not exist. */}
                  <Button
                    size="sm"
                    onClick={() => setAckOpen(true)}
                    title="Writes one receipt onto each member's open case. Alerts notified under this group afterwards are not included — a receipt is never predictive."
                  >
                    Acknowledge every member's open case
                  </Button>
                  {/* ⛔ ALWAYS OFFERED, NEVER CONDITIONAL ON THE COUNT. `acked_count`
                      is a roll-up over cases and a partially-acked group is the
                      normal shape of one, so hiding this below some threshold would
                      hide the way back from exactly the operator who needs it. When
                      there is nothing to withdraw the server says so — the same
                      contract the ack has. */}
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => setWithdrawOpen(true)}
                    title="Removes the receipt from every member whose case is still open. A member that carries no receipt is skipped rather than failing the request."
                  >
                    Withdraw acknowledgement
                  </Button>
                </div>
              </div>

              {/* Both dialogs are shared with the case surface and both are
                  wired to the group-scoped fan-out endpoints. Neither runs its
                  own request: the screen supplies `onSubmit`, the dialog mints
                  the one idempotency key per gesture. */}
              <AckDialog
                open={ackOpen()}
                onClose={() => setAckOpen(false)}
                subject="group"
                onSubmit={(note: string | undefined, key: string) =>
                  ackAlertGroup(params.id, note, key)
                }
                onSuccess={invalidate}
              />
              {/* The same dialog, `withdrawing`, which is what gates it on the
                  contract's `UnackRequest` rather than `AckRequest`. Two endpoints
                  are two generated schemas even while they carry the same field. */}
              <AckDialog
                open={withdrawOpen()}
                onClose={() => setWithdrawOpen(false)}
                subject="group"
                withdrawing
                onSubmit={(note: string | undefined, key: string) =>
                  unackAlertGroup(params.id, note, key)
                }
                onSuccess={invalidate}
              />
            </header>

            <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-auto p-4 xl:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] xl:overflow-hidden">
              <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
                <PanelHeader>
                  <PanelTitle>Group timeline</PanelTitle>
                </PanelHeader>
                <Switch>
                  <Match when={timeline.isPending && feed.rows().length === 0}>
                    <LoadingLine />
                  </Match>
                  <Match when={timeline.isError}>
                    <ErrorState error={timeline.error} onRetry={() => void timeline.refetch()} />
                  </Match>
                  <Match when={true}>
                    <Timeline
                      events={feed.rows()}
                      categories={categories()}
                      onCategoriesChange={setCategories}
                      order={order()}
                      onOrderChange={setOrder}
                      hasMore={feed.hasMore()}
                      loading={timeline.isFetching}
                      onLoadMore={feed.loadMore}
                    />
                  </Match>
                </Switch>
              </Panel>

              <div class="flex min-h-0 flex-col gap-4 xl:overflow-auto">
                {/* Members */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Members</PanelTitle>
                    {/* ⭐ MEMBERSHIP IS DERIVED, NOT STORED, AND THIS LINK IS THE
                        HONEST WAY TO SAY SO. Nothing joins a group any more —
                        migration 00051 deleted `alert_group_members`, `joined_at`
                        and `left_at` — and a member is now exactly an alert whose
                        case carries this `group_id` and has not ended. So "the
                        members" and "this group's open cases" are the same set,
                        and `/cases?group_id=` is that set with the ack column on
                        it, which is the column somebody acting on this group
                        wants. */}
                    <A
                      href={`/cases?group_id=${params.id}`}
                      class="text-meta text-ink-muted underline decoration-line underline-offset-2 hover:text-ink"
                      title="The open case behind each member, in the list where acknowledgement is filterable."
                    >
                      as cases ↗
                    </A>
                    <span class="text-meta text-ink-subtle">
                      {members.data?.data.length ?? 0} loaded
                    </span>
                  </PanelHeader>
                  <Switch>
                    <Match when={members.isPending}>
                      <LoadingLine />
                    </Match>
                    <Match when={(members.data?.data.length ?? 0) === 0}>
                      <EmptyState title="No members." />
                    </Match>
                    <Match when={true}>
                      <ul>
                        <For each={members.data?.data ?? []}>
                          {(alert) => (
                            <li class="border-b border-line last:border-b-0">
                              <A
                                href={`/alerts/${alert.id}`}
                                class="flex items-center gap-2 px-3 py-1.5 hover:bg-raised"
                              >
                                <span
                                  aria-hidden="true"
                                  class={cn(
                                    "h-4 w-[3px] shrink-0 rounded-full",
                                    STATE_BAR[alert.state],
                                  )}
                                />
                                <SeverityMark severity={alert.severity} />
                                <span class="min-w-0 flex-1 truncate text-body text-ink">
                                  {alert.alertname}
                                </span>
                                <StateChip state={alert.state} size="sm" />
                                {/* ⛔ NO ACK CHIP HERE. An ack belongs to one
                                    case and this row is an Alert, which outlives
                                    its cases. The count above is case-scoped —
                                    it counts acknowledged cases — and the alert
                                    page one click away shows the receipt on the
                                    case that carries it. */}
                              </A>
                            </li>
                          )}
                        </For>
                      </ul>
                    </Match>
                  </Switch>
                </Panel>

                {/* Who was told */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Who was told</PanelTitle>
                  </PanelHeader>
                  <DeliveryRollup summary={g().delivery_summary} />
                </Panel>

                {/* The labels the group is keyed on.

                    ⛔ THE SUBTITLE IS NOT DECORATION. Calling these "Group labels"
                    with nothing beside it would say oto chose them, and oto
                    chooses nothing here: `group_by` in alertmanager.yml decides
                    what a group is, and oto mirrors the decision. */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Grouping labels</PanelTitle>
                    <span class="text-meta text-ink-subtle">Alertmanager grouped on these</span>
                  </PanelHeader>
                  <div class="p-3">
                    <dl class="space-y-0.5">
                      <For each={Object.entries(g().group_labels)}>
                        {([k, val]) => (
                          <DataRow term={k}>
                            <span class="break-all font-mono text-body">{val}</span>
                          </DataRow>
                        )}
                      </For>
                    </dl>
                    <Show when={g().source_group_key}>
                      {(key) => (
                        <p
                          class="mt-2 break-all border-t border-line pt-2 font-mono text-micro text-ink-subtle"
                          title="Alertmanager's own groupKey. Stored verbatim for observability only — it embeds the route path and changes on every config reload, so oto never parses it."
                        >
                          {key()}
                        </p>
                      )}
                    </Show>
                  </div>
                </Panel>
              </div>
            </div>
          </div>
        )}
      </Match>
    </Switch>
  );
}

/**
 * The generation's fan-out, in one line.
 *
 * An all-zero roll-up is an ANSWER — "nobody was told" — and is stated as a
 * sentence rather than five zeros, because a row of zeros reads as "no data
 * yet" and this screen's whole job is to distinguish the two.
 */
const DeliveryRollup = (props: { readonly summary: DeliverySummary }) => {
  const s = (): DeliverySummary => props.summary;
  return (
    <div class="px-3 py-2">
      <div class="flex flex-wrap items-center gap-1.5">
        <Chip>{s().sent} sent</Chip>
        <Chip>{s().pending} pending</Chip>
        <Chip>{s().skipped} skipped</Chip>
        <Chip>{s().failed} failed</Chip>
        <Chip>{s().dead} gave up</Chip>
        <Show when={s().last_sent_at}>
          {(at) => (
            <span class="ml-auto text-meta text-ink-subtle">
              last sent <RelativeTime value={at()} label="Last sent" /> ago
            </span>
          )}
        </Show>
      </div>

      <Show when={s().dead > 0}>
        <p class="mt-2 rounded-control border border-line-strong border-l-[3px] border-l-ink bg-sunken px-2 py-1.5 text-body font-medium leading-snug text-ink">
          {s().dead} {s().dead === 1 ? "delivery" : "deliveries"} gave up permanently. Nobody was
          told through {s().dead === 1 ? "that channel" : "those channels"}.
          {s().last_error_class ? ` Last error class: ${s().last_error_class}.` : ""}
        </p>
      </Show>

      <Show when={s().total === 0}>
        <p class="mt-2 text-body leading-snug text-ink-muted">
          No notification was even attempted for this generation. That usually means no policy
          matched it — which is worth knowing, because it is indistinguishable from silence.
        </p>
      </Show>
    </div>
  );
};
