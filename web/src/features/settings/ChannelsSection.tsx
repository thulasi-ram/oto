/**
 * Connections — the org-wide provider setup.
 *
 * ⭐ THIS USED TO BE WHERE INDIVIDUAL CHANNELS WERE CREATED, AND IT NO LONGER
 * IS. A connection is a Slack workspace's bot token, or a webhook receiver
 * family's shared credential — set up ONCE, here, by an admin. A specific
 * destination (`#sre-alerts`, a specific URL) is a Channel, and a Channel is
 * created from the Notification Policy screen, where an operator is already
 * naming a destination for a routing rule (`PoliciesSection.tsx`'s
 * `ChannelPicker`). Requiring an admin-Settings round trip for every new
 * `#channel` was the cost that split removes.
 *
 * The whole form below is generated from the provider's `connection_schema`,
 * served verbatim by `GET /api/v1/channel-types`. Those are the same bytes the
 * server validates against, so there is exactly one copy of the rules and a new
 * provider needs no UI code.
 *
 * Credentials are write-only everywhere in this API: no endpoint ever returns
 * one. So the credential control only ever *sets* a value, and an existing
 * connection shows the credential's **kind and rotation date** rather than
 * pretending to show a masked secret it does not have.
 *
 * ⭐ A WEBHOOK CONNECTION MAY CARRY A PAYLOAD MAPPING (ADR 0055 §2), and it is set
 * up HERE, with the connection, because it is destination setup — never on the
 * templates screen, which is wording. The server renders it against an envelope
 * for every fact before it saves it, so its refusals arrive as per-fact 422
 * violations and are listed under the editor. Its secrets are write-only like
 * every other credential: only their names come back.
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
  createChannelConnection,
  deleteChannelConnection,
  testChannelConnectionMapping,
  updateChannelConnection,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import { channelConnectionsQuery, channelsQuery, channelTypesQuery } from "~/api/queries";
import type {
  ChannelConnection,
  ChannelType,
  ChannelTypeDescriptor,
  NotificationReason,
  PayloadMapping,
} from "~/api/types";
import { REASON_LABEL } from "~/features/notifications/vocabulary";

/** A credential kind, as the descriptor lists it. */
type CredentialKind = ChannelTypeDescriptor["connection_credential_kinds"][number];

/** The one kind the signing slot holds (migration 00088). */
const SIGNING_KIND = "webhook_signing_secret" as const;

/** Every fact a payload mapping renders, in the contract's order. */
const FACTS = Object.keys(REASON_LABEL) as NotificationReason[];

/**
 * Reads the mapping editor: `undefined` for "unchanged", `null` for "remove it",
 * the document otherwise — or a sentence saying why it is not a document.
 */
function readMapping(
  text: string,
  initial: string,
): { mapping?: PayloadMapping | null; error?: string } {
  if (text.trim() === initial.trim()) return {};
  if (text.trim() === "") return { mapping: null };
  try {
    const parsed: unknown = JSON.parse(text);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { error: "A mapping is one JSON object with at least a `body`." };
    }
    return { mapping: parsed as PayloadMapping };
  } catch {
    return { error: "This is not valid JSON." };
  }
}

/** Reads `name=value` lines; `undefined` when blank, which keeps the current secrets. */
function readSecrets(text: string): { secrets?: Record<string, string>; error?: string } {
  if (text.trim() === "") return {};
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    const at = line.indexOf("=");
    if (at <= 0) return { error: "Write one secret per line, as name=value." };
    out[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return { secrets: out };
}
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
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
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { cn } from "~/lib/cn";
import { idempotencyKey } from "~/lib/format";
import { SchemaForm } from "./SchemaForm";
import {
  cleanConfig,
  initialConfig,
  readFields,
  validateConfig,
  type JsonValue,
} from "./jsonSchema";
import { FIELD, FORM, HELP, LEGEND, PANEL_BODY, PANEL_HEADER, ROW, SECTION } from "./rhythm";

export const ChannelsSection: Component = () => {
  const [editing, setEditing] = createSignal<ChannelConnection | null>(null);
  const [creating, setCreating] = createSignal(false);

  const types = useQuery(() => channelTypesQuery());
  const connections = useQuery(() => channelConnectionsQuery());

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Connections</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setCreating(true)}>
            Add a connection
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={connections.isPending}>
            <LoadingLine />
          </Match>
          <Match when={connections.isError}>
            <ErrorState error={connections.error} onRetry={() => void connections.refetch()} />
          </Match>
          <Match when={(connections.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No connections configured."
              body="A connection is a Slack workspace's bot token, or a webhook receiver's shared credential, set up once. Without one, no channel of that type can be created from the notification policy screen."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={connections.data?.data ?? []}>
                {(c) => <ConnectionRow connection={c} onEdit={() => setEditing(c)} />}
              </For>
            </ul>
          </Match>
        </Switch>
      </Panel>

      <Show when={(types.data?.length ?? 0) > 0}>
        <Panel>
          <PanelHeader class={PANEL_HEADER}>
            <PanelTitle>Available providers</PanelTitle>
          </PanelHeader>
          <ul class={cn(PANEL_BODY, "flex flex-col gap-sm")}>
            <For each={types.data ?? []}>
              {(t) => (
                <li class="flex min-h-6 flex-wrap items-center gap-sm">
                  <span class="text-item font-medium text-ink">{t.display_name}</span>
                  <For each={t.capabilities}>{(cap) => <Chip>{cap}</Chip>}</For>
                </li>
              )}
            </For>
          </ul>
        </Panel>
      </Show>

      <ConnectionDialog
        open={creating() || editing() !== null}
        connection={editing()}
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

const ConnectionRow: Component<{
  readonly connection: ChannelConnection;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const c = (): ChannelConnection => props.connection;

  // Only a FUTURE overlap end is worth a line: once it has passed, the previous
  // secret no longer signs and saying when it stopped tells nobody anything.
  const overlapUntil = (): string | null => {
    const until = c().signing_overlap_until;
    return until !== null && until !== undefined && Date.parse(until) > Date.now() ? until : null;
  };

  const remove = useMutation(() => ({
    mutationFn: () => deleteChannelConnection(c().id),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.channelConnections() }),
  }));

  return (
    <li class={cn(ROW, "flex flex-col gap-sm")}>
      <div class="flex min-h-8 flex-wrap items-center gap-sm">
        <span class="text-item font-medium text-ink">{c().name}</span>
        <Chip>{c().type}</Chip>

        <div class="ml-auto flex items-center gap-sm">
          <Button size="sm" variant="secondary" onClick={props.onEdit}>
            Edit
          </Button>
          <Button
            size="sm"
            variant="destructive"
            busy={remove.isPending}
            onClick={() => remove.mutate()}
            title="A connection still open by a channel cannot be removed."
          >
            Remove
          </Button>
        </div>
      </div>

      {/* Credentials are write-only: oto shows the kind and the rotation date,
          never a masked value it would have to invent. */}
      <div class="flex flex-wrap items-center gap-x-lg text-meta text-ink-subtle">
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
        <Show when={c().signing_credential_kind}>
          <span>signs requests (X-Oto-Signature)</span>
        </Show>
        <Show when={c().signing_credential_rotated_at}>
          {(at) => (
            <span>
              signing secret rotated <RelativeTime value={at()} label="Signing secret rotated" /> ago
            </span>
          )}
        </Show>
        {/* The overlap is the one fact about rotation whoever runs the receiver has to act on:
            until it ends, their old copy of the secret still verifies. */}
        <Show when={overlapUntil()}>
          {(until) => (
            <span>
              previous signing secret still signs for{" "}
              <RelativeTime value={until()} label="Previous signing secret retires" />
            </span>
          )}
        </Show>
      </div>

      <Show when={c().payload_mapping}>
        <MappingTest connection={c()} />
      </Show>

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} />
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

/**
 * The payload mapping's test send (ADR 0055 §2): one fact, chosen here, through
 * one of this connection's channels — the connection holds the mapping and the
 * channel holds the URL. It goes the whole real way, so it says plainly that the
 * tool at the other end may open a real incident.
 */
const MappingTest: Component<{ readonly connection: ChannelConnection }> = (props) => {
  const channels = useQuery(() => channelsQuery());
  const mine = createMemo(() =>
    (channels.data?.data ?? []).filter((ch) => ch.connection_id === props.connection.id),
  );
  const nameOf = (id: string): string => mine().find((ch) => ch.id === id)?.name ?? id;
  const [channelId, setChannelId] = createSignal<string | null>(null);
  const [fact, setFact] = createSignal<NotificationReason>("drawn");
  const chosen = (): string | undefined => channelId() ?? mine()[0]?.id;

  const test = useMutation(() => ({
    mutationFn: () =>
      testChannelConnectionMapping(
        props.connection.id,
        { channel_id: chosen() ?? "", fact: fact() },
        idempotencyKey(),
      ),
  }));

  return (
    <div class="flex flex-col gap-sm">
      <div class="flex flex-wrap items-end gap-sm">
        <Select<string>
          class={FIELD}
          options={mine().map((ch) => ch.id)}
          value={chosen() ?? null}
          onChange={(next) => {
            if (next !== null) setChannelId(next);
          }}
          itemComponent={(itemProps) => (
            <SelectItem item={itemProps.item}>{nameOf(itemProps.item.rawValue)}</SelectItem>
          )}
        >
          <SelectLabel>Send through</SelectLabel>
          <SelectTrigger id={`mapping-test-channel-${props.connection.id}`}>
            <SelectValue<string>>{(state) => nameOf(state.selectedOption())}</SelectValue>
          </SelectTrigger>
          <SelectHiddenSelect />
          <SelectContent />
        </Select>
        <Select<NotificationReason>
          class={FIELD}
          options={FACTS}
          value={fact()}
          onChange={(next) => {
            if (next !== null) setFact(next);
          }}
          itemComponent={(itemProps) => (
            <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
          )}
        >
          <SelectLabel>Fact</SelectLabel>
          <SelectTrigger id={`mapping-test-fact-${props.connection.id}`}>
            <SelectValue<NotificationReason>>{(state) => state.selectedOption()}</SelectValue>
          </SelectTrigger>
          <SelectHiddenSelect />
          <SelectContent />
        </Select>
        <Button
          size="sm"
          variant="secondary"
          busy={test.isPending}
          disabled={chosen() === undefined}
          onClick={() => test.mutate()}
        >
          Test the mapping
        </Button>
      </div>
      <p class={HELP}>
        Sends the chosen fact through the mapping, its secrets and this channel, exactly as a real
        delivery would. <strong>The tool at the other end may open a real incident.</strong>
        <Show when={mine().length === 0}> This connection has no channel to send through yet.</Show>
      </p>
      <Show when={test.data}>
        {(res) => (
          <p class={cn(HELP, res().ok ? "text-ink" : "text-error-foreground")} role="status">
            {res().ok ? "Sent, and the receiver accepted it." : (res().error ?? "The test failed.")}
          </p>
        )}
      </Show>
      <Show when={test.error !== null}>
        <ErrorBanner error={test.error} />
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const ConnectionDialog: Component<{
  readonly open: boolean;
  readonly connection: ChannelConnection | null;
  readonly types: readonly ChannelTypeDescriptor[];
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.connection !== null;

  const [type, setType] = createSignal<ChannelType>("slack");
  const [name, setName] = createSignal("");
  const [config, setConfig] = createSignal<Record<string, JsonValue>>({});
  const [secret, setSecret] = createSignal("");
  const [username, setUsername] = createSignal("");
  const [authKind, setAuthKind] = createSignal<CredentialKind | null>(null);
  const [signingSecret, setSigningSecret] = createSignal("");
  const [mappingText, setMappingText] = createSignal("");
  const [initialMapping, setInitialMapping] = createSignal("");
  const [secretsText, setSecretsText] = createSignal("");
  const [showErrors, setShowErrors] = createSignal(false);
  const [dirty, setDirty] = createSignal(false);

  const descriptor = createMemo<ChannelTypeDescriptor | undefined>(() =>
    props.types.find((t) => t.type === (props.connection?.type ?? type())),
  );

  const fields = createMemo(() => readFields(descriptor()?.connection_config_schema));

  // ⭐ TWO SLOTS, NOT ONE (migration 00088). The kinds a connection accepts are split
  // by WHERE they go: `webhook_signing_secret` is the signing slot, every other
  // non-`none` kind authenticates. This used to send `connection_credential_kinds[0]`
  // for every connection — which is `none` for a webhook — so no webhook credential
  // or signing secret could be saved from this form at all.
  const authKinds = createMemo<readonly CredentialKind[]>(() =>
    (descriptor()?.connection_credential_kinds ?? []).filter(
      (k) => k !== "none" && k !== SIGNING_KIND,
    ),
  );
  const signs = createMemo(() =>
    (descriptor()?.connection_credential_kinds ?? []).includes(SIGNING_KIND),
  );
  const chosenAuthKind = (): CredentialKind | undefined => authKind() ?? authKinds()[0];
  // Only a webhook connection carries a payload mapping (ADR 0055 §2).
  const isWebhook = (): boolean => (props.connection?.type ?? type()) === "webhook";
  const mappingInput = createMemo(() => readMapping(mappingText(), initialMapping()));
  const secretsInput = createMemo(() => readSecrets(secretsText()));
  const sealedNames = (): readonly string[] => props.connection?.mapping_secret_names ?? [];

  // A basic credential is two values; every other kind this form offers is one.
  const credential = (): { kind: CredentialKind; values: Record<string, string> } | undefined => {
    const kind = chosenAuthKind();
    if (kind === undefined || secret().trim() === "") return undefined;
    return kind === "basic"
      ? { kind, values: { username: username().trim(), password: secret() } }
      : { kind, values: { token: secret().trim() } };
  };

  // Seed once per *opening*, the same reasoning ChannelDialog used: the dialog
  // element stays mounted, so this has to be an effect keyed on `open`.
  const seed = (): void => {
    const connection = props.connection;
    if (connection !== null) {
      setType(connection.type);
      setName(connection.name);
      setConfig(connection.config as Record<string, JsonValue>);
    } else {
      setName("");
      setConfig(initialConfig(fields()));
    }
    setSecret("");
    setUsername("");
    setAuthKind(null);
    setSigningSecret("");
    const mapping = connection?.payload_mapping;
    const text = mapping ? JSON.stringify(mapping, null, 2) : "";
    setMappingText(text);
    setInitialMapping(text);
    setSecretsText("");
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
      const cred = credential();
      const mapping = isWebhook() ? mappingInput().mapping : undefined;
      const secrets = isWebhook() ? secretsInput().secrets : undefined;
      const body = {
        name: name().trim(),
        config: cleanConfig(fields(), config()),
        ...(cred !== undefined ? { credential: cred } : {}),
        ...(signs() && signingSecret().trim() !== ""
          ? {
              signing_credential: {
                kind: SIGNING_KIND,
                values: { secret: signingSecret().trim() },
              },
            }
          : {}),
        ...(secrets !== undefined ? { mapping_secrets: secrets } : {}),
      };
      const connection = props.connection;
      return connection !== null
        ? updateChannelConnection(connection.id, {
            ...body,
            ...(mapping !== undefined ? { payload_mapping: mapping } : {}),
          })
        : createChannelConnection(
            { ...body, ...(mapping ? { payload_mapping: mapping } : {}), type: type() },
            idempotencyKey(),
          );
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.channelConnections() });
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);
  // A mapping is refused per fact, so its refusals are listed rather than pinned to
  // one control: "drawn (fixture …): the mapping did not render one JSON object".
  const mappingViolations = (): readonly string[] =>
    [...violations()]
      .filter(
        ([field]) => field.startsWith("payload_mapping") || field.startsWith("mapping_secrets"),
      )
      .map(([field, message]) => `${field}: ${message}`);

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          setDirty(false);
          props.onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {editing() ? `Edit ${props.connection?.name ?? "connection"}` : "Add a connection"}
          </ModalTitle>
          <ModalDescription>
            An org-wide provider setup — a Slack workspace's bot token, or a webhook receiver's
            shared credential. Individual channels reference this by name from the notification
            policy screen; nothing about one destination is configured here.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <Show when={!editing()}>
            <Select<ChannelType>
              class={FIELD}
              options={props.types.map((t) => t.type)}
              value={type()}
              onChange={(next) => {
                if (next === null) return;
                setType(next);
                queueMicrotask(() => setConfig(initialConfig(fields())));
              }}
              itemComponent={(itemProps) => (
                <SelectItem item={itemProps.item}>
                  {props.types.find((t) => t.type === itemProps.item.rawValue)?.display_name ??
                    itemProps.item.rawValue}
                </SelectItem>
              )}
            >
              <SelectLabel>
                Provider
                <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                  *
                </span>
              </SelectLabel>
              <SelectTrigger id="conn-type">
                <SelectValue<ChannelType>>
                  {(state) =>
                    props.types.find((t) => t.type === state.selectedOption())?.display_name ??
                    state.selectedOption()
                  }
                </SelectValue>
              </SelectTrigger>
              <SelectHiddenSelect />
              <SelectContent />
            </Select>
          </Show>

          <TextField
            class={FIELD}
            value={name()}
            validationState={
              (violations().get("name") ??
              (showErrors() && name().trim() === "" ? "A name is required." : undefined))
                ? "invalid"
                : "valid"
            }
            onChange={setName}
          >
            <TextFieldLabel>
              Name
              <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                *
              </span>
            </TextFieldLabel>
            <TextFieldInput id="conn-name" placeholder="Acme Slack workspace" />
            <TextFieldDescription class={HELP}>
              Unique within the org, compared case-insensitively.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {violations().get("name") ??
                (showErrors() && name().trim() === "" ? "A name is required." : undefined)}
            </TextFieldErrorMessage>
          </TextField>

          <Show
            when={descriptor() !== undefined && fields().length > 0}
          >
            <fieldset>
              <legend class={LEGEND}>Provider configuration</legend>
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

          <Show when={authKinds().length > 1}>
            <Select<CredentialKind>
              class={FIELD}
              options={[...authKinds()]}
              value={chosenAuthKind() ?? null}
              onChange={(next) => {
                if (next !== null) setAuthKind(next);
              }}
              itemComponent={(itemProps) => (
                <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
              )}
            >
              <SelectLabel>Authentication</SelectLabel>
              <SelectTrigger id="conn-auth-kind">
                <SelectValue<CredentialKind>>{(state) => state.selectedOption()}</SelectValue>
              </SelectTrigger>
              <SelectHiddenSelect />
              <SelectContent />
            </Select>
          </Show>

          <Show when={chosenAuthKind() === "basic"}>
            <TextField class={FIELD} value={username()} onChange={setUsername}>
              <TextFieldLabel>Username</TextFieldLabel>
              <TextFieldInput id="conn-username" autocomplete="off" />
            </TextField>
          </Show>

          <Show when={authKinds().length > 0}>
            <TextField
              class={FIELD}
              value={secret()}
              validationState={violations().get("credential.values.token") ? "invalid" : "valid"}
              onChange={setSecret}
            >
              <TextFieldLabel>
                {editing() ? "Replace credential (optional)" : "Credential"}
              </TextFieldLabel>
              <TextFieldInput id="conn-secret" type="password" autocomplete="off" />
              <TextFieldDescription class={HELP}>
                {editing()
                  ? "Leave blank to keep the current one. oto can never show you the existing value — only a hash is kept."
                  : `This provider accepts: ${authKinds().join(", ")}. It is sealed before it touches disk and no endpoint ever returns it.`}
              </TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {violations().get("credential.values.token") ?? violations().get("credential.kind")}
              </TextFieldErrorMessage>
            </TextField>
          </Show>

          <Show when={signs()}>
            <TextField
              class={FIELD}
              value={signingSecret()}
              validationState={violations().get("signing_credential") ? "invalid" : "valid"}
              onChange={setSigningSecret}
            >
              <TextFieldLabel>
                {editing() && props.connection?.signing_credential_kind
                  ? "Rotate signing secret (optional)"
                  : "Signing secret (optional)"}
              </TextFieldLabel>
              <TextFieldInput id="conn-signing-secret" type="password" autocomplete="off" />
              <TextFieldDescription class={HELP}>
                {editing() && props.connection?.signing_credential_kind
                  ? "Leave blank to keep the current one. A new secret signs at once, and the current one keeps signing beside it for 24 hours, so receivers can switch without rejecting a delivery."
                  : "oto signs every request with it (X-Oto-Signature), beside any credential above, so the receiver can verify the request came from oto."}
              </TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {violations().get("signing_credential") ??
                  violations().get("signing_credential.kind")}
              </TextFieldErrorMessage>
            </TextField>
          </Show>

          <Show when={isWebhook()}>
            <fieldset class="flex flex-col gap-sm">
              <legend class={LEGEND}>Payload mapping</legend>
              <TextField
                class={FIELD}
                validationState={
                  mappingInput().error !== undefined || mappingViolations().length > 0
                    ? "invalid"
                    : "valid"
                }
              >
                <TextFieldLabel>Mapping (optional)</TextFieldLabel>
                <TextFieldTextArea
                  id="conn-payload-mapping"
                  class="min-h-48 font-mono text-meta"
                  spellcheck={false}
                  value={mappingText()}
                  placeholder={'{\n  "body": "{ \\"title\\": \\"{{ summary }}\\" }"\n}'}
                  onInput={(e) => setMappingText(e.currentTarget.value)}
                />
                <TextFieldDescription class={HELP}>
                  Turns oto's envelope into the request an incident tool expects: a{" "}
                  <code>body</code> for every fact, optional per-fact <code>facts</code>,{" "}
                  <code>headers</code>, and a <code>response</code> path to the tool's incident
                  link. Every value is JSON-escaped for you. It is checked against every fact before
                  it is saved, and a mapping that fails when sending fails the delivery — the plain
                  envelope is never sent instead. Leave blank to send the plain envelope.
                </TextFieldDescription>
                <TextFieldErrorMessage role="alert">{mappingInput().error}</TextFieldErrorMessage>
              </TextField>

              <TextField
                class={FIELD}
                validationState={secretsInput().error !== undefined ? "invalid" : "valid"}
              >
                <TextFieldLabel>
                  {sealedNames().length > 0
                    ? "Replace mapping secrets (optional)"
                    : "Mapping secrets (optional)"}
                </TextFieldLabel>
                <TextFieldTextArea
                  id="conn-mapping-secrets"
                  class="min-h-16 font-mono text-meta"
                  spellcheck={false}
                  autocomplete="off"
                  value={secretsText()}
                  placeholder="routing_key=…"
                  onInput={(e) => setSecretsText(e.currentTarget.value)}
                />
                <TextFieldDescription class={HELP}>
                  One <code>name=value</code> per line, referenced in the mapping as{" "}
                  <code>{"{{ secrets.name }}"}</code> and filled in only as the request is sent — a
                  mapping never holds a secret.
                  <Show when={sealedNames().length > 0}>
                    {" "}
                    Sealed now: {sealedNames().join(", ")}. Leave blank to keep them; anything
                    written here replaces the whole set.
                  </Show>
                </TextFieldDescription>
                <TextFieldErrorMessage role="alert">{secretsInput().error}</TextFieldErrorMessage>
              </TextField>

              <Show when={mappingViolations().length > 0}>
                <ul class={cn(HELP, "text-error-foreground")} role="alert">
                  <For each={mappingViolations()}>{(v) => <li>{v}</li>}</For>
                </ul>
              </Show>
            </fieldset>
          </Show>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            onClick={() => {
              setShowErrors(true);
              if (localErrors().size > 0 || name().trim() === "") return;
              const unreadable = mappingInput().error ?? secretsInput().error;
              if (isWebhook() && unreadable !== undefined) return;
              mutation.mutate();
            }}
          >
            {editing() ? "Save" : "Create"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
