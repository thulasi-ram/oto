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

import { violationsByField } from "~/api/client";
import {
  createPolicy,
  deletePolicy,
  listAlerts,
  listChannels,
  listPolicies,
  previewPolicy,
  updatePolicy,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type {
  Channel,
  CreatePolicyRequest,
  Matcher,
  NotificationReason,
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

/** Every fact a policy can choose to communicate. */
const REASONS: readonly NotificationReason[] = [
  "fired",
  "new_alerts",
  "some_resolved",
  "all_resolved",
  "repeat",
  "suppressed",
  "unsuppressed",
  "expired",
  "refired",
  "acked",
  "unacked",
  "enriched",
  "rule_changed",
  "comment",
  "escalation",
  "storm",
];

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
  enriched: "enrichment arrived",
  rule_changed: "rule changed",
  comment: "comment added",
  // Note the wording: this is oto's own timer on an unacknowledged signal, not
  // a human escalation chain. oto has no on-call and no notion of who is next.
  escalation: "still firing and unacknowledged",
  storm: "storm mode",
};

export const PoliciesSection: Component = () => {
  const [editing, setEditing] = createSignal<Policy | null>(null);
  const [creating, setCreating] = createSignal(false);

  const policies = useQuery(() => ({
    queryKey: qk.settings.policies(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listPolicies({ signal }),
  }));

  const channels = useQuery(() => ({
    queryKey: qk.settings.channels(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listChannels({ signal }),
  }));

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
        <Show when={p().escalate_after_seconds}>
          {(secs) => (
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

  // Seed once per *opening*. The dialog stays mounted, so this must be an
  // effect: the component body runs only in the state it mounted in.
  createEffect(() => {
    if (!props.open) {
      if (seeded()) setSeeded(false);
      return;
    }
    if (seeded()) return;
    setSeeded(true);
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

  const draft = createMemo<CreatePolicyRequest>(() => ({
    name: name().trim(),
    priority: priority(),
    enabled: enabled(),
    matchers: [...matchers()],
    reasons: [...reasons()],
    channel_ids: [...channelIds()],
  }));

  const mutation = useMutation(() => ({
    mutationFn: () => {
      const p = props.policy;
      return p !== null
        ? updatePolicy(p.id, draft())
        : createPolicy(draft(), idempotencyKey());
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
            disabled={name().trim() === "" || channelIds().length === 0 || reasons().length === 0}
            onClick={() => mutation.mutate()}
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
            <Field id="pol-name" label="Name" required error={violations().get("name")}>
              {(a) => (
                <Input
                  {...a}
                  value={name()}
                  placeholder="critical → #sre-alerts"
                  onInput={(e) => setName(e.currentTarget.value)}
                />
              )}
            </Field>
          </div>
          <div class="w-28">
            <Field
              id="pol-priority"
              label="Priority"
              hint="Lower first."
              error={violations().get("priority")}
            >
              {(a) => (
                <Input
                  {...a}
                  type="number"
                  value={String(priority())}
                  onInput={(e) => {
                    const n = Number.parseInt(e.currentTarget.value, 10);
                    if (Number.isFinite(n)) setPriority(n);
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
            onChange={setMatcherText}
            onCommit={() => undefined}
          />
          <p class="mt-1 text-[11px] leading-snug text-ink-subtle">
            All matchers must match. An empty list matches everything. Unlike the alert-list filter,
            a policy accepts <code class="font-mono">=~</code> and{" "}
            <code class="font-mono">!~</code> — the server evaluates those itself.
          </p>
          <Show when={violations().get("matchers")}>
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
                    onChange={(next) =>
                      setChannelIds(
                        next ? [...channelIds(), c.id] : channelIds().filter((id) => id !== c.id),
                      )
                    }
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
          <Show when={violations().get("channel_ids")}>
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
            onChange={setReasons}
          />
          <Show when={violations().get("reasons")}>
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

const SUPPRESSED_REASON: Record<string, string> = {
  no_policy: "no policy matched",
  throttled: "the throttle is already spent",
  storm: "the group is in storm mode",
  flapping: "this alert is damped as flapping",
  verbosity: "the channel's verbosity does not carry this",
  channel_disabled: "the channel is disabled",
  duplicate_render: "the message would be identical to the last one",
};

/**
 * "Who would this reach?" answered against an unsaved draft.
 *
 * The endpoint evaluates the inline policy **in addition to** the stored ones,
 * which is what makes the answer honest: it shows what would actually happen,
 * including the stored policy that would also fire, rather than what this
 * policy would do in isolation.
 */
const PolicyPreviewPanel: Component<{ readonly draft: CreatePolicyRequest }> = (props) => {
  const [alertId, setAlertId] = createSignal("");
  const [reason, setReason] = createSignal<NotificationReason>("fired");

  // A short list of recent alerts to dry-run against, so nobody has to paste a
  // UUID to answer a routing question.
  const recent = useQuery(() => ({
    queryKey: ["policy-preview", "recent-alerts"] as const,
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlerts({ limit: 20, sort: "-last_seen_at" }, {}, { signal }),
    staleTime: 60_000,
  }));

  const preview = useMutation(() => ({
    mutationFn: (): Promise<PolicyPreview> =>
      previewPolicy({ alert_id: alertId(), reason: reason(), policy: props.draft }),
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
            value={alertId()}
            onChange={(e) => setAlertId(e.currentTarget.value)}
          >
            <option value="">— pick a recent alert —</option>
            <For each={recent.data?.data ?? []}>
              {(a) => (
                <option value={a.id}>
                  {a.alertname} · {a.cluster_key}
                  {a.namespace ? ` · ${a.namespace}` : ""}
                </option>
              )}
            </For>
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
          disabled={alertId() === ""}
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
                            Would not send —{" "}
                            {SUPPRESSED_REASON[r.suppressed_reason ?? ""] ?? r.suppressed_reason}.
                            It would still be recorded with that reason.
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
