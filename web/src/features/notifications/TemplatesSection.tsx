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
 *
 * ⭐ A TEMPLATE HAS TWO BODIES, AND THEY ARE TWO TABS OF ONE DOCUMENT. The root
 * card is `source`; the thread replies are `reply_source`, branched on `reason`.
 * They share a format, a version and a save, so they live in one dialog under one
 * Format toggle — a second dialog would let them drift apart. A reason the reply
 * body does not handle keeps oto's own reply, and the preview says exactly that
 * per reason rather than drawing an empty box an author would read as "nothing is
 * sent".
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
  TemplateProblem,
  TemplateRendering,
  UpdateNotificationTemplateRequest,
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
import { Chip, PageHeading, Panel, SECTION_LABEL } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, LoadingLine, PageEmptyState } from "~/components/ui/states";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "~/components/ui/Tabs";
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
  ADD_FULL,
  CARD_LIST,
  PANEL_BODY,
  SECTION,
  SECTION_HEAD,
  SECTION_LEDE,
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
const REPLY_MAX = maxLengthOf(CreateNotificationTemplateRequestSchema, "reply_source");

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
  /** The thread-reply body. `""` is "every reply is oto's own". */
  reply_source: string;
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
  reply_source: v.pipe(
    v.string(),
    v.maxLength(REPLY_MAX, `A reply body may be ${REPLY_MAX} characters.`),
  ),
  enabled: v.boolean(),
});

/** A reply body of nothing but whitespace is no reply body, here as on the server. */
function hasReplyBody(replySource: string): boolean {
  return replySource.trim() !== "";
}

function toCreateRequest(f: TemplateForm): CreateNotificationTemplateRequest {
  return {
    name: f.name.trim(),
    provider: f.provider.trim(),
    format: f.format,
    source: f.source,
    ...(hasReplyBody(f.reply_source) ? { reply_source: f.reply_source } : {}),
    enabled: f.enabled,
  };
}

/**
 * ⛔ `reply_source` IS ALWAYS SENT ON AN EDIT, AND `""` IS HOW IT IS CLEARED. The
 * create body leaves an empty one out, which on a PATCH would mean "unchanged" —
 * so an author who emptied the box and pressed Save would get their old replies
 * back, silently.
 */
function toUpdateRequest(f: TemplateForm): UpdateNotificationTemplateRequest {
  return {
    ...toCreateRequest(f),
    reply_source: hasReplyBody(f.reply_source) ? f.reply_source : "",
  };
}

/** A problem with no `field` predates reply bodies, so it is the root card's. */
function isReplyProblem(p: TemplateProblem): boolean {
  return p.field === "reply_source";
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
      {/* ⛔⛔ THIS SENTENCE USED TO SIT ON THE PANEL'S LEFT BORDER, and that is the
          defect that took the card off the header. It was a `<p class={HELP}>`
          placed directly inside `<Panel>` — but `HELP` is a typography recipe and
          carries no inset, and the panel's only padded children were its header
          and its rows, so the one paragraph between them had nothing holding it
          off the edge. Adding `px-lg` would have hidden it; the real answer is
          that a section's own explanation is not panel content at all. It is a
          lede under a heading, and out here it owes no border a margin. */}
      <header class={SECTION_HEAD}>
        <PageHeading brush="swipe">Message templates</PageHeading>
        <p class={SECTION_LEDE}>
          A template is the whole message oto sends. A notification policy picks which one its
          alerts use — templates carry no matchers of their own, because the policy already has
          them.
        </p>
      </header>

      <Switch>
        <Match when={templates.isPending}>
          <LoadingLine />
        </Match>
        <Match when={templates.isError}>
          <ErrorState error={templates.error} onRetry={() => void templates.refetch()} />
        </Match>
        <Match when={rows().length === 0}>
          {/* No action inside the empty state: the full-width control below is
              already on screen, and it is where the button stays once the list
              has something in it. */}
          <PageEmptyState
            motif="kumo"
            title="Every alert reads in oto's own voice"
            body="Write a template to say it differently. You will see it spelled for every channel kind as you type."
          />
        </Match>
        <Match when={rows().length > 0}>
          {/* ⛔ `divide-y` IS GONE WITH THE PANEL THAT MADE IT MEAN SOMETHING. A
              hairline between two rows only separates them while they share one
              box; between two cards it would be a third line beside the two
              borders already there. */}
          <ul class={CARD_LIST}>
            <For each={rows()}>
              {(t) => <TemplateRow template={t} onEdit={() => setEditing(t)} />}
            </For>
          </ul>
        </Match>
      </Switch>

      <Button variant="outline" class={ADD_FULL} onClick={() => setCreating(true)}>
        + Write a template
      </Button>

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
  <li>
    {/* ⛔ THE WHOLE CARD IS NOT THE BUTTON, only the name and its meta line are.
        A card carries a chip beside the button, and a `<button>` wrapping a
        sibling chip is a control whose accessible name reads "house voice slack ·
        card · v3 off" — the disabled marker becoming part of the label of the
        thing that opens the editor. */}
    <Panel class={cn(PANEL_BODY, "flex items-center gap-sm")}>
      <button type="button" class="min-w-0 flex-1 text-left" onClick={props.onEdit}>
        <span class="font-medium">{props.template.name}</span>
        <span class={cn(HELP, "block")}>
          {props.template.provider} · {props.template.format} · v{props.template.version}
        </span>
      </button>
      <Show when={!props.template.enabled}>
        <Chip>off</Chip>
      </Show>
    </Panel>
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
    reply_source: existing()?.reply_source ?? "",
    enabled: existing()?.enabled ?? true,
  });
  const [tab, setTab] = createSignal<"root" | "replies">("root");
  const patch = (d: Partial<TemplateForm>) => setForm((f) => ({ ...f, ...d }));

  /*
   * ⛔ THE PREVIEW IS DEBOUNCED AND THE SAVE IS NOT. A POST per keystroke is the
   * failure this exists to avoid, and 250ms is short enough that the two columns
   * feel attached to the typing. The query key is (format, source), so a
   * keystroke undone gets its previous answer back with no round trip at all.
   * Both bodies ride one debounce and one request: the reply preview needs the
   * root body anyway (the contract requires `source`), and two timers would ask
   * the server about a pair of drafts that never existed together.
   */
  const [debounced, setDebounced] = createSignal({
    source: form().source,
    reply: form().reply_source,
  });
  createEffect(() => {
    const next = { source: form().source, reply: form().reply_source };
    const id = setTimeout(() => setDebounced(next), PREVIEW_DEBOUNCE_MS);
    onCleanup(() => clearTimeout(id));
  });

  const preview = useQuery(() => ({
    ...templatePreviewQuery(
      form().format,
      debounced().source,
      hasReplyBody(debounced().reply) ? debounced().reply : "",
    ),
    enabled: debounced().source.trim().length >= SOURCE_MIN,
  }));

  const problems = createMemo(() => preview.data?.problems ?? []);
  /** Refusals only. A warning is reported and does not stop a save. */
  const blocking = createMemo(() => problems().filter((p) => p.kind !== "warning"));
  /** Each tab shows its own body's problems, and only its own. */
  const rootBlocking = createMemo(() => blocking().filter((p) => !isReplyProblem(p)));
  const replyBlocking = createMemo(() => blocking().filter(isReplyProblem));
  /** The missing `{{ actions }}` warning is about the root card; it is the only one there is. */
  const rootWarnings = createMemo(() =>
    problems().filter((p) => p.kind === "warning" && !isReplyProblem(p)),
  );
  const replyWarnings = createMemo(() =>
    problems().filter((p) => p.kind === "warning" && isReplyProblem(p)),
  );

  /** The ordinary cards first: those are the ones an author is writing for. */
  const renderings = createMemo<readonly TemplateRendering[]>(() => {
    const all = preview.data?.renderings ?? [];
    return [...all].sort((a, b) => Number(b.representative) - Number(a.representative));
  });
  /** One per reason, in the server's order. */
  const replyRenderings = createMemo(() => preview.data?.reply_renderings ?? []);

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
    mutationFn: (body: UpdateNotificationTemplateRequest) =>
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
    if (existing()) update.mutate(toUpdateRequest(parsed.output));
    else create.mutate(toCreateRequest(parsed.output));
  };

  const busy = () => create.isPending || update.isPending || remove.isPending;

  /*
   * ⛔ A TAB THAT IS NOT SHOWING STILL OWES ITS PROBLEMS A MARK. Save is disabled
   * by a refusal in EITHER body, and an author looking at the other tab would
   * otherwise see a dead button and no reason anywhere on screen.
   */
  const rootInvalid = () => rootBlocking().length > 0 || fieldErrors().source !== undefined;
  const replyInvalid = () => replyBlocking().length > 0 || fieldErrors().reply_source !== undefined;

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

          <Tabs
            value={tab()}
            onChange={(t: string) => setTab(t === "replies" ? "replies" : "root")}
          >
            <TabsList aria-label="Which message to edit">
              <TabsTrigger value="root">
                Root card
                <Show when={rootInvalid()}>
                  <ProblemDot />
                </Show>
              </TabsTrigger>
              <TabsTrigger value="replies">
                Thread replies
                {/* A separate node, as `AlertTabs` does with its count, so the
                    tab's own word stays stable for a screen reader. */}
                <Show when={hasReplyBody(form().reply_source)}>
                  <span class="ml-1.5 text-ink-muted">(custom)</span>
                </Show>
                <Show when={replyInvalid()}>
                  <ProblemDot />
                </Show>
              </TabsTrigger>
            </TabsList>

            <TabsContent value="root" class="grid gap-4 md:grid-cols-2">
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
                  blocking={rootBlocking()}
                  warnings={rootWarnings()}
                  renderings={renderings()}
                />
              </div>
            </TabsContent>

            <TabsContent value="replies" class="grid gap-4 md:grid-cols-2">
              <TextField
                class={FIELD}
                validationState={fieldErrors().reply_source ? "invalid" : "valid"}
              >
                <TextFieldLabel class={LABEL}>The reply</TextFieldLabel>
                <TextFieldTextArea
                  class="min-h-80 font-mono text-meta"
                  value={form().reply_source}
                  spellcheck={false}
                  onInput={(e) => patch({ reply_source: e.currentTarget.value })}
                />
                <TextFieldDescription class={HELP}>
                  Leave empty and every reply is oto's own. Branch on <code>reason</code> — a reason
                  you don't handle keeps oto's own reply.
                </TextFieldDescription>
                <TextFieldErrorMessage>{fieldErrors().reply_source}</TextFieldErrorMessage>
              </TextField>

              <div class={FIELD}>
                <span class={LABEL}>What each reply sends</span>
                <PreviewPane
                  replies
                  loading={preview.isFetching}
                  blocking={replyBlocking()}
                  warnings={replyWarnings()}
                  renderings={replyRenderings()}
                />
              </div>
            </TabsContent>
          </Tabs>

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

/** The mark on a tab whose body holds a refusal — the same red the refusal list is drawn in. */
const ProblemDot: Component = () => (
  <>
    <span aria-hidden="true" class="ml-1.5 inline-block size-1.5 rounded-full bg-destructive" />
    <span class="sr-only">(has problems)</span>
  </>
);

/**
 * One body's preview. `replies` switches it to the reply corpus: each fixture is
 * a REASON, the card-only chips mean nothing there, and an empty spelling is not
 * an empty message but oto's own reply going out instead.
 */
const PreviewPane: Component<{
  replies?: boolean;
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
                <span class={cn(HELP, "ml-1")}>
                  (on the {p.fixture} {props.replies ? "reply" : "example"})
                </span>
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

    <Show
      when={
        props.replies &&
        !props.loading &&
        props.renderings.length === 0 &&
        props.blocking.length === 0
      }
    >
      <p class={HELP}>Nothing written yet, so every reply is oto's own.</p>
    </Show>

    <For each={props.renderings}>
      {(r) => (
        <div class="rounded-surface border border-border">
          <div class={cn(SECTION_LABEL, "flex items-center gap-2 border-b border-border px-3 py-1.5")}>
            <span>{r.fixture}</span>
            <Show when={!props.replies && r.representative}>
              <Chip>ordinary card</Chip>
            </Show>
            <Show when={!props.replies && !r.has_actions}>
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
                  <Switch>
                    <Match when={s.error}>
                      <p class="text-meta text-destructive">{s.error}</p>
                    </Match>
                    {/* ⭐ NEITHER TEXT NOR ERROR IS THE CONTRACT'S "this reason is
                        not handled": the body branched past it, so oto's own reply
                        goes out. It is said in words, never drawn as a blank. */}
                    <Match when={props.replies && s.text === ""}>
                      <p class={HELP}>oto's own reply</p>
                    </Match>
                    <Match when={true}>
                      <pre class="whitespace-pre-wrap break-words font-mono text-meta">{s.text}</pre>
                    </Match>
                  </Switch>
                </div>
              )}
            </For>
          </div>
        </div>
      )}
    </For>
  </div>
);
