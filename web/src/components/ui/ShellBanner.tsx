/**
 * The strips that appear under the app header when the shell has something the
 * operator must know **before** reading the rows below.
 *
 * They share one visual treatment on purpose: a full-bleed hairline strip under
 * the header, in the chrome palette, with no motion and no timer. `AppShell.tsx`
 * records why oto ships no toast at all — a message about whether the screen can
 * be trusted must not be able to expire before it is read.
 *
 * Three rules every strip here obeys:
 *
 *   1. **Silent when there is nothing to say.** Each renders nothing *visible*
 *      at all on the healthy path — no border, no padding, no text — so the
 *      table below always starts at the same offset and nothing shifts under a
 *      cursor that is already moving. The one thing that does stay mounted is
 *      the empty region the strip announces through, which is an unstyled node
 *      holding nothing and is therefore zero pixels tall. It has to be there
 *      *before* the news is; `ShellBanner` below says why.
 *   2. **Tier A, always (ADR 0012, §M.2).** These are facts about oto's own
 *      reach and oto's own notifications — not the state of an alert. Spending a
 *      `--oto-state-*` hue here would blunt exactly the scarcity that makes a
 *      firing row loud.
 *   3. **Enumerations are capped.** A strip lives above a live alert table and
 *      may never push it off screen, so a long list stops and points at the
 *      screen built for long lists.
 */
import { A } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";
import { For, Show, createMemo, createSignal, type JSX, type ParentComponent } from "solid-js";

import { getStatsOverview, listActiveSnoozes } from "~/api/endpoints";
import type { ActiveSnooze } from "~/api/types";
import { qk } from "~/api/keys";
import { sourcesQuery } from "~/api/queries";
import { RelativeTime } from "~/components/Time";
import { Button, cx } from "~/components/ui/primitives";
import type { Source } from "~/api/types";

/* -------------------------------------------------------------------------- */
/* The shared strip                                                           */
/* -------------------------------------------------------------------------- */

export interface ShellBannerProps {
  /**
   * Whether the strip has anything to say right now.
   *
   * ⛔ A PROP, AND NOT A `<Show>` AROUND THIS COMPONENT AT THE CALL SITE, WHICH
   * IS WHAT IT USED TO BE. The strip is conditional; the region it speaks
   * through must not be. See the note on the component below.
   */
  readonly when: boolean;
  /**
   * Whether a change to this strip's contents is worth one polite announcement.
   *
   * Defaults to `true` — a strip appearing is news. Pass `false` for a strip
   * whose text ticks: a live region wrapped around a countdown re-announces the
   * whole banner every time the clock moves, which is how a screen reader is
   * taught to be ignored. `false` renders no live region at all, so that strip
   * is read only when the operator walks onto it.
   */
  readonly announce?: boolean;
  /** Renders a Dismiss button. Omit for a strip that must last as long as its cause. */
  readonly onDismiss?: () => void;
  readonly class?: string;
}

/**
 * The strip's dress, worn only while it has something to say. An unstyled empty
 * div is zero pixels tall; a *styled* empty one is a border and an offset, which
 * is the whole reason the region below cannot simply be a permanent strip.
 */
const STRIP_CLASS = cx(
  "flex items-center justify-between gap-3 border-b border-line-strong bg-raised",
  "px-4 py-1.5 text-[12px] text-ink",
);

/**
 * ⛔ THE STRIP IS CONDITIONAL. THE REGION IT SPEAKS THROUGH IS NOT.
 *
 * Assistive technology announces mutations *inside a live region that already
 * existed*. A region that enters the DOM already holding its words is treated as
 * initial page content, and every major screen reader frequently announces it
 * not at all. The old shape here — `<Show>` around the whole component, with
 * `role="status"` on the strip's own root — made the one case the strip exists
 * for (it just appeared) the one case least likely to be spoken.
 *
 * So the region is mounted always and only its contents swap. It stays empty and
 * unstyled while there is nothing to say, which costs the header no border and
 * the table below no offset — that requirement is what rules out the obvious fix
 * of keeping a *dressed* empty strip around. This is the pattern `ConnectionBadge`
 * (`AppShell.tsx`) and the rejection feed's standing line already use: a
 * permanently-mounted `aria-live="polite" aria-atomic="true"` node whose text is
 * replaced. `role="status"` is dropped rather than moved — it means exactly those
 * two attributes, and spelling them out is what the rest of this app does.
 * `aria-relevant` stays off everywhere: its default already covers text arriving,
 * and the one thing it would add — announcing on *removal* — is the strip going
 * away, which is not news.
 *
 * The opposite failure is a region that is always mounted AND always full: it
 * re-announces on every unrelated mutation until the operator learns to read past
 * it. Two things keep this one quiet. It holds nothing whatsoever on the healthy
 * path, so a healthy org is announced never, however often the shell re-renders.
 * And while it is up, its contents come from query data the client keeps
 * referentially stable across an unchanged refetch, so the minute safety-net poll
 * mutates no text and says nothing a second time. A strip whose own words tick
 * cannot be made quiet that way and must pass `announce={false}` — `SnoozeBanner`.
 *
 * One silence is deliberate: `AppShell` is remounted on every route change, so a
 * strip that was already up comes back with its text in place and says nothing.
 * That is right. A fact the operator has already heard is not news, and a region
 * that re-reads itself on every nav click is one that gets turned off.
 */
export const ShellBanner: ParentComponent<ShellBannerProps> = (props) => (
  <div
    aria-live={props.announce === false ? undefined : "polite"}
    aria-atomic={props.announce === false ? undefined : "true"}
    class={props.when ? cx(STRIP_CLASS, props.class) : ""}
  >
    <Show when={props.when}>
      <div class="min-w-0">{props.children}</div>
      <Show when={props.onDismiss}>
        {(dismiss) => (
          <Button size="sm" variant="ghost" class="shrink-0" onClick={() => dismiss()()}>
            Dismiss
          </Button>
        )}
      </Show>
    </Show>
  </div>
);

/* -------------------------------------------------------------------------- */
/* A source oto cannot reach                                                  */
/* -------------------------------------------------------------------------- */

/**
 * How gently the shell re-reads the facts behind these strips.
 *
 * The live stream already invalidates both keys — `source.health` invalidates
 * the source list and `alert.upserted` invalidates the snooze list (see
 * `api/live.tsx`) — so this interval is the safety net for a stream a proxy has
 * killed, not the mechanism. A minute is deliberately slower than the data's own
 * production rate: `source_health` is written by the reconciler, whose shipped
 * period is 30 s, so asking more often cannot surface anything sooner. Background
 * tabs poll not at all, because `refetchIntervalInBackground` stays off.
 */
const SAFETY_NET_MS = 60_000;

/** How many sources the strip names before it stops naming them. */
const NAMED_SOURCES_MAX = 3;

/**
 * ⛔ QUIET IS NOT THE SAME AS BLIND, AND THE SCREEN MUST SAY WHICH IT IS.
 *
 * The reaper guard (§B.4) holds every occurrence belonging to a source oto
 * cannot reach rather than expiring it. That is the single highest-value
 * correctness rule in the system and it is completely invisible: the list below
 * simply stops changing. Without this strip, an operator reads a calm table as
 * "nothing is wrong" when the truth is "oto has lost sight of a cluster and is
 * holding its last known answer".
 *
 * **One strip for every unreachable source, not one strip each.** The fact is
 * singular — oto's view of the world is incomplete — and a strip per source
 * would stack until it pushed the alert table off screen, which is the exact
 * harm the guard exists to avoid causing.
 *
 * **Not dismissible.** It lasts precisely as long as the condition and vanishes
 * on its own when health returns. A dismissed one would restore the confusion it
 * was added to remove.
 *
 * Only `unreachable` raises it. `degraded` is a settings-screen concern: it is
 * the third consecutive failure that blocks the reaper, and a strip that fired
 * on the first transient timeout would be back to being ignorable within a week.
 */
export const SourceReachBanner = (): JSX.Element => {
  // The resource's own definition plus the one thing that is this strip's and
  // not the resource's: the safety net under a stream a proxy may have killed.
  const sources = useQuery(() => ({ ...sourcesQuery(), refetchInterval: SAFETY_NET_MS }));

  /**
   * Whether the page in hand is the whole org — `page.has_more` on `GET /sources`.
   *
   * This is the gate on the second query below and the reason a normal org pays
   * nothing for the paragraph above it.
   */
  const truncated = (): boolean => sources.data?.page.has_more === true;

  /**
   * ⛔ THE SECOND READ IS GATED ON THE FIRST BEING SHORT, AND THAT GATE IS THE
   * WHOLE DESIGN.
   *
   * Health travels embedded on each source — `GET /sources` serves "a page of
   * sources, each with its current health" — so the names, and the upstream error
   * behind each one, cost one request and no per-source fan-out. But that page is
   * keyset with a default `limit` of 50 (§E.1), so an org whose only unreachable
   * source sorts 51st would get no strip at all, and the strip could not even tell
   * that it was looking at part of a list.
   *
   * The org-wide count is on the dashboard roll-up, and `GET /stats/overview` is
   * twenty-six columns across five tables — including a `notification_deliveries`
   * scan with two joins — to be read for one of them. So it is asked for ONLY when
   * `has_more` says the page cannot answer: an org whose sources fit on one page
   * makes no second request, ever, and `enabled` is what enforces that rather than
   * a branch after the fact.
   *
   * The count is trustworthy now, and was not always: the roll-up's source CTE was
   * a bare `FROM source_health WHERE org_id = $1`, and because `SoftDelete` leaves
   * the health row at its last verdict, an org that ever deleted a source while it
   * was unreachable would have carried this strip forever with no source left to
   * name. That CTE now joins `alert_sources` and filters `deleted_at IS NULL`, the
   * way its channel CTE always did.
   *
   * It rides the same sixty-second safety net and no live invalidation: a
   * `source.health` frame refetches the source list, which is where the names are,
   * and putting the whole roll-up behind every reconciler heartbeat would cost far
   * more than the one minute it can lag by.
   */
  const overview = useQuery(() => ({
    queryKey: qk.stats.overview(),
    queryFn: ({ signal }: { signal: AbortSignal }) => getStatsOverview({}, { signal }),
    enabled: truncated(),
    refetchInterval: SAFETY_NET_MS,
  }));

  /** The unreachable sources on the page in hand — the only ones with a name. */
  const seen = createMemo<readonly Source[]>(() =>
    (sources.data?.data ?? []).filter((s) => s.health?.status === "unreachable"),
  );

  /**
   * How many sources oto cannot reach, org-wide.
   *
   * The page's own tally is the floor rather than an alternative: while the
   * roll-up is in flight — or if it somehow answers with less than the page can
   * already see — the strip still says what it has seen for itself, and never
   * less.
   */
  const count = (): number =>
    Math.max(seen().length, truncated() ? (overview.data?.sources.unreachable ?? 0) : 0);

  const named = (): readonly Source[] => seen().slice(0, NAMED_SOURCES_MAX);
  /** Sources in the count that this strip cannot put a name to. */
  const unnamed = (): number => Math.max(0, count() - named().length);
  const plural = (): boolean => count() > 1;

  /**
   * The opening clause when the count runs past the names.
   *
   * ⛔ THE STRIP MAY NEVER IMPLY IT HAS NAMED EVERYTHING IT IS COUNTING. Two
   * different things put a source out of naming range — it sorted past the page,
   * or the enumeration stopped at three so the alert table keeps its screen — and
   * the operator's next move is the same either way: open the sources screen. So
   * both collapse into one honest sentence that leads with the number and admits
   * how much of it is spelled out.
   */
  const counted = (): string => {
    const label = `${count()} source${plural() ? "s" : ""}`;
    if (named().length > 0) {
      return `oto cannot reach ${label}, of which it can name ${named().length}`;
    }
    return plural()
      ? `oto cannot reach ${label}, none of which it can name here`
      : `oto cannot reach ${label}, which it cannot name here`;
  };

  const names = (): JSX.Element => (
    <For each={named()}>
      {(s, i) => (
        <>
          {i() > 0 ? ", " : ""}
          <span
            class="font-medium"
            title={s.health?.last_error ?? "oto's last attempt to read this source did not succeed."}
          >
            {s.name}
          </span>
        </>
      )}
    </For>
  );

  return (
    // `when` rather than a `<Show>` out here: the region has to be mounted while
    // the source list is still in flight, so that losing sight of a source is a
    // mutation inside it rather than the arrival of a fully-formed region.
    <ShellBanner when={count() > 0}>
      <p>
        <Show when={unnamed() > 0} fallback={<>oto cannot reach {names()}.</>}>
          {counted()}
          <Show when={named().length > 0}>: {names()}</Show>.
        </Show>{" "}
        {plural() ? "Their" : "Its"} alerts are held exactly where they are — never resolved, never
        expired — so a row that has gone quiet may be one oto can no longer see, and this list is
        incomplete until {plural() ? "they are" : "it is"} back.{" "}
        <A href="/settings/sources" class="font-medium text-accent">
          Source health
        </A>
      </p>
    </ShellBanner>
  );
};

/* -------------------------------------------------------------------------- */
/* Every quiet period currently in force                                      */
/* -------------------------------------------------------------------------- */

/**
 * How many snoozes the strip spells out before it stops and links to the list.
 *
 * Five rows is roughly an eighth of an operational viewport, which the alert
 * table can afford. Past that the strip states the count and hands off to
 * `/alerts?snoozed=true`, the screen already built to scroll.
 */
const SNOOZE_ROWS_MAX = 5;

/**
 * Enough rows to state an exact count for any plausible org while rendering
 * five. The page is small — a snooze, an alert reference and a number — so the
 * difference costs nothing and buys an honest total.
 */
const SNOOZE_FETCH_LIMIT = 50;

/**
 * Which holds the operator has already read — and why this lives OUTSIDE the
 * component.
 *
 * Dismissal remembers WHICH snoozes were read, not a signature of the set. A
 * signature (the sorted ids joined) looked equivalent and is not: the server drops
 * a snooze from this list the moment it expires, so in any org holding more than
 * one, the first expiry changes the signature and re-opens the strip over a
 * strictly SMALLER set the operator already read. Attrition is not news.
 *
 * ⚠ It is module state because `AppShell` IS REMOUNTED ON EVERY ROUTE CHANGE —
 * `App.tsx` gives each route its own `<Authenticated>` wrapper instead of nesting
 * the screens under one layout route, so component state here would reset on the
 * first nav click and re-open the strip over the identical holds. Component state
 * would pass its own regression test and still break for the operator. Fixing the
 * remount is tracked separately; until then this deliberately outlives the mount.
 *
 * Cross-principal leakage is not a concern: snooze ids are server UUIDs, so
 * another org's holds are never in this set and can never be silenced by it.
 */
const [dismissedSnoozes, setDismissedSnoozes] = createSignal<ReadonlySet<string>>(new Set());

/**
 * Clears the read set. Only a test needs this: because the state deliberately
 * outlives the mount, one case's Dismiss click would otherwise silence the next
 * case's strip.
 */
export const resetDismissedSnoozes = (): void => {
  setDismissedSnoozes(new Set<string>());
};

/**
 * The counterweight that makes snoozing safe (§B.8.6).
 *
 * A snooze is not a state and not a suppression: the alert is still firing and
 * still whatever severity it was, and the only thing that is quiet is oto. That
 * is precisely why it needs a standing enumeration — a hold nobody can see is a
 * hold nobody remembers taking, and the alert it covers is the one that will be
 * discovered late.
 *
 * Dismissible, because §B.8.6 says so — but dismissal is keyed to the **ids the
 * operator has read**, so it silences those holds and not the next one somebody
 * takes. A hold ending is not news and must not re-open the strip.
 */
export const SnoozeBanner = (): JSX.Element => {
  const snoozes = useQuery(() => ({
    queryKey: qk.alerts.activeSnoozes(),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listActiveSnoozes({ limit: SNOOZE_FETCH_LIMIT }, { signal }),
    refetchInterval: SAFETY_NET_MS,
  }));

  const all = createMemo<readonly ActiveSnooze[]>(() => snoozes.data?.data ?? []);
  const shown = (): readonly ActiveSnooze[] => all().slice(0, SNOOZE_ROWS_MAX);
  const more = (): number => Math.max(0, all().length - SNOOZE_ROWS_MAX);
  const partial = (): boolean => snoozes.data?.page.has_more === true;

  const unread = createMemo(() => all().some((s) => !dismissedSnoozes().has(s.id)));
  const dismissAll = (): void => {
    setDismissedSnoozes(new Set(all().map((s) => s.id)));
  };

  // `50+` rather than `50` when the page was capped: the strip may understate how
  // many holds are in force, but it may never claim a total it did not see.
  const countLabel = (): string =>
    `${all().length}${partial() ? "+" : ""} alert${all().length === 1 && !partial() ? "" : "s"}`;

  return (
    /* `announce={false}`: the rows below carry live countdowns, and a live region
       around a ticking clock re-reads itself every ten seconds. So this strip
       gets no `aria-live` at all — what stays mounted for it is one empty,
       unstyled and entirely silent node, and the rows are read when the operator
       walks onto them. */
    <ShellBanner when={all().length > 0 && unread()} announce={false} onDismiss={dismissAll}>
      <p>
        oto is holding notifications on <span class="font-medium">{countLabel()}</span>. They are
        still firing and still whatever severity they were — only oto is quiet.
      </p>

      <ul class="mt-0.5 flex flex-col">
        <For each={shown()}>
          {(s) => (
            <li class="flex flex-wrap items-baseline gap-x-2">
              <A
                href={`/alerts/${s.alert_id}`}
                class="font-medium text-ink underline decoration-line-strong underline-offset-2 hover:decoration-accent"
              >
                {s.alert?.alertname ?? s.alert_key}
              </A>
              <Show when={s.alert?.cluster_key}>
                {(key) => <span class="font-mono text-[11px] text-ink-subtle">{key()}</span>}
              </Show>
              <span class="text-ink-muted">
                resumes <RelativeTime value={s.snoozed_until} label="Notifications resume" />
              </span>
              <span class="text-ink-subtle">{s.snoozed_by_label}</span>
              <Show when={s.note}>
                {(note) => <span class="truncate text-ink-subtle">— {note()}</span>}
              </Show>
            </li>
          )}
        </For>
      </ul>

      <Show when={more() > 0 || partial()}>
        <A href="/alerts?snoozed=true" class="font-medium text-accent">
          {`${more()}${partial() ? "+" : ""} more — open every held alert`}
        </A>
      </Show>
    </ShellBanner>
  );
};
