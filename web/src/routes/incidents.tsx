/**
 * `/incidents` — every Incident in the org, newest first.
 *
 * An **Incident** is a set of one or more Cases drawn together as one story
 * (ADR 0052). An Alert has Cases; an Incident spans Cases. A Correlator an
 * operator wrote draws one, or a human does — from a Case's own screen — and the
 * row says which.
 *
 * ⛔ THERE IS NOTHING TO DO ON THIS LIST, AND THAT IS THE SHAPE OF THE OBJECT. It
 * is not a queue: no Incident is acknowledged, assigned or worked through here,
 * because its RESPONSE — the status, the lead, the comms — lives in the incident
 * tool it is declared to. What oto holds is the grouping, and this screen is a
 * reading of it. Each row links to the Incident, where membership is edited.
 *
 * ⭐⭐ THE STATE ON EACH ROW IS DERIVED, AND THE ROW SHOWS ITS WORKING. `active`
 * while any member Case is open, `quiet` otherwise — read off the Cases by the
 * server on every request, never stored and never set by a hand. The open count
 * sits beside the word so the reader can see the rule hold rather than take the
 * chip's word for it; the contract serves the two together for exactly that.
 *
 * ⭐ THE ROW LEADS WITH `number`, for the reason `/cases` does: it is the
 * per-org name somebody reads out, and it is what `/incidents/:number` addresses.
 *
 * ⭐ AN EMPTY INCIDENT IS HIDDEN, NOT GONE (owner ruling 2026-10-04). One whose
 * every Case was removed or moved away is kept as a record and still opens at
 * `/incidents/:number`, but it is not a story anybody is following, so the server
 * leaves it off the list unless `include_empty=true` asks. "Show empty Incidents"
 * is that ask, and nothing else: it filters which rows are listed and says nothing
 * about any Incident's state.
 *
 * Pagination is keyset and append-only, the same bargain `/cases` makes.
 */
import { createSignal, For, Match, Show, Switch } from "solid-js";
import { A } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { listIncidents } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Incident, IncidentListQuery } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { Checkbox } from "~/components/ui/Checkbox";
import { ErrorState, PageEmptyState, TableSkeleton } from "~/components/ui/states";
import { IncidentStateChip, describeAttribution } from "~/features/incidents/parts";
import { cn } from "~/lib/cn";
import { count as fmtCount } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";

const PAGE_SIZE = 50;

export default function IncidentsRoute() {
  const [showEmpty, setShowEmpty] = createSignal(false);

  // The annotation cuts the inference loop, exactly as on `/cases`: the feed
  // reads the query's envelope and the query's key carries the feed's cursor.
  //
  // ⚠️ THE TOGGLE IS A FILTER AXIS, so it is the feed's fingerprint: the server
  // binds a cursor to `include_empty`, and a position held across the toggle would
  // be answered with `cursor_filter_mismatch`.
  const feed: KeysetFeed<Incident> = createKeysetFeed({
    envelope: () => incidents.data,
    isPlaceholder: () => incidents.isPlaceholderData,
    keyOf: (i) => i.id,
    fingerprint: () => (showEmpty() ? "include_empty" : ""),
  });

  const query = (): IncidentListQuery => {
    const q: Record<string, unknown> = { limit: PAGE_SIZE };
    // Sent only when asked: the default list IS the one without empty Incidents,
    // and an explicit `false` would only be a second spelling of it.
    if (showEmpty()) q["include_empty"] = true;
    if (feed.cursor() !== null) q["cursor"] = feed.cursor();
    return q as IncidentListQuery;
  };

  const incidents = useQuery(() => ({
    queryKey: qk.incidents.list(query()),
    queryFn: ({ signal }: { signal: AbortSignal }) => listIncidents(query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const rows = feed.rows;

  const status = (): string => {
    const n = rows().length;
    if (incidents.isPending && n === 0) return "Loading…";
    return `${fmtCount(n)}${feed.hasMore() ? "+" : ""} Incident${n === 1 ? "" : "s"}`;
  };

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <header
        id="incident-status"
        class="flex h-9 shrink-0 items-center gap-md px-md"
        aria-live="polite"
      >
        <span class="text-body tabular-nums text-ink-muted">{status()}</span>
        <span class="min-w-0 flex-1 truncate text-meta text-ink-subtle">
          Each one is a set of Cases drawn together as one story. Its state is read off its Cases.
        </span>
        <span class="flex shrink-0 items-center gap-xs">
          <Checkbox
            id="incidents-show-empty"
            checked={showEmpty()}
            onChange={(checked: boolean) => setShowEmpty(checked)}
          />
          <label
            for="incidents-show-empty-input"
            class="cursor-pointer select-none text-meta text-ink-muted"
            title="An Incident whose every Case was removed or moved away is kept as a record and still opens by its number. It is left off this list unless you ask for it."
          >
            Show empty Incidents
          </label>
        </span>
      </header>

      <Switch>
        <Match when={incidents.isError}>
          <ErrorState error={incidents.error} onRetry={() => void incidents.refetch()} />
        </Match>

        <Match when={incidents.isPending && rows().length === 0}>
          <div class="flex-1 overflow-hidden">
            <TableSkeleton rows={10} cols={4} />
          </div>
        </Match>

        <Match when={rows().length === 0}>
          {/* ⭐ AN EMPTY LIST IS AN ANSWER, AND IT SAYS WHERE ONE COMES FROM. No
              Incident exists until a Correlator or a person draws one; telling the
              operator that the Cases screen is where a person does it is the whole
              of the onboarding this needs. */}
          {/* With empty Incidents hidden, "none have been drawn" could be false:
              some may have been drawn and emptied. The title says only what the
              list knows, and the body says where the rest are. */}
          <PageEmptyState
            motif="kumo"
            title={showEmpty() ? "No Incidents have been drawn." : "No Incidents with Cases in them."}
            body={
              "An Incident is a set of Cases drawn together as one story. A Correlator draws one as Cases open, or a person draws one by selecting Cases on the Cases screen." +
              (showEmpty()
                ? ""
                : " An Incident whose Cases were all removed or moved away is hidden; Show empty Incidents lists it.")
            }
          />
        </Match>

        <Match when={true}>
          <>
            <div class="min-h-0 flex-1 overflow-auto">
              <ul>
                <For each={rows()}>{(i) => <IncidentRow item={i} />}</For>
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
                  busy={incidents.isFetching}
                  onClick={feed.loadMore}
                >
                  Load {fmtCount(PAGE_SIZE)} more
                </Button>
              </Show>
            </div>
          </>
        </Match>
      </Switch>
    </div>
  );
}

/**
 * One Incident.
 *
 * The quiet row is washed the way an ended Case is on `/cases`, and for the same
 * reason — it is the row being scanned past — while the word stays on the chip,
 * so the wash is never the only thing carrying the state.
 */
const IncidentRow = (props: { readonly item: Incident }) => {
  const i = (): Incident => props.item;
  const active = (): boolean => i().state === "active";

  return (
    <li class={active() ? "bg-surface" : "bg-sunken"}>
      <A
        href={`/incidents/${i().number}`}
        class="flex items-start gap-3 py-3 pl-3 pr-3 hover:bg-raised/60"
      >
        <span
          class={cn(
            "mt-0.5 w-14 shrink-0 text-right font-mono text-title font-semibold tabular-nums",
            active() ? "text-ink" : "text-ink-muted",
          )}
          title="This Incident's number. It counts every Incident in this organisation, so it is unique here and is what to quote."
        >
          #{i().number}
        </span>

        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <span
              class={cn(
                "min-w-0 truncate text-title font-medium",
                active() ? "text-ink" : "text-ink-muted",
              )}
            >
              {/* The alertnames are what the story is ABOUT, and the contract
                  serves them per row so the list needs no request per Incident.
                  An Incident whose members were all removed has none — and says
                  so rather than rendering an empty title. */}
              {i().alertnames.length > 0 ? i().alertnames.join(", ") : "No Cases left in it"}
            </span>
            <IncidentStateChip state={i().state} size="sm" />
          </div>
          <div class="mt-0.5 flex flex-wrap items-center gap-x-3 text-meta text-ink-subtle">
            <span class="tabular-nums">
              {fmtCount(i().member_count)} Case{i().member_count === 1 ? "" : "s"},{" "}
              {fmtCount(i().open_member_count)} open
            </span>
            <span>
              drawn by {describeAttribution(i().drawn_by)}{" "}
              <RelativeTime value={i().drawn_at} label="Drawn" /> ago
            </span>
          </div>
        </div>
      </A>
    </li>
  );
};
