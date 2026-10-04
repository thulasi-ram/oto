/**
 * A subject's Investigations — a Case's, or an Incident's as a whole (git-bug
 * 74ea849): the latest Finding, the Steps behind it, the earlier runs, and the one
 * control that asks for another (ADR 0053, git-bug e8e5ca8).
 *
 * ⭐ ONE PANEL, TWO SUBJECTS, AND ONLY THE NOUNS DIFFER. An Incident's run is the same
 * record as a Case's — the same statuses, the same Finding captioned with its instant,
 * the same transcript — so the panel is told WHICH subject and asks the subject's own
 * list and request; everything below the header is shared. On an Incident it also
 * says why its member Cases show no runs of their own (ADR 0053 §4).
 *
 * ⭐ A FINDING IS WHAT WAS SEEN AT T, AND THE PANEL SAYS T EVERY TIME IT SAYS THE
 * FINDING. ADR 0016 makes every enrichment a snapshot — "what the cluster looked
 * like when we asked", never live state — and a Finding is the sharpest case of
 * it: a model's sentence about a firing reads as present tense unless the screen
 * stops it. So the Finding is never rendered bare. Its caption carries the
 * instant it was REACHED (`ended_at`, not `requested_at`: a run that waited ten
 * minutes in the queue saw the cluster ten minutes later than it was asked), the
 * Investigator and the VERSION that reached it, because a Finding names its
 * version (ADR 0053 §6) and an edited prompt is a different witness.
 *
 * ⛔ IT CHANGES WHAT PEOPLE READ, NEVER WHETHER THEY ARE TOLD (ADR 0053 §2). The
 * panel says so in its subtitle, and nothing on it sends, holds or suppresses a
 * notification. The control is "ask", and the answer is a record.
 *
 * ⛔ EVERY CONTROL THE RUN HIT IS SHOWN AS SUCH (ADR 0053 §6: "recorded, never
 * silent"). `exhausted` keeps its Finding and wears the word "partial" in the
 * caption, beside the budget that stopped it; `skipped` and `failed` say why in
 * the reason's own sentence and the server's `reason_detail` under it; a refused,
 * timed-out or truncated Tool call is labelled on its Step, not folded into a
 * green transcript. A screen that showed the partial Finding as a Finding would
 * be the silent breach the ADR forbids, committed by the UI rather than the loop.
 *
 * ⭐ A CLASSIFICATION IS THE INVESTIGATOR'S WORD, SAID AS SUCH (ADR 0053 §5, git-bug
 * 4298aa0). The class a Finding was given sits under the caption that names who
 * concluded it — "classified … by the model" — never as a badge on the subject,
 * which a reader would take for a fact about the signal. It is the operator's own
 * word as the set stood when the run began: a Finding keeps it when the set
 * changes, so the panel never re-reads it against today's set. A run with no
 * classification (the org wrote no classes) says nothing about one.
 *
 * ⭐ LATEST BY DEFAULT, EVERY EARLIER RUN ONE CLICK AWAY. ADR 0053 §4: a subject
 * may have many Investigations, each seeing the last one's Finding; the latest is
 * shown. The list below the Finding is the rest, latest first, and picking one
 * shows ITS Finding with ITS instant — never the latest one's.
 *
 * ⚠️ WHY IT POLLS. A run's progress announces itself with no stream frame, so
 * while any run on screen is `queued` or `running` both the list and the shown
 * run are re-read every `POLL_MS`, and the moment none is, polling stops — a
 * settled run is frozen and returns the same bytes forever. That is this
 * screen's own safety net, the shape `DrillPanel` uses for the same reason.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import {
  getInvestigation,
  listCaseInvestigations,
  listIncidentInvestigations,
  requestCaseInvestigation,
  requestIncidentInvestigation,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import { investigatorsQuery } from "~/api/queries";
import type {
  Investigation,
  InvestigationDetail,
  InvestigationStep,
  Investigator,
  ListEnvelope,
} from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button, Spinner } from "~/components/ui/Button";
import { Chip, Panel, PanelHeader, PanelTitle, SECTION_LABEL } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { PANEL_CODE_BLOCK, PANEL_HEADER, PANEL_ROW } from "~/features/alerts/detail/rhythm";
import { cn } from "~/lib/cn";
import { absoluteTime, count } from "~/lib/format";
import { IN_PROGRESS, OUTCOME, REASON_SENTENCE, STATUS_LABEL, finishNote } from "./copy";

/** How often a run in progress is re-read. */
export const POLL_MS = 3_000;

/** How many of a subject's Investigations the history lists. */
export const HISTORY_LIMIT = 50;

/**
 * What the panel is about: one Case by its id, or one Incident by the number a
 * human quotes — the way each is addressed everywhere else.
 */
export type InvestigationSubject =
  | { readonly kind: "case"; readonly id: string }
  | { readonly kind: "incident"; readonly number: number };

export interface InvestigationPanelProps {
  readonly subject: InvestigationSubject;
  /** The poll cadence while a run is in progress. A test seam; the app uses `POLL_MS`. */
  readonly pollMs?: number;
}

/**
 * What differs between subjects. The key is typed loosely here so either subject's
 * fits; each branch below still takes it from `qk`.
 */
interface SubjectApi {
  readonly list: Readonly<Record<"queryKey", readonly unknown[]>> & {
    readonly queryFn: (c: { signal: AbortSignal }) => Promise<ListEnvelope<Investigation>>;
  };
  readonly request: (investigatorId: string) => Promise<InvestigationDetail>;
  readonly noun: string;
}

/**
 * A subject's list query — its `qk` key and its read — the request that asks about
 * it, and the noun the panel calls it. These are the only things that differ.
 */
function subjectApi(subject: InvestigationSubject): SubjectApi {
  if (subject.kind === "incident") {
    return {
      list: {
        queryKey: qk.incidents.investigations(String(subject.number)),
        queryFn: ({ signal }: { signal: AbortSignal }) =>
          listIncidentInvestigations(subject.number, { limit: HISTORY_LIMIT }, { signal }),
      },
      request: (investigatorId: string) =>
        requestIncidentInvestigation(subject.number, investigatorId),
      noun: "this Incident",
    };
  }
  return {
    list: {
      queryKey: qk.cases.investigations(subject.id),
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        listCaseInvestigations(subject.id, { limit: HISTORY_LIMIT }, { signal }),
    },
    request: (investigatorId: string) => requestCaseInvestigation(subject.id, investigatorId),
    noun: "this firing",
  };
}

export const InvestigationPanel: Component<InvestigationPanelProps> = (props) => {
  const client = useQueryClient();
  const poll = (): number => props.pollMs ?? POLL_MS;
  const api = () => subjectApi(props.subject);

  /* ---- who can be asked ------------------------------------------------- */

  const investigators = useQuery(() => investigatorsQuery());
  /**
   * Only a switched-on Investigator is offered. Asking a switched-off one is
   * legal — the server records it `skipped` with reason `disabled` — but
   * offering a control whose only possible answer is "nothing ran" is a control
   * an operator has to read to discard.
   */
  const askable = (): readonly Investigator[] =>
    (investigators.data?.data ?? []).filter((i) => i.enabled);

  /* ---- what has been asked ---------------------------------------------- */

  const runs = useQuery(() => ({
    ...api().list,
    refetchInterval: () => {
      const page = client.getQueryData<ListEnvelope<Investigation>>(api().list.queryKey);
      return page?.data.some((r) => IN_PROGRESS[r.status]) === true ? poll() : false;
    },
  }));
  const rows = (): readonly Investigation[] => runs.data?.data ?? [];

  /** The run the operator picked from the history, or `null` for "the latest". */
  const [chosen, setChosen] = createSignal<string | null>(null);
  const shownRow = (): Investigation | null => {
    const id = chosen();
    return (id !== null ? rows().find((r) => r.id === id) : undefined) ?? rows()[0] ?? null;
  };
  const shownId = (): string | null => shownRow()?.id ?? null;

  const detail = useQuery(() => ({
    queryKey: qk.cases.investigation(shownId() ?? ""),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      getInvestigation(shownId() ?? "", { signal }),
    enabled: shownId() !== null,
    refetchInterval: () => {
      const d = client.getQueryData<InvestigationDetail>(qk.cases.investigation(shownId() ?? ""));
      return d !== undefined && !IN_PROGRESS[d.status] ? false : poll();
    },
  }));

  /**
   * The run as best known: its own read once it has answered — it carries the
   * Steps and is never older than the list row — and the list row before that.
   */
  const shown = (): Investigation | null => {
    const d = detail.data;
    return d !== undefined && d.id === shownId() ? d : shownRow();
  };
  const steps = (): readonly InvestigationStep[] | null => {
    const d = detail.data;
    return d !== undefined && d.id === shownId() ? d.steps : null;
  };

  /* ---- asking for another ----------------------------------------------- */

  const [picking, setPicking] = createSignal(false);
  const [refusal, setRefusal] = createSignal<unknown>(null);

  /**
   * The answer is the run AS RECORDED — `queued`, or `skipped` when a kill
   * switch is off — so it is written straight into both entries: the screen
   * shows the new run as the latest between the `202` and the list's refetch,
   * rather than the previous Finding under a control that was just pressed.
   */
  const ask = useMutation(() => ({
    mutationFn: (investigatorId: string) => api().request(investigatorId),
    onMutate: () => setRefusal(null),
    onSuccess: (run: InvestigationDetail) => {
      client.setQueryData(qk.cases.investigation(run.id), run);
      const { queryKey } = api().list;
      client.setQueryData<ListEnvelope<Investigation>>(queryKey, (old) =>
        old === undefined ? old : { ...old, data: [run, ...old.data.filter((r) => r.id !== run.id)] },
      );
      void client.invalidateQueries({ queryKey });
      setChosen(null);
      setPicking(false);
    },
    onError: (err: unknown) => setRefusal(err),
  }));

  return (
    <Panel>
      <PanelHeader class={PANEL_HEADER}>
        <div class="min-w-0">
          <PanelTitle>Investigation</PanelTitle>
          <p class="mt-2xs text-meta text-ink-subtle">
            What a model concluded when it looked. It changes what people read, never whether
            anyone is told.
          </p>
          <Show when={props.subject.kind === "incident"}>
            <p class="mt-2xs text-meta text-ink-subtle" data-incident-coverage>
              The Incident is investigated as a whole: drawing it, and Cases joining or leaving
              it, start runs of the Investigators set to investigate Incidents. Its Cases start
              none of their own while they are in it — anyone may still ask about one.
            </p>
          </Show>
        </div>
        <Show when={investigators.data !== undefined}>
          <Switch>
            <Match when={askable().length === 1}>
              <Button
                variant="secondary"
                size="sm"
                busy={ask.isPending}
                onClick={() => {
                  const only = askable()[0];
                  if (only !== undefined) ask.mutate(only.id);
                }}
                title={`Ask ${askable()[0]?.name ?? ""} v${askable()[0]?.current_version.version ?? ""} to look at ${api().noun}. It runs in the background; nothing waits on it.`}
              >
                Investigate
              </Button>
            </Match>
            <Match when={askable().length > 1}>
              <Button
                variant="secondary"
                size="sm"
                aria-expanded={picking()}
                onClick={() => setPicking(!picking())}
                title="Choose which Investigator to ask. Each runs its current version."
              >
                Investigate…
              </Button>
            </Match>
          </Switch>
        </Show>
      </PanelHeader>

      {/* More than one Investigator: one button each, named and versioned, the
          same minimal inline form the membership controls use. */}
      <Show when={picking() && askable().length > 1}>
        <div class={cn("border-b border-line", PANEL_ROW)}>
          <p class="text-meta text-ink-subtle">Which Investigator? Each runs its current version.</p>
          <div class="mt-sm flex flex-wrap items-center gap-2">
            <For each={askable()}>
              {(i) => (
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={ask.isPending}
                  onClick={() => ask.mutate(i.id)}
                  title={`${i.current_version.model.name} at ${i.current_version.model.endpoint}`}
                >
                  {i.name} v{i.current_version.version}
                </Button>
              )}
            </For>
            <Button variant="ghost" size="sm" onClick={() => setPicking(false)}>
              Cancel
            </Button>
          </div>
        </div>
      </Show>

      <Show when={refusal()}>
        {(err) => <ErrorBanner class="mx-3 mt-2" error={err()} />}
      </Show>

      {/* Nobody to ask, said once the list has answered — never guessed. */}
      <Show when={investigators.data !== undefined && askable().length === 0}>
        <p class={cn("text-body leading-snug text-ink-muted", PANEL_ROW)}>
          {(investigators.data?.data.length ?? 0) === 0
            ? "No Investigator is configured in this organisation, so there is nobody to ask. One is written through the API (POST /api/v1/investigators); there is no settings screen for it yet."
            : "Every Investigator in this organisation is switched off, so there is nobody to ask. Earlier Findings stay readable below."}
        </p>
      </Show>
      <Show when={investigators.isError}>
        <p class={cn("text-body leading-snug text-ink-muted", PANEL_ROW)}>
          oto could not list the Investigators, so none is offered here.
        </p>
      </Show>

      <Switch>
        <Match when={runs.isPending}>
          <LoadingLine />
        </Match>
        <Match when={runs.isError}>
          <ErrorState error={runs.error} onRetry={() => void runs.refetch()} />
        </Match>
        <Match when={rows().length === 0}>
          <p class={cn("text-body text-ink-muted", PANEL_ROW)}>
            No Investigation of {api().noun} has been asked for.
          </p>
        </Match>
        <Match when={shown()}>
          {(run) => (
            <RunView
              run={run()}
              steps={steps()}
              stepsError={detail.isError ? detail.error : null}
              latest={run().id === rows()[0]?.id}
            />
          )}
        </Match>
      </Switch>

      {/* The history. Shown once there is more than the one on screen. */}
      <Show when={rows().length > 1}>
        <div class="border-t border-line">
          <h3 class={cn(SECTION_LABEL, "px-3 pt-2 text-ink-muted")}>
            Every Investigation of {api().noun}
          </h3>
          <ul class="py-1" aria-label={`Every Investigation of ${api().noun}, latest first`}>
            <For each={rows()}>
              {(r, i) => (
                <li>
                  <button
                    type="button"
                    aria-pressed={r.id === shownId()}
                    onClick={() => setChosen(i() === 0 ? null : r.id)}
                    class={cn(
                      "flex w-full flex-wrap items-center gap-x-sm gap-y-2xs px-3 py-1 text-left text-body",
                      "hover:bg-raised",
                      r.id === shownId() ? "bg-sunken text-ink" : "text-ink-muted",
                    )}
                  >
                    <span class="font-mono">
                      {r.investigator_name} v{r.investigator_version}
                    </span>
                    <span>{STATUS_LABEL[r.status]}</span>
                    <Show when={r.partial}>
                      <span>partial</span>
                    </Show>
                    <span class="ml-auto text-meta text-ink-subtle">
                      asked <RelativeTime value={r.requested_at} label="Asked" /> ago
                      {i() === 0 ? " · latest" : ""}
                    </span>
                  </button>
                </li>
              )}
            </For>
          </ul>
          <Show when={runs.data?.page.has_more === true}>
            <p class="px-3 pb-2 text-meta text-ink-subtle">
              The {HISTORY_LIMIT} most recent are listed.
            </p>
          </Show>
        </div>
      </Show>
    </Panel>
  );
};

/* -------------------------------------------------------------------------- */
/* One run                                                                    */
/* -------------------------------------------------------------------------- */

const RunView: Component<{
  readonly run: Investigation;
  readonly steps: readonly InvestigationStep[] | null;
  readonly stepsError: unknown;
  readonly latest: boolean;
}> = (props) => {
  const r = (): Investigation => props.run;
  const [open, setOpen] = createSignal(false);

  return (
    <div class={PANEL_ROW} data-investigation={r().id} data-status={r().status}>
      {/* Who looked: the name, the VERSION that ran, the model behind it. */}
      <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs">
        <span class="font-mono text-body font-medium text-ink">{r().investigator_name}</span>
        <Chip
          mono
          title="The Investigator version that ran — the one its Finding names. Changing the model, prompt or Tools writes the next version."
        >
          v{r().investigator_version}
        </Chip>
        <Chip mono title={`The model, at ${r().model.endpoint}`}>
          {r().model.name}
        </Chip>
        <span
          class={cn(
            "text-meta",
            r().status === "completed" ? "text-ink-muted" : "font-medium text-ink",
          )}
        >
          {STATUS_LABEL[r().status]}
        </span>
        <span class="ml-auto text-meta text-ink-subtle">{props.latest ? "latest" : "earlier"}</span>
      </div>

      <Switch>
        <Match when={IN_PROGRESS[r().status]}>
          <p
            role="status"
            class="mt-sm flex flex-wrap items-center gap-2xs text-body leading-snug text-ink-muted"
          >
            <Spinner />
            <Show
              when={r().status === "running" && r().started_at}
              fallback={
                <span>
                  In progress — waiting to start since{" "}
                  <RelativeTime value={r().requested_at} label="Asked" /> ago.
                </span>
              }
            >
              {(at) => (
                <span>
                  In progress — running since <RelativeTime value={at()} label="Started" /> ago.
                </span>
              )}
            </Show>
            <span>It has not reached a Finding yet.</span>
          </p>
        </Match>
        <Match when={r().status === "skipped"}>
          <Reason run={r()} lead="Skipped — nothing ran." />
        </Match>
        <Match when={r().status === "failed"}>
          <Reason run={r()} lead="Failed." />
        </Match>
        <Match when={r().status === "exhausted"}>
          <Reason run={r()} lead="Stopped at a budget." />
          <FindingView run={r()} />
        </Match>
        <Match when={r().status === "completed"}>
          <FindingView run={r()} />
        </Match>
      </Switch>

      {/* What it cost and who asked — facts on the record, not a verdict. */}
      <div class="mt-sm flex flex-wrap items-center gap-2xs">
        <Chip
          title={`Input ${count(r().tokens_in)} · output ${count(r().tokens_out)}, as the model reported them. Budget ${count(r().budgets.max_tokens)}.`}
        >
          {count(r().tokens_in + r().tokens_out)} tokens spent
        </Chip>
        <Chip title="Tool calls made, refused ones included, against this run's step budget.">
          {count(r().tool_calls)} of {count(r().budgets.max_steps)} Tool calls
        </Chip>
        <Chip title={absoluteTime(r().requested_at)}>
          asked by {r().requested_by_label}{" "}
          <RelativeTime value={r().requested_at} label="Asked" /> ago
        </Chip>
      </div>

      {/* The transcript, in order. Append-only on the server, so it reads the
          same a year later — "why did it say that?" always has an answer. */}
      <Switch>
        <Match when={props.stepsError !== null && props.stepsError !== undefined}>
          <p class="mt-sm text-meta text-ink-muted">oto could not read this run's Steps.</p>
        </Match>
        <Match when={props.steps !== null && props.steps.length === 0}>
          <p class="mt-sm text-meta text-ink-subtle">No Steps are recorded for this run.</p>
        </Match>
        <Match when={props.steps}>
          {(list) => (
            <div class="mt-sm">
              <button
                type="button"
                class="text-meta text-ink-subtle underline decoration-dotted underline-offset-2 hover:text-ink"
                aria-expanded={open()}
                onClick={() => setOpen(!open())}
              >
                {open() ? "Hide" : "Show"} the {list().length} Steps
              </button>
              <Show when={open()}>
                <ol class="mt-sm space-y-sm" aria-label="Steps, in order">
                  <For each={list()}>{(s) => <StepView step={s} />}</For>
                </ol>
              </Show>
            </div>
          )}
        </Match>
      </Switch>
    </div>
  );
};

/** Why a run ended any way but `completed`: the reason's sentence, then the server's. */
const Reason: Component<{ readonly run: Investigation; readonly lead: string }> = (props) => (
  <div class="mt-sm border-l-2 border-line-strong pl-sm text-body leading-snug text-ink">
    <p>
      <span class="font-medium">{props.lead}</span>{" "}
      {props.run.reason !== null ? REASON_SENTENCE[props.run.reason] : ""}
    </p>
    <Show when={props.run.reason_detail}>
      {(d) => <p class="mt-2xs text-meta text-ink-muted">{d()}</p>}
    </Show>
  </div>
);

/** The one class oto names: always admissible, and the answer under doubt. */
const UNCLASSIFIED = "unclassified";

/**
 * The Finding, captioned with the instant it was reached and the Investigator
 * version that reached it. ⛔ Never rendered without that caption.
 */
const FindingView: Component<{ readonly run: Investigation }> = (props) => {
  const r = (): Investigation => props.run;
  return (
    <figure class="mt-sm" data-finding>
      <figcaption class="text-meta text-ink-subtle">
        <span class="font-medium text-ink">{r().partial ? "Partial Finding" : "Finding"}</span>{" "}
        by {r().investigator_name} v{r().investigator_version}, as seen at{" "}
        <time datetime={r().ended_at ?? undefined} class="tabular-nums text-ink-muted">
          {absoluteTime(r().ended_at)}
        </time>{" "}
        (<RelativeTime value={r().ended_at} label="Reached" /> ago) — what it concluded then, not
        the state now.
      </figcaption>
      <Show when={r().classification}>
        {(cls) => (
          <p class="mt-2xs text-meta text-ink-subtle" data-classification>
            <Show
              when={cls() !== UNCLASSIFIED}
              fallback={
                <>
                  The model left it <span class="font-mono text-ink">{UNCLASSIFIED}</span>: none
                  of this organisation's classes clearly fit, or it was unsure.
                </>
              }
            >
              Classified <span class="font-mono text-ink">{cls()}</span> by the model, from this
              organisation's classes as they stood when it ran — a model's judgement, not a fact
              about the signal.
            </Show>
          </p>
        )}
      </Show>
      <Show
        when={r().finding}
        fallback={
          <p class="mt-2xs text-body text-ink-muted">It reached no Finding before it stopped.</p>
        }
      >
        {(text) => (
          <p class="mt-2xs whitespace-pre-wrap break-words border-l-2 border-line-strong pl-sm text-body leading-snug text-ink">
            {text()}
          </p>
        )}
      </Show>
    </figure>
  );
};

/* -------------------------------------------------------------------------- */
/* One Step                                                                   */
/* -------------------------------------------------------------------------- */

const StepView: Component<{ readonly step: InvestigationStep }> = (props) => {
  const s = (): InvestigationStep => props.step;
  const outcome = () => (s().outcome !== null ? OUTCOME[s().outcome!] : null);
  const notable = (): boolean => s().outcome !== null && s().outcome !== "ok";

  return (
    <li
      class="border-l-2 border-line pl-sm"
      data-step={s().kind}
      data-outcome={s().outcome ?? undefined}
    >
      <div class="flex flex-wrap items-center gap-x-sm gap-y-2xs text-meta">
        <span class="font-mono text-ink-subtle">#{s().seq}</span>
        <Show
          when={s().kind === "tool_call"}
          fallback={
            <>
              <span class="text-ink">model turn</span>
              <Show when={s().tokens_in !== null || s().tokens_out !== null}>
                <span class="text-ink-subtle">
                  {count(s().tokens_in)} in · {count(s().tokens_out)} out tokens
                </span>
              </Show>
              <Show when={finishNote(s().finish_reason)}>
                {(note) => <span class="font-medium text-ink">{note()}</span>}
              </Show>
            </>
          }
        >
          <span class="text-ink">Tool call</span>
          <span class="font-mono text-ink">{s().tool_name ?? "unnamed"}</span>
          <Show when={outcome()}>
            {(o) => (
              <span
                class={notable() ? "font-medium text-ink" : "text-ink-muted"}
                title={o().note}
              >
                {o().label}
              </span>
            )}
          </Show>
        </Show>
        <span class="ml-auto tabular-nums text-ink-subtle">{count(s().duration_ms)} ms</span>
      </div>

      {/* A refused, timed-out, truncated or failed call says so in words, not
          only in a label a reader has to hover. */}
      <Show when={notable() ? outcome() : null}>
        {(o) => <p class="mt-2xs text-meta leading-snug text-ink">{o().note}</p>}
      </Show>

      <Show when={s().text}>
        {(t) => (
          <p class="mt-2xs whitespace-pre-wrap break-words text-body leading-snug text-ink">
            {t()}
          </p>
        )}
      </Show>

      <Show when={(s().tool_calls ?? []).length > 0}>
        <ul class="mt-2xs space-y-2xs">
          <For each={s().tool_calls ?? []}>
            {(c) => (
              <li class="break-all font-mono text-meta text-ink-muted">
                asked for {c.name}({c.arguments})
              </li>
            )}
          </For>
        </ul>
      </Show>

      <Show when={s().kind === "tool_call" && s().arguments}>
        {(args) => (
          <p class="mt-2xs break-all font-mono text-meta text-ink-muted">arguments {args()}</p>
        )}
      </Show>

      <Show when={s().result}>
        {(res) => <pre class={cn("mt-2xs max-h-48 overflow-y-auto", PANEL_CODE_BLOCK)}>{res()}</pre>}
      </Show>
    </li>
  );
};
