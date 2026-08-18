/**
 * The two tabs of the alert list: everything oto is speaking about, and
 * everything oto has been asked to be quiet about.
 *
 * ⭐⭐ THE BADGE IS THE WHOLE SAFETY ARGUMENT, NOT DECORATION.
 *
 * Splitting snoozed alerts out of the default list reverses a stated principle —
 * `filters.ts` used to say, correctly, that *"hiding snoozed alerts from the
 * default list is how an incident is lost"*. The reversal is only defensible
 * because of what this strip does, and it fails the moment any of these three
 * rules is relaxed:
 *
 *   1. **The tab is present at zero.** It never disappears, so it can never be
 *      forgotten. A snooze may run for thirty days; a surface that vanishes when
 *      the count reaches zero is a surface nobody learns is there.
 *   2. **The count is hidden at zero.** "Quiet" alone is quieter than "Quiet (0)"
 *      and says the same thing. A zero that is drawn is a number the eye has to
 *      read before it can dismiss it.
 *   3. **The count carries the WORST STATE INSIDE IT** — `Quiet (12 · 2 firing)`,
 *      never `Quiet (12)`. This is the clause that replaces the per-row badge.
 *      Twelve quiet alerts of which none is live is housekeeping; twelve of which
 *      two are firing is an operator being kept from something. A bare total
 *      cannot tell those apart, and a list that hid the difference would have
 *      lost exactly the incident the old rule was written to protect.
 *
 * ⛔ NO STATE HUE, EVEN THOUGH IT NAMES A STATE (ADR 0012, §M.2). The saturated
 * `--oto-state-*` palette belongs to rows, where scarcity is what makes a firing
 * alert loud. A permanently-mounted strip of chrome burning the firing hue would
 * blunt every row beneath it. The worst state is carried by the WORD — "2
 * firing" — which is unambiguous without spending any colour at all.
 */
import { Show, type Component } from "solid-js";

import type { Alert, State } from "~/api/types";
import { STATE_LABEL } from "~/components/StateChip";
import { Tabs, TabsList, TabsTrigger } from "~/components/ui/Tabs";
import { count as fmtCount } from "~/lib/format";
import type { AlertTab } from "~/features/alerts/filters";

/**
 * What the Quiet badge knows.
 *
 * `partial` is what keeps it honest when the count was capped: the strip may
 * understate how many holds are in force, and it may never claim a total it did
 * not see. The org-wide snooze banner already reads `50+` for the same reason.
 */
export interface QuietSummary {
  readonly total: number;
  readonly partial: boolean;
  /** The liveliest state present, and how many members hold it. */
  readonly worst: { readonly state: State; readonly n: number } | null;
}

/**
 * State precedence, liveliest first — the client-side twin of
 * `domain.AlertRollup.RollupState`.
 *
 * ⛔ `expired` OUTRANKS `resolved`, and the two are never merged. "oto stopped
 * hearing about this" is an open question; "the upstream said it ended" is a
 * closed one, and a snooze covering the first is a different kind of quiet from a
 * snooze covering the second.
 */
const LIVELIEST: readonly State[] = ["firing", "suppressed", "expired", "resolved"];

/**
 * Summarise a page of quiet alerts into the badge.
 *
 * ⭐ `worst` IS NULL WHEN NOTHING IS FIRING, SUPPRESSED OR EXPIRED, not when the
 * page is empty. Twelve resolved alerts somebody is still being quiet about are
 * worth counting and not worth alarming anybody with, so the badge reads
 * `Quiet (12)` — the second clause appears exactly when there is something live
 * behind it, which is what makes its presence meaningful.
 */
export function summariseQuiet(alerts: readonly Alert[], partial: boolean): QuietSummary {
  const byState = new Map<State, number>();
  for (const a of alerts) byState.set(a.state, (byState.get(a.state) ?? 0) + 1);

  let worst: QuietSummary["worst"] = null;
  for (const state of LIVELIEST) {
    if (state === "resolved") break;
    const n = byState.get(state) ?? 0;
    if (n > 0) {
      worst = { state, n };
      break;
    }
  }
  return { total: alerts.length, partial, worst };
}

/** The badge's text, e.g. `12 · 2 firing`, or "" when there is nothing to say. */
export function quietBadgeLabel(q: QuietSummary | null): string {
  if (q === null || q.total === 0) return "";
  const total = `${fmtCount(q.total)}${q.partial ? "+" : ""}`;
  if (q.worst === null) return total;
  return `${total} · ${fmtCount(q.worst.n)} ${STATE_LABEL[q.worst.state].toLowerCase()}`;
}

export interface AlertTabsProps {
  readonly tab: AlertTab;
  readonly onChange: (tab: AlertTab) => void;
  /** `null` while the count is still unknown — the tab renders bare, never zero. */
  readonly quiet: QuietSummary | null;
}

export const AlertTabs: Component<AlertTabsProps> = (props) => {
  const badge = (): string => quietBadgeLabel(props.quiet);

  return (
    <Tabs
      value={props.tab}
      onChange={(v: string) => props.onChange(v === "quiet" ? "quiet" : "active")}
    >
      <TabsList aria-label="Which alerts to show">
        <TabsTrigger value="active">Alerts</TabsTrigger>
        <TabsTrigger
          value="quiet"
          title={
            "Alerts oto has been asked to stay quiet about. They are still firing and are still " +
            "whatever severity they were — a snooze is a fact about oto's notifications, never " +
            "about the signal."
          }
        >
          Quiet
          {/* The count is a separate node so the tab's own label is stable for a
              screen reader walking the list, and so the number can be
              tabular-nums without the word being. */}
          <Show when={badge() !== ""}>
            <span class="ml-1.5 tabular-nums text-ink-muted">({badge()})</span>
          </Show>
        </TabsTrigger>
      </TabsList>
    </Tabs>
  );
};
