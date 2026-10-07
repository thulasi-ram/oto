/**
 * Investigators — the named, versioned configurations oto runs against a Case, an Incident or a
 * digest window (ADR 0053).
 *
 * # What changes a version and what does not
 *
 * Changing the model endpoint, the prompt or the Tool allowlist writes version N+1, and a Finding
 * names the version that produced it. The switch, the budgets, the interval and
 * `investigates_incidents` change in place and never version. The edit dialog says which of the two
 * the operator's change is *before* they save, because "saved" reading the same for both would hide
 * that a prompt edit starts a new line of history.
 *
 * ⛔ A VERSION DOES NOT PIN YOUR CLASSIFICATION SET OR A TOOLSERVER'S RE-DISCOVERED TOOLS. Those are
 * read when each run starts; the panel says so rather than letting "versioned" suggest otherwise.
 *
 * # The allowlist
 *
 * It names Tools exactly, with no wildcards, so it is a checklist and not a text field. It offers
 * oto's own Tools, and the usable Tools of every `read` ToolServer. A `write` ToolServer's Tools are
 * not offered at all: the server refuses them (ADR 0053 §6), and a checkbox that could only ever
 * answer 422 is a trap, not a control.
 *
 * `name` is never renamed, because a Finding is published as the Enrichment `investigator.<name>`.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { A } from "@solidjs/router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf, patternOf, rangeOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import { createInvestigator, updateInvestigator } from "~/api/endpoints";
import {
  CreateInvestigatorRequestSchema,
  InvestigatorBudgetsDTOSchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import {
  investigatorQuery,
  investigatorsQuery,
  modelProvidersQuery,
  toolServerToolsQuery,
  toolServersQuery,
} from "~/api/queries";
import type {
  Investigator,
  InvestigatorBudgets,
  ModelProvider,
  ToolServer,
  UpdateInvestigatorRequest,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { Checkbox } from "~/components/ui/Checkbox";
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
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";

import {
  KNOWN_BUILTIN,
  PROPOSING_TOOLS,
  READING_TOOLS,
  type BuiltinTool,
} from "./investigatorTools";
import { CHECK_LABEL, CHECK_ROW, FIELD, FORM, HELP, PANEL_BODY, PANEL_HEADER, ROW, SECTION } from "./rhythm";

/** ⛔ Read off the generated schema, never typed here — `TokensSection`'s rule. */
const NAME_MAX = maxLengthOf(CreateInvestigatorRequestSchema, "name");
const NAME_PATTERN = patternOf(CreateInvestigatorRequestSchema, "name");
const PROMPT_MAX = maxLengthOf(CreateInvestigatorRequestSchema, "prompt");
const INTERVAL = rangeOf(CreateInvestigatorRequestSchema, "min_interval_seconds");
const STEPS = rangeOf(InvestigatorBudgetsDTOSchema, "max_steps");
const TOKENS = rangeOf(InvestigatorBudgetsDTOSchema, "max_tokens");
const WALL = rangeOf(InvestigatorBudgetsDTOSchema, "max_wall_seconds");

/** What a new Investigator starts with; the documented defaults, which the server also applies. */
const DEFAULT_BUDGETS: InvestigatorBudgets = { max_steps: 20, max_tokens: 200_000, max_wall_seconds: 300 };
const DEFAULT_INTERVAL = 600;

export const InvestigatorsSection: Component = () => {
  const [editing, setEditing] = createSignal<Investigator | "new" | null>(null);
  const investigators = useQuery(investigatorsQuery);

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Investigators</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setEditing("new")}>
            Add an Investigator
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={investigators.isPending}>
            <LoadingLine />
          </Match>
          <Match when={investigators.isError}>
            <ErrorState error={investigators.error} onRetry={() => void investigators.refetch()} />
          </Match>
          <Match when={(investigators.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No Investigator is configured."
              body="An Investigator pairs a model with a prompt and the Tools it may read. Add a model provider first, then one here; a human then asks it about a Case or an Incident."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={investigators.data?.data ?? []}>
                {(i) => <InvestigatorRow investigator={i} onEdit={() => setEditing(i)} />}
              </For>
            </ul>
          </Match>
        </Switch>

        <p class={cn(PANEL_BODY, HELP, "border-t border-line")}>
          An Investigator reads, proposes and never decides who is told: a Finding changes what
          people read, not whether a notification is sent. No Case is investigated automatically; an
          Incident is only for Investigators that opt in. A version pins the model, prompt and
          allowlist, but not your classification set or a ToolServer's re-discovered Tools.
        </p>
      </Panel>

      <Show when={editing() !== null}>
        <EditDialog target={editing() as Investigator | "new"} onClose={() => setEditing(null)} />
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const InvestigatorRow: Component<{
  readonly investigator: Investigator;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const i = (): Investigator => props.investigator;
  const [history, setHistory] = createSignal(false);

  const flip = useMutation(() => ({
    mutationFn: () => updateInvestigator(i().id, { enabled: !i().enabled }),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.investigators() }),
  }));

  return (
    <li class={cn(ROW, "flex flex-col gap-sm")}>
      <div class="flex min-h-12 flex-wrap items-center gap-sm">
        <span class="text-item font-medium text-ink">{i().name}</span>
        <Chip title="Its Findings are published as this Enrichment.">{i().enricher}</Chip>
        <Chip
          title={
            i().enabled
              ? "On."
              : "Off: nothing starts, and a human's request is recorded skipped with the reason."
          }
        >
          {i().enabled ? "enabled" : "disabled"}
        </Chip>
        <Chip mono title="The version a new Investigation would pin.">
          v{i().current_version.version}
        </Chip>
        <Chip mono title={i().current_version.model.endpoint}>
          {i().current_version.model.name}
        </Chip>
        <Chip title="The Tools this version may call.">{i().current_version.tools.length} tools</Chip>
        <Show when={i().investigates_incidents}>
          <Chip title="An Incident being drawn, or its membership changing, starts a run on its own.">
            auto on Incidents
          </Chip>
        </Show>
        <div class="ml-auto flex items-center gap-sm">
          <Button size="sm" variant="secondary" onClick={() => setHistory(!history())}>
            {history() ? "Hide versions" : "Versions"}
          </Button>
          <Button size="sm" variant="secondary" busy={flip.isPending} onClick={() => flip.mutate()}>
            {i().enabled ? "Switch off" : "Switch on"}
          </Button>
          <Button size="sm" variant="default" onClick={props.onEdit}>
            Edit
          </Button>
        </div>
      </div>
      <Show when={flip.error !== null}>
        <ErrorBanner error={flip.error} />
      </Show>
      <Show when={history()}>
        <Versions id={i().id} />
      </Show>
    </li>
  );
};

/** Every version, newest first. Read only while open: it is the whole transcript of change. */
const Versions: Component<{ readonly id: string }> = (props) => {
  const detail = useQuery(() => investigatorQuery(props.id));
  return (
    <Switch>
      <Match when={detail.isPending}>
        <LoadingLine />
      </Match>
      <Match when={detail.isError}>
        <ErrorState error={detail.error} onRetry={() => void detail.refetch()} />
      </Match>
      <Match when={true}>
        <ol class="flex flex-col gap-sm">
          <For each={detail.data?.versions ?? []}>
            {(v) => (
              <li class="flex flex-col gap-0.5 text-meta">
                <span class="text-ink">
                  <strong class="font-semibold">v{v.version}</strong> · {v.model.name} ·{" "}
                  {v.tools.length} tools · <RelativeTime value={v.created_at} label="Written" /> ago
                </span>
                <span class="font-mono text-ink-subtle">{v.tools.join(", ") || "no Tools"}</span>
                <details>
                  <summary class="cursor-pointer text-ink-subtle">Prompt</summary>
                  <pre class="whitespace-pre-wrap text-ink-subtle">{v.prompt}</pre>
                </details>
              </li>
            )}
          </For>
        </ol>
      </Match>
    </Switch>
  );
};

/* -------------------------------------------------------------------------- */

const sameSet = (a: readonly string[], b: readonly string[]): boolean =>
  a.length === b.length && [...a].sort().every((x, n) => x === [...b].sort()[n]);

const EditDialog: Component<{
  readonly target: Investigator | "new";
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const existing = (): Investigator | null => (props.target === "new" ? null : props.target);
  const current = existing();

  const [name, setName] = createSignal("");
  const [providerId, setProviderId] = createSignal<string | null>(
    current?.current_version.model_provider_id ?? null,
  );
  const [prompt, setPrompt] = createSignal(current?.current_version.prompt ?? "");
  const [tools, setTools] = createSignal<readonly string[]>(current?.current_version.tools ?? []);
  const [steps, setSteps] = createSignal(String((current?.budgets ?? DEFAULT_BUDGETS).max_steps));
  const [tokens, setTokens] = createSignal(String((current?.budgets ?? DEFAULT_BUDGETS).max_tokens));
  const [wall, setWall] = createSignal(String((current?.budgets ?? DEFAULT_BUDGETS).max_wall_seconds));
  const [interval, setIntervalSeconds] = createSignal(
    String(current?.min_interval_seconds ?? DEFAULT_INTERVAL),
  );
  const [incidents, setIncidents] = createSignal(current?.investigates_incidents ?? false);
  const [enabled, setEnabled] = createSignal(current?.enabled ?? true);

  const providers = useQuery(modelProvidersQuery);

  const budgets = (): InvestigatorBudgets => ({
    max_steps: Number(steps()),
    max_tokens: Number(tokens()),
    max_wall_seconds: Number(wall()),
  });

  /** Whether this edit writes version N+1: model endpoint, prompt or allowlist. */
  const touchesVersion = (): boolean => {
    const c = existing();
    if (c === null) return false;
    return (
      providerId() !== c.current_version.model_provider_id ||
      prompt() !== c.current_version.prompt ||
      !sameSet(tools(), c.current_version.tools)
    );
  };

  const changes = (): UpdateInvestigatorRequest => {
    const c = existing();
    if (c === null) return {};
    const b = budgets();
    const out: {
      -readonly [K in keyof UpdateInvestigatorRequest]: UpdateInvestigatorRequest[K];
    } = {};
    if (enabled() !== c.enabled) out.enabled = enabled();
    if (
      b.max_steps !== c.budgets.max_steps ||
      b.max_tokens !== c.budgets.max_tokens ||
      b.max_wall_seconds !== c.budgets.max_wall_seconds
    ) {
      out.budgets = b;
    }
    if (Number(interval()) !== c.min_interval_seconds) out.min_interval_seconds = Number(interval());
    if (incidents() !== c.investigates_incidents) out.investigates_incidents = incidents();
    const pid = providerId();
    if (pid !== null && pid !== c.current_version.model_provider_id) out.model_provider_id = pid;
    if (prompt() !== c.current_version.prompt) out.prompt = prompt();
    if (!sameSet(tools(), c.current_version.tools)) out.tools = [...tools()];
    return out;
  };

  const save = useMutation(() => ({
    mutationFn: () => {
      const c = existing();
      if (c !== null) return updateInvestigator(c.id, changes());
      const pid = providerId();
      if (pid === null) throw new Error("a model provider must be chosen");
      return createInvestigator({
        name: name().trim(),
        enabled: enabled(),
        budgets: budgets(),
        min_interval_seconds: Number(interval()),
        investigates_incidents: incidents(),
        model_provider_id: pid,
        prompt: prompt(),
        tools: [...tools()],
      });
    },
    onSuccess: () => {
      props.onClose();
      void client.invalidateQueries({ queryKey: qk.settings.investigators() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(save.error);
  const nameOk = (): boolean => NAME_PATTERN.test(name());
  const ready = (): boolean =>
    providerId() !== null &&
    prompt().trim() !== "" &&
    (existing() !== null ? Object.keys(changes()).length > 0 : nameOk());

  const toggle = (tool: string, on: boolean): void => {
    setTools((cur) => (on ? [...new Set([...cur, tool])].sort() : cur.filter((t) => t !== tool)));
  };

  return (
    <Modal
      open
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {current === null ? "Add an Investigator" : `Edit ${current.name}`}
          </ModalTitle>
          <ModalDescription>
            A model, a prompt and the Tools it may read. It reads, proposes and never decides who is
            told.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={save.error !== null}>
            <ErrorBanner error={save.error} />
          </Show>

          <Show when={current === null}>
            <TextField
              class={FIELD}
              value={name()}
              required
              validationState={
                violations().get("name") || (name() !== "" && !nameOk()) ? "invalid" : "valid"
              }
              onChange={setName}
            >
              <TextFieldLabel>Name</TextFieldLabel>
              <TextFieldInput id="investigator-name" maxLength={NAME_MAX} placeholder="triage1" />
              <TextFieldDescription class={HELP}>
                Lower-case letters and digits, starting with a letter. It can never be renamed,
                because its Findings are published as <code>investigator.&lt;name&gt;</code>.
              </TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {violations().get("name") ??
                  (name() !== "" && !nameOk() ? "Lower-case letters and digits, starting with a letter." : "")}
              </TextFieldErrorMessage>
            </TextField>
          </Show>

          <ProviderSelect
            providers={providers.data?.data ?? []}
            loading={providers.isPending}
            value={providerId()}
            onChange={setProviderId}
          />

          <TextField
            class={FIELD}
            value={prompt()}
            required
            validationState={violations().get("prompt") ? "invalid" : "valid"}
            onChange={setPrompt}
          >
            <TextFieldLabel>Prompt</TextFieldLabel>
            <TextFieldTextArea id="investigator-prompt" rows={6} maxLength={PROMPT_MAX} />
            <TextFieldDescription class={HELP}>
              What the model is told it is for. Changing it writes a new version.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">{violations().get("prompt")}</TextFieldErrorMessage>
          </TextField>

          <ToolPicker selected={tools()} onToggle={toggle} />

          <fieldset class="flex flex-col gap-sm">
            <legend class="text-meta font-medium text-ink">Budgets, per run</legend>
            <div class="flex flex-wrap items-start gap-sm">
              <NumberField id="inv-steps" label="Tool calls" value={steps()} range={STEPS} onChange={setSteps} />
              <NumberField id="inv-tokens" label="Tokens" value={tokens()} range={TOKENS} onChange={setTokens} />
              <NumberField id="inv-wall" label="Seconds" value={wall()} range={WALL} onChange={setWall} />
            </div>
            <p class={HELP}>
              Hitting any of them ends the run <em>exhausted</em>; the Finding it reached is kept and
              marked partial. Tokens count input and output together.
            </p>
          </fieldset>

          <NumberField
            id="inv-interval"
            label="Minimum seconds between runs on one subject"
            value={interval()}
            range={INTERVAL}
            onChange={setIntervalSeconds}
          />
          <p class={HELP}>
            Bounds runs an Incident starts on its own; triggers inside it coalesce into one. A
            human asking is never held to it. 0 runs on every trigger.
          </p>

          <div class={CHECK_ROW}>
            <Checkbox id="inv-incidents" checked={incidents()} onChange={setIncidents} />
            <label for="inv-incidents-input" class={CHECK_LABEL}>
              Investigate Incidents on its own
            </label>
          </div>
          <p class={HELP}>
            Off by default: automatic runs cost tokens, so you opt in. When on, an Incident being
            drawn, or its membership changing, starts a run. A human may ask any Investigator about
            any Incident regardless.
          </p>

          <div class={CHECK_ROW}>
            <Checkbox id="inv-enabled" checked={enabled()} onChange={setEnabled} />
            <label for="inv-enabled-input" class={CHECK_LABEL}>
              Enabled
            </label>
          </div>

          <Show when={touchesVersion()}>
            <p class="rounded-control border border-line bg-surface-subtle p-sm text-meta text-ink">
              This change writes <strong>version {(current?.current_version.version ?? 0) + 1}</strong>.
              Earlier Findings keep naming the version that produced them.
            </p>
          </Show>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={save.isPending}
            disabled={!ready()}
            onClick={() => save.mutate()}
          >
            {current === null ? "Add Investigator" : "Save"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */

const NumberField: Component<{
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly range: { readonly min: number; readonly max: number };
  readonly onChange: (next: string) => void;
}> = (props) => {
  const bad = (): boolean => {
    const n = Number(props.value);
    return props.value.trim() === "" || !Number.isInteger(n) || n < props.range.min || n > props.range.max;
  };
  return (
    <TextField
      class={cn(FIELD, "min-w-32")}
      value={props.value}
      validationState={bad() ? "invalid" : "valid"}
      onChange={props.onChange}
    >
      <TextFieldLabel>{props.label}</TextFieldLabel>
      <TextFieldInput
        id={props.id}
        type="number"
        min={props.range.min}
        max={props.range.max}
        step={1}
      />
      <TextFieldErrorMessage role="alert">
        {bad() ? `${props.range.min}–${props.range.max}` : ""}
      </TextFieldErrorMessage>
    </TextField>
  );
};

const ProviderSelect: Component<{
  readonly providers: readonly ModelProvider[];
  readonly loading: boolean;
  readonly value: string | null;
  readonly onChange: (next: string | null) => void;
}> = (props) => {
  const nameOf = (id: string | null | undefined): string => {
    const p = props.providers.find((x) => x.id === id);
    return p === undefined ? "" : `${p.name} — ${p.model}`;
  };
  return (
    <Show
      when={props.loading || props.providers.length > 0}
      fallback={
        <p class={HELP}>
          No model provider exists yet. <A href="/settings/model-providers">Add one first</A>: an
          Investigator needs an endpoint to reason with.
        </p>
      }
    >
      <Select<string>
        class={FIELD}
        options={props.providers.map((p) => p.id)}
        value={props.value}
        onChange={props.onChange}
        placeholder="Choose…"
        itemComponent={(itemProps) => (
          <SelectItem item={itemProps.item}>{nameOf(itemProps.item.rawValue)}</SelectItem>
        )}
      >
        <SelectLabel>Model provider</SelectLabel>
        <SelectTrigger id="investigator-provider">
          <SelectValue<string>>{(state) => nameOf(state.selectedOption())}</SelectValue>
        </SelectTrigger>
        <SelectHiddenSelect />
        <SelectContent />
      </Select>
      <p class={HELP}>
        Its URL and model are pinned into each version, so a Finding names the model that wrote it.
        Changing the provider writes a new version.
      </p>
    </Show>
  );
};

/* -------------------------------------------------------------------------- */

const ToolPicker: Component<{
  readonly selected: readonly string[];
  readonly onToggle: (tool: string, on: boolean) => void;
}> = (props) => {
  const servers = useQuery(toolServersQuery);
  const readServers = (): readonly ToolServer[] =>
    (servers.data?.data ?? []).filter((s) => s.access === "read");
  const writeCount = (): number => (servers.data?.data ?? []).filter((s) => s.access === "write").length;

  return (
    <fieldset class="flex flex-col gap-sm">
      <legend class="text-meta font-medium text-ink">Tools it may call</legend>
      <ToolGroup title="oto's own history" tools={READING_TOOLS} {...props} />
      <ToolGroup title="Proposing — a human decides" tools={PROPOSING_TOOLS} {...props} />
      <For each={readServers()}>{(s) => <ServerGroup server={s} {...props} />}</For>
      <Show when={readServers().length === 0 && !servers.isPending}>
        <p class={HELP}>
          No read ToolServer is configured, so only oto's own Tools are offered. Add one under Tool
          servers to let it look at your systems.
        </p>
      </Show>
      <Show when={writeCount() > 0}>
        <p class={HELP}>
          A write ToolServer's Tools are never offered: no Investigator holds one. They belong to a
          Remedy.
        </p>
      </Show>
      <Held selected={props.selected} servers={readServers()} onToggle={props.onToggle} />
    </fieldset>
  );
};

interface PickerProps {
  readonly selected: readonly string[];
  readonly onToggle: (tool: string, on: boolean) => void;
}

const ToolGroup: Component<PickerProps & { readonly title: string; readonly tools: readonly BuiltinTool[] }> = (
  props,
) => (
  <div class="flex flex-col gap-xs">
    <span class="text-micro font-medium uppercase tracking-[0.08em] text-ink-subtle">{props.title}</span>
    <For each={props.tools}>
      {(t) => <ToolCheck name={t.name} help={t.help} {...props} />}
    </For>
  </div>
);

const ServerGroup: Component<PickerProps & { readonly server: ToolServer }> = (props) => {
  const tools = useQuery(() => toolServerToolsQuery(props.server.id));
  const usable = () => (tools.data?.data ?? []).filter((t) => t.usable && t.qualified_name !== null);
  return (
    <div class="flex flex-col gap-xs">
      <span class="text-micro font-medium uppercase tracking-[0.08em] text-ink-subtle">
        {props.server.name} (read)
      </span>
      <Switch>
        <Match when={tools.isPending}>
          <LoadingLine />
        </Match>
        <Match when={tools.isError}>
          <ErrorState error={tools.error} onRetry={() => void tools.refetch()} />
        </Match>
        <Match when={usable().length === 0}>
          <p class={HELP}>
            No usable Tools listed. Run Discover on <strong>{props.server.name}</strong> under Tool
            servers.
          </p>
        </Match>
        <Match when={true}>
          <For each={usable()}>
            {(t) => (
              <ToolCheck
                name={t.qualified_name ?? t.name}
                help={t.description}
                selected={props.selected}
                onToggle={props.onToggle}
              />
            )}
          </For>
        </Match>
      </Switch>
    </div>
  );
};

/**
 * Names already on the allowlist that no group above offers: a ToolServer since removed from
 * `read`, one not yet discovered, or a built-in this build does not know. Shown checked, so they can
 * be removed instead of being carried forward unseen.
 */
const Held: Component<PickerProps & { readonly servers: readonly ToolServer[] }> = (props) => {
  const prefixes = (): readonly string[] => props.servers.map((s) => `${s.name}__`);
  const orphans = (): readonly string[] =>
    props.selected.filter(
      (t) => !KNOWN_BUILTIN.has(t) && !prefixes().some((p) => t.startsWith(p)),
    );
  return (
    <Show when={orphans().length > 0}>
      <div class="flex flex-col gap-xs">
        <span class="text-micro font-medium uppercase tracking-[0.08em] text-ink-subtle">
          Held but not offered
        </span>
        <For each={orphans()}>
          {(t) => (
            <ToolCheck
              name={t}
              help="Not on any read ToolServer's current list. Uncheck it to drop it."
              selected={props.selected}
              onToggle={props.onToggle}
            />
          )}
        </For>
      </div>
    </Show>
  );
};

const ToolCheck: Component<PickerProps & { readonly name: string; readonly help: string }> = (props) => {
  const id = (): string => `inv-tool-${props.name}`;
  return (
    <div class="flex flex-col">
      <div class={CHECK_ROW}>
        <Checkbox
          id={id()}
          checked={props.selected.includes(props.name)}
          onChange={(on) => props.onToggle(props.name, on)}
        />
        <label for={`${id()}-input`} class={cn(CHECK_LABEL, "font-mono")}>
          {props.name}
        </label>
      </div>
      <Show when={props.help !== ""}>
        <span class="pl-6 text-meta text-ink-subtle">{props.help}</span>
      </Show>
    </div>
  );
};
