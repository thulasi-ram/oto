/**
 * The delivery-drill result screen.
 *
 * ⭐⭐ ITS ONE JOB IS TO NAME THE STAGE THAT FAILED. "Test failed" is what the
 * channel test already says and it is worth almost nothing; "no notification
 * policy matched this group's labels" sends an operator straight to the screen
 * that fixes it. Every stage is rendered, including the ones that never started,
 * because how far it got is half of what this is read for.
 *
 * ⛔ TIER A ONLY (§M, ADR 0012). A drill stage is not an alert state, so nothing
 * here may reach for a saturated `--oto-state-*` hue: scarcity is what makes
 * those mean something. Status is carried by a glyph, a word and font weight —
 * three channels, none of them colour — which is also what keeps it readable for
 * anyone who cannot separate red from green.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { disposeDeliveryDrill, getDeliveryDrill, startDeliveryDrill } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { DeliveryDrill, DrillStage, DrillStageName } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button, Chip, Input, cx } from "~/components/ui/primitives";
import { ErrorBanner } from "~/components/ui/states";

/**
 * What each stage proves, in one line, for the operator who has never read the
 * SPEC. It is keyed by the contract's own enum so a stage added server-side
 * shows up here as a missing label rather than silently as a bare identifier.
 */
const STAGE_TITLE: Record<DrillStageName, string> = {
  accept: "Accepted",
  process: "Processed",
  identity: "Alert identity",
  occurrence: "Firing episode",
  group: "Grouped",
  rule_snapshot: "Rule snapshot",
  policy: "Notification policy",
  thread: "Chat thread",
  ordering: "Ordering gate",
  delivery: "Delivered",
};

/**
 * The glyph is the FIRST of the three channels a status is carried on, and it is
 * deliberately a character rather than an icon component: it copies and pastes
 * into a support ticket alongside the text, which an SVG does not.
 */
const STAGE_GLYPH: Record<string, string> = {
  passed: "✓",
  failed: "✕",
  skipped: "–",
  pending: "·",
};

const STAGE_WORD: Record<string, string> = {
  passed: "passed",
  failed: "failed",
  skipped: "skipped",
  pending: "waiting",
};

/** How often a running drill is re-read. The chain is four queue hops deep. */
const POLL_MS = 1500;

export const DrillPanel: Component<{ readonly sourceID: string }> = (props) => {
  const client = useQueryClient();
  const [drillID, setDrillID] = createSignal<string | null>(null);
  const [severity, setSeverity] = createSignal("");
  const [open, setOpen] = createSignal(false);

  const start = useMutation(() => ({
    mutationFn: () => startDeliveryDrill(props.sourceID, severity().trim() || undefined),
    onSuccess: (drill: DeliveryDrill) => {
      setDrillID(drill.id);
      client.setQueryData(qk.settings.drill(drill.id), drill);
    },
  }));

  const drill = useQuery(() => ({
    queryKey: qk.settings.drill(drillID() ?? ""),
    queryFn: ({ signal }: { signal: AbortSignal }) => getDeliveryDrill(drillID() ?? "", { signal }),
    enabled: drillID() !== null,
    // ⭐ Polling stops the moment the verdict is frozen. A settled drill returns
    // the same bytes forever, so continuing to ask would be pure noise on a
    // screen an operator may well leave open.
    refetchInterval: () => {
      const d = client.getQueryData<DeliveryDrill>(qk.settings.drill(drillID() ?? ""));
      return d && d.status !== "running" ? false : POLL_MS;
    },
  }));

  const dispose = useMutation(() => ({
    mutationFn: () => disposeDeliveryDrill(drillID() ?? ""),
    onSuccess: (updated: DeliveryDrill) => {
      client.setQueryData(qk.settings.drill(updated.id), updated);
    },
  }));

  const current = (): DeliveryDrill | undefined => drill.data ?? start.data ?? undefined;

  return (
    <div class="mt-1.5 rounded-[4px] border border-line bg-sunken px-2 py-1.5">
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="text-[11px] font-medium text-ink underline decoration-line-strong underline-offset-2"
          onClick={() => setOpen(!open())}
          aria-expanded={open()}
        >
          Delivery drill
        </button>
        <span class="text-[11px] text-ink-subtle">
          Pushes one synthetic alert through the real pipeline — ingestion, grouping, the policy
          match, the thread, the delivery record — and names the stage that fails.
        </span>
      </div>

      <Show when={open()}>
        <div class="mt-1.5 flex flex-wrap items-end gap-2">
          <label class="flex flex-col gap-0.5">
            <span class="text-[11px] text-ink-subtle">
              severity to fire at (policies usually match on it)
            </span>
            <Input
              value={severity()}
              placeholder="warning"
              onInput={(e) => setSeverity(e.currentTarget.value)}
            />
          </label>
          <Button size="sm" busy={start.isPending} onClick={() => start.mutate()}>
            Run a drill
          </Button>
          <Show when={current() && current()!.status !== "running" && !current()!.disposed_at}>
            <Button
              size="sm"
              variant="ghost"
              busy={dispose.isPending}
              onClick={() => dispose.mutate()}
              title="Delete the synthetic alert this drill created. The record that the drill ran is kept."
            >
              Delete the synthetic alert
            </Button>
          </Show>
        </div>

        <Show when={start.error !== null || drill.error !== null || dispose.error !== null}>
          <ErrorBanner error={start.error ?? drill.error ?? dispose.error} class="mt-1.5" />
        </Show>

        <Show when={current()}>{(d) => <DrillResult drill={d()} />}</Show>
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const DrillResult: Component<{ readonly drill: DeliveryDrill }> = (props) => {
  const d = (): DeliveryDrill => props.drill;

  /**
   * The headline sentence. It is written per verdict rather than assembled,
   * because `timed_out` and `failed` need to send an operator to different
   * places and a generic "something went wrong" would send them to neither.
   */
  const headline = createMemo(() => {
    const drill = d();
    switch (drill.status) {
      case "passed":
        return "Every stage passed. A real alert from this source would reach the same channel, in a thread, with the same card, and be recorded.";
      case "failed":
        return `Stopped at ${STAGE_TITLE[drill.failed_stage as DrillStageName] ?? drill.failed_stage}.`;
      case "timed_out":
        return "The chain did not finish inside 90 seconds. The pipeline may still complete — but nothing picking the work up usually means no worker process is running.";
      default:
        return "Running. Each stage below is read from the row the real pipeline writes, not from anything reporting itself.";
    }
  });

  return (
    <div class="mt-1.5">
      <p class="text-[11px] font-medium leading-snug text-ink">{headline()}</p>

      <ol class="mt-1.5 flex flex-col gap-0.5">
        <For each={d().stages}>{(stage) => <StageRow stage={stage} />}</For>
      </ol>

      <Show when={d().destinations.length > 0}>
        <ul class="mt-1.5 flex flex-col gap-0.5">
          <For each={d().destinations}>
            {(dest) => (
              <li class="flex flex-wrap items-center gap-1.5 text-[11px] text-ink-muted">
                <Chip>{dest.channel_name}</Chip>
                <span>{dest.status}</span>
                <span class="text-ink-subtle">{dest.mode}</span>
                <Show when={dest.thread_id}>
                  {(ts) => <span class="font-mono text-ink-subtle">ts {ts()}</span>}
                </Show>
                <Show when={dest.error}>
                  {(err) => (
                    <span class="font-medium text-ink">
                      {err()}
                      {dest.error_class ? ` (${dest.error_class})` : ""}
                    </span>
                  )}
                </Show>
              </li>
            )}
          </For>
        </ul>
      </Show>

      <p class="mt-1.5 text-[11px] leading-snug text-ink-subtle">
        Started by {d().started_by_label}, <RelativeTime value={d().started_at} label="Started" />{" "}
        ago.{" "}
        <Show
          when={d().disposed_at}
          fallback={
            <>
              The synthetic alert is excluded from every statistic and from the alert list, and is
              deleted automatically a day after the drill finishes.
            </>
          }
        >
          The synthetic alert has been deleted. This result is kept.
        </Show>
      </p>
    </div>
  );
};

const StageRow: Component<{ readonly stage: DrillStage }> = (props) => {
  const st = (): DrillStage => props.stage;
  const facts = (): readonly [string, string][] =>
    Object.entries(st().facts ?? {}) as [string, string][];

  return (
    <li class="flex items-start gap-1.5 text-[11px] leading-snug">
      <span
        aria-hidden="true"
        class={cx(
          "w-3 shrink-0 text-center font-mono",
          st().status === "failed" ? "font-bold text-ink" : "text-ink-subtle",
        )}
      >
        {STAGE_GLYPH[st().status] ?? "·"}
      </span>
      <div class="min-w-0">
        <span
          class={cx(
            st().status === "failed" ? "font-semibold text-ink" : "font-medium text-ink-muted",
          )}
        >
          {STAGE_TITLE[st().name as DrillStageName] ?? st().name}
        </span>
        {/*
          The word is the second channel and is never dropped: a glyph alone is
          unreadable to a screen reader and ambiguous at a glance.
        */}
        <span class="ml-1 text-ink-subtle">{STAGE_WORD[st().status] ?? st().status}</span>
        <Show when={st().detail}>
          <span class="ml-1 text-ink-muted">— {st().detail}</span>
        </Show>
        <Show when={facts().length > 0}>
          <span class="ml-1 font-mono text-ink-subtle">
            <For each={facts()}>{([k, v]) => <span class="mr-1.5">{`${k}=${v}`}</span>}</For>
          </span>
        </Show>
      </div>
    </li>
  );
};
