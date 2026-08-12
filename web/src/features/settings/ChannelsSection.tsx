/**
 * Channels — the configured destinations.
 *
 * The whole form below is generated from the provider's `config_schema`, served
 * verbatim by `GET /api/v1/channel-types`. Those are the same bytes the server
 * validates against, so there is exactly one copy of the rules and a new
 * provider needs no UI code.
 *
 * Credentials are write-only everywhere in this API: no endpoint ever returns
 * one. So the credential control only ever *sets* a value, and an existing
 * channel shows the credential's **kind and rotation date** rather than
 * pretending to show a masked secret it does not have.
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
  createChannel,
  deleteChannel,
  testChannel,
  updateChannel,
} from "~/api/endpoints";
import { VerbositySchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { channelTypesQuery, channelsQuery } from "~/api/queries";
import type {
  Channel,
  ChannelHealthStatus,
  ChannelType,
  ChannelTypeDescriptor,
  Verbosity,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
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
  cx,
} from "~/components/ui/primitives";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";
import { SchemaForm } from "./SchemaForm";
import {
  cleanConfig,
  initialConfig,
  readFields,
  validateConfig,
  type JsonValue,
} from "./jsonSchema";

const HEALTH_NOTE: Record<ChannelHealthStatus, string> = {
  healthy: "Delivering.",
  degraded: "Delivering, but some attempts are failing.",
  auth_failed:
    "The credential was rejected. oto stops retrying rather than hammering the provider — nothing is being delivered here.",
  config_invalid: "The provider rejected the configuration. Nothing is being delivered here.",
  unknown: "Not checked yet.",
};

/** What each verbosity level actually carries, in the product's own words. */
const VERBOSITY_NOTE: Record<Verbosity, string> = {
  all: "Every fact, including comments and enrichment arrivals.",
  status_changes: "Lifecycle changes and acknowledgements, but not commentary.",
  firing_and_resolved: "Only the start and the end.",
  firing_only: "Only the start. Nothing about it ending.",
};

export const ChannelsSection: Component = () => {
  const [editing, setEditing] = createSignal<Channel | null>(null);
  const [creating, setCreating] = createSignal(false);

  const types = useQuery(() => channelTypesQuery());
  const channels = useQuery(() => channelsQuery());

  return (
    <div class="flex flex-col gap-4">
      <Panel>
        <PanelHeader>
          <PanelTitle>Channels</PanelTitle>
          <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
            Add a channel
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={channels.isPending}>
            <LoadingLine />
          </Match>
          <Match when={channels.isError}>
            <ErrorState error={channels.error} onRetry={() => void channels.refetch()} />
          </Match>
          <Match when={(channels.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No channels configured."
              body="Without a channel oto records everything and tells nobody. That is a valid way to run it, but it is worth doing on purpose rather than by omission."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={channels.data?.data ?? []}>
                {(c) => <ChannelRow channel={c} onEdit={() => setEditing(c)} />}
              </For>
            </ul>
          </Match>
        </Switch>
      </Panel>

      <Show when={(types.data?.length ?? 0) > 0}>
        <Panel>
          <PanelHeader>
            <PanelTitle>Available providers</PanelTitle>
          </PanelHeader>
          <ul class="p-3">
            <For each={types.data ?? []}>
              {(t) => (
                <li class="flex flex-wrap items-center gap-1.5 py-1">
                  <span class="text-[13px] font-medium text-ink">{t.display_name}</span>
                  <For each={t.capabilities}>{(cap) => <Chip>{cap}</Chip>}</For>
                </li>
              )}
            </For>
          </ul>
        </Panel>
      </Show>

      <ChannelDialog
        open={creating() || editing() !== null}
        channel={editing()}
        types={types.data ?? []}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
      />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const ChannelRow: Component<{ readonly channel: Channel; readonly onEdit: () => void }> = (
  props,
) => {
  const client = useQueryClient();
  const c = (): Channel => props.channel;

  const test = useMutation(() => ({ mutationFn: () => testChannel(c().id) }));
  const remove = useMutation(() => ({
    mutationFn: () => deleteChannel(c().id),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.channels() }),
  }));

  return (
    <li class="border-b border-line px-3 py-2.5 last:border-b-0">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-[13px] font-medium text-ink">{c().name}</span>
        <Chip>{c().type}</Chip>
        <span
          class={cx(
            "rounded-[3px] border px-1.5 text-[11px] leading-5",
            c().health_status === "healthy"
              ? "border-line bg-surface text-ink-muted"
              : "border-line-strong bg-raised font-medium text-ink",
          )}
          title={HEALTH_NOTE[c().health_status]}
        >
          {c().health_status}
        </span>
        <Show when={!c().enabled}>
          <Chip title="Disabled channels are skipped, and the skip is recorded with a reason.">
            disabled
          </Chip>
        </Show>
        <Chip title={VERBOSITY_NOTE[c().verbosity]}>{c().verbosity}</Chip>

        <div class="ml-auto flex items-center gap-2">
          <Button size="sm" busy={test.isPending} onClick={() => test.mutate()}>
            Send a test card
          </Button>
          <Button size="sm" onClick={props.onEdit}>
            Edit
          </Button>
          <Button
            size="sm"
            variant="danger"
            busy={remove.isPending}
            onClick={() => remove.mutate()}
            title="Removes the destination. Delivery history is kept — the record is not rewritten."
          >
            Remove
          </Button>
        </div>
      </div>

      {/* Credentials are write-only: oto shows the kind and the rotation date,
          never a masked value it would have to invent. */}
      <div class="mt-1 flex flex-wrap items-center gap-x-3 text-[11px] text-ink-subtle">
        <Show when={c().credential_kind}>
          {(kind) => <span>credential: {kind()}</span>}
        </Show>
        <Show when={c().credential_rotated_at}>
          {(at) => (
            <span>
              rotated <RelativeTime value={at()} label="Credential rotated" /> ago
            </span>
          )}
        </Show>
        <Show when={c().health_checked_at}>
          {(at) => (
            <span>
              checked <RelativeTime value={at()} label="Health checked" /> ago
            </span>
          )}
        </Show>
      </div>

      <Show when={c().health_error}>
        {(err) => (
          <p class="mt-1 border-l-2 border-line-strong pl-2 text-[11px] leading-snug text-ink">
            {err()}
          </p>
        )}
      </Show>

      <Show when={test.data}>
        {(result) => (
          <p
            class={cx(
              "mt-1 rounded-[4px] border px-2 py-1 text-[11px] leading-snug",
              result().ok
                ? "border-line bg-sunken text-ink-muted"
                : "border-line-strong bg-raised font-medium text-ink",
            )}
          >
            {result().ok
              ? "Sent. The card went through the same renderer and the same outbound validator a real notification uses, so the destination and the payload are both good. It does not prove an alert would ever be routed here — for that, run a delivery drill from the source on the Sources panel."
              : `Failed: ${result().error ?? "no detail given"}${result().error_class ? ` (${result().error_class})` : ""}`}
          </p>
        )}
      </Show>

      <Show when={test.error !== null || remove.error !== null}>
        <ErrorBanner error={test.error ?? remove.error} class="mt-1" />
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

/**
 * Every verbosity the contract publishes, READ from its own enum.
 *
 * ⛔ IT WAS FOUR LITERALS, AND THIS IS THE THIRD PLACE THE SAME ENUM LIVED —
 * `tuningCopy.ts` holds the labelled version and the generated schema holds the
 * truth. A copy cannot fail: the day `Verbosity` grows a member, a hand-written
 * list simply stops offering it, and no screen looks wrong.
 */
const VERBOSITIES: readonly Verbosity[] = VerbositySchema.options;

const ChannelDialog: Component<{
  readonly open: boolean;
  readonly channel: Channel | null;
  readonly types: readonly ChannelTypeDescriptor[];
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.channel !== null;

  const [type, setType] = createSignal<ChannelType>("slack");
  const [name, setName] = createSignal("");
  const [verbosity, setVerbosity] = createSignal<Verbosity>("status_changes");
  const [enabled, setEnabled] = createSignal(true);
  const [config, setConfig] = createSignal<Record<string, JsonValue>>({});
  const [secret, setSecret] = createSignal("");
  const [showErrors, setShowErrors] = createSignal(false);
  const [dirty, setDirty] = createSignal(false);

  const descriptor = createMemo<ChannelTypeDescriptor | undefined>(() =>
    props.types.find((t) => t.type === (props.channel?.type ?? type())),
  );

  const fields = createMemo(() => readFields(descriptor()?.config_schema));

  // Seed once per *opening*. The dialog element stays mounted, so this has to
  // be an effect keyed on `open` — reading `props.open` in the component body
  // would only ever see the closed state it mounted in.
  const seed = (): void => {
    const channel = props.channel;
    if (channel !== null) {
      setType(channel.type);
      setName(channel.name);
      setVerbosity(channel.verbosity);
      setEnabled(channel.enabled);
      setConfig(channel.config as Record<string, JsonValue>);
    } else {
      setName("");
      setVerbosity("status_changes");
      setEnabled(true);
      setConfig(initialConfig(fields()));
    }
    setSecret("");
    setShowErrors(false);
  };

  createEffect(() => {
    if (props.open && !dirty()) {
      setDirty(true);
      seed();
    } else if (!props.open && dirty()) {
      setDirty(false);
    }
  });

  const localErrors = createMemo(() => validateConfig(fields(), config()));

  const mutation = useMutation(() => ({
    mutationFn: () => {
      const body = {
        name: name().trim(),
        config: cleanConfig(fields(), config()),
        verbosity: verbosity(),
        enabled: enabled(),
        ...(secret().trim() !== "" && descriptor() !== undefined
          ? {
              credential: {
                kind: descriptor()?.credential_kinds[0] ?? ("none" as const),
                values: { token: secret().trim() },
              },
            }
          : {}),
      };
      const channel = props.channel;
      return channel !== null
        ? updateChannel(channel.id, body)
        : createChannel(
            { ...body, type: type(), thread_updates: true, show_field_emoji: true },
            idempotencyKey(),
          );
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.channels() });
      props.onClose();
    },
  }));

  /**
   * Violations arrive as JSON Pointers (`config/conversation_id`) and are
   * normalised to dotted paths by the client, so they land on the exact control
   * the schema names. This is the whole reason the form is schema-driven.
   */
  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

  return (
    <Dialog
      open={props.open}
      onClose={() => {
        setDirty(false);
        props.onClose();
      }}
      width="md"
      title={editing() ? `Edit ${props.channel?.name ?? "channel"}` : "Add a channel"}
      description="The fields below are generated from this provider's own JSON Schema — the same bytes the server validates against, so what the form accepts and what the server accepts cannot drift."
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            busy={mutation.isPending}
            onClick={() => {
              setShowErrors(true);
              if (localErrors().size > 0 || name().trim() === "") return;
              mutation.mutate();
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

        <Show when={!editing()}>
          <Field id="ch-type" label="Provider" required>
            {(a) => (
              <Select
                {...a}
                value={type()}
                onChange={(e) => {
                  setType(e.currentTarget.value as ChannelType);
                  queueMicrotask(() => setConfig(initialConfig(fields())));
                }}
              >
                <For each={props.types}>
                  {(t) => <option value={t.type}>{t.display_name}</option>}
                </For>
              </Select>
            )}
          </Field>
        </Show>

        <Field
          id="ch-name"
          label="Name"
          required
          hint="Unique within the org, compared case-insensitively."
          error={
            violations().get("name") ??
            (showErrors() && name().trim() === "" ? "A name is required." : undefined)
          }
        >
          {(a) => (
            <Input {...a} value={name()} placeholder="#sre-alerts" onInput={(e) => setName(e.currentTarget.value)} />
          )}
        </Field>

        <Show
          when={descriptor()}
          fallback={
            <p class="text-[12px] text-ink-muted">
              oto has not published a schema for this provider, so there is nothing to configure.
            </p>
          }
        >
          <fieldset class="rounded-[4px] border border-line p-3">
            <legend class="px-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-muted">
              Provider configuration
            </legend>
            <SchemaForm
              fields={fields()}
              value={config()}
              prefix="config"
              showErrors={showErrors()}
              violations={violations()}
              onChange={(key, next) => setConfig({ ...config(), [key]: next })}
            />
          </fieldset>
        </Show>

        <Show when={(descriptor()?.credential_kinds ?? []).some((k) => k !== "none")}>
          <Field
            id="ch-secret"
            label={editing() ? "Replace credential (optional)" : "Credential"}
            hint={
              editing()
                ? "Leave blank to keep the current one. oto can never show you the existing value — only a hash is kept."
                : `This provider accepts: ${(descriptor()?.credential_kinds ?? []).join(", ")}. It is sealed before it touches disk and no endpoint ever returns it.`
            }
            error={violations().get("credential.values.token")}
          >
            {(a) => (
              <Input
                {...a}
                type="password"
                autocomplete="off"
                value={secret()}
                onInput={(e) => setSecret(e.currentTarget.value)}
              />
            )}
          </Field>
        </Show>

        <Field
          id="ch-verbosity"
          label="Verbosity"
          hint={VERBOSITY_NOTE[verbosity()]}
          error={violations().get("verbosity")}
        >
          {(a) => (
            <Select
              {...a}
              value={verbosity()}
              onChange={(e) => setVerbosity(e.currentTarget.value as Verbosity)}
            >
              <For each={VERBOSITIES}>{(v) => <option value={v}>{v}</option>}</For>
            </Select>
          )}
        </Field>

        <Checkbox
          checked={enabled()}
          onChange={setEnabled}
          label={
            <span>
              Enabled
              <span class="ml-1.5 text-[11px] text-ink-subtle">
                a disabled channel is skipped, and the skip is recorded with a reason rather than
                dropped
              </span>
            </span>
          }
        />
      </DialogBody>
    </Dialog>
  );
};
