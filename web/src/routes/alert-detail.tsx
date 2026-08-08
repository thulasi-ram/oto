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
import { For, Match, Show, Switch, createEffect, createMemo, createSignal } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import {
  getAlert,
  getAlertRuleHistory,
  listAlertEnrichments,
  listAlertEvents,
  listAlertNotifications,
  listAlertOccurrences,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertEvent, TimelineQuery } from "~/api/types";
import {
  AckChip,
  FlappingChip,
  STATE_BAR,
  STATE_MEANING,
  SeverityMark,
  StateChip,
  normaliseSeverity,
} from "~/components/StateChip";
import { Elapsed, RelativeTime } from "~/components/Time";
import { Chip, DataRow, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { ErrorState, LoadingLine, Skeleton } from "~/components/ui/states";
import { absoluteTime, count as fmtCount, formatLabels } from "~/lib/format";
import { AlertActions } from "~/features/alerts/detail/Actions";
import { DeliveryPanel } from "~/features/alerts/detail/DeliveryPanel";
import { EnrichmentPanel } from "~/features/alerts/detail/EnrichmentPanel";
import { OccurrencePanel } from "~/features/alerts/detail/OccurrencePanel";
import { RulePanel } from "~/features/alerts/detail/RuleDrift";
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
  const [cursor, setCursor] = createSignal<string | null>(null);
  const [events, setEvents] = createSignal<readonly AlertEvent[]>([]);

  const timelineQuery = createMemo<TimelineQuery>(() => {
    const q: Record<string, unknown> = { limit: TIMELINE_PAGE, order: order() };
    // An empty category selection means "everything", which the contract
    // expresses by omitting `type` rather than by listing all 34 of them.
    if (categories().length > 0) q["type"] = [...typesForCategories(categories())];
    if (cursor() !== null) q["cursor"] = cursor();
    return q as TimelineQuery;
  });

  const timeline = useQuery(() => ({
    queryKey: qk.alerts.events(params.id, timelineQuery()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertEvents(params.id, timelineQuery(), { signal }),
  }));

  // Any change of direction or event-kind filter invalidates every cursor
  // minted under the old one, so the fold resets. Doing it in an effect rather
  // than inside the memo keeps the memo pure and the reset observable.
  const timelineFingerprint = createMemo(() => `${order()}|${[...categories()].sort().join(",")}`);
  createEffect((previous: string | undefined) => {
    const current = timelineFingerprint();
    if (previous !== undefined && previous !== current) {
      setCursor(null);
      setEvents([]);
    }
    return current;
  });

  const foldedEvents = createMemo<readonly AlertEvent[]>(() => {
    const page = timeline.data?.data ?? [];
    if (cursor() === null) return page;
    const seen = new Set(events().map((e) => e.id));
    return [...events(), ...page.filter((e) => !seen.has(e.id))];
  });

  const loadMoreEvents = (): void => {
    setEvents(foldedEvents());
    const next = timeline.data?.page.next_cursor;
    if (typeof next === "string" && next !== "") setCursor(next);
  };

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

  const occurrences = useQuery(() => ({
    queryKey: qk.alerts.occurrences(params.id),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertOccurrences(params.id, { limit: 50 }, { signal }),
  }));

  return (
    <Switch>
      <Match when={alert.isPending}>
        <div class="space-y-3 p-4">
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
            {/* ---- header ------------------------------------------------- */}
            <header class="shrink-0 border-b border-line bg-surface">
              <div class="flex items-start gap-3 px-4 pb-2 pt-3">
                <div
                  class={cx("mt-1 h-9 w-[3px] shrink-0 rounded-full", STATE_BAR[data().state])}
                  aria-hidden="true"
                />

                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <h1 class="min-w-0 truncate text-[18px] font-semibold tracking-tight text-ink">
                      {data().alertname}
                    </h1>
                    <SeverityMark severity={data().severity} withLabel />
                    <StateChip
                      state={data().state}
                      urgent={
                        data().state === "firing" &&
                        data().ack_state === "unacked" &&
                        normaliseSeverity(data().severity) === "critical"
                      }
                    />
                    <AckChip ackState={data().ack_state} />
                    <Show when={data().is_flapping}>
                      <FlappingChip />
                    </Show>
                  </div>

                  <p class="mt-0.5 text-[12px] text-ink-muted">
                    {STATE_MEANING[data().state]}
                  </p>

                  <div class="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px] text-ink-muted">
                    <span>
                      <span class="text-ink-subtle">cluster</span>{" "}
                      <span class="font-mono">{data().cluster_key}</span>
                    </span>
                    <Show when={data().namespace}>
                      <span>
                        <span class="text-ink-subtle">namespace</span>{" "}
                        <span class="font-mono">{data().namespace}</span>
                      </span>
                    </Show>
                    <Show when={data().service}>
                      <span>
                        <span class="text-ink-subtle">service</span>{" "}
                        <span class="font-mono">{data().service}</span>
                      </span>
                    </Show>
                    <span title={absoluteTime(data().first_seen_at)}>
                      <span class="text-ink-subtle">first seen</span>{" "}
                      <RelativeTime value={data().first_seen_at} label="First seen" /> ago
                    </span>
                    <span title={absoluteTime(data().last_seen_at)}>
                      <span class="text-ink-subtle">last seen</span>{" "}
                      <RelativeTime value={data().last_seen_at} label="Last seen" /> ago
                    </span>
                    {/* "Firing duration" — never MTTR (SCOPE-BOUNDARY). */}
                    <Show when={data().current_occurrence}>
                      {(occ) => (
                        <span>
                          <span class="text-ink-subtle">firing duration</span>{" "}
                          <Elapsed from={occ().started_at} to={occ().ended_at ?? null} />
                        </span>
                      )}
                    </Show>
                    <span title="Firing episodes since oto first saw this identity">
                      <span class="text-ink-subtle">episodes</span>{" "}
                      {fmtCount(data().total_occurrences)}
                    </span>
                    <Show when={data().is_flapping || data().flap_score > 0}>
                      <span title="EWMA of state transitions per hour. A derived signal, never a state.">
                        <span class="text-ink-subtle">flap score</span>{" "}
                        {data().flap_score.toFixed(1)}
                      </span>
                    </Show>
                  </div>
                </div>

                <div class="shrink-0">
                  <AlertActions alert={data()} />
                </div>
              </div>

              <div class="flex flex-wrap items-center gap-2 px-4 pb-2">
                <Show when={data().generator_url}>
                  {(url) => (
                    <a
                      href={url()}
                      target="_blank"
                      rel="noreferrer noopener"
                      class="text-[12px] text-accent hover:underline"
                    >
                      Open the query in Prometheus ↗
                    </a>
                  )}
                </Show>
                <Show when={data().group}>
                  {(group) => (
                    <A
                      href={`/groups/${group().id}`}
                      class="text-[12px] text-accent hover:underline"
                      title="The Alertmanager notification group this episode joined"
                    >
                      In group: {group().title} ↗
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

            {/* ---- body --------------------------------------------------- */}
            <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-auto p-4 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)] xl:overflow-hidden">
              {/* Timeline */}
              <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
                <PanelHeader>
                  <PanelTitle>Timeline</PanelTitle>
                  <span class="text-[11px] text-ink-subtle">
                    displayed in the upstream's time · ordered by oto's
                  </span>
                </PanelHeader>
                <Switch>
                  <Match when={timeline.isPending && foldedEvents().length === 0}>
                    <LoadingLine label="Reading the timeline…" />
                  </Match>
                  <Match when={timeline.isError}>
                    <ErrorState error={timeline.error} onRetry={() => void timeline.refetch()} />
                  </Match>
                  <Match when={true}>
                    <Timeline
                      events={foldedEvents()}
                      categories={categories()}
                      onCategoriesChange={setCategories}
                      order={order()}
                      onOrderChange={setOrder}
                      hasMore={timeline.data?.page.has_more ?? false}
                      loading={timeline.isFetching}
                      onLoadMore={loadMoreEvents}
                    />
                  </Match>
                </Switch>
              </Panel>

              {/* Evidence */}
              <div class="flex min-h-0 flex-col gap-4 xl:overflow-auto">
                <LabelsPanel
                  labels={data().labels}
                  annotations={data().annotations}
                />

                <Switch>
                  <Match when={rule.isPending}>
                    <Panel>
                      <PanelHeader>
                        <PanelTitle>Rule at fire time</PanelTitle>
                      </PanelHeader>
                      <LoadingLine />
                    </Panel>
                  </Match>
                  <Match when={rule.isError}>
                    <Panel>
                      <PanelHeader>
                        <PanelTitle>Rule at fire time</PanelTitle>
                      </PanelHeader>
                      <ErrorState error={rule.error} onRetry={() => void rule.refetch()} />
                    </Panel>
                  </Match>
                  <Match when={rule.data}>{(history) => <RulePanel history={history()} />}</Match>
                </Switch>

                <OccurrencePanel
                  occurrences={occurrences.data?.data ?? []}
                  loading={occurrences.isPending}
                  error={occurrences.error}
                  currentId={data().current_occurrence?.id ?? null}
                />

                <EnrichmentPanel
                  enrichments={enrichments.data?.data ?? []}
                  loading={enrichments.isPending}
                  error={enrichments.error}
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
      <PanelHeader>
        <PanelTitle>Labels and annotations</PanelTitle>
        <button
          type="button"
          class="text-[11px] text-ink-subtle hover:text-ink hover:underline"
          onClick={() => void navigator.clipboard?.writeText(formatLabels(props.labels))}
          title="Copy as an Alertmanager matcher set"
        >
          Copy labels
        </button>
      </PanelHeader>

      <div class="p-3">
        <Show
          when={labelEntries().length > 0}
          fallback={<p class="text-[12px] text-ink-subtle">This alert carries no labels.</p>}
        >
          <dl class="space-y-0.5">
            <For each={labelEntries()}>
              {([k, val]) => (
                <DataRow term={k}>
                  <span class="break-all font-mono text-[12px]">{val}</span>
                </DataRow>
              )}
            </For>
          </dl>
        </Show>

        <Show when={annotationEntries().length > 0}>
          <div class="mt-3 border-t border-line pt-3">
            <p class="mb-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted">
              Annotations
            </p>
            <dl class="space-y-1.5">
              <For each={annotationEntries()}>
                {([k, val]) => (
                  <div>
                    <dt class="text-[11px] text-ink-subtle">{k}</dt>
                    <dd class="whitespace-pre-wrap break-words text-[12px] leading-snug text-ink">
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
