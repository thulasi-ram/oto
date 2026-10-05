/**
 * A Finding's Remedies (ADR 0054, git-bug 4148256): changes to a cluster the Investigator
 * proposed, which oto executes only once one or two DIFFERENT people holding the approval
 * grant on the write ToolServer approve them — as many as the operator's risk rules said
 * when it was proposed (git-bug eb4f21b).
 *
 * ⭐⭐ THE EXACT COMMAND COMES FIRST, ABOVE THE INVESTIGATOR'S DESCRIPTION (ADR 0054 §3). An
 * approver approves the write Tool and its exact arguments — shown as the server sent them,
 * byte for byte, never re-encoded in the browser (a `JSON.parse` would round a 20-digit
 * number) — and only below them the target and the model's own account of the change. The
 * approve request names the hash of the arguments shown, so an approval of anything else is
 * refused.
 *
 * ⭐ A REMEDY WITH NO TOOL SAYS SO, AND OFFERS NO APPROVE CONTROL. "No configured Tool can
 * carry this out" is said in place of the command, and the only control is decline. A
 * Remedy whose Tool was removed from configuration since it was proposed says why it cannot
 * be approved, and offers no approve control either.
 *
 * ⭐ WHAT SET THE TIER IS SAID DIRECTLY UNDER THE COMMAND (ADR 0054 §3): the rule by name,
 * "no rule matched", "the rules could not parse this command", or "raised by the risk
 * model" — so an approver reading a one-approval Remedy can see which rule lowered it.
 *
 * ⭐ APPROVALS SO FAR, AND WHO. The count against what it needs and every name, so a
 * second approver can see they would be the second — and the first can see that approving
 * again counts once.
 *
 * ⛔ NOTHING HERE EXECUTES ANYTHING. Approving records an approval; the Remedy is executed by
 * a separate step on the server once it has the approvals it needs.
 *
 * ⚠️ WHY IT POLLS. Another approver, the expiry sweep and the executor all move a Remedy with
 * no stream frame, so while any Remedy on screen is proposed, approved or executing, the
 * list is re-read every `pollMs`; once none is, polling stops.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { approveRemedy, declineRemedy, listInvestigationRemedies } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { ListEnvelope, Remedy, RemedyRisk } from "~/api/types";
import { Button } from "~/components/ui/Button";
import { SECTION_LABEL } from "~/components/ui/surfaces";
import { ErrorBanner } from "~/components/ui/states";
import { PANEL_CODE_BLOCK } from "~/features/alerts/detail/rhythm";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";

/** How often the Remedies are re-read while one is open. */
export const REMEDY_POLL_MS = 5_000;

/** The states in which a Remedy can still change. */
const OPEN: Readonly<Record<Remedy["state"], boolean>> = {
  proposed: true,
  approved: true,
  executing: true,
  executed: false,
  failed: false,
  declined: false,
  expired: false,
};

/** Each state in words, said after the command. */
const STATE_SENTENCE: Readonly<Record<Remedy["state"], string>> = {
  proposed: "Waiting for approval.",
  approved: "Approved — waiting to be executed.",
  executing: "Being executed now.",
  executed: "Executed.",
  failed: "Failed. It is never retried; a retry is a new Remedy and a new approval.",
  declined: "Declined.",
  expired: "Expired — it can no longer be approved or executed.",
};

/** Why a Remedy failed, in words. */
const FAILURE_SENTENCE: Readonly<Record<NonNullable<Remedy["failure_reason"]>, string>> = {
  tool_error: "the write Tool answered that the call failed",
  outcome_unknown: "it was sent, or may have been, and no answer was recorded — the change may or may not have been made",
  tool_unavailable: "its Tool was gone when it was to be sent, so nothing was sent",
  arguments_changed: "its arguments no longer matched what was approved, so nothing was sent",
  approvals_withdrawn: "fewer approvers than it needs still held the grant, so nothing was sent",
};

export interface RemediesViewProps {
  readonly investigationId: string;
  /** The poll cadence while a Remedy is open. A test seam; the app uses `REMEDY_POLL_MS`. */
  readonly pollMs?: number;
}

/**
 * The Remedies one run's Finding proposed, each with the controls it admits. An empty list
 * renders nothing: a Finding that proposed no change is the ordinary case.
 */
export const RemediesView: Component<RemediesViewProps> = (props) => {
  const client = useQueryClient();
  const remedies = useQuery(() => ({
    queryKey: qk.cases.remedies(props.investigationId),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listInvestigationRemedies(props.investigationId, { signal }),
    refetchInterval: () => {
      const page = client.getQueryData<ListEnvelope<Remedy>>(qk.cases.remedies(props.investigationId));
      return page?.data.some((r) => OPEN[r.state]) === true ? (props.pollMs ?? REMEDY_POLL_MS) : false;
    },
  }));
  const [refusal, setRefusal] = createSignal<unknown>(null);
  const settle = {
    onMutate: () => setRefusal(null),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.cases.remedies(props.investigationId) }),
    onError: (err: unknown) => {
      setRefusal(err);
      void client.invalidateQueries({ queryKey: qk.cases.remedies(props.investigationId) });
    },
  };
  const approve = useMutation(() => ({
    mutationFn: (r: Remedy) => approveRemedy(r.id, r.arguments_sha256 ?? ""),
    ...settle,
  }));
  const decline = useMutation(() => ({
    mutationFn: (r: Remedy) => declineRemedy(r.id),
    ...settle,
  }));
  const busy = (): boolean => approve.isPending || decline.isPending;

  const rows = (): readonly Remedy[] => remedies.data?.data ?? [];

  return (
    <Show when={rows().length > 0}>
      <section class="mt-sm" data-remedies aria-label="Remedies the Finding proposes">
        <h3 class={cn(SECTION_LABEL, "text-ink-muted")}>Remedies</h3>
        <p class="mt-2xs text-meta text-ink-subtle">
          Changes to the cluster the model proposed. Nothing runs unless the people it needs —
          one or two different holders of the approval grant on the write ToolServer, as your
          risk rules say — approve exactly the command shown: the command, not the description.
        </p>
        <Show when={refusal()}>{(err) => <ErrorBanner class="mt-2xs" error={err()} />}</Show>
        <ul class="mt-2xs space-y-sm">
          <For each={rows()}>
            {(r) => (
              <li class="border-l-2 border-line-strong pl-sm" data-remedy={r.id} data-state={r.state}>
                <RemedyCommand remedy={r} />
                <Show when={r.tool !== null}>
                  <RemedyTier remedy={r} />
                </Show>
                <p class="mt-2xs text-body leading-snug text-ink">
                  On <span class="font-medium">{r.target}</span>
                </p>
                <p class="mt-2xs whitespace-pre-wrap break-words text-meta text-ink-muted" data-description>
                  In the model's words: {r.description}
                </p>
                <p class="mt-2xs text-meta text-ink" data-remedy-state>
                  {STATE_SENTENCE[r.state]}
                  <Show when={r.failure_reason}>
                    {(f) => <> Why: {FAILURE_SENTENCE[f()]}.</>}
                  </Show>
                  <Show when={r.detail}>{(d) => <span class="text-ink-muted"> {d()}</span>}</Show>
                </p>
                <Show when={r.result}>
                  {(res) => (
                    <pre class={cn("mt-2xs max-h-48 overflow-y-auto", PANEL_CODE_BLOCK)} data-result>
                      {res()}
                    </pre>
                  )}
                </Show>
                <Show when={r.tool !== null}>
                  <p class="mt-2xs text-meta text-ink-subtle" data-approvals>
                    {r.approvals.length} of {r.required_approvals} approvals
                    <Show when={r.approvals.length > 0}>
                      : {r.approvals.map((a) => `${a.label} (${absoluteTime(a.approved_at)})`).join(", ")}
                    </Show>
                    .
                  </p>
                </Show>
                <Show when={r.blocked !== null && r.tool !== null && OPEN[r.state]}>
                  <p class="mt-2xs text-meta font-medium text-ink" role="note" data-blocked>
                    It cannot be approved: {r.blocked}.
                  </p>
                </Show>
                <Show when={r.state === "proposed" || r.state === "approved"}>
                  <div class="mt-2xs flex flex-wrap items-center gap-sm">
                    <Show when={r.state === "proposed" && r.tool !== null && r.blocked === null}>
                      <Button
                        variant="secondary"
                        size="sm"
                        busy={approve.isPending && approve.variables?.id === r.id}
                        disabled={busy()}
                        onClick={() => approve.mutate(r)}
                        title={
                          r.required_approvals > 1
                            ? "Approve exactly the command and arguments shown above. You are recorded as one of the approvers; a second, different approver is needed before it runs."
                            : "Approve exactly the command and arguments shown above. Your risk rules say one approval is enough: it runs after yours."
                        }
                      >
                        Approve
                      </Button>
                    </Show>
                    <Button
                      variant="ghost"
                      size="sm"
                      busy={decline.isPending && decline.variables?.id === r.id}
                      disabled={busy()}
                      onClick={() => decline.mutate(r)}
                      title="Say no. It is recorded as declined by you, said on its Incident, and never executed."
                    >
                      Decline
                    </Button>
                    <span class="text-meta text-ink-subtle">
                      Expires{" "}
                      <time datetime={r.expires_at} class="tabular-nums">
                        {absoluteTime(r.expires_at)}
                      </time>
                      .
                    </span>
                  </div>
                </Show>
              </li>
            )}
          </For>
        </ul>
      </section>
    </Show>
  );
};

/**
 * The exact command: the write Tool and its arguments as the server sent them — or the
 * sentence that no configured Tool can carry it out. ⭐ Always the first thing a Remedy shows.
 */
const RemedyCommand: Component<{ readonly remedy: Remedy }> = (props) => (
  <Show
    when={props.remedy.tool}
    fallback={
      <p class="text-body font-medium leading-snug text-ink" data-command data-no-tool>
        No configured Tool can carry this out.{" "}
        <span class="font-normal text-ink-muted">
          It cannot be approved; make the change by hand, or configure a write Tool and ask for
          another Investigation.
        </span>
      </p>
    }
  >
    {(tool) => (
      <div data-command>
        <p class="text-meta text-ink-subtle">
          Runs <span class="font-mono text-ink">{`${tool().tool_server_name}__${tool().tool_name}`}</span>{" "}
          with exactly these arguments:
        </p>
        <pre class={cn("mt-2xs max-h-64 overflow-y-auto", PANEL_CODE_BLOCK)} data-arguments>
          {props.remedy.arguments_display ?? props.remedy.arguments}
        </pre>
      </div>
    )}
  </Show>
);

/** How many approvals, in words. */
function approvalsInWords(n: number): string {
  return n === 1 ? "Needs one approval" : "Needs two approvals, from different people";
}

/**
 * What set a Remedy's tier, said under its command: the rule, or why no rule could, and what
 * the risk model did. A Remedy proposed before the risk rules existed has no record and says so.
 */
const RemedyTier: Component<{ readonly remedy: Remedy }> = (props) => {
  const rule = (risk: RemedyRisk) => (
    <span class="font-mono text-ink" data-rule>
      {risk.rule}
    </span>
  );
  return (
    <p class="mt-2xs text-meta text-ink" data-tier data-set-by={props.remedy.risk?.set_by ?? "none"}>
      <span class="font-medium">{approvalsInWords(props.remedy.required_approvals)}</span>
      {" — "}
      <Show
        when={props.remedy.risk}
        fallback={<span class="text-ink-muted">proposed before risk rules existed.</span>}
      >
        {(risk) => (
          <Switch>
            <Match when={risk().set_by === "rule"}>
              set by the rule {rule(risk())}.
              <Show when={risk().risk_model_check === "kept"}>
                <span class="text-ink-muted"> The risk model was asked and kept it.</span>
              </Show>
            </Match>
            <Match when={risk().set_by === "no_rule"}>no rule matched this command.</Match>
            <Match when={risk().set_by === "unparseable"}>
              the rules could not parse this command, so it needs two whatever they say
              <Show when={risk().detail}>{(d) => <span class="text-ink-muted">: {d()}</span>}</Show>.
            </Match>
            <Match when={risk().set_by === "risk_model"}>
              raised by the risk model
              <Show when={risk().detail}>{(d) => <span class="text-ink-muted"> ({d()})</span>}</Show>; the rule{" "}
              {rule(risk())} said one.
            </Match>
            <Match when={risk().set_by === "risk_model_failed"}>
              the risk model gave no answer oto could take, so it needs two; the rule {rule(risk())} said
              one.
              <Show when={risk().detail}>{(d) => <span class="text-ink-muted"> {d()}</span>}</Show>
            </Match>
          </Switch>
        )}
      </Show>
    </p>
  );
};
