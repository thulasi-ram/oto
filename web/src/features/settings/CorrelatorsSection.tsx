/**
 * Correlators — the operator-written definitions that draw Incidents (ADR 0052
 * §2, git-bug 61eeddf).
 *
 * # What the screen promises
 *
 * ⭐ THE LIST IS THE ORDER THE SERVER WALKS. Every Case that opens is run through
 * these, top to bottom, and the FIRST whose matchers hold claims it — no later
 * one is asked, even when the first then waits for its count. So the rows render
 * exactly as `GET /correlators` returns them (`priority` ascending, then age) and
 * this file never re-sorts: a list in any other order would make "why did that
 * one draw it?" unanswerable from the screen that configured it.
 *
 * ⭐ REORDERING IS `priority`, as it is for a notification policy. "Move up" and
 * "Move down" renumber the whole list to 10, 20, 30… in the new order and PATCH
 * only the rows whose number changed — so two rows sharing a priority (which the
 * server breaks by age) become two distinct numbers the first time anybody
 * reorders, and the screen never has to explain a tie.
 *
 * ⛔ A CORRELATOR IS NEVER CALLED A RULE HERE. In oto that word is the Prometheus
 * alerting rule, and a settings screen that said "rule" would send an operator
 * to the wrong config.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  type Component,
} from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf, maxValueOf, minValueOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import {
  createCorrelator,
  deleteCorrelator,
  listCorrelators,
  updateCorrelator,
} from "~/api/endpoints";
import { CreateCorrelatorRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { Correlator, CreateCorrelatorRequest } from "~/api/types";
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
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import {
  EmptyState,
  ErrorBanner,
  ErrorState,
  LoadingLine,
} from "~/components/ui/states";
import { MatcherInput } from "~/features/alerts/MatcherInput";
import { cn } from "~/lib/cn";
import { formatMatchers, parseMatchers } from "~/lib/matchers";

import {
  CHECK_LABEL,
  CHECK_ROW,
  FIELD,
  FIELD_ROW,
  FORM,
  HELP,
  LABEL,
  PANEL_BODY,
  PANEL_HEADER,
  ROW,
  SECTION,
} from "./rhythm";

/*
 * ⛔ READ OFF THE GENERATED SCHEMA, NOT TYPED HERE — `TokensSection`'s rule. Each
 * bound is migration 00085's CHECK, the DTO tag and the contract; a fourth copy
 * typed into this file would be the one that drifts.
 */
const NAME_MAX = maxLengthOf(CreateCorrelatorRequestSchema, "name");
const PRIORITY_MIN = minValueOf(CreateCorrelatorRequestSchema, "priority");
const PRIORITY_MAX = maxValueOf(CreateCorrelatorRequestSchema, "priority");
const COUNT_MIN = minValueOf(CreateCorrelatorRequestSchema, "count_min");
const COUNT_MAX = maxValueOf(CreateCorrelatorRequestSchema, "count_min");
const WINDOW_MIN = minValueOf(
  CreateCorrelatorRequestSchema,
  "count_window_seconds",
);
const WINDOW_MAX = maxValueOf(
  CreateCorrelatorRequestSchema,
  "count_window_seconds",
);

/** The gap `renumber` leaves between neighbours, so a later insert has room. */
const PRIORITY_STEP = 10;

export const CorrelatorsSection: Component = () => {
  const client = useQueryClient();
  const [editing, setEditing] = createSignal<Correlator | "new" | null>(null);

  const correlators = useQuery(() => ({
    queryKey: qk.settings.correlators(),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listCorrelators({ signal }),
  }));

  const rows = (): readonly Correlator[] => correlators.data?.data ?? [];

  /*
   * Reordering renumbers the whole list and sends only the changes. Sequential
   * rather than parallel so a failure part-way leaves a prefix applied that the
   * refetch then shows truthfully, rather than an arbitrary subset.
   */
  const reorder = useMutation(() => ({
    mutationFn: async (next: readonly Correlator[]) => {
      for (const [i, k] of next.entries()) {
        const priority = Math.min((i + 1) * PRIORITY_STEP, PRIORITY_MAX);
        if (k.priority !== priority) await updateCorrelator(k.id, { priority });
      }
    },
    onSettled: () =>
      void client.invalidateQueries({ queryKey: qk.settings.correlators() }),
  }));

  const move = (index: number, by: -1 | 1): void => {
    const list = [...rows()];
    const target = index + by;
    if (target < 0 || target >= list.length) return;
    const moved = list[index];
    const other = list[target];
    if (moved === undefined || other === undefined) return;
    list[index] = other;
    list[target] = moved;
    reorder.mutate(list);
  };

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Correlators</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setEditing("new")}>
            Write a Correlator
          </Button>
        </PanelHeader>

        <Show when={reorder.error !== null}>
          <div class={PANEL_BODY}>
            <ErrorBanner error={reorder.error} />
          </div>
        </Show>

        <Switch>
          <Match when={correlators.isPending}>
            <LoadingLine />
          </Match>
          <Match when={correlators.isError}>
            <ErrorState
              error={correlators.error}
              onRetry={() => void correlators.refetch()}
            />
          </Match>
          <Match when={rows().length === 0}>
            <EmptyState
              title="No Correlators yet."
              body="Without one, every Incident is drawn by hand. A Correlator draws them as Cases open — one Incident per matching Case, or one over several once enough have opened together."
            />
          </Match>
          <Match when={true}>
            <ol>
              <For each={rows()}>
                {(k, i) => (
                  <CorrelatorRow
                    correlator={k}
                    position={i() + 1}
                    first={i() === 0}
                    last={i() === rows().length - 1}
                    busy={reorder.isPending}
                    onUp={() => move(i(), -1)}
                    onDown={() => move(i(), 1)}
                    onEdit={() => setEditing(k)}
                  />
                )}
              </For>
            </ol>
          </Match>
        </Switch>

        <div
          class={cn(
            PANEL_BODY,
            HELP,
            "flex flex-col gap-xs border-t border-line",
          )}
        >
          <p>
            Every Case that opens is run through this list from the top, a
            moment after it opens and never in its way: its own notification
            goes out whatever a Correlator does. The{" "}
            <strong class="font-semibold">first</strong> whose matchers hold
            claims the Case and no later one is asked — even while the first is
            still waiting for its count.
          </p>
          <p>
            A claimed Case joins that Correlator's latest Incident while it is
            active; otherwise it draws a new one. A Case already in an Incident
            is left alone, a Case a person took out of one is never put back,
            and an Incident a person drew never grows by itself.
          </p>
        </div>
      </Panel>

      <EditorDialog
        editing={editing()}
        nextPriority={(rows().length + 1) * PRIORITY_STEP}
        onClose={() => setEditing(null)}
      />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

/** "≥ 5 in 10m" — the count condition as an operator would say it. */
function describeCount(k: Correlator): string | null {
  if (k.count_min === null || k.count_window_seconds === null) return null;
  const s = k.count_window_seconds;
  const span =
    s % 3600 === 0 ? `${s / 3600}h` : s % 60 === 0 ? `${s / 60}m` : `${s}s`;
  return `≥ ${k.count_min} within ${span}`;
}

const CorrelatorRow: Component<{
  readonly correlator: Correlator;
  readonly position: number;
  readonly first: boolean;
  readonly last: boolean;
  readonly busy: boolean;
  readonly onUp: () => void;
  readonly onDown: () => void;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const k = (): Correlator => props.correlator;
  const [confirming, setConfirming] = createSignal(false);

  const remove = useMutation(() => ({
    mutationFn: () => deleteCorrelator(k().id),
    onSuccess: () => {
      setConfirming(false);
      void client.invalidateQueries({ queryKey: qk.settings.correlators() });
    },
  }));

  return (
    <li class={cn(ROW, "flex min-h-12 flex-wrap items-center gap-sm")}>
      <span
        class="w-6 text-meta tabular-nums text-ink-subtle"
        title="Evaluation order"
      >
        {props.position}.
      </span>
      <span class="text-item font-medium text-ink">{k().name}</span>
      <Chip title="Lower is evaluated first.">priority {k().priority}</Chip>
      <Show when={!k().enabled}>
        <Chip title="Disabled: skipped by the walk, as if it were not in the list.">
          disabled
        </Chip>
      </Show>
      <Show when={describeCount(k())}>
        {(count) => (
          <Chip title="Draws only once this many of its Cases opened inside one window of this length; a matching Case joins its active Incident whatever the count.">
            {count()}
          </Chip>
        )}
      </Show>
      <span class="min-w-0 basis-full truncate font-mono text-meta text-ink-muted sm:basis-auto">
        {k().matchers.length === 0
          ? "any Case"
          : formatMatchers(
              k().matchers.map((m) => ({
                name: m.name,
                op: m.op,
                value: m.value,
              })),
            )}
      </span>

      <div class="ml-auto flex items-center gap-xs">
        <Button
          size="sm"
          variant="ghost"
          disabled={props.first || props.busy}
          aria-label={`Move ${k().name} up`}
          onClick={() => props.onUp()}
        >
          ↑
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={props.last || props.busy}
          aria-label={`Move ${k().name} down`}
          onClick={() => props.onDown()}
        >
          ↓
        </Button>
        <Button size="sm" variant="secondary" onClick={() => props.onEdit()}>
          Edit
        </Button>
        <Button
          size="sm"
          variant="destructive"
          onClick={() => setConfirming(true)}
        >
          Delete
        </Button>
      </div>

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} />
      </Show>

      <Modal
        open={confirming()}
        onOpenChange={(isOpen) => {
          if (!isOpen) setConfirming(false);
        }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>Delete {k().name}?</ModalTitle>
            <ModalDescription>
              It draws nothing from now on. The Incidents it already drew stay,
              and keep saying it drew them.
            </ModalDescription>
          </ModalHeader>
          <ModalFooter>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setConfirming(false)}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              variant="destructive"
              busy={remove.isPending}
              onClick={() => remove.mutate()}
            >
              Delete it
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

/** An integer box's value, or `null` for an empty box. `NaN` is kept, not hidden. */
function intOrNull(text: string): number | null {
  const t = text.trim();
  return t === "" ? null : Number.parseInt(t, 10);
}

const EditorDialog: Component<{
  readonly editing: Correlator | "new" | null;
  readonly nextPriority: number;
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const [name, setName] = createSignal("");
  const [priority, setPriority] = createSignal("");
  const [enabled, setEnabled] = createSignal(true);
  const [matcherText, setMatcherText] = createSignal("");
  const [countMin, setCountMin] = createSignal("");
  const [countWindow, setCountWindow] = createSignal("");

  const existing = (): Correlator | null =>
    props.editing !== null && props.editing !== "new" ? props.editing : null;

  // Load the form whenever the dialog opens on a different subject.
  createEffect(() => {
    const e = props.editing;
    if (e === null) return;
    if (e === "new") {
      setName("");
      setPriority(String(Math.min(props.nextPriority, PRIORITY_MAX)));
      setEnabled(true);
      setMatcherText("");
      setCountMin("");
      setCountWindow("");
      return;
    }
    setName(e.name);
    setPriority(String(e.priority));
    setEnabled(e.enabled);
    setMatcherText(
      formatMatchers(
        e.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
      ),
    );
    setCountMin(e.count_min === null ? "" : String(e.count_min));
    setCountWindow(
      e.count_window_seconds === null ? "" : String(e.count_window_seconds),
    );
  });

  const parsed = createMemo(() => parseMatchers(matcherText()));

  /*
   * ⭐ THE PAIR RULE IS CHECKED HERE ONLY TO SAY IT EARLY; THE SERVER IS WHAT
   * HOLDS IT (`correlators_count_pair_ck`). Half a count condition is refused as
   * a 422 on `count_min` whatever this says.
   */
  const localError = (field: string): string | undefined => {
    if (field === "matchers" && parsed().errors.length > 0)
      return parsed().errors[0]?.message;
    if (
      field === "count_min" &&
      (countMin().trim() === "") !== (countWindow().trim() === "")
    ) {
      return "A count needs both a number of Cases and a window, or neither.";
    }
    return undefined;
  };

  const body = (): CreateCorrelatorRequest => {
    const p = intOrNull(priority());
    const min = intOrNull(countMin());
    const win = intOrNull(countWindow());
    return {
      name: name().trim(),
      // The contract defaults it to 100 and the generated type therefore makes it
      // required; an emptied box sends the default rather than inventing one.
      priority: p ?? 100,
      enabled: enabled(),
      matchers: parsed().matchers.map((m) => ({
        name: m.name,
        op: m.op,
        value: m.value,
      })),
      ...(min === null ? {} : { count_min: min }),
      ...(win === null ? {} : { count_window_seconds: win }),
    };
  };

  const save = useMutation(() => ({
    mutationFn: async () => {
      const b = body();
      const k = existing();
      if (k === null) return createCorrelator(b);
      // A PATCH sends the whole form, with an explicit `null` for a cleared count,
      // because "no count" is a state the operator chose by emptying both boxes.
      return updateCorrelator(k.id, {
        name: b.name,
        priority: b.priority,
        enabled: b.enabled,
        matchers: b.matchers ?? [],
        count_min: b.count_min ?? null,
        count_window_seconds: b.count_window_seconds ?? null,
      });
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.correlators() });
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> =>
    violationsByField(save.error);
  const errorOf = (field: string): string | undefined =>
    localError(field) ?? violations().get(field);

  return (
    <Modal
      open={props.editing !== null}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {existing() === null
              ? "Write a Correlator"
              : `Edit ${existing()?.name}`}
          </ModalTitle>
          <ModalDescription>
            Matchers over a Case's labels, in the grammar a notification policy
            uses. A change applies to Cases that open after it; Incidents
            already drawn are not redrawn.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={save.error !== null && violations().size === 0}>
            <ErrorBanner error={save.error} />
          </Show>

          <div class={FIELD_ROW}>
            <TextField
              class={cn(FIELD, "min-w-48 flex-1")}
              value={name()}
              required
              validationState={errorOf("name") ? "invalid" : "valid"}
              onChange={setName}
            >
              <TextFieldLabel>Name</TextFieldLabel>
              <TextFieldInput
                maxLength={NAME_MAX}
                placeholder="payments storm"
              />
              <TextFieldErrorMessage role="alert">
                {errorOf("name")}
              </TextFieldErrorMessage>
            </TextField>
            <TextField
              class={cn(FIELD, "w-28")}
              value={priority()}
              validationState={errorOf("priority") ? "invalid" : "valid"}
              onChange={setPriority}
            >
              <TextFieldLabel>Priority</TextFieldLabel>
              <TextFieldInput
                type="number"
                min={PRIORITY_MIN}
                max={PRIORITY_MAX}
                step={1}
              />
              <TextFieldDescription class={HELP}>
                Lower first.
              </TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {errorOf("priority")}
              </TextFieldErrorMessage>
            </TextField>
          </div>

          <div class={FIELD}>
            <label for="corr-matchers" class={LABEL}>
              Matchers
            </label>
            <MatcherInput
              id="corr-matchers"
              value={matcherText()}
              onChange={setMatcherText}
              onCommit={() => undefined}
            />
            <p class={HELP}>
              All must match a Case's labels. An empty list matches every Case.
              For example{" "}
              <code class="font-mono">
                {'cluster="prod", namespace="payments"'}
              </code>
              .
            </p>
            <Show when={errorOf("matchers")}>
              {(msg) => (
                <p class="text-meta font-medium text-ink" role="alert">
                  {msg()}
                </p>
              )}
            </Show>
          </div>

          <div class={FIELD_ROW}>
            <TextField
              class={cn(FIELD, "w-36")}
              value={countMin()}
              validationState={errorOf("count_min") ? "invalid" : "valid"}
              onChange={setCountMin}
            >
              <TextFieldLabel>At least this many Cases</TextFieldLabel>
              <TextFieldInput
                type="number"
                min={COUNT_MIN}
                max={COUNT_MAX}
                step={1}
                placeholder="—"
              />
              <TextFieldErrorMessage role="alert">
                {errorOf("count_min")}
              </TextFieldErrorMessage>
            </TextField>
            <TextField
              class={cn(FIELD, "w-36")}
              value={countWindow()}
              validationState={
                errorOf("count_window_seconds") ? "invalid" : "valid"
              }
              onChange={setCountWindow}
            >
              <TextFieldLabel>Within (seconds)</TextFieldLabel>
              <TextFieldInput
                type="number"
                min={WINDOW_MIN}
                max={WINDOW_MAX}
                step={1}
                placeholder="—"
              />
              <TextFieldErrorMessage role="alert">
                {errorOf("count_window_seconds")}
              </TextFieldErrorMessage>
            </TextField>
          </div>
          <p class={HELP}>
            Optional. Leave both empty and every matching Case draws an Incident
            or joins this Correlator's active one. Fill both and it draws only
            once that many of its Cases have opened inside one window of that
            length — then over all of them at once.
          </p>

          <div class={CHECK_ROW}>
            <Checkbox
              id="corr-enabled"
              checked={enabled()}
              onChange={setEnabled}
            />
            <label for="corr-enabled-input" class={CHECK_LABEL}>
              Enabled
            </label>
          </div>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={save.isPending}
            disabled={
              name().trim() === "" || localError("matchers") !== undefined
            }
            onClick={() => save.mutate()}
          >
            Save
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
