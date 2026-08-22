/**
 * Everything oto decided to say, and everything it decided not to.
 *
 * ⭐⭐ THE SUPPRESSED ROWS ARE WHY THIS SCREEN EXISTS. A notification log that
 * only listed what was sent would answer the easy half of the question. The one
 * an operator actually arrives with — *"why did nobody hear about this?"* — is
 * answered by a row that says `no_policy`, `throttled`, `snoozed`, and oto
 * records every one of those at the moment it takes the decision (§B.6). oto's
 * silence must never be indistinguishable from "no alert"; per alert, the "Who
 * was told" panel says so, and this is the same record read across the org.
 *
 * The feed is `GET /api/v1/notifications`, which has been in the contract since
 * v1 and had no reader in the UI at all — the history was written, retained and
 * unreadable outside `psql`.
 *
 * ⛔ THIS IS A LOG, NOT A QUEUE. Nothing here is actionable and nothing here has
 * a button: a delivery that failed is retried from the alert it belongs to,
 * where the context to judge that lives. Rows are read newest first and paged by
 * keyset, because "page 3" of a list that grows at the head is a lie
 * (`~/lib/keysetFeed`).
 *
 * ⛔ TIER A ONLY (§M.2). A `failed` intent is serious and it is still not the
 * state of an alert; borrowing a state hue here would spend exactly the scarcity
 * that makes a firing row loud. Weight, the stronger hairline and unambiguous
 * words carry it.
 */
import { For, Match, Show, Switch, createMemo, createSignal, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";
import { A } from "@solidjs/router";

import { NotificationReasonSchema, NotificationStatusSchema } from "~/api/generated/validators";
import { notificationActivityQuery } from "~/api/queries";
import type { Notification, NotificationListQuery, NotificationReason, NotificationStatus } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { DeadDeliveries } from "./DeadDeliveries";
import {
  Combobox,
  ComboboxContent,
  ComboboxControl,
  ComboboxInput,
  ComboboxItem,
  ComboboxLabel,
  ComboboxTrigger,
} from "~/components/ui/Combobox";
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorState } from "~/components/ui/states";
import { CheckList, FilterMenu, summarise } from "~/components/ui/FilterMenu";
import { cn } from "~/lib/cn";
import { absoluteTime, count as fmtCount, shortId } from "~/lib/format";
import { createKeysetFeed, type KeysetFeed } from "~/lib/keysetFeed";
import { PANEL_BODY, PANEL_HEADER, ROW } from "~/features/settings/rhythm";

import { describeSuppression, REASON_LABEL, STATUS_LABEL } from "./vocabulary";

/** One page. Fifty: this is a whole screen, not a panel inside a row. */
const PAGE_SIZE = 50;

/**
 * The picklists, read off the contract rather than typed here.
 *
 * ⛔ NO `suppressed_reason` FILTER IS OFFERED, and its absence is deliberate.
 * The server's allow-list for that parameter is *narrower* than the enum the
 * rows carry — `snoozed` is a suppression oto records and not one it accepts as
 * a filter — so a control built from the enum would offer a choice that 400s,
 * and one built from the narrower list would be a filter silently missing the
 * single most useful reason on it (a silence a person chose). Until the two
 * agree, the reason is shown on every row and filtered on none.
 */
const STATUSES: readonly NotificationStatus[] = NotificationStatusSchema.options;
const REASONS: readonly NotificationReason[] = NotificationReasonSchema.options;

/**
 * The one line the feed keeps mounted to say where it stands.
 *
 * ⛔ A LIVE REGION IS NEVER BORN HOLDING ITS TEXT — a node that enters the DOM in
 * the same mutation as its words is commonly announced by nothing at all. So the
 * region is mounted before any answer exists and only ever swaps what is inside
 * it, exactly as `RejectionsPanel` and `AppShell` do.
 */
interface Standing {
  readonly text: string;
  readonly tone: string;
}

export const ActivitySection: Component = () => {
  const [statuses, setStatuses] = createSignal<readonly NotificationStatus[]>([]);
  const [reasons, setReasons] = createSignal<readonly NotificationReason[]>([]);

  const filtered = (): boolean => statuses().length > 0 || reasons().length > 0;

  // Both axes are the cursor's fingerprint: a cursor is minted against a filter
  // set server-side, and carrying one across a change is `400
  // cursor_filter_mismatch` (§E.3). The stamp is read in Solid's pure phase, so
  // no request is ever *built* holding the previous filter's cursor.
  const feed: KeysetFeed<Notification> = createKeysetFeed({
    envelope: () => activity.data,
    isPlaceholder: () => activity.isPlaceholderData,
    keyOf: (n) => n.id,
    fingerprint: () => `${statuses().join(",")}|${reasons().join(",")}`,
  });

  const query = createMemo<NotificationListQuery>(() => {
    const cursor = feed.cursor();
    return {
      limit: PAGE_SIZE,
      ...(statuses().length > 0 ? { status: [...statuses()] } : {}),
      ...(reasons().length > 0 ? { reason: [...reasons()] } : {}),
      ...(cursor !== null ? { cursor } : {}),
    };
  });

  const activity = useQuery(() => notificationActivityQuery(query()));

  const rows = feed.rows;

  /** @see Standing — mounted from the first render, words swap. */
  const standing = createMemo<Standing>(() => {
    // An error already speaks for itself in `ErrorState`; the region says
    // nothing rather than narrating over it, and stays mounted for the recovery.
    if (activity.isError) return { text: "", tone: "" };
    if (activity.isPending && rows().length === 0) {
      return { text: "Loading…", tone: "text-meta text-ink-subtle" };
    }
    // Two different findings, never conflated: a filter that matched nothing is
    // not an org oto has never notified about.
    if (rows().length === 0 && filtered()) {
      return {
        text: "No notification matches those filters.",
        tone: "text-meta font-medium leading-snug text-ink",
      };
    }
    if (rows().length === 0) {
      return {
        text: "oto has never formed a notification intent.",
        tone: "text-meta font-medium leading-snug text-ink",
      };
    }
    const more = feed.hasMore() ? "+" : "";
    return {
      text: `${fmtCount(rows().length)}${more} notifications, newest first.`,
      tone: "text-meta text-ink-muted",
    };
  });

  return (
    <Panel>
      <PanelHeader class={PANEL_HEADER}>
        <PanelTitle>Notification activity</PanelTitle>
        <Show when={filtered()}>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setStatuses([]);
              setReasons([]);
            }}
          >
            Clear filters
          </Button>
        </Show>
      </PanelHeader>

      {/* The filters are a band above the list rather than a panel beside it —
          ADR 0033's rule, applied to the second table in the product. */}
      <div class={cn("flex flex-col gap-md border-b border-line", PANEL_BODY)}>
        {/* ⭐ A DROPDOWN, NOT A STRIP OF PILLS. Six delivery statuses laid out as
            toggles filled a whole row with a control that read as a tab bar —
            one pill lit, the rest waiting — when the fact is a SET: `failed` and
            `dead` together is the normal question. The trigger says which
            statuses are on without being opened, which is what makes the popover
            affordable (see `FilterMenu`). */}
        <div class="flex flex-wrap items-center gap-xs">
          <FilterMenu
            label="Where it got to"
            value={summarise(statuses().map((s) => STATUS_LABEL[s]))}
            title="The delivery's own outcome. `suppressed` and `skipped` are recorded with a reason rather than dropped, which is why they are filterable at all."
          >
            <CheckList<NotificationStatus>
              legend="Where it got to"
              options={STATUSES.map((s) => ({ value: s, label: STATUS_LABEL[s] }))}
              value={statuses()}
              onChange={(next) => setStatuses(next)}
              allLabel="Any outcome"
            />
          </FilterMenu>
        </div>

        {/* Eighteen reasons is too many to lay out as toggles, and the operator
            usually arrives knowing the word they want — so the same searchable
            multi-select the policy editor picks channels with. */}
        {/* Numeric, never `max-w-md`: `md` is one of this app's SPACING steps,
            and Tailwind v4 resolves a named width key against that namespace
            first — `max-w-md` compiles to 12px here (`design/scales.test.ts`). */}
        <div class="max-w-112">
          <Combobox<NotificationReason>
            multiple
            options={[...REASONS]}
            optionValue={(r) => r}
            optionLabel={(r) => REASON_LABEL[r]}
            optionTextValue={(r) => `${REASON_LABEL[r]} ${r}`}
            value={[...reasons()]}
            onChange={(next) => setReasons(next)}
            itemComponent={(itemProps) => (
              <ComboboxItem item={itemProps.item}>
                {REASON_LABEL[itemProps.item.rawValue]}
              </ComboboxItem>
            )}
          >
            <ComboboxLabel class="mb-xs block">About what</ComboboxLabel>
            <ComboboxControl<NotificationReason>>
              {(state) => (
                <>
                  <For each={state.selectedOptions()}>
                    {(r) => (
                      <span class="inline-flex items-center gap-2xs rounded-chip border border-line bg-raised py-0.5 pl-1.5 pr-0.5 text-meta text-ink">
                        {REASON_LABEL[r]}
                        <button
                          type="button"
                          class="flex size-4 items-center justify-center rounded-chip text-ink-subtle hover:bg-surface hover:text-ink"
                          aria-label={`Stop filtering for ${REASON_LABEL[r]}`}
                          onClick={() => state.remove(r)}
                        >
                          <svg
                            xmlns="http://www.w3.org/2000/svg"
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2"
                            stroke-linecap="round"
                            aria-hidden="true"
                            class="size-3"
                          >
                            <path d="M18 6L6 18M6 6l12 12" />
                          </svg>
                        </button>
                      </span>
                    )}
                  </For>
                  <ComboboxInput placeholder={reasons().length > 0 ? "Add another…" : "Any reason"} />
                  <ComboboxTrigger aria-label="Show every reason" />
                </>
              )}
            </ComboboxControl>
            <ComboboxContent />
          </Combobox>
        </div>

        <p class={standing().tone} aria-live="polite" aria-atomic="true">
          {standing().text}
        </p>
      </div>

      <Switch>
        <Match when={activity.isError}>
          <ErrorState error={activity.error} onRetry={() => void activity.refetch()} />
        </Match>

        <Match when={activity.isPending && rows().length === 0}>{null}</Match>

        <Match when={rows().length === 0 && filtered()}>
          <p class={cn(PANEL_BODY, "text-body leading-snug text-ink-muted")}>
            The filters are doing something — which is not the same as oto having stayed quiet.
          </p>
        </Match>

        <Match when={rows().length === 0}>
          <p class={cn(PANEL_BODY, "text-body leading-snug text-ink-muted")}>
            oto records an intent to communicate even when it decides not to send, so an empty log
            means nothing has been computed yet — no alert has reached a case, or this
            deployment runs with notifications wired out entirely.
          </p>
        </Match>

        <Match when={true}>
          <ol>
            <For each={rows()}>{(n) => <ActivityRow notification={n} />}</For>
          </ol>
          <Show when={feed.hasMore()}>
            <div class={PANEL_BODY}>
              <Button size="sm" variant="ghost" busy={activity.isFetching} onClick={feed.loadMore}>
                Load {PAGE_SIZE} more
              </Button>
            </div>
          </Show>
        </Match>
      </Switch>
    </Panel>
  );
};

/* -------------------------------------------------------------------------- */

const ActivityRow: Component<{ readonly notification: Notification }> = (props) => {
  const n = (): Notification => props.notification;

  return (
    <li class={cn(ROW, "flex flex-col gap-2xs")}>
      <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
        <span class="text-body font-medium text-ink">{REASON_LABEL[n().reason] ?? n().reason}</span>
        <Chip title={`Notification status: ${n().status}`}>
          {STATUS_LABEL[n().status] ?? n().status}
        </Chip>
        {/* ⛔ A NULL `policy_id` HAS TWO CAUSES AND THEY ARE NOT THE SAME FACT.
            A policy that matched and has since been deleted is "oto can no
            longer name what routed this"; `no_policy` is "nothing routed it at
            all", and there was never a policy to name. Printing "policy
            deleted" for the second one invents a deletion that never happened —
            and `no_policy` is the commonest row in an org that has not written
            a policy yet, so the wrong reading would be the usual one. The
            suppression sentence below already says it; this chip stays away. */}
        <Show when={n().policy_id ?? (n().suppressed_reason !== "no_policy" ? "deleted" : null)}>
          {(label) => (
            <Chip
              mono={n().policy_id !== null && n().policy_id !== undefined}
              title={
                n().policy_id
                  ? "The notification policy that matched this fact."
                  : "The policy that matched has since been deleted, so oto can no longer name it."
              }
            >
              policy {n().policy_id ? shortId(label()) : "deleted"}
            </Chip>
          )}
        </Show>
        <span class="ml-auto text-meta text-ink-subtle" title={absoluteTime(n().created_at)}>
          <RelativeTime value={n().created_at} label="Recorded" /> ago
        </span>
      </div>

      {/* ⭐ THE ONE ACTIONABLE THING IN THE LOG, AND IT IS GATED ON A COUNT THAT
          COSTS NOTHING. `delivery_summary` arrives on the row: the list handler
          reads the whole page's fan-out in ONE query, so a log with no dead
          deliveries makes no extra request at all and a log with some makes one
          per affected row.

          ⛔ THE GATE IS THE COUNT AND NOT THE STATUS, and that is not a style
          choice. `AggregateStatus` returns `dispatched` whenever anything is
          still in flight, so a fan-out with one dead delivery and one pending
          reads `dispatched` — gating on `failed | partial` would hide the button
          on exactly the mixed case an operator most needs it for. */}
      <Show when={(n().delivery_summary?.dead ?? 0) > 0}>
        <DeadDeliveries notificationId={n().id} />
      </Show>

      {/* Where to go and read the rest. The list carries ids and not names — the
          contract's list response is deliberately thin — so the link is the id,
          and the alert page is where the sentence is. */}
      <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs text-meta">
        <Show
          when={n().alert_id}
          fallback={
            <span
              class="text-ink-subtle"
              title={
                n().subject_kind === "digest"
                  ? "A digest is about a policy's window over a set of alerts, and not about one alert."
                  : "A fact about the notification as a whole rather than about one alert."
              }
            >
              {n().subject_kind === "digest" ? "about a window" : "about no one alert"}
            </span>
          }
        >
          {(id) => (
            <A href={`/alerts/${id()}`} class="text-ink underline decoration-line-strong underline-offset-2">
              alert {shortId(id())}
            </A>
          )}
        </Show>
        {/* ⛔ THE ROW USED TO CARRY A SECOND LINK, TO `/groups/<group_id>`, AND
            THE ALERT GROUP IT POINTED AT NO LONGER EXISTS (git-bug 7570090). The
            Case is the conversation now, so the case link below is the only
            subject link a row offers, and `group_id` is read by nothing here —
            not even to render an id, because an id nothing can be looked up by
            is a dead end dressed as a destination. */}
        <Show when={n().case_id}>
          {(id) => (
            <A
              href={`/cases/${id()}`}
              class="text-ink-muted underline decoration-line underline-offset-2"
            >
              case {shortId(id())}
            </A>
          )}
        </Show>
      </div>

      <Show when={n().suppressed_reason}>
        {(reason) => (
          <p class="text-meta leading-snug text-ink-muted">
            Not sent — {describeSuppression(reason())}. Recorded rather than dropped, so the audit
            trail is complete.
          </p>
        )}
      </Show>

      {/* The list response carries no per-row roll-up — computing one would be a
          query per row, and the contract marks `delivery_summary` optional for
          exactly that reason. When a deployment does send one, it is the answer
          to "did it land", so it is rendered rather than ignored. */}
      <Show when={n().delivery_summary}>
        {(s) => (
          <div class="flex flex-wrap items-center gap-2xs">
            <Stat label="sent" value={s().sent} />
            <Show when={s().pending > 0}>
              <Stat label="pending" value={s().pending} />
            </Show>
            <Show when={s().skipped > 0}>
              <Stat label="skipped" value={s().skipped} />
            </Show>
            <Show when={s().failed > 0}>
              <Stat label="failed" value={s().failed} emphasis />
            </Show>
            <Show when={s().dead > 0}>
              <Stat label="gave up" value={s().dead} emphasis />
            </Show>
          </div>
        )}
      </Show>
    </li>
  );
};

const Stat: Component<{
  readonly label: string;
  readonly value: number;
  readonly emphasis?: boolean;
}> = (props) => (
  <span
    class={cn(
      "inline-flex items-center gap-1 rounded-chip border px-1.5 text-micro leading-5 tabular-nums",
      props.emphasis === true
        ? "border-line-strong bg-raised font-semibold text-ink"
        : "border-line bg-surface text-ink-muted",
    )}
  >
    <span class={props.value === 0 ? "text-ink-subtle" : ""}>{props.value}</span>
    {props.label}
  </span>
);
