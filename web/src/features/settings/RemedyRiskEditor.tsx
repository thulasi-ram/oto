/**
 * Propose a change to the Remedy risk rules (ADR 0054 §3, owner ruling O3, 2026-10-06).
 *
 * # What saving this does, and does not do
 *
 * ⛔⛔ SAVING PROPOSES; IT DOES NOT APPLY. The rules say whether a Remedy needs one approval or two, and
 * they only ever loosen: oto ships none, so with none every Remedy needs two. A rule that says ONE lets
 * one grant holder approve a Remedy alone, so a member who could write one directly could approve alone
 * (the loophole the 2026-10-05 ruling closed). What this dialog creates is a PENDING CHANGE that a
 * DIFFERENT member confirms from the Remedy risk screen. Until then nothing about any Remedy's tier
 * changes, and the dialog says so before the operator presses the button, because a "Save" that quietly
 * did not take effect would read as a bug.
 *
 * It replaces the WHOLE rule set, like `oto remedy-rules apply`: a partial edit of an ordered list is
 * a list nobody wrote. It starts from the rules that stand (or the pending change, to amend it).
 *
 * # What it will not let slide
 *
 * A rule that says one approval must name its write Tool (the field says why: only the operator knows
 * which Tools take a kubectl-shaped command line). The server validates exactly as the CLI does, and its
 * refusals are shown against the rule and field they name, so nothing is validated twice here.
 */
import { For, Show, createSignal, type Component } from "solid-js";
import { useMutation, useQueryClient } from "@tanstack/solid-query";

import { violationsByField } from "~/api/client";
import { proposeRemedyRiskChange } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { ModelProvider, RemedyRiskRule, RemedyRiskRuleRequest } from "~/api/types";
import { Button } from "~/components/ui/Button";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import {
  Select,
  SelectContent,
  SelectHiddenSelect,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ErrorBanner } from "~/components/ui/states";
import { cn } from "~/lib/cn";

import { FIELD, FORM, HELP } from "./rhythm";

type Reversibility = "" | "reversible" | "irreversible";
type Approvals = "1" | "2";

interface DraftRule {
  /** Stable across reorders, so a field keeps its focus and its error. */
  readonly key: number;
  name: string;
  tool: string;
  verbs: string;
  kinds: string;
  namespaces: string;
  reversibility: Reversibility;
  approvals: Approvals;
}

const REVERSIBILITY: readonly Reversibility[] = ["", "reversible", "irreversible"];
const APPROVALS: readonly Approvals[] = ["1", "2"];
const NO_MODEL = "";

const REVERSIBILITY_LABEL: Readonly<Record<Reversibility, string>> = {
  "": "either",
  reversible: "reversible",
  irreversible: "irreversible",
};

let nextKey = 1;

function fromRule(r: RemedyRiskRule): DraftRule {
  return {
    key: nextKey++,
    name: r.name,
    tool: r.tool ?? "",
    verbs: r.verbs.join(", "),
    kinds: r.kinds.join(", "),
    namespaces: r.namespaces.join(", "),
    reversibility: (r.reversibility ?? "") as Reversibility,
    approvals: String(r.approvals) as Approvals,
  };
}

function blankRule(): DraftRule {
  // ⛔ NEVER PRESELECTED TO ONE. A new rule starts at two approvals — the default every Remedy has —
  // so adding a rule and walking away cannot loosen anything.
  return {
    key: nextKey++,
    name: "",
    tool: "",
    verbs: "",
    kinds: "",
    namespaces: "",
    reversibility: "",
    approvals: "2",
  };
}

const list = (s: string): string[] =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter((x) => x !== "");

function toRequest(d: DraftRule): RemedyRiskRuleRequest {
  return {
    name: d.name.trim(),
    tool: d.tool.trim() === "" ? null : d.tool.trim(),
    verbs: list(d.verbs),
    kinds: list(d.kinds),
    namespaces: list(d.namespaces),
    reversibility: d.reversibility === "" ? null : d.reversibility,
    approvals: d.approvals === "1" ? 1 : 2,
  };
}

export const RemedyRiskEditor: Component<{
  /** The rules to start from: the pending change's, to amend it, else the ones that stand. */
  readonly rules: readonly RemedyRiskRule[];
  readonly riskModelProviderId: string | null;
  readonly providers: readonly ModelProvider[];
  /** Whether it amends a change that is already waiting. */
  readonly amending: boolean;
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const [rules, setRules] = createSignal<readonly DraftRule[]>(props.rules.map(fromRule));
  const [model, setModel] = createSignal<string>(props.riskModelProviderId ?? NO_MODEL);

  const update = (key: number, patch: Partial<DraftRule>): void => {
    setRules((cur) => cur.map((r) => (r.key === key ? { ...r, ...patch } : r)));
  };
  const move = (index: number, by: -1 | 1): void => {
    setRules((cur) => {
      const to = index + by;
      if (to < 0 || to >= cur.length) return cur;
      const next = [...cur];
      const [taken] = next.splice(index, 1);
      next.splice(to, 0, taken as DraftRule);
      return next;
    });
  };

  const propose = useMutation(() => ({
    mutationFn: () =>
      proposeRemedyRiskChange({
        rules: rules().map(toRequest),
        risk_model_provider_id: model() === NO_MODEL ? null : model(),
      }),
    onSuccess: () => {
      props.onClose();
      void client.invalidateQueries({ queryKey: qk.settings.remedyRiskRules() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(propose.error);
  const errorOf = (index: number, field: string): string | undefined =>
    violations().get(`rules.${index}.${field}`);
  const singles = (): number => rules().filter((r) => r.approvals === "1").length;

  const modelName = (id: string | null): string =>
    id === null || id === NO_MODEL
      ? "none — the rules' answer stands"
      : (props.providers.find((p) => p.id === id)?.name ?? id);

  return (
    <Modal
      open
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{props.amending ? "Amend the proposed rules" : "Propose a change to the rules"}</ModalTitle>
          <ModalDescription>
            This replaces the whole rule set. <strong>Saving proposes it; it takes effect only when a
            different member confirms it.</strong> Nothing about any Remedy changes until then, and no
            Remedy already proposed is ever re-tiered.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={propose.error !== null}>
            <ErrorBanner error={propose.error} />
          </Show>

          <Show
            when={rules().length > 0}
            fallback={
              <p class={HELP}>
                No rules. Saving this proposes an empty set, which makes every Remedy need two approvals.
              </p>
            }
          >
            <ol class="flex flex-col gap-md" aria-label="Proposed rules, in order">
              <For each={rules()}>
                {(r, i) => (
                  <li
                    class="flex flex-col gap-sm rounded-control border border-line p-sm"
                    data-draft-rule
                  >
                    <div class="flex flex-wrap items-center gap-sm">
                      <span class="text-meta tabular-nums text-ink-muted">{i() + 1}.</span>
                      <TextField
                        class={cn(FIELD, "min-w-48 flex-1")}
                        value={r.name}
                        required
                        validationState={errorOf(i(), "name") ? "invalid" : "valid"}
                        onChange={(v) => update(r.key, { name: v })}
                      >
                        <TextFieldLabel>Name</TextFieldLabel>
                        <TextFieldInput id={`rule-${r.key}-name`} maxLength={63} placeholder="restart-payments" />
                        <TextFieldErrorMessage role="alert">{errorOf(i(), "name")}</TextFieldErrorMessage>
                      </TextField>
                      <div class="ml-auto flex items-center gap-xs">
                        <Button size="sm" variant="secondary" disabled={i() === 0} onClick={() => move(i(), -1)}>
                          Up
                        </Button>
                        <Button
                          size="sm"
                          variant="secondary"
                          disabled={i() === rules().length - 1}
                          onClick={() => move(i(), 1)}
                        >
                          Down
                        </Button>
                        <Button
                          size="sm"
                          variant="destructive"
                          onClick={() => setRules((cur) => cur.filter((x) => x.key !== r.key))}
                        >
                          Remove
                        </Button>
                      </div>
                    </div>

                    <TextField
                      class={FIELD}
                      value={r.tool}
                      validationState={errorOf(i(), "tool") ? "invalid" : "valid"}
                      onChange={(v) => update(r.key, { tool: v })}
                    >
                      <TextFieldLabel>Write Tool</TextFieldLabel>
                      <TextFieldInput id={`rule-${r.key}-tool`} placeholder="k8s-write__kubectl" />
                      <TextFieldDescription class={HELP}>
                        <code>&lt;toolserver&gt;__&lt;tool&gt;</code>. A rule that says one approval must name
                        it: only you know which Tools take a kubectl-shaped command line.
                      </TextFieldDescription>
                      <TextFieldErrorMessage role="alert">{errorOf(i(), "tool")}</TextFieldErrorMessage>
                    </TextField>

                    <div class="flex flex-wrap items-start gap-sm">
                      <ListField
                        id={`rule-${r.key}-verbs`}
                        label="Verbs"
                        placeholder="rollout restart, scale"
                        value={r.verbs}
                        error={errorOf(i(), "verbs")}
                        onChange={(v) => update(r.key, { verbs: v })}
                      />
                      <ListField
                        id={`rule-${r.key}-kinds`}
                        label="Kinds"
                        placeholder="deployment"
                        value={r.kinds}
                        error={errorOf(i(), "kinds")}
                        onChange={(v) => update(r.key, { kinds: v })}
                      />
                      <ListField
                        id={`rule-${r.key}-namespaces`}
                        label="Namespaces"
                        placeholder="payments"
                        value={r.namespaces}
                        error={errorOf(i(), "namespaces")}
                        onChange={(v) => update(r.key, { namespaces: v })}
                      />
                    </div>
                    <p class={HELP}>
                      Comma-separated. Every condition a rule names must hold; one it leaves empty holds for
                      any command.
                    </p>

                    <div class="flex flex-wrap items-start gap-sm">
                      <Select<Reversibility>
                        class={cn(FIELD, "w-40")}
                        options={[...REVERSIBILITY]}
                        value={r.reversibility}
                        onChange={(next) => {
                          if (next !== null) update(r.key, { reversibility: next });
                        }}
                        itemComponent={(p) => (
                          <SelectItem item={p.item}>{REVERSIBILITY_LABEL[p.item.rawValue]}</SelectItem>
                        )}
                      >
                        <SelectLabel>Reversibility</SelectLabel>
                        <SelectTrigger id={`rule-${r.key}-reversibility`}>
                          <SelectValue<Reversibility>>
                            {(state) => REVERSIBILITY_LABEL[state.selectedOption() ?? ""]}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectHiddenSelect />
                        <SelectContent />
                      </Select>

                      <Select<Approvals>
                        class={cn(FIELD, "w-40")}
                        options={[...APPROVALS]}
                        value={r.approvals}
                        onChange={(next) => {
                          if (next !== null) update(r.key, { approvals: next });
                        }}
                        itemComponent={(p) => (
                          <SelectItem item={p.item}>
                            {p.item.rawValue === "1" ? "One approval" : "Two approvals"}
                          </SelectItem>
                        )}
                      >
                        <SelectLabel>Needs</SelectLabel>
                        <SelectTrigger id={`rule-${r.key}-approvals`}>
                          <SelectValue<Approvals>>
                            {(state) => (state.selectedOption() === "1" ? "One approval" : "Two approvals")}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectHiddenSelect />
                        <SelectContent />
                      </Select>
                    </div>
                    <Show when={errorOf(i(), "approvals")}>
                      <p class="text-meta text-ink" role="alert">
                        {errorOf(i(), "approvals")}
                      </p>
                    </Show>
                  </li>
                )}
              </For>
            </ol>
          </Show>

          <div>
            <Button size="sm" variant="secondary" onClick={() => setRules((cur) => [...cur, blankRule()])}>
              Add a rule
            </Button>
          </div>

          <Select<string>
            class={FIELD}
            options={[NO_MODEL, ...props.providers.map((p) => p.id)]}
            value={model()}
            onChange={(next) => {
              if (next !== null) setModel(next);
            }}
            itemComponent={(p) => <SelectItem item={p.item}>{modelName(p.item.rawValue)}</SelectItem>}
          >
            <SelectLabel>Risk model</SelectLabel>
            <SelectTrigger id="remedy-risk-model">
              <SelectValue<string>>{(state) => modelName(state.selectedOption())}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
          <p class={HELP}>
            Asked only about a Remedy the rules say needs one approval, and only ever to raise it to two.
          </p>

          <Show when={singles() > 0}>
            <p
              class="rounded-control border border-line bg-surface-subtle p-sm text-meta text-ink"
              role="note"
              data-one-approval-notice
            >
              <strong class="font-semibold">
                {singles() === 1 ? "One rule says" : `${singles()} rules say`} one approval.
              </strong>{" "}
              A Remedy that matches {singles() === 1 ? "it" : "one"} can be approved by a single person. The
              member who confirms this change is agreeing to that.
            </p>
          </Show>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button size="sm" variant="default" busy={propose.isPending} onClick={() => propose.mutate()}>
            {props.amending ? "Propose the amended rules" : "Propose this change"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

const ListField: Component<{
  readonly id: string;
  readonly label: string;
  readonly placeholder: string;
  readonly value: string;
  readonly error: string | undefined;
  readonly onChange: (next: string) => void;
}> = (props) => (
  <TextField
    class={cn(FIELD, "min-w-40 flex-1")}
    value={props.value}
    validationState={props.error ? "invalid" : "valid"}
    onChange={props.onChange}
  >
    <TextFieldLabel>{props.label}</TextFieldLabel>
    <TextFieldInput id={props.id} placeholder={props.placeholder} />
    <TextFieldErrorMessage role="alert">{props.error}</TextFieldErrorMessage>
  </TextField>
);
