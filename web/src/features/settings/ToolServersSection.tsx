/**
 * Tool servers — the MCP servers an Investigator reads through, and the one a Remedy writes
 * through (ADR 0053 §3, ADR 0054).
 *
 * # `access` IS THE TRUST BOUNDARY, AND THE OPERATOR DECLARES IT
 *
 * An Investigator holds only the Tools of a `read` ToolServer; a `write` one is never offered to a
 * model, and only an approved Remedy's single execution calls its Tools. The contract gives `access`
 * NO DEFAULT, so neither does this form: the field starts empty and Create stays disabled until a
 * choice is made. A preselected "read" would be oto's guess at what somebody else's server can do,
 * and a wrong guess puts a write Tool in a model's hands. The dialog says why a server that does
 * both must be declared `write`.
 *
 * # What this screen does not do
 *
 * ⛔ It grants nobody the right to approve a Remedy. That grant is given from the host shell only
 * (`oto grant remedy-approver`), because a route that let one holder mint a second approver would
 * defeat double approval (ADR 0054 §4). There is no edit or delete route for a ToolServer either, so
 * there is no button for one; a wrong URL or token is a new ToolServer.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf, rangeOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import { createToolServer, discoverToolServer } from "~/api/endpoints";
import { CreateToolServerRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { toolServerToolsQuery, toolServersQuery } from "~/api/queries";
import type { ToolServer, ToolServerTool } from "~/api/types";
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
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";

import { FIELD, FORM, HELP, PANEL_BODY, PANEL_HEADER, ROW, SECTION } from "./rhythm";

/** ⛔ Read off the generated schema, never typed here — `TokensSection`'s rule. */
const NAME_MAX = maxLengthOf(CreateToolServerRequestSchema, "name");
const URL_MAX = maxLengthOf(CreateToolServerRequestSchema, "url");
const TOKEN_MAX = maxLengthOf(CreateToolServerRequestSchema, "token");
const TIMEOUT = rangeOf(CreateToolServerRequestSchema, "call_timeout_seconds");
const RESULT = rangeOf(CreateToolServerRequestSchema, "max_result_bytes");

type Access = ToolServer["access"];
type Transport = ToolServer["transport"];

const ACCESS: readonly Access[] = ["read", "write"];
const TRANSPORTS: readonly Transport[] = ["streamable_http", "sse"];

const ACCESS_HELP: Readonly<Record<Access, string>> = {
  read: "Every Tool it serves only reads. An Investigator may hold these.",
  write:
    "It serves at least one Tool that changes something. No Investigator ever holds its Tools; only an approved Remedy calls them.",
};

export const ToolServersSection: Component = () => {
  const [adding, setAdding] = createSignal(false);
  const servers = useQuery(toolServersQuery);

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Tool servers</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setAdding(true)}>
            Add a tool server
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={servers.isPending}>
            <LoadingLine />
          </Match>
          <Match when={servers.isError}>
            <ErrorState error={servers.error} onRetry={() => void servers.refetch()} />
          </Match>
          <Match when={(servers.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No tool server is configured."
              body="oto ships none. An Investigator reads oto's own history without one; add an MCP server (Kubernetes, VictoriaMetrics…) to let it look at your systems."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={servers.data?.data ?? []}>{(s) => <ServerRow server={s} />}</For>
            </ul>
          </Match>
        </Switch>

        <p class={cn(PANEL_BODY, HELP, "border-t border-line")}>
          oto runs no ToolServer; each is an MCP server you run, reached over HTTP. <strong>Access
          is what you declare</strong>, and oto cannot verify it: give a read server a
          ServiceAccount that can only read. What a Tool returns is redacted by name, which cannot
          see a secret that has none. A ToolServer cannot be edited or removed, so a wrong URL or
          token is replaced by adding another. Who may approve a Remedy is granted from the host
          shell (<code>oto grant remedy-approver</code>), never here.
        </p>
      </Panel>

      <AddDialog open={adding()} onClose={() => setAdding(false)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const ServerRow: Component<{ readonly server: ToolServer }> = (props) => {
  const client = useQueryClient();
  const s = (): ToolServer => props.server;
  const [open, setOpen] = createSignal(false);

  const discover = useMutation(() => ({
    mutationFn: () => discoverToolServer(s().id),
    // A failed discovery is recorded on the server (`discovery_error`), so the row re-reads
    // either way: the operator should see what the server now says, not what it said before.
    onSettled: () => {
      void client.invalidateQueries({ queryKey: qk.settings.toolServers() });
      void client.invalidateQueries({ queryKey: qk.settings.toolServerTools(s().id) });
    },
  }));

  return (
    <li class={cn(ROW, "flex flex-col gap-sm")}>
      <div class="flex min-h-12 flex-wrap items-center gap-sm">
        <span class="text-item font-medium text-ink">{s().name}</span>
        <Chip
          title={
            s().access === "write"
              ? "Declared write: no Investigator holds its Tools."
              : "Declared read: an Investigator may hold its Tools."
          }
        >
          {s().access}
        </Chip>
        <Chip mono title="The MCP transport.">
          {s().transport}
        </Chip>
        <Chip title={s().has_token ? "A token is stored, sealed, and never returned." : "No token is stored."}>
          {s().has_token ? "token set" : "no token"}
        </Chip>
        <span class="min-w-0 truncate font-mono text-meta text-ink-subtle" title={s().url}>
          {s().url}
        </span>
        <div class="ml-auto flex items-center gap-sm">
          <Button size="sm" variant="secondary" onClick={() => setOpen(!open())}>
            {open() ? "Hide tools" : "Tools"}
          </Button>
          <Button size="sm" variant="secondary" busy={discover.isPending} onClick={() => discover.mutate()}>
            Discover
          </Button>
        </div>
      </div>

      <p class="text-meta text-ink-subtle">
        <Switch>
          <Match when={s().discovery_failed_at}>
            {(at) => (
              <>
                Discovery failed <RelativeTime value={at()} label="Failed" /> ago
                <Show when={s().discovery_error}>: {s().discovery_error}</Show>
                <Show when={s().discovered_at}>
                  {(ok) => (
                    <>
                      {" "}
                      (last succeeded <RelativeTime value={ok()} label="Discovered" /> ago)
                    </>
                  )}
                </Show>
              </>
            )}
          </Match>
          <Match when={s().discovered_at}>
            {(at) => (
              <>
                Tools discovered <RelativeTime value={at()} label="Discovered" /> ago. Discover
                again after the server changes.
              </>
            )}
          </Match>
          <Match when={true}>Never discovered — its Tools are not known yet. Run Discover.</Match>
        </Switch>
      </p>

      <Show when={discover.error !== null}>
        <ErrorBanner error={discover.error} />
      </Show>
      <Show when={open()}>
        <ToolList serverId={s().id} access={s().access} />
      </Show>
    </li>
  );
};

/** One ToolServer's Tools at its last discovery, fetched only while the row is open. */
const ToolList: Component<{ readonly serverId: string; readonly access: Access }> = (props) => {
  const tools = useQuery(() => toolServerToolsQuery(props.serverId));
  return (
    <Switch>
      <Match when={tools.isPending}>
        <LoadingLine />
      </Match>
      <Match when={tools.isError}>
        <ErrorState error={tools.error} onRetry={() => void tools.refetch()} />
      </Match>
      <Match when={(tools.data?.data.length ?? 0) === 0}>
        <p class={HELP}>No Tools listed. Run Discover.</p>
      </Match>
      <Match when={true}>
        <ul class="flex flex-col gap-xs">
          <For each={tools.data?.data ?? []}>{(t) => <ToolLine tool={t} access={props.access} />}</For>
        </ul>
      </Match>
    </Switch>
  );
};

const ToolLine: Component<{ readonly tool: ToolServerTool; readonly access: Access }> = (props) => (
  <li class="flex flex-col gap-0.5 text-meta">
    <div class="flex flex-wrap items-center gap-sm">
      <span class="font-mono text-ink">{props.tool.qualified_name ?? props.tool.name}</span>
      <Show when={props.access === "read" && !props.tool.usable}>
        <Chip title={props.tool.unusable_reason ?? "An Investigator cannot hold this Tool."}>
          not usable
        </Chip>
      </Show>
      <Show when={props.tool.read_only_hint === false}>
        <Chip title="The server itself says this Tool is not read-only.">not read-only</Chip>
      </Show>
    </div>
    <Show when={props.tool.description !== ""}>
      <span class="text-ink-subtle">{props.tool.description}</span>
    </Show>
    <Show when={!props.tool.usable && props.tool.unusable_reason}>
      <span class="text-ink-subtle">{props.tool.unusable_reason}</span>
    </Show>
  </li>
);

/* -------------------------------------------------------------------------- */

const AddDialog: Component<{ readonly open: boolean; readonly onClose: () => void }> = (props) => {
  const client = useQueryClient();
  const [name, setName] = createSignal("");
  const [url, setUrl] = createSignal("");
  const [transport, setTransport] = createSignal<Transport>("streamable_http");
  // ⛔ No default: the contract gives `access` none, and neither does this form.
  const [access, setAccess] = createSignal<Access | null>(null);
  const [token, setToken] = createSignal("");
  const [timeout, setTimeoutSeconds] = createSignal("");
  const [maxBytes, setMaxBytes] = createSignal("");

  const reset = (): void => {
    setName("");
    setUrl("");
    setTransport("streamable_http");
    setAccess(null);
    // ⛔ The secret leaves the page's state the moment the dialog closes.
    setToken("");
    setTimeoutSeconds("");
    setMaxBytes("");
  };

  const close = (): void => {
    reset();
    create.reset();
    props.onClose();
  };

  const create = useMutation(() => ({
    mutationFn: () => {
      const chosen = access();
      if (chosen === null) throw new Error("access must be chosen");
      const t = token();
      return createToolServer({
        name: name().trim(),
        url: url().trim(),
        transport: transport(),
        access: chosen,
        ...(t === "" ? {} : { token: t }),
        ...(timeout().trim() === "" ? {} : { call_timeout_seconds: Number(timeout()) }),
        ...(maxBytes().trim() === "" ? {} : { max_result_bytes: Number(maxBytes()) }),
      });
    },
    onSuccess: () => {
      close();
      void client.invalidateQueries({ queryKey: qk.settings.toolServers() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(create.error);
  const ready = (): boolean => name().trim() !== "" && url().trim() !== "" && access() !== null;

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) close();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Add a tool server</ModalTitle>
          <ModalDescription>
            An MCP server you run. Adding it lists nothing: run Discover on it afterwards.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={create.error !== null}>
            <ErrorBanner error={create.error} />
          </Show>

          <TextField
            class={FIELD}
            value={name()}
            required
            validationState={violations().get("name") ? "invalid" : "valid"}
            onChange={setName}
          >
            <TextFieldLabel>Name</TextFieldLabel>
            <TextFieldInput id="toolserver-name" maxLength={NAME_MAX} placeholder="k8s" />
            <TextFieldDescription class={HELP}>
              Lower-case letters, digits and inner hyphens, starting with a letter. It prefixes every
              Tool, as <code>k8s__pods_list</code>.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">{violations().get("name")}</TextFieldErrorMessage>
          </TextField>

          <TextField
            class={FIELD}
            value={url()}
            required
            validationState={violations().get("url") ? "invalid" : "valid"}
            onChange={setUrl}
          >
            <TextFieldLabel>URL</TextFieldLabel>
            <TextFieldInput
              id="toolserver-url"
              type="url"
              maxLength={URL_MAX}
              placeholder="https://mcp-k8s.internal.example/mcp"
            />
            <TextFieldDescription class={HELP}>
              No credentials or query in it. A token is only sent over https.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">{violations().get("url")}</TextFieldErrorMessage>
          </TextField>

          <Select<Access>
            class={FIELD}
            options={[...ACCESS]}
            value={access()}
            onChange={setAccess}
            placeholder="Choose…"
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
            )}
          >
            <SelectLabel>Access</SelectLabel>
            <SelectTrigger id="toolserver-access">
              <SelectValue<Access>>{(state) => state.selectedOption()}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
          <p class={HELP}>
            <Show
              when={access()}
              fallback="You declare this and oto cannot check it, so there is no default. A server that serves both reads and writes must be declared write."
            >
              {(a) => ACCESS_HELP[a()]}
            </Show>
          </p>

          <Select<Transport>
            class={FIELD}
            options={[...TRANSPORTS]}
            value={transport()}
            onChange={(next) => {
              if (next !== null) setTransport(next);
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
            )}
          >
            <SelectLabel>Transport</SelectLabel>
            <SelectTrigger id="toolserver-transport">
              <SelectValue<Transport>>{(state) => state.selectedOption()}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>

          <TextField
            class={FIELD}
            value={token()}
            validationState={violations().get("token") ? "invalid" : "valid"}
            onChange={setToken}
          >
            <TextFieldLabel>Access token</TextFieldLabel>
            <TextFieldInput
              id="toolserver-token"
              type="password"
              autocomplete="off"
              maxLength={TOKEN_MAX}
            />
            <TextFieldDescription class={HELP}>
              Optional. Sealed when saved, never shown again, and sent only to this server's own
              origin.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">{violations().get("token")}</TextFieldErrorMessage>
          </TextField>

          <div class="flex flex-wrap items-start gap-sm">
            <TextField
              class={cn(FIELD, "w-40")}
              value={timeout()}
              validationState={violations().get("call_timeout_seconds") ? "invalid" : "valid"}
              onChange={setTimeoutSeconds}
            >
              <TextFieldLabel>Call timeout (s)</TextFieldLabel>
              <TextFieldInput
                id="toolserver-timeout"
                type="number"
                min={TIMEOUT.min}
                max={TIMEOUT.max}
                step={1}
                placeholder="15"
              />
              <TextFieldErrorMessage role="alert">
                {violations().get("call_timeout_seconds")}
              </TextFieldErrorMessage>
            </TextField>
            <TextField
              class={cn(FIELD, "w-40")}
              value={maxBytes()}
              validationState={violations().get("max_result_bytes") ? "invalid" : "valid"}
              onChange={setMaxBytes}
            >
              <TextFieldLabel>Max result (bytes)</TextFieldLabel>
              <TextFieldInput
                id="toolserver-max-bytes"
                type="number"
                min={RESULT.min}
                max={RESULT.max}
                step={1}
                placeholder="16384"
              />
              <TextFieldErrorMessage role="alert">
                {violations().get("max_result_bytes")}
              </TextFieldErrorMessage>
            </TextField>
          </div>
          <p class={HELP}>
            A call past its timeout is recorded and the run continues; a longer result is truncated.
            Leave both empty for the defaults.
          </p>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={close}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={create.isPending}
            disabled={!ready()}
            onClick={() => create.mutate()}
          >
            Add tool server
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
