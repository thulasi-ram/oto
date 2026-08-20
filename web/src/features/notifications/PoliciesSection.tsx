/**
 * Notification policies — matchers → channels → reasons.
 *
 * ⭐ THIS IS A DESTINATION NOW, NOT A SETTINGS SECTION (ADR 0034). It used to be
 * the third of five bands under `/settings`, filed beside access tokens and the
 * tuning knobs. Routing is not that kind of object: it is edited on the same
 * question the alert list is read on ("did anyone hear about this, and if not,
 * why not"), and it has a sibling — the activity log — that is pure operational
 * reading and would have been absurd under a gear icon. Both now live under
 * `/notifications`.
 *
 * A policy decides **whether** and **where**, never how a message is rendered.
 * That separation is why the form has no formatting controls: rendering belongs
 * to the channel's renderer, and offering it here would imply a coupling that
 * does not exist.
 *
 * The dry run is the point of the screen. "Given this alert, who is told, where,
 * and rendered how" is answerable *before* saving, against an unsaved draft,
 * using the real matcher and the real renderer — and it sends nothing. A routing
 * rule you cannot test is a routing rule you find out about during an incident.
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
import * as v from "valibot";

import { maxLengthOf, maxValueOf, minLengthOf, minValueOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import {
  createPolicy,
  deletePolicy,
  previewPolicy,
  updatePolicy,
} from "~/api/endpoints";
import {
  CreatePolicyRequestSchema,
  MatcherDTOSchema,
  NotificationReasonSchema,
  UuidSchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { channelsQuery, policiesQuery, recentAlertsQuery } from "~/api/queries";
import type {
  Alert,
  Channel,
  CreatePolicyRequest,
  Matcher,
  NotificationReason,
  NotificationSuppressedReason,
  Policy,
  PolicyPreview,
} from "~/api/types";
import { Button } from "~/components/ui/Button";
import { Checkbox } from "~/components/ui/Checkbox";
import {
  Combobox,
  ComboboxContent,
  ComboboxControl,
  ComboboxInput,
  ComboboxItem,
  ComboboxLabel,
  ComboboxTrigger,
} from "~/components/ui/Combobox";
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
import { ErrorBanner, ErrorState, LoadingLine, PageEmptyState } from "~/components/ui/states";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { cn } from "~/lib/cn";
import { idempotencyKey } from "~/lib/format";
import { formatMatchers, parseMatchers } from "~/lib/matchers";
import { MatcherInput } from "~/features/alerts/MatcherInput";

/*
 * The form rhythm is `features/settings`', and the import path is the only thing
 * left of this screen's old address. It is deliberately shared rather than
 * copied: this dialog and the channel dialog sit two clicks apart and are the
 * same object drawn twice, so a second set of gap constants here would be the
 * exact drift `rhythm.ts` was written to end.
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
 * Every fact a policy can choose to communicate — READ from the contract's own
 * enum rather than re-typed from it.
 *
 * ⛔ THIS LIST USED TO BE EIGHTEEN LITERALS. A copy can only ever be right about
 * the day it was written: a reason the server ADDS is one an operator silently
 * cannot select, and there is nothing on this screen that would look wrong. The
 * picklist below is the same object the request schema validates `reasons`
 * against, so the two cannot disagree by construction.
 */
const REASONS: readonly NotificationReason[] = NotificationReasonSchema.options;

/* -------------------------------------------------------------------------- */
/* The contract's bounds, read rather than repeated                           */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ NOT ONE OF THESE NUMBERS IS WRITTEN HERE. The priority box shipped with no
 * `min`/`max` at all while the contract says 0–10000, and the four list and text
 * limits were unenforced — so the first thing an operator learned about any of
 * them was a 422 with the dialog still full of their work. They come off
 * `CreatePolicyRequestSchema`, which is generated from `api/openapi/openapi.yaml`
 * by gate G4 and is the very schema the request is gated through below.
 */
const NAME_MAX = maxLengthOf(CreatePolicyRequestSchema, "name");
const PRIORITY_MIN = minValueOf(CreatePolicyRequestSchema, "priority");
const PRIORITY_MAX = maxValueOf(CreatePolicyRequestSchema, "priority");
const MATCHERS_MAX = maxLengthOf(CreatePolicyRequestSchema, "matchers");
const REASONS_MIN = minLengthOf(CreatePolicyRequestSchema, "reasons");
const REASONS_MAX = maxLengthOf(CreatePolicyRequestSchema, "reasons");
const CHANNELS_MIN = minLengthOf(CreatePolicyRequestSchema, "channel_ids");
const CHANNELS_MAX = maxLengthOf(CreatePolicyRequestSchema, "channel_ids");

const PRIORITY_RANGE = `oto accepts ${PRIORITY_MIN}–${PRIORITY_MAX}. Lower is evaluated first.`;

/** What the dialog holds, before it is anything the API has a name for. */
interface PolicyForm {
  readonly name: string;
  readonly priority: number;
  readonly enabled: boolean;
  readonly matchers: readonly Matcher[];
  readonly reasons: readonly NotificationReason[];
  readonly channel_ids: readonly string[];
}

function toCreatePolicyRequest(form: PolicyForm): CreatePolicyRequest {
  return {
    name: form.name.trim(),
    priority: form.priority,
    enabled: form.enabled,
    matchers: form.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
    reasons: [...form.reasons],
    channel_ids: [...form.channel_ids],
  };
}

/*
 * SPEC §L.8.1: the form schema stays hand-written — the sentences below are the
 * whole point of it — but it `v.pipe`s into the **generated**
 * `CreatePolicyRequestSchema` as its final gate. That last line is what makes
 * the difference between a form that agrees with the contract today and a form
 * that cannot construct a body the API would refuse. The draft used to be sent
 * raw.
 */
const PolicyFormSchema = v.pipe(
  v.strictObject({
    name: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, "A policy needs a name — it is how it is referred to everywhere else."),
      v.maxLength(NAME_MAX, `A name is at most ${NAME_MAX} characters.`),
    ),
    priority: v.pipe(
      v.number("Priority is a whole number."),
      v.integer("Priority is a whole number."),
      v.minValue(PRIORITY_MIN, PRIORITY_RANGE),
      v.maxValue(PRIORITY_MAX, PRIORITY_RANGE),
    ),
    enabled: v.boolean(),
    matchers: v.pipe(
      v.array(MatcherDTOSchema),
      v.maxLength(MATCHERS_MAX, `At most ${MATCHERS_MAX} matchers on one policy.`),
    ),
    reasons: v.pipe(
      v.array(NotificationReasonSchema),
      v.minLength(
        REASONS_MIN,
        "Pick at least one fact. A policy that communicates nothing is a policy that matches and then stays silent.",
      ),
      v.maxLength(REASONS_MAX, `At most ${REASONS_MAX} facts on one policy.`),
    ),
    channel_ids: v.pipe(
      v.array(UuidSchema),
      v.minLength(
        CHANNELS_MIN,
        "Pick at least one channel. A policy with nowhere to send records every notification as suppressed.",
      ),
      v.maxLength(CHANNELS_MAX, `At most ${CHANNELS_MAX} channels on one policy.`),
    ),
  }),
  // The annotation matters: `CreatePolicyRequest` marks the two defaulted keys
  // required (openapi-typescript fills a default in), while the generated
  // schema's *input* leaves them optional. Same contract, two honest readings
  // of it — so the transform is declared in the schema's own terms.
  v.transform((form): v.InferInput<typeof CreatePolicyRequestSchema> => toCreatePolicyRequest(form)),
  CreatePolicyRequestSchema, // the generated schema is the final gate
);

/**
 * The same enum as `./vocabulary`'s `REASON_LABEL`, phrased differently on
 * purpose — and the difference is not cosmetic.
 *
 * These label things a policy MAY BE TOLD ABOUT: they are read as the text of a
 * toggle ("rule changed", pressed or not). `vocabulary.ts` labels the same enum
 * as things that HAVE HAPPENED, read beside a timestamp ("the rule changed").
 * One map cannot be right in both places, and if one had to be wrong it must not
 * be this one — here the label is the entire explanation of what the control
 * does. See that module's header for the full argument.
 */
const REASON_LABEL: Record<NotificationReason, string> = {
  fired: "started firing",
  all_resolved: "all resolved",
  repeat: "repeat",
  suppressed: "suppressed upstream",
  unsuppressed: "no longer suppressed",
  expired: "expired",
  refired: "fired again",
  acked: "acknowledged",
  unacked: "acknowledgement withdrawn",
  // The only two reasons a snooze does not itself suppress (§B.8.4): a damper
  // that cannot announce itself is a silent mute.
  snoozed: "snoozed",
  unsnoozed: "snooze ended",
  enriched: "enrichment arrived",
  rule_changed: "rule changed",
  comment: "comment added",
  // ⛔ `unacked_reminder` WAS HERE AND IS GONE FROM THE ENUM (git-bug bd0fb1d,
  // migration 00067). It was oto's own timer on an unacknowledged signal — ONE
  // stage that ended at a channel, with no ladder, no rota and no notion of who
  // was next, which is why it was never called what the rest of the industry
  // calls it. The owner withdrew it: oto sends nothing unprompted. `escalation`
  // remains a banned word (§A.1) for the reason it always was.
  // There is deliberately no `severity_raised` here: `severity` is an ordinary
  // Prometheus label and is hashed into `alert_key`, so two severities of one
  // rule are two Alerts rather than one Alert changing. Nothing can observe a
  // rise, so nothing could ever write it (openapi `NotificationReason`).
  //
  // ⛔ `storm` WAS HERE, LABELLED "(retired)", AND IS NOW GONE FROM THE ENUM
  // ENTIRELY (ADR 0042, migration `00060`). Keeping a selectable-but-inert value
  // was the honest reading of ADR 0042 §5 while `notifications_reason_ck` still
  // admitted the reason and a stored policy could still carry it. 00060 narrows
  // that CHECK and `policies_reasons_ck` follows the enum down to eighteen, so
  // neither a notification nor a policy can spell it. `REASONS` is still the
  // contract's own picklist and is still not hand-filtered: the value left the
  // contract, so it left the picklist by itself.
  // ⭐ THE WINDOW FACT, AND FOR A POLICY WITH A WINDOW IT IS NOT OPTIONAL. A
  // digest is one message about a window over a namespace rather than about any
  // object: at each closed boundary a tick counts the cases that opened inside
  // the window and sends if the count clears the policy's floor (migration
  // `00058`). The server refuses a policy that sets a digest window without this
  // fact selected — its digests would be recorded as suppressed `no_policy`, once
  // per window, forever (`policies_digest_reason_ck`, `Policy.Validate`). It
  // damps nothing: a policy with a window sends the digest IN ADDITION to
  // whatever else it routes.
  digest: "window summary",
};

export const PoliciesSection: Component = () => {
  const [editing, setEditing] = createSignal<Policy | null>(null);
  const [creating, setCreating] = createSignal(false);

  const policies = useQuery(() => policiesQuery());
  const channels = useQuery(() => channelsQuery());

  const byId = createMemo(() => {
    const map = new Map<string, Channel>();
    for (const c of channels.data?.data ?? []) map.set(c.id, c);
    return map;
  });

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Notification policies</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setCreating(true)}>
            Add a policy
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={policies.isPending}>
            <LoadingLine />
          </Match>
          <Match when={policies.isError}>
            <ErrorState error={policies.error} onRetry={() => void policies.refetch()} />
          </Match>
          <Match when={(policies.data?.data.length ?? 0) === 0}>
            <PageEmptyState
              motif="kumo"
              title="No policies."
              body="With no policy, every notification is recorded as suppressed with reason `no_policy`. oto keeps a complete history and tells nobody — which is a choice worth making deliberately."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={policies.data?.data ?? []}>
                {(p) => (
                  <PolicyRow policy={p} channels={byId()} onEdit={() => setEditing(p)} />
                )}
              </For>
            </ul>
          </Match>
        </Switch>
      </Panel>

      <PolicyDialog
        open={creating() || editing() !== null}
        policy={editing()}
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

const PolicyRow: Component<{
  readonly policy: Policy;
  readonly channels: ReadonlyMap<string, Channel>;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const p = (): Policy => props.policy;

  const remove = useMutation(() => ({
    mutationFn: () => deletePolicy(p().id),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.policies() }),
  }));

  return (
    <li class={cn(ROW, "flex flex-col gap-sm", p().enabled ? "" : "opacity-60")}>
      <div class="flex min-h-8 flex-wrap items-center gap-sm">
        <span class="text-item font-medium text-ink">{p().name}</span>
        <Chip title="Lower is evaluated first.">priority {p().priority}</Chip>
        <Show when={!p().enabled}>
          <Chip>disabled</Chip>
        </Show>
        <div class="ml-auto flex items-center gap-sm">
          <Button size="sm" variant="secondary" onClick={props.onEdit}>
            Edit
          </Button>
          <Button size="sm" variant="destructive" busy={remove.isPending} onClick={() => remove.mutate()}>
            Remove
          </Button>
        </div>
      </div>

      <div class="flex flex-col gap-2xs text-meta text-ink-muted">
        <p>
          <span class="text-ink-subtle">when</span>{" "}
          <code class="font-mono text-ink">
            {p().matchers.length === 0
              ? "anything"
              : formatMatchers(
                  p().matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
                )}
          </code>
        </p>
        <p>
          <span class="text-ink-subtle">tell</span>{" "}
          {p().channel_ids.length === 0
            ? "nobody"
            : p()
                .channel_ids.map((id) => props.channels.get(id)?.name ?? "a removed channel")
                .join(", ")}
        </p>
        <p>
          <span class="text-ink-subtle">about</span>{" "}
          {p().reasons.map((r) => REASON_LABEL[r] ?? r).join(", ")}
        </p>
        <Show when={p().throttle}>
          {(t) => (
            <p title="A throttled notification is recorded as suppressed with a reason, never silently dropped.">
              <span class="text-ink-subtle">at most</span> {t().max} per{" "}
              {Math.round(t().window_seconds / 60)} minutes
            </p>
          )}
        </Show>
      </div>

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} />
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

const PolicyDialog: Component<{
  readonly open: boolean;
  readonly policy: Policy | null;
  readonly channels: readonly Channel[];
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.policy !== null;

  const [name, setName] = createSignal("");
  const [priority, setPriority] = createSignal(100);
  const [enabled, setEnabled] = createSignal(true);
  const [matcherText, setMatcherText] = createSignal("");
  const [reasons, setReasons] = createSignal<readonly NotificationReason[]>(["fired", "all_resolved"]);
  const [channelIds, setChannelIds] = createSignal<readonly string[]>([]);
  const [seeded, setSeeded] = createSignal(false);
  // Nothing complains until something has been typed: a dialog that opens
  // already shouting at an empty name is a dialog people learn to ignore.
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
    {
      const p = props.policy;
      if (p !== null) {
        setName(p.name);
        setPriority(p.priority);
        setEnabled(p.enabled);
        setMatcherText(
          formatMatchers(p.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value }))),
        );
        setReasons(p.reasons);
        setChannelIds(p.channel_ids);
      } else {
        setName("");
        setPriority(100);
        setEnabled(true);
        setMatcherText("");
        setReasons(["fired", "all_resolved"]);
        setChannelIds([]);
      }
    }
  });

  /**
   * A policy matcher is a `MatcherDTO`, and unlike the alert-list filter it
   * **does** accept `=~` and `!~` — the server evaluates those itself rather
   * than translating them to SQL. So the same parser is reused and nothing is
   * rejected here.
   */
  const matchers = createMemo<readonly Matcher[]>(() =>
    parseMatchers(matcherText()).matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
  );

  const form = (): PolicyForm => ({
    name: name(),
    priority: priority(),
    enabled: enabled(),
    matchers: matchers(),
    reasons: reasons(),
    channel_ids: channelIds(),
  });

  /**
   * One parse, through the generated request schema.
   *
   * ⛔ THIS IS THE GATE THE SCREEN DID NOT HAVE. The draft was handed straight
   * to `createPolicy`, so every bound in the contract was discovered as a 422 —
   * which is the exact shape of the bug this file is named after. The per-field
   * sentences above only decide which message a control shows; this decides
   * whether the request may leave the browser at all.
   */
  const gated = createMemo(() => v.safeParse(PolicyFormSchema, form()));

  /** The first complaint about one field, once the operator has touched anything. */
  const localError = (field: string): string | undefined => {
    if (!touched()) return undefined;
    const result = gated();
    if (result.success) return undefined;
    return result.issues.find((i) => i.path?.[0]?.key === field)?.message;
  };

  const draft = createMemo<CreatePolicyRequest>(() => toCreatePolicyRequest(form()));

  const mutation = useMutation(() => ({
    mutationFn: (body: CreatePolicyRequest) => {
      const p = props.policy;
      return p !== null ? updatePolicy(p.id, body) : createPolicy(body, idempotencyKey());
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.policies() });
      setSeeded(false);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

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
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {editing() ? `Edit ${props.policy?.name ?? "policy"}` : "Add a notification policy"}
          </ModalTitle>
          <ModalDescription>
            A policy decides whether and where a fact is communicated. It never decides how the
            message looks — that belongs to the channel's renderer.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <div class={FIELD_ROW}>
            <TextField
              class={cn(FIELD, "min-w-[12rem] flex-[2]")}
              value={name()}
              validationState={
                (localError("name") ?? violations().get("name")) ? "invalid" : "valid"
              }
              onChange={(value) => {
                setTouched(true);
                setName(value);
              }}
            >
              <TextFieldLabel>
                Name
                <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                  *
                </span>
              </TextFieldLabel>
              <TextFieldInput maxLength={NAME_MAX} placeholder="critical → #sre-alerts" />
              <TextFieldErrorMessage role="alert">
                {localError("name") ?? violations().get("name")}
              </TextFieldErrorMessage>
            </TextField>
            <TextField
              class={cn(FIELD, "w-28")}
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
              <TextFieldDescription class={HELP}>{`Lower first. ${PRIORITY_MIN}–${PRIORITY_MAX}.`}</TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {localError("priority") ?? violations().get("priority")}
              </TextFieldErrorMessage>
            </TextField>
          </div>

        <div class={FIELD}>
          <label for="pol-matchers" class={LABEL}>
            Matchers
          </label>
          <MatcherInput
            id="pol-matchers"
            value={matcherText()}
            onChange={(next) => {
              setTouched(true);
              setMatcherText(next);
            }}
            onCommit={() => undefined}
          />
          <p class={HELP}>
            All matchers must match. An empty list matches everything, and at most {MATCHERS_MAX} may
            be given. Unlike the alert-list filter, a policy accepts{" "}
            <code class="font-mono">=~</code> and <code class="font-mono">!~</code> — the server
            evaluates those itself.
          </p>
          <Show when={localError("matchers") ?? violations().get("matchers")}>
            {(msg) => (
              <p class="text-meta font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <ChannelPicker
          channels={props.channels}
          value={channelIds()}
          onChange={(next) => {
            setTouched(true);
            setChannelIds(next);
          }}
          error={localError("channel_ids") ?? violations().get("channel_ids")}
        />

        <div class={FIELD}>
          <ToggleGroup
            showLegend
            legend="About these facts"
            multiple
            value={[...reasons()]}
            onChange={(next) => {
              setTouched(true);
              setReasons(next as NotificationReason[]);
            }}
          >
            <For each={REASONS}>
              {(r) => <ToggleGroupItem value={r}>{REASON_LABEL[r]}</ToggleGroupItem>}
            </For>
          </ToggleGroup>
          <Show when={localError("reasons") ?? violations().get("reasons")}>
            {(msg) => (
              <p class="text-meta font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <div class={CHECK_ROW}>
          <Checkbox id="pol-enabled" checked={enabled()} onChange={setEnabled} />
          <label for="pol-enabled-input" class={CHECK_LABEL}>
            Enabled
          </label>
        </div>

          <PolicyPreviewPanel draft={draft()} />
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={!gated().success}
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

/* -------------------------------------------------------------------------- */
/* Where the policy sends                                                     */
/* -------------------------------------------------------------------------- */

/**
 * "Tell these channels", as a search box rather than a wall of checkboxes.
 *
 * ⭐ THE LIST IS AS LONG AS THE ORG DECIDES IT IS. A checkbox per channel is the
 * right control while there are six of them and the wrong one at forty: the
 * dialog grows a scroll region whose only navigation is the eye, and the fields
 * under it — the facts, the dry run, the Save button — get pushed below the fold
 * of a modal by data the operator does not control. A combobox is the same
 * multiple selection with a filter in front of it: what is picked stays visible
 * as chips at all times, and finding `#sre-alerts` costs three keystrokes rather
 * than a scan.
 *
 * ⛔ THE SEARCH MATCHES THE TYPE AND THE VERBOSITY TOO, not just the name.
 * `optionTextValue` is what Kobalte filters on, so building it out of the same
 * three facts the row *shows* is what makes "slack" a query that works. Filtering
 * on the name alone would render a row saying `#sre-alerts slack status_changes`
 * and then refuse to find it by two of the three words on it.
 *
 * ⛔ THE CAP IS STATED, NOT ENFORCED BY DISABLING. The checkbox list could grey
 * out the rows past `CHANNELS_MAX` because every row was on screen to be greyed;
 * a picker you have to type into cannot tell you about a row you have not found
 * yet. So the selection is accepted and the contract's own sentence — "At most N
 * channels on one policy." — appears under the control with Save disabled behind
 * it. The bound is discovered in the dialog either way, which is the property
 * that matters (a 422 after Save is what this screen is named after).
 */
const ChannelPicker: Component<{
  readonly channels: readonly Channel[];
  readonly value: readonly string[];
  readonly onChange: (next: readonly string[]) => void;
  readonly error: string | undefined;
}> = (props) => {
  /**
   * The chosen channels as the OBJECTS the picker offers, resolved out of
   * `props.channels` rather than rebuilt.
   *
   * ⛔ IDENTITY IS THE WHOLE POINT OF DOING IT THIS WAY. Kobalte resyncs a
   * controlled `value` against `options` by reference, so handing it freshly
   * constructed rows — even structurally identical ones — puts a controlled
   * multi-select into an `onChange` ⇄ recompute loop the moment something is
   * picked. The dry-run picker below carries the same warning in longer form; it
   * is the one bug this primitive reliably produces.
   */
  const selected = createMemo<Channel[]>(() => {
    const chosen = new Set(props.value);
    return props.channels.filter((c) => chosen.has(c.id));
  });

  /** A channel the operator can no longer name — deleted under an open dialog. */
  const orphans = createMemo(() => {
    const known = new Set(props.channels.map((c) => c.id));
    return props.value.filter((id) => !known.has(id));
  });

  return (
    <div class={FIELD}>
      <Show
        when={props.channels.length > 0}
        fallback={
          <>
            <span class={LABEL}>Tell these channels</span>
            <p class={HELP}>
              There are no channels yet, so this policy would have nowhere to send.
            </p>
          </>
        }
      >
        <Combobox<Channel>
          multiple
          options={[...props.channels]}
          optionValue="id"
          optionLabel="name"
          optionTextValue={(c) => `${c.name} ${c.type} ${c.verbosity}`}
          value={selected()}
          // ⛔ THE ORPHANS ARE CARRIED THROUGH. The picker can only ever hand
          // back channels it offers, so mapping its answer straight onto the
          // form would delete a destination that merely could not be named —
          // a silent edit nobody asked for, on the field that decides who hears.
          onChange={(next) => props.onChange([...next.map((c) => c.id), ...orphans()])}
          validationState={props.error === undefined ? "valid" : "invalid"}
          itemComponent={(itemProps) => (
            <ComboboxItem item={itemProps.item}>
              {itemProps.item.rawValue.name}
              <span class="ml-sm text-meta text-ink-subtle">
                {itemProps.item.rawValue.type} · {itemProps.item.rawValue.verbosity}
                {itemProps.item.rawValue.enabled ? "" : " · disabled"}
              </span>
            </ComboboxItem>
          )}
        >
          <ComboboxLabel class="block">Tell these channels</ComboboxLabel>
          <ComboboxControl<Channel>>
            {(state) => (
              <>
                {/* What is already picked, never behind anything. A policy's
                    destinations are the answer to "where does this go", and a
                    control that only showed them once opened would hide it. */}
                <For each={state.selectedOptions()}>
                  {(c) => (
                    <span class="inline-flex items-center gap-2xs rounded-chip border border-line bg-raised py-0.5 pl-1.5 pr-0.5 text-meta text-ink">
                      {c.name}
                      <button
                        type="button"
                        class="flex size-4 items-center justify-center rounded-chip text-ink-subtle hover:bg-surface hover:text-ink"
                        aria-label={`Do not tell ${c.name}`}
                        onClick={() => state.remove(c)}
                      >
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          stroke-width="2"
                          stroke-linecap="round"
                          aria-hidden="true"
                          class="size-3"
                        >
                          <path d="M18 6L6 18M6 6l12 12" />
                        </svg>
                      </button>
                    </span>
                  )}
                </For>
                {/* The placeholder is on the input, which is where the caret
                    goes; Kobalte's root-level `placeholder` is for the *value*
                    surface a `Select` has and a combobox does not. */}
                <ComboboxInput placeholder={state.selectedOptions().length > 0 ? "Add another…" : "Search channels…"} />
                <ComboboxTrigger aria-label="Show every channel" />
              </>
            )}
          </ComboboxControl>
          <ComboboxContent />
        </Combobox>
      </Show>

      {/* A channel deleted while this dialog was open is still on the policy and
          must not vanish from it silently — the save would then quietly drop a
          destination the operator never chose to drop. */}
      <Show when={orphans().length > 0}>
        <p class="text-meta leading-snug text-ink-muted">
          {orphans().length === 1
            ? "One channel on this policy no longer exists"
            : `${orphans().length} channels on this policy no longer exist`}
          . Saving keeps {orphans().length === 1 ? "it" : "them"} exactly as {orphans().length === 1 ? "it is" : "they are"}.
        </p>
      </Show>

      <Show when={props.error}>
        {(msg) => (
          <p class="text-meta font-medium text-ink" role="alert">
            {msg()}
          </p>
        )}
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* The dry run                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Every reason the dry run can give for a message not being sent.
 *
 * ⛔ TYPED AGAINST THE CONTRACT'S OWN ENUM, not `Record<string, string>`. It was
 * the loose type that let this map lose `snoozed`: the compiler had nothing to
 * check against, the lookup fell through to `?? r.suppressed_reason`, and the
 * dry run answered "Would not send — snoozed." — a wire token where a sentence
 * belongs, in the one place on the screen whose whole job is to explain a
 * silence before it happens. With the exhaustive type, the next reason the
 * server publishes is a build failure instead.
 *
 * ⛔ SEVEN VALUES, AND THE TWO THAT LEFT WERE THE ONLY TWO THAT WERE OTO'S OWN
 * OPINION ABOUT A SIGNAL. `storm` and `flapping` are gone from
 * `notifications_suppmap_ck` (migration `00059`, ADR 0042), which narrowed with
 * NO backfill and therefore FAILS on a database that ever recorded either — so
 * there is no stored row left for this map to explain. What the seven have in
 * common is the argument: two mean there was nowhere to send, two are a human
 * asking for less, two are a policy's own ceiling and floor over a window, one is
 * that nothing changed. `below_threshold` (migration `00073`) is the floor, and it
 * is admissible where `flapping` was not because its threshold is a column an
 * operator wrote rather than a constant compiled into oto.
 */
const SUPPRESSED_REASON: Record<NonNullable<NotificationSuppressedReason>, string> = {
  no_policy: "no policy matched",
  // §B.8.2 ranks a snooze above every automatic damper: it is a deliberate human
  // act, and therefore the most useful thing to say about a silence.
  snoozed: "someone is holding oto's notifications for this alert until a fixed time",
  throttled: "the throttle is already spent",
  // The same two policy fields as the throttle, read as a floor: not "too many
  // already" but "not enough yet".
  below_threshold: "the policy's count condition has not been met yet",
  verbosity: "the channel's verbosity does not carry this",
  channel_disabled: "the channel is disabled",
  duplicate_render: "the message would be identical to the last one",
};

/**
 * The sentence for one suppression, with the two honest fallbacks.
 *
 * A reason this build has never heard of renders as its raw wire value rather
 * than as a blank — gate G3 exists to make that a build failure — and a result
 * that carries no reason at all says so, because "would not send —  ." is how a
 * screen loses a fact without anybody noticing.
 */
function describeSuppression(reason: NotificationSuppressedReason | undefined): string {
  if (reason === undefined || reason === null) return "no reason was given";
  return SUPPRESSED_REASON[reason] ?? reason;
}

/**
 * An episode as the dry-run picker holds it: what to send, and what to call it.
 *
 * The label is captured with the choice rather than looked up later, because
 * the point of holding it is to survive the row leaving the list.
 *
 * ⛔ `caseId` IS WHAT GOES ON THE WIRE, AND THE ALERT ID IS NO LONGER SENT AT
 * ALL (git-bug 7570090). `PolicyPreviewRequest` took `alert_id` and `group_id`
 * and now takes `case_id` only: a Case IS the conversation, so a routing
 * question is asked about one firing episode rather than about an identity that
 * may not be firing. The LABEL still comes from the alert, because a case has no
 * name of its own — `case #19` names nothing an operator recognises, and
 * `KubePodCrashLooping · prod-eu · payments` is the whole reason this picker
 * exists instead of a UUID box.
 */
interface PickedCase {
  readonly caseId: string;
  readonly label: string;
}

/**
 * One alert as a pickable episode, or `null` when it has no open case.
 *
 * ⚠️ `null` IS THE COMMON CASE AND NOT AN ERROR. `RECENT_ALERTS` sorts by
 * `-last_seen_at` and filters nothing, so a resolved or expired alert is in the
 * list and has no `current_case` — there is no episode to dry-run against. It is
 * dropped from the options rather than offered and then rejected by the server,
 * because a picker that lets you choose something Preview cannot use is worse
 * than one that is shorter.
 */
function pickedCase(a: Alert): PickedCase | null {
  const id = a.current_case?.id;
  if (id === undefined || id === null) return null;
  return {
    caseId: id,
    label: `${a.alertname} · ${a.cluster_key}${a.namespace ? ` · ${a.namespace}` : ""}`,
  };
}

/**
 * "Who would this reach?" answered against an unsaved draft.
 *
 * The endpoint evaluates the inline policy **in addition to** the stored ones,
 * which is what makes the answer honest: it shows what would actually happen,
 * including the stored policy that would also fire, rather than what this
 * policy would do in isolation.
 */
const PolicyPreviewPanel: Component<{ readonly draft: CreatePolicyRequest }> = (props) => {
  // ⛔ THE CHOICE IS THE EPISODE, NOT ITS ID. The list is the twenty most
  // recently seen alerts and the stream reorders it, so an id held on its own can
  // stop naming any option the picker offers: `<Select>` would fall back to blank
  // while Preview stayed enabled, and the operator would dry-run a routing
  // policy against an episode the screen could no longer name. Holding what was
  // chosen means the picker can keep offering it.
  const [picked, setPicked] = createSignal<PickedCase | null>(null);
  const [reason, setReason] = createSignal<NotificationReason>("fired");

  // A short list of recent alerts to dry-run against, so nobody has to paste a
  // UUID to answer a routing question. `recentAlertsQuery` keys it under
  // `["alerts"]` so the stream reaches it, and rate-limits what it does about
  // that — the whole policy is stated there, not restated here.
  const recent = useQuery(() => recentAlertsQuery());

  /**
   * The recent twenty, as `PickedAlert`s.
   *
   * ⛔ KEPT AS ITS OWN MEMO, DEPENDENT ONLY ON `recent.data`. `options()` below
   * used to `.map(pickedAlert)` this same array inline, which reads `picked()`
   * in the same computation — so *every* selection re-ran the `.map` and handed
   * the controlled `Select` a fresh set of row objects it had never seen by
   * reference. Kobalte resyncs its controlled `value` against `options()` by
   * identity, so a picker driven through its real listbox (not the hidden
   * shim's `fireEvent.change`, which this bug was invisible to) spun into an
   * infinite `onChange` ⇄ recompute loop the instant something was actually
   * selected. Keeping `rows` stable across `picked()` changes is what breaks
   * that cycle.
   */
  // ⚠️ `.filter()` AFTER THE MAP, not a `flatMap` on the alert list: the twenty
  // rows include alerts with no open case, and those have no episode to preview.
  const rows = createMemo<readonly PickedCase[]>(() =>
    (recent.data?.data ?? []).map(pickedCase).filter((c): c is PickedCase => c !== null),
  );

  /**
   * What the picker offers: the recent twenty, plus the alert already chosen if
   * a refetch has since pushed it off the end.
   *
   * The chosen one goes last, and it is still the selected option, so nothing
   * silently changes under the operator between reading the list and pressing
   * Preview.
   */
  const options = createMemo<readonly PickedCase[]>(() => {
    const chosen = picked();
    if (chosen === null || rows().some((o) => o.caseId === chosen.caseId)) return rows();
    return [...rows(), chosen];
  });

  const preview = useMutation(() => ({
    mutationFn: (): Promise<PolicyPreview> =>
      previewPolicy({ case_id: picked()?.caseId ?? "", reason: reason(), policy: props.draft }),
  }));

  // Named, not boxed — the same change as the channel dialog's provider group.
  // The bordered box's `px-lg` put every control in this panel 17px right of the
  // policy's own fields; the panel is still obviously a separate thing because
  // its legend says so, in the same small caps `PanelTitle` uses.
  return (
    <fieldset>
      <legend class={LEGEND}>Dry run</legend>

      <p class={cn("mb-md", HELP)}>
        Runs the real policy matcher and the real renderer against a real alert, including this
        unsaved draft, and <span class="font-medium text-ink">sends nothing</span>.
      </p>

      <div class={FIELD_ROW}>
        <div class="min-w-[14rem] flex-1">
          {/* Labelled with Kobalte's own `SelectLabel` (matching
              `ChannelsSection.tsx`/`SourcesSection.tsx`), not a hand-written
              `<label for>` — a plain `for` cannot target `SelectTrigger`, since
              it renders a `<button>` and a `<button>` is not a labelable
              element (a native `<label for>` on one is left without an
              accessible name). `SelectLabel` wires `aria-labelledby` to the
              trigger itself, which is the real, accessible, interactive
              surface — not `SelectHiddenSelect`'s `aria-hidden` native shim,
              which exists only for browser autofill/native form submission and
              was never meant to be the primary interaction or testing surface.

              The picker is disabled, and says so, until `recent` actually has
              rows: `PolicyDialog` is a Kobalte `Modal`, whose content (this
              panel included) is presence-gated (`<Show when={contentPresent()}>`)
              and mounts only once the dialog opens — so `recentAlertsQuery()`
              does not even start fetching before then. Without a loading state
              an operator (or a test driving the real trigger) could open this
              picker while `options()` is still empty and "select" nothing,
              which is exactly the kind of honesty gap `FilterBar.tsx`'s own
              async-fed Cluster picker avoids by staying out of the way until
              it has something to offer. This picker cannot hide the same way —
              it is the point of the dry run — so it disables itself and says
              "Loading recent alerts…" instead. */}
          <Select<PickedCase>
            class={FIELD}
            options={[...options()]}
            optionValue="caseId"
            optionTextValue="label"
            value={picked()}
            onChange={setPicked}
            disabled={recent.isPending}
            placeholder={recent.isPending ? "Loading recent alerts…" : "— pick a recent alert —"}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue.label}</SelectItem>
            )}
          >
            {/* "episode", not "alert": the request names a case, and a label that
                said "alert" would promise the dry run covers an identity that is
                not currently firing. */}
            <SelectLabel class="block">Against this episode</SelectLabel>
            <SelectTrigger>
              <SelectValue<PickedCase>>{(state) => state.selectedOption().label}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>

        <div class="w-52">
          <Select<NotificationReason>
            class={FIELD}
            options={[...REASONS]}
            optionTextValue={(r) => REASON_LABEL[r]}
            value={reason()}
            onChange={(next) => {
              if (next !== null) setReason(next);
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{REASON_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectLabel class="block">Simulating</SelectLabel>
            <SelectTrigger>
              <SelectValue<NotificationReason>>
                {(state) => REASON_LABEL[state.selectedOption()]}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>

        {/* `self-end` earns its place here and only here: both fields on this
            line are a label over a trigger with nothing hanging below, so the
            row's bottom edge IS the control line. */}
        <Button
          class="self-end"
          busy={preview.isPending}
          disabled={picked() === null}
          onClick={() => preview.mutate()}
        >
          Preview
        </Button>
      </div>

      <Show when={preview.error !== null}>
        <ErrorBanner error={preview.error} class="mt-md" />
      </Show>

      <Show when={preview.data}>
        {(result) => (
          <div class="mt-lg">
            <Show
              when={result().matched}
              fallback={
                <p class="rounded-control border border-line-strong border-l-[3px] border-l-ink bg-surface px-md py-sm text-body font-medium leading-snug text-ink">
                  Nothing would be sent. No enabled policy — including this draft — matches this
                  alert for that fact, so it would go unreported.
                </p>
              }
            >
              <ul class="flex flex-col gap-sm">
                <For each={result().results}>
                  {(r) => (
                    <li
                      class={cn(
                        "flex flex-col gap-xs rounded-control border px-md py-sm",
                        r.would_send
                          ? "border-line bg-surface"
                          : "border-line-strong bg-sunken",
                      )}
                    >
                      <div class="flex flex-wrap items-center gap-sm text-body">
                        <span class="font-medium text-ink">{r.channel_name}</span>
                        <Chip>{r.channel_type}</Chip>
                        <Chip title="How the message would be placed in the thread.">{r.mode}</Chip>
                        <span class="ml-auto text-meta text-ink-subtle">
                          via {r.policy_name}
                        </span>
                      </div>

                      <Show
                        when={r.would_send}
                        fallback={
                          <p class="text-meta leading-snug text-ink-muted">
                            Would not send — {describeSuppression(r.suppressed_reason)}. It would
                            still be recorded with that reason.
                          </p>
                        }
                      >
                        <Show when={r.rendered_fallback}>
                          {(text) => (
                            <p class="border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink">
                              {text()}
                            </p>
                          )}
                        </Show>
                      </Show>
                    </li>
                  )}
                </For>
              </ul>
            </Show>

            <Show when={(result().warnings ?? []).length > 0}>
              <ul class="mt-md flex flex-col gap-2xs">
                <For each={result().warnings ?? []}>
                  {(w) => <li class="text-meta leading-snug text-ink-muted">{w}</li>}
                </For>
              </ul>
            </Show>
          </div>
        )}
      </Show>
    </fieldset>
  );
};
