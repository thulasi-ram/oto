/**
 * Enrichment results, with their provenance.
 *
 * Every result records **which enricher produced it, at which version, in which
 * phase, how long it took and whether it came from cache** — so a wrong answer
 * can always be traced back to its producer rather than being blamed on "the
 * tool". That provenance is the point of the panel, not a footnote on it, so it
 * is rendered next to every result rather than hidden behind a disclosure.
 *
 * A failed enricher is shown, never hidden. Enrichment is additive and never
 * blocks a notification, and saying so is what stops a missing runbook link
 * being read as a missing alert.
 *
 * ⭐ AND A RESULT IS READ, NOT DECODED. `payload` is `additionalProperties: true`
 * in the contract — one shape per enricher, versioned by `enricher_version` —
 * so the panel cannot type it, but it can still know the four enrichers v1
 * ships. `PAYLOAD_VIEW` maps an enricher id to a reader for its shape and falls
 * back to pretty-printed JSON for anything it has not been taught, which is
 * what an enricher added tomorrow gets. See `PromRulePayload`.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { Dynamic } from "solid-js/web";

import type { Enrichment, EnrichmentStatus } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Chip, DataRow, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { duration } from "~/lib/format";
import { PANEL_CODE_BLOCK, PANEL_HEADER, PANEL_ROW } from "./rhythm";

/** Tier A throughout: an enricher's health is not an alert's state (§M.2). */
const STATUS_NOTE: Record<EnrichmentStatus, string> = {
  ok: "Completed.",
  partial: "Completed, but not everything it looked for was available.",
  skipped: "Did not run — its preconditions were not met.",
  failed: "Failed. The alert is unaffected; enrichment never blocks a notification.",
  timeout: "Ran out of time. The alert is unaffected.",
};

const STATUS_WEIGHT: Record<EnrichmentStatus, string> = {
  ok: "text-ink-muted",
  partial: "text-ink",
  skipped: "text-ink-subtle",
  failed: "font-medium text-ink",
  timeout: "font-medium text-ink",
};

/** Whether an instant is already behind us — which verb the expiry chip takes. */
function lapsed(ts: string): boolean {
  const t = new Date(ts).getTime();
  return Number.isFinite(t) && t < Date.now();
}

export interface EnrichmentPanelProps {
  readonly enrichments: readonly Enrichment[];
  readonly loading: boolean;
  readonly error: unknown;
}

export const EnrichmentPanel: Component<EnrichmentPanelProps> = (props) => (
  <Panel>
    <PanelHeader class={PANEL_HEADER}>
      <PanelTitle>Enrichment</PanelTitle>
      <Show when={props.enrichments.length > 0}>
        <span class="shrink-0 text-meta text-ink-subtle">{props.enrichments.length} results</span>
      </Show>
    </PanelHeader>

    <Switch>
      <Match when={props.loading}>
        <LoadingLine />
      </Match>
      <Match when={props.error !== null && props.error !== undefined}>
        <ErrorState error={props.error} />
      </Match>
      <Match when={props.enrichments.length === 0}>
        <EmptyState
          title="No enrichment ran for this alert."
          body="Enrichers attach context — the rule, a runbook, this alert's own history, any matching silence. None of them produced a result here."
        />
      </Match>
      <Match when={true}>
        <ul>
          <For each={props.enrichments}>{(e) => <EnrichmentRow enrichment={e} />}</For>
        </ul>
      </Match>
    </Switch>
  </Panel>
);

const EnrichmentRow: Component<{ readonly enrichment: Enrichment }> = (props) => {
  const [open, setOpen] = createSignal(false);
  const e = (): Enrichment => props.enrichment;
  const hasPayload = (): boolean => Object.keys(e().payload).length > 0;

  return (
    <li class={cn("border-b border-line last:border-b-0", PANEL_ROW)}>
      <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
        <span class="font-mono text-body font-medium text-ink">{e().enricher}</span>
        <span class={cn("text-meta", STATUS_WEIGHT[e().status])} title={STATUS_NOTE[e().status]}>
          {e().status}
        </span>
        <span class="ml-auto text-meta text-ink-subtle">
          <RelativeTime value={e().computed_at} label="Computed" /> ago
        </span>
      </div>

      {/* Provenance. Version, phase, timing and cache origin, always. */}
      <div class="mt-sm flex flex-wrap items-center gap-2xs">
        <Chip title="Bumping the version invalidates the cache and forces a re-run.">
          v{e().enricher_version}
        </Chip>
        <Chip title="Phase 1 runs before the first notification; phase 2 runs after.">
          phase {e().phase}
        </Chip>
        <Show when={e().duration_ms !== undefined}>
          <Chip>{e().duration_ms} ms</Chip>
        </Show>
        <Chip
          title={
            e().from_cache
              ? "Served from cache — this result was computed for an earlier firing episode."
              : "Computed fresh for this firing episode."
          }
        >
          {e().from_cache ? "cached" : "fresh"}
        </Chip>
        {/* ⛔ THE PREPOSITION BELONGS TO THE FORMATTER, NOT TO THIS CHIP.
            `relativeTime` already renders a FUTURE instant as `in 2h` and a
            past one bare, so a hard-coded "in" here spelled the chip "expires
            in in 2h" — and on a cache entry that had already lapsed it spelled
            it "expires in 1d" about a moment that is behind us. Every other
            future-valued call site in the app leaves the "in" to the formatter
            ("resumes", "expires", "asked for quiet until"); this was the one
            that did not.

            The verb is the caller's, though, and it is the caller that knows
            an enrichment cache entry can be read on either side of its own
            expiry: `expired 1d ago` and `expires in 2h` are two different
            facts about whether the next case re-runs this enricher. */}
        <Show when={e().expires_at}>
          {(exp) => (
            <Chip title="After this the result is recomputed rather than reused.">
              {lapsed(exp()) ? "expired" : "expires"}{" "}
              <RelativeTime value={exp()} label="Expires" />
              {lapsed(exp()) ? " ago" : ""}
            </Chip>
          )}
        </Show>
      </div>

      <Show when={e().error}>
        {(err) => (
          <p class="mt-sm border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink">
            {err()}
          </p>
        )}
      </Show>

      <Show when={(e().warnings ?? []).length > 0}>
        <ul class="mt-sm space-y-2xs border-l-2 border-line pl-sm">
          <For each={e().warnings ?? []}>
            {(w) => <li class="text-meta leading-snug text-ink-muted">{w}</li>}
          </For>
        </ul>
      </Show>

      <Show when={hasPayload()}>
        <div class="mt-sm">
          <button
            type="button"
            class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
            aria-expanded={open()}
            onClick={() => setOpen(!open())}
          >
            {open() ? "Hide" : "Show"} result
          </button>
          <Show when={open()}>
            <div class="mt-sm">
              <PayloadView enricher={e().enricher} payload={e().payload} />
            </div>
          </Show>
        </div>
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */
/* Payload views                                                              */
/* -------------------------------------------------------------------------- */

type Payload = Enrichment["payload"];

/** The three readers a payload needs, each returning the absent case, not `undefined`. */
const text = (p: Payload, k: string): string => (typeof p[k] === "string" ? (p[k] as string) : "");
const count = (p: Payload, k: string): number => (typeof p[k] === "number" ? (p[k] as number) : 0);
const yes = (p: Payload, k: string): boolean => p[k] === true;

/** The label above a code block, matching `RulePanel`'s "Expression" heading. */
const CODE_LABEL = "mb-sm text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted";

/**
 * `prom.rule` — the differentiator's own result, read as fields.
 *
 * The shape is `internal/enrichment/enrichers/promrule.Payload` and the house
 * pattern is `RulePanel` on the alert's screen ("Rule at fire time"): the
 * identity of the rule as a `<dl>`, the expression in its own labelled block,
 * and the provenance — origin, match confidence, candidate count — as chips,
 * because an `ambiguous` match is a claim about certainty and must never be
 * flattened into "here is your rule".
 *
 * This is a PROJECTION of the snapshot, not the snapshot: the full definition,
 * its version history and the drift diff live on `/alerts/:id`. What is here is
 * what a person reads in three seconds while looking at one firing episode.
 */
const PromRulePayload: Component<{ readonly payload: Payload }> = (props) => {
  const p = (): Payload => props.payload;

  return (
    <div class="space-y-sm">
      <Show
        when={yes(p(), "available")}
        fallback={
          <p class="text-meta leading-snug text-ink-muted">
            The enricher ran and captured no definition. That is an ordinary outcome — the alert may
            not come from a Prometheus alerting rule, or the rules API was not reachable when it
            fired — and nothing below is a claim about your configuration.
          </p>
        }
      >
        <dl class="space-y-2xs">
          <Show when={text(p(), "rule_name") !== ""}>
            <DataRow term="Rule">
              <span class="break-all font-mono text-body">{text(p(), "rule_name")}</span>
            </DataRow>
          </Show>
          <Show when={text(p(), "rule_group") !== ""}>
            <DataRow term="Rule group">
              <span class="break-all font-mono text-body">{text(p(), "rule_group")}</span>
            </DataRow>
          </Show>
          <Show when={text(p(), "rule_file") !== ""}>
            <DataRow term="File">
              <span class="break-all font-mono text-body text-ink-muted">
                {text(p(), "rule_file")}
              </span>
            </DataRow>
          </Show>
          <DataRow term="for:">
            <span class="font-mono text-body">{duration(count(p(), "for_seconds"))}</span>
            <Show when={count(p(), "keep_firing_for_seconds") > 0}>
              <span class="ml-sm text-meta text-ink-subtle">
                keep_firing_for {duration(count(p(), "keep_firing_for_seconds"))}
              </span>
            </Show>
          </DataRow>
        </dl>

        <Show when={text(p(), "expr") !== ""}>
          <div>
            <p class={CODE_LABEL}>Expression</p>
            <pre class={PANEL_CODE_BLOCK}>
              <code>{text(p(), "expr")}</code>
            </pre>
          </div>
        </Show>
      </Show>

      {/* Provenance, and the drift flags beside it: a rule that was EDITED
          between the last capture and this one is the headline of this payload,
          not a footnote under it. */}
      <div class="flex flex-wrap items-center gap-2xs">
        <Show when={text(p(), "origin") !== ""}>
          <Chip title="Whether the definition was read from the Prometheus rules API or decoded out of the alert's generatorURL.">
            origin: {text(p(), "origin")}
          </Chip>
        </Show>
        <Show when={text(p(), "match_confidence") !== ""}>
          <Chip title="`ambiguous` means several rules matched equally well and oto is not resolving that for you.">
            match: {text(p(), "match_confidence")}
            {count(p(), "candidate_count") > 1 ? ` (${count(p(), "candidate_count")} candidates)` : ""}
          </Chip>
        </Show>
        <Show when={yes(p(), "drifted")}>
          <Chip title="The definition changed between the previous capture and this one.">
            edited since last capture
          </Chip>
        </Show>
        <Show when={yes(p(), "new_version")}>
          <Chip title="oto had not seen this definition before, so it stored a new version.">
            new version
          </Chip>
        </Show>
      </div>

      <Show when={Array.isArray(p()["notes"]) && (p()["notes"] as unknown[]).length > 0}>
        <ul class="space-y-2xs">
          <For each={(p()["notes"] as unknown[]).map(String)}>
            {(n) => <li class="font-mono text-meta leading-snug text-ink-subtle">{n}</li>}
          </For>
        </ul>
      </Show>
    </div>
  );
};

/**
 * The fallback, and it is a real answer rather than a placeholder.
 *
 * An enricher this panel has not been taught still shows its whole result; it
 * just shows it as JSON. The block wraps for the same reason the expression
 * does — a `runbook.link` payload carries URLs, which are exactly as long and
 * exactly as unbreakable as a metric selector — and keeps a vertical cap so one
 * large payload cannot push everything under it off the panel.
 */
const JsonPayload: Component<{ readonly payload: Payload }> = (props) => (
  <pre class={cn("max-h-64 overflow-y-auto", PANEL_CODE_BLOCK)}>
    <code>{JSON.stringify(props.payload, null, 2)}</code>
  </pre>
);

/** Enricher id → the reader for its payload shape. Absent means "show the JSON". */
const PAYLOAD_VIEW: Readonly<Record<string, Component<{ readonly payload: Payload }>>> = {
  "prom.rule": PromRulePayload,
};

const PayloadView: Component<{ readonly enricher: string; readonly payload: Payload }> = (
  props,
) => {
  const View = (): Component<{ readonly payload: Payload }> =>
    PAYLOAD_VIEW[props.enricher] ?? JsonPayload;
  return <Dynamic component={View()} payload={props.payload} />;
};
