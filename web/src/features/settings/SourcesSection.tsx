/**
 * Sources and clusters — the upstreams oto listens to.
 *
 * Two facts shape the copy here:
 *
 *   - **`cluster_key` participates in alert identity**, so it is immutable after
 *     creation. Changing it would change every alert key in the cluster, and the
 *     form says that rather than letting someone discover it from a 422.
 *   - **The ingest token is returned exactly once.** There is no way to retrieve
 *     it later, so it is shown in a panel that makes copying it obvious and
 *     losing it hard.
 */
import { For, Match, Show, Switch, createMemo, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";
import * as v from "valibot";

import { maxLengthOf, patternOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import {
  createCluster,
  createSource,
  testSource,
  updateSource,
} from "~/api/endpoints";
import { CreateSourceRequestSchema, SourceKindSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import { clustersQuery, sourcesQuery } from "~/api/queries";
import type {
  CreateSourceRequest,
  Source,
  SourceCreated,
  SourceHealthStatus,
  SourceKind,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
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
  SelectErrorMessage,
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
import { idempotencyKey } from "~/lib/format";

import { DrillPanel } from "./DrillPanel";
import { RejectionsPanel } from "./RejectionsPanel";

/**
 * Tier A: an upstream's health is not an alert's state (§M.2).
 *
 * Each note ends with what the status means for ENDINGS, because that is the part
 * an operator cannot see from the badge: anything other than `healthy` holds this
 * source's alerts in place rather than expiring them (§B.4). Reconciliation is
 * what refreshes this — every source is polled, on its own interval, with no way
 * to switch it off (ADR 0006), so a stale badge means oto stopped being able to
 * look rather than being told not to.
 */
const HEALTH_NOTE: Record<SourceHealthStatus, string> = {
  healthy: "Reachable, and reconciling on schedule. Alerts oto stops hearing about can expire.",
  degraded:
    "Reachable but not entirely well — some reconciles are failing. Nothing from this source will be expired until it recovers.",
  unreachable:
    "oto cannot reach this Alertmanager. Alerts pushed by webhook may still arrive; state reconciliation will not, so oto cannot see a silence here and will not expire anything from this source.",
  unknown:
    "oto has not checked this source yet, so nothing from it will be expired until a reconcile pass succeeds.",
};

/**
 * How each upstream kind is spelled for a person.
 *
 * ⛔ TYPED AGAINST THE CONTRACT'S ENUM, so a kind the server starts accepting is
 * a build failure here rather than an `<option>` reading `grafana_oncall`. The
 * *list* comes from `SourceKindSchema`; this only supplies the capital letter.
 */
const KIND_LABEL: Record<SourceKind, string> = {
  alertmanager: "Alertmanager",
  grafana: "Grafana",
};

/*
 * SPEC §L.8.1: the form schema stays hand-written — it carries the sentences an
 * operator should read — but it `v.pipe`s into the **generated**
 * `CreateSourceRequestSchema` as its final gate, so this dialog cannot build a
 * body the API would reject. The generated schema comes from
 * `api/openapi/openapi.yaml` via gate G4 (`npm run gen:validators`), which is
 * also where the ceilings this form does not repeat (the 10…3600s reconcile
 * interval) are enforced.
 */

/* -------------------------------------------------------------------------- */
/* The contract's rules, read rather than repeated                            */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ NEITHER THE PATTERN NOR THE CAPS ARE WRITTEN HERE. This form used to spell
 * the base-URL rule `/^https?:\/\/[^\s]+$/i` with a separate trailing-slash
 * `v.check` beside it — a reconstruction of one contract regex that had already
 * drifted in two ways: the `/i` accepted `HTTP://x` the server rejects, and the
 * 2048-character cap was simply absent. The name box was uncapped for the same
 * reason. All three come off `CreateSourceRequestSchema`, the very schema this
 * form is gated through below, so they cannot disagree with it by construction.
 */
const NAME_MAX = maxLengthOf(CreateSourceRequestSchema, "name");
const BASE_URL_MAX = maxLengthOf(CreateSourceRequestSchema, "base_url");
const BASE_URL_PATTERN = patternOf(CreateSourceRequestSchema, "base_url");

/** What the dialog holds, before it is anything the API has a name for. */
interface SourceForm {
  readonly name: string;
  readonly cluster_id: string;
  readonly kind: SourceKind;
  readonly base_url: string;
  readonly prometheus_url: string;
}

/**
 * The defaults are stated here, once, rather than at the call site: they are
 * part of what "register an Alertmanager" means, and the generated schema
 * checks every one of them.
 *
 * There is no `reconcile_enabled`: the reconciler runs for every source, and the
 * interval is the only part of it that is a choice (ADR 0006).
 */
function toCreateSourceRequest(form: SourceForm): v.InferInput<typeof CreateSourceRequestSchema> {
  const prometheus = form.prometheus_url.trim();
  return {
    name: form.name.trim(),
    cluster_id: form.cluster_id,
    kind: form.kind,
    base_url: form.base_url.trim(),
    ...(prometheus !== "" ? { prometheus_url: prometheus } : {}),
    tls_skip_verify: false,
    ignore_labels: [
      "prometheus_replica",
      "__replica__",
      "monitor",
      "replica",
      "pod_template_hash",
    ],
    push_enabled: true,
    reconcile_interval_seconds: 30,
  };
}

const SourceFormSchema = v.pipe(
  v.strictObject({
    name: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, "A name is required."),
      v.maxLength(NAME_MAX, `A name is at most ${NAME_MAX} characters.`),
    ),
    cluster_id: v.pipe(v.string(), v.minLength(1, "Pick a cluster.")),
    // The contract's own picklist, not a second copy of it. The `<option>` list
    // below is generated from the same object, so the control and the schema
    // that gates it cannot come to disagree.
    kind: SourceKindSchema,
    base_url: v.pipe(
      v.string(),
      v.trim(),
      v.maxLength(BASE_URL_MAX, `A URL is at most ${BASE_URL_MAX.toLocaleString("en")} characters.`),
      // One action, because the contract states one rule: the pattern already
      // forbids the trailing slash, and it is case-sensitive — so `HTTP://…`
      // fails here for exactly the reason it fails at the server.
      v.regex(BASE_URL_PATTERN, "An absolute http or https URL, with no trailing slash."),
    ),
    prometheus_url: v.string(),
  }),
  v.transform(toCreateSourceRequest),
  CreateSourceRequestSchema, // the generated schema is the final gate
);

export const SourcesSection: Component = () => {
  const client = useQueryClient();
  const [creating, setCreating] = createSignal(false);
  const [token, setToken] = createSignal<SourceCreated | null>(null);

  const sources = useQuery(() => sourcesQuery());
  const clusters = useQuery(() => clustersQuery());

  return (
    <div class="flex flex-col gap-4">
      <Panel>
        <PanelHeader>
          <PanelTitle>Sources</PanelTitle>
          <Button
            size="sm"
            variant="default"
            disabled={(clusters.data?.data.length ?? 0) === 0}
            title={
              (clusters.data?.data.length ?? 0) === 0
                ? "Create a cluster first — a source must belong to one."
                : undefined
            }
            onClick={() => setCreating(true)}
          >
            Register an Alertmanager
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={sources.isPending}>
            <LoadingLine />
          </Match>
          <Match when={sources.isError}>
            <ErrorState error={sources.error} onRetry={() => void sources.refetch()} />
          </Match>
          <Match when={(sources.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="No sources yet."
              body="oto has nothing to listen to. Register an Alertmanager and point its webhook at the ingest URL you will be given."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={sources.data?.data ?? []}>{(s) => <SourceRow source={s} />}</For>
            </ul>
          </Match>
        </Switch>
      </Panel>

      <ClustersPanel />

      <CreateSourceDialog
        open={creating()}
        onClose={() => setCreating(false)}
        clusters={clusters.data?.data ?? []}
        onCreated={(created) => {
          setToken(created);
          void client.invalidateQueries({ queryKey: qk.settings.sources() });
        }}
      />

      <TokenDialog created={token()} onClose={() => setToken(null)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const SourceRow: Component<{ readonly source: Source }> = (props) => {
  const client = useQueryClient();
  const s = (): Source => props.source;

  const test = useMutation(() => ({
    mutationFn: () => testSource(s().id),
  }));

  const toggle = useMutation(() => ({
    mutationFn: (enabled: boolean) => updateSource(s().id, { push_enabled: enabled }),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.sources() }),
  }));

  return (
    <li class="border-b border-line px-3 py-2.5 last:border-b-0">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-item font-medium text-ink">{s().name}</span>
        <Chip>{s().kind}</Chip>
        <Show when={s().cluster_key}>{(key) => <Chip mono>{key()}</Chip>}</Show>
        <span
          class={cn(
            "rounded-chip border px-1.5 text-meta leading-5",
            s().health?.status === "healthy"
              ? "border-line bg-surface text-ink-muted"
              : "border-line-strong bg-raised font-medium text-ink",
          )}
          title={HEALTH_NOTE[s().health?.status ?? "unknown"]}
        >
          {s().health?.status ?? "unknown"}
        </span>
        <Show when={s().health?.last_reconcile_at}>
          {(at) => (
            <span class="text-meta text-ink-subtle">
              reconciled <RelativeTime value={at()} label="Last reconcile" /> ago
            </span>
          )}
        </Show>
        {/*
          The cadence is shown beside the last pass because it is the only
          reconciliation setting there is. There is no on/off switch here and
          there is no longer one in the API: reconciliation is the only way oto
          can see an upstream silence, so a source that is not polled would show
          a silenced alert as firing and then let the reaper end it.
        */}
        <span
          class="text-meta text-ink-subtle"
          title="How often oto polls this Alertmanager's API v2. Every source is polled — it is the only way oto can see a silence, so it cannot be turned off."
        >
          every {s().reconcile_interval_seconds}s
        </span>

        <div class="ml-auto flex items-center gap-2">
          <Button size="sm" busy={test.isPending} onClick={() => test.mutate()}>
            Test
          </Button>
          <div class="flex items-center gap-1.5">
            <Checkbox
              id={`source-${s().id}-push`}
              checked={s().push_enabled}
              onChange={(next) => toggle.mutate(next)}
            />
            <label for={`source-${s().id}-push-input`} class="cursor-pointer select-none text-meta text-ink">
              accept webhooks
            </label>
          </div>
        </div>
      </div>

      <p class="mt-0.5 break-all font-mono text-meta text-ink-subtle">{s().base_url}</p>

      <Show when={s().health?.last_error}>
        {(err) => (
          <p class="mt-1 border-l-2 border-line-strong pl-2 text-meta leading-snug text-ink">
            {err()}
          </p>
        )}
      </Show>

      <Show when={test.data}>
        {(result) => (
          <p
            class={cn(
              "mt-1 rounded-control border px-2 py-1 text-meta leading-snug",
              result().ok
                ? "border-line bg-sunken text-ink-muted"
                : "border-line-strong bg-raised font-medium text-ink",
            )}
          >
            {result().ok
              ? `Reachable. ${result().am_version ? `Alertmanager ${result().am_version}.` : ""} This used the same client and the same credentials the reconciler uses, so a pass here means the real path works.`
              : `Could not reach it: ${result().error ?? "no detail given"}`}
          </p>
        )}
      </Show>

      {/*
        The drill sits UNDER the source row and not beside the Test button, and
        the two are different questions. `Test` probes the upstream: can oto
        reach this Alertmanager. The drill asks the question an operator actually
        has on day one — will an alert from this source reach my channel, in a
        thread, with the right card, and be recorded — by pushing one synthetic
        alert through the real pipeline. Both are kept: the probe is cheap and
        answers instantly, the drill costs a Slack message and ninety seconds.
      */}
      <DrillPanel sourceID={s().id} />

      {/*
        And under the drill, the question the drill cannot answer. The drill
        proves what WOULD happen to a new alert; this says what DID happen to the
        ones already sent — which of them oto refused, why, and with which
        labels, plus any batch that stopped before its alerts were ever read.
        Both are closed by default: an operator with twenty sources should not
        pay forty requests for a screen they came to rename a cluster on.
      */}
      <RejectionsPanel sourceID={s().id} />

      <Show when={test.error !== null}>
        <ErrorBanner error={test.error} class="mt-1" />
      </Show>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

const ClustersPanel: Component = () => {
  const client = useQueryClient();
  const [key, setKey] = createSignal("");
  const [name, setName] = createSignal("");

  const clusters = useQuery(() => clustersQuery());

  const create = useMutation(() => ({
    mutationFn: () =>
      createCluster({ cluster_key: key().trim(), display_name: name().trim() }, idempotencyKey()),
    onSuccess: () => {
      setKey("");
      setName("");
      void client.invalidateQueries({ queryKey: qk.settings.clusters() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(create.error);

  return (
    <Panel>
      <PanelHeader>
        <PanelTitle>Clusters</PanelTitle>
        <span class="text-meta text-ink-subtle">
          a cluster is an identity and failure domain, not a label
        </span>
      </PanelHeader>

      <Show when={(clusters.data?.data.length ?? 0) > 0}>
        <ul>
          <For each={clusters.data?.data ?? []}>
            {(c) => (
              <li class="flex items-center gap-2 border-b border-line px-3 py-2 last:border-b-0">
                <span class="text-item text-ink">{c.display_name}</span>
                <Chip mono title="Immutable — it participates in every alert key in this cluster.">
                  {c.cluster_key}
                </Chip>
                <Show when={c.source_count !== undefined}>
                  <span class="ml-auto text-meta text-ink-subtle">
                    {c.source_count} source{c.source_count === 1 ? "" : "s"}
                  </span>
                </Show>
              </li>
            )}
          </For>
        </ul>
      </Show>

      <div class="flex flex-wrap items-end gap-2 border-t border-line bg-raised px-3 py-2">
        <div class="min-w-[10rem] flex-1">
          <TextField
            value={key()}
            validationState={violations().get("cluster_key") ? "invalid" : "valid"}
            onChange={setKey}
          >
            <TextFieldLabel>Cluster key</TextFieldLabel>
            <TextFieldInput id="cluster-key" class="font-mono" placeholder="prod-eu" />
            <TextFieldDescription>
              Immutable. It participates in alert identity, so changing it later would re-key every
              alert.
            </TextFieldDescription>
            <TextFieldErrorMessage id="cluster-key-error" role="alert">
              {violations().get("cluster_key")}
            </TextFieldErrorMessage>
          </TextField>
        </div>
        <div class="min-w-[10rem] flex-1">
          <TextField
            value={name()}
            validationState={violations().get("display_name") ? "invalid" : "valid"}
            onChange={setName}
          >
            <TextFieldLabel>Display name</TextFieldLabel>
            <TextFieldInput id="cluster-name" placeholder="Production EU" />
            <TextFieldErrorMessage id="cluster-name-error" role="alert">
              {violations().get("display_name")}
            </TextFieldErrorMessage>
          </TextField>
        </div>
        <Button
          size="default"
          busy={create.isPending}
          disabled={key().trim() === "" || name().trim() === ""}
          onClick={() => create.mutate()}
        >
          Add cluster
        </Button>
      </div>
    </Panel>
  );
};

/* -------------------------------------------------------------------------- */

const CreateSourceDialog: Component<{
  readonly open: boolean;
  readonly onClose: () => void;
  readonly clusters: readonly { readonly id: string; readonly display_name: string }[];
  readonly onCreated: (created: SourceCreated) => void;
}> = (props) => {
  const [name, setName] = createSignal("");
  const [clusterId, setClusterId] = createSignal("");
  const [kind, setKind] = createSignal<SourceKind>("alertmanager");
  const [baseUrl, setBaseUrl] = createSignal("");
  const [promUrl, setPromUrl] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const parsed = createMemo(() =>
    v.safeParse(SourceFormSchema, {
      name: name(),
      cluster_id: clusterId(),
      kind: kind(),
      base_url: baseUrl(),
      prometheus_url: promUrl(),
    }),
  );

  const localError = (field: string): string | undefined => {
    if (!touched()) return undefined;
    const result = parsed();
    if (result.success) return undefined;
    return result.issues.find((i) => i.path?.[0]?.key === field)?.message;
  };

  const create = useMutation(() => ({
    mutationFn: (body: CreateSourceRequest) => createSource(body, idempotencyKey()),
    onSuccess: (created) => {
      props.onCreated(created as unknown as SourceCreated);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(create.error);

  /** Rendered next to a required field's label — matches the old `Field`'s asterisk exactly. */
  const Required: Component = () => (
    <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
      *
    </span>
  );

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Register an Alertmanager</ModalTitle>
          <ModalDescription>
            oto reads state from this upstream and accepts webhooks from it. It never writes to your
            cluster — it cannot create, edit or expire a silence.
          </ModalDescription>
        </ModalHeader>

        <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
          <Show when={create.error !== null}>
            <ErrorBanner error={create.error} />
          </Show>

          <TextField
            value={name()}
            required
            validationState={(localError("name") ?? violations().get("name")) ? "invalid" : "valid"}
            onChange={(value) => {
              setTouched(true);
              setName(value);
            }}
          >
            <TextFieldLabel>
              Name
              <Required />
            </TextFieldLabel>
            <TextFieldInput id="src-name" maxLength={NAME_MAX} placeholder="alertmanager-prod-eu" />
            <TextFieldErrorMessage id="src-name-error" role="alert">
              {localError("name") ?? violations().get("name")}
            </TextFieldErrorMessage>
          </TextField>

          <Select
            options={[...props.clusters]}
            optionValue="id"
            optionTextValue="display_name"
            required
            value={props.clusters.find((c) => c.id === clusterId()) ?? null}
            onChange={(value) => {
              setTouched(true);
              setClusterId(value?.id ?? "");
            }}
            validationState={
              (localError("cluster_id") ?? violations().get("cluster_id")) ? "invalid" : "valid"
            }
            placeholder="— pick one —"
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue.display_name}</SelectItem>
            )}
          >
            <SelectLabel>
              Cluster
              <Required />
            </SelectLabel>
            <SelectTrigger id="src-cluster">
              <SelectValue<{ readonly id: string; readonly display_name: string }>>
                {(state) => state.selectedOption().display_name}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
            <SelectErrorMessage id="src-cluster-error" role="alert">
              {localError("cluster_id") ?? violations().get("cluster_id")}
            </SelectErrorMessage>
          </Select>

          <Select
            options={SourceKindSchema.options}
            optionTextValue={(option) => KIND_LABEL[option]}
            required
            value={kind()}
            onChange={(value) => setKind(value ?? "alertmanager")}
            validationState={violations().get("kind") ? "invalid" : "valid"}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{KIND_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectLabel>
              Kind
              <Required />
            </SelectLabel>
            <SelectTrigger id="src-kind-trigger">
              <SelectValue<SourceKind>>{(state) => KIND_LABEL[state.selectedOption()]}</SelectValue>
            </SelectTrigger>
            {/* `id` goes on the hidden native `<select>`, not the trigger button — this is the
                element `settings-contract.test.tsx` reads `.options` off of, exactly as it did
                the old native `<select>`. */}
            <SelectHiddenSelect id="src-kind" />
            <SelectContent />
            <SelectErrorMessage role="alert">{violations().get("kind")}</SelectErrorMessage>
          </Select>

          <TextField
            value={baseUrl()}
            required
            validationState={
              (localError("base_url") ?? violations().get("base_url")) ? "invalid" : "valid"
            }
            onChange={(value) => {
              setTouched(true);
              setBaseUrl(value);
            }}
          >
            <TextFieldLabel>
              Base URL
              <Required />
            </TextFieldLabel>
            <TextFieldInput
              id="src-url"
              class="font-mono"
              placeholder="https://alertmanager.example.com"
            />
            <TextFieldDescription>
              Absolute, no trailing slash. All replicas of an HA pair must be registered against the
              same cluster — they are the same failure domain, and their duplicate webhooks are
              deduplicated by design.
            </TextFieldDescription>
            <TextFieldErrorMessage id="src-url-error" role="alert">
              {localError("base_url") ?? violations().get("base_url")}
            </TextFieldErrorMessage>
          </TextField>

          <TextField
            value={promUrl()}
            validationState={violations().get("prometheus_url") ? "invalid" : "valid"}
            onChange={setPromUrl}
          >
            <TextFieldLabel>Prometheus URL (optional)</TextFieldLabel>
            <TextFieldInput
              id="src-prom"
              class="font-mono"
              placeholder="https://prometheus.example.com"
            />
            <TextFieldDescription>
              Lets oto read the rules API, which is what makes a rule snapshot authoritative rather
              than reconstructed from a generatorURL.
            </TextFieldDescription>
            <TextFieldErrorMessage id="src-prom-error" role="alert">
              {violations().get("prometheus_url")}
            </TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={create.isPending}
            onClick={() => {
              setTouched(true);
              const result = parsed();
              if (result.success) create.mutate(result.output);
            }}
          >
            Register
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/**
 * The ingest token, shown exactly once.
 *
 * The contract is explicit that there is no way to retrieve it later, so this
 * dialog says so before the user closes it rather than after.
 */
const TokenDialog: Component<{
  readonly created: SourceCreated | null;
  readonly onClose: () => void;
}> = (props) => (
  <Modal
    open={props.created !== null}
    onOpenChange={(isOpen) => {
      if (!isOpen) props.onClose();
    }}
  >
    <ModalContent>
      <ModalHeader>
        <ModalTitle>Your ingest token</ModalTitle>
        <ModalDescription>
          This is the only time oto will show it. Only a hash is stored, so it cannot be shown again —
          rotate the source to get a new one.
        </ModalDescription>
      </ModalHeader>

      <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
        <Show when={props.created}>
          {(created) => (
            <>
              <div class="rounded-control border border-line-strong bg-sunken px-2 py-2">
                <code class="block break-all font-mono text-body text-ink">
                  {created().ingest_token}
                </code>
              </div>
              <Button
                size="sm"
                onClick={() => void navigator.clipboard?.writeText(created().ingest_token)}
              >
                Copy token
              </Button>
              <p class="text-body leading-relaxed text-ink-muted">
                Point your Alertmanager's webhook receiver at oto's ingest URL for this source and send
                this token as its bearer credential.
              </p>
            </>
          )}
        </Show>
      </div>

      <ModalFooter>
        <Button size="sm" variant="default" onClick={props.onClose}>
          I have copied it
        </Button>
      </ModalFooter>
    </ModalContent>
  </Modal>
);
