/**
 * `/cases` — what is firing that somebody needs to acknowledge.
 *
 * A **Case** is one contiguous firing episode of ONE alert: it opens when the
 * alert starts firing, it ends when the upstream resolves it or oto stops
 * hearing about it, and it is the thing a human acknowledges. An alert that has
 * fired forty times has forty cases, one after another, and never two at once.
 *
 * ⛔ AND A CASE IS TERMINAL. Nothing here renders a reopen count, because there
 * is nothing to count: an ended case is ended, and a re-fire opens the NEXT one
 * at the next `seq`, unacknowledged. `#seq` on the row is the whole of that
 * story now.
 *
 * ⛔ A CASE IS NOT A GROUP AND NEVER SPANS TWO ALERTS. Alertmanager's
 * notification grouping — one batch, one chat thread — is an AlertGroup, lives
 * at `/groups`, and is plumbing. This screen carries `group_id` as a filter so a
 * group can hand its cases over, and that is the only relationship between them.
 * It is not a correlation and it is not an incident: oto records signals.
 *
 * ⭐ THE DEFAULT VIEW IS THE QUEUE, AND IT IS SPELLED `state=open`. The screen
 * opens on episodes that are still running, because "what is firing that I need
 * to acknowledge" is the question an operator arrives with.
 *
 * ⛔ THERE IS NO `open=` PARAMETER, AND THERE IS NO SECOND CONTROL FOR IT. A case
 * has exactly one axis of its own — `alert_cases.state`, which holds `open` and
 * `closed` and nothing else — and a boolean beside a state list would have been
 * two controls spelling one fact, with the endpoint's allow-list answering the
 * boolean with a `400`. `state=open` is what reaches `case_ack_idx (org_id,
 * ack_state, started_at DESC, id DESC) WHERE ended_at IS NULL`, so the queue
 * sends it, and pairing it with `ack=unacked` is the shape that index is for.
 *
 * ⛔ THERE IS NO SORT CONTROL, AND THERE MUST NOT BE ONE. The order is fixed at
 * `-started_at` with the case id as tiebreak: a keyset cursor is only sound over
 * an indexed total order, and this list has exactly one. `GET /api/v1/cases`
 * takes no `sort` parameter at all.
 *
 * ⛔ FOUR FILTERS ARE REFUSED WITH A 400 AND ARE THEREFORE NOT OFFERED:
 * `label[…]`, `matcher`, `q` and `flapping`/`snoozed`. The first three are
 * questions about the ALERT — answered by GIN indexes on the identity, which
 * reaching once per case row would turn a keyset page into a scan — and the last
 * two are properties of the identity that say nothing about which of its firings
 * you are looking at. `/alerts` is where those are asked.
 *
 * Pagination is keyset and **append-only**, the same bargain `/alerts` makes:
 * "Load more" adds a page rather than replacing one, because numbered pages over
 * a cursor are a lie the moment a new case opens above you.
 */
import { For, Match, Show, Switch, createMemo } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { ackCase, listCases, unackCase } from "~/api/endpoints";
import { AckStateSchema, CaseStateSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { AckState, CaseListItem, CaseListQuery } from "~/api/types";
import { Elapsed, RelativeTime } from "~/components/Time";
import {
  AckChip,
  CASE_STATE_LABEL,
  CASE_STATE_RAIL,
  CaseStateChip,
  SeverityMark,
  StateChip,
  StateGlyph,
  type CaseState,
} from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { FilterRow } from "~/components/ui/FilterRow";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "~/components/ui/Select";
import { Chip } from "~/components/ui/surfaces";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { ErrorState, PageEmptyState, TableSkeleton } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { count as fmtCount, idempotencyKey, shortId } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";

const PAGE_SIZE = 100;

/**
 * Both episode states, the contract's own list rather than a copy of it.
 *
 * ⛔ `firing`, `suppressed`, `resolved` and `expired` ARE NOT ON THIS AXIS. They
 * are what an ALERT is, they are refused here with a `400`, and the four of them
 * used to sit on this bar as case filters — which read as "show me the suppressed
 * episodes" when the fact behind it is that a silence mutes an identity. What an
 * episode ended AS is `resolve_reason`, and it is on the row rather than in the
 * filter bar: `resolved`/`expired` are a one-to-one recoding of `upstream`/
 * `timeout`, so filtering by them would be filtering `closed` twice.
 */
const CASE_STATES: readonly CaseState[] = CaseStateSchema.options;

/**
 * ⭐ ABSENCE MEANS OPEN, NOT BOTH, AND THE BAR SAYS SO OUT LOUD.
 *
 * The endpoint's own default is both, which is the right default for an API and
 * the wrong one for this screen: a queue that silently included every episode
 * since the beginning of retention would answer a question nobody asked. Asking
 * for both stays one click away — it is just a deliberate choice rather than what
 * you get by not choosing — and because the default is reflected back into the
 * toggle, the screen never claims to be unfiltered while it is filtering.
 */
const DEFAULT_STATES: readonly CaseState[] = ["open"];

/** Both acknowledgement states, from the contract rather than from memory. */
const ALL_ACK: readonly AckState[] = AckStateSchema.options;

/**
 * The three severities most estates use, offered as chips.
 *
 * `severity` is a FREE VOCABULARY — the endpoint matches whatever the promoted
 * label holds — so these are a convenience, never the closed set. A value
 * outside them still filters when it arrives in the URL, and says so in the
 * narrowing strip below rather than vanishing because no chip could hold it.
 */
const COMMON_SEVERITIES = ["critical", "warning", "info"] as const;

/** An option for the small single-selects below: a value plus its label. */
interface Opt<T extends string> {
  readonly value: T;
  readonly label: string;
}

const ACK_OPTIONS: Opt<"" | AckState>[] = [
  { value: "", label: "Any" },
  { value: "unacked", label: "Unacknowledged" },
  { value: "acked", label: "Acknowledged" },
];

/** A narrowing carried in the URL that has no control of its own on the bar. */
interface Narrowing {
  readonly param: string;
  readonly label: string;
  readonly value: string;
}

export default function CasesRoute() {
  const navigate = useNavigate();
  const [params] = useSearchParams();

  const str = (key: string): string =>
    typeof params[key] === "string" ? (params[key] as string) : "";

  const csv = (key: string): readonly string[] =>
    str(key)
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s !== "");

  /**
   * The episode states in force — what the URL asks for, or the queue's default
   * when it asks for nothing. Junk in `?state=` narrows to nothing rather than
   * being sent on to be refused, and an empty result of that filtering falls
   * back to the default rather than silently meaning "both".
   */
  const states = createMemo<readonly CaseState[]>(() => {
    const picked = csv("state").filter((s): s is CaseState =>
      (CASE_STATES as readonly string[]).includes(s),
    );
    return picked.length > 0 ? picked : DEFAULT_STATES;
  });

  const ack = (): AckState | null => {
    const raw = str("ack");
    return (ALL_ACK as readonly string[]).includes(raw) ? (raw as AckState) : null;
  };

  const severities = createMemo<readonly string[]>(() => csv("severity"));

  /**
   * The filters this bar has no control for, kept because a URL is how one
   * screen hands work to another: a group links here with `group_id`, a drill
   * links here with `synthetic=true`, and dropping them on arrival would make
   * the link a lie. Each is shown, and each can be taken off.
   */
  const narrowings = createMemo<readonly Narrowing[]>(() => {
    const out: Narrowing[] = [];
    const carry: readonly (readonly [string, string])[] = [
      ["group_id", "group"],
      ["cluster", "cluster"],
      ["namespace", "namespace"],
      ["alertname", "alert"],
      ["synthetic", "synthetic"],
      ["since", "since"],
    ];
    for (const [param, label] of carry) {
      const value = str(param);
      if (value !== "") {
        out.push({ param, label, value: param === "group_id" ? shortId(value) : value });
      }
    }
    return out;
  });

  const setParams = (patch: Readonly<Record<string, string | null>>): void => {
    const next = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (typeof v === "string" && v !== "") next.set(k, v);
    }
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === "") next.delete(k);
      else next.set(k, v);
    }
    const s = next.toString();
    navigate(`/cases${s === "" ? "" : `?${s}`}`, { scroll: false });
  };

  /**
   * A cursor is minted under the whole filter set, and §E.3 answers one carried
   * across a filter change with `400 cursor_filter_mismatch`. The whole query
   * string minus the cursor is therefore the fingerprint — anything the operator
   * can change is in it by construction, so a filter added later cannot be
   * forgotten here (see `createKeysetFeed`).
   */
  const fingerprint = createMemo(() => {
    const sp = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (typeof v === "string" && v !== "") sp.set(k, v);
    }
    sp.sort();
    return sp.toString();
  });

  // The annotation cuts the type-inference loop the closure creates: the feed
  // reads the query's envelope, and the query's key carries the feed's cursor.
  const feed: KeysetFeed<CaseListItem> = createKeysetFeed({
    envelope: () => cases.data,
    isPlaceholder: () => cases.isPlaceholderData,
    keyOf: (c) => c.id,
    fingerprint,
  });

  const query = createMemo<CaseListQuery>(() => {
    const q: Record<string, unknown> = { limit: PAGE_SIZE };

    // ⭐ `state` IS ALWAYS ON THE WIRE, because `states()` always has a value:
    // the queue's default is a choice this screen makes, not one it leaves to
    // the endpoint. Naming both values is the same as omitting the parameter,
    // which is exactly what "Open + Ended" should mean.
    q["state"] = [...states()];

    if (ack() !== null) q["ack"] = [ack()];
    if (severities().length > 0) q["severity"] = [...severities()];

    for (const key of ["cluster", "namespace", "alertname", "group_id"] as const) {
      const values = csv(key);
      if (values.length > 0) q[key] = [...values];
    }
    if (str("synthetic") !== "") q["synthetic"] = str("synthetic") === "true";
    if (str("since") !== "") q["since"] = str("since");
    if (feed.cursor() !== null) q["cursor"] = feed.cursor();

    return q as CaseListQuery;
  });

  const cases = useQuery(() => ({
    queryKey: qk.cases.list(query()),
    queryFn: ({ signal }: { signal: AbortSignal }) => listCases(query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const rows = feed.rows;

  const status = (): string => {
    const n = rows().length;
    if (cases.isPending && n === 0) return "Loading…";
    return `${fmtCount(n)}${feed.hasMore() ? "+" : ""} case${n === 1 ? "" : "s"}`;
  };

  /** Nothing narrowed at all — the default queue, which has its own sentence. */
  const isDefaultView = (): boolean =>
    states().length === 1 &&
    states()[0] === "open" &&
    ack() === null &&
    severities().length === 0 &&
    narrowings().length === 0;

  return (
    // ⛔ THE `min-h-0` / `overflow-hidden` CHAIN IS LOAD-BEARING, the same one
    // `/alerts` depends on: every ancestor from `AppShell`'s `h-screen` down to
    // here refuses to grow with its content, which is what gives the list its
    // own bounded scroller instead of scrolling the document.
    <div class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <div class="shrink-0">
        <FilterRow>
          <label class="flex items-center gap-1.5 text-body text-ink-muted">
            <span>Ack</span>
            <Select<Opt<"" | AckState>>
              multiple={false}
              options={ACK_OPTIONS}
              optionValue="value"
              optionTextValue="label"
              value={ACK_OPTIONS.find((o) => o.value === (ack() ?? "")) ?? ACK_OPTIONS[0]!}
              onChange={(opt) => setParams({ ack: opt && opt.value !== "" ? opt.value : null })}
              itemComponent={(p) => <SelectItem item={p.item}>{p.item.rawValue.label}</SelectItem>}
            >
              <SelectTrigger
                id="case-ack"
                aria-label="Acknowledgement"
                title="A receipt belongs to one firing. `Unacknowledged` is the list of firings nobody has recorded seeing yet."
              >
                <SelectValue<Opt<"" | AckState>>>
                  {(state) => state.selectedOption().label}
                </SelectValue>
              </SelectTrigger>
              <SelectContent />
            </Select>
          </label>

          {/* ⭐ ONE CONTROL FOR THE ONE AXIS AN EPISODE HAS. Turning both on is
              how you ask for everything in retention; turning both off is not a
              third answer, so it falls back to the queue rather than to a list
              that could not contain anything. */}
          <ToggleGroup
            legend="Episode"
            multiple
            value={[...states()]}
            onChange={(next) => setParams({ state: next.length > 0 ? next.join(",") : null })}
          >
            <For each={CASE_STATES}>
              {(s) => (
                <ToggleGroupItem
                  value={s}
                  title={
                    s === "open"
                      ? "Episodes that have not ended. This is the queue, and it is the shape of this list the ack index is built for."
                      : "Episodes that have ended, whether the upstream resolved them or oto stopped hearing about them. A case is terminal — a re-fire is the next episode, not this one again."
                  }
                >
                  {CASE_STATE_LABEL[s]}
                </ToggleGroupItem>
              )}
            </For>
          </ToggleGroup>

          <ToggleGroup
            legend="Severity"
            multiple
            value={[...severities()]}
            onChange={(next) => setParams({ severity: next.length > 0 ? next.join(",") : null })}
          >
            <For each={COMMON_SEVERITIES}>
              {(s) => <ToggleGroupItem value={s}>{s}</ToggleGroupItem>}
            </For>
          </ToggleGroup>
        </FilterRow>
      </div>

      {/* ⛔ A STRIP, NOT A SECOND CHROME BAR — `/alerts` argues this at length,
          and the two screens have to read as one product. The polite region is
          the strip itself and is mounted unconditionally, so a count change is a
          mutation inside a region that already existed. */}
      <header
        id="case-status"
        class="flex h-9 shrink-0 items-center gap-md border-b border-line px-md"
        aria-live="polite"
      >
        <span class="text-body tabular-nums text-ink-muted">{status()}</span>

        {/* Every narrowing the bar has no control for, said out loud with its
            way off. A filter that only exists in a query string is a filter an
            operator cannot tell is on. */}
        <For each={narrowings()}>
          {(n) => (
            <button
              type="button"
              class="inline-flex items-center gap-1 rounded-chip border border-line-strong bg-raised px-1.5 py-px text-meta text-ink hover:bg-sunken"
              /* The chips are built from adjacent spans, so their text content
                 runs together into one word for a screen reader. The label is
                 written out rather than inferred. */
              aria-label={`Remove the ${n.label} filter: ${n.value}`}
              title={`Filtering on ${n.label}. Click to remove.`}
              onClick={() => setParams({ [n.param]: null })}
            >
              <span class="text-ink-subtle">{n.label}</span>
              <span class="font-mono">{n.value}</span>
              <span aria-hidden="true">×</span>
            </button>
          )}
        </For>
      </header>

      <Switch>
        <Match when={cases.isError}>
          <ErrorState error={cases.error} onRetry={() => void cases.refetch()} />
        </Match>

        <Match when={cases.isPending && rows().length === 0}>
          <div class="flex-1 overflow-hidden">
            <TableSkeleton rows={14} cols={5} />
          </div>
        </Match>

        <Match when={rows().length === 0}>
          <Switch>
            {/* ⭐ THE GOOD VERSION OF THIS SCREEN. An empty queue is an ANSWER —
                nothing is firing — and it must not borrow the sentence a typo'd
                filter gets. */}
            <Match when={isDefaultView()}>
              <PageEmptyState
                motif="kumo"
                title="Nothing is firing."
                body="Every case oto has opened has ended. A case opens the moment an alert starts firing, so this list fills itself the next time something does."
              />
            </Match>
            <Match when={true}>
              <PageEmptyState
                motif="kumo"
                title="No cases match these filters."
                body="The filters are doing something — that is not the same as there being nothing here. Clear them to see the whole queue."
                action={
                  <Button variant="secondary" size="sm" onClick={() => navigate("/cases")}>
                    Clear filters
                  </Button>
                }
              />
            </Match>
          </Switch>
        </Match>

        <Match when={true}>
          <div class="min-h-0 flex-1 overflow-auto">
            <ul>
              <For each={rows()}>{(c) => <CaseRow item={c} />}</For>
            </ul>

            <div class="flex items-center justify-center gap-3 border-t border-line bg-surface px-3 py-2">
              <Show
                when={feed.hasMore()}
                fallback={
                  <span class="text-meta text-ink-subtle">
                    That is all {fmtCount(rows().length)} of them.
                  </span>
                }
              >
                <Button
                  variant="secondary"
                  size="sm"
                  busy={cases.isFetching}
                  onClick={feed.loadMore}
                >
                  Load {fmtCount(PAGE_SIZE)} more
                </Button>
                <span class="text-meta text-ink-subtle">
                  {fmtCount(rows().length)} loaded across {feed.pageCount()} page
                  {feed.pageCount() === 1 ? "" : "s"}
                </span>
              </Show>
            </div>
          </div>
        </Match>
      </Switch>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* One row                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * One firing episode, with the identity it belongs to.
 *
 * ⛔ THE ALERT'S FIELDS COME OFF `alert`, NEVER OFF THE CASE. `alertname`,
 * `severity`, `namespace` and `cluster_key` describe the IDENTITY; a case that
 * copied them would go on asserting them after a relabel. The server batch-loads
 * the reference for the whole page, which is why the row can render them at all
 * without a request of its own.
 *
 * ⭐ AND SO DOES `state`, WHICH IS WHY THE ROW WEARS TWO CHIPS. `StateChip` says
 * what the ALERT is right now — firing, or muted by a silence — and it is read
 * through `alert` for exactly the same reason `alertname` is. `CaseStateChip`
 * says whether the EPISODE in this row is the one still running. They can
 * legitimately disagree: an ended case under an alert that has since fired again
 * is a row that says `Firing` and `Ended · resolved`, and that is the truth.
 *
 * ⛔ NOTHING HERE SPENDS A STATE HUE, AND THAT IS §M.7 RATHER THAN TASTE. The row
 * used to carry a firing tint and a lifecycle rail keyed on the case's own state;
 * §M.2's scarcity argument allows a row status only on the surfaces
 * `chromeExceptions` names, this file is not one of them, and the axis it was
 * colouring — open versus ended — is not a state hue's job anyway.
 */
const CaseRow = (props: { readonly item: CaseListItem }) => {
  const c = (): CaseListItem => props.item;
  /** The episode's own state is authoritative; `ended_at` only agrees with it. */
  const open = (): boolean => c().state === "open";

  return (
    <li class="border-b border-line bg-surface">
      <div class="flex items-center gap-3 px-3 py-2 hover:bg-raised/60">
        <A href={`/cases/${c().id}`} class="flex min-w-0 flex-1 items-start gap-3">
          <span
            aria-hidden="true"
            class={cn("mt-0.5 h-8 w-[3px] shrink-0 rounded-full", CASE_STATE_RAIL[c().state])}
          />

          <SeverityMark severity={c().alert.severity} class="mt-0.5" />

          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <span class="min-w-0 truncate text-item font-medium text-ink">
                {c().alert.alertname}
              </span>
              <StateChip state={c().alert.state} size="sm" />
              <CaseStateChip state={c().state} resolveReason={c().resolve_reason} size="sm" />
              <AckChip ackState={c().ack_state} />
              {/* Which firing of this alert this is. `#1` is its first ever, and
                  a high number is a fact worth seeing from the list — it is also
                  the whole of what a re-fire does now, since a case is terminal
                  and the next one opens at the next `seq`. */}
              <Chip title="Which firing of this alert this is, counted since oto first saw the identity. A re-fire opens the next one rather than reopening this one.">
                #{c().seq}
              </Chip>
            </div>

            <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-meta text-ink-muted">
              <span
                title={
                  open()
                    ? "How long this firing has been running. oto times the signal, never anyone's response."
                    : "How long this firing ran. oto times the signal, never anyone's response."
                }
              >
                <Elapsed from={c().started_at} to={c().ended_at ?? null} /> firing
              </span>
              <Show when={c().alert.namespace}>
                {(ns) => <span class="font-mono">namespace {ns()}</span>}
              </Show>
              <span class="font-mono">cluster {c().alert.cluster_key}</span>
              <Show when={c().acked_by_label}>
                {(who) => <span>seen by {who()}</span>}
              </Show>
            </div>
          </div>

          <span class="shrink-0 text-right text-meta text-ink-subtle">
            <RelativeTime value={c().started_at} label="Started" /> ago
          </span>
        </A>

        {/* ⛔ ONLY THE ENDED CASE DISABLES IT. An acknowledged one does not — it
            switches the control to the way back (see `RowAck`). */}
        <RowAck item={c()} disabled={!open()} />
      </div>
    </li>
  );
};

/**
 * The one row action, and the reason it is the only one.
 *
 * ⛔ VISIBLE ON EVERY ROW, ON EVERY PAINT, WITH OR WITHOUT A POINTER (§0.4). A
 * list that re-sorts as alerts fire, with an ack control that materialises under
 * the cursor, is a 3am misclick generator. A disabled button that says *why* in
 * its tooltip is information.
 *
 * ⭐ AND THIS IS THE ONLY LIST THAT MAY CARRY IT. `AlertTable` used to render the
 * same button and no longer does: a row there is an **Alert** — the identity of a
 * label set, which outlives every one of its firings — while an ack is a receipt
 * on ONE firing. Here the row IS the firing, so the button's subject is the thing
 * under the cursor.
 *
 * ⭐ IT IS ONE CONTROL WITH TWO WORDS, NOT TWO CONTROLS. `ack_state` has two
 * values, so a separate Acknowledge and Withdraw meant one of the two was always
 * dead on every row — a greyed-out button beside a live one, asking the operator
 * to read both to learn one fact. This reads what the case IS and does the other
 * thing: `Acknowledge` while the firing carries no receipt, the withdrawal's own
 * words once it does. The disabled state is now about the case having ENDED and
 * nothing else, which is the one condition under which neither direction is
 * possible.
 *
 * It writes no note either way. Prose on the record belongs on `/cases/:id`, one
 * click away, where the dialog is.
 */
const RowAck = (props: { readonly item: CaseListItem; readonly disabled: boolean }) => {
  const client = useQueryClient();

  const acked = (): boolean => props.item.ack_state === "acked";

  const receipt = useMutation(() => ({
    // One key per gesture, minted at the click. The server's idempotency promise
    // only holds if the client stops re-minting on every retry.
    //
    // ⛔ THE DIRECTION IS READ AT THE CLICK, NOT CAPTURED AT RENDER. The row
    // re-renders under SSE, and a mutation bound to the ack state of an older
    // frame is how the wrong verb gets sent to the right case.
    mutationFn: () =>
      acked()
        ? unackCase(props.item.id, undefined, idempotencyKey())
        : ackCase(props.item.id, undefined, idempotencyKey()),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.cases.all() });
      void client.invalidateQueries({ queryKey: qk.alerts.all() });
    },
  }));

  const failure = (): string | null =>
    receipt.error === null ? null : ((receipt.error as Error).message ?? "The request failed.");

  /** The verb this press would perform, for every sentence about it. */
  const verb = (): string => (acked() ? "withdraw the acknowledgement of" : "acknowledge");

  /**
   * The accessible name, and it is the ONLY place the two directions are told
   * apart: the mark is a check either way, because the check is the state of the
   * case and the row is not the place to invent a glyph for "un-check".
   */
  const label = (): string =>
    acked()
      ? `Withdraw the acknowledgement of ${props.item.alert.alertname}`
      : `Acknowledge ${props.item.alert.alertname}`;

  const title = (): string => {
    const failed = failure();
    if (failed !== null) return `oto could not ${verb()} this: ${failed}`;
    if (props.item.state === "closed") {
      return "This case has already ended, so there is no receipt to write or take back.";
    }
    if (acked()) {
      return "Withdraw the receipt from this firing, recorded as a deliberate withdrawal. A note goes on the record from the case's own screen.";
    }
    return "Record that a human has seen this firing. It stays firing, at the same severity.";
  };

  return (
    <>
      {/* ⛔ THE "ON" STATE IS TIER A INK, NOT THE ACK HUE. §M.7 lets a state
          colour onto a state badge, a row status or a timeline marker, and a
          control is none of the three — `AckChip` two columns to the left is the
          badge, and it is already wearing that colour on this very row. The
          button says the same thing by going from `ink-muted` to full ink, which
          is the ghost variant's own hover tone held on. */}
      <Button
        variant="ghost"
        size="sm"
        class={cn("size-6 shrink-0 px-0", acked() && "text-ink")}
        disabled={props.disabled || receipt.isPending}
        aria-busy={receipt.isPending ? "true" : undefined}
        aria-label={label()}
        title={title()}
        onClick={() => receipt.mutate()}
      >
        <StateGlyph state="acked" tone="inherit" />
      </Button>

      {/* A failure with no dialog to land in still has to be *said*: silence
          about a refusal is the one thing oto is not allowed to do. */}
      <Show when={failure()}>
        {(message) => (
          <span role="alert" class="sr-only-focusable">
            {`Could not ${verb()} ${props.item.alert.alertname}: ${message()}`}
          </span>
        )}
      </Show>
    </>
  );
};
