/**
 * The org's Remedy risk rules and risk model — what says whether a Remedy needs one approval
 * or two (ADR 0054 §3, git-bug eb4f21b) — shown READ-ONLY.
 *
 * ⛔⛔ MANAGED BY `oto remedy-rules`, NOT HERE (owner ruling 2026-10-05). A rule that says one
 * lets one grant holder approve alone, so writing one is the same authority as granting a
 * second approver, and it is exercised where that is: from the host shell, with
 * `oto remedy-rules apply --org SLUG -f rules.yaml`. No route writes a rule, so this screen
 * offers no control that could; it shows what stands and says where it is changed.
 *
 * ⛔ OTO SHIPS NO RULE. With no rules every Remedy needs two different approvers; a rule exists
 * to lower that for a command the operator judges harmless, never to be the only thing between
 * a command and one approval.
 *
 * ⭐ THE MOST SEVERE MATCHING RULE WINS, AND THE SCREEN SAYS SO. Order only decides which rule
 * a Remedy names; a rule saying two cannot be outvoted by one above it. No match is two, and a
 * command the rules cannot parse (`sh -c`, a pipe, a redirect…) is two whatever they say.
 *
 * ⭐ THE RISK MODEL MAY ONLY RAISE, AND IT SPENDS FROM THE DAY'S BUDGET. It is asked only about a
 * Remedy the rules said needs one, sees only the command, its target and the rules' verdict —
 * never the Investigation — and a model that fails, or a day whose token budget is spent, leaves
 * two.
 */
import { For, Match, Show, Switch, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { modelProvidersQuery, remedyRiskRulesQuery } from "~/api/queries";
import type { RemedyRiskRule } from "~/api/types";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";

import { FORM, HELP, PANEL_BODY, PANEL_HEADER, SECTION } from "./rhythm";

/** A rule's conditions, as the operator wrote them in the file; one left out holds for any. */
function conditions(r: RemedyRiskRule): readonly (readonly [string, string])[] {
  const out: (readonly [string, string])[] = [];
  if (r.tool !== null) out.push(["tool", r.tool]);
  if (r.verbs.length > 0) out.push(["verbs", r.verbs.join(", ")]);
  if (r.kinds.length > 0) out.push(["kinds", r.kinds.join(", ")]);
  if (r.namespaces.length > 0) out.push(["namespaces", r.namespaces.join(", ")]);
  if (r.reversibility !== null) out.push(["reversibility", r.reversibility]);
  return out;
}

export const RemedyRiskSection: Component = () => {
  const risk = useQuery(() => remedyRiskRulesQuery());
  const providers = useQuery(() => modelProvidersQuery());

  /** The risk model's name, or its id until the endpoints are read. */
  const modelName = (): string | null => {
    const id = risk.data?.risk_model_provider_id ?? null;
    if (id === null) return null;
    return providers.data?.data.find((p) => p.id === id)?.name ?? id;
  };

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Remedy risk</PanelTitle>
        </PanelHeader>

        <div class={cn(PANEL_BODY, FORM)}>
          <p class={HELP} data-managed-by>
            <strong class="font-semibold text-ink">
              Managed by <span class="font-mono">oto remedy-rules</span>
            </strong>{" "}
            from the host shell —{" "}
            <span class="font-mono text-ink">oto remedy-rules apply --org &lt;slug&gt; -f rules.yaml</span>.
            Nothing in oto writes them: a rule that says one lets one person approve alone, so changing the
            rules takes the same access as granting an approver.
          </p>
          <p class={HELP}>
            Rules that say whether a Remedy needs <strong class="font-semibold text-ink">one</strong> approval
            or <strong class="font-semibold text-ink">two</strong> from different people, read off its exact
            command — its write Tool, verb, resource kind, namespace, and whether the verb is one oto knows to
            be reversible. oto ships none: with no rules, every Remedy needs two.
          </p>
          <p class={HELP} data-semantics>
            The <strong class="font-semibold text-ink">most severe matching rule wins</strong>: if any rule that
            matches says two, it needs two, whatever a rule above it says. No match is two. A command the rules
            cannot parse — <span class="font-mono">sh -c</span>, a pipe, <span class="font-mono">;</span>,{" "}
            <span class="font-mono">&amp;&amp;</span>, backticks, <span class="font-mono">$(…)</span>, a
            redirect, a quote, a flag oto does not know — is two whatever the rules say. Order only decides which
            rule a Remedy names.
          </p>

          <Switch>
            <Match when={risk.isPending}>
              <LoadingLine />
            </Match>
            <Match when={risk.isError}>
              <ErrorState error={risk.error} onRetry={() => void risk.refetch()} />
            </Match>
            <Match when={risk.data}>
              {(data) => (
                <>
                  <Show
                    when={data().rules.length > 0}
                    fallback={
                      <p class={HELP} data-no-rules>
                        No rules. Every Remedy needs two approvals.
                      </p>
                    }
                  >
                    <ol class="flex flex-col gap-sm" aria-label="Risk rules, in order">
                      <For each={data().rules}>
                        {(r, i) => (
                          <li class="flex flex-col gap-xs border-l-2 border-line-strong pl-sm" data-rule-row>
                            <div class="flex flex-wrap items-baseline gap-sm">
                              <span class="text-meta text-ink-muted tabular-nums">{i() + 1}.</span>
                              <span class="font-mono text-ink">{r.name}</span>
                              <span class="ml-auto font-semibold text-ink" data-approvals>
                                {r.approvals === 1 ? "One approval" : "Two approvals"}
                              </span>
                            </div>
                            <dl class="flex flex-wrap gap-x-md gap-y-xs text-meta">
                              <For each={conditions(r)}>
                                {([k, v]) => (
                                  <div class="flex gap-xs">
                                    <dt class="text-ink-muted">{k}</dt>
                                    <dd class="font-mono text-ink">{v}</dd>
                                  </div>
                                )}
                              </For>
                            </dl>
                          </li>
                        )}
                      </For>
                    </ol>
                  </Show>

                  <p class={HELP}>
                    Every condition a rule names must hold; one it leaves out holds for any command. A command
                    that names no namespace, or all of them, matches no namespace. Reversible verbs:{" "}
                    <span class="font-mono text-ink">{data().reversible_verbs.join(", ")}</span> — every other
                    verb is irreversible.
                  </p>

                  <div class="flex flex-col gap-xs">
                    <p class="text-meta text-ink" data-risk-model>
                      Risk model:{" "}
                      <Show when={modelName()} fallback={<span>none — the rules' answer stands.</span>}>
                        {(name) => <span class="font-mono">{name()}</span>}
                      </Show>
                    </p>
                    <p class={HELP} data-risk-model-help>
                      Asked only about a Remedy the rules say needs one approval, and only ever to raise it to
                      two. It sees the command, its target and the rules' verdict — never the Investigation, its
                      logs or its Finding. If it fails or answers anything but one or two, the Remedy needs two.
                      Its tokens count against the org's daily token budget, and once that is spent it is not
                      asked and the Remedy needs two.
                    </p>
                  </div>
                </>
              )}
            </Match>
          </Switch>
        </div>

        <div class={cn(PANEL_BODY, HELP, "flex flex-col gap-xs border-t border-line")}>
          <Show when={risk.data?.written_by_label}>
            {(by) => (
              <p data-written-by>
                Last applied by {by()}
                <Show when={risk.data?.written_at}>{(at) => <> at {absoluteTime(at())}</>}</Show>.
              </p>
            )}
          </Show>
          <p data-one-approval-warning>
            <strong class="font-semibold text-ink">A rule that says one lets one person approve alone.</strong>{" "}
            Applying the rules decides the next Remedy's approvals; it never changes a Remedy already proposed.
          </p>
        </div>
      </Panel>
    </div>
  );
};
