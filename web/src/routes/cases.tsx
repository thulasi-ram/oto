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
 * ⛔ A CASE NEVER SPANS TWO ALERTS. It is one firing episode of ONE alert, and
 * it is not a correlation and not an incident: oto records signals.
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
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
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
  CaseStateChip,
  SeverityEnso,
  SeverityMark,
  StateGlyph,
  severityLevel,
  type CaseState,
} from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { FilterRow } from "~/components/ui/FilterRow";
import {
  CheckList,
  ChoiceList,
  FilterMenu,
  MenuSection,
  summarise,
} from "~/components/ui/FilterMenu";
import {
  ErrorState,
  PageEmptyState,
  TableSkeleton,
} from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { count as fmtCount, idempotencyKey } from "~/lib/format";
import {
  createKeysetFeed,
  keepPrevious,
  type KeysetFeed,
} from "~/lib/keysetFeed";

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

/**
 * What weight of the §0.3 ensō a severity inks — the same reading
 * `AlertFilterToolbar` does, and for the same reason: the glyph on the trigger has
 * to be the glyph on the rows below it or it is teaching a second alphabet.
 *
 * Anything outside the common three ranks 0 and draws the FAINT ring rather than
 * guessing at an order nobody stated. `severity` is a free vocabulary.
 */
const severityRank = (severity: string): number => severityLevel(severity);

/** The worst severity in a set, for the one glyph a 32 px trigger can hold. */
const worstSeverityRank = (severities: readonly string[]): number =>
  severities.reduce((worst, s) => Math.max(worst, severityRank(s)), 0);

const ACK_LABEL: Record<AckState, string> = {
  unacked: "Unacknowledged",
  acked: "Acknowledged",
};

const ACK_TITLE: Record<AckState, string> = {
  unacked:
    "Firings nobody has recorded seeing yet. This is the queue, and paired with Open it is the shape `case_ack_idx` exists for.",
  acked:
    "Firings somebody has signed for. A receipt belongs to ONE firing and clears itself when the next one opens.",
};

/* -------------------------------------------------------------------------- */
/* Grouping                                                                   */
/* -------------------------------------------------------------------------- */

/**
 * How the loaded page is laid out. **Not a filter, and not on the wire.**
 *
 * ⛔ `GET /api/v1/cases` HAS NO `group_by` AND MUST NOT GROW ONE HERE. A grouped
 * server response would need a second ordering to page over, and this list has
 * exactly one indexed total order — `-started_at` with the id as tiebreak — which
 * is the whole reason a keyset cursor is sound (see the file header). So grouping
 * is a **presentation of the rows already loaded** and nothing else: it re-orders
 * nothing, it requests nothing, and it never claims to have seen a firing that is
 * still behind the cursor.
 *
 * ⭐ WHICH IS THE ONE THING IT HAS TO SAY OUT LOUD. "Four earlier firings" under a
 * grouped row means *four in the pages you have loaded* — an alert that fired
 * forty times has thirty-six more waiting on "Load more". The strip says so while
 * grouping is on, because a count that looks total and is not is worse than no
 * count.
 */
type GroupMode = "none" | "alert";

const GROUP_MODES: readonly GroupMode[] = ["none", "alert"];

/**
 * URL parameters this screen reads and the REQUEST does not — see `fingerprint`.
 * Every name here has to be absent from `query()` below, and that is the whole
 * of what earns a line in this set.
 */
const DECORATIVE_PARAMS: ReadonlySet<string> = new Set(["group"]);

const GROUP_LABEL: Record<GroupMode, string> = {
  none: "Flat, newest first",
  alert: "By alert",
};

const GROUP_TITLE: Record<GroupMode, string> = {
  none: "Every firing as its own row, newest first — the order the cursor pages in.",
  alert:
    "One row per alert, carrying its most recent firing, with earlier firings of the same alert folded underneath it. Only the pages already loaded are grouped.",
};

/**
 * One alert's firings, out of the rows currently loaded.
 *
 * `latest` is the newest episode of that identity in the loaded set and is the
 * row that carries the alert's name; `earlier` are the older ones, newest first,
 * and they are the same `CaseRow` drawn indented. The list arrives ordered by
 * `-started_at`, so the first case seen for an alert IS its latest and no sort is
 * needed to find it.
 */
interface AlertGroup {
  readonly alertId: string;
  readonly latest: CaseListItem;
  readonly earlier: readonly CaseListItem[];
}

function groupByAlert(rows: readonly CaseListItem[]): readonly AlertGroup[] {
  const order: string[] = [];
  const byAlert = new Map<string, CaseListItem[]>();
  for (const c of rows) {
    const key = c.alert.id;
    const bucket = byAlert.get(key);
    if (bucket === undefined) {
      order.push(key);
      byAlert.set(key, [c]);
    } else {
      bucket.push(c);
    }
  }
  return order.map((id) => {
    const [latest, ...earlier] = byAlert.get(id) as CaseListItem[];
    return { alertId: id, latest: latest as CaseListItem, earlier };
  });
}

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

  /**
   * The acknowledgement states in force, as a SET.
   *
   * ⭐ IT IS A SET BECAUSE THE ENDPOINT'S IS. `ack` is a comma-separated list on
   * the wire, and this was a three-option single-select whose first option was
   * the word `Any` — an invented third value standing in for "neither of the two
   * that exist". Naming both is the same as naming none, so the empty set is what
   * `Any` always meant, and now it is spelled that way in one control instead of
   * being a magic option inside it.
   */
  const acks = createMemo<readonly AckState[]>(() =>
    csv("ack").filter((s): s is AckState =>
      (ALL_ACK as readonly string[]).includes(s),
    ),
  );

  const severities = createMemo<readonly string[]>(() => csv("severity"));

  /**
   * Severities in the URL that are not one of the common three.
   *
   * A deployment spelling it `sev1` sees its own word back in the menu rather
   * than having it silently dropped — but no invented rank: the ruler stays empty
   * for a severity nobody told us how to order.
   */
  const customSeverities = createMemo<readonly string[]>(() =>
    severities().filter(
      (s) => !(COMMON_SEVERITIES as readonly string[]).includes(s),
    ),
  );

  /** How the loaded rows are laid out. Junk falls back to the flat list. */
  const group = createMemo<GroupMode>(() =>
    (GROUP_MODES as readonly string[]).includes(str("group"))
      ? (str("group") as GroupMode)
      : "none",
  );

  /**
   * The filters this bar has no control for, kept because a URL is how one
   * screen hands work to another: a drill links here with `synthetic=true`, and
   * dropping it on arrival would make the link a lie. Each is shown, and each
   * can be taken off.
   */
  const narrowings = createMemo<readonly Narrowing[]>(() => {
    const out: Narrowing[] = [];
    const carry: readonly (readonly [string, string])[] = [
      ["cluster", "cluster"],
      ["namespace", "namespace"],
      ["alertname", "alert"],
      ["synthetic", "synthetic"],
      ["since", "since"],
    ];
    for (const [param, label] of carry) {
      const value = str(param);
      if (value !== "") {
        out.push({ param, label, value });
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
   *
   * ⛔ MINUS THE PARAMETERS THAT NEVER REACH THE WIRE, AND `group` IS THE FIRST
   * OF THEM. The fingerprint's job is to describe the keyset the server minted a
   * cursor under; a parameter the request does not carry cannot have changed that
   * keyset, and including it would throw away every loaded page the moment
   * somebody folded the list — which is precisely the set grouping is a view of.
   * A parameter belongs in `DECORATIVE_PARAMS` only when `query()` below provably
   * ignores it.
   */
  const fingerprint = createMemo(() => {
    const sp = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (DECORATIVE_PARAMS.has(k)) continue;
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

    if (acks().length > 0) q["ack"] = [...acks()];
    if (severities().length > 0) q["severity"] = [...severities()];

    for (const key of ["cluster", "namespace", "alertname"] as const) {
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
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listCases(query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const rows = feed.rows;

  /** The loaded rows folded by identity, when that is what the operator asked for. */
  const groups = createMemo<readonly AlertGroup[]>(() =>
    group() === "alert" ? groupByAlert(rows()) : [],
  );

  const status = (): string => {
    const n = rows().length;
    if (cases.isPending && n === 0) return "Loading…";
    const cases_ = `${fmtCount(n)}${feed.hasMore() ? "+" : ""} case${n === 1 ? "" : "s"}`;
    if (group() !== "alert") return cases_;
    const g = groups().length;
    return `${cases_} across ${fmtCount(g)} alert${g === 1 ? "" : "s"}`;
  };

  /**
   * Grouping by alert while only OPEN episodes are loaded folds nothing, and it
   * is worth saying rather than leaving the operator to conclude the feature is
   * broken: a case is one contiguous firing and never two at once, so an alert
   * has at most ONE open episode. The earlier firings the fold exists to show are
   * all `closed`.
   *
   * ⛔ AND THE SCREEN DOES NOT QUIETLY ADD THE FILTER ITSELF. Changing what is on
   * the wire as a side effect of a layout choice is how an operator ends up
   * looking at rows they did not ask for; the strip offers the press instead.
   */
  const groupingIsInert = (): boolean =>
    group() === "alert" && states().length === 1 && states()[0] === "open";

  /** Nothing narrowed at all — the default queue, which has its own sentence. */
  const isDefaultView = (): boolean =>
    states().length === 1 &&
    states()[0] === "open" &&
    acks().length === 0 &&
    severities().length === 0 &&
    narrowings().length === 0;

  return (
    // ⛔ THE `min-h-0` / `overflow-hidden` CHAIN IS LOAD-BEARING, the same one
    // `/alerts` depends on: every ancestor from `AppShell`'s `h-screen` down to
    // here refuses to grow with its content, which is what gives the list its
    // own bounded scroller instead of scrolling the document.
    <div class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      {/* ⭐ THREE AXES AND A LAYOUT, EACH BEHIND ITS OWN DROPDOWN, EACH SAYING ITS
          VALUE AT REST. This row used to be three `ToggleGroup`s — six chips and
          three selects spread across the full width — and the six chips read as a
          TAB STRIP: a row of pills with one lit, which says "you are in one of
          these" when the fact is "the list is narrowed to these". The two
          sentences disagree the moment two chips are on, which for an operator
          asking for open AND ended episodes is the normal case. `FilterMenu`'s header argues the trade in full.

          ⛔ AND STILL NO SORT CONTROL. The file header spends a paragraph on why:
          `GET /api/v1/cases` takes no `sort`, because a keyset cursor is only
          sound over an indexed total order and this list has exactly one. The
          Group menu below is a LAYOUT, not an order — it re-folds the rows the
          cursor already handed over and leaves their sequence alone. */}
      <div class="shrink-0">
        {/* ⛔ NO BOTTOM BORDER. `FilterRow`'s `standalone` default draws one —
            right, `/alerts`'s toolbar sits flush on the page background with
            no dividing line, and the two screens have to read as one product. */}
        <FilterRow gap="tight" class="border-b-0">
          <FilterMenu
            label="Ack"
            value={summarise(acks().map((a) => ACK_LABEL[a]))}
            title="A receipt belongs to ONE firing, and this list is the only one in the product where the row IS the firing."
          >
            <CheckList<AckState>
              legend="Acknowledgement"
              options={ALL_ACK.map((a) => ({
                value: a,
                label: ACK_LABEL[a],
                icon:
                  a === "acked" ? (
                    <StateGlyph state="acked" tone="inherit" class="size-3.5" />
                  ) : undefined,
                title: ACK_TITLE[a],
              }))}
              value={acks()}
              onChange={(next) =>
                setParams({ ack: next.length > 0 ? next.join(",") : null })
              }
              allLabel="Either way"
              allTitle="Both acknowledged and unacknowledged firings — the same thing as omitting the filter."
            />
          </FilterMenu>

          {/* ⭐ ONE CONTROL FOR THE ONE AXIS AN EPISODE HAS. Turning both on is
              how you ask for everything in retention; turning both off is not a
              third answer, so it falls back to the queue rather than to a list
              that could not contain anything — which is why this axis's `All` row
              EMITS both values instead of the empty set. It is spelled `All` like
              every other axis's: `Open + Ended` was the mechanism written out
              where a word was wanted, and it made this one menu read as the
              exception in a band of four. */}
          <FilterMenu
            label="Episode"
            value={summarise(states().map((s) => CASE_STATE_LABEL[s]))}
            title="`alert_cases.state` — the one axis an episode has. `firing` and `suppressed` are facts about the ALERT and are refused here with a 400."
          >
            <CheckList<CaseState>
              legend="Episode"
              options={CASE_STATES.map((s) => ({
                value: s,
                label: CASE_STATE_LABEL[s],
                title:
                  s === "open"
                    ? "Episodes that have not ended. This is the queue, and it is the shape of this list the ack index is built for."
                    : "Episodes that have ended, whether the upstream resolved them or oto stopped hearing about them. A case is terminal — a re-fire is the next episode, not this one again.",
              }))}
              value={states()}
              // ⛔ CLEARING THIS AXIS IS NOT "EVERYTHING", WHICH IS WHY `allValue`
              // NAMES BOTH VALUES. An absent `?state=` reads back as the QUEUE
              // (see `DEFAULT_STATES`), so emitting the empty set here would turn
              // a press on "All" into a press on "Open".
              allValue={CASE_STATES}
              onChange={(next) =>
                setParams({ state: next.length > 0 ? next.join(",") : null })
              }
              allLabel="All"
              allTitle="Everything in retention. The queue is the default because a list of every episode oto has ever opened answers a question nobody asked."
            />
          </FilterMenu>

          <FilterMenu
            label="Severity"
            value={summarise(severities())}
            leading={
              <SeverityEnso
                level={worstSeverityRank(severities())}
                class="size-3"
              />
            }
            title="Matched against the alert's promoted `severity` label. A free vocabulary, so the three offered here are a convenience and never the closed set."
          >
            <CheckList<string>
              legend="Severity"
              options={[...COMMON_SEVERITIES, ...customSeverities()].map(
                (s) => ({
                  value: s,
                  label: s,
                  icon: <SeverityEnso level={severityRank(s)} />,
                }),
              )}
              value={severities()}
              onChange={(next) =>
                setParams({ severity: next.length > 0 ? next.join(",") : null })
              }
              allLabel="Any severity"
            />
          </FilterMenu>

          {/* The layout, and it is deliberately the last control in the row: it
              changes nothing about WHICH firings are here, only how the ones
              already loaded are stacked. */}
          <FilterMenu
            label="Group"
            value={group() === "none" ? undefined : GROUP_LABEL.alert}
            title={GROUP_TITLE[group()]}
            width="w-80"
          >
            <MenuSection label="Layout">
              <ChoiceList<GroupMode>
                legend="Group cases by"
                value={group()}
                onChange={(next) =>
                  setParams({ group: next === "none" ? null : next })
                }
                options={GROUP_MODES.map((g) => ({
                  value: g,
                  label: GROUP_LABEL[g],
                  title: GROUP_TITLE[g],
                }))}
              />
              {/* ⛔ THE HONEST SENTENCE, AND IT IS NOT OPTIONAL. Grouping folds
                  the LOADED pages, because the endpoint has no grouped ordering
                  to page over. A count that reads as total and is not is worse
                  than no count at all. */}
              <p class="px-xs pt-2xs text-meta leading-snug text-ink-subtle">
                Folds the pages already loaded. An alert that fired forty times
                shows the firings oto has handed over so far, not all forty —
                “Load more” brings the rest.
              </p>
            </MenuSection>
          </FilterMenu>
        </FilterRow>
      </div>

      {/* ⛔ A STRIP, NOT A SECOND CHROME BAR — `/alerts` argues this at length,
          and the two screens have to read as one product. The polite region is
          the strip itself and is mounted unconditionally, so a count change is a
          mutation inside a region that already existed. */}
      <header
        id="case-status"
        class="flex h-9 shrink-0 items-center gap-md px-md"
        aria-live="polite"
      >
        <span class="text-body tabular-nums text-ink-muted">{status()}</span>

        {/* Grouping by alert with only the queue loaded folds nothing, because an
            alert has at most one OPEN episode. The strip says so and offers the
            press, rather than the Group menu quietly widening what is on the
            wire. */}
        <Show when={groupingIsInert()}>
          <span class="flex items-center gap-2xs text-meta text-ink-subtle">
            Only open episodes are loaded, and an alert has at most one.
            <Button
              variant="link"
              size="sm"
              class="h-auto p-0 text-meta"
              onClick={() => setParams({ state: CASE_STATES.join(",") })}
              title="Include ended episodes, so earlier firings of the same alert have something to fold under."
            >
              Include ended
            </Button>
          </span>
        </Show>

        {/* Every narrowing the bar has no control for, said out loud with its
            way off. A filter that only exists in a query string is a filter an
            operator cannot tell is on. */}
        <For each={narrowings()}>
          {(n) => (
            <button
              type="button"
              class="inline-flex items-center gap-1 rounded-chip bg-raised px-sm py-2xs text-meta text-ink hover:bg-sunken"
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
          <ErrorState
            error={cases.error}
            onRetry={() => void cases.refetch()}
          />
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
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => navigate("/cases")}
                  >
                    Clear filters
                  </Button>
                }
              />
            </Match>
          </Switch>
        </Match>

        <Match when={true}>
          {/* The footer is a sibling of the scroller, not a last row inside it —
              the same `shrink-0` split `case-status` uses at the top. A pager
              that scrolls away with the last page is a control the operator has
              to scroll to find right when they want to reach for it. */}
          <>
            <div class="min-h-0 flex-1 overflow-auto">
              <ul>
                <Show
                  when={group() === "alert"}
                  fallback={
                    <For each={rows()}>{(c) => <CaseRow item={c} />}</For>
                  }
                >
                  <For each={groups()}>
                    {(g) => <AlertGroupRows group={g} />}
                  </For>
                </Show>
              </ul>
            </div>

            <div class="flex shrink-0 items-center justify-center gap-3 border-t border-line bg-surface px-3 py-4">
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
                  {fmtCount(rows().length)} loaded across {feed.pageCount()}{" "}
                  page
                  {feed.pageCount() === 1 ? "" : "s"}
                </span>
              </Show>
            </div>
          </>
        </Match>
      </Switch>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* One alert's firings, folded                                                */
/* -------------------------------------------------------------------------- */

/**
 * One alert's most recent loaded firing, with its earlier ones folded under it.
 *
 * ⭐ THE FOLD IS CLOSED BY DEFAULT AND THE COUNT IS ON THE HANDLE. An alert that
 * has fired forty times is one line until asked, which is the whole point of
 * grouping; an alert that has fired once has no handle at all, so the grouped
 * list of a healthy estate looks exactly like the flat one rather than growing a
 * column of empty disclosure triangles.
 *
 * ⛔ IT COUNTS WHAT IS LOADED AND SAYS SO IN THE HANDLE'S TOOLTIP. The endpoint
 * has no grouped ordering, so this fold is over the pages the cursor has already
 * handed over — see `GroupMode`. "3 earlier firings" is therefore a floor, never
 * a total, and the one place that could be mistaken for a total is here.
 *
 * ⛔ AND IT IS A `<ul>` INSIDE THE ROW'S `<li>`, not a sibling list. A nested list
 * is the markup for "these belong to that"; two flat lists with an indent are the
 * markup for "these are unrelated and one of them has a margin", and a screen
 * reader reads out exactly the difference.
 */
const AlertGroupRows = (props: { readonly group: AlertGroup }) => {
  const [open, setOpen] = createSignal(false);
  const earlier = (): readonly CaseListItem[] => props.group.earlier;
  const n = (): number => earlier().length;

  return (
    <>
      <CaseRow item={props.group.latest} />

      <Show when={n() > 0}>
        <li class="bg-surface">
          <button
            type="button"
            class={
              "flex w-full items-center gap-2xs py-1 pl-6 pr-3 text-meta text-ink-subtle " +
              "transition-colors duration-100 hover:bg-raised/60 hover:text-ink"
            }
            aria-expanded={open()}
            title={
              `Earlier firings of ${props.group.latest.alert.alertname} among the cases loaded so far. ` +
              `Grouping folds the pages already fetched, so there may be more behind “Load more”.`
            }
            onClick={() => setOpen(!open())}
          >
            <svg
              viewBox="0 0 12 12"
              class={cn(
                "size-3 shrink-0 transition-transform duration-100",
                open() ? "rotate-90" : "",
              )}
              aria-hidden="true"
            >
              <path
                d="M4.4 2.6 7.8 6l-3.4 3.4"
                fill="none"
                stroke="currentColor"
                stroke-width="1.4"
                stroke-linecap="round"
                stroke-linejoin="round"
              />
            </svg>
            <span class="tabular-nums">
              {fmtCount(n())} earlier firing{n() === 1 ? "" : "s"} loaded
            </span>
          </button>

          <Show when={open()}>
            <ul>
              <For each={earlier()}>{(c) => <CaseRow item={c} nested />}</For>
            </ul>
          </Show>
        </li>
      </Show>
    </>
  );
};

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
 * ⛔⛔ AND THE ALERT'S `state` IS NOT ONE OF THEM ANY MORE. This row used to wear
 * TWO badges — `StateChip` for what the identity is doing right now, `CaseStateChip`
 * for whether this episode is the one still running — and the pair was defensible
 * in prose and unreadable on screen. `Firing · Ended · resolved` on one row is
 * three words about two different subjects in one wrap group, and the loudest of
 * them (a saturated `Firing`) is about the thing this screen is NOT listing: a
 * case list lists episodes, and the reader has to look at the second chip to
 * learn which one they are looking at. Worse, it was the only Tier-B hue on the
 * screen, so the eye went to it first, every row, correctly reading it as the
 * row's status and wrongly believing that status belonged to the row.
 *
 * `firing` lives where its subject is: `/alerts` and `/alerts/:id`, where the row
 * IS the identity — and `/cases/:id`, where "The alert" panel states it under the
 * heading `state now`, next to a link out to the identity. Nothing was lost; one
 * fact stopped being said in the one place its subject was absent.
 *
 * ⛔ NOTHING HERE SPENDS A STATE HUE, AND THAT IS §M.7 RATHER THAN TASTE. §M.2's
 * scarcity argument allows a row status only on the surfaces `chromeExceptions`
 * names, and this file is not one of them.
 *
 * ⛔ AND THE 3 px RAIL IS GONE TOO. It was a neutral bar — `bg-ink-muted` when
 * open, `bg-line-strong` when ended — down the left edge of every row, and the
 * only fact it carried was one the row already stated in words two columns to the
 * right. It is the residue of a review: the rail once carried the STATE hue, that
 * was correctly refused here, and it survived as a decolourised stub encoding a
 * boolean nobody needed a colour swatch for. An unlabelled tint that duplicates
 * an adjacent label is not a status — it is a thing to decode, and there was
 * nothing behind it. The ended row's own wash (below) is now the whole of that
 * channel, and it goes across the row rather than beside it.
 */
const CaseRow = (props: {
  readonly item: CaseListItem;
  /** Drawn as an earlier firing folded under its alert's most recent one. */
  readonly nested?: boolean;
}) => {
  const c = (): CaseListItem => props.item;
  /** The episode's own state is authoritative; `ended_at` only agrees with it. */
  const open = (): boolean => c().state === "open";

  return (
    <li
      class={cn(
        // ⭐ THE ENDED ROW IS WASHED, AND IT IS THE ONE THING ON THIS SCREEN THAT
        // ENCODES `state` WITHOUT A WORD — which is allowed because the word is
        // still there. An episode that has ended is history: it cannot be
        // acknowledged, it will never change again, and in a list holding both it
        // is the row you are scanning PAST. Tier A only — the sunken surface and
        // one tier off the title's ink, no hue, and both halves measured pairs.
        //
        // ⛔ NO `border-b` BETWEEN ROWS. A hairline under every row is a rule
        // for each one of them; the wash above already tells the ended rows
        // apart, and the padding below gives the open ones room to breathe
        // instead of a line to lean on. Space is the separator here, not ink.
        open() ? "bg-surface" : "bg-sunken",
      )}
    >
      <div
        class={cn(
          "flex items-center gap-3 py-3 pr-3 hover:bg-raised/60",
          // The fold's indent, and the guide that makes it read as a fold rather
          // than as a row that lost its left margin.
          props.nested === true
            ? "border-l-2 border-line-strong pl-6 ml-3"
            : "pl-3",
        )}
      >
        <A
          href={`/cases/${c().id}`}
          class="flex min-w-0 flex-1 items-start gap-3"
        >
          {/* The firing number is the fastest thing to scan a case list by —
              faster than the alertname or its labels — so it leads every row,
              folded or not, in its own column ahead of the severity glyph. */}
          <span
            class={cn(
              "mt-0.5 w-9 shrink-0 text-right font-mono text-title font-semibold tabular-nums",
              open() ? "text-ink" : "text-ink-muted",
            )}
            title="Which firing of this alert this is, counted since oto first saw the identity. A re-fire opens the next one rather than reopening this one."
          >
            #{c().seq}
          </span>
          <SeverityMark severity={c().alert.severity} class="mt-0.5" />

          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              {/* Under a fold the identity is already named by the row above —
                  the alertname repeated five times down a stack of earlier
                  firings is noise — so a nested row's title line says nothing
                  here; its number is already in the lead column, to the left. */}
              <Show when={props.nested !== true}>
                <span
                  class={cn(
                    "min-w-0 truncate text-title font-medium",
                    open() ? "text-ink" : "text-ink-muted",
                  )}
                >
                  {c().alert.alertname}
                </span>
              </Show>
              <CaseStateChip
                state={c().state}
                resolveReason={c().resolve_reason}
                size="sm"
              />
              <AckChip ackState={c().ack_state} />
            </div>

            <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-body text-ink-muted">
              <span
                title={
                  open()
                    ? "How long this firing has been running. oto times the signal, never anyone's response."
                    : "How long this firing ran. oto times the signal, never anyone's response."
                }
              >
                <Elapsed from={c().started_at} to={c().ended_at ?? null} />{" "}
                firing
              </span>
              {/* The identity's own labels belong to the row that names it. An
                  earlier firing shares them by construction, so repeating
                  `namespace`/`cluster` under a fold is the same string five
                  times down one column. */}
              <Show when={props.nested !== true}>
                <Show when={c().alert.namespace}>
                  {(ns) => <span class="font-mono">namespace {ns()}</span>}
                </Show>
                <span class="font-mono">cluster {c().alert.cluster_key}</span>
              </Show>
              <Show when={c().acked_by_label}>
                {(who) => <span>seen by {who()}</span>}
              </Show>
            </div>
          </div>

          <span class="shrink-0 text-right text-body text-ink-subtle">
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
const RowAck = (props: {
  readonly item: CaseListItem;
  readonly disabled: boolean;
}) => {
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
    receipt.error === null
      ? null
      : ((receipt.error as Error).message ?? "The request failed.");

  /** The verb this press would perform, for every sentence about it. */
  const verb = (): string =>
    acked() ? "withdraw the acknowledgement of" : "acknowledge";

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
