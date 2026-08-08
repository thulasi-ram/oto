/**
 * "The rule as it was when this fired, and how it has changed since."
 *
 * This is the thing Alertmanager cannot tell you. An alert from six weeks ago
 * shows the threshold that was **actually in force** when it fired, because the
 * snapshot was content-addressed at fire time rather than read live — so a rule
 * that has since been edited, or deleted entirely, does not rewrite history.
 *
 * The diff is deliberately the loudest element on this panel. A threshold that
 * moved under you is the single most valuable fact oto holds, and §B is explicit
 * that a `rule.definition_changed` is delivered regardless of channel verbosity
 * because it is never noise.
 *
 * Colour discipline (§M.2): a diff is not an alert state, so it gets **no Tier B
 * hue**. It signals with a strong left rule, weight, monospace and explicit
 * `was` / `now` words. Spending red here would devalue the red that means
 * "firing", and would also mislead — a rule change is not a failure.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { listRuleSnapshots } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { RuleChange, RuleHistory, RuleSnapshot, RuleSnapshotQuery } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button, Chip, DataRow, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { EmptyState } from "~/components/ui/states";
import { absoluteTime, duration } from "~/lib/format";

/**
 * The cap on `RuleHistoryDTO.versions`. Reaching it does not mean the history
 * ends there — `GET /api/v1/rule-snapshots` pages past it with a real keyset
 * cursor, so the panel offers to keep reading rather than stopping silently.
 */
const EMBEDDED_VERSION_CAP = 200;

/* -------------------------------------------------------------------------- */
/* Payload sniffing, for the timeline                                         */
/* -------------------------------------------------------------------------- */

/**
 * `AlertEventDTO.payload` is `{[key: string]: unknown}` by contract — the shape
 * varies by `type` and unknown keys are forward-compatible. So the timeline
 * checks structurally rather than casting, and falls back to the generic
 * payload table if the shape is not what this version understands.
 */
export function isRuleChangePayload(type: string, payload: Record<string, unknown>): boolean {
  if (type !== "rule.definition_changed") return false;
  return typeof payload["expr_changed"] === "boolean" || typeof payload["for_changed"] === "boolean";
}

function asRuleChange(payload: Record<string, unknown>): RuleChange | null {
  if (typeof payload["expr_changed"] !== "boolean" && typeof payload["for_changed"] !== "boolean") {
    return null;
  }
  return payload as unknown as RuleChange;
}

export const RuleChangePayload: Component<{
  readonly payload: Record<string, unknown>;
  readonly class?: string;
}> = (props) => {
  const change = createMemo(() => asRuleChange(props.payload));
  return (
    <Show when={change()}>{(c) => <RuleDiff change={c()} class={props.class} />}</Show>
  );
};

/* -------------------------------------------------------------------------- */
/* The diff                                                                   */
/* -------------------------------------------------------------------------- */

export const RuleDiff: Component<{
  readonly change: RuleChange;
  readonly class?: string | undefined;
}> = (props) => {
  const labelDiff = (): readonly [string, readonly string[]][] =>
    Object.entries(props.change.label_diff ?? {});
  const annotationDiff = (): readonly [string, readonly string[]][] =>
    Object.entries(props.change.annotation_diff ?? {});

  const nothing = (): boolean =>
    !props.change.expr_changed &&
    !props.change.for_changed &&
    labelDiff().length === 0 &&
    annotationDiff().length === 0;

  return (
    <div
      class={cx(
        "rounded-[4px] border border-line-strong border-l-[3px] border-l-ink-muted bg-sunken",
        props.class,
      )}
    >
      <div class="flex flex-wrap items-baseline gap-x-2 border-b border-line px-2.5 py-1.5">
        <span class="text-[12px] font-semibold text-ink">This rule changed</span>
        <span class="text-[11px] text-ink-subtle">
          previous version captured{" "}
          <RelativeTime value={props.change.previous_captured_at} label="Previously captured" /> ago
        </span>
      </div>

      <div class="space-y-2 px-2.5 py-2">
        <Show when={nothing()}>
          <p class="text-[11px] text-ink-subtle">
            The fingerprint changed but no field oto compares differed — usually formatting.
          </p>
        </Show>

        <Show when={props.change.expr_changed}>
          <DiffBlock
            term="Expression"
            was={props.change.previous_expr ?? ""}
            now={props.change.new_expr ?? ""}
            mono
          />
        </Show>

        <Show when={props.change.for_changed}>
          <DiffBlock
            term="for:"
            was={fmtFor(props.change.previous_for_seconds)}
            now={fmtFor(props.change.new_for_seconds)}
            hint="How long the condition must hold before it fires."
          />
        </Show>

        <For each={labelDiff()}>
          {([name, pair]) => (
            <DiffBlock
              term={`label ${name}`}
              was={pair[0] ?? ""}
              now={pair[1] ?? ""}
              mono
              hint={
                name === "severity"
                  ? "A severity change alters how loudly every future firing is presented."
                  : undefined
              }
            />
          )}
        </For>

        <For each={annotationDiff()}>
          {([name, pair]) => (
            <DiffBlock term={`annotation ${name}`} was={pair[0] ?? ""} now={pair[1] ?? ""} />
          )}
        </For>
      </div>
    </div>
  );
};

function fmtFor(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return "";
  return duration(seconds);
}

/**
 * One changed field, as two labelled lines.
 *
 * Not a red/green character diff: PromQL is dense and a token-level diff of it
 * is harder to read than the two expressions side by side, and red/green would
 * both mislead (a change is not an error) and break the Tier-B rule.
 */
const DiffBlock: Component<{
  readonly term: string;
  readonly was: string;
  readonly now: string;
  readonly mono?: boolean;
  readonly hint?: string | undefined;
}> = (props) => (
  <div>
    <div class="mb-0.5 flex items-baseline gap-2">
      <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted">
        {props.term}
      </span>
      <Show when={props.hint}>
        <span class="text-[11px] text-ink-subtle">{props.hint}</span>
      </Show>
    </div>
    <div class="grid grid-cols-[2.6rem_minmax(0,1fr)] gap-x-2 gap-y-1">
      <span class="pt-px text-right text-[11px] text-ink-subtle">was</span>
      <code
        class={cx(
          "min-w-0 break-words rounded-[3px] bg-surface px-1.5 py-0.5 text-[11px] leading-snug text-ink-muted line-through decoration-ink-subtle/60",
          props.mono === true ? "font-mono" : "",
        )}
      >
        {props.was === "" ? "(absent)" : props.was}
      </code>

      <span class="pt-px text-right text-[11px] font-semibold text-ink">now</span>
      <code
        class={cx(
          "min-w-0 break-words rounded-[3px] border border-line-strong bg-surface px-1.5 py-0.5 text-[11px] font-medium leading-snug text-ink",
          props.mono === true ? "font-mono" : "",
        )}
      >
        {props.now === "" ? "(absent)" : props.now}
      </code>
    </div>
  </div>
);

/* -------------------------------------------------------------------------- */
/* The panel                                                                  */
/* -------------------------------------------------------------------------- */

const ORIGIN_NOTE: Record<string, string> = {
  prometheus_api: "Read from the Prometheus rules API — the authoritative source.",
  generator_url:
    "Reconstructed from the alert's generatorURL because the rules API was not reachable. The expression is what the URL encoded, not what the file said.",
  unavailable:
    "oto could not obtain the rule at all. The expression below is empty, and that absence is recorded rather than guessed at.",
};

const CONFIDENCE_NOTE: Record<string, string> = {
  exact: "Exactly one rule matched this alert.",
  probable: "More than one rule could have produced this alert; oto picked the best match.",
  ambiguous:
    "Several rules matched equally well. Treat the definition below as one candidate, not as certainty.",
  none: "No rule matched. Nothing below is a claim about your configuration.",
};

export const RulePanel: Component<{ readonly history: RuleHistory }> = (props) => {
  const current = (): RuleSnapshot | null => props.history.current;

  return (
    <Panel>
      <PanelHeader>
        <PanelTitle>Rule at fire time</PanelTitle>
        <Show when={props.history.versions.length > 0}>
          <span class="text-[11px] text-ink-subtle">
            {props.history.versions.length} version
            {props.history.versions.length === 1 ? "" : "s"} captured
          </span>
        </Show>
      </PanelHeader>

      <Show
        when={current()}
        fallback={
          <EmptyState
            title="No rule was captured for this occurrence."
            body="oto records the absence rather than guessing. The rules API may have been unreachable when this fired, or the alert may not come from a Prometheus rule at all."
          />
        }
      >
        {(rule) => (
          <div class="space-y-3 p-3">
            {/* The drift diff comes FIRST. If the rule moved, that is the
                headline, not a footnote under the current definition. */}
            <Show when={props.history.change}>
              {(change) => <RuleDiff change={change()} />}
            </Show>

            <dl class="space-y-1">
              <DataRow term="Rule">
                <span class="font-mono text-[12px]">{rule().rule_name}</span>
              </DataRow>
              <Show when={rule().rule_group !== ""}>
                <DataRow term="Group">
                  <span class="font-mono text-[12px]">{rule().rule_group}</span>
                </DataRow>
              </Show>
              <Show when={rule().rule_file !== ""}>
                <DataRow term="File">
                  <span class="break-all font-mono text-[12px] text-ink-muted">
                    {rule().rule_file}
                  </span>
                </DataRow>
              </Show>
              <DataRow term="for:">
                <span class="font-mono text-[12px]">{duration(rule().for_seconds)}</span>
                <Show when={rule().keep_firing_for_seconds > 0}>
                  <span class="ml-2 text-[11px] text-ink-subtle">
                    keep_firing_for {duration(rule().keep_firing_for_seconds)}
                  </span>
                </Show>
              </DataRow>
              <DataRow term="Captured">
                <span title={absoluteTime(rule().captured_at)}>
                  <RelativeTime value={rule().captured_at} label="Captured" /> ago
                </span>
              </DataRow>
            </dl>

            <div>
              <p class="mb-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted">
                Expression
              </p>
              <pre class="overflow-x-auto rounded-[4px] border border-line bg-sunken px-2 py-1.5 font-mono text-[11px] leading-relaxed text-ink">
                <code>{rule().expr === "" ? "(oto could not read this rule)" : rule().expr}</code>
              </pre>
            </div>

            {/* Provenance. A snapshot that came from a generatorURL is a weaker
                claim than one read from the rules API, and saying which is the
                difference between evidence and a guess. */}
            <div class="flex flex-wrap items-center gap-1.5">
              <Chip title={ORIGIN_NOTE[rule().origin] ?? ""}>origin: {rule().origin}</Chip>
              <Chip title={CONFIDENCE_NOTE[rule().match_confidence] ?? ""}>
                match: {rule().match_confidence}
                {rule().candidate_count > 1 ? ` (${rule().candidate_count} candidates)` : ""}
              </Chip>
              <Show when={rule().prometheus_url}>
                {(url) => (
                  <a
                    href={url()}
                    target="_blank"
                    rel="noreferrer noopener"
                    class="text-[11px] text-accent hover:underline"
                  >
                    Prometheus ↗
                  </a>
                )}
              </Show>
            </div>

            <p class="text-[11px] leading-snug text-ink-subtle">
              {ORIGIN_NOTE[rule().origin] ?? ""} {CONFIDENCE_NOTE[rule().match_confidence] ?? ""}
            </p>

            <Show when={props.history.versions.length > 1}>
              <VersionHistory
                versions={props.history.versions}
                currentId={rule().id}
                ruleKey={props.history.rule_key}
              />
            </Show>
          </div>
        )}
      </Show>
    </Panel>
  );
};

/**
 * Every version oto has captured for this RuleKey, newest first.
 *
 * It answers "has anyone been changing this?" at a glance, which is a question
 * with no other honest answer once the file has been edited.
 *
 * The embedded array stops at 200. That used to be the end of the road — the
 * history was served from a 200-version in-memory window, so a heavily edited
 * rule simply had history nothing could show. `GET /api/v1/rule-snapshots` is
 * now keyset-paginated over `(captured_at, id)` for real, so on hitting the cap
 * this keeps reading rather than presenting a truncated list as if it were
 * complete.
 */
const VersionHistory: Component<{
  readonly versions: readonly RuleSnapshot[];
  readonly currentId: string;
  readonly ruleKey: RuleHistory["rule_key"];
}> = (props) => {
  const [cursor, setCursor] = createSignal<string | null>(null);
  const [started, setStarted] = createSignal(false);
  const [older, setOlder] = createSignal<readonly RuleSnapshot[]>([]);

  const atCap = (): boolean => props.versions.length >= EMBEDDED_VERSION_CAP;

  const query = createMemo<RuleSnapshotQuery>(() => {
    const q: Record<string, unknown> = {
      source_id: props.ruleKey.source_id,
      rule_name: props.ruleKey.rule_name,
      limit: EMBEDDED_VERSION_CAP,
    };
    // Both narrow an otherwise ambiguous match, and both are optional on the
    // RuleKey, so neither is sent unless it is actually there.
    if (props.ruleKey.rule_group) q["rule_group"] = props.ruleKey.rule_group;
    if (props.ruleKey.rule_file) q["rule_file"] = props.ruleKey.rule_file;
    if (cursor() !== null) q["cursor"] = cursor();
    return q as RuleSnapshotQuery;
  });

  const page = useQuery(() => ({
    queryKey: qk.rules.snapshots(query()),
    queryFn: ({ signal }: { signal: AbortSignal }) => listRuleSnapshots(query(), { signal }),
    enabled: atCap() && started(),
  }));

  const all = createMemo<readonly RuleSnapshot[]>(() => {
    const fetched = page.data?.data ?? [];
    const seen = new Set(props.versions.map((v) => v.id));
    const extra = [...older(), ...fetched].filter((v) => !seen.has(v.id));
    const deduped = new Map(extra.map((v) => [v.id, v]));
    return [...props.versions, ...deduped.values()];
  });

  const hasMore = (): boolean =>
    started() ? (page.data?.page.has_more ?? false) : atCap();

  const loadOlder = (): void => {
    if (!started()) {
      // The embedded array carries no cursor of its own, so the first press
      // starts the endpoint's own keyset from the top. The overlap with what is
      // already on screen is deduplicated by id rather than shown twice.
      setStarted(true);
      return;
    }
    setOlder(all().slice(props.versions.length));
    const next = page.data?.page.next_cursor;
    if (typeof next === "string" && next !== "") setCursor(next);
  };

  return (
    <details class="rounded-[4px] border border-line">
      <summary class="cursor-pointer list-none px-2 py-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted hover:bg-raised">
        Version history ({all().length}
        {hasMore() ? "+" : ""})
      </summary>
      <ol class="border-t border-line">
        <For each={all()}>
        {(v) => (
          <li
            class={cx(
              "border-b border-line px-2 py-1.5 last:border-b-0",
              v.id === props.currentId ? "bg-accent-fill" : "",
            )}
          >
            <div class="flex items-baseline gap-2">
              <span class="font-mono text-[10px] text-ink-subtle">
                {v.rule_fingerprint.slice(0, 12)}
              </span>
              <span class="text-[11px] text-ink-muted">
                <RelativeTime value={v.captured_at} label="Captured" /> ago
              </span>
              <span class="text-[11px] text-ink-subtle">for {duration(v.for_seconds)}</span>
              <Show when={v.id === props.currentId}>
                <span class="ml-auto text-[10px] font-semibold uppercase tracking-wide text-ink">
                  bound to this occurrence
                </span>
              </Show>
            </div>
            <code class="mt-0.5 block truncate font-mono text-[10px] text-ink-muted" title={v.expr}>
              {v.expr === "" ? "(unavailable)" : v.expr}
            </code>
          </li>
        )}
        </For>
      </ol>

      <Show when={hasMore()}>
        <div class="border-t border-line px-2 py-1.5 text-center">
          <Button size="sm" busy={page.isFetching} onClick={loadOlder}>
            Load older versions
          </Button>
          <p class="mt-1 text-[10px] leading-snug text-ink-subtle">
            The alert's own response embeds at most {EMBEDDED_VERSION_CAP} captures. There are more,
            and they are reachable — this pages the full history with a keyset cursor.
          </p>
        </div>
      </Show>
    </details>
  );
};
