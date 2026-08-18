/**
 * `/alerts/:id` — one alert, everything oto knows about it.
 *
 * The layout is a two-column split with the timeline on the left, because the
 * timeline is the answer to the question people arrive with ("what happened?")
 * and the labels, rule and delivery history are the supporting evidence.
 *
 * Every panel loads independently. A slow enrichment query must never hold the
 * timeline back, and a failed rule lookup must never blank the page — each one
 * renders its own error inside its own box, at its own size, so the page never
 * reflows around a failure.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import {
  getAlert,
  getAlertRuleHistory,
  listAlertEnrichments,
  listAlertEvents,
  listAlertNotifications,
  listAlertCases,
  listAlertSnoozes,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, TimelineQuery } from "~/api/types";
import {
  STATE_BAR,
  STATE_MEANING,
  SeverityMark,
  StateChip,
  normaliseSeverity,
} from "~/components/StateChip";
import { SnoozeChip } from "~/components/SnoozeChip";
import { Elapsed, RelativeTime } from "~/components/Time";
import { Chip, DataRow, PageHeading, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime, count as fmtCount, formatLabels } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";
import { AlertActions } from "~/features/alerts/detail/Actions";
import { DeliveryPanel } from "~/features/alerts/detail/DeliveryPanel";
import { EnrichmentPanel } from "~/features/alerts/detail/EnrichmentPanel";
import { CasePanel } from "~/features/alerts/detail/CasePanel";
import { PANEL_BODY, PANEL_HEADER } from "~/features/alerts/detail/rhythm";
import { RulePanel } from "~/features/alerts/detail/RuleDrift";
import { SnoozePanel } from "~/features/alerts/detail/SnoozePanel";
import { Timeline } from "~/features/alerts/detail/Timeline";
import { typesForCategories, type EventCategory } from "~/features/alerts/detail/eventKinds";

const TIMELINE_PAGE = 100;

export default function AlertDetailRoute() {
  const params = useParams<{ id: string }>();

  const alert = useQuery(() => ({
    queryKey: qk.alerts.detail(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => getAlert(params.id, { signal }),
  }));

  /* ---- timeline state ---------------------------------------------------- */

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
    const q: Record<string, unknown> = { limit: TIMELINE_PAGE, order: order() };
    // An empty category selection means "everything", which the contract
    // expresses by omitting `type` rather than by listing all 34 of them.
    if (categories().length > 0) q["type"] = [...typesForCategories(categories())];
    if (feed.cursor() !== null) q["cursor"] = feed.cursor();
    return q as TimelineQuery;
  });

  const timeline = useQuery(() => ({
    queryKey: qk.alerts.events(params.id, timelineQuery()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertEvents(params.id, timelineQuery(), { signal }),
    placeholderData: keepPrevious,
  }));

  /* ---- supporting panels ------------------------------------------------- */

  const rule = useQuery(() => ({
    queryKey: qk.alerts.rule(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => getAlertRuleHistory(params.id, {}, { signal }),
  }));

  const enrichments = useQuery(() => ({
    queryKey: qk.alerts.enrichments(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => listAlertEnrichments(params.id, { signal }),
  }));

  const notifications = useQuery(() => ({
    queryKey: qk.alerts.notifications(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertNotifications(params.id, {}, { signal }),
  }));

  const cases = useQuery(() => ({
    queryKey: qk.alerts.cases(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertCases(params.id, { limit: 50 }, { signal }),
  }));

  const snoozes = useQuery(() => ({
    queryKey: qk.alerts.snoozes(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) => listAlertSnoozes(params.id, {}, { signal }),
  }));

  return (
    <Switch>
      <Match when={alert.isPending}>
        <div class="space-y-md p-lg">
          <Skeleton class="h-6 w-80" />
          <Skeleton class="h-4 w-56" />
          <Skeleton class="h-64 w-full" />
        </div>
      </Match>

      <Match when={alert.isError}>
        <ErrorState error={alert.error} onRetry={() => void alert.refetch()} />
      </Match>

      <Match when={alert.data}>
        {(data) => (
          <div class="flex min-h-0 flex-1 flex-col">
            {/* ---- header -------------------------------------------------
                ONE focal element: the alert's name. It used to share a wrap row
                with five chips, which meant the eye landed on whichever of the
                six happened to be widest. Everything that is not the name is now
                on a line of its own below it and demoted by weight and colour —
                never by inflating the title, which is already at the top of the
                type scale and stays there. */}
            <header class="shrink-0 border-b border-line bg-surface">
              <div class="flex items-stretch gap-md px-lg pb-md pt-lg">
                {/* The state as a full-height rail rather than a stub beside the
                    title: it is the one thing here that has to be legible from
                    across a room, and (§0.6) it is deliberately the only
                    saturated pigment in this header. */}
                <div
                  class={cn("w-2xs shrink-0 rounded-full", STATE_BAR[data().state])}
                  aria-hidden="true"
                />

                <div class="min-w-0 flex-1">
                  {/* The swipe rather than the rule (§M.9): this heading is
                      alone on its line, so it has the room for a background
                      pass. `muted`, not `accent` — the accent draws focus rails
                      and links a few pixels away. */}
                  <PageHeading brush="swipe">{data().alertname}</PageHeading>

                  {/* The three orthogonal axes, on their own line so they read as
                      one group instead of competing with the name.

                      ⛔ AND `acked` IS NOT ONE OF THEM. An `AckChip` stood here,
                      fed from `current_case.ack_state`, which made a receipt on
                      one firing look like a property of the identity — an alert
                      that has fired forty times would have worn "Acked" because
                      somebody signed for the fortieth. A receipt is shown where it
                      was written: on the case, and the case panel below links to
                      it. The pulse on the state chip is a different thing — it
                      reads the open episode to decide how loudly to say `firing`,
                      and it never reports "acknowledged" as a state. */}
                  <div class="mt-sm flex flex-wrap items-center gap-sm">
                    <SeverityMark severity={data().severity} withLabel />
                    <StateChip
                      state={data().state}
                      urgent={
                        data().state === "firing" &&
                        (data().current_case?.ack_state ?? "unacked") === "unacked" &&
                        normaliseSeverity(data().severity) === "critical"
                      }
                    />
                    {/* ⛔ NO FLAPPING CHIP. `is_flapping` is still on the DTO and
                        no longer means anything live: an episode damped by the
                        case retention window W appends none of the `case.*`
                        events `flap_score` counts, so the flag reads false
                        exactly when an alert flaps (ADR 0041 Amendment 1). The
                        retired `alert.flapping_*` events still render on the
                        timeline below, because history is a different claim. */}
                    {/* Beside the state chip, never instead of it (§B.8.6): the
                        three axes are orthogonal, and a snoozed critical alert
                        still reads critical and still reads firing. */}
                    <SnoozeChip snooze={data().snooze ?? null} />
                  </div>

                  <p class="mt-sm text-body text-ink-muted">
                    {STATE_MEANING[data().state]}
                    <Show when={data().snooze}>
                      {" "}
                      oto is holding its own notifications about it — that is a fact about oto, not
                      about the signal.
                    </Show>
                  </p>

                  {/* Reference material. It is read when someone goes looking for
                      it, never at a glance, so it sits a colour step and a type
                      step below the sentence above it and is spaced widely enough
                      that each pair is picked out individually. */}
                  <div class="mt-md flex flex-wrap items-center gap-x-lg gap-y-2xs text-meta text-ink-subtle">
                    <span>
                      cluster <span class="font-mono text-ink-muted">{data().cluster_key}</span>
                    </span>
                    <Show when={data().namespace}>
                      <span>
                        namespace <span class="font-mono text-ink-muted">{data().namespace}</span>
                      </span>
                    </Show>
                    <Show when={data().service}>
                      <span>
                        service <span class="font-mono text-ink-muted">{data().service}</span>
                      </span>
                    </Show>
                    <span title={absoluteTime(data().first_seen_at)}>
                      first seen{" "}
                      <span class="text-ink-muted">
                        <RelativeTime value={data().first_seen_at} label="First seen" /> ago
                      </span>
                    </span>
                    <span title={absoluteTime(data().last_seen_at)}>
                      last seen{" "}
                      <span class="text-ink-muted">
                        <RelativeTime value={data().last_seen_at} label="Last seen" /> ago
                      </span>
                    </span>
                    {/* "Firing duration" — never MTTR (SCOPE-BOUNDARY). */}
                    <Show when={data().current_case}>
                      {(ac) => (
                        <span>
                          firing duration{" "}
                          <span class="text-ink-muted">
                            <Elapsed from={ac().started_at} to={ac().ended_at ?? null} />
                          </span>
                        </span>
                      )}
                    </Show>
                    <span title="Firing episodes — cases — since oto first saw this identity">
                      cases{" "}
                      <span class="text-ink-muted">{fmtCount(data().total_cases)}</span>
                    </span>
                  </div>
                </div>

                {/* Persistent, never hover-revealed (§0.4). It keeps its own
                    column so the buttons sit in the same place on every alert
                    regardless of how long the name is or how many chips it has. */}
                <div class="shrink-0">
                  <AlertActions alert={data()} />
                </div>
              </div>

              {/* Ancillary links and identity. Muted rather than accented, so the
                  state rail and the severity mark stay the most saturated things
                  on the screen (§0.6); the underline, not a hue, is what makes a
                  link a link. */}
              <div class="flex flex-wrap items-center gap-sm px-lg pb-lg">
                <Show when={data().generator_url}>
                  {(url) => (
                    <a
                      href={url()}
                      target="_blank"
                      rel="noreferrer noopener"
                      class="text-meta text-ink-muted underline decoration-line-strong underline-offset-2 hover:text-ink"
                    >
                      Open the query in Prometheus ↗
                    </a>
                  )}
                </Show>
                <Show when={data().group}>
                  {(group) => (
                    <A
                      href={`/groups/${group().id}`}
                      class="text-meta text-ink-muted underline decoration-line-strong underline-offset-2 hover:text-ink"
                      title="The notification group this alert's current firing was batched into. Alertmanager decided the batching; oto mirrors it."
                    >
                      Notified in: {group().title} ↗
                    </A>
                  )}
                </Show>
                <Show when={data().source}>
                  {(source) => <Chip title="The upstream that reported this">{source().name}</Chip>}
                </Show>
                <Chip mono title="oto's durable identity for this alert. Survives Alertmanager reloads.">
                  {data().alert_key.slice(0, 16)}…
                </Chip>
              </div>
            </header>

            {/* ---- body ---------------------------------------------------
                Two section groups (`gap-xl`) each holding panels a step closer
                together (`gap-lg`), so "these are two different kinds of thing"
                and "these are six panels of the same kind" are told apart by
                space alone rather than by a rule between them. */}
            <div class="grid min-h-0 flex-1 grid-cols-1 gap-xl overflow-auto p-lg xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)] xl:overflow-hidden">
              {/* Timeline */}
              <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
                <PanelHeader class={PANEL_HEADER}>
                  <PanelTitle>Timeline</PanelTitle>
                  <span class="text-meta text-ink-subtle">
                    displayed in the upstream's time · ordered by oto's
                  </span>
                </PanelHeader>
                <Switch>
                  <Match when={timeline.isPending && feed.rows().length === 0}>
                    <LoadingLine label="Reading the timeline…" />
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

              {/* Evidence */}
              <div class="flex min-h-0 flex-col gap-lg xl:overflow-auto">
                <LabelsPanel
                  labels={data().labels}
                  annotations={data().annotations}
                />

                {/* Each panel loads and fails on its own, and stays NAMED while
                    it does — a panel that vanished while loading would be
                    indistinguishable from one that has nothing to say. */}
                <Switch>
                  <Match when={rule.isPending}>
                    <Panel>
                      <PanelHeader class={PANEL_HEADER}>
                        <PanelTitle>Rule at fire time</PanelTitle>
                      </PanelHeader>
                      <LoadingLine />
                    </Panel>
                  </Match>
                  <Match when={rule.isError}>
                    <Panel>
                      <PanelHeader class={PANEL_HEADER}>
                        <PanelTitle>Rule at fire time</PanelTitle>
                      </PanelHeader>
                      <ErrorState error={rule.error} onRetry={() => void rule.refetch()} />
                    </Panel>
                  </Match>
                  <Match when={rule.data}>{(history) => <RulePanel history={history()} />}</Match>
                </Switch>

                <CasePanel
                  cases={cases.data?.data ?? []}
                  loading={cases.isPending}
                  error={cases.error}
                  currentId={data().current_case?.id ?? null}
                />

                <EnrichmentPanel
                  enrichments={enrichments.data?.data ?? []}
                  loading={enrichments.isPending}
                  error={enrichments.error}
                />

                <SnoozePanel
                  history={snoozes.data ?? []}
                  loading={snoozes.isPending}
                  error={snoozes.error}
                />

                <DeliveryPanel
                  notifications={notifications.data?.data ?? []}
                  summary={data().delivery_summary ?? null}
                  loading={notifications.isPending}
                  error={notifications.error}
                />
              </div>
            </div>
          </div>
        )}
      </Match>
    </Switch>
  );
}

/* -------------------------------------------------------------------------- */
/* Labels and annotations                                                     */
/* -------------------------------------------------------------------------- */

/**
 * Labels are identity, annotations are prose. They are shown separately because
 * confusing the two is how people end up filtering on a `summary`.
 */
function LabelsPanel(props: {
  readonly labels: Readonly<Record<string, string>>;
  readonly annotations: Readonly<Record<string, string>>;
}) {
  const labelEntries = createMemo(() =>
    Object.entries(props.labels).sort(([a], [b]) => a.localeCompare(b)),
  );
  const annotationEntries = createMemo(() =>
    Object.entries(props.annotations).sort(([a], [b]) => a.localeCompare(b)),
  );

  return (
    <Panel>
      <PanelHeader class={PANEL_HEADER}>
        <PanelTitle>Labels and annotations</PanelTitle>
        <button
          type="button"
          class="shrink-0 text-meta text-ink-subtle hover:text-ink hover:underline"
          onClick={() => void navigator.clipboard?.writeText(formatLabels(props.labels))}
          title="Copy as an Alertmanager matcher set"
        >
          Copy labels
        </button>
      </PanelHeader>

      <div class={PANEL_BODY}>
        <Show
          when={labelEntries().length > 0}
          fallback={<p class="text-body text-ink-subtle">This alert carries no labels.</p>}
        >
          <dl class="space-y-2xs">
            <For each={labelEntries()}>
              {([k, val]) => (
                <DataRow term={k}>
                  <span class="break-all font-mono text-body">{val}</span>
                </DataRow>
              )}
            </For>
          </dl>
        </Show>

        <Show when={annotationEntries().length > 0}>
          <div class="mt-md border-t border-line pt-md">
            <p class="mb-sm text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted">
              Annotations
            </p>
            <dl class="space-y-sm">
              <For each={annotationEntries()}>
                {([k, val]) => (
                  <div>
                    <dt class="text-meta text-ink-subtle">{k}</dt>
                    <dd class="whitespace-pre-wrap break-words text-body leading-snug text-ink">
                      {val}
                    </dd>
                  </div>
                )}
              </For>
            </dl>
          </div>
        </Show>
      </div>
    </Panel>
  );
}
