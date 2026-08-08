/**
 * `/groups/:id` — one group generation.
 *
 * The same timeline component the alert detail uses, because a group's history
 * is the same kind of thing: an ordered stream of immutable, attributed events.
 * Reusing it means the two screens can never drift in how they present a clock
 * skew or an unknown event type.
 *
 * "Who was told" is the interesting extra. A group generation is the thing oto
 * actually notifies about — the intents are keyed on it — so whether its fan-out
 * landed is a fact about THIS screen and nowhere else.
 *
 * This panel used to be "Where this is being narrated", listing chat threads
 * from `GroupDetailDTO.threads`. That field was in the contract, was rendered
 * here, and was emitted by no server code at all: there was no ChannelThreadDTO
 * in oto's Go tree, so the panel was permanently in its empty state and a group
 * being actively discussed in Slack looked identical to one nobody had been told
 * about. `delivery_summary` answers the question the panel was reaching for,
 * from data the group module can actually see.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import {
  ackAlertGroup,
  getAlertGroup,
  getAlertGroupTimeline,
  listAlertGroupAlerts,
  snoozeAlertGroup,
  unsnoozeAlertGroup,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, DeliverySummary, SnoozeRequest, TimelineQuery } from "~/api/types";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";
import { RelativeTime } from "~/components/Time";
import { AckChip, SeverityMark, STATE_BAR, StateChip, StormChip } from "~/components/StateChip";
import { Button, Chip, DataRow, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { ApiError } from "~/api/client";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { count as fmtCount, idempotencyKey } from "~/lib/format";
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
  const [cursor, setCursor] = createSignal<string | null>(null);
  const [events, setEvents] = createSignal<readonly AlertEvent[]>([]);

  const timelineQuery = createMemo<TimelineQuery>(() => {
    const q: Record<string, unknown> = { limit: 100, order: order() };
    if (categories().length > 0) q["type"] = [...typesForCategories(categories())];
    if (cursor() !== null) q["cursor"] = cursor();
    return q as TimelineQuery;
  });

  const timeline = useQuery(() => ({
    queryKey: qk.groups.timeline(params.id, timelineQuery()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      getAlertGroupTimeline(params.id, timelineQuery(), { signal }),
  }));

  const folded = createMemo<readonly AlertEvent[]>(() => {
    const page = timeline.data?.data ?? [];
    if (cursor() === null) return page;
    const seen = new Set(events().map((e) => e.id));
    return [...events(), ...page.filter((e) => !seen.has(e.id))];
  });

  /**
   * Acking a group is a **fan-out of the same primitive**, not a new one: one
   * receipt per currently-joined member. Alerts that join later are not
   * acknowledged, because a receipt is never predictive — and the copy says so.
   */
  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.groups.all() });
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  const ack = useMutation(() => ({
    mutationFn: () => ackAlertGroup(params.id, undefined, idempotencyKey()),
    onSuccess: invalidate,
  }));

  /**
   * Snoozing a group is the same fan-out: one quiet period per currently-joined
   * member. Alerts that join later are not snoozed, because a group-level mute
   * covering future members would silence alerts nobody has ever seen.
   */
  const [snoozeOpen, setSnoozeOpen] = createSignal(false);

  const unsnooze = useMutation(() => ({
    mutationFn: () => unsnoozeAlertGroup(params.id, undefined, idempotencyKey()),
    onSuccess: invalidate,
  }));

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
                    <h1 class="min-w-0 truncate text-[18px] font-semibold tracking-tight text-ink">
                      {g().title}
                    </h1>
                    <SeverityMark severity={g().severity} withLabel />
                    <Show when={g().storm_mode}>
                      <StormChip />
                    </Show>
                    <Chip title="A generation owns exactly one chat thread.">
                      generation {g().generation}
                    </Chip>
                    <Chip>{g().state}</Chip>
                  </div>

                  <div class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px] text-ink-muted">
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
                      <p class="mt-1 text-[11px] text-ink-subtle">
                        Alertmanager last said: <span class="font-mono">{reason()}</span>
                      </p>
                    )}
                  </Show>
                </div>

                <div class="flex shrink-0 flex-wrap items-center gap-2">
                  <Button
                    variant="primary"
                    size="sm"
                    busy={ack.isPending}
                    onClick={() => ack.mutate()}
                    title="Records a receipt on every member that has already joined. Members that join later are not included — a receipt is never predictive."
                  >
                    Acknowledge every current member
                  </Button>
                  <Button
                    size="sm"
                    onClick={() => setSnoozeOpen(true)}
                    title="Holds oto's own notifications for every member that has already joined. They keep firing, keep their severity, and stay in the alert list."
                  >
                    Snooze every current member
                  </Button>
                  <Button
                    size="sm"
                    busy={unsnooze.isPending}
                    onClick={() => unsnooze.mutate()}
                    title="Ends the quiet period on every currently-joined member. A member that is already awake is skipped rather than failing the request."
                  >
                    Resume notifications
                  </Button>
                </div>
              </div>

              <Show when={ack.error !== null}>
                <ErrorBanner error={ack.error} class="mt-2" />
              </Show>
              <Show when={unsnooze.error !== null}>
                <ErrorBanner class="mt-2">
                  {unsnooze.error instanceof ApiError && unsnooze.error.status === 412
                    ? "Nothing here was snoozed, so there was nothing to resume."
                    : ((unsnooze.error as Error | null)?.message ?? "")}
                </ErrorBanner>
              </Show>

              <SnoozeDialog
                open={snoozeOpen()}
                onClose={() => setSnoozeOpen(false)}
                subject="group"
                onSubmit={(body: SnoozeRequest, key: string) =>
                  snoozeAlertGroup(params.id, body, key)
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
                  <Match when={timeline.isPending && folded().length === 0}>
                    <LoadingLine />
                  </Match>
                  <Match when={timeline.isError}>
                    <ErrorState error={timeline.error} onRetry={() => void timeline.refetch()} />
                  </Match>
                  <Match when={true}>
                    <Timeline
                      events={folded()}
                      categories={categories()}
                      onCategoriesChange={setCategories}
                      order={order()}
                      onOrderChange={setOrder}
                      hasMore={timeline.data?.page.has_more ?? false}
                      loading={timeline.isFetching}
                      onLoadMore={() => {
                        setEvents(folded());
                        const next = timeline.data?.page.next_cursor;
                        if (typeof next === "string" && next !== "") setCursor(next);
                      }}
                    />
                  </Match>
                </Switch>
              </Panel>

              <div class="flex min-h-0 flex-col gap-4 xl:overflow-auto">
                {/* Members */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Members</PanelTitle>
                    <span class="text-[11px] text-ink-subtle">
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
                                  class={cx(
                                    "h-4 w-[3px] shrink-0 rounded-full",
                                    STATE_BAR[alert.state],
                                  )}
                                />
                                <SeverityMark severity={alert.severity} />
                                <span class="min-w-0 flex-1 truncate text-[12px] text-ink">
                                  {alert.alertname}
                                </span>
                                <StateChip state={alert.state} size="sm" />
                                <AckChip ackState={alert.ack_state} />
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

                {/* Group labels */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Group labels</PanelTitle>
                  </PanelHeader>
                  <div class="p-3">
                    <dl class="space-y-0.5">
                      <For each={Object.entries(g().group_labels)}>
                        {([k, val]) => (
                          <DataRow term={k}>
                            <span class="break-all font-mono text-[12px]">{val}</span>
                          </DataRow>
                        )}
                      </For>
                    </dl>
                    <Show when={g().source_group_key}>
                      {(key) => (
                        <p
                          class="mt-2 break-all border-t border-line pt-2 font-mono text-[10px] text-ink-subtle"
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
            <span class="ml-auto text-[11px] text-ink-subtle">
              last sent <RelativeTime value={at()} label="Last sent" /> ago
            </span>
          )}
        </Show>
      </div>

      <Show when={s().dead > 0}>
        <p class="mt-2 rounded-[4px] border border-line-strong border-l-[3px] border-l-ink bg-sunken px-2 py-1.5 text-[12px] font-medium leading-snug text-ink">
          {s().dead} {s().dead === 1 ? "delivery" : "deliveries"} gave up permanently. Nobody was
          told through {s().dead === 1 ? "that channel" : "those channels"}.
          {s().last_error_class ? ` Last error class: ${s().last_error_class}.` : ""}
        </p>
      </Show>

      <Show when={s().total === 0}>
        <p class="mt-2 text-[12px] leading-snug text-ink-muted">
          No notification was even attempted for this generation. That usually means no policy
          matched it — which is worth knowing, because it is indistinguishable from silence.
        </p>
      </Show>
    </div>
  );
};
