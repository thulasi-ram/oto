/**
 * "The rule as it was when this fired, and how it has changed since."
 *
 * This is the thing Alertmanager cannot tell you. An alert from six weeks ago
 * shows the threshold that was **actually in force** when it fired, because the
 * snapshot was content-addressed at fire time rather than read live — so a rule
 * that has since been edited, or deleted entirely, does not rewrite history.
 *
 * The diff is deliberately the loudest element on this panel. A threshold that
 * moved under you is the single most valuable fact oto holds, and §B is explicit
 * that a `rule.definition_changed` is delivered regardless of channel verbosity
 * because it is never noise.
 *
 * An expression change also carries oto's **verdict** on itself. oto does not
 * parse PromQL, so it says one of three things — the numbers moved and here they
 * are, the shape moved and no number claim applies, or it changed somewhere oto
 * will not interpret — and this panel renders all three, including the refusal.
 * "5 → 10" for an edit that was really `[5m]` → `[10m]` would tell an operator
 * the opposite of the truth, so the refusal is a feature and is shown as one.
 *
 * Colour discipline (§M.2): a diff is not an alert state, so it gets **no Tier B
 * hue** — and neither does the verdict, which must not read as a severity
 * ranking. It signals with a strong left rule, weight, monospace and explicit
 * `was` / `now` words. Spending red here would devalue the red that means
 * "firing", and would also mislead — a rule change is not a failure.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createMemo,
  createSignal,
  type Component,
  type ParentComponent,
} from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { ruleSnapshotsQuery } from "~/api/queries";
import type {
  MatchConfidence,
  RuleChange,
  RuleExprNumberChange,
  RuleHistory,
  RuleOrigin,
  RuleSnapshot,
  RuleSnapshotQuery,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import {
  Chip,
  DataRow,
  Panel,
  PanelHeader,
  PanelTitle,
  SECTION_LABEL,
} from "~/components/ui/surfaces";
import { EmptyState } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime, duration } from "~/lib/format";
import { PANEL_BODY, PANEL_HEADER } from "./rhythm";

/**
 * The cap on `RuleHistoryDTO.versions`. Reaching it does not mean the history
 * ends there — `GET /api/v1/rule-snapshots` pages past it with a real keyset
 * cursor, so the panel offers to keep reading rather than stopping silently.
 */
const EMBEDDED_VERSION_CAP = 200;

/* -------------------------------------------------------------------------- */
/* Payload sniffing, for the timeline                                         */
/* -------------------------------------------------------------------------- */

/**
 * `AlertEventDTO.payload` is `{[key: string]: unknown}` by contract — the shape
 * varies by `type` and unknown keys are forward-compatible. So the timeline
 * checks structurally rather than casting, and falls back to the generic
 * payload table if the shape is not what this version understands.
 */
export function isRuleChangePayload(type: string, payload: Record<string, unknown>): boolean {
  if (type !== "rule.definition_changed") return false;
  return typeof payload["expr_changed"] === "boolean" || typeof payload["for_changed"] === "boolean";
}

function asRuleChange(payload: Record<string, unknown>): RuleChange | null {
  if (typeof payload["expr_changed"] !== "boolean" && typeof payload["for_changed"] !== "boolean") {
    return null;
  }
  return payload as unknown as RuleChange;
}

export const RuleChangePayload: Component<{
  readonly payload: Record<string, unknown>;
  readonly class?: string;
}> = (props) => {
  const change = createMemo(() => asRuleChange(props.payload));
  return (
    <Show when={change()}>{(c) => <RuleDiff change={c()} class={props.class} />}</Show>
  );
};

/* -------------------------------------------------------------------------- */
/* The diff                                                                   */
/* -------------------------------------------------------------------------- */

export const RuleDiff: Component<{
  readonly change: RuleChange;
  readonly class?: string | undefined;
}> = (props) => {
  const labelDiff = (): readonly [string, readonly string[]][] =>
    Object.entries(props.change.label_diff ?? {});
  const annotationDiff = (): readonly [string, readonly string[]][] =>
    Object.entries(props.change.annotation_diff ?? {});

  const nothing = (): boolean =>
    !props.change.expr_changed &&
    !props.change.for_changed &&
    labelDiff().length === 0 &&
    annotationDiff().length === 0;

  return (
    <div
      class={cn(
        "rounded-control border border-line-strong border-l-2 border-l-ink-muted bg-sunken",
        props.class,
      )}
    >
      <div class="flex flex-wrap items-baseline gap-x-sm gap-y-2xs border-b border-line px-md py-sm">
        <span class="text-body font-semibold text-ink">This rule changed</span>
        <span class="text-meta text-ink-subtle">
          previous version captured{" "}
          <RelativeTime value={props.change.previous_captured_at} label="Previously captured" /> ago
        </span>
      </div>

      <div class="space-y-md px-md py-md">
        <Show when={nothing()}>
          <p class="text-meta text-ink-subtle">
            The fingerprint changed but no field oto compares differed — usually formatting.
          </p>
        </Show>

        <Show when={props.change.expr_changed}>
          <div>
            <DiffBlock
              term="Expression"
              was={props.change.previous_expr ?? ""}
              now={props.change.new_expr ?? ""}
              mono
            />
            <ExprVerdict drift={exprDrift(props.change)} />
          </div>
        </Show>

        <Show when={props.change.for_changed}>
          <DiffBlock
            term="for:"
            was={fmtFor(props.change.previous_for_seconds)}
            now={fmtFor(props.change.new_for_seconds)}
            hint="How long the condition must hold before it fires."
          />
        </Show>

        <For each={labelDiff()}>
          {([name, pair]) => (
            <DiffBlock
              term={`label ${name}`}
              was={pair[0] ?? ""}
              now={pair[1] ?? ""}
              mono
              hint={
                name === "severity"
                  ? "A severity change alters how loudly every future firing is presented."
                  : undefined
              }
            />
          )}
        </For>

        <For each={annotationDiff()}>
          {([name, pair]) => (
            <DiffBlock term={`annotation ${name}`} was={pair[0] ?? ""} now={pair[1] ?? ""} />
          )}
        </For>
      </div>
    </div>
  );
};

function fmtFor(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return "";
  return duration(seconds);
}

/* -------------------------------------------------------------------------- */
/* What changed INSIDE the expression                                         */
/* -------------------------------------------------------------------------- */

/**
 * The rendering cases for `RuleChangeDTO.expr_diff`.
 *
 * There are more of them than there are verdicts, because `numbers_moved` with
 * an empty list is a different thing to say than `numbers_moved` with entries:
 * the contract calls it a reformat, and "the threshold moved" with nothing after
 * the colon would read as a bug.
 */
export type ExprDrift =
  | { readonly kind: "unchanged" }
  | { readonly kind: "uncompared" }
  | { readonly kind: "numbers"; readonly numbers: readonly RuleExprNumberChange[] }
  | { readonly kind: "reformat" }
  | { readonly kind: "structural" }
  | { readonly kind: "uncharacterised" };

/**
 * Turn one `RuleChange` into the single thing this panel is allowed to say
 * about its expression.
 *
 * Pure, and separate from the markup, because the honesty rule lives here: a
 * numeric narrative is reachable through `verdict === "numbers_moved"` and
 * through nothing else. The generated union makes `numbers` invisible on the
 * other two variants, so this function cannot accidentally read one; the switch
 * is what makes the *absence* of a claim explicit rather than a fall-through.
 *
 * `uncompared` is not a fourth verdict. It is what a payload looks like when it
 * carries no `expr_diff` at all — a `rule.definition_changed` timeline payload,
 * or a server older than this field. Rendering the two expressions with no
 * commentary is the only honest response to it.
 */
export function exprDrift(change: RuleChange): ExprDrift {
  if (!change.expr_changed) return { kind: "unchanged" };

  const diff = change.expr_diff;
  if (diff === null || diff === undefined) return { kind: "uncompared" };

  switch (diff.verdict) {
    case "numbers_moved": {
      const numbers = diff.numbers ?? [];
      return numbers.length === 0 ? { kind: "reformat" } : { kind: "numbers", numbers };
    }
    case "structural":
      return { kind: "structural" };
    case "uncharacterised":
      return { kind: "uncharacterised" };
    default:
      // A verdict this build has never heard of. Newer server, older UI — and
      // an unknown verdict is precisely a verdict that did not vouch for a
      // number, so it degrades to "no claim" rather than to a guess.
      return { kind: "uncompared" };
  }
}

/**
 * Render a numeric literal exactly as it arrived.
 *
 * Not `toLocaleString`: thresholds are routinely fractions like `0.05`, and a
 * locale formatter rounds `0.0001` to `0`. A threshold shown as a different
 * number than the rule holds is the one failure mode this whole panel exists to
 * avoid.
 */
function fmtLiteral(n: number): string {
  return String(n);
}

/**
 * oto's verdict on the expression edit, in words.
 *
 * Colour discipline (§M.2): every branch here is Tier-A chrome — hairlines,
 * muted ink, monospace. None of the three verdicts is an alert state, so none of
 * them may borrow a state hue, and "structural" must not read as more alarming
 * than "numbers moved" just because it is less specific. The channels doing the
 * work are a text label and a sentence, never colour alone (§M.3 U1).
 */
const ExprVerdict: Component<{ readonly drift: ExprDrift }> = (props) => {
  // The one place a number may be read. Anything but `numbers` yields an empty
  // list, so there is no rendering path from another verdict to a value.
  const numbers = (): readonly RuleExprNumberChange[] =>
    props.drift.kind === "numbers" ? props.drift.numbers : [];

  return (
    <Switch>
      <Match when={props.drift.kind === "numbers"}>
        <VerdictNote
          label="numbers moved"
          note="Both versions are the same expression with different numbers in it, so oto can name the values that moved."
        >
          <ul class="mt-sm space-y-2xs">
            <For each={numbers()}>
              {(n) => (
                <li class="flex items-baseline gap-sm">
                  <span class="w-10 shrink-0 text-right font-mono text-micro text-ink-subtle">
                    #{n.index + 1}
                  </span>
                  <span class="font-mono text-meta text-ink-muted line-through decoration-ink-subtle/60">
                    {fmtLiteral(n.previous_value)}
                  </span>
                  <span class="text-meta text-ink-subtle" aria-hidden="true">
                    →
                  </span>
                  <span class="font-mono text-meta font-semibold text-ink">
                    {fmtLiteral(n.new_value)}
                  </span>
                </li>
              )}
            </For>
          </ul>
          <p class="mt-sm text-meta leading-snug text-ink-subtle">
            Numbered by position among the literals oto vouched for — durations, subquery steps and{" "}
            <code class="font-mono">offset</code> operands are not counted, and are never reported as
            thresholds.
          </p>
        </VerdictNote>
      </Match>

      <Match when={props.drift.kind === "reformat"}>
        <VerdictNote
          label="formatting only"
          note="The expression was rewritten but not changed: same shape, same numbers, different whitespace. Nothing about when this rule fires has moved."
        />
      </Match>

      <Match when={props.drift.kind === "structural"}>
        <VerdictNote
          label="shape changed"
          note="The expression changed shape — a different metric, aggregation, label matcher, or a term added or removed. Its numbers no longer line up with the old ones, so oto reports no threshold move. Compare the two versions above."
        />
      </Match>

      <Match when={props.drift.kind === "uncharacterised"}>
        <VerdictNote
          label="not characterised"
          note="oto is not saying what changed here. The edit touches something it will not read as a threshold — a range window, a subquery step, an offset — and calling that a threshold move could be exactly backwards. Both versions are above; the comparison is yours to make."
        />
      </Match>
    </Switch>
  );
};

/**
 * One verdict, as a label plus a plain sentence.
 *
 * The label is a Chip and not a coloured badge on purpose: it names the verdict,
 * it does not rank it. There is no "good" verdict here — `numbers moved` is more
 * specific than `not characterised`, not more urgent.
 */
const VerdictNote: ParentComponent<{
  readonly label: string;
  readonly note: string;
}> = (props) => (
  <div class="mt-sm border-l border-line pl-sm">
    <div class="flex flex-wrap items-baseline gap-x-sm gap-y-2xs">
      <Chip>oto: {props.label}</Chip>
      <p class="min-w-0 flex-1 text-meta leading-snug text-ink-subtle">{props.note}</p>
    </div>
    {props.children}
  </div>
);

/**
 * One changed field, as two labelled lines.
 *
 * Not a red/green character diff: PromQL is dense and a token-level diff of it
 * is harder to read than the two expressions side by side, and red/green would
 * both mislead (a change is not an error) and break the Tier-B rule.
 */
const DiffBlock: Component<{
  readonly term: string;
  readonly was: string;
  readonly now: string;
  readonly mono?: boolean;
  readonly hint?: string | undefined;
}> = (props) => (
  <div>
    <div class="mb-2xs flex flex-wrap items-baseline gap-x-sm gap-y-2xs">
      <span class="text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted">
        {props.term}
      </span>
      <Show when={props.hint}>
        <span class="text-meta text-ink-subtle">{props.hint}</span>
      </Show>
    </div>
    <div class="grid grid-cols-[2.6rem_minmax(0,1fr)] gap-x-sm gap-y-2xs">
      <span class="pt-px text-right text-meta text-ink-subtle">was</span>
      <code
        class={cn(
          "min-w-0 break-words rounded-chip bg-surface px-xs py-2xs text-meta leading-snug text-ink-muted line-through decoration-ink-subtle/60",
          props.mono === true ? "font-mono" : "",
        )}
      >
        {props.was === "" ? "(absent)" : props.was}
      </code>

      <span class="pt-px text-right text-meta font-semibold text-ink">now</span>
      <code
        class={cn(
          "min-w-0 break-words rounded-chip border border-line-strong bg-surface px-xs py-2xs text-meta font-medium leading-snug text-ink",
          props.mono === true ? "font-mono" : "",
        )}
      >
        {props.now === "" ? "(absent)" : props.now}
      </code>
    </div>
  </div>
);

/* -------------------------------------------------------------------------- */
/* The panel                                                                  */
/* -------------------------------------------------------------------------- */

// ⛔ Both notes are keyed by the contract's own enums rather than by `string`:
// an origin or a confidence the server adds must fail the build here, not render
// a bare wire token in the tooltip that explains how much to trust the panel.
const ORIGIN_NOTE: Record<RuleOrigin, string> = {
  prometheus_api: "Read from the Prometheus rules API — the authoritative source.",
  generator_url:
    "Reconstructed from the alert's generatorURL because the rules API was not reachable. The expression is what the URL encoded, not what the file said.",
  unavailable:
    "oto could not obtain the rule at all. The expression below is empty, and that absence is recorded rather than guessed at.",
};

const CONFIDENCE_NOTE: Record<MatchConfidence, string> = {
  exact: "Exactly one rule matched this alert.",
  probable: "More than one rule could have produced this alert; oto picked the best match.",
  ambiguous:
    "Several rules matched equally well. Treat the definition below as one candidate, not as certainty.",
  none: "No rule matched. Nothing below is a claim about your configuration.",
};

export const RulePanel: Component<{ readonly history: RuleHistory }> = (props) => {
  const current = (): RuleSnapshot | null => props.history.current;

  return (
    <Panel>
      <PanelHeader class={PANEL_HEADER}>
        <PanelTitle>Rule at fire time</PanelTitle>
        <Show when={props.history.versions.length > 0}>
          <span class="shrink-0 text-meta text-ink-subtle">
            {props.history.versions.length} version
            {props.history.versions.length === 1 ? "" : "s"} captured
          </span>
        </Show>
      </PanelHeader>

      {/* ⛔ NO RULE IS AN ORDINARY ANSWER AND IS RENDERED AS ONE.
          `current: null` with an empty `versions` means oto captured nothing
          for this alert, and that is normal rather than broken: a
          Grafana-sourced alert, an alert fired by hand, an Alertmanager whose
          Prometheus is unreachable, a `generatorURL` with no `g0.expr` in it.

          This used to be a red "Validation failed — a rule key's source id must
          be a UUID" box with a Try again button and a request id, because the
          server reported the absence as a 422 (TICKET 015b25b). Three things
          were wrong with that and all three are worth naming, because they are
          the trap any absence-as-error falls into: it was not a validation
          failure and nothing the operator did caused it; the sentence was
          internal vocabulary about a database index; and the retry could never
          succeed, so the button was an invitation to waste time at 3am.

          The alert LIST has always rendered these same alerts as a plain
          em-dash. This is the same fact at the size a panel affords: a sentence
          instead of a dash, and the ordinary causes named so the operator knows
          whether to go and configure something or to stop looking. */}
      <Show
        when={current()}
        fallback={
          <EmptyState
            title="oto captured no rule for this alert."
            body="That is an ordinary outcome, not a failure. The alert may not come from a Prometheus alerting rule at all — one raised by Grafana, or fired by hand — or its generatorURL carried no expression and the rules API was not reachable when it fired. oto records the absence rather than guessing at a definition it never saw."
          />
        }
      >
        {(rule) => (
          <div class={cn("space-y-md", PANEL_BODY)}>
            {/* The drift diff comes FIRST. If the rule moved, that is the
                headline, not a footnote under the current definition. */}
            <Show when={props.history.change}>
              {(change) => <RuleDiff change={change()} />}
            </Show>

            <dl class="space-y-2xs">
              <DataRow term="Rule">
                <span class="font-mono text-body">{rule().rule_name}</span>
              </DataRow>
              <Show when={rule().rule_group !== ""}>
                <DataRow term="Group">
                  <span class="font-mono text-body">{rule().rule_group}</span>
                </DataRow>
              </Show>
              <Show when={rule().rule_file !== ""}>
                <DataRow term="File">
                  <span class="break-all font-mono text-body text-ink-muted">
                    {rule().rule_file}
                  </span>
                </DataRow>
              </Show>
              <DataRow term="for:">
                <span class="font-mono text-body">{duration(rule().for_seconds)}</span>
                <Show when={rule().keep_firing_for_seconds > 0}>
                  <span class="ml-sm text-meta text-ink-subtle">
                    keep_firing_for {duration(rule().keep_firing_for_seconds)}
                  </span>
                </Show>
              </DataRow>
              <DataRow term="Captured">
                <span title={absoluteTime(rule().captured_at)}>
                  <RelativeTime value={rule().captured_at} label="Captured" /> ago
                </span>
              </DataRow>
            </dl>

            <div>
              <p class="mb-sm text-meta font-semibold uppercase tracking-[0.06em] text-ink-muted">
                Expression
              </p>
              <pre class="overflow-x-auto rounded-control border border-line bg-sunken px-md py-sm font-mono text-meta leading-relaxed text-ink">
                <code>{rule().expr === "" ? "(oto could not read this rule)" : rule().expr}</code>
              </pre>
            </div>

            {/* Provenance. A snapshot that came from a generatorURL is a weaker
                claim than one read from the rules API, and saying which is the
                difference between evidence and a guess. */}
            <div class="flex flex-wrap items-center gap-xs">
              <Chip title={ORIGIN_NOTE[rule().origin] ?? ""}>origin: {rule().origin}</Chip>
              <Chip title={CONFIDENCE_NOTE[rule().match_confidence] ?? ""}>
                match: {rule().match_confidence}
                {rule().candidate_count > 1 ? ` (${rule().candidate_count} candidates)` : ""}
              </Chip>
              <Show when={rule().prometheus_url}>
                {(url) => (
                  <a
                    href={url()}
                    target="_blank"
                    rel="noreferrer noopener"
                    class="text-meta text-ink-muted underline decoration-line-strong underline-offset-2 hover:text-ink"
                  >
                    Prometheus ↗
                  </a>
                )}
              </Show>
            </div>

            <p class="text-meta leading-snug text-ink-subtle">
              {ORIGIN_NOTE[rule().origin] ?? ""} {CONFIDENCE_NOTE[rule().match_confidence] ?? ""}
            </p>

            <Show when={props.history.versions.length > 1}>
              <VersionHistory
                versions={props.history.versions}
                currentId={rule().id}
                ruleKey={props.history.rule_key}
              />
            </Show>
          </div>
        )}
      </Show>
    </Panel>
  );
};

/**
 * Every version oto has captured for this RuleKey, newest first.
 *
 * It answers "has anyone been changing this?" at a glance, which is a question
 * with no other honest answer once the file has been edited.
 *
 * The embedded array stops at 200. That used to be the end of the road — the
 * history was served from a 200-version in-memory window, so a heavily edited
 * rule simply had history nothing could show. `GET /api/v1/rule-snapshots` is
 * now keyset-paginated over `(captured_at, id)` for real, so on hitting the cap
 * this keeps reading rather than presenting a truncated list as if it were
 * complete.
 *
 * Deliberately not `createKeysetFeed`: this machine differs in kind, not in
 * spelling. Page one is the embedded `versions` array from a *different*
 * endpoint, the first "load more" press starts the snapshot keyset from the top
 * rather than advancing a cursor, `hasMore` before that press is a claim about
 * the embedded cap rather than about any envelope — and the RuleKey never
 * changes under the panel, so there is no filter axis to fingerprint.
 */
const VersionHistory: Component<{
  readonly versions: readonly RuleSnapshot[];
  readonly currentId: string;
  readonly ruleKey: RuleHistory["rule_key"];
}> = (props) => {
  const [cursor, setCursor] = createSignal<string | null>(null);
  const [started, setStarted] = createSignal(false);
  const [older, setOlder] = createSignal<readonly RuleSnapshot[]>([]);

  const atCap = (): boolean => props.versions.length >= EMBEDDED_VERSION_CAP;

  const query = createMemo<RuleSnapshotQuery>(() => {
    const q: Record<string, unknown> = {
      source_id: props.ruleKey.source_id,
      rule_name: props.ruleKey.rule_name,
      limit: EMBEDDED_VERSION_CAP,
    };
    // Both narrow an otherwise ambiguous match, and both are optional on the
    // RuleKey, so neither is sent unless it is actually there.
    if (props.ruleKey.rule_group) q["rule_group"] = props.ruleKey.rule_group;
    if (props.ruleKey.rule_file) q["rule_file"] = props.ruleKey.rule_file;
    if (cursor() !== null) q["cursor"] = cursor();
    return q as RuleSnapshotQuery;
  });

  const page = useQuery(() => ({
    ...ruleSnapshotsQuery(query()),
    // The only thing that is this screen's rather than the resource's: history
    // is fetched when the operator asks for it, not when the panel renders.
    enabled: atCap() && started(),
  }));

  const all = createMemo<readonly RuleSnapshot[]>(() => {
    const fetched = page.data?.data ?? [];
    const seen = new Set(props.versions.map((v) => v.id));
    const extra = [...older(), ...fetched].filter((v) => !seen.has(v.id));
    const deduped = new Map(extra.map((v) => [v.id, v]));
    return [...props.versions, ...deduped.values()];
  });

  const hasMore = (): boolean =>
    started() ? (page.data?.page.has_more ?? false) : atCap();

  const loadOlder = (): void => {
    if (!started()) {
      // The embedded array carries no cursor of its own, so the first press
      // starts the endpoint's own keyset from the top. The overlap with what is
      // already on screen is deduplicated by id rather than shown twice.
      setStarted(true);
      return;
    }
    setOlder(all().slice(props.versions.length));
    const next = page.data?.page.next_cursor;
    if (typeof next === "string" && next !== "") setCursor(next);
  };

  return (
    <details class="rounded-control border border-line">
      <summary
        class={cn(
          "cursor-pointer list-none px-md py-sm text-ink-muted hover:bg-raised",
          SECTION_LABEL,
        )}
      >
        Version history ({all().length}
        {hasMore() ? "+" : ""})
      </summary>
      <ol class="border-t border-line">
        <For each={all()}>
        {(v) => (
          <li
            class={cn(
              "border-b border-line px-md py-sm last:border-b-0",
              // Neutral, not accented (§0.6). "The one bound to this case"
              // is chrome; the words in the row already say so.
              v.id === props.currentId ? "border-l-2 border-l-ink-muted bg-sunken" : "",
            )}
          >
            <div class="flex flex-wrap items-baseline gap-x-sm gap-y-2xs">
              <span class="font-mono text-micro text-ink-subtle">
                {v.rule_fingerprint.slice(0, 12)}
              </span>
              <span class="text-meta text-ink-muted">
                <RelativeTime value={v.captured_at} label="Captured" /> ago
              </span>
              <span class="text-meta text-ink-subtle">for {duration(v.for_seconds)}</span>
              <Show when={v.id === props.currentId}>
                <span class="ml-auto text-micro font-semibold uppercase tracking-wide text-ink">
                  bound to this case
                </span>
              </Show>
            </div>
            <code class="mt-2xs block truncate font-mono text-micro text-ink-muted" title={v.expr}>
              {v.expr === "" ? "(unavailable)" : v.expr}
            </code>
          </li>
        )}
        </For>
      </ol>

      <Show when={hasMore()}>
        <div class="border-t border-line px-md py-md text-center">
          <Button size="sm" busy={page.isFetching} onClick={loadOlder}>
            Load older versions
          </Button>
          <p class="mt-sm text-micro leading-snug text-ink-subtle">
            The alert's own response embeds at most {EMBEDDED_VERSION_CAP} captures. There are more,
            and they are reachable — this pages the full history with a keyset cursor.
          </p>
        </div>
      </Show>
    </details>
  );
};
