/**
 * `/incidents/:number` — one Incident, and every Case that has ever been in it.
 *
 * ⭐ THE ONLY THINGS A HUMAN CAN CHANGE HERE ARE WHICH CASES ARE IN IT. Add a
 * Case, remove one, move one to another Incident — the three membership verbs
 * ADR 0052 §4 gives a human, each attributed to whoever pressed it and each
 * leaving a fact on the Case's own timeline. That is the whole edit surface.
 *
 * ⛔ AND THE STATE IS NOT ONE OF THEM. `active`/`quiet` is read off the member
 * Cases by the server on every request (§3); there is no control for it on this
 * screen, no request that could carry it, and no `status`, `lead` or `severity`
 * anywhere — the response to an Incident lives in the incident tool it is
 * declared to. The header's chip is a reading. When membership changes, the
 * state the server sends back may change with it, and that is the only way it
 * ever moves from here.
 *
 * ⭐ A REMOVED CASE STAYS ON THE PAGE. Removal tombstones the membership rather
 * than deleting it, so the lower panel keeps "removed by alice" and "moved to #7"
 * on record — the story includes what was taken out of it, and a Correlator is
 * bound never to re-add a Case a human removed, which only means something while
 * the removal is visible.
 *
 * ⛔ A REFUSAL IS SHOWN IN THE SERVER'S OWN WORDS. Adding a Case that already
 * belongs to another Incident is `409 case_in_incident`, and the problem's
 * `detail` names that Incident and the move that would bring the Case here. That
 * sentence is the pointer — rewording it client-side would drop the one fact the
 * operator needs — so the banner renders it verbatim.
 */
import { For, Match, Show, Switch, createMemo, createSignal } from "solid-js";
import { A, useNavigate, useParams } from "@solidjs/router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import {
  addIncidentCase,
  getIncident,
  moveIncidentCase,
  removeIncidentCase,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { IncidentDetail, IncidentMember } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { CaseStateChip } from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { Chip, PageHeading, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, Skeleton } from "~/components/ui/states";
import { TextField, TextFieldInput, TextFieldLabel } from "~/components/ui/TextField";
import { IncidentStateChip, describeAttribution } from "~/features/incidents/parts";
import { idempotencyKey } from "~/lib/format";

/** A route parameter that names an Incident, or `null` when it cannot. */
function parseNumber(raw: string): number | null {
  if (!/^[1-9][0-9]{0,17}$/.test(raw)) return null;
  const n = Number(raw);
  return Number.isSafeInteger(n) ? n : null;
}

export default function IncidentDetailRoute() {
  const params = useParams<{ number: string }>();
  const client = useQueryClient();
  const navigate = useNavigate();

  const number = createMemo(() => parseNumber(params.number));

  const detail = useQuery(() => ({
    queryKey: qk.incidents.detail(params.number),
    queryFn: ({ signal }: { signal: AbortSignal }) => getIncident(number() ?? 0, { signal }),
    // A path that names no Incident is not worth a request whose only answer is
    // a refusal; it is said below instead.
    enabled: number() !== null,
  }));

  /**
   * Every membership write changes this Incident, the Incident list (its counts
   * and its state), and the Case it touched — whose own timeline has just gained
   * an `incident.case_*` fact. The answer the server returns is written straight
   * into the entry it describes, so the page never shows the pre-write
   * membership between the response and the refetch.
   */
  const settle = (fresh: IncidentDetail): void => {
    client.setQueryData(qk.incidents.detail(String(fresh.number)), fresh);
    void client.invalidateQueries({ queryKey: qk.incidents.all() });
    void client.invalidateQueries({ queryKey: qk.cases.all() });
  };

  // One refusal on screen at a time: the most recent one is the one the operator
  // is reacting to, and stacking three banners would bury it.
  const [refusal, setRefusal] = createSignal<unknown>(null);

  const add = useMutation(() => ({
    mutationFn: (caseId: string) => addIncidentCase(number() ?? 0, caseId, idempotencyKey()),
    onMutate: () => setRefusal(null),
    onSuccess: (fresh: IncidentDetail) => {
      settle(fresh);
      setCaseId("");
    },
    onError: (err: unknown) => setRefusal(err),
  }));

  const remove = useMutation(() => ({
    mutationFn: (caseId: string) => removeIncidentCase(number() ?? 0, caseId, idempotencyKey()),
    onMutate: () => setRefusal(null),
    onSuccess: settle,
    onError: (err: unknown) => setRefusal(err),
  }));

  const move = useMutation(() => ({
    mutationFn: (v: { readonly caseId: string; readonly to: number }) =>
      moveIncidentCase(number() ?? 0, v.caseId, v.to, idempotencyKey()),
    onMutate: () => setRefusal(null),
    onSuccess: (destination: IncidentDetail) => {
      // ⭐ THE ANSWER IS THE INCIDENT THE CASE WENT TO, so that is where the
      // operator goes: the Case they just moved is on that page now, and this
      // one is refetched by the invalidation for when they come back.
      settle(destination);
      navigate(`/incidents/${destination.number}`);
    },
    onError: (err: unknown) => setRefusal(err),
  }));

  const [caseId, setCaseId] = createSignal("");

  return (
    <Switch>
      <Match when={number() === null}>
        <ErrorState error={new Error("That is not an Incident number. Incidents are numbered from 1.")} />
      </Match>
      <Match when={detail.isPending}>
        <div class="space-y-3 p-4">
          <Skeleton class="h-6 w-96" />
          <Skeleton class="h-64 w-full" />
        </div>
      </Match>
      <Match when={detail.isError}>
        <ErrorState error={detail.error} onRetry={() => void detail.refetch()} />
      </Match>
      <Match when={detail.data}>
        {(inc) => {
          const current = (): readonly IncidentMember[] =>
            inc().members.filter((m) => m.removed_at === null || m.removed_at === undefined);
          const removed = (): readonly IncidentMember[] =>
            inc().members.filter((m) => m.removed_at !== null && m.removed_at !== undefined);

          return (
            <div class="flex min-h-0 flex-1 flex-col">
              <header class="shrink-0 border-b border-line bg-surface px-4 pb-2 pt-3">
                <div class="flex flex-wrap items-center gap-2">
                  <PageHeading brush="rule">Incident #{inc().number}</PageHeading>
                  <IncidentStateChip state={inc().state} />
                  <Chip title="How many Cases are in it now, and how many of those are still open. The state beside it is read off exactly this.">
                    {inc().member_count} Case{inc().member_count === 1 ? "" : "s"},{" "}
                    {inc().open_member_count} open
                  </Chip>
                </div>
                <p class="mt-1 text-meta text-ink-subtle">
                  A set of Cases drawn together as one story. It is active while any of them is
                  open and quiet once all have ended — read off the Cases, never set by hand.
                </p>
                <p class="mt-1 text-body text-ink-muted">
                  <span class="text-ink-subtle">drawn by</span> {describeAttribution(inc().drawn_by)}{" "}
                  <RelativeTime value={inc().drawn_at} label="Drawn" /> ago
                </p>

                <Show when={refusal()}>
                  {(err) => <ErrorBanner class="mt-2" error={err()} />}
                </Show>
              </header>

              <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-auto p-4 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
                <Panel>
                  <PanelHeader>
                    <PanelTitle>In this Incident</PanelTitle>
                  </PanelHeader>
                  <Show
                    when={current().length > 0}
                    fallback={
                      <p class="px-3 py-2 text-body text-ink-muted">
                        No Case is in this Incident now. Every one that was is listed as removed.
                      </p>
                    }
                  >
                    <ul>
                      <For each={current()}>
                        {(m) => (
                          <CurrentMember
                            member={m}
                            busy={remove.isPending || move.isPending}
                            onRemove={() => remove.mutate(m.case_id)}
                            onMove={(to) => move.mutate({ caseId: m.case_id, to })}
                            self={inc().number}
                          />
                        )}
                      </For>
                    </ul>
                  </Show>

                  {/* Adding by id is the minimal form of the verb: the Case screen
                      is where an operator usually starts, and it draws a new
                      Incident; this adds an existing Case to THIS one. */}
                  <form
                    class="flex flex-wrap items-end gap-2 border-t border-line px-3 py-3"
                    onSubmit={(e) => {
                      e.preventDefault();
                      const id = caseId().trim();
                      if (id !== "") add.mutate(id);
                    }}
                  >
                    <TextField class="min-w-64 flex-1" value={caseId()} onChange={setCaseId}>
                      <TextFieldLabel>Add a Case by id</TextFieldLabel>
                      <TextFieldInput id="incident-add-case" placeholder="Case id" />
                    </TextField>
                    <Button
                      type="submit"
                      size="sm"
                      variant="secondary"
                      busy={add.isPending}
                      disabled={caseId().trim() === ""}
                    >
                      Add Case
                    </Button>
                  </form>
                </Panel>

                <Panel>
                  <PanelHeader>
                    <PanelTitle>No longer in it</PanelTitle>
                    <span class="text-meta text-ink-subtle">kept on record, never deleted</span>
                  </PanelHeader>
                  <Show
                    when={removed().length > 0}
                    fallback={
                      <p class="px-3 py-2 text-body text-ink-muted">
                        Nothing has been taken out of this Incident.
                      </p>
                    }
                  >
                    <ul>
                      <For each={removed()}>{(m) => <RemovedMember member={m} />}</For>
                    </ul>
                  </Show>
                </Panel>
              </div>
            </div>
          );
        }}
      </Match>
    </Switch>
  );
}

/** The Case's own identity, as every member row leads with it. */
const MemberHead = (props: { readonly member: IncidentMember }) => (
  <div class="flex min-w-0 flex-wrap items-center gap-2">
    <A
      href={`/cases/${props.member.case_id}`}
      class="font-mono text-body font-semibold tabular-nums text-ink underline-offset-2 hover:underline"
      title="This Case's number. Opens the firing itself."
    >
      #{props.member.case_number}
    </A>
    <span class="min-w-0 truncate text-body text-ink">{props.member.alertname}</span>
    <CaseStateChip state={props.member.case_state} size="sm" />
  </div>
);

/**
 * A current member, with the two ways out of this Incident.
 *
 * ⛔ "REMOVE" DOES NOTHING TO THE CASE. The firing stays open or ended exactly as
 * it was; only its membership here is tombstoned. Move is remove-and-add in one
 * transaction on the server, so there is no instant at which the Case is in
 * neither Incident or in both.
 */
const CurrentMember = (props: {
  readonly member: IncidentMember;
  readonly busy: boolean;
  readonly self: number;
  readonly onRemove: () => void;
  readonly onMove: (to: number) => void;
}) => {
  const [moving, setMoving] = createSignal(false);
  const [to, setTo] = createSignal("");
  const target = (): number | null => {
    const n = parseNumber(to().trim());
    return n !== null && n !== props.self ? n : null;
  };

  return (
    <li class="border-b border-line px-3 py-2 last:border-b-0" data-case-id={props.member.case_id}>
      <div class="flex flex-wrap items-center gap-3">
        <div class="min-w-0 flex-1">
          <MemberHead member={props.member} />
          <p class="mt-0.5 text-meta text-ink-subtle">
            added by {describeAttribution(props.member.added_by)}{" "}
            <RelativeTime value={props.member.added_at} label="Added" /> ago
          </p>
        </div>
        <div class="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant="secondary"
            disabled={props.busy}
            onClick={() => setMoving(!moving())}
            title="Move this Case to another Incident, in one step."
          >
            Move…
          </Button>
          <Button
            size="sm"
            variant="secondary"
            disabled={props.busy}
            onClick={() => props.onRemove()}
            title="Take this Case out of this Incident. The firing itself is unchanged, and the Incident keeps it on record as removed."
          >
            Remove
          </Button>
        </div>
      </div>
      <Show when={moving()}>
        <form
          class="mt-2 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const n = target();
            if (n !== null) props.onMove(n);
          }}
        >
          <TextField class="w-48" value={to()} onChange={setTo}>
            <TextFieldLabel>Move to Incident #</TextFieldLabel>
            <TextFieldInput inputMode="numeric" placeholder="7" />
          </TextField>
          <Button type="submit" size="sm" disabled={props.busy || target() === null}>
            Move Case
          </Button>
        </form>
      </Show>
    </li>
  );
};

/** A tombstone: who took the Case out, and — for a move — where it went. */
const RemovedMember = (props: { readonly member: IncidentMember }) => {
  const m = (): IncidentMember => props.member;
  return (
    <li class="border-b border-line bg-sunken px-3 py-2 last:border-b-0" data-case-id={m().case_id}>
      <MemberHead member={m()} />
      <p class="mt-0.5 text-meta text-ink-subtle">
        <Show
          when={m().moved_to_number}
          fallback={<>removed by {m().removed_by_label ?? "a person"} </>}
        >
          {(n) => (
            <>
              moved to{" "}
              <A href={`/incidents/${n()}`} class="font-mono text-ink underline underline-offset-2">
                #{n()}
              </A>{" "}
              by {m().removed_by_label ?? "a person"}{" "}
            </>
          )}
        </Show>
        <Show when={m().removed_at}>
          {(at) => (
            <>
              <RelativeTime value={at()} label="Removed" /> ago
            </>
          )}
        </Show>
      </p>
    </li>
  );
};
