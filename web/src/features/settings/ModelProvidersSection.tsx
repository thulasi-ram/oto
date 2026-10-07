/**
 * Model providers — the endpoint (and key) an Investigator reasons with (ADR 0053 §3).
 *
 * # What this screen is
 *
 * One adapter, three settings: oto speaks the OpenAI-compatible Chat Completions API with tool
 * calling, so a provider is a base URL, a model name and an optional key. A model that does not
 * speak that API is reached through a gateway the operator runs; the panel says so, because the
 * absence of an "Anthropic" option would otherwise read as a missing feature.
 *
 * ⛔ THE KEY IS WRITE-ONLY. The server seals it and every response says only `has_key`, so a row
 * shows "key set" or "no key" and nothing else — there is no value to mask, and inventing `sk-••••`
 * would render a string that exists nowhere. The field is cleared the moment a create succeeds
 * and is never put in a query key, a log or an error message.
 *
 * ⛔ THE KEY CAN BE REPLACED AND THE ENDPOINT CAN BE DELETED, BUT ITS URL AND MODEL CANNOT BE
 * EDITED. An Investigator version pins `(base_url, model)` and a Finding names that identity, so
 * changing either in place would rewrite what a past Finding says produced it (00096): a different
 * endpoint is a new provider. A delete is refused with a 409 that says what holds it — a version
 * that dials it, or the Remedy risk model — and the dialog shows that sentence rather than a code.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf } from "~/api/bounds";
import { orphanViolations, violationsByField } from "~/api/client";
import { createModelProvider, deleteModelProvider, rotateModelProviderKey } from "~/api/endpoints";
import { CreateModelProviderRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { modelProvidersQuery } from "~/api/queries";
import type { ModelProvider } from "~/api/types";
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
const NAME_MAX = maxLengthOf(CreateModelProviderRequestSchema, "name");
const URL_MAX = maxLengthOf(CreateModelProviderRequestSchema, "base_url");
const MODEL_MAX = maxLengthOf(CreateModelProviderRequestSchema, "model");
const KEY_MAX = maxLengthOf(CreateModelProviderRequestSchema, "api_key");

const FIELDS = ["name", "base_url", "model", "api_key"] as const;

export const ModelProvidersSection: Component = () => {
  const [adding, setAdding] = createSignal(false);
  const providers = useQuery(modelProvidersQuery);

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Model providers</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setAdding(true)}>
            Add a provider
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={providers.isPending}>
            <LoadingLine />
          </Match>
          <Match when={providers.isError}>
            <ErrorState error={providers.error} onRetry={() => void providers.refetch()} />
          </Match>
          <Match when={(providers.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No model provider is configured."
              body="An Investigator needs one to reason with. Add the base URL, the model and, if the endpoint wants one, its API key."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={providers.data?.data ?? []}>{(p) => <ProviderRow provider={p} />}</For>
            </ul>
          </Match>
        </Switch>

        <p class={cn(PANEL_BODY, HELP, "border-t border-line")}>
          oto speaks the OpenAI-compatible Chat Completions API with tool calling, and nothing
          vendor-specific. A model that does not (Anthropic's own API, say) is reached through a
          gateway you run, such as LiteLLM. Keys are sealed when saved and never shown again, so a
          key is replaced, not revealed. The URL and model are fixed once saved, because Findings
          name them: to change either, add a provider and point the Investigator at it. A provider
          an Investigator uses cannot be deleted.
        </p>
      </Panel>

      <AddDialog open={adding()} onClose={() => setAdding(false)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const ProviderRow: Component<{ readonly provider: ModelProvider }> = (props) => {
  const p = (): ModelProvider => props.provider;
  const [keying, setKeying] = createSignal(false);
  const [deleting, setDeleting] = createSignal(false);
  return (
    <li class={cn(ROW, "flex min-h-12 flex-wrap items-center gap-sm")}>
      <span class="text-item font-medium text-ink">{p().name}</span>
      <Chip mono title="The model every request to this endpoint names.">
        {p().model}
      </Chip>
      <Chip
        title={
          p().has_key
            ? "A key is stored, sealed. It is never returned by any route."
            : "No key is stored: requests to this endpoint are sent without one."
        }
      >
        {p().has_key ? "key set" : "no key"}
      </Chip>
      <span class="min-w-0 truncate font-mono text-meta text-ink-subtle" title={p().base_url}>
        {p().base_url}
      </span>
      <span class="ml-auto text-meta text-ink-subtle">
        added <RelativeTime value={p().created_at} label="Added" /> ago
      </span>
      <div class="flex items-center gap-sm">
        <Button size="sm" variant="secondary" onClick={() => setKeying(true)}>
          {p().has_key ? "Replace key" : "Set key"}
        </Button>
        <Button size="sm" variant="destructive" onClick={() => setDeleting(true)}>
          Delete
        </Button>
      </div>

      <KeyDialog provider={p()} open={keying()} onClose={() => setKeying(false)} />
      <DeleteDialog provider={p()} open={deleting()} onClose={() => setDeleting(false)} />
    </li>
  );
};

/* -------------------------------------------------------------------------- */

const KeyDialog: Component<{
  readonly provider: ModelProvider;
  readonly open: boolean;
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const [apiKey, setApiKey] = createSignal("");

  const close = (): void => {
    // ⛔ A dismissed dialog must not keep a key in memory for the next open.
    setApiKey("");
    rotate.reset();
    props.onClose();
  };

  const rotate = useMutation(() => ({
    mutationFn: () => rotateModelProviderKey(props.provider.id, apiKey()),
    onSuccess: () => {
      close();
      void client.invalidateQueries({ queryKey: qk.settings.modelProviders() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(rotate.error);

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) close();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {props.provider.has_key ? "Replace" : "Set"} the key for {props.provider.name}
          </ModalTitle>
          <ModalDescription>
            Sealed when saved and never shown again. The URL and model stay as they are, so
            Findings that name this endpoint still do.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={rotate.error !== null}>
            <ErrorBanner error={rotate.error} />
          </Show>
          <TextField
            class={FIELD}
            value={apiKey()}
            required
            validationState={violations().get("api_key") ? "invalid" : "valid"}
            onChange={setApiKey}
          >
            <TextFieldLabel>New API key</TextFieldLabel>
            <TextFieldInput
              id={`provider-key-${props.provider.id}`}
              type="password"
              autocomplete="off"
              maxLength={KEY_MAX}
            />
            <TextFieldErrorMessage role="alert">{violations().get("api_key")}</TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={close}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={rotate.isPending}
            disabled={apiKey().trim() === ""}
            onClick={() => rotate.mutate()}
          >
            Save key
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */

const DeleteDialog: Component<{
  readonly provider: ModelProvider;
  readonly open: boolean;
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();

  const remove = useMutation(() => ({
    mutationFn: () => deleteModelProvider(props.provider.id),
    onSuccess: () => {
      props.onClose();
      void client.invalidateQueries({ queryKey: qk.settings.modelProviders() });
    },
  }));

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          remove.reset();
          props.onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Delete {props.provider.name}?</ModalTitle>
          <ModalDescription>
            The endpoint and its sealed key are removed. It cannot be undone, and the key is not
            recoverable.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          {/*
            The server's refusal IS the explanation: a 409 names what still holds the endpoint —
            an Investigator version that dials it, or the Remedy risk model — in a sentence, so
            the banner shows it and nothing here re-words it.
          */}
          <Show when={remove.error !== null}>
            <ErrorBanner error={remove.error} />
          </Show>
        </div>

        <ModalFooter>
          <Button
            size="sm"
            variant="secondary"
            onClick={() => {
              remove.reset();
              props.onClose();
            }}
          >
            Cancel
          </Button>
          <Button
            size="sm"
            variant="destructive"
            busy={remove.isPending}
            onClick={() => remove.mutate()}
          >
            Delete it
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */

const AddDialog: Component<{ readonly open: boolean; readonly onClose: () => void }> = (props) => {
  const client = useQueryClient();
  const [name, setName] = createSignal("");
  const [baseUrl, setBaseUrl] = createSignal("");
  const [model, setModel] = createSignal("");
  const [apiKey, setApiKey] = createSignal("");

  const create = useMutation(() => ({
    mutationFn: () => {
      const key = apiKey();
      return createModelProvider({
        name: name().trim(),
        base_url: baseUrl().trim(),
        model: model().trim(),
        // Omitted, not sent empty: "no key" is the absence of one.
        ...(key === "" ? {} : { api_key: key }),
      });
    },
    onSuccess: () => {
      setName("");
      setBaseUrl("");
      setModel("");
      // ⛔ The secret leaves the page's state the moment it is saved.
      setApiKey("");
      props.onClose();
      void client.invalidateQueries({ queryKey: qk.settings.modelProviders() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(create.error);
  const stray = (): readonly string[] => orphanViolations(create.error, FIELDS);
  const ready = (): boolean => name().trim() !== "" && baseUrl().trim() !== "" && model().trim() !== "";

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          // A dismissed dialog must not keep a key in memory for the next open.
          setApiKey("");
          props.onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Add a model provider</ModalTitle>
          <ModalDescription>
            Any endpoint that serves the OpenAI-compatible Chat Completions API with tool calling —
            a hosted API, your gateway, or a model server on the cluster network.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={create.error !== null}>
            <ErrorBanner error={create.error} />
          </Show>
          <Show when={stray().length > 0}>
            <ul class="text-meta text-ink-subtle" role="alert">
              <For each={stray()}>{(m) => <li>{m}</li>}</For>
            </ul>
          </Show>

          <TextField
            class={FIELD}
            value={name()}
            required
            validationState={violations().get("name") ? "invalid" : "valid"}
            onChange={setName}
          >
            <TextFieldLabel>Name</TextFieldLabel>
            <TextFieldInput id="provider-name" maxLength={NAME_MAX} placeholder="gateway" />
            <TextFieldDescription class={HELP}>
              How an Investigator picks it. Unique within the org.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">{violations().get("name")}</TextFieldErrorMessage>
          </TextField>

          <TextField
            class={FIELD}
            value={baseUrl()}
            required
            validationState={violations().get("base_url") ? "invalid" : "valid"}
            onChange={setBaseUrl}
          >
            <TextFieldLabel>Base URL</TextFieldLabel>
            <TextFieldInput
              id="provider-base-url"
              type="url"
              maxLength={URL_MAX}
              placeholder="https://llm.internal.example/v1"
            />
            <TextFieldDescription class={HELP}>
              Without credentials in it. A key is only ever sent over https; an http URL is accepted
              only with no key.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {violations().get("base_url")}
            </TextFieldErrorMessage>
          </TextField>

          <TextField
            class={FIELD}
            value={model()}
            required
            validationState={violations().get("model") ? "invalid" : "valid"}
            onChange={setModel}
          >
            <TextFieldLabel>Model</TextFieldLabel>
            <TextFieldInput id="provider-model" maxLength={MODEL_MAX} placeholder="gpt-4o" />
            <TextFieldErrorMessage role="alert">{violations().get("model")}</TextFieldErrorMessage>
          </TextField>

          <TextField
            class={FIELD}
            value={apiKey()}
            validationState={violations().get("api_key") ? "invalid" : "valid"}
            onChange={setApiKey}
          >
            <TextFieldLabel>API key</TextFieldLabel>
            <TextFieldInput
              id="provider-api-key"
              type="password"
              autocomplete="off"
              maxLength={KEY_MAX}
            />
            <TextFieldDescription class={HELP}>
              Optional — leave empty for a gateway inside the cluster that takes none. Sealed when
              saved and never shown again.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {violations().get("api_key")}
            </TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button
            size="sm"
            variant="secondary"
            onClick={() => {
              setApiKey("");
              props.onClose();
            }}
          >
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={create.isPending}
            disabled={!ready()}
            onClick={() => create.mutate()}
          >
            Add provider
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
