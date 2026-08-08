/**
 * Query keys, in one place.
 *
 * They are a factory rather than string literals because the live stream
 * invalidates by prefix: an `alert.upserted` frame invalidates `["alerts"]`,
 * which covers every filtered list without the stream needing to know what
 * filters are on screen.
 */
import type { AlertListQuery, GroupListQuery, TimelineQuery } from "./types";
import type { LabelSelector } from "./client";

export const qk = {
  alerts: {
    all: () => ["alerts"] as const,
    list: (query: AlertListQuery, label: LabelSelector) =>
      ["alerts", "list", query, label] as const,
    detail: (id: string) => ["alerts", "detail", id] as const,
    events: (id: string, query: TimelineQuery) => ["alerts", "events", id, query] as const,
    occurrences: (id: string) => ["alerts", "occurrences", id] as const,
    enrichments: (id: string) => ["alerts", "enrichments", id] as const,
    rule: (id: string) => ["alerts", "rule", id] as const,
    notifications: (id: string) => ["alerts", "notifications", id] as const,
  },
  groups: {
    all: () => ["groups"] as const,
    list: (query: GroupListQuery) => ["groups", "list", query] as const,
    detail: (id: string) => ["groups", "detail", id] as const,
    alerts: (id: string) => ["groups", "alerts", id] as const,
    timeline: (id: string, query: TimelineQuery) => ["groups", "timeline", id, query] as const,
  },
  labels: {
    names: () => ["labels", "names"] as const,
    values: (name: string, q: string) => ["labels", "values", name, q] as const,
  },
  settings: {
    clusters: () => ["settings", "clusters"] as const,
    sources: () => ["settings", "sources"] as const,
    sourceHealth: (id: string) => ["settings", "sources", id, "health"] as const,
    channelTypes: () => ["settings", "channel-types"] as const,
    channels: () => ["settings", "channels"] as const,
    policies: () => ["settings", "policies"] as const,
  },
  deliveries: {
    all: () => ["deliveries"] as const,
  },
  meta: {
    me: () => ["meta", "me"] as const,
    version: () => ["meta", "version"] as const,
  },
} as const;
