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
  updateChannelConnection,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import { channelConnectionsQuery, channelTypesQuery } from "~/api/queries";
import type { ChannelConnection, ChannelType, ChannelTypeDescriptor } from "~/api/types";

/** A credential kind, as the descriptor lists it. */
type CredentialKind = ChannelTypeDescriptor["connection_credential_kinds"][number];

/** The one kind the signing slot holds (migration 00088). */
const SIGNING_KIND = "webhook_signing_secret" as const;
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

      <Show when={remove.error !== null}>
        <ErrorBanner error={remove.error} />
      </Show>
    </li>
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
      };
      const connection = props.connection;
      return connection !== null
        ? updateChannelConnection(connection.id, body)
        : createChannelConnection({ ...body, type: type() }, idempotencyKey());
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.channelConnections() });
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

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
