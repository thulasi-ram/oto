/**
 * A notification policy's digest Investigations (review D4).
 *
 * ⭐ "RECORDED, NEVER SILENT" NEEDS SOMEWHERE TO READ THE RECORD. A Case's runs are on
 * the Case and an Incident's on the Incident; a digest window's run has no page of its
 * own, so a run skipped for the day's budget or because its window closed — or one that
 * failed — was on the record and nowhere on screen. The policy that names the digest
 * Investigator is where a person looks, so the list sits under it, collapsed: it is read
 * only once someone opens it.
 *
 * Each row is the run in the panel's own words — `STATUS_LABEL`, and for any ending
 * but `completed` the `REASON_SENTENCE` with the server's sentence beneath — and the
 * window it summarised. A Finding is shown as reached, never as the state now.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useQuery } from "@tanstack/solid-query";

import { listPolicyDigestInvestigations } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Investigation } from "~/api/types";
import { ErrorBanner, LoadingLine } from "~/components/ui/states";
import { absoluteTime } from "~/lib/format";
import { REASON_SENTENCE, STATUS_LABEL } from "./copy";

/** How many of a policy's digest runs are read: a day of five-minute windows is 288. */
export const DIGEST_HISTORY_LIMIT = 50;

export const DigestInvestigations: Component<{ readonly policyId: string }> = (props) => {
  const [open, setOpen] = createSignal(false);
  const runs = useQuery(() => ({
    queryKey: qk.settings.policyInvestigations(props.policyId),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listPolicyDigestInvestigations(props.policyId, { limit: DIGEST_HISTORY_LIMIT }, { signal }),
    enabled: open(),
  }));
  const rows = (): readonly Investigation[] => runs.data?.data ?? [];

  return (
    <details
      class="text-meta text-ink-muted"
      data-digest-investigations
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary class="cursor-pointer text-ink-subtle">Digest Investigations</summary>
      <Switch>
        <Match when={!open()}>{null}</Match>
        <Match when={runs.isPending}>
          <LoadingLine />
        </Match>
        <Match when={runs.isError}>
          <ErrorBanner class="mt-2xs" error={runs.error} />
        </Match>
        <Match when={rows().length === 0}>
          <p class="mt-2xs">No digest window of this policy has asked for an Investigation yet.</p>
        </Match>
        <Match when={rows().length > 0}>
          <ul class="mt-2xs flex flex-col gap-xs" aria-label="Digest Investigations, latest first">
            <For each={rows()}>
              {(r) => (
                <li data-digest-run={r.status} class="border-l-2 border-line pl-sm">
                  <p class="text-ink">
                    <span class="font-mono">
                      {r.investigator_name} v{r.investigator_version}
                    </span>{" "}
                    · {STATUS_LABEL[r.status]}
                    <Show when={r.partial}> · partial</Show>
                    <Show when={r.digest_window_start && r.digest_window_end}>
                      {" "}
                      · window{" "}
                      <time datetime={r.digest_window_start} class="tabular-nums">
                        {absoluteTime(r.digest_window_start)}
                      </time>
                      –
                      <time datetime={r.digest_window_end} class="tabular-nums">
                        {absoluteTime(r.digest_window_end)}
                      </time>
                    </Show>
                  </p>
                  <Show when={r.reason}>
                    {(reason) => <p>{REASON_SENTENCE[reason()]}</p>}
                  </Show>
                  <Show when={r.reason_detail}>{(d) => <p class="text-ink-subtle">{d()}</p>}</Show>
                  <Show when={r.finding}>
                    {(f) => (
                      <p class="whitespace-pre-wrap break-words" data-finding>
                        <span class="text-ink-subtle">
                          Reached{" "}
                          <time datetime={r.ended_at ?? undefined}>{absoluteTime(r.ended_at)}</time>
                          :
                        </span>{" "}
                        {f()}
                      </p>
                    )}
                  </Show>
                </li>
              )}
            </For>
          </ul>
          <Show when={runs.data?.page.has_more === true}>
            <p class="mt-2xs text-ink-subtle">The {DIGEST_HISTORY_LIMIT} most recent are listed.</p>
          </Show>
        </Match>
      </Switch>
    </details>
  );
};
