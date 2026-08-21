/**
 * The one place a delivery that gave up can be told to try again.
 *
 * ⭐ THE ROUTE HERE WAS ARGUED SOMEWHERE ELSE FIRST. `ActivitySection` — the
 * org-wide log — says of itself: *"THIS IS A LOG, NOT A QUEUE. Nothing here is
 * actionable and nothing here has a button: a delivery that failed is retried
 * from the alert it belongs to, where the context to judge that lives."* That
 * sentence described an intention nothing implemented; this component is the
 * other half of it. It is mounted only by the alert detail's "Who was told"
 * panel, and it is deliberately not exported to any list screen.
 *
 * ⛔ THE BUTTON EXISTS ONLY FOR A ROW THE SERVER WILL ACCEPT. `POST
 * /deliveries/{id}/retry` is a `412 delivery_not_dead` for anything that is not
 * `dead`: pending and failed deliveries are already on §G.6's backoff and
 * nudging one would double-send. So the query asks for `status=dead` and nothing
 * else, and a retried row leaves the list on the refetch rather than sitting
 * there offering a second press that would fail.
 *
 * ⚠️ THE QUERY KEY IS DECLARED HERE AND IT SHOULD NOT BE. Every other key in the
 * app comes from `~/api/keys`, and this one belongs beside `alerts.notifications`
 * for the same reason that one does. It sits under the `["alerts"]` prefix so the
 * `delivery.updated` frame already invalidates it (`api/live.tsx` invalidates
 * `qk.alerts.all()` there), which is the property that actually matters — but the
 * declaration is in the wrong file and moving it is a one-line change to
 * `~/api/keys`.
 */
import { For, Show, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { listDeliveries, retryDelivery } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Delivery } from "~/api/types";
import { Button } from "~/components/ui/Button";
import { ErrorBanner } from "~/components/ui/states";
import { idempotencyKey, shortId } from "~/lib/format";

export interface DeadDeliveriesProps {
  /** The intent whose fan-out gave up. */
  readonly notificationId: string;
}

export const DeadDeliveries: Component<DeadDeliveriesProps> = (props) => {
  const client = useQueryClient();

  const dead = useQuery(() => ({
    queryKey: qk.alerts.deadDeliveries(props.notificationId),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listDeliveries({ notification_id: props.notificationId, status: "dead" }, { signal }),
  }));

  const retry = useMutation(() => ({
    mutationFn: (id: string) => retryDelivery(id, idempotencyKey()),
    onSuccess: () => {
      // ⭐ BOTH PREFIXES, BECAUSE THE ROW MOVES IN TWO PLACES AT ONCE. `["alerts"]`
      // carries this list and the panel's counts; `["notifications"]` carries the
      // org-wide log, whose headline is the intent status a revived delivery
      // changes. `api/live.tsx` invalidates exactly this pair on `delivery.updated`
      // for the same reason, and an operator who pressed the button should not
      // have to wait for the stream to agree.
      void client.invalidateQueries({ queryKey: qk.alerts.all() });
      void client.invalidateQueries({ queryKey: qk.notifications.all() });
    },
  }));

  const rows = (): readonly Delivery[] => dead.data?.data ?? [];

  return (
    <Show when={rows().length > 0}>
      <div class="mt-sm flex flex-col gap-2xs">
        <Show when={retry.error !== null}>
          <ErrorBanner error={retry.error} />
        </Show>

        <For each={rows()}>
          {(d) => (
            <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs rounded-control border border-line bg-sunken px-sm py-2xs">
              <span class="text-meta text-ink">
                {d.channel_name ?? `channel ${shortId(d.channel_id)}`}
              </span>
              {/* The class is the actionable half — `auth_expired` needs a new
                  token before a retry can do anything, `config_invalid` needs a
                  fix in oto or in the channel — so it is said before the button
                  rather than hidden behind a tooltip. */}
              <Show when={d.error_class}>
                {(cls) => <span class="text-micro text-ink-muted">{cls()}</span>}
              </Show>
              <Show when={d.error}>
                {(msg) => (
                  <span class="text-micro text-ink-subtle" title={msg()}>
                    {msg()}
                  </span>
                )}
              </Show>
              <Button
                class="ml-auto"
                variant="secondary"
                size="sm"
                busy={retry.isPending && retry.variables === d.id}
                onClick={() => retry.mutate(d.id)}
              >
                Send it again
              </Button>
            </div>
          )}
        </For>

        {/* Said once, under the list, rather than on every button: a retry is a
            fresh attempt at the SAME message, so a destination that has since
            been fixed gets the alert it never received. */}
        <p class="text-micro leading-snug text-ink-subtle">
          A retry re-sends this exact message. Fix the destination first — a
          revoked token will give up again immediately.
        </p>
      </div>
    </Show>
  );
};
