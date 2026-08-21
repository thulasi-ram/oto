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
 * carry, and the state marks beside the lifecycle checkboxes are the same six
 * shapes — but every one of them is drawn `tone="inherit"`, in Tier A ink. The
 * *shape* is the vocabulary the chrome may borrow; the *hue* is state's alone,
 * and lending it to a filter chip is exactly how scarcity is spent.
 *
 * ⭐ AND EVERY MULTI-VALUE AXIS IS A COLUMN OF CHECKBOXES, NOT A SEGMENTED STRIP.
 * The pills said "you are in one of these" while the axis meant "narrowed to
 * these", which is a disagreement that shows up the moment two of them are on.
 * The control recipe lives in `~/components/ui/FilterMenu` now and is shared with
 * `/cases`, the alert Timeline, the notification log and the rejections panel —
 * see that file's header for the argument, which was originally this one's.
 *
 * Severity is a free vocabulary, not an enum (`AlertDTO.severity` is
 * deliberately open), so the common three are offered and anything else arriving
 * from a URL is preserved and shown rather than silently dropped.
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
} from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { clustersQuery, labelNamesQuery } from "~/api/queries";
import { SeverityEnso, StateGlyph } from "~/components/glyphs";
import {
  CaretGlyph,
  CheckList,
  ChoiceList,
  FILTER_TRIGGER,
  FILTER_TRIGGER_OFF,
  FILTER_TRIGGER_ON,
  FilterMenu,
  MenuSection,
  MENU_ROW,
  MENU_ROW_ACTIVE,
  MENU_ROW_QUIET,
  summarise,
} from "~/components/ui/FilterMenu";
import {
  Select,
  SelectContent,
  SelectHiddenSelect,
  SelectItem,
  SelectTrigger,
} from "~/components/ui/Select";
import { Button } from "~/components/ui/Button";
import { Chip } from "~/components/ui/surfaces";
import { TextField, TextFieldInput } from "~/components/ui/TextField";
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

/*
 * ⭐ THE CONTROL RECIPE, THE MENU, THE CARET, THE MENU SECTION AND `summarise`
 * ALL MOVED TO `~/components/ui/FilterMenu` AND ARE IMPORTED ABOVE.
 *
 * They were invented here, for this toolbar, and they stopped being this
 * toolbar's business the moment the same shape was the right answer on `/cases`,
 * on the alert Timeline, on the notification activity log and in the rejections
 * panel — five screens which between them had four different filter idioms. A
 * private copy per screen is how a product ends up with a segmented strip here, a
 * tab bar there and a dropdown somewhere else, all filtering, none agreeing.
 *
 * Nothing about the *argument* moved: the trigger states its VALUE and not just
 * its axis, `Clear N filters` stays outside every popover, and the two axes with
 * no control keep their pills. That is still this file's header, and it is still
 * what pays for the popovers.
 */

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
/* Grouping, as one of a list                                                  */
/* -------------------------------------------------------------------------- */

/**
 * The roll-up axis, as a column of rows inside the Group menu.
 *
 * ⛔⛔ IT WAS A `role="tablist"` AND IT IS NOT ANY MORE, WHICH IS THE FIX RATHER
 * THAN THE COST. The strip was a manual-activation ARIA tab list, and every word
 * of the paragraph that used to sit here was about fighting the widget: tabs
 * activate on arrow keys, this axis fires a DIFFERENT ENDPOINT when it changes
 * (`/alerts/rollups` rather than `/alerts`), so activation had to be forced to
 * manual — and then the orientation had to be declared `horizontal` while the
 * thing was laid out as a column, because Kobalte's vertical keyboard delegate
 * retires ←/→ and ←/→ was the roving-focus contract the tests pinned. Two
 * deliberate lies to the accessibility tree, to make a tab list behave like a
 * list of buttons.
 *
 * It also *was* a tab list in the one sense that matters: three rows, one lit,
 * inside a popover that already had a caret and a trigger stating its value. A
 * tab strip nested in a dropdown is two navigation metaphors for one choice.
 *
 * `ChoiceList` is the plain thing: one focus stop per row, `aria-current` on the
 * live one, activation on click/Enter/Space only and never on a cursor key. No
 * orientation to misdeclare, and no request fired while somebody walks the list.
 */
const GroupingChoices: Component<{
  readonly value: GroupBy;
  readonly onChange: (next: GroupBy) => void;
  /** The flat-list total, already formatted with its "+" qualifier when more
   *  pages remain unloaded — shown only on the "All" row. */
  readonly totalCountLabel?: string | undefined;
}> = (props) => (
  <ChoiceList<GroupBy>
    legend="Group alerts by"
    value={props.value}
    onChange={props.onChange}
    options={GROUP_BY_VALUES.map((g) => ({
      value: g,
      label: GROUP_TAB_LABEL[g],
      hint: g === "none" ? props.totalCountLabel : undefined,
    }))}
  />
);

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
 * three ranks 0 and draws the faint ring rather than guessing at an order it was
 * never told. The glyph keeps a constant bounding box either way, which is the
 * whole reason level 0 is drawn faint instead of omitted.
 *
 * ⛔ AND IN HERE THE GLYPH IS TIER A, SO `info` AND `warning` DRAW THE SAME MARK.
 * That is faithful rather than broken, and it is the same bargain `CATEGORY_MARK`
 * makes in the timeline's own filter: this menu is a LEGEND for the rows below it,
 * so it draws what the rows draw, and the rows separate those two by hue — which
 * chrome may not spend (§M.2). The label beside each row is what tells them apart
 * here; a menu that invented a distinguishing shape would be disagreeing with the
 * thing it is a legend for. `critical` still differs by weight, which is the one
 * distinction worth having without colour.
 */
const SEVERITY_RANK: Record<string, number> = { critical: 3, warning: 2, info: 1 };
const severityRank = (severity: string): number => SEVERITY_RANK[severity] ?? 0;

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
      FILTER_TRIGGER,
      "w-auto justify-start",
      on ? FILTER_TRIGGER_ON : FILTER_TRIGGER_OFF,
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
                      class={cn(MENU_ROW, activeView() === view.id ? MENU_ROW_ACTIVE : MENU_ROW_QUIET)}
                    >
                      <span class="min-w-0 flex-1 truncate text-left">{view.label}</span>
                    </button>
                  )}
                </For>
              </div>
            </MenuSection>

            {/* U1: never colour alone — and here, never colour at all. The mark
                is the §0.3 state shape drawn `tone="inherit"`, so the row carries
                the same alphabet the table's rows do without spending a Tier B
                hue on chrome (§M.2).

                ⭐ AND THEY ARE CHECKBOXES NOW, NOT A SEGMENTED STRIP. Four
                lifecycle states as pills inside a 288 px panel wrapped to two
                rows and read as a tab bar directly beneath a list of views that
                genuinely IS one — two rows of pills, one meaning "go here" and
                one meaning "and also these". A checkbox says "several at once"
                without being read. */}
            <MenuSection label="Lifecycle state">
              <CheckList<State>
                legend="Lifecycle state"
                options={ALL_STATES.map((s) => ({
                  value: s,
                  label: STATE_LABEL[s],
                  icon: <StateGlyph state={s} tone="inherit" />,
                }))}
                value={props.filters.state}
                onChange={(next) => patch({ state: [...next] })}
                allLabel="Any state"
              />
            </MenuSection>
          </FilterMenu>

          {/* Severity gets its own trigger rather than a line in Status, because
              it is the axis this product is *about*: the ruler on the face of it
              is the same one every row in the table below carries, filled to the
              worst severity being filtered for. */}
          <FilterMenu
            label="Severity"
            value={severityValue()}
            leading={<SeverityEnso level={severityMark()} class="size-3" />}
          >
            {/* The common three, then any severity a URL brought in that is not
                one of them: a deployment using `sev1` sees its own word back
                rather than having it silently dropped — but no invented rank, so
                the ruler stays empty for a severity nobody told us how to order. */}
            <CheckList<string>
              legend="Severity"
              options={[...COMMON_SEVERITIES, ...customSeverities()].map((s) => ({
                value: s,
                label: s,
                icon: <SeverityEnso level={severityRank(s)} />,
              }))}
              value={props.filters.severity}
              onChange={(next) => patch({ severity: [...next] })}
              allLabel="Any severity"
            />
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
            <GroupingChoices
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
