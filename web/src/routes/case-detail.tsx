/**
 * `/cases/:id` — one firing episode of one alert.
 *
 * ⭐ THIS IS THE SCREEN AN OPERATOR ACTS ON, and the controls in its header are
 * why. A **Case** is one contiguous firing of one Alert, and it is the only
 * thing in oto a human can have *seen*: a receipt written here is a fact about
 * this firing, it clears itself when the next one opens, and it can never quietly
 * come to be about a different firing than the one that was looked at.
 *
 * ⛔ ACK IS THE ONLY VERB ON THIS SCREEN, AND SNOOZE IS DELIBERATELY NOT HERE.
 *
 *   - **Acknowledge** is case-scoped (`POST /api/v1/cases/{id}/ack`). It ends
 *     with this episode, which is exactly why it is written here.
 *   - **Snooze** is ALERT-scoped: it holds oto's notifications for the IDENTITY
 *     until a fixed time, so it outlives this case and applies to whatever fires
 *     next under the same labels. A hold that outlives the thing you took it from
 *     must be taken where its subject is on screen — the alert (`/alerts/:id`,
 *     where the bar offers it) — and ended from the **Quiet** tab, which is the
 *     list of what oto is currently not saying. Offering it from a case put an
 *     alert-scoped decision behind a case-shaped heading, and the panel below
 *     links out to the identity for exactly that reason.
 *
 * ⛔ THE ONE CONTROL HERE HAS ITS WAY BACK, and that is a rule rather than a
 * coincidence. A gesture that writes a record and cannot unwrite it leaves the
 * operator with nothing to do but be wrong in public — so the receipt is a
 * TOGGLE: one control, reading `Acknowledge` while this firing carries none and
 * the withdrawal's own words once it does.
 *
 * The same timeline component the alert detail uses, reading this episode's own
 * events (`GET /cases/{id}/events`) rather than the identity's whole history:
 * "what happened during this firing" is a different question from "what has this
 * rule ever done", and this screen only ever asks the first.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useQuery, useQueryClient } from "@tanstack/solid-query";

import { ackCase, getCase, getCaseTimeline, unackCase } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, TimelineQuery } from "~/api/types";
import { AckDialog } from "~/features/alerts/AckDialog";
import { Elapsed, RelativeTime } from "~/components/Time";
import { AckChip, CaseStateChip, SeverityMark, StateChip } from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { Chip, DataRow, PageHeading, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { absoluteTime } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";
import { EnrichmentPanel } from "~/features/alerts/detail/EnrichmentPanel";
import { Timeline } from "~/features/alerts/detail/Timeline";
import { typesForCategories, type EventCategory } from "~/features/alerts/detail/eventKinds";

export default function CaseDetailRoute() {
  const params = useParams<{ id: string }>();
  const client = useQueryClient();

  const detail = useQuery(() => ({
    queryKey: qk.cases.detail(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => getCase(params.id, { signal }),
  }));

  const [categories, setCategories] = createSignal<readonly EventCategory[]>([]);
  const [order, setOrder] = createSignal<"asc" | "desc">("desc");

  // Any change of direction or event-kind filter invalidates every cursor
  // minted under the old one (§E.3), so both are the fingerprint — see
  // `createKeysetFeed` for why the reset must be a pure-phase derivation.
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
    queryKey: qk.cases.timeline(params.id, timelineQuery()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      getCaseTimeline(params.id, timelineQuery(), { signal }),
    placeholderData: keepPrevious,
  }));

  /**
   * A receipt changes this case and the queue it is listed in — and the alert
   * lists too, because a row there renders its current episode's ack state.
   * Invalidating both is cheap, and a stale queue after an ack is exactly the row
   * an operator would act on twice.
   */
  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.cases.all() });
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  const [ackOpen, setAckOpen] = createSignal(false);

  return (
    <Switch>
      <Match when={detail.isPending}>
        <div class="space-y-3 p-4">
          <Skeleton class="h-6 w-96" />
          <Skeleton class="h-64 w-full" />
        </div>
      </Match>
      <Match when={detail.isError}>
        <ErrorState error={detail.error} onRetry={() => void detail.refetch()} />
      </Match>
      <Match when={detail.data}>
        {(c) => {
          /** The episode's own state is authoritative; `ended_at` only agrees. */
          const open = (): boolean => c().state === "open";
          /** Whether this firing already carries a receipt — the toggle's side. */
          const acked = (): boolean => c().ack_state === "acked";
          return (
            <div class="flex min-h-0 flex-1 flex-col">
              <header class="shrink-0 border-b border-line bg-surface px-4 pb-2 pt-3">
                <div class="flex flex-wrap items-start gap-3">
                  <div class="min-w-0 flex-1">
                    <div class="flex flex-wrap items-center gap-2">
                      {/* The rule rather than the swipe (§M.9): this heading
                          shares a wrap row with several chips, and a background
                          pass under a title with a severity mark beside it is
                          one texture too many. */}
                      <PageHeading brush="rule">
                        {c().alert?.alertname ?? "This firing"}
                      </PageHeading>
                      <SeverityMark severity={c().alert?.severity} withLabel />
                      {/* ⛔ THE HEADING'S CHIP IS THE EPISODE'S, NOT THE ALERT'S.
                          This screen's subject is one firing, and one firing is
                          either running or ended — `firing` and `suppressed` are
                          facts about the identity and are stated where the
                          identity is, in "The alert" panel's `state now`. A
                          four-colour alert badge up here would have been the
                          screen claiming a case can be suppressed. */}
                      <CaseStateChip state={c().state} resolveReason={c().resolve_reason} />
                      <AckChip ackState={c().ack_state} />
                      <Chip title="Which firing of this alert this is, counted since oto first saw the identity. A re-fire opens the next one rather than reopening this one.">
                        firing #{c().seq}
                      </Chip>
                    </div>

                    <p class="mt-1 text-meta text-ink-subtle">
                      One firing of one alert. It is what an acknowledgement is written on, it ends
                      when the alert resolves or oto stops hearing about it, and it never comes back
                      — the next fire is the next episode.
                    </p>

                    <div class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-body text-ink-muted">
                      <span title="How long the signal has been firing. oto times the signal, never anyone's response.">
                        <span class="text-ink-subtle">firing for</span>{" "}
                        <Elapsed from={c().started_at} to={c().ended_at ?? null} />
                      </span>
                      <span title={absoluteTime(c().started_at)}>
                        <span class="text-ink-subtle">started</span>{" "}
                        <RelativeTime value={c().started_at} label="Started" /> ago
                      </span>
                      <Show when={c().ended_at}>
                        {(at) => (
                          <span title={absoluteTime(at())}>
                            <span class="text-ink-subtle">ended</span>{" "}
                            <RelativeTime value={at()} label="Ended" /> ago
                          </span>
                        )}
                      </Show>
                      <Show when={c().alert}>
                        {(a) => (
                          <>
                            <span>
                              <span class="text-ink-subtle">cluster</span>{" "}
                              <span class="font-mono">{a().cluster_key}</span>
                            </span>
                            <Show when={a().namespace}>
                              {(ns) => (
                                <span>
                                  <span class="text-ink-subtle">namespace</span>{" "}
                                  <span class="font-mono">{ns()}</span>
                                </span>
                              )}
                            </Show>
                          </>
                        )}
                      </Show>
                    </div>

                    <Show when={c().ack_note}>
                      {(note) => (
                        <p class="mt-1.5 border-l-2 border-line-strong pl-2 text-body leading-snug text-ink-muted">
                          {note()}
                        </p>
                      )}
                    </Show>
                  </div>

                  <div class="flex shrink-0 flex-wrap items-center gap-2">
                    {/* ⭐ ONE CONTROL, NOT TWO, AND THE SINGLE CONTROL IS THE
                        HONEST SHAPE OF THE FACT. `ack_state` has two values, and
                        a pair of buttons meant one of them was always dead: an
                        `Acknowledge` greyed out beside an enabled `Withdraw`
                        asked the operator to read two controls to learn one
                        thing. This reads what the case IS and does the other
                        thing — the same way a play button becomes pause.

                        ⛔ AND IT STAYS OFFERED WHILE THE CASE IS OPEN, NEVER
                        GATED ON THE ACK STATE BEYOND CHOOSING ITS WORD. The
                        state on screen is as old as the last frame; if the case
                        moved underneath it, the server refuses in the dialog, in
                        the refused verb's own words. */}
                    <Button
                      variant={acked() ? "secondary" : "default"}
                      size="sm"
                      disabled={!open()}
                      onClick={() => setAckOpen(true)}
                      title={
                        !open()
                          ? "This case has already ended, so there is no receipt to write or take back."
                          : acked()
                            ? "Removes the receipt from this firing, recorded as a deliberate withdrawal."
                            : "Record that a human has seen this firing. It stays firing, at the same severity."
                      }
                    >
                      {acked() ? "Withdraw acknowledgement" : "Acknowledge"}
                    </Button>
                  </div>
                </div>

                {/* One dialog, shared with the rest of the product, and the mode
                    it opens in is the same read the button's word came from —
                    `withdrawing` is what gates it on the contract's
                    `UnackRequest` rather than `AckRequest`. It runs no request of
                    its own: the screen supplies `onSubmit`, the dialog mints the
                    one idempotency key per gesture. */}
                <AckDialog
                  open={ackOpen()}
                  onClose={() => setAckOpen(false)}
                  withdrawing={acked()}
                  onSubmit={(note: string | undefined, key: string) =>
                    acked() ? unackCase(params.id, note, key) : ackCase(params.id, note, key)
                  }
                  onSuccess={invalidate}
                />
              </header>

              <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-auto p-4 xl:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] xl:overflow-hidden">
                <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
                  <PanelHeader>
                    <PanelTitle>What happened during this firing</PanelTitle>
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
                  {/* The identity. Everything on this row describes the ALERT
                      and not the episode — which is exactly why it is a panel of
                      its own with a link out of it. */}
                  <Panel>
                    <PanelHeader>
                      <PanelTitle>The alert</PanelTitle>
                      <span class="text-meta text-ink-subtle">the identity, not this firing</span>
                    </PanelHeader>
                    <Show
                      when={c().alert}
                      fallback={
                        <p class="px-3 py-2 text-body text-ink-muted">
                          oto could not read the identity behind this firing.
                        </p>
                      }
                    >
                      {(a) => (
                        <div class="p-3">
                          <dl class="space-y-0.5">
                            <DataRow term="alertname">
                              <span class="break-all font-mono text-body">{a().alertname}</span>
                            </DataRow>
                            <DataRow term="cluster">
                              <span class="break-all font-mono text-body">{a().cluster_key}</span>
                            </DataRow>
                            <Show when={a().namespace}>
                              {(ns) => (
                                <DataRow term="namespace">
                                  <span class="break-all font-mono text-body">{ns()}</span>
                                </DataRow>
                              )}
                            </Show>
                            <DataRow term="state now">
                              <StateChip state={a().state} size="sm" />
                            </DataRow>
                          </dl>
                          <A
                            href={`/alerts/${a().id}`}
                            class="mt-2 inline-block text-body text-ink underline underline-offset-2 hover:text-ink-muted"
                          >
                            Every firing of this alert ↗
                          </A>
                        </div>
                      )}
                    </Show>
                  </Panel>


                  {/* The rule as it was AT THIS FIRING'S START — not as it is
                      now. That is the whole value of a snapshot: an episode from
                      six weeks ago still reports the threshold that was actually
                      in force then. */}
                  <Show when={c().rule}>
                    {(rule) => (
                      <Panel>
                        <PanelHeader>
                          <PanelTitle>The rule, as it was here</PanelTitle>
                          <span class="text-meta text-ink-subtle">captured at fire time</span>
                        </PanelHeader>
                        <div class="p-3">
                          <pre class="overflow-x-auto whitespace-pre-wrap break-all rounded-control bg-sunken p-2 font-mono text-meta text-ink">
                            {rule().expr}
                          </pre>
                          <dl class="mt-2 space-y-0.5">
                            <DataRow term="rule">
                              <span class="break-all font-mono text-body">{rule().rule_name}</span>
                            </DataRow>
                            <Show when={rule().for_seconds > 0}>
                              <DataRow term="for">
                                <span class="font-mono text-body">{rule().for_seconds}s</span>
                              </DataRow>
                            </Show>
                          </dl>
                        </div>
                      </Panel>
                    )}
                  </Show>

                  <EnrichmentPanel enrichments={c().enrichments} loading={false} error={null} />

                  {/* Who was told about this firing. */}
                  <Panel>
                    <PanelHeader>
                      <PanelTitle>Who was told</PanelTitle>
                    </PanelHeader>
                    <div class="px-3 py-2">
                      <div class="flex flex-wrap items-center gap-1.5">
                        <Chip>{c().delivery_summary.sent} sent</Chip>
                        <Chip>{c().delivery_summary.pending} pending</Chip>
                        <Chip>{c().delivery_summary.skipped} skipped</Chip>
                        <Chip>{c().delivery_summary.failed} failed</Chip>
                        <Chip>{c().delivery_summary.dead} gave up</Chip>
                      </div>
                      <Show when={c().delivery_summary.total === 0}>
                        <p class="mt-2 text-body leading-snug text-ink-muted">
                          No notification was even attempted for this firing. That usually means no
                          policy matched it — which is worth knowing, because it is
                          indistinguishable from silence.
                        </p>
                      </Show>
                    </div>
                  </Panel>

                  {/* The upstream objects doing the suppressing, when there are
                      any: `suppression_reason` says a silence is muting this,
                      and this says WHICH one — the half an operator can act on. */}
                  <Show when={c().suppression_reason}>
                    {(reason) => (
                      <Panel>
                        <PanelHeader>
                          <PanelTitle>Suppressed upstream</PanelTitle>
                        </PanelHeader>
                        <div class="p-3">
                          <p class="text-body leading-snug text-ink-muted">
                            Alertmanager is suppressing this firing:{" "}
                            <span class="font-mono">{reason()}</span>. It is still firing — the
                            suppression is about who gets told, not about the signal.
                          </p>
                          <For
                            each={[
                              ...(c().suppressed_by?.silenced_by ?? []),
                              ...(c().suppressed_by?.inhibited_by ?? []),
                              ...(c().suppressed_by?.muted_by ?? []),
                            ]}
                          >
                            {(id) => (
                              <p class="mt-1 break-all font-mono text-meta text-ink-subtle">{id}</p>
                            )}
                          </For>
                        </div>
                      </Panel>
                    )}
                  </Show>
                </div>
              </div>
            </div>
          );
        }}
      </Match>
    </Switch>
  );
}
