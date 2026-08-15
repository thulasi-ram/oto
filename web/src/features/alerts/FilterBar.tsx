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
import { Button, Chip, Select, ToggleGroup, cx } from "~/components/ui/primitives";
import { STATE_LABEL } from "~/components/StateChip";
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
    <Chip mono class="shrink-0 gap-1">
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
      <label for="alert-q" class="sr-only-focusable">
        Search alerts, or type a label matcher
      </label>
      <div
        class={cx(
          "flex min-w-0 flex-wrap items-center gap-1 rounded-control border bg-surface px-1.5 py-1",
          hasProblem() ? "border-line-strong ring-1 ring-accent-border" : "border-line-strong",
        )}
      >
        <For each={committedParsed().matchers}>
          {(m, i) => (
            <MatcherChip matcher={m} onEdit={() => editAt(i())} onRemove={() => removeAt(i())} />
          )}
        </For>

        <input
          ref={inputRef}
          id="alert-q"
          type="text"
          list={listId}
          value={draft()}
          placeholder={
            committedParsed().matchers.length === 0
              ? 'Search, or namespace="payments"…'
              : "Add another matcher, or search…"
          }
          spellcheck={false}
          autocapitalize="off"
          autocorrect="off"
          aria-describedby={statusId}
          aria-invalid={hasProblem() ? "true" : undefined}
          class="min-w-[8rem] flex-1 border-0 bg-transparent p-0 text-item text-ink placeholder:text-ink-subtle"
          onFocus={() => setFocused(true)}
          onInput={(e) => setDraft(e.currentTarget.value)}
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
          <ul class="mt-1 space-y-0.5">
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
          <ul class="mt-1 space-y-1">
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
          <p class="mt-1 text-meta leading-snug text-ink-subtle">{summary()}</p>
        </Show>

        <Show when={teaching()}>
          <ul class="mt-1 space-y-0.5">
            <For each={MATCHER_EXAMPLES}>
              {(ex) => (
                <li class="flex flex-wrap items-baseline gap-x-2 text-meta leading-snug">
                  <code
                    class={cx(
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
  const refs: (HTMLButtonElement | undefined)[] = [];

  const focusAt = (index: number): void => {
    const n = GROUP_BY_VALUES.length;
    refs[((index % n) + n) % n]?.focus();
  };

  return (
    <div role="tablist" aria-label="Group alerts by" class="flex items-center gap-1">
      <For each={GROUP_BY_VALUES}>
        {(g, i) => {
          const selected = (): boolean => props.value === g;
          return (
            <button
              ref={(el) => {
                refs[i()] = el;
              }}
              type="button"
              role="tab"
              aria-selected={selected() ? "true" : "false"}
              tabindex={selected() ? 0 : -1}
              class={cx(
                "-mb-px border-b-2 px-2.5 py-1.5 text-item transition-colors duration-100",
                selected()
                  ? "border-accent font-medium text-ink"
                  : "border-transparent text-ink-muted hover:text-ink",
              )}
              onClick={() => props.onChange(g)}
              onKeyDown={(e) => {
                if (e.key === "ArrowRight") {
                  e.preventDefault();
                  focusAt(i() + 1);
                } else if (e.key === "ArrowLeft") {
                  e.preventDefault();
                  focusAt(i() - 1);
                } else if (e.key === "Home") {
                  e.preventDefault();
                  focusAt(0);
                } else if (e.key === "End") {
                  e.preventDefault();
                  focusAt(GROUP_BY_VALUES.length - 1);
                } else if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  props.onChange(g);
                }
              }}
            >
              {GROUP_TAB_LABEL[g]}
              <Show when={g === "none" && props.totalCountLabel !== undefined}>
                <span class="ml-1 tabular-nums text-ink-subtle">{props.totalCountLabel}</span>
              </Show>
            </button>
          );
        }}
      </For>
    </div>
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
}

export const FilterBar: Component<FilterBarProps> = (props) => {
  const clusters = useQuery(() => clustersQuery());

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
    <div class="shrink-0 border-b border-line bg-surface">
      {/* ---- row 1: the one search box ---------------------------------- */}
      <div class="px-3 pb-2 pt-2">
        <SearchBox filters={props.filters} onChange={props.onChange} />
      </div>

      {/* ---- row 2: how the list is arranged, and in what order ---------- */}
      <FilterRow standalone={false} gap="tight">
        <GroupingTabs
          value={props.filters.groupBy}
          onChange={(g) => patch({ groupBy: g })}
          totalCountLabel={props.totalCountLabel}
        />

        <div class="ml-auto flex shrink-0 items-center gap-2">
          <label for="alert-sort" class="sr-only-focusable">
            Sort order
          </label>
          <Select
            id="alert-sort"
            value={props.filters.sort}
            onChange={(e) => patch({ sort: e.currentTarget.value as SortKey })}
            title="Only two orderings exist, because a keyset cursor needs a total order backed by an index."
          >
            <option value="-last_seen_at">Newest activity</option>
            <option value="-first_seen_at">Newest first seen</option>
          </Select>

          {props.status}
        </div>
      </FilterRow>

      {/* ---- row 3: glance-first — when, what state, how bad ------------- */}
      <FilterRow standalone={false}>
        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Since</span>
          <Select
            value={sincePreset()}
            onChange={(e) => {
              const preset = SINCE_PRESETS.find((p) => p.label === e.currentTarget.value);
              if (!preset) return;
              patch({
                since:
                  preset.hours === null
                    ? null
                    : new Date(Date.now() - preset.hours * 3_600_000).toISOString(),
              });
            }}
            title="Lower bound on last activity."
          >
            <For each={SINCE_PRESETS}>{(p) => <option value={p.label}>{p.label}</option>}</For>
            <Show when={sincePreset() === "Custom"}>
              <option value="Custom">Custom (from link)</option>
            </Show>
          </Select>
        </label>

        <Divider />

        <ToggleGroup<State>
          legend="Lifecycle state"
          options={ALL_STATES.map((s) => ({ value: s, label: STATE_LABEL[s] }))}
          selected={props.filters.state}
          onChange={(next) => patch({ state: next })}
        />

        <Divider />

        <ToggleGroup<string>
          legend="Severity"
          options={[
            ...COMMON_SEVERITIES.map((s) => ({ value: s as string, label: s })),
            ...customSeverities().map((s) => ({ value: s, label: s })),
          ]}
          selected={props.filters.severity}
          onChange={(next) => patch({ severity: next })}
        />
      </FilterRow>

      {/* ---- row 4: investigation-second — orthogonal axes, then Clear --- */}
      <FilterRow standalone={false}>
        {/* Acknowledgement is orthogonal to state (§B): `acked` still returns
            firing alerts, because acknowledging one does not end it. */}
        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Ack</span>
          <Select
            value={props.filters.ack ?? ""}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ ack: v === "acked" || v === "unacked" ? v : null });
            }}
            title="A receipt on a signal. An acknowledged alert is still firing."
          >
            <option value="">Any</option>
            <option value="unacked">Not yet seen</option>
            <option value="acked">Seen by someone</option>
          </Select>
        </label>

        {/* Snooze is a third orthogonal axis, never a state (§B.8): the default
            includes both, because hiding snoozed alerts is how an incident is
            lost. A snoozed alert still reads at its true severity. */}
        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Snoozed</span>
          <Select
            value={props.filters.snoozed === null ? "" : String(props.filters.snoozed)}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ snoozed: v === "" ? null : v === "true" });
            }}
            title="Whether oto is currently holding its notifications for the alert. It says nothing about the signal — a snoozed alert is still firing and still whatever severity it was."
          >
            <option value="">Any (default — includes both)</option>
            <option value="true">Notifications held</option>
            <option value="false">Notifications flowing</option>
          </Select>
        </label>

        <label class="flex items-center gap-1.5 text-body text-ink-muted">
          <span>Flapping</span>
          <Select
            value={props.filters.flapping === null ? "" : String(props.filters.flapping)}
            onChange={(e) => {
              const v = e.currentTarget.value;
              patch({ flapping: v === "" ? null : v === "true" });
            }}
          >
            <option value="">Any</option>
            <option value="true">Damped as flapping</option>
            <option value="false">Not flapping</option>
          </Select>
        </label>

        <Show when={(clusters.data?.data.length ?? 0) > 0}>
          <label class="flex items-center gap-1.5 text-body text-ink-muted">
            <span>Cluster</span>
            <Select
              value={props.filters.cluster[0] ?? ""}
              onChange={(e) => {
                const v = e.currentTarget.value;
                patch({ cluster: v === "" ? [] : [v] });
              }}
            >
              <option value="">All clusters</option>
              <For each={clusters.data?.data ?? []}>
                {(c) => <option value={c.cluster_key}>{c.display_name}</option>}
              </For>
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

const Divider: Component = () => (
  <span class={cx("h-4 w-px shrink-0 bg-line")} aria-hidden="true" />
);
