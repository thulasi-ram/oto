/**
 * "My alert never appeared." — the panel that answers it from the product.
 *
 * ⭐⭐ THE EMPTY FEED IS THE POINT, not the failure mode. oto records every alert
 * it refuses and every batch that stopped, so "nothing here" is a *finding*: the
 * alert was not turned away at the door, which sends the operator to the drill
 * above rather than to `psql`. Both halves are shown together because they are
 * different facts — a rejection is an alert oto read and refused, a failed batch
 * is a payload it never finished reading — and the operator does not know in
 * advance which one they are looking at.
 *
 * ⛔ TIER A ONLY (§M, ADR 0012). A refusal is not an alert state, so nothing here
 * reaches for a saturated `--oto-state-*` hue. Weight and wording carry it.
 *
 * The sibling of `DrillPanel`, and deliberately built to the same shape: a
 * disclosure under the source row, closed until asked, so a settings screen with
 * twenty sources issues no requests for any of this until someone has a question.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createMemo,
  createSignal,
  createUniqueId,
  type Component,
} from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { listSourceFailedBatches, listSourceRejections } from "~/api/endpoints";
import { RejectionReasonSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type {
  FailedBatch,
  FailedBatchStatus,
  Rejection,
  RejectionListQuery,
  RejectionReason,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { Chip } from "~/components/ui/surfaces";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { ErrorState } from "~/components/ui/states";
import { count as fmtCount, formatLabels, truncate } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";

/**
 * Each reason, in the words an operator would use.
 *
 * ⛔ TYPED AGAINST THE CONTRACT'S ENUM. A reason the server starts recording is a
 * build failure here rather than a raw wire token — `too_many_labels` — on the
 * one screen whose whole job is to say why something was refused. The *list* the
 * filter offers comes from `RejectionReasonSchema`, the contract's own picklist;
 * this only supplies the English.
 *
 * They are phrased short and lower-case because each one is used twice: as the
 * headline of a row and as the label of a filter chip.
 */
const REASON_LABEL: Record<RejectionReason, string> = {
  too_many_labels: "too many labels",
  label_value_too_large: "label value too large",
  label_name_too_large: "label name too large",
  labelset_too_large: "label set too large",
  too_many_annotations: "too many annotations",
  annotation_too_large: "annotation too large",
  annotation_unstorable: "annotation could not be stored",
  missing_alertname: "no alertname",
  invalid_label_name: "invalid label name",
  invalid_label_value: "invalid label value",
  timestamp_out_of_window: "timestamp outside the window",
  too_many_alerts: "too many alerts in one batch",
  body_too_large: "body too large",
  undecodable: "body could not be decoded",
  unknown_source: "unknown source",
};

/**
 * `failed` and `partial` are not two shades of the same thing, and the words say
 * so: one stopped because oto decided to stop, the other stopped because the
 * process handling it died — which is why `partial` usually carries no `error`.
 */
const BATCH_LABEL: Record<FailedBatchStatus, string> = {
  failed: "failed",
  partial: "stopped part-way",
};

/** One page. Small: this is a nested panel, not a screen. */
const PAGE_SIZE = 20;

/** How many labels a rejection shows before the rest are folded behind a count. */
const LABELS_SHOWN = 8;

/** A refused value can be a whole megabyte. Nothing wide reaches the layout. */
const VALUE_CHARS = 28;

/**
 * The one line each half of the panel keeps mounted to say where it stands.
 *
 * ⛔ A LIVE REGION IS NEVER BORN HOLDING ITS TEXT. A node that enters the DOM in
 * the same mutation as the words inside it is commonly announced by nothing at
 * all — that is open ticket `56c4728`, and this panel had it seven times over.
 * So each async section mounts exactly one region before any answer exists and
 * only ever swaps its words, which is the shape that works in `AppShell`,
 * `routes/alerts` and `routes/cases`. `tone` carries the styling that used to
 * live on the branch this sentence was hoisted out of; an empty `text` renders
 * an empty block, which takes no room and stays mounted for the recovery.
 */
interface Standing {
  readonly text: string;
  readonly tone: string;
}

/** The one "Load more" both halves share, named for the page size it adds. */
const LoadMore: Component<{
  readonly hasMore: boolean;
  readonly busy: boolean;
  readonly onLoadMore: () => void;
}> = (props) => (
  <Show when={props.hasMore}>
    <Button class="mt-xs" size="sm" variant="ghost" busy={props.busy} onClick={props.onLoadMore}>
      Load {PAGE_SIZE} more
    </Button>
  </Show>
);

export const RejectionsPanel: Component<{ readonly sourceID: string }> = (props) => {
  const [open, setOpen] = createSignal(false);
  /** Named by the toggle, so the disclosure has somewhere to send a reader. */
  const panelID = createUniqueId();

  return (
    <div class="rounded-control border border-line bg-sunken px-md py-sm">
      <div class="flex flex-wrap items-center gap-sm">
        <button
          type="button"
          class="text-meta font-medium text-ink underline decoration-line-strong underline-offset-2"
          onClick={() => setOpen(!open())}
          aria-expanded={open()}
          aria-controls={panelID}
        >
          Why an alert never appeared
        </button>
        <span class="text-meta text-ink-subtle">
          Everything oto refused from this source, with the reason and the label set it refused,
          and every batch that stopped before its alerts were read. Nothing is dropped silently —
          so an empty feed here is an answer, not a gap.
        </span>
      </div>

      {/* The container `aria-controls` names is mounted whether or not it holds
          anything, so the toggle never points at an element that is not there. */}
      <div id={panelID}>
        <Show when={open()}>
          <RejectionFeed sourceID={props.sourceID} />
          <FailedBatches sourceID={props.sourceID} />
        </Show>
      </div>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Refused alerts                                                             */
/* -------------------------------------------------------------------------- */

const RejectionFeed: Component<{ readonly sourceID: string }> = (props) => {
  const [reasons, setReasons] = createSignal<readonly RejectionReason[]>([]);

  // The reasons are the feed's whole filter axis, so they are its fingerprint:
  // changing them discards the cursor and the kept pages in the same pure-phase
  // read, and no request is ever built carrying the last filter's cursor
  // (§E.3) — see `createKeysetFeed`. The annotation cuts the type-inference
  // loop the closure creates: the feed reads the query's envelope, and the
  // query's key carries the feed's cursor.
  const rejections: KeysetFeed<Rejection> = createKeysetFeed({
    envelope: () => feed.data,
    isPlaceholder: () => feed.isPlaceholderData,
    keyOf: (r) => r.id,
    fingerprint: () => reasons().join(","),
  });

  const query = createMemo<RejectionListQuery>(() => {
    const cursor = rejections.cursor();
    return {
      limit: PAGE_SIZE,
      ...(reasons().length > 0 ? { reason: [...reasons()] } : {}),
      ...(cursor !== null ? { cursor } : {}),
    };
  });

  const feed = useQuery(() => ({
    queryKey: qk.settings.rejections(props.sourceID, query()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listSourceRejections(props.sourceID, query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const rows = rejections.rows;

  /** @see Standing — mounted before there is anything to say, and never remounted. */
  const standing = createMemo<Standing>(() => {
    // An error already speaks for itself in `ErrorState`. The region says
    // nothing rather than narrating over it, and stays mounted for the recovery.
    if (feed.isError) return { text: "", tone: "" };
    if (feed.isPending && rows().length === 0) {
      return { text: "Loading…", tone: "mt-sm text-meta text-ink-subtle" };
    }
    // Two different facts, never conflated (see `ui/states`): a filter that
    // matched nothing is not the same as a source that has never had anything
    // refused, and only the second one is the answer this panel exists to give.
    if (rows().length === 0 && reasons().length > 0) {
      return {
        text: "No refusal from this source matches those reasons.",
        tone: "mt-sm text-meta font-medium leading-snug text-ink",
      };
    }
    if (rows().length === 0) {
      return {
        text: "oto has never refused anything from this source.",
        tone: "mt-sm text-meta font-medium leading-snug text-ink",
      };
    }
    const more = rejections.hasMore() ? "+" : "";
    return {
      text: `${fmtCount(rows().length)}${more} refused, newest first.`,
      tone: "mt-sm text-meta text-ink-muted",
    };
  });

  return (
    <section class="mt-md">
      <h3 class="text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted">
        Refused alerts
      </h3>

      <div class="mt-xs">
        <ToggleGroup
          legend="Reason"
          showLegend
          multiple
          value={[...reasons()]}
          onChange={(next) => setReasons(next as RejectionReason[])}
        >
          <For each={RejectionReasonSchema.options}>
            {(reason) => <ToggleGroupItem value={reason}>{REASON_LABEL[reason]}</ToggleGroupItem>}
          </For>
        </ToggleGroup>
      </div>

      <p class={standing().tone} aria-live="polite" aria-atomic="true">
        {standing().text}
      </p>

      <Switch>
        <Match when={feed.isError}>
          <ErrorState error={feed.error} onRetry={() => void feed.refetch()} />
        </Match>

        <Match when={feed.isPending && rows().length === 0}>{null}</Match>

        <Match when={rows().length === 0 && reasons().length > 0}>
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            The filter is doing something — that is not the same as nothing having been refused.
          </p>
          <Button class="mt-xs" size="sm" variant="ghost" onClick={() => setReasons([])}>
            Clear reasons
          </Button>
        </Match>

        <Match when={rows().length === 0}>
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            That is an answer rather than an absence: every alert oto turns away is recorded here
            with its reason, so a missing alert was not turned away at the door. Look further down
            the chain — run the delivery drill above, or check the batches below for one that never
            finished.
          </p>
        </Match>

        <Match when={true}>
          <ol class="mt-xs">
            <For each={rows()}>{(r) => <RejectionRow rejection={r} />}</For>
          </ol>
          <LoadMore hasMore={rejections.hasMore()} busy={feed.isFetching} onLoadMore={rejections.loadMore} />
        </Match>
      </Switch>
    </section>
  );
};

const RejectionRow: Component<{ readonly rejection: Rejection }> = (props) => {
  const r = (): Rejection => props.rejection;

  return (
    <li class="border-t border-line py-sm first:border-t-0">
      <div class="flex flex-wrap items-baseline gap-xs text-meta leading-snug">
        <span class="font-semibold text-ink">{REASON_LABEL[r().reason] ?? r().reason}</span>
        <span class="text-ink-subtle">
          <RelativeTime value={r().received_at} label="Refused" /> ago
        </span>
      </div>
      {/* The specifics are written AFTER redaction server-side, so they carry no
          secret and are safe to render verbatim — but not to render unbroken: a
          `label_name_too_large` detail quotes the over-cap name itself, which is
          one ~200-character token that would give the settings column its own
          horizontal scrollbar. Wrapped mid-token, as every arbitrary server
          string on this app is. */}
      <Show when={r().detail}>
        <p class="mt-2xs min-w-0 break-words border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink-muted">
          {r().detail}
        </p>
      </Show>
      <LabelSet labels={r().labels} />
    </li>
  );
};

/* -------------------------------------------------------------------------- */
/* The refused label set                                                      */
/* -------------------------------------------------------------------------- */

/**
 * The evidence, and the one thing on this panel that is unbounded.
 *
 * A `too_many_labels` rejection carries more than 64 entries by definition and a
 * `label_value_too_large` one carries a value over 4 KiB — the set is precisely
 * the one that breaks `LabelMap`'s bounds, which is why the contract does not
 * bound it. So the rendering does: values are truncated to a readable head with
 * the whole pair on the chip's title, and the tail of a wide set is folded behind
 * a count. The full set is one click away as text, which is the form that pastes
 * into a support ticket.
 *
 * A value reading `[redacted]` is not a rendering choice — it is what is stored,
 * and there is no plaintext behind it to reveal.
 */
const LabelSet: Component<{ readonly labels: Readonly<Record<string, string>> }> = (props) => {
  const [expanded, setExpanded] = createSignal(false);

  const entries = createMemo(() =>
    Object.entries(props.labels).sort(([a], [b]) => a.localeCompare(b)),
  );
  const shown = createMemo(() => (expanded() ? entries() : entries().slice(0, LABELS_SHOWN)));
  const hidden = createMemo(() => entries().length - shown().length);

  return (
    <Show
      when={entries().length > 0}
      fallback={
        <p class="mt-2xs text-meta text-ink-subtle">
          This refusal names no alert, so there is no label set to show.
        </p>
      }
    >
      <div class="mt-2xs flex flex-wrap items-center gap-2xs">
        <For each={shown()}>
          {([k, val]) => (
            <Chip mono title={`${k}=${JSON.stringify(val)}`} class="max-w-full">
              <span class="min-w-0 truncate">
                {k}={truncate(val, VALUE_CHARS)}
              </span>
            </Chip>
          )}
        </For>
        <Show when={hidden() > 0}>
          <button
            type="button"
            class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
            aria-expanded={expanded()}
            onClick={() => setExpanded(true)}
          >
            +{fmtCount(hidden())} more
          </button>
        </Show>
        <Show when={expanded() && entries().length > LABELS_SHOWN}>
          <button
            type="button"
            class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
            aria-expanded={expanded()}
            onClick={() => setExpanded(false)}
          >
            Show fewer
          </button>
        </Show>
        <button
          type="button"
          class="text-meta text-ink-subtle hover:text-ink hover:underline"
          title="Copy as an Alertmanager matcher set"
          onClick={() => void navigator.clipboard?.writeText(formatLabels(props.labels))}
        >
          Copy labels
        </button>
      </div>
    </Show>
  );
};

/* -------------------------------------------------------------------------- */
/* Batches that stopped                                                       */
/* -------------------------------------------------------------------------- */

const FailedBatches: Component<{ readonly sourceID: string }> = (props) => {
  // No status filter: the contract's default is both, and both are the same
  // finding here — alerts that are on disk and were never read. With no filter
  // axis at all, nothing can invalidate a cursor, so the feed has no
  // fingerprint.
  const stopped: KeysetFeed<FailedBatch> = createKeysetFeed({
    envelope: () => batches.data,
    isPlaceholder: () => batches.isPlaceholderData,
    keyOf: (b) => b.id,
  });

  const query = createMemo(() => {
    const cursor = stopped.cursor();
    return { limit: PAGE_SIZE, ...(cursor !== null ? { cursor } : {}) };
  });

  const batches = useQuery(() => ({
    queryKey: qk.settings.failedBatches(props.sourceID, query()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listSourceFailedBatches(props.sourceID, query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const rows = stopped.rows;

  /** @see Standing — one region, mounted from the first render, words swap. */
  const standing = createMemo<Standing>(() => {
    if (batches.isError) return { text: "", tone: "" };
    if (batches.isPending && rows().length === 0) {
      return { text: "Loading…", tone: "mt-xs text-meta text-ink-subtle" };
    }
    if (rows().length === 0) {
      return {
        text: "Every batch from this source finished.",
        tone: "mt-xs text-meta font-medium leading-snug text-ink",
      };
    }
    const more = stopped.hasMore() ? "+" : "";
    return {
      text: `${fmtCount(rows().length)}${more} never finished, newest first.`,
      tone: "mt-xs text-meta text-ink-muted",
    };
  });

  return (
    <section class="mt-md border-t border-line pt-md">
      <h3 class="text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted">
        Batches that stopped
      </h3>

      <p class={standing().tone} aria-live="polite" aria-atomic="true">
        {standing().text}
      </p>

      <Switch>
        <Match when={batches.isError}>
          <ErrorState error={batches.error} onRetry={() => void batches.refetch()} />
        </Match>

        <Match when={batches.isPending && rows().length === 0}>{null}</Match>

        <Match when={rows().length === 0}>
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            Nothing arrived and then stalled, so a missing alert was not lost between the webhook
            and the pipeline. It was either never sent, or it was read and refused — and the feed
            above is the other half of that answer.
          </p>
        </Match>

        <Match when={true}>
          <ol class="mt-xs">
            <For each={rows()}>{(b) => <BatchRow batch={b} />}</For>
          </ol>
          <LoadMore hasMore={stopped.hasMore()} busy={batches.isFetching} onLoadMore={stopped.loadMore} />
        </Match>
      </Switch>
    </section>
  );
};

const BatchRow: Component<{ readonly batch: FailedBatch }> = (props) => {
  const b = (): FailedBatch => props.batch;

  return (
    <li class="border-t border-line py-sm first:border-t-0">
      <div class="flex flex-wrap items-baseline gap-xs text-meta leading-snug">
        <span class="font-semibold text-ink">{BATCH_LABEL[b().status] ?? b().status}</span>
        <Chip>{b().mode}</Chip>
        <span class="text-ink-subtle">
          <RelativeTime value={b().received_at} label="Received" /> ago
        </span>
        {/* What the failure cost, which is the part no metric carries. */}
        <span class="text-ink-muted">
          {fmtCount(b().alert_count)} alert{b().alert_count === 1 ? "" : "s"} never processed
        </span>
        <Show when={b().truncated_alerts > 0}>
          <span
            class="text-ink-muted"
            title="Dropped at accept time for exceeding the per-batch cap. Each one is also a `too many alerts in one batch` refusal above."
          >
            + {fmtCount(b().truncated_alerts)} dropped at the door
          </span>
        </Show>
      </div>
      <Show
        when={b().error}
        fallback={
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            No error was recorded, which is what a batch that stopped by dying looks like — the
            process handling it did not survive to write one.
          </p>
        }
      >
        {/* Whatever the pipeline wrote, wrapped mid-token: an arbitrary server
            string is not allowed to widen the settings column. */}
        <p class="mt-2xs min-w-0 break-words border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink">
          {b().error}
        </p>
      </Show>
    </li>
  );
};
