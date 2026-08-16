/**
 * Every filter the contract serves, and not one it does not.
 *
 * The bar is wide and dense rather than hidden behind a "Filters" drawer,
 * because the single most important property of a filtered view is that you can
 * see what is filtering it. A drawer makes "why is this list empty?" a two-click
 * question, and at 3am that is how a typo becomes an outage. Nothing here is ever
 * collapsed into an overflow menu for the same reason — it just has to fit in
 * two rows instead of one.
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
} from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { clustersQuery, labelNamesQuery } from "~/api/queries";
import { FilterRow } from "~/components/ui/FilterRow";
import {
  Select,
  SelectContent,
  SelectHiddenSelect,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
import { Button } from "~/components/ui/Button";
import { Chip } from "~/components/ui/surfaces";
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

/**
 * Ack, Snoozed and Flapping are all the same shape underneath: a nullable
 * boolean (or, for Ack, a nullable two-member enum) stood in for as a string
 * tri-state — `""` for "unset" — because that is the value type both a native
 * `<select>` and the Kobalte listbox key that replaces it need.
 */
const ACK_OPTIONS = ["", "unacked", "acked"] as const;
const ACK_LABEL: Record<(typeof ACK_OPTIONS)[number], string> = {
  "": "Any",
  unacked: "Not yet seen",
  acked: "Seen by someone",
};

const SNOOZED_OPTIONS = ["", "true", "false"] as const;
const SNOOZED_LABEL: Record<(typeof SNOOZED_OPTIONS)[number], string> = {
  "": "Any (default — includes both)",
  true: "Notifications held",
  false: "Notifications flowing",
};

const FLAPPING_OPTIONS = ["", "true", "false"] as const;
const FLAPPING_LABEL: Record<(typeof FLAPPING_OPTIONS)[number], string> = {
  "": "Any",
  true: "Damped as flapping",
  false: "Not flapping",
};

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
 * The word "Group" is deliberately absent. It used to collide with `/groups`
 * elsewhere in the app — notification-group generations, an unrelated feature —
 * and a tab strip that already reads as "how is this list arranged" does not
 * need the word repeated in every label.
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

  return (
    <div class="min-w-0">
      <label id="alert-q-label" for="alert-q" class="sr-only-focusable">
        Search alerts, or type a label matcher
      </label>
      <div
        class={cn(
          "flex min-w-0 flex-wrap items-center gap-2xs rounded-control border bg-surface px-xs py-2xs",
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
          mid-word is worse than telling them a beat later. */}
      <div id={statusId} aria-live="polite" class="min-w-0">
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
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Grouping, as tabs                                                          */
/* -------------------------------------------------------------------------- */

/**
 * A manual-activation tab strip (ARIA APG), deliberately not automatic — moving
 * focus with the arrow keys must never fire a request. Selecting an axis reads
 * `GET /api/v1/alerts/rollups` instead of `GET /api/v1/alerts`; automatic
 * activation would fire one of those per arrow key press while someone merely
 * tabs through the list of axes.
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
      <TabsList aria-label="Group alerts by">
        <For each={GROUP_BY_VALUES}>
          {(g) => (
            <TabsTrigger value={g}>
              {GROUP_TAB_LABEL[g]}
              <Show when={g === "none" && props.totalCountLabel !== undefined}>
                <span class="ml-1 tabular-nums text-ink-subtle">{props.totalCountLabel}</span>
              </Show>
            </TabsTrigger>
          )}
        </For>
      </TabsList>
    </Tabs>
  );
};

/* -------------------------------------------------------------------------- */
/* The bar itself                                                             */
/* -------------------------------------------------------------------------- */

export interface FilterBarProps {
  readonly filters: AlertFilters;
  readonly onChange: (next: AlertFilters) => void;
  readonly onReset: () => void;
  /** Rendered at the right of the grouping-tab row — the result count and live pill. */
  readonly status?: JSX.Element;
  /** The flat-list total, pre-formatted with its "+" qualifier, shown on the
   *  "All" tab only. `undefined` while a roll-up axis is active. */
  readonly totalCountLabel?: string | undefined;
  /** See `SearchBox`'s own prop of the same name. */
  readonly partialMatchEnabled?: boolean | undefined;
}

export const FilterBar: Component<FilterBarProps> = (props) => {
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

  return (
    // `bg-raised`, not `bg-surface`: the filter chrome is a panel sitting
    // above the table, the same elevation step the table's own sticky header
    // uses — not a bare row of controls floating on the page background.
    <div class="shrink-0 border-b border-line bg-raised">
      {/* ---- row 1: the one search box ---------------------------------- */}
      <div class="px-md pb-sm pt-sm">
        <SearchBox
          filters={props.filters}
          onChange={props.onChange}
          partialMatchEnabled={props.partialMatchEnabled}
        />
      </div>

      {/* ---- row 2: how the list is arranged, and in what order ---------- */}
      <FilterRow standalone={false} gap="tight">
        <GroupingTabs
          value={props.filters.groupBy}
          onChange={(g) => patch({ groupBy: g })}
          totalCountLabel={props.totalCountLabel}
        />

        <div class="ml-auto flex shrink-0 items-center gap-sm">
          <label for="alert-sort" class="sr-only-focusable">
            Sort order
          </label>
          <Select<SortKey>
            class="flex flex-col gap-2xs"
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
              title="Only two orderings exist, because a keyset cursor needs a total order backed by an index."
            >
              <SelectValue<SortKey>>{(state) => SORT_LABEL[state.selectedOption()]}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>

          {props.status}
        </div>
      </FilterRow>

      {/* ---- row 3: glance-first — when, what state, how bad ------------- */}
      <FilterRow standalone={false}>
        <label for="alert-since" class="flex items-center gap-xs text-body text-ink-muted">
          <span>Since</span>
          <Select<string>
            class="flex flex-col gap-2xs"
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
            <SelectTrigger id="alert-since" title="Lower bound on last activity.">
              <SelectValue<string>>{(state) => sinceOptionLabel(state.selectedOption())}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </label>

        <ToggleGroup
          legend="Lifecycle state"
          multiple
          value={[...props.filters.state]}
          onChange={(next) => patch({ state: next as State[] })}
        >
          <For each={ALL_STATES}>
            {(s) => <ToggleGroupItem value={s}>{STATE_LABEL[s]}</ToggleGroupItem>}
          </For>
        </ToggleGroup>

        <ToggleGroup
          legend="Severity"
          multiple
          value={[...props.filters.severity]}
          onChange={(next) => patch({ severity: next })}
        >
          <For each={COMMON_SEVERITIES}>
            {(s) => <ToggleGroupItem value={s}>{s}</ToggleGroupItem>}
          </For>
          <For each={customSeverities()}>
            {(s) => <ToggleGroupItem value={s}>{s}</ToggleGroupItem>}
          </For>
        </ToggleGroup>
      </FilterRow>

      {/* ---- row 4: investigation-second — orthogonal axes, then Clear --- */}
      <FilterRow standalone={false}>
        {/* Acknowledgement is orthogonal to state (§B): `acked` still returns
            firing alerts, because acknowledging one does not end it. */}
        <label for="alert-ack" class="flex items-center gap-xs text-body text-ink-muted">
          <span>Ack</span>
          {/* `id`/`title` go on Kobalte's own `SelectTrigger` — the real,
              accessible, interactive surface — same as Sort/Since/Flapping.
              `SelectHiddenSelect` stays unlabelled: it is a separate,
              genuinely `aria-hidden` native `<select>` Kobalte renders only
              for native form-submission/autofill, not a stand-in for the
              trigger. */}
          <Select<(typeof ACK_OPTIONS)[number]>
            class="flex flex-col gap-2xs"
            options={[...ACK_OPTIONS]}
            optionTextValue={(v) => ACK_LABEL[v]}
            value={props.filters.ack ?? ""}
            onChange={(v) => {
              if (v === null) return;
              patch({ ack: v === "acked" || v === "unacked" ? v : null });
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{ACK_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectTrigger
              id="alert-ack"
              title="A receipt on a signal. An acknowledged alert is still firing."
            >
              <SelectValue<(typeof ACK_OPTIONS)[number]>>
                {(state) => ACK_LABEL[state.selectedOption()]}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </label>

        {/* Snooze is a third orthogonal axis, never a state (§B.8): the default
            includes both, because hiding snoozed alerts is how an incident is
            lost. A snoozed alert still reads at its true severity. */}
        <label for="alert-snoozed" class="flex items-center gap-xs text-body text-ink-muted">
          <span>Snoozed</span>
          {/* Same choice as Ack just above: `id`/`title` on the real
              `SelectTrigger`, not the hidden shim. */}
          <Select<(typeof SNOOZED_OPTIONS)[number]>
            class="flex flex-col gap-2xs"
            options={[...SNOOZED_OPTIONS]}
            optionTextValue={(v) => SNOOZED_LABEL[v]}
            value={props.filters.snoozed === null ? "" : props.filters.snoozed ? "true" : "false"}
            onChange={(v) => {
              if (v === null) return;
              patch({ snoozed: v === "" ? null : v === "true" });
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{SNOOZED_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectTrigger
              id="alert-snoozed"
              title="Whether oto is currently holding its notifications for the alert. It says nothing about the signal — a snoozed alert is still firing and still whatever severity it was."
            >
              <SelectValue<(typeof SNOOZED_OPTIONS)[number]>>
                {(state) => SNOOZED_LABEL[state.selectedOption()]}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </label>

        <label for="alert-flapping" class="flex items-center gap-xs text-body text-ink-muted">
          <span>Flapping</span>
          <Select<(typeof FLAPPING_OPTIONS)[number]>
            class="flex flex-col gap-2xs"
            options={[...FLAPPING_OPTIONS]}
            optionTextValue={(v) => FLAPPING_LABEL[v]}
            value={props.filters.flapping === null ? "" : props.filters.flapping ? "true" : "false"}
            onChange={(v) => {
              if (v === null) return;
              patch({ flapping: v === "" ? null : v === "true" });
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{FLAPPING_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectTrigger id="alert-flapping">
              <SelectValue<(typeof FLAPPING_OPTIONS)[number]>>
                {(state) => FLAPPING_LABEL[state.selectedOption()]}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </label>

        <Show when={(clusters.data?.data.length ?? 0) > 0}>
          <label for="alert-cluster" class="flex items-center gap-xs text-body text-ink-muted">
            <span>Cluster</span>
            <Select<ClusterOption>
              class="flex flex-col gap-2xs"
              options={[...clusterOptions()]}
              optionValue="cluster_key"
              optionTextValue="display_name"
              value={
                clusterOptions().find(
                  (c) => c.cluster_key === (props.filters.cluster[0] ?? ""),
                ) ?? ALL_CLUSTERS
              }
              onChange={(next) => {
                if (next === null) return;
                patch({ cluster: next.cluster_key === "" ? [] : [next.cluster_key] });
              }}
              itemComponent={(itemProps) => (
                <SelectItem item={itemProps.item}>{itemProps.item.rawValue.display_name}</SelectItem>
              )}
            >
              <SelectTrigger id="alert-cluster">
                <SelectValue<ClusterOption>>{(state) => state.selectedOption().display_name}</SelectValue>
              </SelectTrigger>
              <SelectHiddenSelect />
              <SelectContent />
            </Select>
          </label>
        </Show>

        <Show when={activeFilterCount(props.filters) > 0}>
          <Button size="sm" variant="ghost" onClick={props.onReset} class="ml-auto">
            Clear {activeFilterCount(props.filters)} filter
            {activeFilterCount(props.filters) === 1 ? "" : "s"}
          </Button>
        </Show>
      </FilterRow>
    </div>
  );
};
