/**
 * `/cases/:id` — one case.
 *
 * ⭐ THIS IS THE SCREEN AN OPERATOR ACTS ON, and the controls in its header are
 * why. Acknowledging and snoozing used to live on the alert detail, one
 * signal at a time; they are here now, wired to the case-scoped endpoints
 * (`POST /api/v1/alert-groups/{id}/ack|unack|snooze|unsnooze`), because a case is
 * the unit a human responds to. Forty pods crash-looping is one thing happening, and
 * a receipt written on one of the forty is a receipt everybody else misreads.
 *
 * ⛔ EVERY CONTROL HERE HAS ITS WAY BACK, and that is a rule rather than a
 * coincidence. When these moved off the alert detail the ack arrived without one:
 * `unack` was case-scoped nowhere, so the widest gesture in the product was also its
 * only irreversible one and the route back was forty alert pages.
 *
 * ⛔ "CASE" HERE IS THE CORRELATION — one generation of one Alertmanager
 * notification group, derived from `(source, receiver, groupLabels)` and owning
 * exactly one chat thread. It is NOT `AlertCase` (`internal/alerts/domain/case.go`),
 * which is one ALERT'S FIRING EPISODE. The API and the database still say "group"
 * for this object and "case" for the episode; the collision is accepted, and it is
 * kept off the screen by never calling an episode a case in any string a person
 * reads. Where this screen touches the episode — the members list's ack note — it
 * says *episode*.
 *
 * The same timeline component the alert detail uses, because a case's history is
 * the same kind of thing: an ordered stream of immutable, attributed events.
 * Reusing it means the two screens can never drift in how they present a clock
 * skew or an unknown event type.
 *
 * "Who was told" is the interesting extra. A case is the thing oto actually
 * notifies about — the intents are keyed on it — so whether its fan-out landed is
 * a fact about THIS screen and nowhere else.
 *
 * That panel used to be "Where this is being narrated", listing chat threads from
 * `GroupDetailDTO.threads`. That field was in the contract, was rendered here, and
 * was emitted by no server code at all: there was no ChannelThreadDTO in oto's Go
 * tree, so the panel was permanently in its empty state and a case being actively
 * discussed in Slack looked identical to one nobody had been told about.
 * `delivery_summary` answers the question the panel was reaching for, from data
 * the module can actually see.
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
  unackAlertGroup,
  unsnoozeAlertGroup,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, DeliverySummary, SnoozeRequest, TimelineQuery } from "~/api/types";
import { AckDialog } from "~/features/alerts/AckDialog";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";
import { RelativeTime } from "~/components/Time";
import { SeverityMark, STATE_BAR, StateChip, StormChip } from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { Chip, DataRow, PageHeading, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ApiError } from "~/api/client";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { count as fmtCount, idempotencyKey } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";
import { Timeline } from "~/features/alerts/detail/Timeline";
import { typesForCategories, type EventCategory } from "~/features/alerts/detail/eventKinds";

export default function CaseDetailRoute() {
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
   * Both mutations touch both lists: a case-wide ack changes every member's
   * receipt, so the alert list is as stale as the case list afterwards.
   */
  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.groups.all() });
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  /**
   * Acking a case is a **fan-out of the per-alert primitive**, not a new one: one
   * receipt per currently-joined member. Alerts that join later are not
   * acknowledged, because a receipt is never predictive — and the copy says so.
   *
   * ⛔ IT GOES THROUGH A DIALOG, AND THE DIALOG IS THE POINT. This is the widest
   * gesture in the product: one click writes a receipt on every member, and
   * everybody else then reads "a human has seen this" about all of them. It used
   * to fire straight from the button with no note field and no confirmation,
   * which meant the operator could not say *why* — and the note is what whoever
   * reads the timeline next actually needs. `AckDialog` is shared with the alert
   * surface for the same reason `SnoozeDialog` is: one copy of the sentence
   * explaining that a receipt does not change the signal.
   */
  const [ackOpen, setAckOpen] = createSignal(false);

  /**
   * ⛔ THE WAY BACK, AND IT IS NOT OPTIONAL. When acknowledging moved onto this
   * screen it arrived without its withdrawal: the case-scoped ack was reachable and
   * `POST /alert-groups/{id}/unack` did not exist, so the widest gesture in the
   * product was also the only irreversible one — an operator who acknowledged forty
   * alerts could only undo it by visiting forty alert pages. A control that writes a
   * record on forty signals must be able to unwrite it from the same place.
   *
   * It goes through the SAME dialog, in withdrawing mode, because the note matters
   * more here than it does on the way in: "un-acking, it's back" is the sentence
   * whoever reads the timeline next actually needs, and it lands on each member's
   * timeline rather than on the case, which has nowhere left to keep it.
   */
  const [withdrawOpen, setWithdrawOpen] = createSignal(false);

  /**
   * Snoozing a case is the same fan-out: one quiet period per currently-joined
   * member. Alerts that join later are not snoozed, because a case-level mute
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
                    {/* The rule rather than the swipe (§M.9): this heading
                        shares a wrap row with four chips, and a background pass
                        under a title with a severity mark beside it is one
                        texture too many. An underline is the quieter of the two
                        shapes and it is what that row can carry. */}
                    <PageHeading brush="rule">{g().title}</PageHeading>
                    <SeverityMark severity={g().severity} withLabel />
                    <Show when={g().storm_mode}>
                      <StormChip />
                    </Show>
                    <Chip title="A generation owns exactly one chat thread.">
                      generation {g().generation}
                    </Chip>
                    <Chip>{g().state}</Chip>
                  </div>

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
                  <Button
                    size="sm"
                    onClick={() => setAckOpen(true)}
                    title="Records a receipt on every member that has already joined. Members that join later are not included — a receipt is never predictive."
                  >
                    Acknowledge every current member
                  </Button>
                  {/* ⛔ ALWAYS OFFERED, NEVER CONDITIONAL ON THE COUNT. `acked_count`
                      is a roll-up over episodes and a partially-acked case is the
                      normal shape of one, so hiding this below some threshold would
                      hide the way back from exactly the operator who needs it. When
                      there is nothing to withdraw the server says so — the same
                      contract the ack has. */}
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => setWithdrawOpen(true)}
                    title="Removes the receipt from every member whose episode is still open. A member that carries no receipt is skipped rather than failing the request."
                  >
                    Withdraw acknowledgement
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => setSnoozeOpen(true)}
                    title="Holds oto's own notifications for every member that has already joined. They keep firing, keep their severity, and stay in the alert list."
                  >
                    Snooze every current member
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    busy={unsnooze.isPending}
                    onClick={() => unsnooze.mutate()}
                    title="Ends the quiet period on every currently-joined member. A member that is already awake is skipped rather than failing the request."
                  >
                    Resume notifications
                  </Button>
                </div>
              </div>

              <Show when={unsnooze.error !== null}>
                <ErrorBanner class="mt-2">
                  {unsnooze.error instanceof ApiError && unsnooze.error.status === 412
                    ? "Nothing here was snoozed, so there was nothing to resume."
                    : ((unsnooze.error as Error | null)?.message ?? "")}
                </ErrorBanner>
              </Show>

              {/* Three dialogs, all shared with the alert surface, all wired to
                  the case-scoped endpoints. None runs its own request: the
                  screen supplies `onSubmit`, the dialog mints the one
                  idempotency key per gesture. */}
              <AckDialog
                open={ackOpen()}
                onClose={() => setAckOpen(false)}
                subject="case"
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
                subject="case"
                withdrawing
                onSubmit={(note: string | undefined, key: string) =>
                  unackAlertGroup(params.id, note, key)
                }
                onSuccess={invalidate}
              />
              <SnoozeDialog
                open={snoozeOpen()}
                onClose={() => setSnoozeOpen(false)}
                subject="case"
                onSubmit={(body: SnoozeRequest, key: string) =>
                  snoozeAlertGroup(params.id, body, key)
                }
                onSuccess={invalidate}
              />
            </header>

            <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-auto p-4 xl:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] xl:overflow-hidden">
              <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
                <PanelHeader>
                  <PanelTitle>Case timeline</PanelTitle>
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
                                    firing episode and this row is an Alert, which
                                    outlives its episodes. The count above is
                                    episode-scoped — it counts acknowledged firing
                                    episodes — and the alert page one click away
                                    shows the episode's receipt. */}
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

                {/* The labels the case is keyed on.

                    ⛔ THE SUBTITLE IS NOT DECORATION. Calling these "Case labels"
                    with nothing beside it would say oto chose them, and oto
                    chooses nothing here: `group_by` in alertmanager.yml decides
                    what a case is, and oto mirrors the decision. The panel has to
                    keep saying whose labels these are now that the title no
                    longer does. */}
                <Panel>
                  <PanelHeader>
                    <PanelTitle>Case labels</PanelTitle>
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
