/**
 * Every filter the contract serves, and not one it does not — as a toolbar
 * above the alert table (ADR 0033).
 *
 * This has been a three-row strip above the table, then an `<aside>` beside it,
 * then five stacked sections inside the shell's left rail. The rail is the one
 * being retired here. It was never wrong about *structure* — the sections were
 * legible and every control was visible at once — it was wrong about **what a
 * 256 px vertical column beside a table says**: that the filters are a place you
 * go, a peer of Cases / Alerts / Settings, rather than instruments belonging to
 * the list. An operator arriving mid-incident read the rail top-to-bottom and
 * found navigation and filtering in one plane, sharing one scrollbar, with the
 * severity marks they actually came for pushed into the remaining width.
 *
 * So the controls come back into the content column, spanning exactly the table
 * they narrow, as a single band of instruments. `AppShell`'s rail keeps
 * navigation and the two standing facts; §5's "one chrome" rule is unbroken,
 * because what it forbids is a second *rail* (see the note on it in
 * `AppShell.tsx`).
 *
 * ⛔ THE "NOTHING BEHIND A POPOVER" RULE IS SPENT HERE, AND THIS IS THE PRICE.
 * The rail could afford twelve controls open at once; forty pixels of toolbar
 * cannot. Four axes therefore live behind menus, and the honesty that rule was
 * protecting — *"why is this list empty?" must never be a click away* — is
 * bought back three other ways instead, all of them at rest and none of them
 * requiring a click:
 *
 *   1. **A trigger states its value, not just its axis.** `Severity` when it is
 *      off; `Severity critical +1` when it is on, with the ruler glyph filled to
 *      match. Every narrowing axis says so on its own face.
 *   2. **`Clear N filters` is always present when N > 0**, so the *count* of what
 *      is narrowing the list is never behind anything at all.
 *   3. **The two axes with no control of their own** — `namespace` and
 *      `alertname`, which only ever arrive from a roll-up drill-down — keep
 *      their removable pills, on their own row beneath the toolbar. For those
 *      the pill is not a second telling; it is the only one.
 *
 * A menu's contents are still absent from the accessibility tree while it is
 * closed. That is a real cost, stated rather than papered over, and it is why
 * (1) and (2) are load-bearing rather than decorative: a screen-reader user who
 * never opens a menu can still hear every axis, its value, and the total.
 *
 * The toolbar is deliberately quiet, and quieter than the rail was. **It spends
 * no accent at all.** An active control lifts with neutrals — a raised surface,
 * a stronger hairline, and a `text-ink-muted` → `text-ink` weight lift — so it
 * reads correctly in greyscale and costs the severity column nothing (§0.6,
 * §M.2). The one accent left in the whole band is the current *view* inside the
 * Status menu, because that is the only control here that answers "where am I"
 * rather than "what am I looking through".
 *
 * ⭐ THE GLYPH ALPHABET IS THE SIGNATURE (§0.3, `~/components/glyphs`). The
 * severity ruler on the Severity trigger is the same ruler the table's rows
 * carry, and the state marks beside the lifecycle toggles are the same six
 * shapes — but every one of them is drawn `tone="inherit"`, in Tier A ink. The
 * *shape* is the vocabulary the chrome may borrow; the *hue* is state's alone,
 * and lending it to a filter chip is exactly how scarcity is spent.
 *
 * Severity is a free vocabulary, not an enum (`AlertDTO.severity` is
 * deliberately open), so the common three are offered as toggles and anything
 * else arriving from a URL is preserved and shown rather than silently dropped.
 *
 * **The search bar is one control, not two.** A committed label matcher
 * (`namespace="payments"`) and a free-text search word used to live in separate
 * inputs that looked like they did the same job. They render as chips followed
 * by a single trailing `<input>` here instead: type something shaped like a
 * matcher and Space/Enter turns it into a chip; type anything else and Enter
 * makes it `q`. One box, one decision each time you press a key, not "which of
 * these two boxes is this word for".
 */
import {
  For,
  Show,
  createEffect,
  createMemo,
  createSignal,
  type Component,
  type JSX,
  type ParentComponent,
} from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { clustersQuery, labelNamesQuery } from "~/api/queries";
import { SeverityBars, StateGlyph } from "~/components/glyphs";
import { Popover, PopoverContent, PopoverTrigger } from "~/components/ui/Popover";
import {
  Select,
  SelectContent,
  SelectHiddenSelect,
  SelectItem,
  SelectTrigger,
} from "~/components/ui/Select";
import { Button } from "~/components/ui/Button";
import { Chip, SECTION_LABEL } from "~/components/ui/surfaces";
import { Tabs, TabsList, TabsTrigger } from "~/components/ui/Tabs";
import { TextField, TextFieldInput } from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { STATE_LABEL } from "~/components/StateChip";
import { cn } from "~/lib/cn";
import type { State } from "~/api/types";
import { count as fmtCount } from "~/lib/format";
import {
  MATCHER_EXAMPLES,
  compileMatchers,
  formatMatchers,
  parseMatchers,
  type LabelMatcher,
} from "~/lib/matchers";
import {
  ALL_STATES,
  GROUP_BY_VALUES,
  activeFilterCount,
  withoutMatcher,
  type AlertFilters,
  type GroupBy,
  type SortKey,
} from "./filters";

/** The severities almost everyone uses. Not a closed set — see the note above. */
const COMMON_SEVERITIES = ["critical", "warning", "info"] as const;

/** Relative windows, resolved to an absolute `since` at click time. */
const SINCE_PRESETS = [
  { label: "Any time", hours: null },
  { label: "Last hour", hours: 1 },
  { label: "Last 24 hours", hours: 24 },
  { label: "Last 7 days", hours: 24 * 7 },
  { label: "Last 30 days", hours: 24 * 30 },
] as const;

/** What the "Since" picker shows for a preset label — "Custom" gets the
 *  longer, more honest sentence; every real preset just shows its own label. */
const CUSTOM_SINCE_LABEL = "Custom (from link)";
function sinceOptionLabel(label: string): string {
  return label === "Custom" ? CUSTOM_SINCE_LABEL : label;
}

/** The two sort orders, spelled out for the picker. */
const SORT_LABEL: Record<SortKey, string> = {
  "-last_seen_at": "Newest activity",
  "-first_seen_at": "Newest first seen",
};
const SORT_OPTIONS: readonly SortKey[] = ["-last_seen_at", "-first_seen_at"];

/*
 * ⛔ THIS TOOLBAR NARROWS FOUR AXES, AND THE THREE IT DOES NOT ARE EACH ABSENT
 * FOR THEIR OWN REASON. Every one of them was a tri-state here once.
 *
 * ⛔ THERE IS NO ACK CONTROL, AND THERE MUST NOT BE ONE HERE. `?ack=` read
 * `alerts.ack_state`, a column that no longer exists: an acknowledgement is a
 * receipt for one firing episode and the Alert outlives its episodes, so the
 * filter answered a question about some earlier firing while reading as a
 * question about this one. The ack facet belongs to the case surface.
 *
 * ⛔ AND THERE IS NO SNOOZED CONTROL, FOR A DIFFERENT REASON. Snooze did not
 * lose its home, it got a better one: it is a TAB, not a facet. A tri-state
 * buried three levels into a popover could not do the one job that makes hiding
 * snoozed alerts safe — say, permanently and at rest, how many are being held
 * back and whether any of them is still firing. `AlertTabs` does that, and two
 * controls for one axis would let the toolbar and the tab bar disagree.
 *
 * ⛔ AND THERE IS NO FLAPPING CONTROL, BECAUSE THE COLUMN BEHIND IT WENT BLIND.
 * `?flapping=` reads `alerts.is_flapping`, which the deleted `flap.score` job
 * derived from the `case.*` lifecycle events inside `flap_window_s`; nothing
 * recomputes the column now, so every row holds whatever that job last wrote. A
 * flap damped by the case retention window W appends none of them — one
 * `case.opened` at the start of the episode and one `case.resolved` at its real
 * end, however many times the signal oscillated in between (ADR 0041 Amendment 1)
 * — so the flag reads false exactly when an alert is flapping. Flap noise is removed at case formation
 * now, which is why nothing here presents flapping as a live signal any more; a
 * filter is the loudest possible claim that a column still means something.
 */

/**
 * The Cluster picker's own "no filter" row — a real entry in its `options`,
 * since Kobalte's `Select` (unlike a native `<select>`) needs every value it
 * can land on, placeholder included, to be one of the options it was given.
 */
interface ClusterOption {
  readonly cluster_key: string;
  readonly display_name: string;
}
const ALL_CLUSTERS: ClusterOption = { cluster_key: "", display_name: "All clusters" };

/**
 * The word "Group" is deliberately absent from the labels. It used to collide
 * with a `/groups` screen elsewhere in the app — the AlertGroup, deleted whole
 * by git-bug 7570090 — and the collision is gone with it, but the labels stay as
 * they are for the other reason they were chosen: a tab strip that already reads
 * as "how is this list arranged" does not need the word repeated in every label.
 *
 * ⛔ THIS CONTROL IS A ROLL-UP AXIS AND NOT A CASE. It buckets whatever the
 * current query returned (`GET /api/v1/alerts/rollups`) — no row of its own, no
 * timeline, nothing to acknowledge — so nothing here may borrow the word Case,
 * which names the firing episode on `/cases`.
 */
const GROUP_TAB_LABEL: Record<GroupBy, string> = {
  none: "All",
  alertname: "By alert name",
  namespace: "By namespace",
  fingerprint: "By fingerprint",
};

/* -------------------------------------------------------------------------- */
/* The matcher chip, and the small pure helpers around it                     */
/* -------------------------------------------------------------------------- */

/** A single matcher's canonical spelling — what a chip shows and what a click
 * to edit puts back into the input, so a typo is fixed the same way it was
 * typed. */
function chipText(m: LabelMatcher): string {
  return `${m.name}${m.op}${JSON.stringify(m.value)}`;
}

/**
 * Append matchers to `matcherText`, deduplicating an exact repeat.
 *
 * The same shape as `withMatcher`/`withoutMatcher` in `filters.ts` (canonical
 * re-render via `formatMatchers`, dedup by structural equality), generalised to
 * any operator and to committing several matchers from one `{...}` group at
 * once — which is what a chip commit needs and `withMatcher`'s `=`-only,
 * one-at-a-time shape does not cover.
 */
function appendMatchers(f: AlertFilters, ms: readonly LabelMatcher[]): AlertFilters {
  const existing = parseMatchers(f.matcherText).matchers;
  const merged = [...existing];
  for (const m of ms) {
    if (!merged.some((e) => e.name === m.name && e.op === m.op && e.value === m.value)) {
      merged.push(m);
    }
  }
  return merged.length === existing.length ? f : { ...f, matcherText: formatMatchers(merged) };
}

/**
 * The maximal trailing "content" run in `text` — the last thing typed that is
 * not itself a separator — with quotes and `{...}` groups tracked so a space
 * inside a quoted value or a brace group is never mistaken for the boundary
 * between two matchers.
 */
function extractTrailingToken(text: string): { readonly head: string; readonly token: string } {
  const isSep = (ch: string): boolean => ch === "," || /\s/.test(ch);
  let quote: '"' | "'" | null = null;
  let depth = 0;
  // Every index gets classified exactly once — an escape pair inside a quote
  // advances `i` by two in one step, so it must mark both positions itself
  // rather than let the loop's own increment silently skip the second one.
  const kind: ("sep" | "content")[] = new Array(text.length).fill("content");

  let i = 0;
  while (i < text.length) {
    const ch = text[i] as string;
    if (quote !== null) {
      if (ch === "\\" && i + 1 < text.length) {
        i += 2;
        continue;
      }
      if (ch === quote) quote = null;
      i += 1;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      i += 1;
      continue;
    }
    if (ch === "{") {
      depth += 1;
      i += 1;
      continue;
    }
    if (ch === "}") {
      depth = Math.max(0, depth - 1);
      i += 1;
      continue;
    }
    kind[i] = depth === 0 && isSep(ch) ? "sep" : "content";
    i += 1;
  }

  let end = kind.length;
  while (end > 0 && kind[end - 1] === "sep") end -= 1;
  let start = end;
  while (start > 0 && kind[start - 1] === "content") start -= 1;
  return { head: text.slice(0, start), token: text.slice(start, end) };
}

/**
 * Peel every matcher-shaped token off the end of `text`, so `ns="a", sev="b" `
 * — typed as one continuous phrase, comma or not — commits both at once
 * instead of only the last one. Stops at the first trailing token that is not,
 * on its own, a valid matcher: that is where the free text starts.
 */
function peelTrailingMatchers(text: string): {
  readonly matchers: readonly LabelMatcher[];
  readonly rest: string;
} {
  const matchers: LabelMatcher[] = [];
  let remainder = text;
  for (let guard = 0; guard < 64; guard += 1) {
    const { head, token } = extractTrailingToken(remainder);
    const trimmed = token.trim();
    if (trimmed === "") break;
    // A bare trailing operator with nothing typed after it yet (`namespace=`)
    // parses cleanly to a matcher with an empty value — which is indistinguishable
    // from someone who has not finished typing. Committing that silently is the
    // exact "typo becomes an outage" failure this file exists to prevent, so a
    // dangling operator is treated as still-being-typed, not a completed matcher.
    // An intentional empty-string match (`namespace=""`) is unaffected: it ends in
    // a quote character, not the operator itself.
    if (/[=!~]$/.test(trimmed)) break;
    const parsed = parseMatchers(trimmed);
    if (parsed.errors.length > 0 || parsed.matchers.length === 0) break;
    matchers.unshift(...parsed.matchers);
    remainder = head;
  }
  return { matchers, rest: remainder };
}

/**
 * Say the committed matchers back in English.
 *
 * Duplicated from `MatcherInput`'s own `describe` rather than imported:
 * `MatcherInput` still stands alone for `PoliciesSection`'s policy-matcher
 * field, out of this change's scope, and that helper was never part of its
 * exported contract. Repeated names OR and distinct names AND (§E.3), an
 * asymmetry worth stating rather than documenting.
 */
function describeMatchers(matchers: readonly LabelMatcher[]): string {
  const byName = new Map<string, number>();
  for (const m of matchers) byName.set(m.name, (byName.get(m.name) ?? 0) + 1);
  const ored = [...byName.entries()].filter(([, n]) => n > 1).map(([name]) => name);

  const base = `${matchers.length} matcher${matchers.length === 1 ? "" : "s"}`;
  if (ored.length === 0) return `${base}, all of which must match`;
  return `${base} — values for ${ored.join(", ")} are OR-ed, everything else AND-ed`;
}

/** A Tier-A problem mark. Deliberately not a state hue: a typo is not an alert. */
const MarkGlyph: Component = () => (
  <svg viewBox="0 0 12 12" class="mt-0.5 size-3 shrink-0 text-ink-subtle" aria-hidden="true">
    <circle cx="6" cy="6" r="4.6" fill="none" stroke="currentColor" stroke-width="1.2" />
    <path d="M6 3.6v3M6 8.2v.6" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
  </svg>
);

const MatcherChip: Component<{
  readonly matcher: LabelMatcher;
  readonly onEdit: () => void;
  readonly onRemove: () => void;
}> = (props) => {
  const text = (): string => chipText(props.matcher);
  return (
    <Chip mono class="shrink-0 gap-2xs">
      {/* `onMouseDown` here has to win the race against the input's own
          `onBlur`: a mousedown on this button moves focus away from the input
          before the click fires, and that blur commits whatever draft is
          sitting in the input — which can rewrite `matcherText` and, with it,
          every chip's identity, unmounting this very button out from under
          the click that was already in flight. Blocking the browser's default
          focus-shift-on-mousedown keeps focus (and the chip list) put until
          the click actually lands. */}
      <button
        type="button"
        class="min-w-0 max-w-[12rem] truncate"
        title="Click to edit this matcher"
        onMouseDown={(e) => e.preventDefault()}
        onClick={props.onEdit}
      >
        {text()}
      </button>
      <button
        type="button"
        class="shrink-0 rounded-chip px-0.5 text-ink-subtle hover:text-ink"
        aria-label={`Remove ${text()}`}
        onMouseDown={(e) => e.preventDefault()}
        onClick={(e) => {
          e.stopPropagation();
          props.onRemove();
        }}
      >
        ×
      </button>
    </Chip>
  );
};

/* -------------------------------------------------------------------------- */
/* The merged search box                                                      */
/* -------------------------------------------------------------------------- */

const SearchBox: Component<{
  readonly filters: AlertFilters;
  readonly onChange: (next: AlertFilters) => void;
  /** Whether this deployment's Postgres has `pg_trgm` enabled (`SearchDTO` on
   *  `GET /api/v1/me`) — told to the operator only where the teaching copy
   *  already lives, not as permanent chrome: a capability worth knowing when
   *  you reach for the box, not a fact worth a badge forever. Read by the
   *  caller, not here, so this component stays provider-free and testable
   *  standalone — the same reason `totalCountLabel` arrives as a prop. */
  readonly partialMatchEnabled?: boolean | undefined;
}> = (props) => {
  const [draft, setDraft] = createSignal(props.filters.q);
  const [focused, setFocused] = createSignal(false);
  let inputRef: HTMLInputElement | undefined;

  const names = useQuery(() => labelNamesQuery());

  const committedParsed = createMemo(() => parseMatchers(props.filters.matcherText));
  const committedCompiled = createMemo(() => compileMatchers(committedParsed().matchers));

  /** Does the draft even look like an attempt at a matcher? Gates the live
   *  preview below so ordinary search words are never run through the parser
   *  looking for a reason to complain. */
  const draftLooksLikeAttempt = createMemo(() => {
    const t = draft().trim();
    return t !== "" && (t.startsWith("{") || /[=!~]/.test(t));
  });

  // A live preview of whatever is still being typed, so a regex the server
  // would refuse is explained *before* Space/Enter commits it, not only after —
  // the same promise the single matcher box used to make.
  const previewParsed = createMemo(() =>
    focused() && draftLooksLikeAttempt() ? parseMatchers(draft()) : null,
  );
  const previewCompiled = createMemo(() => {
    const pp = previewParsed();
    return pp ? compileMatchers(pp.matchers) : null;
  });

  const errors = createMemo(() => [...committedParsed().errors, ...(previewParsed()?.errors ?? [])]);
  const rejected = createMemo(() => [
    ...committedCompiled().rejected,
    ...(previewCompiled()?.rejected ?? []),
  ]);
  const hasProblem = (): boolean => errors().length > 0 || rejected().length > 0;

  /** Show the worked examples while the box is focused and wholly empty — no
   *  chips committed, nothing drafted either. */
  const teaching = (): boolean =>
    focused() && committedParsed().matchers.length === 0 && draft().trim() === "";

  const summary = createMemo((): string | null => {
    const parts: string[] = [];
    const ms = committedParsed().matchers;
    if (ms.length > 0 && errors().length === 0 && rejected().length === 0) {
      parts.push(describeMatchers(ms));
    }
    const trimmed = draft().trim();
    if (trimmed !== "") {
      const words = trimmed.split(/\s+/).length;
      parts.push(`${words} word${words === 1 ? "" : "s"} → search text`);
    }
    return parts.length > 0 ? parts.join(" · ") : null;
  });

  // The committed value is authoritative whenever nobody is typing: a Clear, a
  // Back button, or another chip's edit all have to win over whatever a box
  // nobody is looking at was last showing.
  createEffect(() => {
    if (!focused()) setDraft(props.filters.q);
  });

  const commitMatchers = (ms: readonly LabelMatcher[]): void => {
    if (ms.length === 0) return;
    props.onChange(appendMatchers(props.filters, ms));
  };

  const removeAt = (i: number): void => props.onChange(withoutMatcher(props.filters, i));

  const editAt = (i: number): void => {
    const m = committedParsed().matchers[i];
    if (m === undefined) return;
    props.onChange(withoutMatcher(props.filters, i));
    // Order matters: `focused` has to flip before the real DOM focus event
    // fires, or the sync effect above overwrites the draft we are about to set.
    setFocused(true);
    setDraft(chipText(m));
    inputRef?.focus();
  };

  /** Whatever is left after any trailing matchers are peeled off becomes `q`. */
  const applyRest = (rest: string): void => {
    const q = rest.trim();
    setDraft(q);
    if (q !== props.filters.q) props.onChange({ ...props.filters, q });
  };

  const listId = "alert-q-names";
  const statusId = "alert-q-status";

  /**
   * Whether the region below the box has anything to say right now.
   *
   * It gates the *card*, never the region — see the region itself for why that
   * distinction is the whole design.
   */
  const speaking = (): boolean =>
    errors().length > 0 || rejected().length > 0 || (focused() && summary() !== null) || teaching();

  return (
    /* `relative`, because everything this box has to say is said in a layer
       above the table rather than in the flow above it — see the region below. */
    <div class="relative min-w-0">
      <label id="alert-q-label" for="alert-q" class="sr-only-focusable">
        Search alerts, or type a label matcher
      </label>
      <div
        class={cn(
          "flex min-h-8 min-w-0 flex-wrap items-center gap-2xs rounded-control border bg-surface px-xs py-2xs",
          hasProblem() ? "border-line-strong ring-1 ring-accent-border" : "border-line-strong",
        )}
      >
        <For each={committedParsed().matchers}>
          {(m, i) => (
            <MatcherChip matcher={m} onEdit={() => editAt(i())} onRemove={() => removeAt(i())} />
          )}
        </For>

        <TextField
          class="min-w-[8rem] flex-1"
          value={draft()}
          onChange={setDraft}
          validationState={hasProblem() ? "invalid" : "valid"}
          aria-label="Alert search"
        >
          <TextFieldInput
            ref={(el) => (inputRef = el)}
            id="alert-q"
            type="text"
            list={listId}
            placeholder={
              committedParsed().matchers.length === 0
                ? 'Search, or namespace="payments"…'
                : "Add another matcher, or search…"
            }
            spellcheck={false}
            autocapitalize="off"
            autocorrect="off"
            aria-describedby={statusId}
            class="h-auto w-full border-0 bg-transparent p-0"
            onFocus={() => setFocused(true)}
            onBlur={(e) => {
              setFocused(false);
              const { matchers, rest } = peelTrailingMatchers(e.currentTarget.value);
              commitMatchers(matchers);
              applyRest(rest);
            }}
            onKeyDown={(e) => {
              if (e.key === " ") {
                const { matchers, rest } = peelTrailingMatchers(e.currentTarget.value);
                if (matchers.length > 0) {
                  e.preventDefault();
                  commitMatchers(matchers);
                  setDraft(rest);
                }
                return;
              }
              if (e.key === "Enter") {
                e.preventDefault();
                const { matchers, rest } = peelTrailingMatchers(e.currentTarget.value);
                commitMatchers(matchers);
                applyRest(rest);
                return;
              }
              if (e.key === "Escape") {
                // Nothing unsaved to revert — Escape's job is then to clear a
                // committed search outright, same as it always has.
                if (draft() === props.filters.q && props.filters.q !== "") {
                  setDraft("");
                  props.onChange({ ...props.filters, q: "" });
                } else {
                  setDraft(props.filters.q);
                }
                e.currentTarget.blur();
                return;
              }
              if (e.key === "Backspace" && e.currentTarget.value === "") {
                const n = committedParsed().matchers.length;
                if (n > 0) {
                  e.preventDefault();
                  removeAt(n - 1);
                }
              }
            }}
          />
        </TextField>
      </div>

      {/* A native datalist gives name completion with zero ARIA of our own and
          zero chance of trapping the keyboard. `alert_count` is what orders the
          list server-side, so it is shown: a filter bar that offers a label
          matching nothing spends the one minute of an incident that matters. */}
      <datalist id={listId}>
        <For each={names.data ?? []}>
          {(row) => (
            <option value={`${row.name}="`} label={`${fmtCount(row.alert_count)} alerts`} />
          )}
        </For>
      </datalist>

      {/* `polite`, because a filter is typed, not pushed — interrupting someone
          mid-word is worse than telling them a beat later.

          ⛔ THE REGION IS ALWAYS MOUNTED AND ALWAYS EMPTY-WHEN-SILENT; THE CARD
          INSIDE IT IS WHAT COMES AND GOES. A live region that arrives already
          holding its sentence is one screen readers commonly never speak, and
          the sentence here is "the filter you just typed will not be run".

          ⭐ AND IT FLOATS. In the rail this hung in the flow beneath the input,
          where growing by four lines cost nothing. In a toolbar the same growth
          would push the table down every time somebody focused the box — a list
          that jumps under a reader mid-incident, caused by nothing but a caret
          landing. So it is a layer: absolutely positioned, `top-full`, over the
          rows rather than above them. Nothing below the toolbar moves, ever. */}
      <div id={statusId} aria-live="polite" class="absolute left-0 top-full z-30 mt-2xs min-w-0">
       <Show when={speaking()}>
        <div class="w-[26rem] max-w-[calc(100vw-4rem)] border border-line bg-surface px-sm py-xs shadow-md">
        <Show when={errors().length > 0}>
          <ul class="mt-2xs space-y-0.5">
            <For each={errors()}>
              {(err) => (
                <li class="flex items-start gap-1.5 text-meta leading-snug text-ink">
                  <MarkGlyph />
                  <span>
                    <span class="text-ink-subtle">at {err.at}:</span> {err.message}
                  </span>
                </li>
              )}
            </For>
          </ul>
        </Show>

        <Show when={rejected().length > 0}>
          <ul class="mt-2xs space-y-2xs">
            <For each={rejected()}>
              {(r) => (
                <li class="flex items-start gap-1.5 text-meta leading-snug text-ink">
                  <MarkGlyph />
                  <span>
                    <code class="font-mono text-ink">{chipText(r.matcher)}</code> — {r.reason}
                  </span>
                </li>
              )}
            </For>
          </ul>
        </Show>

        <Show when={focused() && summary() !== null}>
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">{summary()}</p>
        </Show>

        <Show when={teaching()}>
          <ul class="mt-2xs space-y-0.5">
            <For each={MATCHER_EXAMPLES}>
              {(ex) => (
                <li class="flex flex-wrap items-baseline gap-x-2 text-meta leading-snug">
                  <code
                    class={cn(
                      "font-mono",
                      ex.served
                        ? "text-ink"
                        : "text-ink-subtle line-through decoration-ink-subtle/60",
                    )}
                  >
                    {ex.text}
                  </code>
                  <span class="text-ink-subtle">{ex.note}</span>
                </li>
              )}
            </For>
          </ul>

          {/* The raw-text half of "structural and raw" search, told at the
              same moment the structural examples above are — reaching for the
              box is when this is worth knowing, not before. */}
          <p class="mt-2xs text-meta leading-snug text-ink-subtle">
            <Show
              when={props.partialMatchEnabled === true}
              fallback={
                <>
                  Plain words search alertname, summary and description. Substring matches inside a
                  compound name (e.g. "error" in <code class="font-mono">CheckoutErrorRateHigh</code>)
                  aren't available on this deployment — ask an admin about enabling Postgres's{" "}
                  <code class="font-mono">pg_trgm</code> extension.
                </>
              }
            >
              Plain words search alertname, summary and description — including substrings inside a
              compound name, like "error" in <code class="font-mono">CheckoutErrorRateHigh</code>.
            </Show>
          </p>
        </Show>
        </div>
       </Show>
      </div>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* The toolbar's own furniture                                                */
/* -------------------------------------------------------------------------- */

/**
 * ⭐ THE ONE CONTROL RECIPE THIS TOOLBAR HAS, AND IT SPENDS NO ACCENT.
 *
 * Every instrument in the band — the four menu triggers and the three selects —
 * wears exactly this, so a row of seven different Kobalte primitives reads as
 * one set of instruments rather than as whatever each library shipped.
 *
 * The active treatment is deliberately **neutral**: a raised surface, the
 * stronger hairline, and a `text-ink-muted` → `text-ink` weight lift. Three
 * channels, no hue, legible in greyscale. The rail's version of this tinted
 * every pressed control with `bg-accent-fill`, which put five accent marks in
 * the chrome of a screen whose entire job is to make one severity mark the
 * loudest thing on it. The accent left in this file is spent once, on the
 * current view — the only control here that answers "where am I" (§0.6, §M.2).
 */
const TRIGGER =
  "inline-flex h-8 shrink-0 items-center gap-2xs rounded-control border px-sm text-item " +
  "transition-colors duration-100";
const TRIGGER_OFF = "border-line bg-transparent text-ink-muted hover:bg-raised hover:text-ink";
const TRIGGER_ON = "border-line-strong bg-raised font-medium text-ink";

/** A full-width control row, inside a menu. */
const ROW =
  "flex h-8 w-full items-center gap-xs rounded-control px-xs text-item transition-colors duration-100";
const ROW_QUIET = "text-ink-muted hover:bg-raised hover:text-ink";
/** The file's ONE accent: the view you are currently in, and nothing else. */
const ROW_ACTIVE = "bg-accent-fill text-ink";

/**
 * The menu caret. Points down at rest and flips when the menu is open, so the
 * shape says which way the panel will move — read off the trigger's own
 * `data-expanded`, never off a second copy of the state.
 */
const CaretGlyph: Component = () => (
  <svg
    viewBox="0 0 12 12"
    class={
      "size-3 shrink-0 text-ink-subtle transition-transform duration-100 " +
      "group-data-[expanded]:rotate-180"
    }
    aria-hidden="true"
  >
    <path
      d="M2.6 4.4 6 7.8l3.4-3.4"
      fill="none"
      stroke="currentColor"
      stroke-width="1.4"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);

/**
 * One axis, behind a menu — and saying on its face what it is doing.
 *
 * `value` is the whole design (see this file's header on what the popover
 * costs): `undefined` means the axis is not narrowing anything and the trigger
 * shows only its name, quietly; a string means it is, and the trigger lifts and
 * says what to. The axis name stays `text-ink-subtle` in both cases, so the
 * *value* is what the eye lands on — hierarchy from weight and tone, at one
 * size, because there is only one size a 32 px control can hold.
 *
 * The trigger's own text is its accessible name. No `aria-label` is added on
 * top: it would have to repeat the same two words, and a label that drifts from
 * the text beside it is worse than no label at all.
 */
const FilterMenu: ParentComponent<{
  readonly label: string;
  /** What this axis is currently narrowing by, or `undefined` when it is not. */
  readonly value?: string | undefined;
  /** A glyph from the §0.3 alphabet, drawn in Tier A ink. Optional. */
  readonly leading?: JSX.Element;
  /** The long form, for a hover. The trigger itself can only afford a summary. */
  readonly title?: string | undefined;
}> = (props) => (
  <Popover>
    <PopoverTrigger
      class={cn("group", TRIGGER, props.value === undefined ? TRIGGER_OFF : TRIGGER_ON)}
      title={props.title}
    >
      {props.leading}
      <span class={cn("font-normal", props.value === undefined ? "" : "text-ink-subtle")}>
        {props.label}
      </span>
      {/* ⛔ A REAL SPACE, NOT THE FLEX GAP. `gap-2xs` separates these two spans
          on screen and not at all in the accessibility tree, where the name
          would concatenate to "Severitycritical +1". A whitespace-only text run
          is not rendered as a flex item (CSS Flexbox §4), so this costs no
          layout and buys the trigger a name a screen reader can say. */}
      {" "}
      <Show when={props.value}>
        {(value) => <span class="max-w-[9rem] truncate">{value()}</span>}
      </Show>
      <CaretGlyph />
    </PopoverTrigger>
    {/* `p-md` and a column of `gap-md` bands: a menu holding three axes needs
        the axes to read as three things, and the only separator used here is
        space — a hairline between every pair would draw more ink than the
        controls do. */}
    <PopoverContent class="w-72 p-md">
      <div class="flex flex-col gap-md">{props.children}</div>
    </PopoverContent>
  </Popover>
);

/**
 * A labelled band inside a menu.
 *
 * The label is the quietest type in the app (`text-micro`, `text-ink-subtle`)
 * on purpose: each axis is named exactly once, above the control that narrows
 * it, so nothing inside has to repeat the word.
 */
const MenuSection: ParentComponent<{ readonly label: string }> = (props) => (
  <div class="flex min-w-0 flex-col gap-2xs">
    <h3 class={cn(SECTION_LABEL, "flex items-center gap-2xs text-ink-subtle")}>{props.label}</h3>
    {props.children}
  </div>
);

/**
 * The saved views.
 *
 * Each is a *statement about three axes* rather than a stored filter blob, so a
 * view stays legible when the operator has also typed a matcher or picked a
 * cluster: switching to "Firing" narrows the lifecycle and leaves everything
 * else exactly where it was. `matches` is what lights the row up, and it is
 * deliberately strict — a view claims to be current only when those three axes
 * say what it says, never merely "close enough".
 */
interface ViewPreset {
  readonly id: string;
  readonly label: string;
  readonly matches: (f: AlertFilters) => boolean;
  readonly apply: (f: AlertFilters) => AlertFilters;
}

const VIEW_PRESETS: readonly ViewPreset[] = [
  {
    id: "all",
    label: "All",
    matches: (f) => f.state.length === 0,
    apply: (f) => ({ ...f, state: [] }),
  },
  {
    id: "firing",
    label: "Firing",
    matches: (f) => f.state.length === 1 && f.state[0] === "firing",
    apply: (f) => ({ ...f, state: ["firing"] }),
  },
  // ⛔ THE `Snoozed` VIEW IS GONE AND IS NOT COMING BACK HERE. It set
  // `snoozed: true` on the same list, which is now a TAB — and a preset that
  // silently moved the operator to a different tab while the tab bar carried on
  // showing the other one is the disagreement `AlertTabs` exists to prevent.
];

/**
 * One applied filter, said in words and undoable on its own.
 *
 * ⭐ IN THE TOOLBAR THESE ARE FOR THE ORPHANS ONLY. Every axis with a control
 * says its own value on its own trigger (see `FilterMenu`), so a pill repeating
 * `Severity: critical` beside a trigger already reading `Severity critical`
 * would be two controls for one fact — the exact confusion the merged search
 * box exists to end. `namespace` and `alertname` have no control anywhere: they
 * arrive from a roll-up drill-down, and without a pill they are invisible and
 * unremovable except by editing the URL. So for them this is not a second
 * telling, it is the only one, and it is the only case that gets a pill.
 */
interface AppliedFilter {
  readonly id: string;
  readonly label: string;
  readonly next: AlertFilters;
}

const AppliedChip: Component<{
  readonly label: string;
  readonly onRemove: () => void;
}> = (props) => (
  <Chip class="max-w-full gap-2xs" title={props.label}>
    <span class="min-w-0 truncate">{props.label}</span>
    <button
      type="button"
      class="shrink-0 rounded-chip px-0.5 text-ink-subtle hover:text-ink"
      aria-label={`Remove ${props.label}`}
      onClick={props.onRemove}
    >
      ×
    </button>
  </Chip>
);

/* -------------------------------------------------------------------------- */
/* Grouping, as tabs                                                          */
/* -------------------------------------------------------------------------- */

/**
 * A manual-activation tab strip (ARIA APG), deliberately not automatic — moving
 * focus with the arrow keys must never fire a request. Selecting an axis reads
 * `GET /api/v1/alerts/rollups` instead of `GET /api/v1/alerts`; automatic
 * activation would fire one of those per arrow key press while someone merely
 * tabs through the list of axes.
 *
 * ⛔ THE ORIENTATION STAYS HORIZONTAL even though the strip is laid out as a
 * column — inside the Group menu, where "By alert name" and "By namespace" are
 * far too long to sit side by side in a 288 px panel. Kobalte derives the
 * arrow-key axis from `orientation`, and its `TabsKeyboardDelegate` answers
 * `getKeyRightOf` with `undefined` under `vertical` — so declaring the visual
 * truth here would silently retire ArrowLeft/ArrowRight, which is the
 * roving-focus contract `FilterBar.test.tsx` pins ("moves focus on the arrow
 * keys without activating"). The list is a column driven with ←/→; that
 * mismatch is the smaller of the two, and it is stated here rather than left to
 * be discovered.
 */
const GroupingTabs: Component<{
  readonly value: GroupBy;
  readonly onChange: (next: GroupBy) => void;
  /** The flat-list total, already formatted with its "+" qualifier when more
   *  pages remain unloaded — shown only on the "All" tab. */
  readonly totalCountLabel?: string | undefined;
}> = (props) => {
  return (
    <Tabs value={props.value} onChange={(v) => props.onChange(v as GroupBy)} activationMode="manual">
      <TabsList
        aria-label="Group alerts by"
        class="-mx-xs flex h-auto flex-col items-stretch gap-0 rounded-none bg-transparent p-0"
      >
        <For each={GROUP_BY_VALUES}>
          {(g) => (
            <TabsTrigger
              value={g}
              class={cn(
                ROW,
                "justify-start font-normal",
                ROW_QUIET,
                // Neutral, like every other selected thing in this file except
                // the view rows: a raised surface and the text lift, no tint.
                "data-[selected]:border data-[selected]:border-line-strong",
                "data-[selected]:bg-raised data-[selected]:font-medium data-[selected]:text-ink",
                "data-[selected]:shadow-none",
              )}
            >
              <span class="min-w-0 flex-1 truncate text-left">{GROUP_TAB_LABEL[g]}</span>
              <Show when={g === "none" && props.totalCountLabel !== undefined}>
                <span class="shrink-0 tabular-nums text-meta text-ink-subtle">
                  {props.totalCountLabel}
                </span>
              </Show>
            </TabsTrigger>
          )}
        </For>
      </TabsList>
    </Tabs>
  );
};

/* -------------------------------------------------------------------------- */
/* The panel itself                                                           */
/* -------------------------------------------------------------------------- */

/* -------------------------------------------------------------------------- */
/* The toolbar itself                                                         */
/* -------------------------------------------------------------------------- */

export interface FilterBarProps {
  readonly filters: AlertFilters;
  readonly onChange: (next: AlertFilters) => void;
  readonly onReset: () => void;
  /** The flat-list total, pre-formatted with its "+" qualifier, shown on the
   *  "All" tab only. `undefined` while a roll-up axis is active. */
  readonly totalCountLabel?: string | undefined;
  /** See `SearchBox`'s own prop of the same name. */
  readonly partialMatchEnabled?: boolean | undefined;
}

/**
 * How many of the three bars a severity inks — the §0.3 ruler, reused.
 *
 * `AlertDTO.severity` is a free vocabulary, so anything outside the common
 * three ranks 0 and draws the empty ruler rather than guessing at an order it
 * was never told. The glyph keeps a constant bounding box either way, which is
 * the whole reason the unfilled bars are drawn faint instead of omitted.
 */
const SEVERITY_RANK: Record<string, number> = { critical: 3, warning: 2, info: 1 };
const severityRank = (severity: string): number => SEVERITY_RANK[severity] ?? 0;

/** `first +n`, the only summary a 32 px trigger can hold. */
function summarise(parts: readonly string[]): string | undefined {
  const [first, ...rest] = parts;
  if (first === undefined) return undefined;
  return rest.length === 0 ? first : `${first} +${rest.length}`;
}

/**
 * The band of instruments above the alert table.
 *
 * Deliberately layout-neutral in the horizontal axis: no gutter of its own
 * beyond the `px-md` that lines its first control up with the table's first
 * column, and no width. The content column supplies the rest, exactly as it
 * does for the table below.
 */
export const AlertFilterToolbar: Component<FilterBarProps> = (props) => {
  const clusters = useQuery(() => clustersQuery());

  const clusterOptions = createMemo<readonly ClusterOption[]>(() => [
    ALL_CLUSTERS,
    ...(clusters.data?.data ?? []),
  ]);

  const patch = (part: Partial<AlertFilters>): void => {
    props.onChange({ ...props.filters, ...part });
  };

  /** Severities present in the URL that are not one of the common three. */
  const customSeverities = createMemo(() =>
    props.filters.severity.filter((s) => !(COMMON_SEVERITIES as readonly string[]).includes(s)),
  );

  const sincePreset = (): string => {
    if (props.filters.since === null) return "Any time";
    const target = new Date(props.filters.since).getTime();
    const hours = (Date.now() - target) / 3_600_000;
    const match = SINCE_PRESETS.find((p) => p.hours !== null && Math.abs(p.hours - hours) < 0.2);
    return match?.label ?? "Custom";
  };

  const activeView = createMemo<string | null>(
    () => VIEW_PRESETS.find((v) => v.matches(props.filters))?.id ?? null,
  );

  /* ---- what each trigger says about itself ------------------------------- */

  /**
   * ⭐ THE FOUR LINES THAT PAY FOR THE POPOVERS.
   *
   * Everything the Status menu can narrow, in words, at rest. A trigger that
   * only said "Status" would make "why is this list empty?" a click away, which
   * is the one thing the rail's always-open sections were protecting and the
   * one thing a toolbar cannot buy with space.
   */
  const statusParts = createMemo<readonly string[]>(() => {
    const f = props.filters;
    const out: string[] = [];
    for (const s of f.state) out.push(STATE_LABEL[s]);
    return out;
  });

  /**
   * A view is three axes agreeing, so when one matches exactly it is the truer
   * thing to show: "Firing" says what `Firing +1` only implies. `all` is the
   * default and narrows nothing, so it never lights the trigger.
   */
  const statusValue = (): string | undefined => {
    const view = VIEW_PRESETS.find((v) => v.id !== "all" && v.matches(props.filters));
    if (view !== undefined) return view.label;
    return summarise(statusParts());
  };

  const statusTitle = (): string | undefined => {
    const parts = statusParts();
    return parts.length > 0 ? parts.join(" · ") : undefined;
  };

  const severityValue = (): string | undefined => summarise(props.filters.severity);

  /** The ruler on the trigger reads the *worst* severity being filtered for. */
  const severityMark = (): number =>
    props.filters.severity.reduce((worst, s) => Math.max(worst, severityRank(s)), 0);

  const clusterOption = createMemo<ClusterOption>(
    () =>
      clusterOptions().find((c) => c.cluster_key === (props.filters.cluster[0] ?? "")) ??
      ALL_CLUSTERS,
  );

  const clusterValue = (): string | undefined =>
    props.filters.cluster.length === 0 ? undefined : clusterOption().display_name;

  const sinceValue = (): string | undefined =>
    props.filters.since === null ? undefined : sincePreset();

  const groupValue = (): string | undefined => {
    const g = props.filters.groupBy;
    if (g === "none") return undefined;
    const label = GROUP_TAB_LABEL[g];
    // The tab says "By namespace" because it sits under a "Group by" heading;
    // the trigger already says "Group", so it takes the axis alone.
    return label.startsWith("By ") ? label.slice(3) : label;
  };

  const sortValue = (): string => SORT_LABEL[props.filters.sort];

  /**
   * The two axes with no control anywhere — see `AppliedFilter`.
   *
   * They arrive from a roll-up drill-down, so without these pills they are
   * invisible and unremovable except by editing the URL.
   */
  const orphans = createMemo<readonly AppliedFilter[]>(() => {
    const f = props.filters;
    const out: AppliedFilter[] = [];
    for (const ns of f.namespace) {
      out.push({
        id: `namespace:${ns}`,
        label: `Namespace: ${ns}`,
        next: { ...f, namespace: f.namespace.filter((v) => v !== ns) },
      });
    }
    for (const name of f.alertname) {
      out.push({
        id: `alertname:${name}`,
        label: `Alert name: ${name}`,
        next: { ...f, alertname: f.alertname.filter((v) => v !== name) },
      });
    }
    return out;
  });

  /**
   * One recipe for the three selects that sit in the band rather than in a menu.
   *
   * ⭐ AND ONE CARET FOR THE WHOLE BAND. `SelectTrigger` appends Kobalte's own
   * up/down chevron pair after its children; beside `FilterMenu`'s single caret
   * that reads as two different idioms in one row of seven controls, which is
   * the kind of seam nobody can name and everybody sees. The stock icon is
   * hidden at this call site — never in `Select.tsx`, which settings and the
   * dialogs share — and `CaretGlyph` is rendered in its place. `group` is what
   * lets that glyph read the trigger's own `data-expanded`.
   *
   * ⛔ AND NO `aria-label` ON ANY OF THE THREE. One would override the trigger's
   * text outright, and the text is where the *value* is — "Cluster" as an
   * accessible name is a control that has stopped saying what it is filtering
   * by, to precisely the reader who cannot see the word beside it. The visible
   * text is the accessible name here, exactly as it is on `FilterMenu`.
   */
  const selectClass = (on: boolean): string =>
    cn(
      "group [&>svg:last-child]:hidden",
      TRIGGER,
      "w-auto justify-start",
      on ? TRIGGER_ON : TRIGGER_OFF,
    );

  return (
    <div class="shrink-0">
      {/* ⭐ ONE BAND, `items-start`, AND IT WRAPS.
          `items-start` because the search box is the only control here that can
          grow — commit four matchers and it becomes two lines of chips — and a
          row that centres would then float every button down beside it. It
          wraps rather than scrolls or truncates: at a narrow window the
          instruments stack, which costs a line of height, where an overflowing
          toolbar would cost whichever axis fell off the right edge.

          `px-md` is the table's own cell inset, so the search box's left edge
          and the first column's text sit on one line down the screen. */}
      <div class="flex flex-wrap items-start gap-xs px-md py-sm">
        {/* The one control that must never cost a click to reach: it is the
            first thing anyone types into at 3am, so it leads the band, takes
            the elastic width, and is the only thing here drawn on a filled
            surface. `max-w` stops it from eating the whole row on a wide
            monitor and leaving the instruments stranded at the far right. */}
        <div class="min-w-[15rem] max-w-[34rem] flex-1">
          <SearchBox
            filters={props.filters}
            onChange={props.onChange}
            partialMatchEnabled={props.partialMatchEnabled}
          />
        </div>

        {/* ---- what to look through ------------------------------------- */}
        <div class="flex flex-wrap items-center gap-xs">
          <FilterMenu label="Status" value={statusValue()} title={statusTitle()}>
            {/* The views first, because they are the gesture, and the axes they
                are made of underneath — so picking "Firing" and then loosening
                one axis is one continuous motion instead of two mental models.
                This is the file's only accent: it marks where you are. */}
            <MenuSection label="View">
              <div class="-mx-xs flex flex-col">
                <For each={VIEW_PRESETS}>
                  {(view) => (
                    <button
                      type="button"
                      aria-current={activeView() === view.id ? "true" : undefined}
                      onClick={() => props.onChange(view.apply(props.filters))}
                      class={cn(ROW, activeView() === view.id ? ROW_ACTIVE : ROW_QUIET)}
                    >
                      <span class="min-w-0 flex-1 truncate text-left">{view.label}</span>
                    </button>
                  )}
                </For>
              </div>
            </MenuSection>

            {/* U1: never colour alone — and here, never colour at all. The mark
                is the §0.3 state shape drawn `tone="inherit"`, so the toggle
                carries the same alphabet the table's rows do without spending a
                Tier B hue on chrome (§M.2). */}
            <MenuSection label="Lifecycle state">
              <ToggleGroup
                legend="Lifecycle state"
                multiple
                value={[...props.filters.state]}
                onChange={(next) => patch({ state: next as State[] })}
              >
                <For each={ALL_STATES}>
                  {(s) => (
                    <ToggleGroupItem value={s} class="gap-2xs">
                      <StateGlyph state={s} tone="inherit" />
                      {STATE_LABEL[s]}
                    </ToggleGroupItem>
                  )}
                </For>
              </ToggleGroup>
            </MenuSection>
          </FilterMenu>

          {/* Severity gets its own trigger rather than a line in Status, because
              it is the axis this product is *about*: the ruler on the face of it
              is the same one every row in the table below carries, filled to the
              worst severity being filtered for. */}
          <FilterMenu
            label="Severity"
            value={severityValue()}
            leading={<SeverityBars filled={severityMark()} class="size-3" />}
          >
            <ToggleGroup
              legend="Severity"
              multiple
              value={[...props.filters.severity]}
              onChange={(next) => patch({ severity: next })}
            >
              <For each={COMMON_SEVERITIES}>
                {(s) => (
                  <ToggleGroupItem value={s} class="gap-2xs">
                    <SeverityBars filled={severityRank(s)} />
                    {s}
                  </ToggleGroupItem>
                )}
              </For>
              {/* A deployment using `sev1` sees its own word back rather than
                  having it silently dropped — but no invented rank: the ruler
                  stays empty for a severity nobody told us how to order. */}
              <For each={customSeverities()}>
                {(s) => (
                  <ToggleGroupItem value={s} class="gap-2xs">
                    <SeverityBars filled={severityRank(s)} />
                    {s}
                  </ToggleGroupItem>
                )}
              </For>
            </ToggleGroup>
          </FilterMenu>

          {/* Cluster and Since are already listboxes, so they sit in the band
              directly: putting a real `Select` inside a `Popover` would be a
              menu inside a menu for nothing. Their trigger wears the same
              recipe as the menus, and names its axis the same way — the word
              quiet, the value carrying the weight. */}
          <Show when={(clusters.data?.data.length ?? 0) > 0}>
            <Select<ClusterOption>
              options={[...clusterOptions()]}
              optionValue="cluster_key"
              optionTextValue="display_name"
              value={clusterOption()}
              onChange={(next) => {
                if (next === null) return;
                patch({ cluster: next.cluster_key === "" ? [] : [next.cluster_key] });
              }}
              itemComponent={(itemProps) => (
                <SelectItem item={itemProps.item}>{itemProps.item.rawValue.display_name}</SelectItem>
              )}
            >
              <SelectTrigger
                id="alert-cluster"
                class={selectClass(clusterValue() !== undefined)}
              >
                <span class={cn("font-normal", clusterValue() === undefined ? "" : "text-ink-subtle")}>
                  Cluster
                </span>{" "}
                <Show when={clusterValue()}>
                  {(value) => <span class="max-w-[9rem] truncate">{value()}</span>}
                </Show>
                <CaretGlyph />
              </SelectTrigger>
              <SelectHiddenSelect />
              <SelectContent />
            </Select>
          </Show>

          <Select<string>
            options={
              sincePreset() === "Custom"
                ? [...SINCE_PRESETS.map((p) => p.label), "Custom"]
                : SINCE_PRESETS.map((p) => p.label)
            }
            optionTextValue={sinceOptionLabel}
            value={sincePreset()}
            onChange={(label) => {
              if (label === null) return;
              const preset = SINCE_PRESETS.find((p) => p.label === label);
              if (!preset) return;
              patch({
                since:
                  preset.hours === null
                    ? null
                    : new Date(Date.now() - preset.hours * 3_600_000).toISOString(),
              });
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{sinceOptionLabel(itemProps.item.rawValue)}</SelectItem>
            )}
          >
            <SelectTrigger
              id="alert-since"
              class={selectClass(sinceValue() !== undefined)}
              title="Lower bound on last activity."
            >
              <span class={cn("font-normal", sinceValue() === undefined ? "" : "text-ink-subtle")}>
                Since
              </span>{" "}
              <Show when={sinceValue()}>
                {(value) => <span class="max-w-[9rem] truncate">{value()}</span>}
              </Show>
              <CaretGlyph />
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>

        {/* ---- how to arrange it ----------------------------------------
            A second group, pushed to the far edge, because these three do not
            narrow anything: nothing here changes *which* alerts are on screen,
            only how they are stacked, ordered and spaced. Putting them beside
            the filters would make "Group by namespace" look like a filter, and
            an operator who read it that way would think alerts were missing. */}
        <div class="ml-auto flex flex-wrap items-center gap-xs">
          <FilterMenu label="Group" value={groupValue()}>
            <GroupingTabs
              value={props.filters.groupBy}
              onChange={(g) => patch({ groupBy: g })}
              totalCountLabel={props.totalCountLabel}
            />
          </FilterMenu>

          {/* Always on, so it always shows its value and never lifts: an
              ordering is not a filter, and a control that looked "active"
              whenever the list was sorted at all would say nothing. */}
          <Select<SortKey>
            options={[...SORT_OPTIONS]}
            optionTextValue={(s) => SORT_LABEL[s]}
            value={props.filters.sort}
            onChange={(next) => {
              if (next !== null) patch({ sort: next });
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{SORT_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectTrigger
              id="alert-sort"
              class={selectClass(false)}
              title="Only two orderings exist, because a keyset cursor needs a total order backed by an index."
            >
              <span class="font-normal text-ink-subtle">Sort</span>{" "}
              <span class="max-w-[9rem] truncate">{sortValue()}</span>
              <CaretGlyph />
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>

          {/* No direction control, because there is no direction to offer: §E.3
              permits exactly two orderings and both are descending, since a
              keyset cursor needs a total order backed by an index. A pair of
              asc/desc buttons here would be chrome for a request the server
              answers with a 422. */}

          {/* ⛔ NO "DISPLAY" MENU HERE. Row density used to be the fifth control
              in this group, and it was the odd one out: everything else in this
              band is a statement about *this list*, and density is a statement
              about the person — it is written to `localStorage`, it applies to
              every table in the product, and it outlives the filter set by
              design. It lives in the profile menu at the foot of the rail,
              beside the other two preferences of exactly that kind (ADR 0033).
              Offering it in both places was two controls for one fact. */}

          {/* ⭐ THE COUNT IS NEVER BEHIND ANYTHING. Whatever else a menu is
              hiding, the *number* of axes narrowing this list is on screen at
              rest — which is half of what the always-open sections used to buy
              and the half that matters when the table is empty. */}
          <Show when={activeFilterCount(props.filters) > 0}>
            <Button variant="ghost" size="sm" onClick={props.onReset}>
              Clear {activeFilterCount(props.filters)} filter
              {activeFilterCount(props.filters) === 1 ? "" : "s"}
            </Button>
          </Show>
        </div>
      </div>

      {/* The orphans, on their own line and only when there are any — see
          `AppliedFilter`. A row that appeared for every filter would push the
          table down on the most ordinary gesture there is; these two only ever
          arrive by drilling into a roll-up, which is already a navigation. */}
      <Show when={orphans().length > 0}>
        <div class="flex flex-wrap items-center gap-2xs px-md pb-sm">
          <For each={orphans()}>
            {(chip) => (
              <AppliedChip label={chip.label} onRemove={() => props.onChange(chip.next)} />
            )}
          </For>
        </div>
      </Show>
    </div>
  );
};

/**
 * The public name, unchanged for every call site.
 *
 * There is nothing left to decide here — the sections used to be handed to the
 * shell's rail or wrapped in an `<aside>` depending on where they were mounted,
 * and both homes are gone. The toolbar renders where it is written, inside the
 * screen's own column, in the shell and in `/proto/alerts-preview` alike.
 */
export const FilterBar: Component<FilterBarProps> = (props) => <AlertFilterToolbar {...props} />;
