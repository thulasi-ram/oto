/**
 * The org's Remedy risk rules and risk model — what says whether a Remedy needs one approval
 * or two (ADR 0054 §3, git-bug eb4f21b).
 *
 * ⛔ OTO SHIPS NO RULE, AND THIS SCREEN OFFERS NONE. With no rules every Remedy needs two
 * different approvers; a rule exists to lower that for a command the operator judges harmless,
 * never to be the only thing between a command and one approval.
 *
 * ⭐ THE MOST SEVERE MATCHING RULE WINS, AND THE SCREEN SAYS SO. Order only decides which rule
 * a Remedy names; a rule saying two cannot be outvoted by one above it. No match is two, and a
 * command the rules cannot parse (`sh -c`, a pipe, a redirect…) is two whatever they say.
 *
 * ⭐ THE RISK MODEL MAY ONLY RAISE. It is asked only about a Remedy the rules said needs one,
 * sees only the command, its target and the rules' verdict — never the Investigation — and a
 * model that fails leaves two.
 *
 * ⭐ SAVED WHOLE, like the Classification set, and the last writer is shown: a rule that says
 * one lets one grant holder approve alone, so who changed the rules is worth seeing. Saving
 * re-tiers no Remedy already proposed.
 */
import { For, Match, Show, Switch, createEffect, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { orphanViolations, violationsByField } from "~/api/client";
import { getRemedyRiskRules, replaceRemedyRiskRules } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { modelProvidersQuery } from "~/api/queries";
import type { RemedyRiskRule, RemedyRiskRules } from "~/api/types";
import { Button } from "~/components/ui/Button";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime } from "~/lib/format";

import { FIELD, FIELD_ROW, FORM, HELP, PANEL_BODY, PANEL_HEADER, SECTION } from "./rhythm";

/** The list's ceiling, as the server enforces it (`domain.MaxRiskRules`). */
const MAX_RULES = 100;

type Reversibility = "either" | "reversible" | "irreversible";

/** One rule of the draft, its lists as the comma-separated text the operator types. */
interface DraftRule {
  readonly key: number;
  readonly name: string;
  readonly tool: string;
  readonly verbs: string;
  readonly kinds: string;
  readonly namespaces: string;
  readonly reversibility: Reversibility;
  readonly approvals: 1 | 2;
}

let nextKey = 0;
function draftOf(r: RemedyRiskRule): DraftRule {
  return {
    key: nextKey++,
    name: r.name,
    tool: r.tool ?? "",
    verbs: r.verbs.join(", "),
    kinds: r.kinds.join(", "),
    namespaces: r.namespaces.join(", "),
    reversibility: r.reversibility ?? "either",
    approvals: r.approvals === 1 ? 1 : 2,
  };
}

/** A comma-separated field as the list the API takes; blanks dropped. */
function listOf(text: string): string[] {
  return text
    .split(",")
    .map((v) => v.trim())
    .filter((v) => v !== "");
}

export const RemedyRiskSection: Component = () => {
  const client = useQueryClient();
  const risk = useQuery(() => ({
    queryKey: qk.settings.remedyRiskRules(),
    queryFn: ({ signal }: { signal: AbortSignal }) => getRemedyRiskRules({ signal }),
  }));
  const providers = useQuery(() => modelProvidersQuery());

  const [draft, setDraft] = createSignal<readonly DraftRule[]>([]);
  const [model, setModel] = createSignal<string>("");
  const [dirty, setDirty] = createSignal(false);

  // The draft follows the server until the operator touches it.
  createEffect(() => {
    const data = risk.data;
    if (data !== undefined && !dirty()) {
      setDraft(data.rules.map(draftOf));
      setModel(data.risk_model_provider_id ?? "");
    }
  });

  const save = useMutation(() => ({
    mutationFn: () =>
      replaceRemedyRiskRules({
        rules: draft().map((r) => ({
          name: r.name.trim(),
          ...(r.tool.trim() !== "" ? { tool: r.tool.trim() } : {}),
          verbs: listOf(r.verbs),
          kinds: listOf(r.kinds),
          namespaces: listOf(r.namespaces),
          ...(r.reversibility !== "either" ? { reversibility: r.reversibility } : {}),
          approvals: r.approvals,
        })),
        risk_model_provider_id: model() === "" ? null : model(),
      }),
    onSuccess: (stored: RemedyRiskRules) => {
      client.setQueryData(qk.settings.remedyRiskRules(), stored);
      setDraft(stored.rules.map(draftOf));
      setModel(stored.risk_model_provider_id ?? "");
      setDirty(false);
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(save.error);
  const fieldsOf = (i: number): string[] =>
    ["", ".name", ".tool", ".verbs", ".kinds", ".namespaces", ".reversibility", ".approvals"].map(
      (f) => `rules.${i}${f}`,
    );
  const known = (): readonly string[] => [...draft().flatMap((_, i) => fieldsOf(i)), "risk_model_provider_id"];
  /** The first violation about one field of a rule, its list entries included. */
  const errorOf = (i: number, field: string): string | undefined => {
    const prefix = `rules.${i}.${field}`;
    for (const [k, v] of violations()) if (k === prefix || k.startsWith(prefix + ".")) return v;
    return undefined;
  };

  const edit = (key: number, patch: Partial<Omit<DraftRule, "key">>): void => {
    setDraft((rows) => rows.map((r) => (r.key === key ? { ...r, ...patch } : r)));
    setDirty(true);
  };
  const add = (): void => {
    setDraft((rows) => [
      ...rows,
      { key: nextKey++, name: "", tool: "", verbs: "", kinds: "", namespaces: "", reversibility: "either", approvals: 1 },
    ]);
    setDirty(true);
  };
  const remove = (key: number): void => {
    setDraft((rows) => rows.filter((r) => r.key !== key));
    setDirty(true);
  };
  const revert = (): void => {
    setDraft((risk.data?.rules ?? []).map(draftOf));
    setModel(risk.data?.risk_model_provider_id ?? "");
    setDirty(false);
    save.reset();
  };

  const text = (
    row: DraftRule,
    i: number,
    field: "name" | "tool" | "verbs" | "kinds" | "namespaces",
    label: string,
    placeholder: string,
    width: string,
  ) => (
    <TextField
      class={cn(FIELD, width)}
      value={row[field]}
      required={field === "name"}
      validationState={errorOf(i, field) ? "invalid" : "valid"}
      onChange={(v) => edit(row.key, { [field]: v })}
    >
      <TextFieldLabel>{label}</TextFieldLabel>
      <TextFieldInput
        class={field === "name" || field === "tool" ? "font-mono" : undefined}
        placeholder={placeholder}
        aria-label={`Rule ${i + 1} ${label.toLowerCase()}`}
      />
      <TextFieldErrorMessage role="alert">{errorOf(i, field)}</TextFieldErrorMessage>
    </TextField>
  );

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Remedy risk</PanelTitle>
        </PanelHeader>

        <div class={cn(PANEL_BODY, FORM)}>
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
            <Match when={true}>
              <Show when={save.error !== null && orphanViolations(save.error, known()).length > 0}>
                <ErrorBanner error={save.error} />
              </Show>

              <Show
                when={draft().length > 0}
                fallback={
                  <p class={HELP} data-no-rules>
                    No rules. Every Remedy needs two approvals.
                  </p>
                }
              >
                <ol class={FORM} aria-label="Risk rules, in order">
                  <For each={draft()}>
                    {(row, i) => (
                      <li class="flex flex-col gap-sm border-l-2 border-line-strong pl-sm" data-rule-row>
                        <div class={FIELD_ROW}>
                          {text(row, i(), "name", "Name", "restart-payments", "w-48")}
                          {text(row, i(), "tool", "Write Tool", "k8s-write__kubectl (any)", "w-56")}
                          <ToggleGroup
                            showLegend
                            legend="Approvals"
                            value={String(row.approvals)}
                            onChange={(v) => v !== null && edit(row.key, { approvals: v === "1" ? 1 : 2 })}
                          >
                            <ToggleGroupItem value="1" aria-label={`Rule ${i() + 1}: one approval`}>
                              One
                            </ToggleGroupItem>
                            <ToggleGroupItem value="2" aria-label={`Rule ${i() + 1}: two approvals`}>
                              Two
                            </ToggleGroupItem>
                          </ToggleGroup>
                          <Button
                            size="sm"
                            variant="ghost"
                            class="mt-lg ml-auto"
                            aria-label={`Remove rule ${row.name || i() + 1}`}
                            onClick={() => remove(row.key)}
                          >
                            Remove
                          </Button>
                        </div>
                        <div class={FIELD_ROW}>
                          {text(row, i(), "verbs", "Verbs", "rollout restart, scale", "min-w-0 flex-1")}
                          {text(row, i(), "kinds", "Kinds", "deployment", "min-w-0 flex-1")}
                          {text(row, i(), "namespaces", "Namespaces", "payments", "min-w-0 flex-1")}
                          <ToggleGroup
                            showLegend
                            legend="Reversibility"
                            value={row.reversibility}
                            onChange={(v) => v !== null && edit(row.key, { reversibility: v as Reversibility })}
                          >
                            <ToggleGroupItem value="either">Either</ToggleGroupItem>
                            <ToggleGroupItem value="reversible">Reversible</ToggleGroupItem>
                            <ToggleGroupItem value="irreversible">Irreversible</ToggleGroupItem>
                          </ToggleGroup>
                        </div>
                        <Show when={errorOf(i(), "approvals") ?? violations().get(`rules.${i()}`)}>
                          {(msg) => (
                            <p class="text-meta text-ink" role="alert">
                              {msg()}
                            </p>
                          )}
                        </Show>
                      </li>
                    )}
                  </For>
                </ol>
              </Show>

              <p class={HELP}>
                Every condition a rule names must hold; leave one empty for any. Separate several verbs, kinds
                or namespaces with commas. A command that names no namespace, or all of them, matches no
                namespace. Reversible verbs:{" "}
                <span class="font-mono text-ink">{(risk.data?.reversible_verbs ?? []).join(", ")}</span> — every
                other verb is irreversible.
              </p>

              <div class={FIELD}>
                <ToggleGroup
                  showLegend
                  legend="Risk model"
                  value={model()}
                  onChange={(v) => {
                    if (v === null) return;
                    setModel(v);
                    setDirty(true);
                  }}
                >
                  <ToggleGroupItem value="">None</ToggleGroupItem>
                  <For each={providers.data?.data ?? []}>
                    {(p) => (
                      <ToggleGroupItem value={p.id} title={`${p.base_url} · ${p.model}`}>
                        {p.name}
                      </ToggleGroupItem>
                    )}
                  </For>
                </ToggleGroup>
                <p class={HELP} data-risk-model-help>
                  Asked only about a Remedy the rules say needs one approval, and only ever to raise it to two.
                  It sees the command, its target and the rules' verdict — never the Investigation, its logs or
                  its Finding. If it fails or answers anything but one or two, the Remedy needs two. With none,
                  the rules' answer stands.
                </p>
                <Show when={violations().get("risk_model_provider_id")}>
                  {(msg) => (
                    <p class="text-meta text-ink" role="alert">
                      {msg()}
                    </p>
                  )}
                </Show>
              </div>

              <div class="flex flex-wrap items-center gap-sm">
                <Button size="sm" variant="secondary" disabled={draft().length >= MAX_RULES} onClick={add}>
                  Add a rule
                </Button>
                <div class="ml-auto flex items-center gap-sm">
                  <Show when={dirty()}>
                    <Button size="sm" variant="ghost" onClick={revert}>
                      Revert
                    </Button>
                  </Show>
                  <Button
                    size="sm"
                    variant="default"
                    busy={save.isPending}
                    disabled={!dirty()}
                    onClick={() => save.mutate()}
                  >
                    Save rules
                  </Button>
                </div>
              </div>
            </Match>
          </Switch>
        </div>

        <div class={cn(PANEL_BODY, HELP, "flex flex-col gap-xs border-t border-line")}>
          <Show when={risk.data?.written_by_label}>
            {(by) => (
              <p data-written-by>
                Last written by {by()}
                <Show when={risk.data?.written_at}>{(at) => <> at {absoluteTime(at())}</>}</Show>.
              </p>
            )}
          </Show>
          <p data-one-approval-warning>
            <strong class="font-semibold text-ink">A rule that says one lets one person approve alone.</strong>{" "}
            Treat a change here like a change to who may approve. Saving decides the next Remedy's approvals; it
            never changes a Remedy already proposed.
          </p>
        </div>
      </Panel>
    </div>
  );
};
