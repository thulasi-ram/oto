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

import { violationsByField } from "~/api/client";
import {
  createCluster,
  createSource,
  listClusters,
  listSources,
  testSource,
  updateSource,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Source, SourceCreated, SourceHealthStatus, SourceKind } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import { Button, Checkbox, Chip, Field, Input, Panel, PanelHeader, PanelTitle, Select, cx } from "~/components/ui/primitives";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";

/** Tier A: an upstream's health is not an alert's state (§M.2). */
const HEALTH_NOTE: Record<SourceHealthStatus, string> = {
  healthy: "Reachable, and reconciling on schedule.",
  degraded: "Reachable but not entirely well — some reconciles are failing.",
  unreachable:
    "oto cannot reach this Alertmanager. Alerts pushed by webhook may still arrive; state reconciliation will not.",
  unknown: "oto has not checked this source yet.",
};

const SourceSchema = v.object({
  name: v.pipe(v.string(), v.trim(), v.minLength(1, "A name is required.")),
  cluster_id: v.pipe(v.string(), v.minLength(1, "Pick a cluster.")),
  base_url: v.pipe(
    v.string(),
    v.trim(),
    v.regex(/^https?:\/\/[^\s]+$/i, "An absolute http or https URL."),
    v.check((s) => !s.endsWith("/"), "No trailing slash."),
  ),
});

export const SourcesSection: Component = () => {
  const client = useQueryClient();
  const [creating, setCreating] = createSignal(false);
  const [token, setToken] = createSignal<SourceCreated | null>(null);

  const sources = useQuery(() => ({
    queryKey: qk.settings.sources(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listSources({ signal }),
  }));

  const clusters = useQuery(() => ({
    queryKey: qk.settings.clusters(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listClusters({ signal }),
  }));

  return (
    <div class="flex flex-col gap-4">
      <Panel>
        <PanelHeader>
          <PanelTitle>Sources</PanelTitle>
          <Button
            size="sm"
            variant="primary"
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
        <span class="text-[13px] font-medium text-ink">{s().name}</span>
        <Chip>{s().kind}</Chip>
        <Show when={s().cluster_key}>{(key) => <Chip mono>{key()}</Chip>}</Show>
        <span
          class={cx(
            "rounded-[3px] border px-1.5 text-[11px] leading-5",
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
            <span class="text-[11px] text-ink-subtle">
              reconciled <RelativeTime value={at()} label="Last reconcile" /> ago
            </span>
          )}
        </Show>

        <div class="ml-auto flex items-center gap-2">
          <Button size="sm" busy={test.isPending} onClick={() => test.mutate()}>
            Test
          </Button>
          <Checkbox
            checked={s().push_enabled}
            onChange={(next) => toggle.mutate(next)}
            label={<span class="text-[11px]">accept webhooks</span>}
          />
        </div>
      </div>

      <p class="mt-0.5 break-all font-mono text-[11px] text-ink-subtle">{s().base_url}</p>

      <Show when={s().health?.last_error}>
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
              ? `Reachable. ${result().am_version ? `Alertmanager ${result().am_version}.` : ""} This used the same client and the same credentials the reconciler uses, so a pass here means the real path works.`
              : `Could not reach it: ${result().error ?? "no detail given"}`}
          </p>
        )}
      </Show>

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

  const clusters = useQuery(() => ({
    queryKey: qk.settings.clusters(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listClusters({ signal }),
  }));

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
        <span class="text-[11px] text-ink-subtle">
          a cluster is an identity and failure domain, not a label
        </span>
      </PanelHeader>

      <Show when={(clusters.data?.data.length ?? 0) > 0}>
        <ul>
          <For each={clusters.data?.data ?? []}>
            {(c) => (
              <li class="flex items-center gap-2 border-b border-line px-3 py-2 last:border-b-0">
                <span class="text-[13px] text-ink">{c.display_name}</span>
                <Chip mono title="Immutable — it participates in every alert key in this cluster.">
                  {c.cluster_key}
                </Chip>
                <Show when={c.source_count !== undefined}>
                  <span class="ml-auto text-[11px] text-ink-subtle">
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
          <Field
            id="cluster-key"
            label="Cluster key"
            hint="Immutable. It participates in alert identity, so changing it later would re-key every alert."
            error={violations().get("cluster_key")}
          >
            {(a) => (
              <Input
                {...a}
                mono
                value={key()}
                placeholder="prod-eu"
                onInput={(e) => setKey(e.currentTarget.value)}
              />
            )}
          </Field>
        </div>
        <div class="min-w-[10rem] flex-1">
          <Field id="cluster-name" label="Display name" error={violations().get("display_name")}>
            {(a) => (
              <Input
                {...a}
                value={name()}
                placeholder="Production EU"
                onInput={(e) => setName(e.currentTarget.value)}
              />
            )}
          </Field>
        </div>
        <Button
          size="md"
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
    v.safeParse(SourceSchema, {
      name: name(),
      cluster_id: clusterId(),
      base_url: baseUrl(),
    }),
  );

  const localError = (field: string): string | undefined => {
    if (!touched()) return undefined;
    const result = parsed();
    if (result.success) return undefined;
    return result.issues.find((i) => i.path?.[0]?.key === field)?.message;
  };

  const create = useMutation(() => ({
    mutationFn: () =>
      createSource(
        {
          name: name().trim(),
          cluster_id: clusterId(),
          kind: kind(),
          base_url: baseUrl().trim(),
          ...(promUrl().trim() !== "" ? { prometheus_url: promUrl().trim() } : {}),
          tls_skip_verify: false,
          ignore_labels: ["prometheus_replica", "__replica__", "monitor", "replica", "pod_template_hash"],
          push_enabled: true,
          reconcile_enabled: true,
          reconcile_interval_seconds: 30,
        },
        idempotencyKey(),
      ),
    onSuccess: (created) => {
      props.onCreated(created as unknown as SourceCreated);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(create.error);

  return (
    <Dialog
      open={props.open}
      onClose={props.onClose}
      title="Register an Alertmanager"
      description="oto reads state from this upstream and accepts webhooks from it. It never writes to your cluster — it cannot create, edit or expire a silence."
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            busy={create.isPending}
            onClick={() => {
              setTouched(true);
              if (parsed().success) create.mutate();
            }}
          >
            Register
          </Button>
        </>
      }
    >
      <DialogBody>
        <Show when={create.error !== null}>
          <ErrorBanner error={create.error} />
        </Show>

        <Field id="src-name" label="Name" required error={localError("name") ?? violations().get("name")}>
          {(a) => (
            <Input {...a} value={name()} placeholder="alertmanager-prod-eu" onInput={(e) => { setTouched(true); setName(e.currentTarget.value); }} />
          )}
        </Field>

        <Field id="src-cluster" label="Cluster" required error={localError("cluster_id") ?? violations().get("cluster_id")}>
          {(a) => (
            <Select {...a} value={clusterId()} onChange={(e) => { setTouched(true); setClusterId(e.currentTarget.value); }}>
              <option value="">— pick one —</option>
              <For each={props.clusters}>{(c) => <option value={c.id}>{c.display_name}</option>}</For>
            </Select>
          )}
        </Field>

        <Field id="src-kind" label="Kind" required error={violations().get("kind")}>
          {(a) => (
            <Select {...a} value={kind()} onChange={(e) => setKind(e.currentTarget.value as SourceKind)}>
              <option value="alertmanager">Alertmanager</option>
              <option value="grafana">Grafana</option>
            </Select>
          )}
        </Field>

        <Field
          id="src-url"
          label="Base URL"
          required
          hint="Absolute, no trailing slash. All replicas of an HA pair must be registered against the same cluster — they are the same failure domain, and their duplicate webhooks are deduplicated by design."
          error={localError("base_url") ?? violations().get("base_url")}
        >
          {(a) => (
            <Input {...a} mono value={baseUrl()} placeholder="https://alertmanager.example.com" onInput={(e) => { setTouched(true); setBaseUrl(e.currentTarget.value); }} />
          )}
        </Field>

        <Field
          id="src-prom"
          label="Prometheus URL (optional)"
          hint="Lets oto read the rules API, which is what makes a rule snapshot authoritative rather than reconstructed from a generatorURL."
          error={violations().get("prometheus_url")}
        >
          {(a) => <Input {...a} mono value={promUrl()} placeholder="https://prometheus.example.com" onInput={(e) => setPromUrl(e.currentTarget.value)} />}
        </Field>
      </DialogBody>
    </Dialog>
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
  <Dialog
    open={props.created !== null}
    onClose={props.onClose}
    title="Your ingest token"
    description="This is the only time oto will show it. Only a hash is stored, so it cannot be shown again — rotate the source to get a new one."
    footer={
      <Button size="sm" variant="primary" onClick={props.onClose}>
        I have copied it
      </Button>
    }
  >
    <DialogBody>
      <Show when={props.created}>
        {(created) => (
          <>
            <div class="rounded-[4px] border border-line-strong bg-sunken px-2 py-2">
              <code class="block break-all font-mono text-[12px] text-ink">
                {created().ingest_token}
              </code>
            </div>
            <Button
              size="sm"
              onClick={() => void navigator.clipboard?.writeText(created().ingest_token)}
            >
              Copy token
            </Button>
            <p class="text-[12px] leading-relaxed text-ink-muted">
              Point your Alertmanager's webhook receiver at oto's ingest URL for this source and send
              this token as its bearer credential.
            </p>
          </>
        )}
      </Show>
    </DialogBody>
  </Dialog>
);
