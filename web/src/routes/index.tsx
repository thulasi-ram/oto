import { Match, Switch, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { getHealth, ApiError } from "~/api/client";

/**
 * The landing route. It exists to prove the whole path works:
 * browser -> Vite dev proxy -> Go /healthz -> valibot -> solid-query -> DOM.
 */
const Index: Component = () => {
  const health = useQuery(() => ({
    queryKey: ["healthz"] as const,
    queryFn: ({ signal }: { signal: AbortSignal }) => getHealth(signal),
    retry: false,
    refetchInterval: 15_000,
  }));

  return (
    <main class="flex min-h-full flex-col items-center justify-center gap-6 p-8">
      <h1 class="font-mono text-6xl font-bold tracking-tight">oto</h1>
      <p class="max-w-md text-center text-sm text-neutral-500 dark:text-neutral-400">
        The alert history layer your Prometheus stack does not have.
      </p>

      <section
        class="w-full max-w-md rounded-lg border border-neutral-200 p-4 dark:border-neutral-800"
        aria-live="polite"
      >
        <h2 class="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
          Backend
        </h2>
        <Switch>
          <Match when={health.isPending}>
            <p class="text-sm text-neutral-500">checking /healthz…</p>
          </Match>
          <Match when={health.isError}>
            <p class="text-sm text-[--color-state-firing]">
              unreachable —{" "}
              {health.error instanceof ApiError
                ? `${health.error.status} ${health.error.message}`
                : String(health.error)}
            </p>
          </Match>
          <Match when={health.data}>
            {(data) => (
              <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 font-mono text-sm">
                <dt class="text-neutral-500">status</dt>
                <dd class="text-[--color-state-resolved]">{data().status}</dd>
                <dt class="text-neutral-500">service</dt>
                <dd>{data().service}</dd>
                <dt class="text-neutral-500">version</dt>
                <dd>{data().version}</dd>
              </dl>
            )}
          </Match>
        </Switch>
      </section>
    </main>
  );
};

export default Index;
