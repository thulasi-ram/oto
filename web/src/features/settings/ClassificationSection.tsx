/**
 * The org's Classification set — the closed vocabulary an Investigation classifies
 * its Finding in (ADR 0053 §5, git-bug 4298aa0).
 *
 * ⛔ OTO SHIPS NO CLASSES, AND THIS SCREEN OFFERS NONE. A fresh org's set is empty and
 * stays empty until an operator writes one; there is no "suggested" list, no starter
 * and no `noise` placeholder, because any class oto proposed would be oto's opinion
 * of someone else's signal. With no classes, a Finding carries no classification.
 *
 * ⭐ `unclassified` IS NOT A ROW. It is always admissible and is the right answer
 * under doubt, so it is not the operator's to add or to take away — the screen says
 * so, and the server refuses it as a name.
 *
 * ⭐ THE SET IS SAVED WHOLE. It is one vocabulary; the form edits a draft of the whole
 * thing and `Save` replaces the set in one request. A Finding is never rewritten by a
 * save — each keeps the word it was given — and the copy says so, because renaming a
 * class is the change most likely to be mistaken for a relabelling of history.
 *
 * ⚠️ AND THE PAGING WARNING IS ON THE SCREEN WHERE THE WORDS ARE WRITTEN. A class
 * travels outbound with its Finding, and a receiver may page on it; paging on a
 * classification is paging on a model's judgement. The operator choosing the words
 * is the one who most needs to read that.
 */
import { For, Match, Show, Switch, createEffect, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf } from "~/api/bounds";
import { orphanViolations, violationsByField } from "~/api/client";
import { getInvestigationClasses, replaceInvestigationClasses } from "~/api/endpoints";
import { InvestigationClassRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { InvestigationClass, InvestigationClassSet } from "~/api/types";
import { Button } from "~/components/ui/Button";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";

import { FIELD, FIELD_ROW, FORM, HELP, PANEL_BODY, PANEL_HEADER, SECTION } from "./rhythm";

/** Read off the generated schema, never typed here — `bounds.ts`'s rule. */
const NAME_MAX = maxLengthOf(InvestigationClassRequestSchema, "name");
const DESCRIPTION_MAX = maxLengthOf(InvestigationClassRequestSchema, "description");
/** The set's ceiling, as the server enforces it (`domain.MaxClasses`). */
const MAX_CLASSES = 50;

/** One row of the draft. `key` keeps a row's identity while its name is edited. */
interface DraftRow {
  readonly key: number;
  readonly name: string;
  readonly description: string;
}

let nextKey = 0;
function rowOf(c: InvestigationClass): DraftRow {
  return { key: nextKey++, name: c.name, description: c.description };
}

export const ClassificationSection: Component = () => {
  const client = useQueryClient();
  const set = useQuery(() => ({
    queryKey: qk.settings.investigationClasses(),
    queryFn: ({ signal }: { signal: AbortSignal }) => getInvestigationClasses({ signal }),
  }));

  const [draft, setDraft] = createSignal<readonly DraftRow[]>([]);
  const [dirty, setDirty] = createSignal(false);

  // The draft follows the server until the operator touches it.
  createEffect(() => {
    const data = set.data;
    if (data !== undefined && !dirty()) setDraft(data.classes.map(rowOf));
  });

  const save = useMutation(() => ({
    mutationFn: () =>
      replaceInvestigationClasses({
        classes: draft().map((r) => ({ name: r.name.trim(), description: r.description.trim() })),
      }),
    onSuccess: (stored: InvestigationClassSet) => {
      client.setQueryData(qk.settings.investigationClasses(), stored);
      setDraft(stored.classes.map(rowOf));
      setDirty(false);
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(save.error);
  const known = (): readonly string[] =>
    draft().flatMap((_, i) => [`classes.${i}.name`, `classes.${i}.description`]);

  const edit = (key: number, patch: Partial<Omit<DraftRow, "key">>): void => {
    setDraft((rows) => rows.map((r) => (r.key === key ? { ...r, ...patch } : r)));
    setDirty(true);
  };
  const add = (): void => {
    setDraft((rows) => [...rows, { key: nextKey++, name: "", description: "" }]);
    setDirty(true);
  };
  const remove = (key: number): void => {
    setDraft((rows) => rows.filter((r) => r.key !== key));
    setDirty(true);
  };
  const revert = (): void => {
    setDraft((set.data?.classes ?? []).map(rowOf));
    setDirty(false);
    save.reset();
  };

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Classification</PanelTitle>
        </PanelHeader>

        <div class={cn(PANEL_BODY, FORM)}>
          <p class={HELP}>
            The classes an Investigator must choose from when it classifies a Finding — your
            words, not oto's. oto ships none. While this list is empty, Findings carry no
            classification at all. With classes, the model must pick exactly one of them or{" "}
            <span class="font-mono text-ink">unclassified</span>, which is always allowed and is
            the right answer when it is unsure; a word outside the list is refused and recorded.
          </p>

          <Switch>
            <Match when={set.isPending}>
              <LoadingLine />
            </Match>
            <Match when={set.isError}>
              <ErrorState error={set.error} onRetry={() => void set.refetch()} />
            </Match>
            <Match when={true}>
              <Show when={save.error !== null && orphanViolations(save.error, known()).length > 0}>
                <ErrorBanner error={save.error} />
              </Show>

              <Show
                when={draft().length > 0}
                fallback={
                  <p class={HELP} data-no-classes>
                    No classes. Findings are not classified.
                  </p>
                }
              >
                <ol class={FORM} aria-label="Classes, in the order the model is told them">
                  <For each={draft()}>
                    {(row, i) => (
                      <li class={FIELD_ROW}>
                        <TextField
                          class={cn(FIELD, "w-56")}
                          value={row.name}
                          required
                          validationState={violations().get(`classes.${i()}.name`) ? "invalid" : "valid"}
                          onChange={(v) => edit(row.key, { name: v })}
                        >
                          <TextFieldLabel>Name</TextFieldLabel>
                          <TextFieldInput
                            class="font-mono"
                            maxLength={NAME_MAX}
                            placeholder="deploy-regression"
                            aria-label={`Class ${i() + 1} name`}
                          />
                          <TextFieldErrorMessage role="alert">
                            {violations().get(`classes.${i()}.name`)}
                          </TextFieldErrorMessage>
                        </TextField>
                        <TextField
                          class={cn(FIELD, "min-w-0 flex-1")}
                          value={row.description}
                          validationState={
                            violations().get(`classes.${i()}.description`) ? "invalid" : "valid"
                          }
                          onChange={(v) => edit(row.key, { description: v })}
                        >
                          <TextFieldLabel>What it means</TextFieldLabel>
                          <TextFieldInput
                            maxLength={DESCRIPTION_MAX}
                            placeholder="A change we shipped broke it."
                            aria-label={`Class ${i() + 1} description`}
                          />
                          <TextFieldErrorMessage role="alert">
                            {violations().get(`classes.${i()}.description`)}
                          </TextFieldErrorMessage>
                        </TextField>
                        <Button
                          size="sm"
                          variant="ghost"
                          class="mt-lg"
                          aria-label={`Remove class ${row.name || i() + 1}`}
                          onClick={() => remove(row.key)}
                        >
                          Remove
                        </Button>
                      </li>
                    )}
                  </For>
                </ol>
              </Show>

              <p class={HELP}>
                Names are lower-case letters, digits, <span class="font-mono">_</span> and{" "}
                <span class="font-mono">-</span>, starting with a letter.{" "}
                <span class="font-mono text-ink">unclassified</span> is not one you write: it is
                always there.
              </p>

              <div class="flex flex-wrap items-center gap-sm">
                <Button
                  size="sm"
                  variant="secondary"
                  disabled={draft().length >= MAX_CLASSES}
                  onClick={add}
                >
                  Add a class
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
                    Save classes
                  </Button>
                </div>
              </div>
            </Match>
          </Switch>
        </div>

        <div class={cn(PANEL_BODY, HELP, "flex flex-col gap-xs border-t border-line")}>
          <p>
            Saving replaces the whole list for the next Investigation. It never changes a Finding
            already reached: each keeps the class it was given, even one you have since renamed or
            removed.
          </p>
          <p data-paging-warning>
            <strong class="font-semibold text-ink">A classification is a model's judgement.</strong>{" "}
            It goes out with the Finding — to webhooks and incident tools as well as cards — and a
            receiver may route or page on it. Paging on a classification is paging on a model's
            judgement. It never decides whether oto itself notifies anyone.
          </p>
        </div>
      </Panel>
    </div>
  );
};
