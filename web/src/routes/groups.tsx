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
import { For, Match, Show, Switch, createEffect, createMemo, createSignal } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { listAlertGroups } from "~/api/endpoints";
import { GroupStateSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { Group, GroupListQuery, GroupState } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { SeverityMark, StormChip } from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { FilterRow } from "~/components/ui/FilterRow";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "~/components/ui/Select";
import { Chip } from "~/components/ui/surfaces";
import { TextField, TextFieldInput, TextFieldLabel } from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { EmptyState, ErrorState, TableSkeleton } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { count as fmtCount } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";

/** An option for the small single-selects below: a value plus its label. */
interface Opt<T extends string> {
  readonly value: T;
  readonly label: string;
}

const ACK_OPTIONS: Opt<"" | "acked" | "unacked">[] = [
  { value: "", label: "Any" },
  { value: "unacked", label: "Has unseen members" },
  { value: "acked", label: "Fully acknowledged" },
];

const STORM_OPTIONS: Opt<"" | "true" | "false">[] = [
  { value: "", label: "Any" },
  { value: "true", label: "In storm mode" },
  { value: "false", label: "Not in storm mode" },
];

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

  // A cursor is minted under the whole filter set — state, search, storm and
  // ack, all of them URL params — and §E.3 answers one carried across a filter
  // change with `400 cursor_filter_mismatch`. The fingerprint is those four,
  // so any of them changing discards the held pages (see `createKeysetFeed`).
  const fingerprint = createMemo(
    () => `${states().join(",")}|${search()}|${String(storm())}|${String(ack())}`,
  );

  // The annotation cuts the type-inference loop the closure creates: the feed
  // reads the query's envelope, and the query's key carries the feed's cursor.
  const feed: KeysetFeed<Group> = createKeysetFeed({
    envelope: () => groups.data,
    isPlaceholder: () => groups.isPlaceholderData,
    keyOf: (g) => g.id,
    fingerprint,
  });

  const query = createMemo<GroupListQuery>(() => {
    const q: Record<string, unknown> = { limit: PAGE_SIZE, sort: "-last_activity_at" };
    if (states().length > 0) q["state"] = [...states()];
    if (search() !== "") q["q"] = search();
    if (storm() !== null) q["storm"] = storm();
    if (ack() !== null) q["ack"] = ack();
    if (feed.cursor() !== null) q["cursor"] = feed.cursor();
    return q as GroupListQuery;
  });

  const groups = useQuery(() => ({
    queryKey: qk.groups.list(query()),
    queryFn: ({ signal }: { signal: AbortSignal }) => listAlertGroups(query(), { signal }),
    placeholderData: keepPrevious,
  }));

  const all = feed.rows;

  // The URL is authoritative; typing only ever reads and writes this local
  // draft. `search()` changes exactly when a commit (Enter/blur) navigates or
  // when Back/Forward changes the URL out from under the box, and either way
  // the draft is meant to follow it. It is never written to on every
  // keystroke, matching the old `<input>`'s deliberately delayed commit.
  const [draft, setDraft] = createSignal(search());
  createEffect(() => setDraft(search()));

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <FilterRow class="shrink-0">
        <TextField
          class="min-w-[14rem] flex-[1_1_16rem]"
          value={draft()}
          onChange={setDraft}
        >
          <TextFieldLabel class="sr-only-focusable">
            Search group titles and labels
          </TextFieldLabel>
          <TextFieldInput
            id="group-q"
            type="search"
            placeholder="Search group titles and group labels…"
            onKeyDown={(e) => {
              if (e.key === "Enter") setParam("q", e.currentTarget.value);
            }}
            onBlur={(e) => setParam("q", e.currentTarget.value)}
          />
        </TextField>

        <ToggleGroup legend="Group state" multiple value={[...states()]} onChange={(next) => setParam("state", next.length > 0 ? next.join(",") : null)}>
          <For each={GROUP_STATES}>
            {(s) => <ToggleGroupItem value={s}>{STATE_LABEL[s]}</ToggleGroupItem>}
          </For>
        </ToggleGroup>

        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Ack</span>
          <Select<Opt<"" | "acked" | "unacked">>
            multiple={false}
            options={ACK_OPTIONS}
            optionValue="value"
            optionTextValue="label"
            value={ACK_OPTIONS.find((o) => o.value === (ack() ?? "")) ?? ACK_OPTIONS[0]!}
            onChange={(opt) => setParam("ack", opt && opt.value !== "" ? opt.value : null)}
            itemComponent={(p) => <SelectItem item={p.item}>{p.item.rawValue.label}</SelectItem>}
          >
            <SelectTrigger
              aria-label="Ack"
              title="`unacked` returns groups with at least one unacknowledged member."
            >
              <SelectValue<Opt<"" | "acked" | "unacked">>>
                {(state) => state.selectedOption().label}
              </SelectValue>
            </SelectTrigger>
            <SelectContent />
          </Select>
        </label>

        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Storm</span>
          <Select<Opt<"" | "true" | "false">>
            multiple={false}
            options={STORM_OPTIONS}
            optionValue="value"
            optionTextValue="label"
            value={
              STORM_OPTIONS.find((o) => o.value === (storm() === null ? "" : String(storm()))) ??
              STORM_OPTIONS[0]!
            }
            onChange={(opt) => setParam("storm", opt && opt.value !== "" ? opt.value : null)}
            itemComponent={(p) => <SelectItem item={p.item}>{p.item.rawValue.label}</SelectItem>}
          >
            <SelectTrigger aria-label="Storm">
              <SelectValue<Opt<"" | "true" | "false">>>
                {(state) => state.selectedOption().label}
              </SelectValue>
            </SelectTrigger>
            <SelectContent />
          </Select>
        </label>

        <span class="ml-auto text-body tabular-nums text-ink-muted" aria-live="polite">
          {fmtCount(all().length)}
          {feed.hasMore() ? "+" : ""} groups
        </span>
      </FilterRow>

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

            <Show when={feed.hasMore()}>
              <div class="border-t border-line px-3 py-2 text-center">
                <Button variant="secondary" size="sm" busy={groups.isFetching} onClick={feed.loadMore}>
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
    <li class={cn("border-b border-line", live() ? "bg-firing-fill/30" : "bg-surface")}>
      <A
        href={`/groups/${g().id}`}
        class="flex items-start gap-3 px-3 py-2.5 hover:bg-raised/60"
      >
        <span
          aria-hidden="true"
          class={cn(
            "mt-0.5 h-8 w-[3px] shrink-0 rounded-full",
            live() ? "bg-firing-solid" : "bg-suppressed-solid",
          )}
        />

        <SeverityMark severity={g().severity} class="mt-0.5" />

        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <span class="min-w-0 truncate text-item font-medium text-ink">{g().title}</span>
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

          <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-meta text-ink-muted">
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

        <span class="shrink-0 text-right text-meta text-ink-subtle">
          <RelativeTime value={g().last_activity_at} label="Last activity" />
        </span>
      </A>
    </li>
  );
};
