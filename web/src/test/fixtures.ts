/**
 * Payloads shaped like the ones the server actually sends.
 *
 * Every builder is typed against `~/api/types`, which is typed against the
 * generated contract — so a field the contract adds as required breaks this file
 * at `tsc --noEmit` rather than producing a fixture the screens would never see
 * in production. That is the point of building them here rather than casting a
 * literal at each call site.
 */
import type {
  Alert,
  AlertDetail,
  Channel,
  Cluster,
  DeliveryDrill,
  Occurrence,
  OrgSettingsView,
  Policy,
  RuleSnapshot,
  SettingBound,
  Snooze,
  Source,
  StreamFrame,
} from "~/api/types";
import type { Bound } from "~/test/contract";

const T0 = "2026-08-09T09:00:00.000Z";

export function occurrence(patch: Partial<Occurrence> = {}): Occurrence {
  return {
    id: "0f8fad5b-d9cb-469f-a165-70867728950e",
    alert_id: "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae",
    seq: 3,
    state: "firing",
    ack_state: "unacked",
    started_at: T0,
    last_observed_at: T0,
    source_starts_at: T0,
    reopen_count: 0,
    observed_skew_ms: 0,
    ...patch,
  };
}

export function ruleSnapshot(patch: Partial<RuleSnapshot> = {}): RuleSnapshot {
  return {
    id: "1c9d3f4a-2b5e-4c7d-8e9f-0a1b2c3d4e5f",
    source_id: "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60",
    rule_fingerprint: "sha256:abc",
    rule_file: "/etc/prometheus/rules/oto.yaml",
    rule_group: "oto",
    rule_name: "HighErrorRate",
    expr: "sum(rate(http_errors[5m])) > 0.05",
    for_seconds: 900,
    keep_firing_for_seconds: 0,
    rule_labels: {},
    rule_annotations: {},
    origin: "prometheus_api",
    match_confidence: "exact",
    candidate_count: 1,
    captured_at: T0,
    ...patch,
  };
}

export function alert(patch: Partial<Alert> = {}): Alert {
  return {
    id: "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae",
    alert_key: "b3f1c2d4e5a60718",
    source_fingerprint: "9a8b7c6d5e4f3021",
    alertname: "HighErrorRate",
    severity: "critical",
    namespace: "payments",
    cluster_key: "prod-eu",
    labels: { alertname: "HighErrorRate", severity: "critical", namespace: "payments", pod: "api-7" },
    annotations: { summary: "Error rate above 5% for 15 minutes" },
    state: "firing",
    ack_state: "unacked",
    first_seen_at: T0,
    last_seen_at: T0,
    last_state_change_at: T0,
    total_occurrences: 3,
    flap_score: 0,
    is_flapping: false,
    synthetic: false,
    ...patch,
  };
}

/**
 * An active snooze.
 *
 * It carries no `ended_at`, because a snooze that has ended is history rather
 * than a hold — and the screens read exactly that difference to decide whether
 * the button offers to start one or to end one.
 */
export function snooze(patch: Partial<Snooze> = {}): Snooze {
  return {
    id: "9d4e0a1b-8c2f-4d5e-6a7b-8c9d0e1f2a3b",
    snoozed_at: T0,
    snoozed_until: "2026-08-09T10:00:00.000Z",
    snoozed_by_label: "Ada Lovelace",
    ...patch,
  };
}

export function alertDetail(patch: Partial<AlertDetail> = {}): AlertDetail {
  return {
    ...alert(),
    current_occurrence: occurrence(),
    delivery_summary: { total: 0, sent: 0, failed: 0, pending: 0, suppressed: 0 },
    ...patch,
  } as AlertDetail;
}

export function cluster(patch: Partial<Cluster> = {}): Cluster {
  return {
    id: "3e9f5b6c-4d7a-4e9f-0b1c-2d3e4f506172",
    cluster_key: "prod-eu",
    display_name: "Production (EU)",
    source_count: 1,
    created_at: T0,
    updated_at: T0,
    ...patch,
  };
}

export function channel(patch: Partial<Channel> = {}): Channel {
  return {
    id: "4f0a6c7d-5e8b-4f0a-1c2d-3e4f50617283",
    type: "slack",
    name: "#sre-alerts",
    config: { channel_id: "C0123456789" },
    renderer: "slack.default",
    verbosity: "status_changes",
    thread_updates: true,
    show_field_emoji: true,
    enabled: true,
    health_status: "healthy",
    created_at: T0,
    updated_at: T0,
    ...patch,
  };
}

export function policy(patch: Partial<Policy> = {}): Policy {
  return {
    id: "5a1b7d8e-6f9c-4a1b-2d3e-4f5061728394",
    name: "critical → #sre-alerts",
    priority: 100,
    enabled: true,
    matchers: [{ name: "severity", op: "=", value: "critical" }],
    reasons: ["fired", "all_resolved"],
    channel_ids: ["4f0a6c7d-5e8b-4f0a-1c2d-3e4f50617283"],
    created_at: T0,
    updated_at: T0,
    ...patch,
  };
}

/* -------------------------------------------------------------------------- */
/* Sources and their route timings                                            */
/* -------------------------------------------------------------------------- */

export interface TimingSpec {
  readonly groupWaitS?: number | null;
  readonly groupIntervalS?: number | null;
  readonly repeatIntervalS?: number | null;
  readonly route?: "top_level" | "oto_receiver";
  readonly routesAgree?: boolean;
  readonly receiverBasis?: Source extends never ? never : "sole_webhook" | "ambiguous" | "no_webhook" | "unknown";
}

/** A source whose configuration oto has read, with the provenance of each timing. */
export function source(patch: Partial<Source> = {}, timings: TimingSpec = {}): Source {
  const timing = (
    seconds: number | null | undefined,
  ): { provenance: "observed" | "default_applies" | "unknown"; value_ms: number | null } =>
    seconds === null || seconds === undefined
      ? { provenance: "unknown", value_ms: null }
      : { provenance: "observed", value_ms: seconds * 1000 };

  const inherited = (
    seconds: number | null | undefined,
  ): {
    provenance: "observed" | "default_applies" | "unknown";
    value_ms: number | null;
    from_depth: number | null;
  } => ({ ...timing(seconds), from_depth: 0 });

  return {
    id: "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60",
    cluster_id: "3e9f5b6c-4d7a-4e9f-0b1c-2d3e4f506172",
    name: "prod-eu alertmanager",
    kind: "alertmanager",
    base_url: "http://alertmanager.prod:9093",
    tls_skip_verify: false,
    inject_labels: {},
    ignore_labels: [],
    redact_labels: [],
    redact_annotations: [],
    push_enabled: true,
    reconcile_interval_seconds: 60,
    ingest_path: "/api/v1/ingest/alertmanager/2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60",
    created_at: T0,
    updated_at: T0,
    health: {
      source_id: "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60",
      status: "healthy",
      consecutive_failures: 0,
      clock_skew_ms: 0,
      divergence_count: 0,
      warnings: [],
      updated_at: T0,
      route_timings: {
        group_wait: timing(timings.groupWaitS ?? 30),
        group_interval: timing(timings.groupIntervalS ?? 300),
        repeat_interval: timing(timings.repeatIntervalS ?? 14_400),
        route: timings.route ?? "oto_receiver",
        child_routes: 1,
        child_routes_with_timings: 1,
        receiver: "oto",
        receiver_basis: timings.receiverBasis ?? "sole_webhook",
        webhook_receivers: ["oto"],
        routes: [
          {
            receiver: "oto",
            path: [
              { matchers: [], deprecated: false, continue: false },
              { matchers: ['severity="critical"'], deprecated: false, continue: false },
            ],
            group_wait: inherited(timings.groupWaitS ?? 30),
            group_interval: inherited(timings.groupIntervalS ?? 300),
            repeat_interval: inherited(timings.repeatIntervalS ?? 14_400),
            group_by: ["alertname"],
            group_by_all: false,
            reaches_oto: true,
            unreachable: false,
          },
        ],
        routes_agree: timings.routesAgree ?? true,
        routes_dropped: 0,
        defaults_from_version: "0.28.0",
        defaults_verified: true,
        observed_at: T0,
      },
    },
    ...patch,
  };
}

/* -------------------------------------------------------------------------- */
/* Org settings — built FROM the contract's own bounds                        */
/* -------------------------------------------------------------------------- */

/**
 * The tuning view, with `bounds` taken from the OpenAPI document rather than
 * typed here.
 *
 * ⛔ THAT IS THE WHOLE POINT. The screen validates against `bounds`, so a
 * fixture carrying hand-written ranges would test the screen against a second
 * invention of the server's table. Pass `integerBounds("UpdateOrgSettingsRequest")`
 * and the form is exercised against the numbers the server will actually reject
 * with.
 */
export function orgSettings(
  bounds: ReadonlyMap<string, Bound>,
  patch: {
    readonly settings?: Readonly<Record<string, unknown>>;
    readonly origins?: Readonly<Record<string, "default" | "org" | "config">>;
    readonly configKeys?: Readonly<Record<string, string>>;
    readonly shadowed?: Readonly<Record<string, unknown>>;
    readonly why?: string;
  } = {},
): OrgSettingsView {
  const settings: Record<string, unknown> = {
    refire_grace_s: 900,
    resolve_grace_s: 300,
    group_close_delay_s: 900,
    flap_threshold: 5,
    flap_window_s: 7200,
    flap_digest_interval_s: 900,
    storm_threshold: 10,
    storm_window_s: 60,
    storm_cooldown_s: 600,
    raw_retention_days: 30,
    event_retention_months: 13,
    unacked_reminder_after_s: 0,
    default_verbosity: "status_changes",
    broadcast_on_resolved: false,
    unacked_reminder_mention: "none",
    unacked_reminder_mention_list: [],
    unacked_reminder_mention_min_severity: "critical",
    ...patch.settings,
  };

  const origins: Record<string, "default" | "org" | "config"> = {};
  for (const key of Object.keys(settings)) origins[key] = "default";
  Object.assign(origins, patch.origins ?? {});

  const served: Record<string, SettingBound> = {};
  for (const [key, b] of bounds) {
    served[key] = {
      min: b.min,
      max: b.max,
      why: patch.why ?? "the range oto enforces on this key.",
    };
  }

  return {
    settings,
    origins,
    config_keys: { ...patch.configKeys },
    shadowed: { ...patch.shadowed },
    bounds: served,
  } as unknown as OrgSettingsView;
}

/* -------------------------------------------------------------------------- */
/* Drills                                                                     */
/* -------------------------------------------------------------------------- */

/** A drill whose stages are built from the contract's own stage names. */
export function drill(
  stageNames: readonly string[],
  patch: Partial<DeliveryDrill> = {},
): DeliveryDrill {
  return {
    id: "6b2c8e9f-7a0d-4b2c-3e4f-506172839405",
    source_id: "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60",
    severity: "warning",
    status: "running",
    stages: stageNames.map((name) => ({
      name,
      status: "pending",
      detail: "waiting",
    })) as DeliveryDrill["stages"],
    destinations: [],
    started_by_label: "Ada Lovelace",
    started_at: T0,
    deadline_at: "2026-08-09T09:01:30.000Z",
    ...patch,
  } as DeliveryDrill;
}

/* -------------------------------------------------------------------------- */
/* Stream frames                                                              */
/* -------------------------------------------------------------------------- */

/** One `ui_events` frame, as it arrives on the wire. */
export function frame(seq: number, kind: string, data: Readonly<Record<string, unknown>> = {}): StreamFrame {
  return {
    seq,
    kind,
    org_id: "7c3d9f0a-8b1e-4c3d-4f50-617283940516",
    occurred_at: T0,
    data,
  } as unknown as StreamFrame;
}

/** Serialise frames into `text/event-stream` bytes, id line and all. */
export function sse(...frames: readonly StreamFrame[]): string {
  return frames
    .map((f) => {
      const seq = (f as unknown as { seq: number }).seq;
      const kind = (f as unknown as { kind: string }).kind;
      return `id: ${seq}\nevent: ${kind}\ndata: ${JSON.stringify(f)}\n\n`;
    })
    .join("");
}
