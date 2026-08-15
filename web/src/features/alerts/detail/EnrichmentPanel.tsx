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
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";

import type { Enrichment, EnrichmentStatus } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Chip, Panel, PanelHeader, PanelTitle, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorState, LoadingLine } from "~/components/ui/states";

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

export interface EnrichmentPanelProps {
  readonly enrichments: readonly Enrichment[];
  readonly loading: boolean;
  readonly error: unknown;
}

export const EnrichmentPanel: Component<EnrichmentPanelProps> = (props) => (
  <Panel>
    <PanelHeader>
      <PanelTitle>Enrichment</PanelTitle>
      <Show when={props.enrichments.length > 0}>
        <span class="text-meta text-ink-subtle">{props.enrichments.length} results</span>
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
    <li class="border-b border-line px-3 py-2 last:border-b-0">
      <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span class="font-mono text-body font-medium text-ink">{e().enricher}</span>
        <span class={cx("text-meta", STATUS_WEIGHT[e().status])} title={STATUS_NOTE[e().status]}>
          {e().status}
        </span>
        <span class="ml-auto text-meta text-ink-subtle">
          <RelativeTime value={e().computed_at} label="Computed" /> ago
        </span>
      </div>

      {/* Provenance. Version, phase, timing and cache origin, always. */}
      <div class="mt-1 flex flex-wrap items-center gap-1">
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
              ? "Served from cache — this result was computed for an earlier occurrence."
              : "Computed fresh for this occurrence."
          }
        >
          {e().from_cache ? "cached" : "fresh"}
        </Chip>
        <Show when={e().expires_at}>
          {(exp) => (
            <Chip title="After this the result is recomputed rather than reused.">
              expires in <RelativeTime value={exp()} />
            </Chip>
          )}
        </Show>
      </div>

      <Show when={e().error}>
        {(err) => (
          <p class="mt-1 border-l-2 border-line-strong pl-2 text-meta leading-snug text-ink">
            {err()}
          </p>
        )}
      </Show>

      <Show when={(e().warnings ?? []).length > 0}>
        <ul class="mt-1 space-y-0.5 border-l-2 border-line pl-2">
          <For each={e().warnings ?? []}>
            {(w) => <li class="text-meta leading-snug text-ink-muted">{w}</li>}
          </For>
        </ul>
      </Show>

      <Show when={hasPayload()}>
        <div class="mt-1">
          <button
            type="button"
            class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
            aria-expanded={open()}
            onClick={() => setOpen(!open())}
          >
            {open() ? "Hide" : "Show"} result
          </button>
          <Show when={open()}>
            <pre class="mt-1 max-h-64 overflow-auto rounded-control border border-line bg-sunken px-2 py-1.5 font-mono text-meta leading-relaxed text-ink">
              <code>{JSON.stringify(e().payload, null, 2)}</code>
            </pre>
          </Show>
        </div>
      </Show>
    </li>
  );
};
