/**
 * `/groups` — notification-group generations.
 *
 * A group is **one generation of one Alertmanager notification group**, derived
 * from `(source, receiver, groupLabels)`. It is the unit humans actually respond
 * to: forty pods crash-looping is one thing happening, not forty. It owns
 * exactly one chat thread, which is why a new generation is what produces a new
 * root message rather than a reply.
 *
 * This is deliberately *not* "the alert list, grouped" — that is
 * `GET /api/v1/alerts/rollups`, behind the grouping control on `/alerts`. The
 * two are separate endpoints because they are separate concepts: a roll-up
 * bucket is a view computed for one query, with no row, no generation and no
 * thread. Conflating them would suggest oto invents groupings, and it does not:
 * grouping is Alertmanager's decision, mirrored.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { listAlertGroups } from "~/api/endpoints";
import { GroupStateSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { Group, GroupListQuery, GroupState } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { SeverityMark, StormChip } from "~/components/StateChip";
import { Button, Chip, Input, Select, ToggleGroup, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "~/components/ui/states";
import { count as fmtCount } from "~/lib/format";

const PAGE_SIZE = 50;

/**
 * The two group states, the contract's list rather than a copy of it. This only
 * supplies the capital letter — a state added server-side is a compile error
 * here rather than a toggle nobody can reach.
 */
const GROUP_STATES: readonly GroupState[] = GroupStateSchema.options;
const STATE_LABEL: Record<GroupState, string> = { open: "Open", closed: "Closed" };

export default function GroupsRoute() {
  const navigate = useNavigate();
  const [params] = useSearchParams();

  const states = createMemo<readonly GroupState[]>(() => {
    const raw = typeof params["state"] === "string" ? params["state"] : "";
    return raw
      .split(",")
      .filter((s): s is GroupState => (GROUP_STATES as readonly string[]).includes(s));
  });

  const search = (): string => (typeof params["q"] === "string" ? params["q"] : "");
  const storm = (): boolean | null => {
    const raw = params["storm"];
    return raw === "true" ? true : raw === "false" ? false : null;
  };
  const ack = (): "acked" | "unacked" | null => {
    const raw = params["ack"];
    return raw === "acked" || raw === "unacked" ? raw : null;
  };

  const setParam = (key: string, value: string | null): void => {
    const next = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (typeof v === "string" && v !== "") next.set(k, v);
    }
    if (value === null || value === "") next.delete(key);
    else next.set(key, value);
    const s = next.toString();
    navigate(`/groups${s === "" ? "" : `?${s}`}`, { scroll: false });
  };

  const [cursor, setCursor] = createSignal<string | null>(null);
  const [rows, setRows] = createSignal<readonly Group[]>([]);

  const query = createMemo<GroupListQuery>(() => {
    const q: Record<string, unknown> = { limit: PAGE_SIZE, sort: "-last_activity_at" };
    if (states().length > 0) q["state"] = [...states()];
    if (search() !== "") q["q"] = search();
    if (storm() !== null) q["storm"] = storm();
    if (ack() !== null) q["ack"] = ack();
    if (cursor() !== null) q["cursor"] = cursor();
    return q as GroupListQuery;
  });

  const groups = useQuery(() => ({
    queryKey: qk.groups.list(query()),
    queryFn: ({ signal }: { signal: AbortSignal }) => listAlertGroups(query(), { signal }),
  }));

  const all = createMemo<readonly Group[]>(() => {
    const page = groups.data?.data ?? [];
    if (cursor() === null) return page;
    const seen = new Set(rows().map((g) => g.id));
    return [...rows(), ...page.filter((g) => !seen.has(g.id))];
  });

  const loadMore = (): void => {
    setRows(all());
    const next = groups.data?.page.next_cursor;
    if (typeof next === "string" && next !== "") setCursor(next);
  };

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <div class="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2 border-b border-line bg-surface px-3 py-2">
        <div class="min-w-[14rem] flex-[1_1_16rem]">
          <label for="group-q" class="sr-only-focusable">
            Search group titles and labels
          </label>
          <Input
            id="group-q"
            type="search"
            value={search()}
            placeholder="Search group titles and group labels…"
            onKeyDown={(e) => {
              if (e.key === "Enter") setParam("q", e.currentTarget.value);
            }}
            onBlur={(e) => setParam("q", e.currentTarget.value)}
          />
        </div>

        <ToggleGroup<GroupState>
          legend="Group state"
          options={GROUP_STATES.map((s) => ({ value: s, label: STATE_LABEL[s] }))}
          selected={states()}
          onChange={(next) => setParam("state", next.length > 0 ? next.join(",") : null)}
        />

        <label class="flex items-center gap-1.5 text-[12px] text-ink-muted">
          <span>Ack</span>
          <Select
            value={ack() ?? ""}
            onChange={(e) => setParam("ack", e.currentTarget.value || null)}
            title="`unacked` returns groups with at least one unacknowledged member."
          >
            <option value="">Any</option>
            <option value="unacked">Has unseen members</option>
            <option value="acked">Fully acknowledged</option>
          </Select>
        </label>

        <label class="flex items-center gap-1.5 text-[12px] text-ink-muted">
          <span>Storm</span>
          <Select
            value={storm() === null ? "" : String(storm())}
            onChange={(e) => setParam("storm", e.currentTarget.value || null)}
          >
            <option value="">Any</option>
            <option value="true">In storm mode</option>
            <option value="false">Not in storm mode</option>
          </Select>
        </label>

        <span class="ml-auto text-[12px] tabular-nums text-ink-muted" aria-live="polite">
          {fmtCount(all().length)}
          {groups.data?.page.has_more === true ? "+" : ""} groups
        </span>
      </div>

      <Switch>
        <Match when={groups.isError}>
          <ErrorState error={groups.error} onRetry={() => void groups.refetch()} />
        </Match>
        <Match when={groups.isPending && all().length === 0}>
          <TableSkeleton rows={10} cols={5} />
        </Match>
        <Match when={all().length === 0}>
          <EmptyState
            title="No groups match."
            body="A group appears when Alertmanager routes a notification to oto. If nothing has been routed yet, there is nothing to group."
          />
        </Match>
        <Match when={true}>
          <div class="min-h-0 flex-1 overflow-auto">
            <ul>
              <For each={all()}>{(group) => <GroupRow group={group} />}</For>
            </ul>

            <Show when={groups.data?.page.has_more === true}>
              <div class="border-t border-line px-3 py-2 text-center">
                <Button size="sm" busy={groups.isFetching} onClick={loadMore}>
                  Load more
                </Button>
              </div>
            </Show>
          </div>
        </Match>
      </Switch>
    </div>
  );
}

const GroupRow = (props: { readonly group: Group }) => {
  const g = (): Group => props.group;
  /** A group is as alive as its liveliest member. */
  const live = (): boolean => g().firing_count > 0;
  const unacked = (): number => Math.max(0, g().total_count - g().acked_count);

  return (
    <li class={cx("border-b border-line", live() ? "bg-firing-fill/30" : "bg-surface")}>
      <A
        href={`/groups/${g().id}`}
        class="flex items-start gap-3 px-3 py-2.5 hover:bg-raised/60"
      >
        <span
          aria-hidden="true"
          class={cx(
            "mt-0.5 h-8 w-[3px] shrink-0 rounded-full",
            live() ? "bg-firing-solid" : "bg-suppressed-solid",
          )}
        />

        <SeverityMark severity={g().severity} class="mt-0.5" />

        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <span class="min-w-0 truncate text-[13px] font-medium text-ink">{g().title}</span>
            <Show when={g().generation > 1}>
              <Chip title="A new generation opens when a closed group re-opens. One generation owns exactly one chat thread.">
                gen {g().generation}
              </Chip>
            </Show>
            <Show when={g().storm_mode}>
              <StormChip />
            </Show>
            <Show when={g().state === "closed"}>
              <Chip>closed</Chip>
            </Show>
          </div>

          <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-ink-muted">
            <span class="tabular-nums">
              {fmtCount(g().total_count)} member{g().total_count === 1 ? "" : "s"}
            </span>
            <Show when={g().firing_count > 0}>
              <span class="inline-flex items-center gap-1">
                <span class="size-1.5 rounded-full bg-firing-solid" aria-hidden="true" />
                {g().firing_count} firing
              </span>
            </Show>
            <Show when={g().suppressed_count > 0}>
              <span class="inline-flex items-center gap-1">
                <span class="size-1.5 rounded-full bg-suppressed-solid" aria-hidden="true" />
                {g().suppressed_count} suppressed
              </span>
            </Show>
            <Show when={g().resolved_count > 0}>
              <span class="inline-flex items-center gap-1">
                <span class="size-1.5 rounded-full bg-resolved-solid" aria-hidden="true" />
                {g().resolved_count} resolved
              </span>
            </Show>
            <Show when={g().expired_count > 0}>
              <span class="inline-flex items-center gap-1">
                <span class="size-1.5 rounded-full bg-expired-solid" aria-hidden="true" />
                {g().expired_count} expired
              </span>
            </Show>
            <Show when={unacked() > 0}>
              <span title="Members nobody has recorded seeing yet.">{unacked()} unseen</span>
            </Show>
            {/* `cluster_key` is a first-class field on the group, never read
                out of `group_labels` — the route need not have grouped on it. */}
            <span class="font-mono">cluster {g().cluster_key}</span>
            <Show when={g().receiver !== ""}>
              <span class="font-mono">receiver {g().receiver}</span>
            </Show>
          </div>
        </div>

        <span class="shrink-0 text-right text-[11px] text-ink-subtle">
          <RelativeTime value={g().last_activity_at} label="Last activity" />
        </span>
      </A>
    </li>
  );
};
