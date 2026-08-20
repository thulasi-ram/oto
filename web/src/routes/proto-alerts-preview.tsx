/**
 * `/proto/alerts-preview` — the redesigned alert surfaces, drawn against
 * fixtures, with no session and no backend.
 *
 * ⛔ IT IS OUTSIDE THE AUTHENTICATED LAYOUT ROUTE, exactly as
 * `/proto/linear-issues` is, and for the same reason: it must open on a laptop
 * with nothing running on :8080. `RequireSession` would hold the whole tree
 * behind a `/me` probe that can only 401, and `LiveProvider` would open an SSE
 * connection nobody can serve.
 *
 * ⭐ IT RENDERS THE SHIPPING COMPONENTS, NEVER COPIES OF THEM. `FilterBar`,
 * `AlertTable`, `GroupedAlerts`, `AlertActions`, `Timeline`, `RulePanel`,
 * `CasePanel`, `EnrichmentPanel`, `SnoozePanel` and `DeliveryPanel` are
 * imported and fed props — so what a reviewer sees here is what an operator will
 * see. What IS restated below is the route-level markup those components sit in
 * (the column frame from `routes/alerts.tsx`, the detail header from
 * `routes/alert-detail.tsx`), because that markup belongs to a route and there
 * is no component to import. That frame is one column since ADR 0033 — the
 * filters are a band above the table rather than a pane beside it.
 *
 * The only stub is a second `QueryClient`. `FilterBar` reads the cluster list
 * and the label-name typeahead through TanStack, so this file mounts a nested
 * provider whose cache is pre-seeded and whose `staleTime` is `Infinity` — the
 * queries resolve from memory and no request is ever made. Nothing in the
 * components was changed to accommodate it.
 *
 * Never imports from the Linear prototype feature and never touches its private
 * CSS variables. This screen is oto's own tokens or nothing.
 */
import {
  For,
  Show,
  createMemo,
  createSignal,
  type Component,
  type JSX,
  type ParentComponent,
} from "solid-js";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";

import { clustersQuery, labelNamesQuery } from "~/api/queries";
import type { Alert, ListEnvelope, Cluster, State } from "~/api/types";
import {
  STATE_BAR,
  STATE_MEANING,
  SeverityMark,
  StateChip,
  normaliseSeverity,
} from "~/components/StateChip";
import { SnoozeChip } from "~/components/SnoozeChip";
import { Elapsed, RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { Chip, DataRow, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { setTheme, theme } from "~/design/theme";
import { cn } from "~/lib/cn";
import { absoluteTime, count as fmtCount, formatLabels } from "~/lib/format";
import { AlertTable } from "~/features/alerts/AlertTable";
import { FilterBar } from "~/features/alerts/FilterBar";
import { GroupedAlerts } from "~/features/alerts/GroupedAlerts";
import { AlertActions } from "~/features/alerts/detail/Actions";
import { DeliveryPanel } from "~/features/alerts/detail/DeliveryPanel";
import { EnrichmentPanel } from "~/features/alerts/detail/EnrichmentPanel";
import { CasePanel } from "~/features/alerts/detail/CasePanel";
import { PANEL_BODY, PANEL_HEADER } from "~/features/alerts/detail/rhythm";
import { RulePanel } from "~/features/alerts/detail/RuleDrift";
import { SnoozePanel } from "~/features/alerts/detail/SnoozePanel";
import { Timeline } from "~/features/alerts/detail/Timeline";
import type { EventCategory } from "~/features/alerts/detail/eventKinds";
import { DEFAULT_FILTERS, withMatcher, type AlertFilters } from "~/features/alerts/filters";
import {
  PREVIEW_ALERTS,
  PREVIEW_CLUSTERS,
  PREVIEW_DETAIL,
  PREVIEW_ENRICHMENTS,
  PREVIEW_EVENTS,
  PREVIEW_LABEL_NAMES,
  PREVIEW_NOTIFICATIONS,
  PREVIEW_CASES,
  PREVIEW_ROLLUPS,
  PREVIEW_RULE_HISTORY,
  PREVIEW_RULE_SNAPSHOTS,
  PREVIEW_SNOOZE_HISTORY,
} from "~/features/alerts/previewFixtures";

/* -------------------------------------------------------------------------- */
/* The stub: a cache that already has every answer                            */
/* -------------------------------------------------------------------------- */

/**
 * `staleTime: Infinity` is the whole mechanism. A seeded entry that can go stale
 * would be refetched on mount, the refetch would fail against nothing, and the
 * filter panel would lose its cluster list the moment it appeared — which is the
 * one thing a credential-free preview must not do.
 *
 * Nothing below writes a `qk` key. `FilterBar` reads clusters and label names
 * through `clustersQuery()`/`labelNamesQuery()` — the factories that already own
 * those keys (`api/queries.ts`) — so this file asks them for the key instead of
 * restating it. `queryFn` on `defaultOptions` states the same promise once for
 * the client rather than once per entry: whatever is mounted here answers from
 * this fixture table or answers with nothing, and never reaches a backend
 * nobody is running.
 */
const clusterEnvelope: ListEnvelope<Cluster> = {
  data: [...PREVIEW_CLUSTERS],
  page: { has_more: false, limit: 100, next_cursor: null },
  meta: { request_id: "preview", elapsed_ms: 0 },
};

type PreviewFixture = readonly [key: readonly unknown[], data: unknown];

const PREVIEW_FIXTURES: readonly PreviewFixture[] = [
  [clustersQuery().queryKey, clusterEnvelope],
  [labelNamesQuery().queryKey, [...PREVIEW_LABEL_NAMES]],
];

function fixtureFor(key: readonly unknown[]): unknown {
  const hit = PREVIEW_FIXTURES.find((fixture) => JSON.stringify(fixture[0]) === JSON.stringify(key));
  return hit?.[1];
}

const previewClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: Infinity,
      gcTime: Infinity,
      retry: false,
      refetchOnMount: false,
      refetchOnWindowFocus: false,
      refetchOnReconnect: false,
      queryFn: ({ queryKey }) => fixtureFor(queryKey),
    },
    mutations: { retry: 0 },
  },
});

for (const [key, data] of PREVIEW_FIXTURES) previewClient.setQueryData(key, data);

/* -------------------------------------------------------------------------- */
/* A filter set that actually does something                                  */
/* -------------------------------------------------------------------------- */

/**
 * A deliberately small subset of `compileFilters`, applied in the browser.
 *
 * The real screen sends every one of these to the server, and this preview has
 * no server — but a filter panel whose controls change nothing cannot be
 * reviewed, so the axes that need no index (state, severity, cluster, namespace,
 * snoozed and free text) are honoured locally. Matchers, `since` and sorting are
 * not: approximating those is how a preview starts lying.
 */
function matchesPreviewFilters(alert: Alert, f: AlertFilters): boolean {
  if (f.state.length > 0 && !f.state.includes(alert.state)) return false;
  if (f.severity.length > 0 && !f.severity.includes(alert.severity ?? "")) return false;
  if (f.cluster.length > 0 && !f.cluster.includes(alert.cluster_key)) return false;
  if (f.namespace.length > 0 && !f.namespace.includes(alert.namespace ?? "")) return false;
  if (f.alertname.length > 0 && !f.alertname.includes(alert.alertname)) return false;
  // The tab, applied client-side the way the server applies `?snoozed=`: the
  // main tab is the alerts with no snooze in force, Quiet is the rest.
  const quiet = alert.snooze !== null && alert.snooze !== undefined;
  if (quiet !== (f.tab === "quiet")) return false;
  const q = f.q.trim().toLowerCase();
  if (q !== "") {
    const haystack = `${alert.alertname} ${alert.namespace ?? ""} ${alert.service ?? ""}`;
    if (!haystack.toLowerCase().includes(q)) return false;
  }
  return true;
}

/* -------------------------------------------------------------------------- */
/* Chrome                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * The label that stops a screenshot of this page being mistaken for production.
 * Sticky rather than absolutely positioned so it never sits on top of the first
 * band's own header.
 */
const PreviewHeader: Component = () => (
  <header class="sticky top-0 z-30 flex h-12 shrink-0 items-center gap-md border-b border-line-strong bg-raised px-lg">
    <span class="rounded-chip border border-accent-border bg-accent-fill px-2xs py-0.5 text-micro font-semibold uppercase tracking-widest text-accent">
      Design preview
    </span>
    <span class="text-meta text-ink-muted">
      Fixture data — no session, no backend, nothing here is real.
    </span>
    <div class="flex-1" />
    <span class="text-micro uppercase tracking-widest text-ink-subtle">{theme()}</span>
    <Button
      variant="secondary"
      size="sm"
      onClick={() => setTheme(theme() === "dark" ? "light" : "dark")}
    >
      Switch to {theme() === "dark" ? "light" : "dark"}
    </Button>
  </header>
);

/** One reviewable band: a title, a sentence about what it is, and the thing itself. */
const Band: ParentComponent<{ readonly title: string; readonly note: string }> = (props) => (
  <section class="flex h-screen min-h-0 flex-col overflow-hidden border-b border-line-strong">
    <div class="shrink-0 border-b border-line bg-sunken px-lg py-sm">
      <h2 class="text-title font-semibold tracking-tight text-ink">{props.title}</h2>
      <p class="text-meta text-ink-muted">{props.note}</p>
    </div>
    <div class="flex min-h-0 flex-1 flex-col overflow-hidden">{props.children}</div>
  </section>
);

/* -------------------------------------------------------------------------- */
/* Band 1 — the two-pane list                                                 */
/* -------------------------------------------------------------------------- */

/**
 * The frame is `routes/alerts.tsx`'s, down to the `min-h-0`/`overflow-hidden`
 * chain: `AlertTable` virtualises against its own scroller and only has a
 * bounded height because every ancestor refuses to grow with its content.
 */
const ListBand: Component = () => {
  const [filters, setFilters] = createSignal<AlertFilters>(DEFAULT_FILTERS);

  const rows = createMemo<readonly Alert[]>(() =>
    PREVIEW_ALERTS.filter((a) => matchesPreviewFilters(a, filters())),
  );

  const onFilterLabel = (name: string, value: string): void => {
    setFilters(withMatcher(filters(), name, value));
  };

  return (
    <Band
      title="Alert list — filter toolbar and table"
      note="The shipping FilterBar and AlertTable, stacked in the layout /alerts uses. The toolbar filters these fixtures for real on state, severity, cluster, namespace, snoozed and free text."
    >
      <div class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        <FilterBar
          filters={filters()}
          onChange={setFilters}
          onReset={() => setFilters(DEFAULT_FILTERS)}
          totalCountLabel={fmtCount(rows().length)}
          partialMatchEnabled={true}
        />

        <div class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
          <header class="flex h-9 shrink-0 items-center gap-md border-b border-line px-md">
            <span class="text-body tabular-nums text-ink-muted" aria-live="polite">
              {fmtCount(rows().length)} alert{rows().length === 1 ? "" : "s"}
            </span>
          </header>

          <AlertTable
            alerts={rows()}
            quiet={filters().tab === "quiet"}
            rules={PREVIEW_RULE_SNAPSHOTS}
            rulesPending={false}
            onFilterLabel={onFilterLabel}
            footer={
              <div class="flex items-center justify-center gap-3 border-t border-line bg-surface px-3 py-2">
                <span class="text-meta text-ink-subtle">
                  That is all {fmtCount(rows().length)} of them.
                </span>
              </div>
            }
          />
        </div>
      </div>
    </Band>
  );
};

/* -------------------------------------------------------------------------- */
/* Band 2 — the server-side roll-up                                           */
/* -------------------------------------------------------------------------- */

const ROLLUP_STATES: readonly State[] = ["firing", "suppressed", "expired", "resolved"];

const RollupBand: Component = () => {
  const [drilled, setDrilled] = createSignal<string | null>(null);

  return (
    <Band
      title="Grouped alerts — roll-up buckets"
      note="GroupedAlerts on the alertname axis. Drilling is wired to a local signal rather than to the URL, so the bucket rows are clickable without a router navigation."
    >
      <div class="flex min-h-0 flex-1 flex-col overflow-hidden pl-lg">
        <header class="flex h-14 shrink-0 items-center gap-md border-b border-line">
          <span class="text-body tabular-nums text-ink-muted">
            {fmtCount(PREVIEW_ROLLUPS.length)} buckets
          </span>
          <Show when={drilled()}>
            {(key) => (
              <span class="text-meta text-ink-subtle">
                drilled into <span class="font-mono text-ink-muted">{key()}</span>
              </span>
            )}
          </Show>
          <span class="text-meta text-ink-subtle">
            counted over {ROLLUP_STATES.length} lifecycle states
          </span>
        </header>

        <GroupedAlerts
          buckets={PREVIEW_ROLLUPS}
          by="alertname"
          hasMore={true}
          loading={false}
          onLoadMore={() => setDrilled(null)}
          onDrillDown={(key) => setDrilled(key)}
        />
      </div>
    </Band>
  );
};

/* -------------------------------------------------------------------------- */
/* Band 3 — one alert, in full                                                */
/* -------------------------------------------------------------------------- */

/**
 * Labels and annotations.
 *
 * `routes/alert-detail.tsx` keeps this panel private to the route, so the
 * arrangement is restated here — but every piece of chrome in it (`Panel`,
 * `PanelHeader`, `PanelTitle`, `DataRow`, and the `PANEL_*` rhythm strings) is
 * the shipping one.
 */
const LabelsPanel: Component<{
  readonly labels: Readonly<Record<string, string>>;
  readonly annotations: Readonly<Record<string, string>>;
}> = (props) => {
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
        <dl class="space-y-2xs">
          <For each={labelEntries()}>
            {(entry) => (
              <DataRow term={entry[0]}>
                <span class="break-all font-mono text-body">{entry[1]}</span>
              </DataRow>
            )}
          </For>
        </dl>

        <Show when={annotationEntries().length > 0}>
          <div class="mt-md border-t border-line pt-md">
            <p class="mb-sm text-meta font-semibold uppercase tracking-widest text-ink-muted">
              Annotations
            </p>
            <dl class="space-y-sm">
              <For each={annotationEntries()}>
                {(entry) => (
                  <div>
                    <dt class="text-meta text-ink-subtle">{entry[0]}</dt>
                    <dd class="whitespace-pre-wrap break-words text-body leading-snug text-ink">
                      {entry[1]}
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
};

/** The header of `/alerts/:id`, restated around the shipping chips and actions. */
const DetailHeader: Component = () => (
  <header class="shrink-0 border-b border-line bg-surface">
    <div class="flex items-stretch gap-md px-lg pb-md pt-lg">
      <div
        class={cn("w-2xs shrink-0 rounded-full", STATE_BAR[PREVIEW_DETAIL.state])}
        aria-hidden="true"
      />

      <div class="min-w-0 flex-1">
        <h1 class="min-w-0 truncate text-page font-semibold tracking-tight text-ink">
          {PREVIEW_DETAIL.alertname}
        </h1>

        <div class="mt-sm flex flex-wrap items-center gap-sm">
          <SeverityMark severity={PREVIEW_DETAIL.severity} withLabel />
          <StateChip
            state={PREVIEW_DETAIL.state}
            urgent={
              PREVIEW_DETAIL.state === "firing" &&
              (PREVIEW_DETAIL.current_case?.ack_state ?? "unacked") === "unacked" &&
              normaliseSeverity(PREVIEW_DETAIL.severity) === "critical"
            }
          />
          {/* No `AckChip`: `acked` is not a state an ALERT can be in — a receipt
              belongs to one firing, and this preview mirrors the real header.
              No `FlappingChip` either: the flap detector went blind under the
              case retention window W (ADR 0041 Amendment 1), so nothing presents
              flapping as a live signal any more. */}
          <SnoozeChip snooze={PREVIEW_DETAIL.snooze ?? null} />
        </div>

        <p class="mt-sm text-body text-ink-muted">{STATE_MEANING[PREVIEW_DETAIL.state]}</p>

        <div class="mt-md flex flex-wrap items-center gap-x-lg gap-y-2xs text-meta text-ink-subtle">
          <span>
            cluster <span class="font-mono text-ink-muted">{PREVIEW_DETAIL.cluster_key}</span>
          </span>
          <Show when={PREVIEW_DETAIL.namespace}>
            <span>
              namespace <span class="font-mono text-ink-muted">{PREVIEW_DETAIL.namespace}</span>
            </span>
          </Show>
          <Show when={PREVIEW_DETAIL.service}>
            <span>
              service <span class="font-mono text-ink-muted">{PREVIEW_DETAIL.service}</span>
            </span>
          </Show>
          <span title={absoluteTime(PREVIEW_DETAIL.first_seen_at)}>
            first seen{" "}
            <span class="text-ink-muted">
              <RelativeTime value={PREVIEW_DETAIL.first_seen_at} label="First seen" /> ago
            </span>
          </span>
          <span title={absoluteTime(PREVIEW_DETAIL.last_seen_at)}>
            last seen{" "}
            <span class="text-ink-muted">
              <RelativeTime value={PREVIEW_DETAIL.last_seen_at} label="Last seen" /> ago
            </span>
          </span>
          <Show when={PREVIEW_DETAIL.current_case}>
            {(ac) => (
              <span>
                firing duration{" "}
                <span class="text-ink-muted">
                  <Elapsed from={ac().started_at} to={ac().ended_at ?? null} />
                </span>
              </span>
            )}
          </Show>
          <span title="Firing episodes since oto first saw this identity">
            episodes{" "}
            <span class="text-ink-muted">{fmtCount(PREVIEW_DETAIL.total_cases)}</span>
          </span>
        </div>
      </div>

      <div class="shrink-0">
        <AlertActions alert={PREVIEW_DETAIL} />
      </div>
    </div>

    <div class="flex flex-wrap items-center gap-sm px-lg pb-lg">
      <Show when={PREVIEW_DETAIL.generator_url}>
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
      <Show when={PREVIEW_DETAIL.source}>
        {(source) => <Chip title="The upstream that reported this">{source().name}</Chip>}
      </Show>
      <Chip mono title="oto's durable identity for this alert. Survives Alertmanager reloads.">
        {PREVIEW_DETAIL.alert_key.slice(0, 16)}…
      </Chip>
    </div>
  </header>
);

const DetailBand: Component = () => {
  const [categories, setCategories] = createSignal<readonly EventCategory[]>([]);
  const [order, setOrder] = createSignal<"asc" | "desc">("desc");

  return (
    <Band
      title="Alert detail — header and panels"
      note="The header of /alerts/:id restated around the shipping chips and AlertActions, then the six panels exactly as that route mounts them. Every panel is fed a resolved fixture, so none of them are in a loading or error state."
    >
      <div class="flex min-h-0 flex-1 flex-col overflow-hidden">
        <DetailHeader />

        <div class="grid min-h-0 flex-1 grid-cols-1 gap-xl overflow-auto p-lg xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)] xl:overflow-hidden">
          <Panel class="flex min-h-0 flex-col xl:overflow-hidden">
            <PanelHeader class={PANEL_HEADER}>
              <PanelTitle>Timeline</PanelTitle>
              <span class="text-meta text-ink-subtle">
                displayed in the upstream's time · ordered by oto's
              </span>
            </PanelHeader>
            <Timeline
              events={PREVIEW_EVENTS}
              categories={categories()}
              onCategoriesChange={setCategories}
              order={order()}
              onOrderChange={setOrder}
              hasMore={false}
              loading={false}
              onLoadMore={() => setOrder(order())}
            />
          </Panel>

          <div class="flex min-h-0 flex-col gap-lg xl:overflow-auto">
            <LabelsPanel
              labels={PREVIEW_DETAIL.labels}
              annotations={PREVIEW_DETAIL.annotations}
            />

            <RulePanel history={PREVIEW_RULE_HISTORY} />

            <CasePanel
              cases={PREVIEW_CASES}
              loading={false}
              error={null}
              currentId={PREVIEW_DETAIL.current_case?.id ?? null}
            />

            <EnrichmentPanel enrichments={PREVIEW_ENRICHMENTS} loading={false} error={null} />

            <SnoozePanel history={PREVIEW_SNOOZE_HISTORY} loading={false} error={null} />

            <DeliveryPanel
              notifications={PREVIEW_NOTIFICATIONS}
              summary={PREVIEW_DETAIL.delivery_summary}
              loading={false}
              error={null}
            />
          </div>
        </div>
      </div>
    </Band>
  );
};

/* -------------------------------------------------------------------------- */

const ProtoAlertsPreviewRoute: Component = (): JSX.Element => (
  <QueryClientProvider client={previewClient}>
    <div class="flex min-h-screen flex-col bg-bg text-ink">
      <PreviewHeader />
      <ListBand />
      <RollupBand />
      <DetailBand />
    </div>
  </QueryClientProvider>
);

export default ProtoAlertsPreviewRoute;
