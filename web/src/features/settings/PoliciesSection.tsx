/**
 * Notification policies — matchers → channels → reasons.
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
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import {
  Button,
  Checkbox,
  Chip,
  Field,
  Input,
  Panel,
  PanelHeader,
  PanelTitle,
  Select,
  ToggleGroup,
  cx,
} from "~/components/ui/primitives";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";
import { formatMatchers, parseMatchers } from "~/lib/matchers";
import { MatcherInput } from "~/features/alerts/MatcherInput";

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

const REASON_LABEL: Record<NotificationReason, string> = {
  fired: "started firing",
  new_alerts: "new alerts joined",
  some_resolved: "some resolved",
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
  // Note the wording: this is oto's own timer on an unacknowledged signal, and
  // it is ONE stage that ends at a channel. oto has no ladder, no rota and no
  // notion of who is next — which is why the reason is not called what the rest
  // of this industry calls it (SPEC §G.9.1, §A.1).
  unacked_reminder: "still firing and unacknowledged",
  // A fact about ONE group going quiet, and it stays on that group's thread.
  // The channel-level "oto has started withholding" notice is a separate,
  // once-per-channel decision and is not a Reason a policy selects.
  //
  // There is deliberately no `severity_raised` here: `severity` is an ordinary
  // Prometheus label and is hashed into `alert_key`, so two severities of one
  // rule are two Alerts rather than one Alert changing. Nothing can observe a
  // rise, so nothing could ever write it (openapi `NotificationReason`).
  storm: "storm mode",
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
    <div class="flex flex-col gap-4">
      <Panel>
        <PanelHeader>
          <PanelTitle>Notification policies</PanelTitle>
          <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
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
            <EmptyState
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
    <li class={cx("border-b border-line px-3 py-2.5 last:border-b-0", p().enabled ? "" : "opacity-60")}>
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-[13px] font-medium text-ink">{p().name}</span>
        <Chip title="Lower is evaluated first.">priority {p().priority}</Chip>
        <Show when={!p().enabled}>
          <Chip>disabled</Chip>
        </Show>
        <div class="ml-auto flex items-center gap-2">
          <Button size="sm" onClick={props.onEdit}>
            Edit
          </Button>
          <Button size="sm" variant="danger" busy={remove.isPending} onClick={() => remove.mutate()}>
            Remove
          </Button>
        </div>
      </div>

      <div class="mt-1 flex flex-col gap-1 text-[11px] text-ink-muted">
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
        <Show when={p().unacked_reminder_after_seconds}>
          {(secs) => (
            // vocab:allow — user-facing copy that DENIES the concept; the sentence exists to tell an operator oto will not page.
            <p title="oto's own clock on an unacknowledged signal. It broadcasts one reply in the existing thread; it does not page anyone and it does not know who is on call.">
              <span class="text-ink-subtle">if still unacknowledged after</span>{" "}
              {Math.round(secs() / 60)} minutes, say so once
            </p>
          )}
        </Show>
      </div>

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} class="mt-1" />
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
    <Dialog
      open={props.open}
      onClose={() => {
        setSeeded(false);
        props.onClose();
      }}
      width="lg"
      title={editing() ? `Edit ${props.policy?.name ?? "policy"}` : "Add a notification policy"}
      description="A policy decides whether and where a fact is communicated. It never decides how the message looks — that belongs to the channel's renderer."
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
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
        </>
      }
    >
      <DialogBody>
        <Show when={mutation.error !== null}>
          <ErrorBanner error={mutation.error} />
        </Show>

        <div class="flex flex-wrap gap-3">
          <div class="min-w-[12rem] flex-[2]">
            <Field
              id="pol-name"
              label="Name"
              required
              error={localError("name") ?? violations().get("name")}
            >
              {(a) => (
                <Input
                  {...a}
                  value={name()}
                  maxLength={NAME_MAX}
                  placeholder="critical → #sre-alerts"
                  onInput={(e) => {
                    setTouched(true);
                    setName(e.currentTarget.value);
                  }}
                />
              )}
            </Field>
          </div>
          <div class="w-28">
            <Field
              id="pol-priority"
              label="Priority"
              hint={`Lower first. ${PRIORITY_MIN}–${PRIORITY_MAX}.`}
              error={localError("priority") ?? violations().get("priority")}
            >
              {(a) => (
                <Input
                  {...a}
                  type="number"
                  min={PRIORITY_MIN}
                  max={PRIORITY_MAX}
                  step={1}
                  value={Number.isFinite(priority()) ? String(priority()) : ""}
                  onInput={(e) => {
                    setTouched(true);
                    // An unreadable box becomes `NaN` rather than the last good
                    // number: silently keeping the previous value would save
                    // something the operator can no longer see.
                    setPriority(Number.parseInt(e.currentTarget.value, 10));
                  }}
                />
              )}
            </Field>
          </div>
        </div>

        <div>
          <label for="pol-matchers" class="mb-1 block text-[12px] font-medium text-ink-muted">
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
          <p class="mt-1 text-[11px] leading-snug text-ink-subtle">
            All matchers must match. An empty list matches everything, and at most {MATCHERS_MAX} may
            be given. Unlike the alert-list filter, a policy accepts{" "}
            <code class="font-mono">=~</code> and <code class="font-mono">!~</code> — the server
            evaluates those itself.
          </p>
          <Show when={localError("matchers") ?? violations().get("matchers")}>
            {(msg) => (
              <p class="mt-1 text-[11px] font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <fieldset>
          <legend class="mb-1 text-[12px] font-medium text-ink-muted">Tell these channels</legend>
          <Show
            when={props.channels.length > 0}
            fallback={
              <p class="text-[12px] text-ink-muted">
                There are no channels yet, so this policy would have nowhere to send.
              </p>
            }
          >
            <div class="flex flex-col gap-1">
              <For each={props.channels}>
                {(c) => (
                  <Checkbox
                    checked={channelIds().includes(c.id)}
                    // The cap is the contract's, and a box that cannot be ticked
                    // says so before the save does. Already-ticked boxes stay
                    // clickable, or the cap would be a trap you cannot back out of.
                    disabled={!channelIds().includes(c.id) && channelIds().length >= CHANNELS_MAX}
                    onChange={(next) => {
                      setTouched(true);
                      setChannelIds(
                        next ? [...channelIds(), c.id] : channelIds().filter((id) => id !== c.id),
                      );
                    }}
                    label={
                      <span>
                        {c.name}
                        <span class="ml-1.5 text-[11px] text-ink-subtle">
                          {c.type} · {c.verbosity}
                          {c.enabled ? "" : " · disabled"}
                        </span>
                      </span>
                    }
                  />
                )}
              </For>
            </div>
          </Show>
          <Show when={localError("channel_ids") ?? violations().get("channel_ids")}>
            {(msg) => (
              <p class="mt-1 text-[11px] font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </fieldset>

        <div>
          <ToggleGroup<NotificationReason>
            showLegend
            legend="About these facts"
            options={REASONS.map((r) => ({ value: r, label: REASON_LABEL[r] }))}
            selected={reasons()}
            onChange={(next) => {
              setTouched(true);
              setReasons(next);
            }}
          />
          <Show when={localError("reasons") ?? violations().get("reasons")}>
            {(msg) => (
              <p class="mt-1 text-[11px] font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <Checkbox checked={enabled()} onChange={setEnabled} label={<span>Enabled</span>} />

        <PolicyPreviewPanel draft={draft()} />
      </DialogBody>
    </Dialog>
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
 */
const SUPPRESSED_REASON: Record<NonNullable<NotificationSuppressedReason>, string> = {
  no_policy: "no policy matched",
  // §B.8.2 ranks a snooze above every automatic damper: it is a deliberate human
  // act, and therefore the most useful thing to say about a silence.
  snoozed: "someone is holding oto's notifications for this alert until a fixed time",
  throttled: "the throttle is already spent",
  storm: "the group is in storm mode",
  flapping: "this alert is damped as flapping",
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
 * An alert as the dry-run picker holds it: what to send, and what to call it.
 *
 * The label is captured with the choice rather than looked up later, because
 * the point of holding it is to survive the row leaving the list.
 */
interface PickedAlert {
  readonly id: string;
  readonly label: string;
}

function pickedAlert(a: Alert): PickedAlert {
  return {
    id: a.id,
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
  // ⛔ THE CHOICE IS THE ALERT, NOT ITS ID. The list is the twenty most recently
  // seen alerts and the stream reorders it, so an id held on its own can stop
  // naming any option the picker offers: `<Select>` would fall back to blank
  // while Preview stayed enabled, and the operator would dry-run a routing
  // policy against an alert the screen could no longer name. Holding what was
  // chosen means the picker can keep offering it.
  const [picked, setPicked] = createSignal<PickedAlert | null>(null);
  const [reason, setReason] = createSignal<NotificationReason>("fired");

  // A short list of recent alerts to dry-run against, so nobody has to paste a
  // UUID to answer a routing question. `recentAlertsQuery` keys it under
  // `["alerts"]` so the stream reaches it, and rate-limits what it does about
  // that — the whole policy is stated there, not restated here.
  const recent = useQuery(() => recentAlertsQuery());

  /**
   * What the picker offers: the recent twenty, plus the alert already chosen if
   * a refetch has since pushed it off the end.
   *
   * The chosen one goes last, and it is still the selected option, so nothing
   * silently changes under the operator between reading the list and pressing
   * Preview.
   */
  const options = createMemo<readonly PickedAlert[]>(() => {
    const rows = (recent.data?.data ?? []).map(pickedAlert);
    const chosen = picked();
    if (chosen === null || rows.some((o) => o.id === chosen.id)) return rows;
    return [...rows, chosen];
  });

  const preview = useMutation(() => ({
    mutationFn: (): Promise<PolicyPreview> =>
      previewPolicy({ alert_id: picked()?.id ?? "", reason: reason(), policy: props.draft }),
  }));

  return (
    <fieldset class="rounded-[4px] border border-line bg-raised p-3">
      <legend class="px-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted">
        Dry run
      </legend>

      <p class="mb-2 text-[11px] leading-snug text-ink-muted">
        Runs the real policy matcher and the real renderer against a real alert, including this
        unsaved draft, and <span class="font-medium text-ink">sends nothing</span>.
      </p>

      <div class="flex flex-wrap items-end gap-2">
        <div class="min-w-[14rem] flex-1">
          <label for="preview-alert" class="mb-1 block text-[12px] font-medium text-ink-muted">
            Against this alert
          </label>
          <Select
            id="preview-alert"
            value={picked()?.id ?? ""}
            onChange={(e) => {
              const id = e.currentTarget.value;
              setPicked(options().find((o) => o.id === id) ?? null);
            }}
          >
            <option value="">— pick a recent alert —</option>
            <For each={options()}>{(o) => <option value={o.id}>{o.label}</option>}</For>
          </Select>
        </div>

        <div class="w-52">
          <label for="preview-reason" class="mb-1 block text-[12px] font-medium text-ink-muted">
            Simulating
          </label>
          <Select
            id="preview-reason"
            value={reason()}
            onChange={(e) => setReason(e.currentTarget.value as NotificationReason)}
          >
            <For each={REASONS}>{(r) => <option value={r}>{REASON_LABEL[r]}</option>}</For>
          </Select>
        </div>

        <Button
          size="md"
          busy={preview.isPending}
          disabled={picked() === null}
          onClick={() => preview.mutate()}
        >
          Preview
        </Button>
      </div>

      <Show when={preview.error !== null}>
        <ErrorBanner error={preview.error} class="mt-2" />
      </Show>

      <Show when={preview.data}>
        {(result) => (
          <div class="mt-3">
            <Show
              when={result().matched}
              fallback={
                <p class="rounded-[4px] border border-line-strong border-l-[3px] border-l-ink bg-surface px-2 py-1.5 text-[12px] font-medium leading-snug text-ink">
                  Nothing would be sent. No enabled policy — including this draft — matches this
                  alert for that fact, so it would go unreported.
                </p>
              }
            >
              <ul class="space-y-1.5">
                <For each={result().results}>
                  {(r) => (
                    <li
                      class={cx(
                        "rounded-[4px] border px-2 py-1.5",
                        r.would_send
                          ? "border-line bg-surface"
                          : "border-line-strong bg-sunken",
                      )}
                    >
                      <div class="flex flex-wrap items-center gap-2 text-[12px]">
                        <span class="font-medium text-ink">{r.channel_name}</span>
                        <Chip>{r.channel_type}</Chip>
                        <Chip title="How the message would be placed in the thread.">{r.mode}</Chip>
                        <span class="ml-auto text-[11px] text-ink-subtle">
                          via {r.policy_name}
                        </span>
                      </div>

                      <Show
                        when={r.would_send}
                        fallback={
                          <p class="mt-1 text-[11px] leading-snug text-ink-muted">
                            Would not send — {describeSuppression(r.suppressed_reason)}. It would
                            still be recorded with that reason.
                          </p>
                        }
                      >
                        <Show when={r.rendered_fallback}>
                          {(text) => (
                            <p class="mt-1 border-l-2 border-line-strong pl-2 text-[11px] leading-snug text-ink">
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
              <ul class="mt-2 space-y-0.5">
                <For each={result().warnings ?? []}>
                  {(w) => <li class="text-[11px] leading-snug text-ink-muted">{w}</li>}
                </For>
              </ul>
            </Show>
          </div>
        )}
      </Show>
    </fieldset>
  );
};
