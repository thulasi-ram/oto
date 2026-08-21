/**
 * Wordings — the customer's own words for one Stanza of a notification.
 *
 * ⭐ THE PREVIEW PANE IS THE POINT OF THIS SCREEN, AND IT TEACHES ONE THING
 * (ADR 0048). A Wording writes TEXT and never markup: a curated filter emits a
 * NEUTRAL mark and each provider's Dialect spells it, so `{{ alert.severity |
 * bold }}` is `*critical*` on Slack and `critical` down a webhook. An author
 * shown one spelling concludes that punctuation is theirs to write; an author
 * shown both, side by side, on the same template, cannot. So every fixture is
 * rendered in every Dialect the SERVER answered with — never a list this file
 * keeps — and the two columns sit beside each other rather than behind a tab.
 *
 * ⛔ STRUCTURE STAYS OTO'S, WHICH IS WHY THERE IS NO COLOUR, NO BLOCK AND NO
 * BUTTON ON THIS FORM (ADR 0037). Four of SPEC §H.7's eight stanzas take prose;
 * the other four are a grid, two sequences and a row of buttons whose labels are
 * bound to their action ids. All eight are offered here anyway, and the four that
 * are refused are refused BY THE SERVER, IN A SENTENCE — a name silently missing
 * from a menu teaches nobody why it is missing.
 *
 * ⭐ PRECEDENCE IS READ OFF THE SCREEN, NOT OFF THE DOCS (ADR 0049). A Wording
 * carries its own `when` clause and nothing consults routing. A channel-bound row
 * beats an org-wide one; within a scope `priority` is LOWER FIRST and the first
 * match wins, per Stanza. "Which of these actually speaks on #alerts" is
 * therefore a question with an answer, and `PrecedencePanel` is that answer.
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

import { maxLengthOf, maxValueOf, minLengthOf, minValueOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import { createWording, deleteWording, updateWording } from "~/api/endpoints";
import {
  CreateWordingRequestSchema,
  MatcherDTOSchema,
  NotificationReasonSchema,
  WordingStanzaSchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { channelsQuery, wordingPreviewQuery, wordingsQuery } from "~/api/queries";
import type {
  Channel,
  CreateWordingRequest,
  Matcher,
  NotificationReason,
  Wording,
  WordingRendering,
  WordingStanza,
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
import {
  Select,
  SelectContent,
  SelectHiddenSelect,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
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
import { keepPrevious } from "~/lib/keysetFeed";
import { formatMatchers, parseMatchers } from "~/lib/matchers";
import { MatcherInput } from "~/features/alerts/MatcherInput";
import { REASON_LABEL } from "./vocabulary";

/*
 * The same rhythm every other form on the product uses. Shared rather than
 * copied for the reason `rhythm.ts` states: two panels a click apart that each
 * picked their own gap read as two different products.
 */
import {
  CHECK_LABEL,
  CHECK_ROW,
  FIELD,
  FIELD_ROW,
  FORM,
  HELP,
  LABEL,
  LEGEND,
  PANEL_HEADER,
  ROW,
  SECTION,
} from "~/features/settings/rhythm";

/**
 * Every Stanza a card has, READ from the contract's own enum.
 *
 * ⛔ ALL EIGHT ARE OFFERED AND FOUR OF THEM WILL BE REFUSED. That is the design
 * and not an oversight: SPEC §H.7 names eight, the enum carries eight, and a menu
 * that quietly showed four would leave an author who went looking for `fields`
 * with nothing to read. The refusal — and the sentence explaining which kind of
 * structure the stanza is — comes back from the server, so this file never has to
 * hold a second opinion about which four are prose.
 */
const STANZAS: readonly WordingStanza[] = WordingStanzaSchema.options;

/** Which facts a Wording may speak for. Read from the contract, like Policies'. */
const REASONS: readonly NotificationReason[] = NotificationReasonSchema.options;

/* -------------------------------------------------------------------------- */
/* The contract's bounds, read rather than repeated                           */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ NOT ONE OF THESE NUMBERS IS WRITTEN HERE, for the reason `PoliciesSection`
 * learned the hard way: a bound the form does not know about is a bound the
 * operator meets as a 422 with the dialog still full of their work. They come off
 * `CreateWordingRequestSchema`, which gate G4 generates from the contract and
 * which is the very schema the request is gated through below.
 */
const TEMPLATE_MIN = minLengthOf(CreateWordingRequestSchema, "template");
const TEMPLATE_MAX = maxLengthOf(CreateWordingRequestSchema, "template");
const MATCHERS_MAX = maxLengthOf(CreateWordingRequestSchema, "matchers");
const REASONS_MAX = maxLengthOf(CreateWordingRequestSchema, "reasons");
const PRIORITY_MIN = minValueOf(CreateWordingRequestSchema, "priority");
const PRIORITY_MAX = maxValueOf(CreateWordingRequestSchema, "priority");

const PRIORITY_RANGE = `oto accepts ${PRIORITY_MIN}–${PRIORITY_MAX}. Lower is asked first.`;

/**
 * How long the author stops typing before the preview asks the server.
 *
 * The answer is cached per (stanza, template) — `wordingPreviewQuery` — so this
 * only governs how many DISTINCT drafts become requests, never whether a draft
 * already seen costs one.
 */
const PREVIEW_DEBOUNCE_MS = 250;

/**
 * What the dialog holds, before it is anything the API has a name for.
 *
 * `channel_id` is the empty string for the house voice rather than `null`,
 * because a `Select` picks between OPTIONS and an option needs a value; it is
 * turned back into `null` on the way to the wire.
 */
interface WordingForm {
  readonly channelId: string;
  readonly stanza: WordingStanza;
  readonly template: string;
  readonly matchers: readonly Matcher[];
  readonly reasons: readonly string[];
  readonly priority: number;
  readonly enabled: boolean;
}

function toCreateWordingRequest(form: WordingForm): CreateWordingRequest {
  return {
    channel_id: form.channelId === "" ? null : form.channelId,
    stanza: form.stanza,
    template: form.template,
    matchers: form.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
    reasons: [...form.reasons],
    priority: form.priority,
    enabled: form.enabled,
  };
}

/*
 * The form schema is hand-written — the sentences are the whole point of it —
 * but it `v.pipe`s into the GENERATED request schema as its final gate. That
 * last line is the difference between a form that agrees with the contract today
 * and a form that cannot construct a body the API would refuse.
 */
const WordingFormSchema = v.pipe(
  v.strictObject({
    channelId: v.string(),
    stanza: WordingStanzaSchema,
    template: v.pipe(
      v.string(),
      v.minLength(
        TEMPLATE_MIN,
        "A wording with no text would blank the stanza. Delete the wording instead — the stanza then reads in oto's own words again.",
      ),
      v.maxLength(
        TEMPLATE_MAX,
        `A wording is one line of prose, and the limit is ${TEMPLATE_MAX} characters.`,
      ),
    ),
    matchers: v.pipe(
      v.array(MatcherDTOSchema),
      v.maxLength(MATCHERS_MAX, `At most ${MATCHERS_MAX} matchers on one wording.`),
    ),
    reasons: v.pipe(
      v.array(v.string()),
      v.maxLength(REASONS_MAX, `At most ${REASONS_MAX} facts on one wording.`),
    ),
    priority: v.pipe(
      v.number("Priority is a whole number."),
      v.integer("Priority is a whole number."),
      v.minValue(PRIORITY_MIN, PRIORITY_RANGE),
      v.maxValue(PRIORITY_MAX, PRIORITY_RANGE),
    ),
    enabled: v.boolean(),
  }),
  v.transform((form): v.InferInput<typeof CreateWordingRequestSchema> =>
    toCreateWordingRequest(form),
  ),
  CreateWordingRequestSchema, // the generated schema is the final gate
);

/* -------------------------------------------------------------------------- */
/* Precedence, as arithmetic rather than as prose                             */
/* -------------------------------------------------------------------------- */

/** A row still in force: the soft-deleted ones are history, not configuration. */
function live(rows: readonly Wording[]): readonly Wording[] {
  return rows.filter((w) => (w.deleted_at ?? null) === null);
}

/** The house voice — the rows that name no destination. */
function isHouse(w: Wording): boolean {
  return (w.channel_id ?? null) === null;
}

/**
 * The server's own order within one scope: LOWER PRIORITY FIRST, and the oldest
 * row first when two share a number.
 *
 * ⛔ "Lower first" is the sentence `notification_policies.priority` already
 * carries, deliberately (ADR 0049). Two orderings that read the same way and
 * behave differently is how an operator learns to distrust both — so this screen
 * must not quietly sort the other way for looking tidier.
 */
function inPrecedenceOrder(rows: readonly Wording[]): readonly Wording[] {
  return [...rows].sort(
    (a, b) => a.priority - b.priority || a.created_at.localeCompare(b.created_at),
  );
}

/**
 * Every row that could speak on one destination, MOST SPECIFIC FIRST.
 *
 * A channel-bound Wording beats an org-wide one — a rule naming one destination
 * is more specific than one naming a whole tenant — and there is no third scope
 * and no cascade. `null` is "nowhere in particular", which is the house voice on
 * its own.
 */
function chainFor(rows: readonly Wording[], channelId: string | null): readonly Wording[] {
  const all = live(rows);
  const bound =
    channelId === null ? [] : inPrecedenceOrder(all.filter((w) => w.channel_id === channelId));
  return [...bound, ...inPrecedenceOrder(all.filter(isHouse))];
}

/**
 * Where each row stands in the queue for its own Stanza, by row id.
 *
 * ⚠️ A DISABLED ROW IS NOT IN THE QUEUE AT ALL, which is why it is skipped rather
 * than ranked last: it is not asked, so ranking it would imply it could still
 * answer if the ones above it declined.
 *
 * ⚠️ AND RANK 0 IS "ASKED FIRST", NEVER "WINS". A Wording's `when` clause can
 * decline — an unmatched matcher passes the turn to the next row for that Stanza,
 * and if every row declines the Stanza reads in oto's own words. Only the server
 * knows, per notification, which one actually spoke; the honest thing this screen
 * can say is the order they are asked in.
 */
function ranksIn(chain: readonly Wording[]): ReadonlyMap<string, number> {
  const seen = new Map<WordingStanza, number>();
  const out = new Map<string, number>();
  for (const w of chain) {
    if (!w.enabled) continue;
    const next = seen.get(w.stanza) ?? 0;
    out.set(w.id, next);
    seen.set(w.stanza, next + 1);
  }
  return out;
}

/** What to call a destination that may since have been deleted. */
function destinationName(channels: ReadonlyMap<string, Channel>, id: string): string {
  return channels.get(id)?.name ?? "a removed channel";
}

/** How a row names its own scope, in one phrase. */
function scopeOf(w: Wording, channels: ReadonlyMap<string, Channel>): string {
  return isHouse(w) ? "the house voice" : destinationName(channels, w.channel_id ?? "");
}

/* -------------------------------------------------------------------------- */

export const WordingsSection: Component = () => {
  const [editing, setEditing] = createSignal<Wording | null>(null);
  const [creating, setCreating] = createSignal(false);
  /** "" is "nowhere in particular" — the house voice asked on its own. */
  const [focus, setFocus] = createSignal("");

  const wordings = useQuery(() => wordingsQuery());
  const channels = useQuery(() => channelsQuery());

  const byId = createMemo(() => {
    const map = new Map<string, Channel>();
    for (const c of channels.data?.data ?? []) map.set(c.id, c);
    return map;
  });

  const rows = createMemo<readonly Wording[]>(() => wordings.data?.data ?? []);
  const chain = createMemo(() => chainFor(rows(), focus() === "" ? null : focus()));
  const ranks = createMemo(() => ranksIn(chain()));

  /** The destinations that have a Wording of their own, in the order shown. */
  const boundGroups = createMemo<readonly (readonly [string, readonly Wording[]])[]>(() => {
    const groups = new Map<string, Wording[]>();
    for (const w of live(rows())) {
      if (isHouse(w)) continue;
      const id = w.channel_id ?? "";
      const bucket = groups.get(id);
      if (bucket === undefined) groups.set(id, [w]);
      else bucket.push(w);
    }
    return [...groups.entries()]
      .map(([id, group]) => [id, inPrecedenceOrder(group)] as const)
      .sort((a, b) => destinationName(byId(), a[0]).localeCompare(destinationName(byId(), b[0])));
  });

  const houseRows = createMemo(() => inPrecedenceOrder(live(rows()).filter(isHouse)));

  /*
   * ⛔ A FUNCTION, NOT A SPREADABLE OBJECT. `<WordingRow {...rowProps(w)} />`
   * evaluates the call once and hands the row plain values, so changing the
   * destination above would leave every rank chip on screen answering the
   * previous question. Written out, Solid compiles each attribute into a getter
   * and the chips track `focus()` the way the panel does.
   */
  const row = (w: Wording) => (
    <WordingRow
      wording={w}
      channels={byId()}
      rank={ranks().get(w.id)}
      inChain={chain().some((c) => c.id === w.id)}
      onEdit={() => setEditing(w)}
    />
  );

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Wordings</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setCreating(true)}>
            Add a wording
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={wordings.isPending}>
            <LoadingLine />
          </Match>
          <Match when={wordings.isError}>
            <ErrorState error={wordings.error} onRetry={() => void wordings.refetch()} />
          </Match>
          <Match when={live(rows()).length === 0}>
            <PageEmptyState
              motif="kumo"
              title="No wordings."
              body="Every card reads in oto's own words. A wording replaces the text of one stanza — the title, the body, the rule line or the footer — with one line of your own, and nothing else about the card changes."
            />
          </Match>
          <Match when={true}>
            <div class={cn(ROW, "flex flex-col gap-md")}>
              <PrecedencePanel
                channels={channels.data?.data ?? []}
                byId={byId()}
                chain={chain()}
                focus={focus()}
                onFocus={setFocus}
              />
            </div>

            <Show when={boundGroups().length > 0}>
              <div class={cn(ROW, "flex flex-col gap-2xs")}>
                <p class={cn(SECTION_LABEL, "text-ink-muted")}>Per destination — asked first</p>
                <p class={HELP}>
                  A wording naming one destination is more specific than one naming the whole
                  organisation, so it is asked before the house voice for its stanza.
                </p>
              </div>
              <For each={boundGroups()}>
                {([id, group]) => (
                  <>
                    <div class={cn(ROW, "flex items-center gap-sm bg-sunken")}>
                      <span class="text-body font-medium text-ink">
                        {destinationName(byId(), id)}
                      </span>
                      <Chip>
                        {group.length === 1 ? "1 wording" : `${group.length} wordings`}
                      </Chip>
                    </div>
                    <ul>
                      <For each={group}>{(w) => row(w)}</For>
                    </ul>
                  </>
                )}
              </For>
            </Show>

            <Show when={houseRows().length > 0}>
              <div class={cn(ROW, "flex flex-col gap-2xs")}>
                <p class={cn(SECTION_LABEL, "text-ink-muted")}>The house voice — every card this organisation sends</p>
                <p class={HELP}>
                  Reached for a stanza only where no wording above it answered for that stanza.
                </p>
              </div>
              <ul>
                <For each={houseRows()}>{(w) => row(w)}</For>
              </ul>
            </Show>
          </Match>
        </Switch>
      </Panel>

      <WordingDialog
        open={creating() || editing() !== null}
        wording={editing()}
        channels={channels.data?.data ?? []}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
      />
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* "Which of these speaks on #alerts?"                                        */
/* -------------------------------------------------------------------------- */

/**
 * The screen's central claim, spelled out for one destination at a time.
 *
 * ⭐ THIS EXISTS BECAUSE THE LIST BELOW CANNOT ANSWER THE QUESTION ON ITS OWN. A
 * list ordered by scope and priority shows the RULE; it does not show the
 * RESULT, and "which of these actually speaks on #alerts" is the only question an
 * operator has when they arrive here. Picking a destination resolves the two
 * scopes into one queue and names, per Stanza, the row at the front of it.
 */
const PrecedencePanel: Component<{
  readonly channels: readonly Channel[];
  readonly byId: ReadonlyMap<string, Channel>;
  readonly chain: readonly Wording[];
  readonly focus: string;
  readonly onFocus: (next: string) => void;
}> = (props) => {
  interface Destination {
    readonly id: string;
    readonly label: string;
  }
  const ANYWHERE: Destination = { id: "", label: "Anywhere else" };

  const options = createMemo<readonly Destination[]>(() => [
    ANYWHERE,
    ...props.channels.map((c) => ({ id: c.id, label: c.name })),
  ]);
  const chosen = createMemo<Destination>(
    () => options().find((o) => o.id === props.focus) ?? ANYWHERE,
  );

  const leaders = createMemo(() => {
    const seen = new Set<WordingStanza>();
    const out: Wording[] = [];
    for (const w of props.chain) {
      if (!w.enabled || seen.has(w.stanza)) continue;
      seen.add(w.stanza);
      out.push(w);
    }
    return out;
  });

  const where = (): string => (props.focus === "" ? "every other destination" : chosen().label);

  return (
    <>
      <div class={FIELD_ROW}>
        <div class="min-w-64 flex-1">
          <Select<Destination>
            class={FIELD}
            options={[...options()]}
            optionValue="id"
            optionTextValue="label"
            value={chosen()}
            onChange={(next) => props.onFocus(next?.id ?? "")}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue.label}</SelectItem>
            )}
          >
            <SelectLabel class="block">Which destination?</SelectLabel>
            <SelectTrigger>
              <SelectValue<Destination>>{(state) => state.selectedOption().label}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>
      </div>

      <Show
        when={leaders().length > 0}
        fallback={
          <p class={HELP}>
            Nothing is asked for {where()}. Every stanza of every card sent there reads in oto's
            own words.
          </p>
        }
      >
        <div class="flex flex-col gap-xs">
          <p class={LABEL}>Asked first on {where()}</p>
          <ul class="flex flex-col gap-xs">
            <For each={leaders()}>
              {(w) => {
                const behind = (): number =>
                  props.chain.filter((c) => c.enabled && c.stanza === w.stanza).length - 1;
                return (
                  <li class="flex flex-wrap items-baseline gap-sm">
                    <Chip mono>{w.stanza}</Chip>
                    <code class="min-w-0 flex-1 truncate font-mono text-meta text-ink">
                      {w.template}
                    </code>
                    <span class="text-meta text-ink-subtle">
                      from {scopeOf(w, props.byId)}
                      {behind() > 0
                        ? `, ahead of ${behind() === 1 ? "1 other" : `${behind()} others`}`
                        : ""}
                    </span>
                  </li>
                );
              }}
            </For>
          </ul>
          {/* ⚠️ The honest caveat, and it is not small print: the order is
              knowable from configuration, the outcome is not. A `when` clause
              that does not match passes the turn to the next row, and if every
              row for a stanza declines, oto's own words are used. */}
          <p class={HELP}>
            Asked first is not the same as spoken: a wording whose <code class="font-mono">when</code>{" "}
            clause does not match passes the turn to the next one for that stanza, and if none of
            them match, the stanza reads in oto's own words.
          </p>
        </div>
      </Show>
    </>
  );
};

/* -------------------------------------------------------------------------- */

const WordingRow: Component<{
  readonly wording: Wording;
  readonly channels: ReadonlyMap<string, Channel>;
  /** Its place in the queue for the chosen destination, or `undefined` if it is not in it. */
  readonly rank: number | undefined;
  readonly inChain: boolean;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const w = (): Wording => props.wording;

  const remove = useMutation(() => ({
    mutationFn: () => deleteWording(w().id),
    // ⛔ `qk.wordings.list()`, NOT the `["wordings"]` prefix. Every cached
    // preview lives under that prefix and none of them can have been changed by
    // this delete; invalidating the parent would re-POST every draft the author
    // has typed today.
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.wordings.list() }),
  }));

  return (
    <li class={cn(ROW, "flex flex-col gap-sm", w().enabled ? "" : "opacity-60")}>
      <div class="flex min-h-8 flex-wrap items-center gap-sm">
        <Chip mono title="Which part of the card this writes.">
          {w().stanza}
        </Chip>
        <Chip title="Lower is asked first.">priority {w().priority}</Chip>
        <Show when={props.inChain && props.rank !== undefined}>
          <Chip
            class={props.rank === 0 ? "border-line-strong text-ink" : ""}
            title="Where this sits in the queue for the destination chosen above."
          >
            {props.rank === 0 ? `asked first for ${w().stanza}` : `asked #${(props.rank ?? 0) + 1}`}
          </Chip>
        </Show>
        <Show when={!w().enabled}>
          <Chip title="A disabled wording is never asked.">disabled</Chip>
        </Show>
        <div class="ml-auto flex items-center gap-sm">
          <Button size="sm" variant="secondary" onClick={props.onEdit}>
            Edit
          </Button>
          <Button
            size="sm"
            variant="destructive"
            busy={remove.isPending}
            onClick={() => remove.mutate()}
          >
            Remove
          </Button>
        </div>
      </div>

      <div class="flex flex-col gap-2xs text-meta text-ink-muted">
        <p>
          <span class="text-ink-subtle">says</span>{" "}
          <code class="font-mono text-ink">{w().template}</code>
        </p>
        <p>
          <span class="text-ink-subtle">when</span>{" "}
          <code class="font-mono text-ink">
            {w().matchers.length === 0
              ? "anything"
              : formatMatchers(
                  w().matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
                )}
          </code>
        </p>
        <p>
          <span class="text-ink-subtle">about</span>{" "}
          {w().reasons.length === 0
            ? "every fact"
            : w()
                .reasons.map((r) => REASON_LABEL[r as NotificationReason] ?? r)
                .join(", ")}
        </p>
      </div>

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} />
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */
/* Writing one                                                                */
/* -------------------------------------------------------------------------- */

const WordingDialog: Component<{
  readonly open: boolean;
  readonly wording: Wording | null;
  readonly channels: readonly Channel[];
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.wording !== null;

  const [channelId, setChannelId] = createSignal("");
  const [stanza, setStanza] = createSignal<WordingStanza>("body");
  const [template, setTemplate] = createSignal("");
  const [matcherText, setMatcherText] = createSignal("");
  const [reasons, setReasons] = createSignal<readonly string[]>([]);
  const [priority, setPriority] = createSignal(100);
  const [enabled, setEnabled] = createSignal(true);
  const [seeded, setSeeded] = createSignal(false);
  // Nothing complains until something has been typed: a dialog that opens
  // already shouting at an empty box is a dialog people learn to ignore.
  const [touched, setTouched] = createSignal(false);

  // Seed once per *opening*. The dialog stays mounted, so this must be an
  // effect: the component body runs only in the state it mounted in.
  createEffect(() => {
    if (!props.open) {
      if (seeded()) setSeeded(false);
      return;
    }
    if (seeded()) return;
    setSeeded(true);
    setTouched(false);
    const existing = props.wording;
    if (existing !== null) {
      setChannelId(existing.channel_id ?? "");
      setStanza(existing.stanza);
      setTemplate(existing.template);
      setMatcherText(
        formatMatchers(existing.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value }))),
      );
      setReasons(existing.reasons);
      setPriority(existing.priority);
      setEnabled(existing.enabled);
    } else {
      setChannelId("");
      setStanza("body");
      setTemplate(STARTER_TEMPLATE);
      setMatcherText("");
      setReasons([]);
      setPriority(100);
      setEnabled(true);
    }
  });

  /** A Wording's matchers are `MatcherDTO`s, so `=~`/`!~` are accepted here. */
  const matchers = createMemo<readonly Matcher[]>(() =>
    parseMatchers(matcherText()).matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
  );

  const form = (): WordingForm => ({
    channelId: channelId(),
    stanza: stanza(),
    template: template(),
    matchers: matchers(),
    reasons: reasons(),
    priority: priority(),
    enabled: enabled(),
  });

  /** One parse, through the generated request schema. */
  const gated = createMemo(() => v.safeParse(WordingFormSchema, form()));

  const localError = (field: string): string | undefined => {
    if (!touched()) return undefined;
    const result = gated();
    if (result.success) return undefined;
    return result.issues.find((i) => i.path?.[0]?.key === field)?.message;
  };

  /**
   * ⭐ THE SERVER'S OWN REFUSAL FOR A STANZA THAT TAKES NO WORDING.
   *
   * It is not computed here, and could not honestly be: which four of the eight
   * stanzas are prose is the server's ruling, and the SENTENCE explaining that a
   * fields grid is ten separately-budgeted cells — or that a button's label is
   * bound to its action id — is the whole reason those four are listed rather
   * than hidden. The preview endpoint runs the same save-time gate, so choosing
   * one puts the reason on screen before anything is sent.
   */
  const [refusal, setRefusal] = createSignal<string | undefined>(undefined);

  const mutation = useMutation(() => ({
    mutationFn: (body: CreateWordingRequest) => {
      const existing = props.wording;
      if (existing === null) return createWording(body);
      // ⛔ `stanza` AND `channel_id` ARE NOT SENT, because the contract has no
      // field for either (ADR 0049). Moving a Wording between stanzas or scopes
      // is a different Wording — the read set differs, the budget differs, and
      // the row's history would claim it had always been the new one.
      return updateWording(existing.id, {
        template: body.template,
        matchers: body.matchers ?? [],
        reasons: body.reasons ?? [],
        priority: body.priority,
        enabled: body.enabled,
      });
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.wordings.list() });
      setSeeded(false);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);
  const stanzaProblem = (): string | undefined => refusal() ?? violations().get("stanza");

  const destinationLabel = (): string => {
    const id = channelId();
    if (id === "") return "The house voice — every card this organisation sends";
    return props.channels.find((c) => c.id === id)?.name ?? "a removed channel";
  };

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          setSeeded(false);
          props.onClose();
        }
      }}
    >
      <ModalContent class="max-w-256">
        <ModalHeader>
          <ModalTitle>{editing() ? "Edit this wording" : "Add a wording"}</ModalTitle>
          <ModalDescription>
            One line of your own words in place of oto's, for one part of the card. Structure stays
            oto's: the blocks, the colour and the buttons are not yours to set, and the punctuation
            of a mark is spelled by each provider rather than typed here.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <Show
            when={!editing()}
            fallback={
              /* ⛔ NEITHER IS EDITABLE, AND THE FORM SAYS SO RATHER THAN GOING
                 QUIET. The PATCH body has no field for a stanza or a channel, so
                 a control offering one could only ever be a control whose value
                 is discarded. */
              <div class={FIELD}>
                <span class={LABEL}>Where and what</span>
                <p class="text-body text-ink">
                  <code class="font-mono">{props.wording?.stanza}</code> on {destinationLabel()}
                </p>
                <p class={HELP}>
                  Neither can be changed. Moving a wording to a different stanza, or re-binding it
                  to a different destination, changes which cards it speaks on and what it may say
                  — that is a different wording. Delete this one and write it.
                </p>
              </div>
            }
          >
            <div class={FIELD_ROW}>
              <div class="min-w-64 flex-1">
                <DestinationSelect
                  channels={props.channels}
                  value={channelId()}
                  onChange={(next) => {
                    setTouched(true);
                    setChannelId(next);
                  }}
                />
              </div>
              <div class="min-w-48 flex-1">
                <StanzaSelect
                  value={stanza()}
                  problem={stanzaProblem()}
                  onChange={(next) => {
                    setTouched(true);
                    setStanza(next);
                  }}
                />
              </div>
            </div>
          </Show>

          <TextField
            class={FIELD}
            value={template()}
            validationState={
              (localError("template") ?? violations().get("template")) ? "invalid" : "valid"
            }
            onChange={(value) => {
              setTouched(true);
              setTemplate(value);
            }}
          >
            <TextFieldLabel>
              What it says
              <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                *
              </span>
            </TextFieldLabel>
            <TextFieldTextArea
              class="font-mono"
              rows={2}
              maxLength={TEMPLATE_MAX}
              placeholder={STARTER_TEMPLATE}
            />
            <TextFieldDescription class={HELP}>
              Liquid, with no <code class="font-mono">{"{% if %}"}</code> and no{" "}
              <code class="font-mono">{"{% for %}"}</code> — there is no branching and no
              iteration, and the ceiling is one line of prose. Filters emit a neutral mark and each
              provider spells it, so write <code class="font-mono">| bold</code> rather than
              asterisks.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {localError("template") ?? violations().get("template")}
            </TextFieldErrorMessage>
          </TextField>

          <WordingPreviewPanel
            stanza={stanza()}
            template={template()}
            onRefusal={setRefusal}
          />

          <fieldset>
            <legend class={LEGEND}>When it is asked</legend>

            <div class={FIELD}>
              <label for="wording-matchers" class={LABEL}>
                Matchers
              </label>
              <MatcherInput
                id="wording-matchers"
                value={matcherText()}
                onChange={(next) => {
                  setTouched(true);
                  setMatcherText(next);
                }}
                onCommit={() => undefined}
              />
              <p class={HELP}>
                All matchers must match. An empty list matches everything, which is what makes one
                org-wide row the natural way to set a house voice. At most {MATCHERS_MAX}.
              </p>
              <Show when={localError("matchers") ?? violations().get("matchers")}>
                {(msg) => (
                  <p class="text-meta font-medium text-ink" role="alert">
                    {msg()}
                  </p>
                )}
              </Show>
            </div>

            <div class={cn(FIELD, "mt-md")}>
              <ToggleGroup
                showLegend
                legend="Only for these facts"
                multiple
                value={[...reasons()]}
                onChange={(next) => {
                  setTouched(true);
                  setReasons(next);
                }}
              >
                <For each={REASONS}>
                  {(r) => <ToggleGroupItem value={r}>{REASON_LABEL[r]}</ToggleGroupItem>}
                </For>
              </ToggleGroup>
              {/* ⚠️ The contract types this as free strings rather than as the
                  reason enum, because the channels module does not depend on the
                  notification module. Offering the enum is still right: a name
                  that matches nothing simply never selects the wording. */}
              <p class={HELP}>Pick none to speak for every fact.</p>
            </div>

            <div class={cn(FIELD_ROW, "mt-md")}>
              <TextField
                class={cn(FIELD, "w-32")}
                value={Number.isFinite(priority()) ? String(priority()) : ""}
                validationState={
                  (localError("priority") ?? violations().get("priority")) ? "invalid" : "valid"
                }
                onChange={(value) => {
                  setTouched(true);
                  // An unreadable box becomes `NaN` rather than the last good
                  // number: silently keeping the previous value would save
                  // something the operator can no longer see.
                  setPriority(Number.parseInt(value, 10));
                }}
              >
                <TextFieldLabel>Priority</TextFieldLabel>
                <TextFieldInput type="number" min={PRIORITY_MIN} max={PRIORITY_MAX} step={1} />
                <TextFieldDescription class={HELP}>
                  {`Lower first. ${PRIORITY_MIN}–${PRIORITY_MAX}.`}
                </TextFieldDescription>
                <TextFieldErrorMessage role="alert">
                  {localError("priority") ?? violations().get("priority")}
                </TextFieldErrorMessage>
              </TextField>

              <div class={cn(CHECK_ROW, "self-end pb-md")}>
                <Checkbox id="wording-enabled" checked={enabled()} onChange={setEnabled} />
                <label for="wording-enabled-input" class={CHECK_LABEL}>
                  Enabled
                </label>
              </div>
            </div>
          </fieldset>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={!gated().success || refusal() !== undefined}
            onClick={() => {
              setTouched(true);
              const parsed = gated();
              if (!parsed.success) return;
              mutation.mutate(parsed.output);
            }}
          >
            {editing() ? "Save" : "Create"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/**
 * The starter, and it is a starter rather than a placeholder on purpose: the
 * preview pane is the teaching surface, and a pane that says "type something
 * first" teaches nothing. Every filter and field named here is one the engine
 * registers, so the pane is alive on the first frame and the author edits words
 * rather than inventing syntax.
 */
const STARTER_TEMPLATE =
  '{{ alert.name }} is {{ alert.severity | bold }}, {{ group.firing_for | human_duration }} in, {{ alert.total_cases | plural: "case", "cases" }} this week';

/* -------------------------------------------------------------------------- */

/** Where a Wording applies. `""` is the house voice, which names no destination. */
const DestinationSelect: Component<{
  readonly channels: readonly Channel[];
  readonly value: string;
  readonly onChange: (next: string) => void;
}> = (props) => {
  interface Destination {
    readonly id: string;
    readonly label: string;
  }
  const HOUSE: Destination = { id: "", label: "The house voice — every card" };

  const options = createMemo<readonly Destination[]>(() => [
    HOUSE,
    ...props.channels.map((c) => ({ id: c.id, label: c.name })),
  ]);
  const chosen = createMemo<Destination>(() => options().find((o) => o.id === props.value) ?? HOUSE);

  return (
    <Select<Destination>
      class={FIELD}
      options={[...options()]}
      optionValue="id"
      optionTextValue="label"
      value={chosen()}
      onChange={(next) => props.onChange(next?.id ?? "")}
      itemComponent={(itemProps) => (
        <SelectItem item={itemProps.item}>{itemProps.item.rawValue.label}</SelectItem>
      )}
    >
      <SelectLabel class="block">Where it applies</SelectLabel>
      <SelectTrigger>
        <SelectValue<Destination>>{(state) => state.selectedOption().label}</SelectValue>
      </SelectTrigger>
      <SelectHiddenSelect />
      <SelectContent />
    </Select>
  );
};

/**
 * Which part of the card this writes — all eight of them.
 *
 * ⛔ THE FOUR THAT TAKE NO WORDING ARE IN THIS MENU, and nothing here marks them
 * as such. Marking them would mean keeping a second opinion about which four are
 * prose, and the point of listing-and-refusing is that the REASON is what a
 * reader needs: picking `fields` puts the server's own sentence about the grid on
 * screen. A menu that greyed them out would still teach nobody why.
 */
const StanzaSelect: Component<{
  readonly value: WordingStanza;
  readonly problem: string | undefined;
  readonly onChange: (next: WordingStanza) => void;
}> = (props) => (
  <Select<WordingStanza>
    class={FIELD}
    options={[...STANZAS]}
    value={props.value}
    onChange={(next) => {
      if (next !== null) props.onChange(next);
    }}
    validationState={props.problem === undefined ? "valid" : "invalid"}
    itemComponent={(itemProps) => (
      <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
    )}
  >
    <SelectLabel class="block">Which part of the card</SelectLabel>
    <SelectTrigger>
      <SelectValue<WordingStanza>>{(state) => state.selectedOption()}</SelectValue>
    </SelectTrigger>
    <SelectHiddenSelect />
    <SelectContent />
    <Show when={props.problem}>
      {(msg) => (
        <p class="text-meta font-medium leading-snug text-ink" role="alert">
          {msg()}
        </p>
      )}
    </Show>
  </Select>
);

/* -------------------------------------------------------------------------- */
/* ⭐ The preview                                                              */
/* -------------------------------------------------------------------------- */

/**
 * What this template would say, on every fixture, in every Dialect.
 *
 * ⛔ THE PROBLEMS AND THE OUTPUT ARE ON SCREEN TOGETHER, ALWAYS. The endpoint
 * answers `200` for a template it would refuse to save, precisely so that a
 * client can show both — the fix for "renders empty on a digest" is usually only
 * visible next to what it did render on the other six. Treating `problems` as an
 * error state that replaces the renderings would throw away the half that
 * explains the other half.
 */
const WordingPreviewPanel: Component<{
  readonly stanza: WordingStanza;
  readonly template: string;
  /** The server's sentence for a stanza that takes no wording, or `undefined`. */
  readonly onRefusal: (message: string | undefined) => void;
}> = (props) => {
  // The template as the preview asks about it: settled, not mid-keystroke. The
  // answer is cached per (stanza, template), so this bounds how many DISTINCT
  // drafts become requests rather than how often a repeated one does.
  const [settled, setSettled] = createSignal("");
  createEffect(() => {
    const next = props.template;
    const timer = setTimeout(() => setSettled(next), PREVIEW_DEBOUNCE_MS);
    onCleanup(() => clearTimeout(timer));
  });

  const preview = useQuery(() => ({
    ...wordingPreviewQuery(props.stanza, settled()),
    enabled: settled().trim() !== "",
    // Keep the last rendering on screen while the next one is in flight: an
    // authoring loop that blanks between keystrokes is one nobody can read.
    placeholderData: keepPrevious,
  }));

  // The refusal travels up so the Save button can honour it. It is a `field:
  // "stanza"` violation and never anything this file decides.
  createEffect(() => {
    props.onRefusal(preview.data?.problems.find((p) => p.field === "stanza")?.message);
  });
  onCleanup(() => props.onRefusal(undefined));

  /** Characters the server stripped before rendering — bidi overrides and friends. */
  const sanitised = (): boolean =>
    preview.data !== undefined && preview.data.template !== settled();

  return (
    <fieldset>
      <legend class={LEGEND}>What it would say</legend>

      <p class={cn("mb-md", HELP)}>
        The same template on every card oto ships a fixture for, spelled by every provider it can
        write for. Nothing is saved and nothing is sent.
      </p>

      <Switch>
        <Match when={settled().trim() === ""}>
          <p class={HELP}>Write a line above and it will be rendered here.</p>
        </Match>
        <Match when={preview.isError}>
          <ErrorBanner error={preview.error} />
        </Match>
        <Match when={preview.data === undefined}>
          <LoadingLine label="Rendering…" />
        </Match>
        <Match when={preview.data}>
          {(data) => (
            <div class="flex flex-col gap-md">
              <Show when={data().problems.length > 0}>
                <ul
                  class="flex flex-col gap-2xs rounded-control border border-line-strong bg-sunken px-md py-sm"
                  role="alert"
                >
                  <For each={data().problems}>
                    {(problem) => (
                      <li class="text-meta leading-snug text-ink">
                        <span class="font-mono text-ink-subtle">{problem.field}</span>{" "}
                        {problem.message}
                      </li>
                    )}
                  </For>
                  <li class="text-meta leading-snug text-ink-muted">
                    Saving is refused while any of these stand. What rendered anyway is below.
                  </li>
                </ul>
              </Show>

              <Show when={sanitised()}>
                <p class={HELP}>
                  oto removed some characters from this template before rendering it — private-use
                  and other-format codepoints are stripped, because forging one of oto's own marks
                  is how raw markup would otherwise reach a card.
                </p>
              </Show>

              <Show
                when={data().renderings.length > 0}
                fallback={
                  <p class={HELP}>
                    Nothing rendered: the template did not compile at all, and the refusals above
                    say why.
                  </p>
                }
              >
                <ul class="flex flex-col gap-sm">
                  <For each={data().renderings}>
                    {(rendering) => <FixtureCard rendering={rendering} />}
                  </For>
                </ul>
              </Show>
            </div>
          )}
        </Match>
      </Switch>
    </fieldset>
  );
};

/**
 * One fixture, spelled by every Dialect that answered — side by side.
 *
 * ⛔ THE DIALECT LIST IS THE RESPONSE'S, NEVER THIS FILE'S. ADR 0048's owed work
 * is a Dialect registry so a provider shipped without one refuses to construct;
 * until it exists, a Dialect added server-side and forgotten is one this pane
 * would not show — and it would be a shorter list rather than a broken one, which
 * is the failure mode that hides. Rendering whatever came back keeps this screen
 * out of that conversation entirely.
 *
 * ⛔ BOTH COLUMNS ARE MONOSPACED, and that is the teaching. The difference
 * between the two is punctuation — `*critical*` against `critical` — and
 * proportional type is where punctuation goes to hide.
 */
const FixtureCard: Component<{ readonly rendering: WordingRendering }> = (props) => (
  <li class="flex flex-col gap-xs rounded-control border border-line bg-surface px-md py-sm">
    <div class="flex flex-wrap items-center gap-sm">
      <span class="text-body font-medium text-ink">{props.rendering.fixture}</span>
      <Chip
        title={
          props.rendering.representative
            ? "An ordinary card. A template that renders empty here is refused at save time."
            : "A deliberately nasty card. Rendering empty here is expected, and degrades one stanza at delivery."
        }
      >
        {props.rendering.representative ? "ordinary card" : "hostile fixture"}
      </Chip>
    </div>

    <dl class="grid grid-cols-1 gap-sm md:grid-cols-2">
      <For each={props.rendering.spellings}>
        {(spelling) => (
          <div class="flex min-w-0 flex-col gap-2xs">
            <dt class={cn(SECTION_LABEL, "text-ink-subtle")}>{spelling.dialect}</dt>
            <dd
              class={cn(
                "min-w-0 whitespace-pre-wrap break-words rounded-chip bg-sunken px-sm py-xs",
                "font-mono text-meta leading-snug",
                spelling.error ? "text-ink-muted" : "text-ink",
              )}
            >
              <Show when={spelling.text !== ""} fallback={<em>nothing</em>}>
                {spelling.text}
              </Show>
            </dd>
            <Show when={spelling.error}>
              {(message) => (
                <p class="text-meta leading-snug text-ink-muted">
                  {message()} — at delivery this stanza would fall back to oto's own words rather
                  than killing the card.
                </p>
              )}
            </Show>
          </div>
        )}
      </For>
    </dl>
  </li>
);
