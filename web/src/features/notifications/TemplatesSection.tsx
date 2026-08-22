/**
 * The screen where somebody writes ONE WHOLE MESSAGE and watches two providers
 * spell it.
 *
 * ⭐ IT IS A DOCUMENT EDITOR, NOT A SET OF SLOT OVERRIDES, AND THAT IS THE WHOLE
 * PIVOT. The screen this replaced offered four named holes in oto's card and a
 * precedence panel to explain which override won. It was safe, it was
 * defensible, and the first question every reader asked was "so where is my
 * template?" — a document you can read top to bottom is one you can predict, and
 * four independent overrides are not.
 *
 * ⭐⭐ THE TWO PREVIEW COLUMNS ARE THE POINT OF THE PAGE. One Markdown document
 * compiles to Slack's `*bold*` and to a webhook consumer's plain words. An author
 * shown ONE spelling concludes that markup is theirs to write; an author shown
 * both cannot. It is also the only place the portability claim is visible rather
 * than asserted.
 *
 * ⛔ A WARNING IS NOT AN ERROR, AND THE SAVE BUTTON MUST STAY ENABLED THROUGH
 * ONE. A card with no `{{ actions }}` carries no Acknowledge button — the operator
 * is allowed to ship that, and an alert stays acknowledgeable from the console
 * and from `POST /api/v1/cases/{id}/ack`. The screen says so loudly and then
 * gets out of the way.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  onCleanup,
  type Component,
} from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";
import * as v from "valibot";

import { maxLengthOf, minLengthOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import {
  createNotificationTemplate,
  deleteNotificationTemplate,
  updateNotificationTemplate,
} from "~/api/endpoints";
import {
  CreateNotificationTemplateRequestSchema,
  NotificationTemplateFormatSchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { notificationTemplatesQuery, templatePreviewQuery } from "~/api/queries";
import type {
  CreateNotificationTemplateRequest,
  NotificationTemplate,
  NotificationTemplateFormat,
  TemplateRendering,
} from "~/api/types";
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
import { Chip, Panel, PanelHeader, PanelTitle, SECTION_LABEL } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, LoadingLine, PageEmptyState } from "~/components/ui/states";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { cn } from "~/lib/cn";
import {
  CHECK_LABEL,
  CHECK_ROW,
  FIELD,
  FIELD_ROW,
  FORM,
  HELP,
  LABEL,
  PANEL_HEADER,
  ROW,
  SECTION,
} from "~/features/settings/rhythm";

/** The formats, READ from the contract's own enum rather than restated here. */
const FORMATS: readonly NotificationTemplateFormat[] = NotificationTemplateFormatSchema.options;

/**
 * What each format is, in one line, at the moment somebody is choosing.
 *
 * ⛔ `raw` SAYS IT IS SLACK-ONLY IN THE PICKER AND NOT IN A TOOLTIP. It is the
 * one irreversible-feeling choice on the screen: a `raw` template sent to any
 * other provider falls back to oto's built-in card, and finding that out after
 * writing two hundred lines of Block Kit is the worst version of this feature.
 */
const FORMAT_HELP: Record<NotificationTemplateFormat, string> = {
  card: "Markdown. Works on every channel — oto compiles it to each one's own shape.",
  text: "One plain line. Works on every channel.",
  raw: "Slack Block Kit JSON. Slack only — every other channel falls back to oto's own card.",
};

const SOURCE_MIN = minLengthOf(CreateNotificationTemplateRequestSchema, "source");
const SOURCE_MAX = maxLengthOf(CreateNotificationTemplateRequestSchema, "source");
const NAME_MAX = maxLengthOf(CreateNotificationTemplateRequestSchema, "name");

/**
 * The starter, and it is a TEACHING ARTEFACT rather than a placeholder.
 *
 * Every construct the format has is in it once: a heading, prose, a divider, the
 * `:::fields` grid, a loop, a link written the only way links can be written, and
 * the actions token. An author who deletes the parts they do not want has learnt
 * the whole language; an author handed an empty box has to go and find the docs.
 */
const STARTER = `# {{ alert.name }}

{{ annotations.summary | default: "No summary on this alert." }}

---

:::fields
Severity | {{ alert.severity | upper }}
Firing | {{ group.firing_for }}
Seen | {{ alert.total_cases }} times
:::

{% for l in label_list %}- {{ l.name }}: {{ l.value }}
{% endfor %}
> [Open in oto]({{ links.group }})

{{ actions }}`;

const PREVIEW_DEBOUNCE_MS = 250;

type TemplateForm = {
  name: string;
  provider: string;
  format: NotificationTemplateFormat;
  source: string;
  enabled: boolean;
};

const TemplateFormSchema = v.object({
  name: v.pipe(v.string(), v.trim(), v.minLength(1, "Give it a name."), v.maxLength(NAME_MAX)),
  provider: v.pipe(v.string(), v.trim(), v.minLength(1, "Pick a channel kind.")),
  format: v.picklist(FORMATS),
  source: v.pipe(
    v.string(),
    v.minLength(SOURCE_MIN, "A template with no body would send an empty message."),
    v.maxLength(SOURCE_MAX, `A template may be ${SOURCE_MAX} characters.`),
  ),
  enabled: v.boolean(),
});

function toCreateRequest(f: TemplateForm): CreateNotificationTemplateRequest {
  return {
    name: f.name.trim(),
    provider: f.provider.trim(),
    format: f.format,
    source: f.source,
    enabled: f.enabled,
  };
}

function live(rows: readonly NotificationTemplate[]): readonly NotificationTemplate[] {
  return rows.filter((t) => t.deleted_at == null);
}

/* -------------------------------------------------------------------------- */

export const TemplatesSection: Component = () => {
  const [editing, setEditing] = createSignal<NotificationTemplate | null>(null);
  const [creating, setCreating] = createSignal(false);

  const templates = useQuery(() => notificationTemplatesQuery());
  const rows = createMemo(() => live(templates.data?.data ?? []));

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Message templates</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setCreating(true)}>
            Write a template
          </Button>
        </PanelHeader>

        <p class={HELP}>
          A template is the whole message oto sends. A notification policy picks which one its
          alerts use — templates carry no matchers of their own, because the policy already has
          them.
        </p>

        <Switch>
          <Match when={templates.isPending}>
            <LoadingLine />
          </Match>
          <Match when={templates.isError}>
            <ErrorState error={templates.error} onRetry={() => void templates.refetch()} />
          </Match>
          <Match when={rows().length === 0}>
            <PageEmptyState
              motif="kumo"
              title="Every alert reads in oto's own voice"
              body="Write a template to say it differently. You will see it spelled for every channel kind as you type."
            />
          </Match>
          <Match when={rows().length > 0}>
            <ul class="divide-y divide-border">
              <For each={rows()}>
                {(t) => <TemplateRow template={t} onEdit={() => setEditing(t)} />}
              </For>
            </ul>
          </Match>
        </Switch>
      </Panel>

      <Show when={creating()}>
        <TemplateDialog onClose={() => setCreating(false)} />
      </Show>
      <Show when={editing()} keyed>
        {(t) => <TemplateDialog template={t} onClose={() => setEditing(null)} />}
      </Show>
    </div>
  );
};

const TemplateRow: Component<{
  template: NotificationTemplate;
  onEdit: () => void;
}> = (props) => (
  <li class={ROW}>
    <button type="button" class="flex-1 text-left" onClick={props.onEdit}>
      <span class="font-medium">{props.template.name}</span>
      <span class={cn(HELP, "block")}>
        {props.template.provider} · {props.template.format} · v{props.template.version}
      </span>
    </button>
    <Show when={!props.template.enabled}>
      <Chip>off</Chip>
    </Show>
  </li>
);

/* -------------------------------------------------------------------------- */

const TemplateDialog: Component<{
  template?: NotificationTemplate;
  onClose: () => void;
}> = (props) => {
  const qc = useQueryClient();
  const existing = () => props.template;

  const [form, setForm] = createSignal<TemplateForm>({
    name: existing()?.name ?? "",
    provider: existing()?.provider ?? "slack",
    format: (existing()?.format as NotificationTemplateFormat) ?? "card",
    source: existing()?.source ?? STARTER,
    enabled: existing()?.enabled ?? true,
  });
  const patch = (d: Partial<TemplateForm>) => setForm((f) => ({ ...f, ...d }));

  /*
   * ⛔ THE PREVIEW IS DEBOUNCED AND THE SAVE IS NOT. A POST per keystroke is the
   * failure this exists to avoid, and 250ms is short enough that the two columns
   * feel attached to the typing. The query key is (format, source), so a
   * keystroke undone gets its previous answer back with no round trip at all.
   */
  const [debounced, setDebounced] = createSignal(form().source);
  createEffect(() => {
    const next = form().source;
    const id = setTimeout(() => setDebounced(next), PREVIEW_DEBOUNCE_MS);
    onCleanup(() => clearTimeout(id));
  });

  const preview = useQuery(() => ({
    ...templatePreviewQuery(form().format, debounced()),
    enabled: debounced().trim().length >= SOURCE_MIN,
  }));

  const problems = createMemo(() => preview.data?.problems ?? []);
  /** Refusals only. A warning is reported and does not stop a save. */
  const blocking = createMemo(() => problems().filter((p) => p.kind !== "warning"));
  const warnings = createMemo(() => problems().filter((p) => p.kind === "warning"));

  /** The ordinary cards first: those are the ones an author is writing for. */
  const renderings = createMemo<readonly TemplateRendering[]>(() => {
    const all = preview.data?.renderings ?? [];
    return [...all].sort((a, b) => Number(b.representative) - Number(a.representative));
  });

  const [fieldErrors, setFieldErrors] = createSignal<Record<string, string>>({});
  const [banner, setBanner] = createSignal<unknown>(null);

  const done = () => {
    void qc.invalidateQueries({ queryKey: qk.templates.list() });
    props.onClose();
  };
  const failed = (e: unknown) => {
    setBanner(e);
    setFieldErrors(Object.fromEntries(violationsByField(e)));
  };

  const create = useMutation(() => ({
    mutationFn: (body: CreateNotificationTemplateRequest) => createNotificationTemplate(body),
    onSuccess: done,
    onError: failed,
  }));
  const update = useMutation(() => ({
    mutationFn: (body: CreateNotificationTemplateRequest) =>
      updateNotificationTemplate(existing()!.id, body),
    onSuccess: done,
    onError: failed,
  }));
  const remove = useMutation(() => ({
    mutationFn: () => deleteNotificationTemplate(existing()!.id),
    onSuccess: done,
    onError: failed,
  }));

  const submit = (e: SubmitEvent) => {
    e.preventDefault();
    setBanner(null);
    const parsed = v.safeParse(TemplateFormSchema, form());
    if (!parsed.success) {
      const errs: Record<string, string> = {};
      for (const issue of parsed.issues) {
        const key = String(issue.path?.[0]?.key ?? "");
        if (key !== "" && errs[key] === undefined) errs[key] = issue.message;
      }
      setFieldErrors(errs);
      return;
    }
    setFieldErrors({});
    const body = toCreateRequest(parsed.output);
    if (existing()) update.mutate(body);
    else create.mutate(body);
  };

  const busy = () => create.isPending || update.isPending || remove.isPending;

  return (
    <Modal open onOpenChange={(o) => !o && props.onClose()}>
      <ModalContent class="max-w-5xl">
        <ModalHeader>
          <ModalTitle>{existing() ? "Edit template" : "Write a template"}</ModalTitle>
          <ModalDescription>
            oto builds the card; you write what it says. Every value from the alert is escaped, so a
            label can never become formatting.
          </ModalDescription>
        </ModalHeader>

        <form class={FORM} onSubmit={submit}>
          <Show when={banner()}>
            <ErrorBanner error={banner()} />
          </Show>

          <div class={FIELD_ROW}>
            <TextField class={FIELD} validationState={fieldErrors().name ? "invalid" : "valid"}>
              <TextFieldLabel class={LABEL}>Name</TextFieldLabel>
              <TextFieldInput
                value={form().name}
                maxLength={NAME_MAX}
                onInput={(e) => patch({ name: e.currentTarget.value })}
              />
              <TextFieldErrorMessage>{fieldErrors().name}</TextFieldErrorMessage>
            </TextField>

            <TextField class={FIELD} validationState={fieldErrors().provider ? "invalid" : "valid"}>
              <TextFieldLabel class={LABEL}>Written for</TextFieldLabel>
              <TextFieldInput
                value={form().provider}
                onInput={(e) => patch({ provider: e.currentTarget.value })}
              />
              <TextFieldDescription class={HELP}>
                A note to yourself. oto does not stop a policy sending this anywhere.
              </TextFieldDescription>
            </TextField>
          </div>

          <div class={FIELD}>
            <ToggleGroup
              legend="Format"
              multiple={false}
              value={form().format}
              onChange={(val) => {
                if (val !== null) patch({ format: val as NotificationTemplateFormat });
              }}
            >
              <For each={FORMATS}>
                {(f) => (
                  <ToggleGroupItem value={f} aria-label={f}>
                    {f}
                  </ToggleGroupItem>
                )}
              </For>
            </ToggleGroup>
            <p class={HELP}>{FORMAT_HELP[form().format]}</p>
          </div>

          <div class="grid gap-4 md:grid-cols-2">
            <TextField class={FIELD} validationState={fieldErrors().source ? "invalid" : "valid"}>
              <TextFieldLabel class={LABEL}>The message</TextFieldLabel>
              <TextFieldTextArea
                class="min-h-80 font-mono text-meta"
                value={form().source}
                spellcheck={false}
                onInput={(e) => patch({ source: e.currentTarget.value })}
              />
              <TextFieldErrorMessage>{fieldErrors().source}</TextFieldErrorMessage>
            </TextField>

            <div class={FIELD}>
              <span class={LABEL}>What it sends</span>
              <PreviewPane
                loading={preview.isFetching}
                blocking={blocking()}
                warnings={warnings()}
                renderings={renderings()}
              />
            </div>
          </div>

          <label class={CHECK_ROW}>
            <Checkbox
              checked={form().enabled}
              onChange={(checked) => patch({ enabled: checked })}
            />
            <span class={CHECK_LABEL}>Enabled</span>
          </label>

          <ModalFooter>
            <Show when={existing()}>
              <Button
                type="button"
                variant="destructive"
                disabled={busy()}
                onClick={() => remove.mutate()}
              >
                Delete
              </Button>
            </Show>
            <Button type="button" variant="ghost" onClick={props.onClose} disabled={busy()}>
              Cancel
            </Button>
            {/*
              ⛔ DISABLED ON A REFUSAL, NEVER ON A WARNING. A card with no
              `{{ actions }}` is the operator's decision and the button must stay
              live through it, or the screen has overruled them.
            */}
            <Button type="submit" disabled={busy() || blocking().length > 0}>
              {existing() ? "Save" : "Create"}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */

const PreviewPane: Component<{
  loading: boolean;
  blocking: readonly { kind: string; message: string; fixture?: string }[];
  warnings: readonly { kind: string; message: string; fixture?: string }[];
  renderings: readonly TemplateRendering[];
}> = (props) => (
  <div class="space-y-3">
    <Show when={props.blocking.length > 0}>
      <ul class="space-y-1 rounded-surface border border-destructive/40 bg-destructive/5 p-3 text-meta">
        <For each={props.blocking}>
          {(p) => (
            <li>
              <span class="font-medium">{p.message}</span>
              <Show when={p.fixture}>
                <span class={cn(HELP, "ml-1")}>(on the {p.fixture} example)</span>
              </Show>
            </li>
          )}
        </For>
      </ul>
    </Show>

    {/*
      ⭐ THE WARNING IS DRAWN, NOT SWALLOWED, AND IT DOES NOT LOOK LIKE AN ERROR.
      An operator shipping a card with no Acknowledge button should see exactly
      one sentence about it — at the moment they can still change their mind, and
      never as a thing blocking their way.
    */}
    <Show when={props.warnings.length > 0}>
      <ul class="space-y-1 rounded-surface border border-warning/40 bg-warning/5 p-3 text-meta">
        <For each={props.warnings}>{(p) => <li>{p.message}</li>}</For>
      </ul>
    </Show>

    <Show when={props.loading}>
      <LoadingLine />
    </Show>

    <For each={props.renderings}>
      {(r) => (
        <div class="rounded-surface border border-border">
          <div class={cn(SECTION_LABEL, "flex items-center gap-2 border-b border-border px-3 py-1.5")}>
            <span>{r.fixture}</span>
            <Show when={r.representative}>
              <Chip>ordinary card</Chip>
            </Show>
            <Show when={!r.has_actions}>
              <Chip>no buttons</Chip>
            </Show>
          </div>
          {/*
            ⭐⭐ SIDE BY SIDE, ALWAYS, EVEN WHEN THEY LOOK THE SAME. This grid IS
            the portability claim: one source, two columns, and the differences
            between them are exactly the quirks the author never has to think
            about again.
          */}
          <div class="grid gap-px bg-border sm:grid-cols-2">
            <For each={r.spellings}>
              {(s) => (
                <div class="bg-background p-3">
                  <div class={SECTION_LABEL}>{s.dialect}</div>
                  <Show
                    when={!s.error}
                    fallback={<p class="text-meta text-destructive">{s.error}</p>}
                  >
                    <pre class="whitespace-pre-wrap break-words font-mono text-meta">{s.text}</pre>
                  </Show>
                </div>
              )}
            </For>
          </div>
        </div>
      )}
    </For>
  </div>
);
