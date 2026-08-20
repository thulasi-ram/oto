/**
 * `/proto/settings-preview` — the four settings sections, drawn against
 * fixtures, with no session and no backend.
 *
 * ⛔ IT IS OUTSIDE THE AUTHENTICATED LAYOUT ROUTE, exactly as
 * `/proto/alerts-preview` is, and for the same reason: it must open on a laptop
 * with nothing running on :8080. `RequireSession` would hold the whole tree
 * behind a `/me` probe that can only 401, and `LiveProvider` would open an SSE
 * connection nobody can serve.
 *
 * ⭐ IT RENDERS THE SHIPPING SECTIONS, NEVER COPIES OF THEM. `SourcesSection`,
 * `ChannelsSection`, `PoliciesSection` and `TuningSection` are imported and
 * mounted as-is, inside the *same* content column `routes/settings.tsx` draws
 * (`max-w-3xl`, `p-lg`, `gap-xl`) — so a label that does not share its left edge
 * with the one above it here does not share it in production either. The four
 * are stacked in one scroll column rather than switched between, because the
 * thing under review is exactly whether they read as one form or as four.
 *
 * The only stub is a second `QueryClient`. Every section reads its data through
 * TanStack, so this file mounts a nested provider whose cache is pre-seeded and
 * whose `staleTime` is `Infinity` — the queries resolve from memory and no
 * request is ever made. Nothing in the sections was changed to accommodate it.
 *
 * Nothing below writes a query key as a literal. The shared resources are seeded
 * through the factories in `api/queries.ts` that already own their keys, and the
 * one key with no factory (`qk.settings.org()`, read inline by `TuningSection`)
 * is asked of `api/keys.ts` rather than restated here.
 */
import { For, Show, createSignal, type Component, type ParentComponent } from "solid-js";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";

import { rangeOf } from "~/api/bounds";
import { UpdateOrgSettingsRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import {
  channelTypesQuery,
  channelsQuery,
  clustersQuery,
  policiesQuery,
  sourcesQuery,
} from "~/api/queries";
import type {
  Channel,
  ChannelTypeDescriptor,
  Cluster,
  ListEnvelope,
  Policy,
  Source,
} from "~/api/types";
import { Button } from "~/components/ui/Button";
import { setTheme, theme } from "~/design/theme";
import { ChannelsSection } from "~/features/settings/ChannelsSection";
import { PoliciesSection } from "~/features/notifications/PoliciesSection";
import { SourcesSection } from "~/features/settings/SourcesSection";
import { TuningSection } from "~/features/settings/TuningSection";
import { channel, cluster, orgSettings, policy, source } from "~/test/fixtures";
import type { Bound } from "~/test/contract";

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * The org-settings bounds, read out of the generated request schema rather than
 * typed here.
 *
 * `TuningSection` renders `min`/`max` on every number input from this table, so
 * a hand-written copy would review a form against a second invention of the
 * server's numbers. `api/bounds#rangeOf` is the same walker the screens read
 * their bounds through, and it is browser-safe — unlike `test/contract`, which
 * reads the YAML off disk and is imported here for its `Bound` type only.
 */
const NUMERIC_KEYS = [
  "refire_grace_s",
  "resolve_grace_s",
  "group_close_delay_s",
  "flap_threshold",
  "flap_window_s",
  "flap_digest_interval_s",
  "raw_retention_days",
  "event_retention_months",
  // ⛔ `unacked_reminder_after_s` WAS LISTED HERE AND THE CONTRACT HAS NO BOUND FOR
  // IT (git-bug bd0fb1d). ⚠️ THAT WAS A LIVE BREAK, NOT A TIDY-UP: `rangeOf` THROWS
  // for a key with no declared `minimum` — by design, so a screen can never invent
  // the server's numbers — so `previewBounds()` raised on every render of this route
  // and the whole preview was blank. `tsc` cannot see it: these are strings in an
  // `as const` array and `rangeOf` takes a `string`.
  //
  // ⚠️ IF A NUMERIC KEY IS ADDED OR REMOVED FROM THE TUNING SURFACE, THIS LIST HAS
  // TO MOVE WITH IT. Nothing derives it from `KNOBS`, which is the reason it drifted.
] as const;

function previewBounds(): ReadonlyMap<string, Bound> {
  const out = new Map<string, Bound>();
  for (const key of NUMERIC_KEYS) out.set(key, rangeOf(UpdateOrgSettingsRequestSchema, key));
  return out;
}

const CLUSTERS: readonly Cluster[] = [
  cluster(),
  cluster({
    id: "3e9f5b6c-4d7a-4e9f-0b1c-2d3e4f506173",
    cluster_key: "prod-us",
    display_name: "Production (US)",
    source_count: 2,
  }),
];

const SOURCES: readonly Source[] = [
  source(),
  source(
    {
      id: "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f61",
      cluster_id: "3e9f5b6c-4d7a-4e9f-0b1c-2d3e4f506173",
      name: "prod-us alertmanager",
      base_url: "http://alertmanager.us.prod:9093",
      ingest_path: "/api/v1/ingest/alertmanager/2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f61",
    },
    { groupWaitS: 45, groupIntervalS: 600, repeatIntervalS: 28_800 },
  ),
];

const CHANNELS: readonly Channel[] = [
  channel(),
  channel({
    id: "4f0a6c7d-5e8b-4f0a-1c2d-3e4f50617284",
    type: "webhook",
    name: "pagerduty bridge",
    config: { url: "https://events.pagerduty.com/oto" },
    renderer: "webhook.json",
    verbosity: "firing_only",
    thread_updates: false,
    health_status: "degraded",
  }),
];

const CHANNEL_TYPES: readonly ChannelTypeDescriptor[] = [
  {
    type: "slack",
    display_name: "Slack",
    credential_kinds: ["slack_bot_token"],
    capabilities: ["threading", "amend", "rich_layout"],
    renderers: ["slack.default"],
    config_schema: {
      type: "object",
      required: ["conversation_id"],
      properties: {
        conversation_id: {
          type: "string",
          title: "Conversation",
          description: "The channel id oto posts into. Not the name — ids survive renames.",
          maxLength: 32,
        },
        team_id: {
          type: "string",
          title: "Workspace",
          description: "The Slack team id the bot token belongs to.",
          maxLength: 32,
        },
        max_instances: {
          type: "integer",
          title: "Concurrent cards",
          description: "How many cards oto keeps open in this conversation at once.",
          minimum: 1,
          maximum: 50,
        },
        mention_here: {
          type: "boolean",
          title: "Use @here",
          description: "Adds an @here to the first card of an incident.",
        },
      },
    },
  },
  {
    type: "webhook",
    display_name: "Webhook",
    credential_kinds: ["bearer", "basic", "none"],
    capabilities: ["dedupe_key"],
    renderers: ["webhook.json"],
    config_schema: {
      type: "object",
      required: ["url"],
      properties: {
        url: {
          type: "string",
          title: "Endpoint",
          description: "Where the JSON body is POSTed.",
          maxLength: 2048,
        },
        method: {
          type: "string",
          title: "Method",
          enum: ["POST", "PUT"],
          description: "The verb oto sends with.",
        },
      },
    },
  },
];

const POLICIES: readonly Policy[] = [
  policy(),
  policy({
    id: "5a1b7d8e-6f9c-4a1b-2d3e-4f5061728395",
    name: "everything else → webhook",
    priority: 900,
    enabled: false,
    matchers: [{ name: "namespace", op: "=~", value: "prod-.*" }],
    reasons: ["fired"],
    channel_ids: ["4f0a6c7d-5e8b-4f0a-1c2d-3e4f50617284"],
  }),
];

function envelope<T>(rows: readonly T[]): ListEnvelope<T> {
  return {
    data: [...rows],
    page: { has_more: false, limit: 100, next_cursor: null },
    meta: { request_id: "preview", elapsed_ms: 0 },
  };
}

/* -------------------------------------------------------------------------- */
/* The stub: a cache that already has every answer                            */
/* -------------------------------------------------------------------------- */

type PreviewFixture = readonly [key: readonly unknown[], data: unknown];

const PREVIEW_FIXTURES: readonly PreviewFixture[] = [
  [clustersQuery().queryKey, envelope(CLUSTERS)],
  [sourcesQuery().queryKey, envelope(SOURCES)],
  [channelsQuery().queryKey, envelope(CHANNELS)],
  [channelTypesQuery().queryKey, [...CHANNEL_TYPES]],
  [policiesQuery().queryKey, envelope(POLICIES)],
  [qk.settings.org(), orgSettings(previewBounds())],
];

function fixtureFor(key: readonly unknown[]): unknown {
  const hit = PREVIEW_FIXTURES.find((fixture) => JSON.stringify(fixture[0]) === JSON.stringify(key));
  return hit?.[1];
}

/**
 * `staleTime: Infinity` is the whole mechanism. A seeded entry that can go stale
 * would be refetched on mount, the refetch would fail against nothing, and the
 * section would lose its list the moment it appeared — the one thing a
 * credential-free preview must not do.
 */
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
/* Chrome                                                                     */
/* -------------------------------------------------------------------------- */

/** The label that stops a screenshot of this page being mistaken for production. */
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

/**
 * One section, in the column `routes/settings.tsx` gives it.
 *
 * The wrapper is that route's content column restated — `max-w-3xl` and the
 * `gap-xl p-lg` inside it — because that markup belongs to a route and there is
 * no component to import. Everything inside it is the shipping section.
 */
const Band: ParentComponent<{ readonly title: string; readonly id: string }> = (props) => (
  <section id={props.id} class="border-b border-line-strong">
    <div class="sticky top-12 z-20 border-b border-line bg-sunken px-lg py-sm">
      <h2 class="text-title font-semibold tracking-tight text-ink">{props.title}</h2>
    </div>
    <div class="flex w-full max-w-3xl flex-col gap-xl p-lg">{props.children}</div>
  </section>
);

/* -------------------------------------------------------------------------- */

const BANDS = ["all", "sources", "channels", "policies", "tuning"] as const;

type BandId = (typeof BANDS)[number];

const ProtoSettingsPreview: Component = () => {
  const [only, setOnly] = createSignal<BandId>("all");
  const shows = (id: BandId): boolean => only() === "all" || only() === id;

  return (
    <QueryClientProvider client={previewClient}>
      <div class="min-h-screen bg-surface text-ink">
        <PreviewHeader />
        <nav class="sticky top-12 z-30 flex flex-wrap items-center gap-sm border-b border-line bg-surface px-lg py-sm">
          <For each={BANDS}>
            {(id) => (
              <Button
                size="sm"
                variant={only() === id ? "default" : "secondary"}
                onClick={() => setOnly(id)}
              >
                {id}
              </Button>
            )}
          </For>
        </nav>

        <Show when={shows("sources")}>
          <Band id="sources" title="Sources and clusters">
            <SourcesSection />
          </Band>
        </Show>
        <Show when={shows("channels")}>
          <Band id="channels" title="Channels">
            <ChannelsSection />
          </Band>
        </Show>
        <Show when={shows("policies")}>
          <Band id="policies" title="Notification policies">
            <PoliciesSection />
          </Band>
        </Show>
        <Show when={shows("tuning")}>
          <Band id="tuning" title="Tuning">
            <TuningSection />
          </Band>
        </Show>
      </div>
    </QueryClientProvider>
  );
};

export default ProtoSettingsPreview;
