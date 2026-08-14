/**
 * Every filter the contract serves, and not one it does not.
 *
 * The bar is wide and dense rather than hidden behind a "Filters" drawer,
 * because the single most important property of a filtered view is that you can
 * see what is filtering it. A drawer makes "why is this list empty?" a two-click
 * question, and at 3am that is how a typo becomes an outage.
 *
 * Severity is a free vocabulary, not an enum (`AlertDTO.severity` is
 * deliberately open), so the common three are offered as toggles and anything
 * else arriving from a URL is preserved and shown rather than silently dropped.
 */
import { For, Show, createMemo, createSignal, type Component, type JSX } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { clustersQuery } from "~/api/queries";
import { Button, Input, Select, ToggleGroup, cx } from "~/components/ui/primitives";
import { STATE_LABEL } from "~/components/StateChip";
import type { State } from "~/api/types";
import {
  ALL_STATES,
  GROUP_BY_VALUES,
  activeFilterCount,
  type AlertFilters,
  type GroupBy,
  type SortKey,
} from "./filters";
import { MatcherInput } from "./MatcherInput";

/** The severities almost everyone uses. Not a closed set — see the note above. */
const COMMON_SEVERITIES = ["critical", "warning", "info"] as const;

/** Relative windows, resolved to an absolute `since` at click time. */
const SINCE_PRESETS = [
  { label: "Any time", hours: null },
  { label: "Last hour", hours: 1 },
  { label: "Last 24 hours", hours: 24 },
  { label: "Last 7 days", hours: 24 * 7 },
  { label: "Last 30 days", hours: 24 * 30 },
] as const;

const GROUP_LABEL: Record<GroupBy, string> = {
  none: "No grouping",
  alertname: "Group by alert name",
  namespace: "Group by namespace",
  fingerprint: "Group by fingerprint",
};

export interface FilterBarProps {
  readonly filters: AlertFilters;
  readonly onChange: (next: AlertFilters) => void;
  readonly onReset: () => void;
  /** Rendered on the right of the top row — the result count and live pill. */
  readonly status?: JSX.Element;
}

export const FilterBar: Component<FilterBarProps> = (props) => {
  const [qDraft, setQDraft] = createSignal(props.filters.q);

  const clusters = useQuery(() => clustersQuery());

  const patch = (part: Partial<AlertFilters>): void => {
    props.onChange({ ...props.filters, ...part });
  };

  /** Severities present in the URL that are not one of the common three. */
  const customSeverities = createMemo(() =>
    props.filters.severity.filter((s) => !(COMMON_SEVERITIES as readonly string[]).includes(s)),
  );

  const sincePreset = (): string => {
    if (props.filters.since === null) return "Any time";
    const target = new Date(props.filters.since).getTime();
    const hours = (Date.now() - target) / 3_600_000;
    const match = SINCE_PRESETS.find((p) => p.hours !== null && Math.abs(p.hours - hours) < 0.2);
    return match?.label ?? "Custom";
  };

  return (
    <div class="shrink-0 border-b border-line bg-surface">
      {/* ---- row 1: search, matchers, ordering ------------------------------ */}
      <div class="flex flex-wrap items-start gap-2 px-3 pb-2 pt-2">
        <div class="min-w-[13rem] flex-[1_1_13rem]">
          <label for="alert-q" class="sr-only-focusable">
            Search alert names and summaries
          </label>
          <Input
            id="alert-q"
            type="search"
            value={qDraft()}
            placeholder="Search name, summary, description…"
            onInput={(e) => setQDraft(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") patch({ q: qDraft() });
              if (e.key === "Escape") {
                setQDraft("");
                patch({ q: "" });
              }
            }}
            onBlur={() => {
              if (qDraft() !== props.filters.q) patch({ q: qDraft() });
            }}
          />
        </div>

        <div class="min-w-[16rem] flex-[2_1_20rem]">
          <label for="alert-matchers" class="sr-only-focusable">
            Label matchers, Alertmanager syntax
          </label>
          <MatcherInput
            id="alert-matchers"
            value={props.filters.matcherText}
            onChange={(next) => patch({ matcherText: next })}
            onCommit={() => undefined}
          />
        </div>

        <div class="flex shrink-0 items-center gap-2">
          <label for="alert-sort" class="sr-only-focusable">
            Sort order
          </label>
          <Select
            id="alert-sort"
            value={props.filters.sort}
            onChange={(e) => patch({ sort: e.currentTarget.value as SortKey })}
            title="Only two orderings exist, because a keyset cursor needs a total order backed by an index."
          >
            <option value="-last_seen_at">Newest activity</option>
            <option value="-first_seen_at">Newest first seen</option>
          </Select>

          <label for="alert-group" class="sr-only-focusable">
            Grouping
          </label>
          <Select
            id="alert-group"
            value={props.filters.groupBy}
            onChange={(e) => patch({ groupBy: e.currentTarget.value as GroupBy })}
          >
            <For each={GROUP_BY_VALUES}>
              {(g) => <option value={g}>{GROUP_LABEL[g]}</option>}
            </For>
          </Select>
        </div>

        <div class="ml-auto flex shrink-0 items-center gap-2 self-center">{props.status}</div>
      </div>

      {/* ---- row 2: the enumerable filters ---------------------------------- */}
      <div class="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 pb-2">
        <ToggleGroup<State>
          legend="Lifecycle state"
          options={ALL_STATES.map((s) => ({ value: s, label: STATE_LABEL[s] }))}
          selected={props.filters.state}
          onChange={(next) => patch({ state: next })}
        />

        <Divider />

        <ToggleGroup<string>
          legend="Severity"
          options={[
            ...COMMON_SEVERITIES.map((s) => ({ value: s as string, label: s })),
            ...customSeverities().map((s) => ({ value: s, label: s })),
          ]}
          selected={props.filters.severity}
          onChange={(next) => patch({ severity: next })}
        />

        <Divider />

        {/* Acknowledgement is orthogonal to state (§B): `acked` still returns
            firing alerts, because acknowledging one does not end it. */}
        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Ack</span>
          <Select
            value={props.filters.ack ?? ""}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ ack: v === "acked" || v === "unacked" ? v : null });
            }}
            title="A receipt on a signal. An acknowledged alert is still firing."
          >
            <option value="">Any</option>
            <option value="unacked">Not yet seen</option>
            <option value="acked">Seen by someone</option>
          </Select>
        </label>

        {/* Snooze is a third orthogonal axis, never a state (§B.8): the default
            includes both, because hiding snoozed alerts is how an incident is
            lost. A snoozed alert still reads at its true severity. */}
        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Snoozed</span>
          <Select
            value={props.filters.snoozed === null ? "" : String(props.filters.snoozed)}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ snoozed: v === "" ? null : v === "true" });
            }}
            title="Whether oto is currently holding its notifications for the alert. It says nothing about the signal — a snoozed alert is still firing and still whatever severity it was."
          >
            <option value="">Any (default — includes both)</option>
            <option value="true">Notifications held</option>
            <option value="false">Notifications flowing</option>
          </Select>
        </label>

        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Flapping</span>
          <Select
            value={props.filters.flapping === null ? "" : String(props.filters.flapping)}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ flapping: v === "" ? null : v === "true" });
            }}
          >
            <option value="">Any</option>
            <option value="true">Damped as flapping</option>
            <option value="false">Not flapping</option>
          </Select>
        </label>

        <Show when={(clusters.data?.data.length ?? 0) > 0}>
          <label class="flex items-center gap-1.5 text-body text-ink-muted">
            <span>Cluster</span>
            <Select
              value={props.filters.cluster[0] ?? ""}
              onChange={(e) => {
                const v = e.currentTarget.value;
                patch({ cluster: v === "" ? [] : [v] });
              }}
            >
              <option value="">All clusters</option>
              <For each={clusters.data?.data ?? []}>
                {(c) => <option value={c.cluster_key}>{c.display_name}</option>}
              </For>
            </Select>
          </label>
        </Show>

        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Since</span>
          <Select
            value={sincePreset()}
            onChange={(e) => {
              const preset = SINCE_PRESETS.find((p) => p.label === e.currentTarget.value);
              if (!preset) return;
              patch({
                since:
                  preset.hours === null
                    ? null
                    : new Date(Date.now() - preset.hours * 3_600_000).toISOString(),
              });
            }}
            title="Lower bound on last activity."
          >
            <For each={SINCE_PRESETS}>{(p) => <option value={p.label}>{p.label}</option>}</For>
            <Show when={sincePreset() === "Custom"}>
              <option value="Custom">Custom (from link)</option>
            </Show>
          </Select>
        </label>

        <Show when={activeFilterCount(props.filters) > 0}>
          <Button size="sm" variant="ghost" onClick={props.onReset} class="ml-auto">
            Clear {activeFilterCount(props.filters)} filter
            {activeFilterCount(props.filters) === 1 ? "" : "s"}
          </Button>
        </Show>
      </div>
    </div>
  );
};

const Divider: Component = () => (
  <span class={cx("h-4 w-px shrink-0 bg-line")} aria-hidden="true" />
);
