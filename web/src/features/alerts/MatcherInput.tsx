/**
 * The label filter (ADR 0017).
 *
 * Alertmanager matcher syntax, because that is the idiom oto's audience already
 * types every day — not a general expression language. The whole expression is
 * sent verbatim in the contract's `matcher=` parameter, so **all four operators
 * work**: `=`, `!=`, `=~` and `!~`.
 *
 * The ADR's binding rule governs the one boundary that remains:
 *
 * > anything untranslatable is rejected at parse time with a precise message —
 * > never silently degraded to a scan.
 *
 * A `=~` value that is an alternation of literals (`critical|warning`) is an
 * `IN` list and is answered from the label index. A value carrying a
 * metacharacter is a sequential scan over every alert in the org, and the server
 * refuses it at parse time by design. This input says so *before* sending —
 * with the reason, not just a rejection — because an operator who understands
 * the boundary can work around it, and one who is told "invalid" cannot.
 *
 * The input is a plain `<input>` on purpose. A contenteditable token field would
 * look more sophisticated and would break paste, undo, screen readers and the
 * one thing that actually matters here — typing a filter fast at 3am.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { labelNamesQuery } from "~/api/queries";
import { Input, cx } from "~/components/ui/primitives";
import { count as fmtCount } from "~/lib/format";
import {
  MATCHER_EXAMPLES,
  compileMatchers,
  parseMatchers,
  type LabelMatcher,
} from "~/lib/matchers";

export interface MatcherInputProps {
  readonly value: string;
  readonly onChange: (next: string) => void;
  /** Fired on Enter, so the list only refetches when the user says so. */
  readonly onCommit: () => void;
  readonly id?: string;
  readonly class?: string;
}

export const MatcherInput: Component<MatcherInputProps> = (props) => {
  const [draft, setDraft] = createSignal(props.value);
  const [focused, setFocused] = createSignal(false);

  // The committed value is authoritative; the draft only exists while typing so
  // that a half-typed matcher does not fire a request per keystroke.
  const text = (): string => (focused() ? draft() : props.value);

  const parsed = createMemo(() => parseMatchers(text()));
  const compiled = createMemo(() => compileMatchers(parsed().matchers));

  const names = useQuery(() => labelNamesQuery());

  const hasProblem = (): boolean =>
    parsed().errors.length > 0 || compiled().rejected.length > 0;

  const listId = (): string => `${props.id ?? "matchers"}-names`;
  const statusId = (): string => `${props.id ?? "matchers"}-status`;

  /** Show the worked examples while the box is focused and still empty. */
  const teaching = (): boolean => focused() && text().trim() === "";

  const commit = (): void => {
    props.onChange(draft());
    props.onCommit();
  };

  return (
    <div class={cx("min-w-0", props.class)}>
      <Input
        id={props.id}
        mono
        list={listId()}
        value={text()}
        placeholder={'{namespace="payments", severity=~"critical|warning"}'}
        spellcheck={false}
        autocapitalize="off"
        autocorrect="off"
        aria-label="Label matchers, Alertmanager syntax"
        aria-describedby={statusId()}
        invalid={hasProblem()}
        onFocus={() => {
          setDraft(props.value);
          setFocused(true);
        }}
        onBlur={(e) => {
          setFocused(false);
          if (e.currentTarget.value !== props.value) props.onChange(e.currentTarget.value);
        }}
        onInput={(e) => setDraft(e.currentTarget.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
            e.currentTarget.blur();
          }
          if (e.key === "Escape") {
            setDraft(props.value);
            e.currentTarget.blur();
          }
        }}
      />

      {/* A native datalist gives name completion with zero ARIA of our own and
          zero chance of trapping the keyboard. `alert_count` is what orders the
          list server-side, so it is shown: a filter bar that offers a label
          matching nothing spends the one minute of an incident that matters. */}
      <datalist id={listId()}>
        <For each={names.data ?? []}>
          {(row) => (
            <option value={`${row.name}="`} label={`${fmtCount(row.alert_count)} alerts`} />
          )}
        </For>
      </datalist>

      {/* The parse result, always visible while there is something to say. It is
          `polite` because a filter is typed, not pushed — interrupting someone
          mid-word is worse than telling them a beat later. */}
      <div id={statusId()} aria-live="polite" class="min-w-0">
        <Show when={parsed().errors.length > 0}>
          <ul class="mt-1 space-y-0.5">
            <For each={parsed().errors}>
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

        <Show when={compiled().rejected.length > 0}>
          <ul class="mt-1 space-y-1">
            <For each={compiled().rejected}>
              {(r) => (
                <li class="flex items-start gap-1.5 text-meta leading-snug text-ink">
                  <MarkGlyph />
                  <span>
                    <code class="font-mono text-ink">
                      {r.matcher.name}
                      {r.matcher.op}
                      {JSON.stringify(r.matcher.value)}
                    </code>{" "}
                    — {r.reason}
                  </span>
                </li>
              )}
            </For>
          </ul>
        </Show>

        <Show when={!hasProblem() && parsed().matchers.length > 0 && focused()}>
          <p class="mt-1 text-meta leading-snug text-ink-subtle">
            {describe(parsed().matchers)} · press Enter to apply
          </p>
        </Show>

        <Show when={teaching()}>
          <ul class="mt-1 space-y-0.5">
            <For each={MATCHER_EXAMPLES}>
              {(ex) => (
                <li class="flex flex-wrap items-baseline gap-x-2 text-meta leading-snug">
                  <code
                    class={cx(
                      "font-mono",
                      ex.served ? "text-ink" : "text-ink-subtle line-through decoration-ink-subtle/60",
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

/**
 * Say the filter back in English.
 *
 * Repeated names OR and distinct names AND (§E.3), and that asymmetry surprises
 * people often enough to be worth stating rather than documenting.
 */
function describe(matchers: readonly LabelMatcher[]): string {
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
