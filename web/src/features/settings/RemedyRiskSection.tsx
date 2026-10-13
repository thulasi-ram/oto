/**
 * The org's Remedy risk rules and risk model — what says whether a Remedy needs one approval
 * or two (ADR 0054 §3, git-bug eb4f21b) — and the change waiting for a second person.
 *
 * ⛔⛔ A CHANGE FROM HERE IS PROPOSED BY ONE MEMBER AND CONFIRMED BY ANOTHER (owner ruling O3,
 * 2026-10-06, refining the 2026-10-05 "host shell only" ruling). A rule that says one lets one grant
 * holder approve alone, so a member who could write one directly could approve alone. The editor
 * therefore creates a PENDING CHANGE, which changes no Remedy's tier; it takes effect when a member
 * OTHER THAN its proposer confirms it, and this screen disables Confirm for the proposer and says why
 * rather than letting the server's 403 be the explanation. A browser session only: the host shell's
 * `oto remedy-rules apply` is still the way in from a script, and it overtakes a pending change.
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
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { confirmRemedyRiskChange, discardRemedyRiskChange } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { modelProvidersQuery, remedyRiskRulesQuery } from "~/api/queries";
import type { RemedyRiskChange, RemedyRiskRule } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";

import { RemedyRiskEditor } from "./RemedyRiskEditor";
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
  const [editing, setEditing] = createSignal(false);

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
          <Show when={risk.data}>
            <Button size="sm" variant="default" onClick={() => setEditing(true)}>
              {risk.data?.pending_change ? "Amend the proposal" : "Propose a change"}
            </Button>
          </Show>
        </PanelHeader>

        <div class={cn(PANEL_BODY, FORM)}>
          <p class={HELP} data-managed-by>
            <strong class="font-semibold text-ink">A change takes two people.</strong> One member proposes it
            here and a <em>different</em> member confirms it; nothing changes until then. From the host shell,{" "}
            <span class="font-mono text-ink">oto remedy-rules apply --org &lt;slug&gt; -f rules.yaml</span>{" "}
            still applies a file directly, and overtakes a pending proposal. A rule that says one lets one
            person approve alone, so no one member can write it by themselves.
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
                  <Show when={data().pending_change}>
                    {(change) => <PendingChange change={change()} providers={providers.data?.data ?? []} />}
                  </Show>
                  <Show
                    when={data().rules.length > 0}
                    fallback={
                      <p class={HELP} data-no-rules>
                        No rules. Every Remedy needs two approvals.
                      </p>
                    }
                  >
                    <RuleList rules={data().rules} label="Risk rules, in order" />
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

      <Show when={editing() && risk.data}>
        {(data) => (
          <RemedyRiskEditor
            amending={data().pending_change !== null}
            rules={data().pending_change?.rules ?? data().rules}
            riskModelProviderId={
              data().pending_change ? data().pending_change?.risk_model_provider_id ?? null : data().risk_model_provider_id
            }
            providers={providers.data?.data ?? []}
            onClose={() => setEditing(false)}
          />
        )}
      </Show>
    </div>
  );
};

/**
 * The change waiting for a second person: what it would make the rules, who proposed it, and the two
 * things a member can do about it.
 *
 * ⛔ THE PROPOSER IS SHOWN A DISABLED CONFIRM WITH THE REASON, never a button that works and a server
 * 403 to explain it. The server refuses the proposer regardless (and so does the database); this is
 * the screen not lying about what is possible. Discard stays open to the proposer: withdrawing is the
 * safe direction.
 */
const PendingChange: Component<{
  readonly change: RemedyRiskChange;
  readonly providers: readonly { readonly id: string; readonly name: string }[];
}> = (props) => {
  const client = useQueryClient();
  const [confirming, setConfirming] = createSignal(false);

  const settle = (): void => {
    void client.invalidateQueries({ queryKey: qk.settings.remedyRiskRules() });
  };
  const confirm = useMutation(() => ({
    mutationFn: () => confirmRemedyRiskChange(props.change.id),
    onSuccess: () => {
      setConfirming(false);
      settle();
    },
    // A change superseded or discarded meanwhile is refused; re-read so the screen shows what stands.
    onError: settle,
  }));
  const discard = useMutation(() => ({
    mutationFn: () => discardRemedyRiskChange(props.change.id),
    onSettled: settle,
  }));

  const singles = (): number => props.change.rules.filter((r) => r.approvals === 1).length;
  const modelName = (): string => {
    const id = props.change.risk_model_provider_id;
    return id === null ? "none" : (props.providers.find((p) => p.id === id)?.name ?? id);
  };

  return (
    <section
      class="flex flex-col gap-sm rounded-control border border-line-strong p-sm"
      aria-label="Proposed change"
      data-pending-change
    >
      <p class="text-item text-ink">
        <strong class="font-semibold">A change is waiting for a second person.</strong> Proposed by{" "}
        {props.change.proposed_by_you ? "you" : props.change.proposed_by_label}{" "}
        <RelativeTime value={props.change.proposed_at} label="Proposed" /> ago. Until it is confirmed, no
        Remedy's approvals change.
      </p>

      <Show
        when={props.change.rules.length > 0}
        fallback={<p class={HELP}>It proposes no rules: every Remedy would need two approvals.</p>}
      >
        <RuleList rules={props.change.rules} label="Proposed rules, in order" />
      </Show>
      <p class="text-meta text-ink">
        Risk model: <span class="font-mono">{modelName()}</span>
      </p>

      <Show when={singles() > 0}>
        <p class="text-meta text-ink" data-pending-single>
          <strong class="font-semibold">
            {singles() === 1 ? "One rule says" : `${singles()} rules say`} one approval:
          </strong>{" "}
          a Remedy that matches could be approved by a single person.
        </p>
      </Show>

      <Show when={confirm.error !== null}>
        <ErrorBanner error={confirm.error} />
      </Show>
      <Show when={discard.error !== null}>
        <ErrorBanner error={discard.error} />
      </Show>

      <div class="flex flex-wrap items-center gap-sm">
        <Button
          size="sm"
          variant="default"
          disabled={props.change.proposed_by_you}
          onClick={() => setConfirming(true)}
        >
          Confirm
        </Button>
        <Button size="sm" variant="secondary" busy={discard.isPending} onClick={() => discard.mutate()}>
          Discard
        </Button>
        <Show when={props.change.proposed_by_you}>
          <span class={HELP} data-needs-second-person>
            You proposed this, so a colleague has to confirm it. You can discard it, or amend it.
          </span>
        </Show>
      </div>

      <Modal
        open={confirming()}
        onOpenChange={(isOpen) => {
          if (!isOpen) setConfirming(false);
        }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>Confirm this change?</ModalTitle>
            <ModalDescription>
              These become the rules for every Remedy proposed from now on, and the second person this
              change needed is you. No Remedy already proposed is re-tiered.
            </ModalDescription>
          </ModalHeader>
          <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
            <RuleList rules={props.change.rules} label="Rules you are confirming" />
            <Show when={singles() > 0}>
              <p class="text-meta text-ink">
                <strong class="font-semibold">
                  {singles() === 1 ? "One of these rules says" : `${singles()} of these rules say`} one
                  approval.
                </strong>{" "}
                Confirming it lets one person approve a matching Remedy alone.
              </p>
            </Show>
            <Show when={confirm.error !== null}>
              <ErrorBanner error={confirm.error} />
            </Show>
          </div>
          <ModalFooter>
            <Button size="sm" variant="secondary" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button size="sm" variant="default" busy={confirm.isPending} onClick={() => confirm.mutate()}>
              Confirm the rules
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </section>
  );
};

/** The rules in order, each with its conditions: the same rendering for what stands and what waits. */
const RuleList: Component<{ readonly rules: readonly RemedyRiskRule[]; readonly label: string }> = (props) => (
  <ol class="flex flex-col gap-sm" aria-label={props.label}>
    <For each={props.rules}>
      {(r, i) => (
        <li class="flex flex-col gap-xs border-l-2 border-line-strong pl-sm" data-rule-row>
          <div class="flex flex-wrap items-baseline gap-sm">
            <span class="text-meta tabular-nums text-ink-muted">{i() + 1}.</span>
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
);
