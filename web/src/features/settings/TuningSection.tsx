/**
 * `/settings/tuning` — the org's tuning knobs.
 *
 * This is the screen where a badly-explained control does real damage, so it is
 * built to explain rather than merely to edit. Four things it does that a
 * generated settings form would not:
 *
 *   1. **Both failure modes, always visible.** Every knob states what breaks if
 *      the number is too small AND what breaks if it is too large, because an
 *      operator choosing a value is choosing between two outages, not avoiding
 *      one. The copy lives in `tuningCopy.ts` and is taken from
 *      `docs/setup/tuning.md` — this screen must not contradict that page.
 *
 *   2. **The Alertmanager relationship is inline, live, PROVENANCED and
 *      ROUTE-RESOLVED.** Almost every value here is only meaningful as a multiple
 *      of the source's own `group_wait` / `group_interval` / `repeat_interval`. A
 *      re-fire grace shorter than `group_interval` is unreachable at any value.
 *      oto reads those three off each source's published configuration and serves
 *      them on `SourceHealthDTO.route_timings`, each with one of three states —
 *      `observed`, `default_applies`, `unknown` — and this screen renders all
 *      three differently. They were four inputs kept in one browser's
 *      `localStorage` until this rewrite: unshared, unvalidated, and silently
 *      wrong the moment somebody edited `alertmanager.yml`.
 *
 *      ⛔ AND THE THREE ARE PER-ROUTE. The numbers that govern the alerts oto is
 *      sent are the ones on the route delivering to oto's own RECEIVER, not the
 *      top-level route. `route_timings.route` says which of the two the headline
 *      three are; `routes` is the whole resolved tree; and where several routes
 *      reach oto with different timings this screen shows the DISAGREEMENT rather
 *      than picking one — see `RouteOrigin`.
 *
 *   3. **Origin is the primary fact, not a footnote.** An operator debugging a
 *      noisy Slack needs to see instantly which values are theirs, which are
 *      oto's, and which this deployment's own configuration is forcing. Each knob
 *      is badged with `default` / `org` / `config`; a `config` knob is READ-ONLY
 *      and names the environment variable or file key that owns it; and an org
 *      override that configuration is shadowing is shown BESIDE the value in
 *      force, never hidden — a stored number nobody can see is a number that
 *      takes effect the day somebody deletes a config key.
 *
 *   4. **Reset removes the override; it never writes the default back.** Those
 *      are different facts — "600 because we chose it" and "600 because that is
 *      what oto ships" behave identically today and diverge the moment oto's
 *      default moves. The API distinguishes them with `reset`, and so does this.
 *
 * Bounds are never hardcoded here. They are read from `bounds` in the same
 * response, which is the table the server rejects with; a copy in the UI is
 * drift waiting to happen.
 */
import { useBeforeLeave, type BeforeLeaveEventArgs } from "@solidjs/router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";
import {
  For,
  Match,
  Show,
  Switch,
  createMemo,
  createSignal,
  onCleanup,
  onMount,
  type Component,
  type JSX,
} from "solid-js";

import { ApiError, orphanViolations, violationsByField } from "~/api/client";
import { getOrgSettings, updateOrgSettings } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { sourcesQuery } from "~/api/queries";
import type {
  InheritedTiming,
  OrgSettingsView,
  ReceiverRoute,
  RouteTiming,
  SettingBound,
  SettingOrigin,
  Source,
  TimingProvenance,
  UpdateOrgSettingsRequest,
} from "~/api/types";
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
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ErrorBanner, ErrorState, Skeleton } from "~/components/ui/states";
import { RelativeTime } from "~/components/Time";
import { duration } from "~/lib/format";
import { cn } from "~/lib/cn";
import {
  AM_FIELDS,
  ASSUMED_RULE_FOR_S,
  KNOBS,
  RECEIVER_BASIS_COPY,
  KNOB_GROUPS,
  MENTION_LIST_MAX,
  MENTION_MODE_OPTIONS,
  MENTION_TOKEN_HINT,
  SEVERITY_OPTIONS,
  VERBOSITY_OPTIONS,
  isNumeric,
  readValue,
  unitSuffix,
  type AmFieldCopy,
  type AmRef,
  type AmTiming,
  type Guidance,
  type KnobCopy,
  type KnobKey,
} from "./tuningCopy";

/* -------------------------------------------------------------------------- */
/* The Alertmanager reference, READ from each source                          */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THERE IS NO localStorage HERE, AND THERE IS NO FORM.
 *
 * These three numbers used to be typed into this screen and kept under
 * `oto.tuning.alertmanager.v1` in whichever browser happened to be open. That is
 * unshared (the person beside you was given different guidance), unvalidated
 * (nothing ever checked them against the cluster), and silently wrong the moment
 * somebody edited `alertmanager.yml`. oto reads them off `config.original` on the
 * status call it already makes, per source, and serves them on
 * `SourceHealthDTO.route_timings` with the provenance of each.
 */

/** Turn one source's served health into the reference the guidance argues from. */
function amRefOf(source: Source): AmRef | null {
  const timings = source.health?.route_timings;
  if (timings === undefined) return null;
  const asTiming = (t: RouteTiming): AmTiming => ({
    provenance: t.provenance,
    // `value_ms` is null exactly when the provenance is `unknown`, and
    // milliseconds because `group_wait: 500ms` is legal upstream.
    seconds: t.value_ms === null ? null : t.value_ms / 1000,
  });
  return {
    sourceId: source.id,
    sourceName: source.name,
    groupWait: asTiming(timings.group_wait),
    groupInterval: asTiming(timings.group_interval),
    repeatInterval: asTiming(timings.repeat_interval),
    route: timings.route,
    childRoutes: timings.child_routes,
    childRoutesWithTimings: timings.child_routes_with_timings,
    receiver: timings.receiver ?? null,
    receiverBasis: timings.receiver_basis,
    webhookReceivers: timings.webhook_receivers,
    routes: timings.routes,
    routesAgree: timings.routes_agree,
    routesDropped: timings.routes_dropped,
    observedAt: timings.observed_at ?? null,
    defaultsFromVersion: timings.defaults_from_version ?? null,
    defaultsVerified: timings.defaults_verified,
  };
}

/** The routes that deliver to the receiver oto believes is its own. */
function reachingRoutes(am: AmRef): readonly ReceiverRoute[] {
  return am.routes.filter((r) => r.reaches_oto);
}

/**
 * One route's matcher path, rendered the way an operator's own file reads it.
 *
 * The top-level route contributes `{}` and is dropped from the label when there
 * is anything below it: `route:` is where everybody starts, and repeating it in
 * every line is noise. A route that IS the top-level one keeps it, because "the
 * top-level route" is exactly what it needs to say.
 */
function routeLabel(r: ReceiverRoute): string {
  const steps = r.path.map((s) => `{${s.matchers.join(", ")}}`);
  if (steps.length === 1) return "the top-level route";
  return steps.slice(1).join(" \u203a ");
}

/** Whether any route on the path used the deprecated `match` / `match_re`. */
function usesDeprecatedMatchers(r: ReceiverRoute): boolean {
  return r.path.some((s) => s.deprecated);
}

/** How usable a reference is, for picking which source the guidance argues from. */
function knownCount(am: AmRef): number {
  return [am.groupWait, am.groupInterval, am.repeatInterval].filter(
    (t) => t.provenance !== "unknown",
  ).length;
}

/* -------------------------------------------------------------------------- */
/* Draft state                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Edits are held as text, one entry per touched knob, so a half-typed number is
 * a half-typed number rather than `NaN` or a silent zero. An absent entry means
 * untouched — which is exactly what the partial PATCH means by an omitted key.
 */
type Draft = Readonly<Record<string, string>>;

/** A string array served for `unacked_reminder_mention_list`, read defensively. */
function asStringList(raw: unknown): readonly string[] {
  return Array.isArray(raw) ? raw.filter((x): x is string => typeof x === "string") : [];
}

/* -------------------------------------------------------------------------- */
/* The controller handed to each row                                          */
/* -------------------------------------------------------------------------- */

interface Ctl {
  /** The reference the guidance argues from, or null when no source can supply one. */
  readonly am: () => AmRef | null;
  /** Present in the served payload at all. False for a key the contract dropped. */
  readonly supported: (key: KnobKey) => boolean;
  readonly origin: (key: KnobKey) => SettingOrigin | null;
  /**
   * The env var or file key that owns a `config` knob. Non-null EXACTLY for
   * origin `config` — a badge reading "managed by configuration" with no key
   * beside it tells an operator only that they cannot fix it here.
   */
  readonly configKey: (key: KnobKey) => string | null;
  /**
   * This org's own override where declarative configuration is overriding it.
   * It is still stored and takes effect the moment the config key is removed,
   * which is exactly why it is shown rather than hidden.
   */
  readonly shadowed: (key: KnobKey) => unknown;
  /** True when the deployment's configuration owns this knob: the control is read-only. */
  readonly managed: (key: KnobKey) => boolean;
  readonly bound: (key: KnobKey) => SettingBound | null;
  readonly served: (key: KnobKey) => unknown;
  readonly text: (key: KnobKey) => string;
  readonly setText: (key: KnobKey, next: string) => void;
  readonly num: (key: KnobKey) => number;
  readonly dirty: (key: KnobKey) => boolean;
  readonly resetQueued: (key: KnobKey) => boolean;
  readonly toggleReset: (key: KnobKey) => void;
  readonly revert: (key: KnobKey) => void;
  readonly localError: (key: KnobKey) => string | undefined;
  readonly serverError: (key: KnobKey) => string | undefined;
  readonly busy: () => boolean;
}

/* -------------------------------------------------------------------------- */
/* The screen                                                                 */
/* -------------------------------------------------------------------------- */

export const TuningSection: Component = () => {
  const client = useQueryClient();

  const view = useQuery(() => ({
    queryKey: qk.settings.org(),
    queryFn: ({ signal }: { signal: AbortSignal }) => getOrgSettings({ signal }),
  }));

  // The upstream timings, read per source. This is the query that replaced four
  // number inputs and a localStorage key.
  const sources = useQuery(() => sourcesQuery());

  const refs = createMemo<readonly AmRef[]>(() =>
    (sources.data?.data ?? []).map(amRefOf).filter((r): r is AmRef => r !== null),
  );

  const [basisId, setBasisId] = createSignal<string | null>(null);

  /**
   * Which source the guidance argues from.
   *
   * ⚠️ IT IS ONE SOURCE, NAMED, AND NEVER A BLEND. Two Alertmanagers can batch
   * differently, and averaging them — or silently picking one — would produce a
   * verdict about a cluster the reader is not looking at. The default is the
   * source that can answer the most of the three; the operator can switch, and
   * the panel says which one every verdict below is about.
   */
  const am = (): AmRef | null => {
    const all = refs();
    if (all.length === 0) return null;
    const chosen = all.find((r) => r.sourceId === basisId());
    if (chosen !== undefined) return chosen;
    return [...all].sort((a, b) => knownCount(b) - knownCount(a))[0] ?? null;
  };

  const [edits, setEdits] = createSignal<Draft>({});
  const [resets, setResets] = createSignal<readonly KnobKey[]>([]);
  const [onlyOverrides, setOnlyOverrides] = createSignal(false);
  const [pendingLeave, setPendingLeave] = createSignal<BeforeLeaveEventArgs | null>(null);

  /* ---- reading the served payload -------------------------------------- */

  // The one place the contract is read by string key. `settings` is a closed
  // object in the generated types, but this screen must survive a key arriving
  // (or leaving) under a concurrently-changing contract without failing to
  // compile or rendering a blank control — so it reads the bag and asks
  // `supported()` what is actually there.
  const bag = (): Readonly<Record<string, unknown>> =>
    (view.data?.settings ?? {}) as unknown as Readonly<Record<string, unknown>>;

  const supported = (key: KnobKey): boolean => view.data !== undefined && key in bag();
  const served = (key: KnobKey): unknown => bag()[key];
  const origin = (key: KnobKey): SettingOrigin | null => view.data?.origins[key] ?? null;
  const bound = (key: KnobKey): SettingBound | null => view.data?.bounds[key] ?? null;

  // `config_keys` carries a key ONLY for origin `config`, so its presence is the
  // answer to "can I change this here, and if not, where?".
  const configKey = (key: KnobKey): string | null => view.data?.config_keys[key] ?? null;
  const managed = (key: KnobKey): boolean => origin(key) === "config";
  const shadowed = (key: KnobKey): unknown =>
    (view.data?.shadowed as Readonly<Record<string, unknown>> | undefined)?.[key];

  const servedText = (key: KnobKey): string => {
    const raw = served(key);
    const knob = KNOBS[key];
    if (knob.kind === "boolean") return raw === true ? "true" : "false";
    if (knob.kind === "mentionList") return JSON.stringify(asStringList(raw));
    return raw === undefined || raw === null ? "" : String(raw);
  };

  const text = (key: KnobKey): string => edits()[key] ?? servedText(key);

  const setText = (key: KnobKey, next: string): void => {
    // Typing in a knob withdraws a queued reset for it: the two are opposite
    // intentions and silently keeping both would send a value AND a reset for
    // the same key in one request.
    setResets((prev) => prev.filter((k) => k !== key));
    setEdits((prev) => ({ ...prev, [key]: next }));
  };

  const num = (key: KnobKey): number => {
    const n = Number.parseInt(text(key), 10);
    return Number.isFinite(n) ? n : Number.NaN;
  };

  const parseFor = (key: KnobKey, raw: string): unknown => {
    const knob = KNOBS[key];
    if (isNumeric(knob.kind)) {
      const trimmed = raw.trim();
      if (trimmed === "" || !/^-?\d+$/.test(trimmed)) return undefined;
      return Number.parseInt(trimmed, 10);
    }
    if (knob.kind === "boolean") return raw === "true";
    if (knob.kind === "mentionList") {
      try {
        return asStringList(JSON.parse(raw));
      } catch {
        return undefined;
      }
    }
    return raw;
  };

  const same = (a: unknown, b: unknown): boolean =>
    typeof a === "object" || typeof b === "object" ? JSON.stringify(a) === JSON.stringify(b) : a === b;

  const dirty = (key: KnobKey): boolean => {
    if (resets().includes(key)) return true;
    const e = edits()[key];
    if (e === undefined) return false;
    const parsed = parseFor(key, e);
    if (parsed === undefined) return true;
    return !same(parsed, served(key));
  };

  const resetQueued = (key: KnobKey): boolean => resets().includes(key);

  const toggleReset = (key: KnobKey): void => {
    setResets((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
    // A queued reset and a pending edit are contradictory, so queueing one
    // discards the other rather than leaving the operator to guess which wins.
    setEdits((prev) => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  };

  const revert = (key: KnobKey): void => {
    setResets((prev) => prev.filter((k) => k !== key));
    setEdits((prev) => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  };

  /* ---- validation, against the served bounds only ----------------------- */

  const localError = (key: KnobKey): string | undefined => {
    const raw = edits()[key];
    if (raw === undefined) return undefined;
    const knob = KNOBS[key];

    if (isNumeric(knob.kind)) {
      const trimmed = raw.trim();
      if (trimmed === "") return "Enter a number. To follow oto's default instead, use Reset.";
      if (!/^-?\d+$/.test(trimmed)) return "Whole numbers only.";
      const n = Number.parseInt(trimmed, 10);
      const b = bound(key);
      if (b === null) return undefined;
      // 0 reads as "unset" but is not writable. The way back to unset is the
      // reset mechanism, and saying so is more useful than repeating the range.
      if (knob.zeroIsUnset === true && n === 0) {
        return same(0, served(key))
          ? undefined
          : "0 reads as unset but cannot be written — a reminder delay of zero seconds is not a delay. Use Reset to remove this org's default instead.";
      }
      if (n < b.min || n > b.max) {
        return `oto accepts ${b.min}–${b.max} for this. ${b.why}`;
      }
      return undefined;
    }

    if (knob.kind === "mentionList") {
      const parsed = parseFor(key, raw);
      if (parsed === undefined) return "That mention list could not be read.";
      const list = parsed as readonly string[];
      // The cap is the server's and it says so in its own 422; stating it here
      // saves a round trip. The *shape* of an entry is deliberately not
      // re-implemented — that rule lives on the server and its message lands on
      // this field, so a copy here could only drift out of agreement with it.
      if (list.length > MENTION_LIST_MAX) {
        return `At most ${MENTION_LIST_MAX} entries — beyond that a reminder is a page, and oto pages nobody.`;
      }
      return undefined;
    }

    return undefined;
  };

  /* ---- the write --------------------------------------------------------- */

  const patch = (): UpdateOrgSettingsRequest => {
    const body: Record<string, unknown> = {};
    for (const key of Object.keys(KNOBS) as KnobKey[]) {
      if (!supported(key)) continue;
      // ⛔ A CONFIG-MANAGED KEY IS NEVER IN THE BODY. The control is read-only,
      // so this should be unreachable; it is belt as well as braces, because the
      // server answers 409 for the whole write and one stray key would refuse the
      // other nine changes with it.
      if (managed(key)) continue;
      if (resets().includes(key)) continue;
      const raw = edits()[key];
      if (raw === undefined) continue;
      const parsed = parseFor(key, raw);
      if (parsed === undefined) continue;
      if (same(parsed, served(key))) continue;
      body[key] = parsed;
    }
    // A reset is refused for a managed key too, and for the same reason: "return
    // this to oto's default" cannot happen while configuration is forcing a
    // value, and unlike a write a reset would also destroy the shadowed override
    // underneath — the one value that comes back if the config key is removed.
    const queued = resets().filter((k) => supported(k) && !managed(k));
    if (queued.length > 0) body["reset"] = [...queued];
    return body as UpdateOrgSettingsRequest;
  };

  const save = useMutation(() => ({
    mutationFn: () => updateOrgSettings(patch()),
    onSuccess: (next: OrgSettingsView) => {
      // The response IS the new view, origins and bounds included, so the screen
      // reflects the write without a second round trip and without a moment in
      // which the badges disagree with the values.
      client.setQueryData(qk.settings.org(), next);
      setEdits({});
      setResets([]);
    },
  }));

  const serverViolations = createMemo(() => violationsByField(save.error));
  const serverError = (key: KnobKey): string | undefined => serverViolations().get(key);
  const orphans = createMemo(() => orphanViolations(save.error, Object.keys(KNOBS)));

  const dirtyKeys = createMemo(() =>
    (Object.keys(KNOBS) as KnobKey[]).filter((k) => supported(k) && dirty(k)),
  );
  const blockedKeys = createMemo(() =>
    (Object.keys(KNOBS) as KnobKey[]).filter((k) => supported(k) && localError(k) !== undefined),
  );
  const overrideKeys = createMemo(() =>
    (Object.keys(KNOBS) as KnobKey[]).filter((k) => supported(k) && origin(k) === "org"),
  );
  const managedKeys = createMemo(() => (Object.keys(KNOBS) as KnobKey[]).filter(managed));
  const shadowedKeys = createMemo(() =>
    (Object.keys(KNOBS) as KnobKey[]).filter((k) => shadowed(k) !== undefined),
  );
  const knownKeys = createMemo(() => (Object.keys(KNOBS) as KnobKey[]).filter(supported));

  /* ---- unsaved-change awareness ----------------------------------------- */

  // In-app navigation is intercepted and offered a real choice. A half-edited
  // form that vanishes because someone clicked "Alerts" is a small betrayal, and
  // on this screen the lost edit is a change to how loud the product is.
  useBeforeLeave((e) => {
    if (dirtyKeys().length === 0 || e.defaultPrevented) return;
    e.preventDefault();
    setPendingLeave(e);
  });

  onMount(() => {
    const onUnload = (e: BeforeUnloadEvent): void => {
      if (dirtyKeys().length > 0) e.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    onCleanup(() => window.removeEventListener("beforeunload", onUnload));
  });

  const ctl: Ctl = {
    am,
    supported,
    origin,
    configKey,
    shadowed,
    managed,
    bound,
    served,
    text,
    setText,
    num,
    dirty,
    resetQueued,
    toggleReset,
    revert,
    localError,
    serverError,
    busy: () => save.isPending,
  };

  return (
    <div class="flex flex-col gap-4 pb-24">
      <AlertmanagerPanel
        sources={refs()}
        basis={am()}
        pending={sources.isPending}
        error={sources.isError ? sources.error : null}
        onRetry={() => void sources.refetch()}
        onChooseBasis={setBasisId}
      />

      <Switch>
        <Match when={view.isPending}>
          <TuningSkeleton />
        </Match>

        <Match when={view.isError}>
          <Panel>
            <ErrorState error={view.error} onRetry={() => void view.refetch()} />
          </Panel>
        </Match>

        <Match when={view.data !== undefined}>
          <OriginSummary
            total={knownKeys().length}
            overrides={overrideKeys().length}
            managed={managedKeys().length}
            shadowed={shadowedKeys().length}
            onlyOverrides={onlyOverrides()}
            setOnlyOverrides={setOnlyOverrides}
          />

          <Show when={save.error !== null}>
            <ErrorBanner error={save.error}>
              <Switch>
                {/*
                  ⛔ THE 409 SHOULD BE UNREACHABLE FROM THIS SCREEN — every
                  config-managed control is read-only and its key is excluded
                  from the body. If it happens anyway, the deployment's
                  configuration changed under the open tab, and the useful
                  answer is WHICH KEY to go and edit, not "the request failed".
                */}
                <Match when={managedByConfig(save.error)}>
                  <div class="flex flex-col gap-1">
                    <span class="font-medium">
                      This deployment's configuration owns one of these settings, so nothing was
                      changed.
                    </span>
                    <span class="text-ink-muted">
                      Configuration beats an org override on purpose: without it somebody edits a
                      value here, the next deploy reverts it, and nobody can work out why. Change it
                      where it is set, or remove that key to hand the setting back to this org.
                    </span>
                    <For each={managedViolations(save.error)}>
                      {(v) => (
                        <span class="text-ink">
                          <code class="font-mono text-meta">{v.field}</code> — {v.message}
                        </span>
                      )}
                    </For>
                    <Button
                      size="sm"
                      variant="secondary"
                      class="mt-1 self-start"
                      onClick={() => void view.refetch()}
                    >
                      Reload what is in force
                    </Button>
                  </div>
                </Match>
                <Match when={true}>
                  <div class="flex flex-col gap-1">
                    <span class="font-medium">oto refused the write, and nothing was changed.</span>
                    <span class="text-ink-muted">
                      The bounds are enforced on the server against the merged state, so a value can
                      be refused even though every field on this screen looked legal on its own.
                    </span>
                    <For each={orphans()}>{(o) => <span class="text-ink-muted">{o}</span>}</For>
                  </div>
                </Match>
              </Switch>
            </ErrorBanner>
          </Show>

          <For each={KNOB_GROUPS}>
            {(group) => {
              // A SHADOWED key counts as "this org has changed something": the
              // org wrote it, it is still stored, and hiding it behind a filter
              // labelled "what this org has changed" is how somebody spends an
              // afternoon on a number they can see in the database and never in
              // force.
              const keys = (): readonly KnobKey[] =>
                group.keys.filter(
                  (k) =>
                    supported(k) &&
                    (!onlyOverrides() ||
                      origin(k) === "org" ||
                      shadowed(k) !== undefined ||
                      dirty(k)),
                );
              return (
                <Show when={keys().length > 0}>
                  <Panel>
                    <PanelHeader class="flex-col items-start gap-0.5">
                      <PanelTitle>{group.title}</PanelTitle>
                      <p class="text-meta leading-snug text-ink-muted">{group.blurb}</p>
                    </PanelHeader>
                    <ul>
                      <For each={keys()}>{(key) => <KnobRow knob={KNOBS[key]} ctl={ctl} />}</For>
                    </ul>
                  </Panel>
                </Show>
              );
            }}
          </For>

          <Show
            when={
              onlyOverrides() &&
              overrideKeys().length === 0 &&
              shadowedKeys().length === 0 &&
              dirtyKeys().length === 0
            }
          >
            <Panel class="px-3 py-8 text-center">
              <p class="text-item font-medium text-ink">This org has changed nothing.</p>
              <p class="mx-auto mt-1 max-w-md text-body leading-relaxed text-ink-muted">
                Every value in force is oto's shipped default, and each will follow that default if
                oto moves it. That is a legitimate state, not an empty one.
              </p>
            </Panel>
          </Show>
        </Match>
      </Switch>

      <SaveBar
        dirty={dirtyKeys().length}
        blocked={blockedKeys().length}
        resets={resets().length}
        busy={save.isPending}
        onSave={() => save.mutate()}
        onDiscard={() => {
          setEdits({});
          setResets([]);
          save.reset();
        }}
      />

      <LeaveGuard
        pending={pendingLeave()}
        count={dirtyKeys().length}
        onStay={() => setPendingLeave(null)}
        onLeave={() => {
          const e = pendingLeave();
          setPendingLeave(null);
          setEdits({});
          setResets([]);
          e?.retry(true);
        }}
      />
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* The Alertmanager reference panel                                           */
/* -------------------------------------------------------------------------- */

/**
 * The three provenance states, rendered so they can never be confused.
 *
 * `observed` is the operator's own line in `alertmanager.yml`. `default_applies`
 * is Alertmanager's documented value governing because no such line exists — the
 * number is real and the arithmetic below is valid, but there is nothing to edit
 * upstream. `unknown` carries no number at all, and is the only state that
 * withdraws guidance rather than qualifying it.
 */
const ProvenanceBadge: Component<{ readonly provenance: TimingProvenance }> = (props) => {
  const copy = (): { label: string; title: string; tone: string } => {
    switch (props.provenance) {
      case "observed":
        return {
          label: "from your config",
          title:
            "This value is stated in your configuration, on the route named just below these three numbers or on one of its parents. Changing it means editing alertmanager.yml.",
          tone: "border-accent-border bg-accent-fill font-semibold text-ink",
        };
      case "default_applies":
        return {
          label: "Alertmanager default",
          title:
            "Your configuration states nothing for this, so Alertmanager's documented default governs. It is applied in dispatch.NewRoute, which is why the status endpoint does not publish it. The arithmetic below is valid — there is simply no line in alertmanager.yml to change.",
          tone: "border-line-strong bg-raised text-ink",
        };
      default:
        return {
          label: "unknown",
          title:
            "oto could not read or parse this source's configuration, so it cannot say what governs — not even that a default applies. Every verdict that depends on this number is withheld.",
          tone: "border-line bg-sunken text-ink-subtle",
        };
    }
  };

  return (
    <span
      class={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-chip border px-1.5 py-px text-meta leading-4",
        copy().tone,
      )}
      title={copy().title}
    >
      {copy().label}
    </span>
  );
};

/** One timing cell: the number, then where the number came from. */
const TimingCell: Component<{ readonly field: AmFieldCopy; readonly am: AmRef }> = (props) => {
  const timing = (): AmTiming => props.am[props.field.key];
  return (
    <div class="flex min-w-0 flex-col gap-1">
      <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <code class="font-mono text-meta text-ink-muted">{props.field.label}</code>
        <span class="text-item font-medium text-ink tabular-nums">
          {timing().provenance === "unknown" ? "—" : duration(timing().seconds)}
        </span>
        <ProvenanceBadge provenance={timing().provenance} />
      </div>
      <p class="text-meta leading-snug text-ink-subtle">{props.field.why}</p>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Where the three numbers actually came from                                 */
/* -------------------------------------------------------------------------- */

/** One per-route duration, rendered from milliseconds. */
function routeDuration(t: InheritedTiming): string {
  return t.value_ms === null ? "\u2014" : duration(t.value_ms / 1000);
}

/**
 * One resolved route: its matchers, its receiver, and its three timings with the
 * depth each was stated at.
 *
 * ⭐ INHERITED AND STATED ARE SHOWN DIFFERENTLY ON PURPOSE. `group_interval: 5m`
 * inherited from the top-level route and `group_interval: 5m` written on this
 * route are the same number and two different lines of the operator's file, and
 * only the second survives them editing the child.
 */
const RouteRow: Component<{ readonly route: ReceiverRoute; readonly ownDepth: number }> = (
  props,
) => {
  const own = (t: InheritedTiming): boolean => t.from_depth === props.ownDepth;
  const cell = (label: string, t: InheritedTiming): JSX.Element => (
    <span class="whitespace-nowrap">
      <code class="font-mono text-micro text-ink-subtle">{label}</code>{" "}
      <span class={cn("tabular-nums", own(t) ? "font-medium text-ink" : "text-ink-muted")}>
        {routeDuration(t)}
      </span>
      <Show when={t.provenance === "default_applies"}>
        <span class="text-ink-subtle" title="No route on this path states it, so Alertmanager's documented default governs. There is no line in alertmanager.yml to change.">
          {" "}
          (default)
        </span>
      </Show>
      <Show when={t.provenance !== "default_applies" && !own(t)}>
        <span class="text-ink-subtle" title="Inherited from a parent route, not stated on this one.">
          {" "}
          (inherited)
        </span>
      </Show>
    </span>
  );

  return (
    <li
      class={cn(
        "flex flex-col gap-1 border-b border-line px-2 py-1.5 last:border-b-0",
        props.route.reaches_oto && "bg-accent-fill/40",
        props.route.unreachable && "opacity-60",
      )}
    >
      <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <Show
          when={props.route.path.length > 1}
          fallback={<span class="text-meta font-medium text-ink">the top-level route</span>}
        >
          <code class="font-mono text-meta text-ink">{routeLabel(props.route)}</code>
        </Show>
        <span class="text-meta text-ink-muted">
          &rarr; <span class="font-medium text-ink">{props.route.receiver}</span>
        </span>
        <Show when={props.route.reaches_oto}>
          <span class="rounded-chip border border-accent-border bg-accent-fill px-1 text-micro leading-4 text-ink">
            reaches oto
          </span>
        </Show>
        <Show when={props.route.path.at(-1)?.continue === true}>
          <span
            class="rounded-chip border border-line-strong px-1 text-micro leading-4 text-ink-muted"
            title="continue: true — evaluation does not stop here, so later sibling routes are still considered. This is why more than one route can reach the same receiver."
          >
            continue
          </span>
        </Show>
        <Show when={usesDeprecatedMatchers(props.route)}>
          <span
            class="rounded-chip border border-line-strong px-1 text-micro leading-4 text-ink-muted"
            title="Written with the deprecated match / match_re keys. Still routing, still read by oto; shown here in the current matchers spelling."
          >
            match/match_re
          </span>
        </Show>
        <Show when={props.route.group_by_all}>
          <span
            class="rounded-chip border border-line-strong px-1 text-micro leading-4 text-ink-muted"
            title="group_by: ['...'] groups by every label, so no group ever accumulates a second member and storm collapse is unreachable at any threshold. That fix is in alertmanager.yml, not on this screen."
          >
            group_by: ...
          </span>
        </Show>
      </div>
      <div class="flex flex-wrap gap-x-3 gap-y-0.5 text-meta">
        {cell("group_wait", props.route.group_wait)}
        {cell("group_interval", props.route.group_interval)}
        {cell("repeat_interval", props.route.repeat_interval)}
      </div>
      <Show when={props.route.unreachable}>
        <p class="text-micro leading-snug text-ink-muted">
          <span class="font-medium text-ink">Unreachable. </span>
          An earlier sibling route states no matchers and no{" "}
          <code class="font-mono text-micro">continue</code>, so it takes everything before
          evaluation gets this far. Nothing can ever match here.
        </p>
      </Show>
    </li>
  );
};

/** The resolved tree, listed. Collapsed by default: calm unless it matters. */
const RouteList: Component<{
  readonly am: AmRef;
  readonly openByDefault: boolean;
}> = (props) => {
  const [open, setOpen] = createSignal(props.openByDefault);
  return (
    <div class="mt-2">
      <Show
        when={open()}
        fallback={
          <Button size="sm" variant="ghost" onClick={() => setOpen(true)}>
            Show all {props.am.routes.length} route{props.am.routes.length === 1 ? "" : "s"}
          </Button>
        }
      >
        <ul class="rounded-control border border-line">
          <For each={props.am.routes}>
            {(r) => <RouteRow route={r} ownDepth={r.path.length - 1} />}
          </For>
        </ul>
        <Show when={props.am.routesDropped > 0}>
          <p class="mt-1 text-micro leading-snug text-ink-muted">
            {props.am.routesDropped} more route{props.am.routesDropped === 1 ? "" : "s"} exist and
            were not read: this configuration is past the size oto resolves, so the list above is
            not the whole tree.
          </p>
        </Show>
        <Show when={!props.openByDefault}>
          <Button size="sm" variant="ghost" class="mt-1" onClick={() => setOpen(false)}>
            Hide routes
          </Button>
        </Show>
      </Show>
    </div>
  );
};

/**
 * WHICH ROUTE the three numbers above came from — the fact that used to be a
 * caveat and is now the answer.
 *
 * ⛔ THIS BLOCK IS THE HONESTY OF THE WHOLE SCREEN. The three timings are
 * per-route and inherited, so the numbers governing the alerts oto is sent are
 * the ones on the route delivering to oto's receiver. Where oto can name that
 * route it says so and shows the matchers. Where it cannot — Alertmanager
 * redacts the webhook URL that would identify oto's receiver, so several webhook
 * receivers are genuinely indistinguishable — it says THAT, shows every route,
 * and claims none. And where several routes reach oto and disagree, it shows the
 * disagreement rather than picking a side, because there is no single answer and
 * inventing one is exactly what the old hand-typed form did.
 */
const RouteOrigin: Component<{ readonly am: AmRef }> = (props) => {
  const reaching = (): readonly ReceiverRoute[] => reachingRoutes(props.am);
  const basis = (): (typeof RECEIVER_BASIS_COPY)[keyof typeof RECEIVER_BASIS_COPY] =>
    RECEIVER_BASIS_COPY[props.am.receiverBasis];

  return (
    <div class="mt-2 rounded-control border border-line px-2 py-1.5 text-meta leading-snug text-ink-muted">
      <Switch>
        {/* The answer. One route reaches oto, or several that agree. */}
        <Match when={props.am.route === "oto_receiver"}>
          <p>
            <span class="font-medium text-ink">
              Read from the route your oto receiver is attached to.
            </span>{" "}
            <Show
              when={reaching().length === 1}
              fallback={
                <>
                  {reaching().length} routes deliver to{" "}
                  <span class="font-medium text-ink">{props.am.receiver}</span> and they agree on
                  all three, so the numbers above describe every one of them.
                </>
              }
            >
              <code class="font-mono text-meta text-ink">{routeLabel(reaching()[0]!)}</code>{" "}
              delivers to <span class="font-medium text-ink">{props.am.receiver}</span>, and the
              three numbers above are that route&apos;s — everything it inherits from its parents
              included.
            </Show>{" "}
            oto identified that receiver because this configuration has {basis().label}.
          </p>
        </Match>

        {/*
          ⚠️ THE DISAGREEMENT CASE. Alertmanager evaluates children in order and
          `continue: true` does not stop it, so two routes can both deliver to
          oto under different matchers with different timings. There is no single
          triple; the guidance above is the TOP-LEVEL route's and describes
          neither of them.
        */}
        <Match when={!props.am.routesAgree}>
          <p>
            <span class="font-medium text-ink">
              {reaching().length} routes reach your oto receiver and they disagree.
            </span>{" "}
            They state different timings, so there is no single set of numbers for oto&apos;s
            traffic. The three above are the <span class="font-medium">top-level route&apos;s</span>{" "}
            and describe none of them — treat every verdict below as approximate until these routes
            agree or you know which one your alerts take.
          </p>
        </Match>

        {/* Identified a receiver, but nothing delivers to it. */}
        <Match when={props.am.receiver !== null && reaching().length === 0}>
          <p>
            <span class="font-medium text-ink">
              No route delivers to {props.am.receiver}.
            </span>{" "}
            oto reads that as its own receiver ({basis().label}), but every alert is consumed by a
            route pointing somewhere else — so this Alertmanager is not sending oto anything. The
            numbers above are the top-level route&apos;s.
          </p>
        </Match>

        {/* Cannot tell which receiver is oto's. Show everything, claim nothing. */}
        <Match when={props.am.receiverBasis === "ambiguous"}>
          <p>
            <span class="font-medium text-ink">
              oto cannot tell which of these receivers is its own.
            </span>{" "}
            {basis().detail}{" "}
            <span class="text-ink">
              Candidates:{" "}
              <For each={props.am.webhookReceivers}>
                {(name, i) => (
                  <>
                    <code class="font-mono text-meta">{name}</code>
                    {i() < props.am.webhookReceivers.length - 1 ? ", " : ""}
                  </>
                )}
              </For>
              .
            </span>{" "}
            The numbers above are the top-level route&apos;s — what governs every alert matching
            nothing more specific.
          </p>
        </Match>

        <Match when={props.am.receiverBasis === "no_webhook"}>
          <p>
            <span class="font-medium text-ink">No receiver here can push to oto.</span>{" "}
            {basis().detail} The numbers above are the top-level route&apos;s.
          </p>
        </Match>

        <Match when={true}>
          <p>{basis().detail}</p>
        </Match>
      </Switch>

      <Show when={props.am.routes.length > 0}>
        <RouteList
          am={props.am}
          openByDefault={!props.am.routesAgree || props.am.receiverBasis === "ambiguous"}
        />
      </Show>
    </div>
  );
};

/** One source's three timings, with its caveats. */
const SourceTimings: Component<{
  readonly am: AmRef;
  readonly isBasis: boolean;
  readonly onChoose: () => void;
}> = (props) => (
  <li class={cn("border-b border-line px-3 py-3 last:border-b-0", props.isBasis && "bg-accent-fill/40")}>
    <div class="flex flex-wrap items-center justify-between gap-2">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-item font-medium text-ink">{props.am.sourceName}</span>
        <Show when={props.isBasis}>
          <span class="rounded-chip border border-accent-border bg-accent-fill px-1.5 py-px text-meta font-semibold leading-4 text-ink">
            guidance below uses this source
          </span>
        </Show>
      </div>
      <div class="flex items-center gap-2 text-meta text-ink-subtle">
        <Show
          when={props.am.observedAt !== null}
          fallback={<span>never read</span>}
        >
          <span>
            read <RelativeTime value={props.am.observedAt} label="Configuration last read" />
          </span>
        </Show>
        <Show when={!props.isBasis}>
          <Button size="sm" variant="ghost" onClick={props.onChoose}>
            Use this source
          </Button>
        </Show>
      </div>
    </div>

    <div class="mt-2 grid gap-3 md:grid-cols-3">
      <For each={AM_FIELDS}>{(f) => <TimingCell field={f} am={props.am} />}</For>
    </div>

    <RouteOrigin am={props.am} />

    <Show when={props.am.defaultsFromVersion !== null}>
      <p class="mt-2 text-meta leading-snug text-ink-subtle">
        Defaulted values are Alertmanager{" "}
        <span class="font-mono">{props.am.defaultsFromVersion}</span>&apos;s documented 30s / 5m / 4h.
        <Show when={!props.am.defaultsVerified}>
          {" "}
          <span class="text-ink-muted">
            oto has not verified those constants for this release — they are upstream values oto
            copies, and this source is newer than the one they were last checked against.
          </span>
        </Show>
      </p>
    </Show>
  </li>
);

const AlertmanagerPanel: Component<{
  readonly sources: readonly AmRef[];
  readonly basis: AmRef | null;
  readonly pending: boolean;
  readonly error: unknown;
  readonly onRetry: () => void;
  readonly onChooseBasis: (id: string) => void;
}> = (props) => (
  <Panel>
    <PanelHeader class="flex-col items-start gap-0.5">
      <PanelTitle>Your Alertmanager</PanelTitle>
      <p class="text-meta leading-snug text-ink-muted">
        Read from each source&apos;s own running configuration on the status call oto already makes.
        Every duration below this panel is a multiple of these, not an absolute time.
      </p>
    </PanelHeader>

    <Switch>
      <Match when={props.pending}>
        <div class="flex flex-col gap-2 px-3 py-3">
          <Skeleton class="h-2.5 w-40" />
          <Skeleton class="h-2 w-full" />
        </div>
      </Match>

      <Match when={props.error !== null}>
        <div class="px-3 py-3">
          <ErrorState error={props.error} onRetry={props.onRetry} />
        </div>
      </Match>

      <Match when={props.sources.length === 0}>
        <div class="px-3 py-6 text-center">
          <p class="text-item font-medium text-ink">No source has been read yet.</p>
          <p class="mx-auto mt-1 max-w-lg text-body leading-relaxed text-ink-muted">
            Every duration on this screen is a multiple of an Alertmanager&apos;s{" "}
            <code class="font-mono text-meta">group_wait</code>,{" "}
            <code class="font-mono text-meta">group_interval</code> and{" "}
            <code class="font-mono text-meta">repeat_interval</code>. Add a source under Sources
            and clusters, and oto reads all three off its published configuration — the guidance
            below turns on by itself. It is deliberately not something you can type in here: a
            number entered by hand is unshared, unchecked, and wrong the moment somebody edits{" "}
            <code class="font-mono text-meta">alertmanager.yml</code>.
          </p>
        </div>
      </Match>

      <Match when={true}>
        <ul>
          <For each={props.sources}>
            {(ref) => (
              <SourceTimings
                am={ref}
                isBasis={props.basis?.sourceId === ref.sourceId}
                onChoose={() => props.onChooseBasis(ref.sourceId)}
              />
            )}
          </For>
        </ul>
      </Match>
    </Switch>

    <div class="border-t border-line bg-raised px-3 py-2">
      <p class="text-meta leading-snug text-ink-subtle">
        oto does not read your rule files, so every verdict that depends on a rule&apos;s{" "}
        <code class="font-mono text-ink-muted">for:</code> assumes{" "}
        {duration(ASSUMED_RULE_FOR_S)} and says so where it is used. And one thing no number
        captures: if your <code class="font-mono text-ink-muted">group_by</code> contains a
        per-replica label such as <code class="font-mono text-ink-muted">instance</code> or{" "}
        <code class="font-mono text-ink-muted">pod</code>, storm collapse is unreachable at any
        threshold, because no group ever accumulates members. That fix is in{" "}
        <code class="font-mono text-ink-muted">alertmanager.yml</code>, not on this screen.
      </p>
    </div>
  </Panel>
);

/* -------------------------------------------------------------------------- */
/* Origin summary                                                             */
/* -------------------------------------------------------------------------- */

const OriginSummary: Component<{
  readonly total: number;
  readonly overrides: number;
  readonly managed: number;
  readonly shadowed: number;
  readonly onlyOverrides: boolean;
  readonly setOnlyOverrides: (next: boolean) => void;
}> = (props) => (
  <div class="flex flex-wrap items-center justify-between gap-3 rounded-surface border border-line bg-raised px-3 py-2">
    <p class="text-body leading-snug text-ink">
      <span class="font-medium">
        {props.overrides} of {props.total}
      </span>{" "}
      {props.overrides === 1 ? "value is" : "values are"} this org&apos;s own. The rest are oto&apos;s
      shipped defaults and will follow them if oto moves them.
      <Show when={props.managed > 0}>
        {" "}
        <span class="font-medium">
          {props.managed} {props.managed === 1 ? "is" : "are"} set by this deployment&apos;s
          configuration
        </span>{" "}
        and cannot be changed here.
      </Show>
      <Show when={props.shadowed > 0}>
        {" "}
        <span class="font-medium">
          {props.shadowed} of this org&apos;s overrides{" "}
          {props.shadowed === 1 ? "is" : "are"} being overridden
        </span>{" "}
        by that configuration — still stored, and back in force the moment the config key goes.
      </Show>
    </p>
    <div class="flex items-center gap-1.5">
      <Checkbox
        id="tuning-only-overrides"
        checked={props.onlyOverrides}
        onChange={props.setOnlyOverrides}
      />
      <label for="tuning-only-overrides-input" class="text-body text-ink">
        Show only what this org has changed
      </label>
    </div>
  </div>
);

/* -------------------------------------------------------------------------- */
/* The 409 this screen should never be able to trigger                        */
/* -------------------------------------------------------------------------- */

/**
 * `409 setting_managed_by_config`.
 *
 * Every managed control on this screen is read-only and its key is stripped from
 * the body, so reaching this means the deployment's configuration changed while
 * the tab was open. The recovery is to say WHICH KEY owns the value — the server
 * puts it in a violation per key — rather than to render a generic failure and
 * leave the operator hunting through a Helm chart.
 */
function managedByConfig(err: unknown): boolean {
  return err instanceof ApiError && err.code === "setting_managed_by_config";
}

function managedViolations(err: unknown): readonly { field: string; message: string }[] {
  if (!(err instanceof ApiError)) return [];
  return err.violations.map((v) => ({ field: v.field, message: v.message }));
}

/* -------------------------------------------------------------------------- */
/* One knob                                                                   */
/* -------------------------------------------------------------------------- */

/**
 * Where the value in force came from: oto's default, this org, or this
 * deployment's configuration.
 *
 * ⭐ `config` IS NOT A THIRD SHADE OF "override". It beats an org override, it
 * cannot be changed here, and the key that owns it is named on the badge —
 * because "managed by configuration" with no key beside it tells an operator only
 * that they cannot fix it, and turns a five-second edit into an archaeology
 * exercise across a Helm chart, a values file and a Deployment's env block.
 */
const OriginBadge: Component<{
  readonly origin: SettingOrigin | null;
  readonly configKey: string | null;
}> = (props) => {
  const copy = (): { label: string; title: string; tone: string; dot: string } => {
    switch (props.origin) {
      case "config":
        return {
          label: "managed by configuration",
          title:
            `This deployment's configuration sets this value${props.configKey === null ? "" : ` through ${props.configKey}`}. ` +
            "It beats an org override on purpose: without that, somebody edits a value here, the next deploy reverts it, and nobody can work out why it changed back.",
          tone: "border-line-strong bg-raised font-semibold text-ink",
          dot: "bg-ink-muted",
        };
      case "org":
        return {
          label: "override",
          title:
            "This org wrote this value. It will not follow oto's shipped default if that moves.",
          tone: "border-accent-border bg-accent-fill font-semibold text-ink",
          dot: "bg-accent",
        };
      default:
        return {
          label: "oto default",
          title: "oto's shipped default is in force. This org has never written this key.",
          tone: "border-line bg-surface text-ink-subtle",
          dot: "border border-line-strong",
        };
    }
  };

  return (
    <span
      class={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-chip border px-1.5 py-px text-meta leading-4",
        copy().tone,
      )}
      title={copy().title}
    >
      <span aria-hidden="true" class={cn("size-1.5 rounded-full", copy().dot)} />
      {copy().label}
      <Show when={props.origin === "config" && props.configKey !== null}>
        <code class="font-mono text-micro text-ink-muted">{props.configKey}</code>
      </Show>
    </span>
  );
};

const Note: Component<{ readonly kind: "warn" | "quiet"; readonly children: JSX.Element }> = (
  props,
) => (
  // Tier A only (SPEC §M.2). A warning on a settings screen is not an alert
  // state, so it signals with a border, weight and a word — never with a state
  // hue. Spending a saturated colour here is what makes a firing row stop
  // reading as urgent.
  <p
    class={cn(
      "rounded-control border px-2 py-1 text-meta leading-snug",
      props.kind === "warn"
        ? "border-line-strong bg-raised font-medium text-ink"
        : "border-line bg-sunken text-ink-muted",
    )}
  >
    {props.children}
  </p>
);

/**
 * A shadowed override, rendered in the same units as the value in force.
 *
 * It is deliberately not a bare number: "900" and "15m" are the same fact and
 * only one of them can be compared at a glance with the 600s beside it.
 */
function shadowedText(knob: KnobCopy, raw: unknown): string {
  if (typeof raw === "number") return readValue(knob.kind, raw);
  if (typeof raw === "boolean") return raw ? "on" : "off";
  if (Array.isArray(raw)) return JSON.stringify(raw);
  return String(raw);
}

/** One string-valued option, shared by every enum knob's `options`. */
interface KnobOption {
  readonly value: string;
  readonly label: string;
}

/**
 * One knob rendered as a Kobalte listbox, over the label/value pairs
 * `tuningCopy.ts` derives from the contract's own enum — `VERBOSITY_OPTIONS`,
 * `MENTION_MODE_OPTIONS`, `SEVERITY_OPTIONS`. Kobalte's `Select` is driven by
 * an `options` array and an `itemComponent`, not by JSX `<option>` children,
 * so the selected option is looked up by value rather than handed the raw
 * string the rest of this screen's state holds.
 */
const KnobSelect: Component<{
  readonly id: string;
  readonly options: readonly KnobOption[];
  readonly value: string;
  readonly disabled: boolean;
  readonly error: string | undefined;
  readonly onChange: (next: string) => void;
}> = (props) => (
  <Select<KnobOption>
    options={[...props.options]}
    optionValue="value"
    optionTextValue="label"
    value={props.options.find((o) => o.value === props.value) ?? null}
    disabled={props.disabled}
    validationState={props.error !== undefined ? "invalid" : "valid"}
    onChange={(opt) => {
      if (opt !== null) props.onChange(opt.value);
    }}
    itemComponent={(itemProps) => (
      <SelectItem item={itemProps.item}>{itemProps.item.rawValue.label}</SelectItem>
    )}
  >
    <SelectLabel>Value</SelectLabel>
    <SelectTrigger id={props.id}>
      <SelectValue<KnobOption>>{(state) => state.selectedOption()?.label}</SelectValue>
    </SelectTrigger>
    <SelectContent />
    <SelectErrorMessage role="alert">{props.error}</SelectErrorMessage>
  </Select>
);

const KnobRow: Component<{ readonly knob: KnobCopy; readonly ctl: Ctl }> = (props) => {
  const key = (): KnobKey => props.knob.key;
  const ctl = (): Ctl => props.ctl;
  const id = (): string => `knob-${key()}`;

  const numeric = (): boolean => isNumeric(props.knob.kind);
  const b = (): SettingBound | null => ctl().bound(key());
  const servedNum = (): number => {
    const raw = ctl().served(key());
    return typeof raw === "number" ? raw : Number.NaN;
  };

  /**
   * The contract says out-of-range values are clamped on read, so this should be
   * unreachable. If it is ever reached, the value is shown exactly as served —
   * silently correcting it would hide the one fact worth knowing.
   */
  const outsideBounds = (): boolean => {
    const bound = b();
    if (bound === null || !Number.isFinite(servedNum())) return false;
    // The published bounds are the *write* bounds. A key whose read domain
    // includes a sentinel the write schema excludes is not out of bounds.
    if (props.knob.zeroIsUnset === true && servedNum() === 0) return false;
    return servedNum() < bound.min || servedNum() > bound.max;
  };

  /**
   * A stored value that sat outside a bound is clamped on read, and the API does
   * not publish the raw stored number — so an override sitting exactly on a
   * bound is indistinguishable from one that was clamped to get there. That
   * ambiguity is real and is stated rather than papered over.
   */
  const clampAmbiguous = (): boolean => {
    const bound = b();
    if (bound === null || ctl().origin(key()) !== "org" || !Number.isFinite(servedNum())) {
      return false;
    }
    return servedNum() === bound.min || servedNum() === bound.max;
  };

  /**
   * The live verdict, or nothing.
   *
   * ⛔ IT IS WITHHELD, NEVER GUESSED. With no source read, or with the timing it
   * depends on `unknown`, there is no number to argue from and the row says so
   * once at the foot of the panel rather than inventing an Alertmanager. A
   * `default_applies` timing DOES produce a verdict — the default is what governs
   * — and the wording names it as Alertmanager's rather than the operator's.
   */
  const guidance = (): Guidance | null => {
    const g = props.knob.guide;
    const am = ctl().am();
    if (g === undefined || am === null) return null;
    const v = ctl().num(key());
    if (!Number.isFinite(v)) return null;
    return g(v, am, (k) => ctl().num(k));
  };

  const error = (): string | undefined => ctl().localError(key()) ?? ctl().serverError(key());

  const suggestion = (): number | null => {
    const g = guidance();
    if (g?.suggest === undefined) return null;
    const bound = b();
    const raw = Math.round(g.suggest);
    const clamped = bound === null ? raw : Math.min(bound.max, Math.max(bound.min, raw));
    return clamped === ctl().num(key()) ? null : clamped;
  };

  return (
    <li
      class={cn(
        "border-b border-line px-3 py-3 last:border-b-0",
        ctl().dirty(key()) ? "bg-accent-fill/40" : "",
      )}
    >
      <div class="grid gap-x-5 gap-y-3 md:grid-cols-[17rem_minmax(0,1fr)]">
        {/* ---- control column ---- */}
        <div class="flex min-w-0 flex-col gap-2">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-item font-medium text-ink">{props.knob.label}</span>
            <OriginBadge origin={ctl().origin(key())} configKey={ctl().configKey(key())} />
          </div>

          <Switch>
            <Match when={props.knob.kind === "boolean"}>
              <div class="flex items-center gap-1.5">
                <Checkbox
                  id={id()}
                  checked={ctl().text(key()) === "true"}
                  disabled={ctl().resetQueued(key()) || ctl().managed(key())}
                  onChange={(next) => ctl().setText(key(), next ? "true" : "false")}
                />
                <label for={`${id()}-input`} class="text-body text-ink">
                  {ctl().text(key()) === "true"
                    ? "On — the resolve is broadcast into the channel"
                    : "Off — the resolve stays in the thread"}
                </label>
              </div>
            </Match>

            <Match when={props.knob.kind === "verbosity"}>
              <KnobSelect
                id={id()}
                options={VERBOSITY_OPTIONS}
                value={ctl().text(key())}
                disabled={ctl().resetQueued(key()) || ctl().managed(key())}
                error={error()}
                onChange={(next) => ctl().setText(key(), next)}
              />
            </Match>

            <Match when={props.knob.kind === "mentionMode" || props.knob.kind === "severity"}>
              <KnobSelect
                id={id()}
                options={props.knob.kind === "severity" ? SEVERITY_OPTIONS : MENTION_MODE_OPTIONS}
                value={ctl().text(key())}
                disabled={ctl().resetQueued(key()) || ctl().managed(key())}
                error={error()}
                onChange={(next) => ctl().setText(key(), next)}
              />
            </Match>

            <Match when={props.knob.kind === "mentionList"}>
              <MentionListField
                id={id()}
                value={ctl().text(key())}
                disabled={ctl().resetQueued(key()) || ctl().managed(key())}
                error={error()}
                onChange={(next) => ctl().setText(key(), next)}
              />
            </Match>

            <Match when={numeric()}>
              <TextField
                value={ctl().text(key())}
                disabled={ctl().resetQueued(key()) || ctl().managed(key())}
                validationState={error() !== undefined ? "invalid" : "valid"}
                onChange={(next) => ctl().setText(key(), next)}
              >
                <TextFieldLabel>Value</TextFieldLabel>
                <div class="flex items-center gap-2">
                  <TextFieldInput
                    id={id()}
                    type="number"
                    inputmode="numeric"
                    class="max-w-28"
                    min={b()?.min}
                    max={b()?.max}
                  />
                  <span class="shrink-0 text-meta text-ink-subtle">
                    {unitSuffix(props.knob)}
                  </span>
                </div>
                <TextFieldErrorMessage role="alert">{error()}</TextFieldErrorMessage>
              </TextField>
            </Match>
          </Switch>

          <Show when={numeric() && Number.isFinite(ctl().num(key()))}>
            <p class="text-meta text-ink-subtle">
              in force: {readValue(props.knob.kind, ctl().num(key()))}
              <Show when={b()}>
                {(bound) => (
                  <>
                    {" · "}
                    <span title={bound().why}>
                      oto accepts {bound().min}–{bound().max}
                    </span>
                  </>
                )}
              </Show>
            </p>
          </Show>

          {/*
            ⭐ THE SHADOWED OVERRIDE, SHOWN AND NOT HIDDEN. "You have 900s stored
            and OTO_TUNING_REFIRE_GRACE_S is forcing 600s" is actionable; showing
            only the 600 leaves a number sitting in Postgres that nobody can see
            and that takes effect the moment somebody deletes the config key.
          */}
          <Show when={ctl().shadowed(key()) !== undefined}>
            <Note kind="warn">
              Your override of{" "}
              <span class="font-mono">{shadowedText(props.knob, ctl().shadowed(key()))}</span> is
              being overridden by{" "}
              <code class="font-mono">{ctl().configKey(key()) ?? "this deployment's configuration"}</code>
              {" = "}
              <span class="font-mono">{shadowedText(props.knob, ctl().served(key()))}</span>. It is
              still stored and comes back in force the moment that key is removed.
            </Note>
          </Show>

          <Show when={ctl().managed(key())}>
            <Note kind="quiet">
              Read-only here. Change it where it is set —{" "}
              <code class="font-mono">{ctl().configKey(key()) ?? "this deployment's configuration"}</code>{" "}
              — or remove that key to hand the setting back to this org. oto refuses a write to a
              managed key rather than storing a number that is never in force and reverts, visibly,
              on the next deploy.
            </Note>
          </Show>

          <div class="flex flex-wrap items-center gap-2">
            {/*
              ⛔ NO RESET ON A MANAGED KEY. The server refuses one with the same
              409 as a write, and for a sharper reason: unlike a write, a reset
              would destroy the shadowed override underneath — the one value that
              comes back if the config key is removed.
            */}
            <Show when={ctl().origin(key()) === "org" && !ctl().managed(key())}>
              <Button
                size="sm"
                variant={ctl().resetQueued(key()) ? "default" : "secondary"}
                disabled={ctl().busy()}
                onClick={() => ctl().toggleReset(key())}
                title="Removes this org's override so the value follows oto's shipped default. Writing today's default back by hand is a different fact — it records an override that happens to match, and it would not follow the default if oto moved it."
              >
                {ctl().resetQueued(key()) ? "Reset queued — undo" : "Reset to oto's default"}
              </Button>
            </Show>
            <Show when={ctl().dirty(key()) && !ctl().resetQueued(key())}>
              <Button size="sm" variant="ghost" onClick={() => ctl().revert(key())}>
                Revert
              </Button>
            </Show>
          </div>

          <Show when={ctl().resetQueued(key())}>
            <Note kind="quiet">
              Queued. On save, oto will be told to drop this org's override for{" "}
              <code class="font-mono">{key()}</code> — not to write the current number back. The
              value will then follow oto's shipped default, including if that default moves.
            </Note>
          </Show>
        </div>

        {/* ---- explanation column ---- */}
        <div class="flex min-w-0 flex-col gap-2">
          <p class="text-body leading-relaxed text-ink-muted">{props.knob.what}</p>

          <dl class="flex flex-col gap-1.5">
            <For each={props.knob.risks}>
              {(risk) => (
                <div class="grid grid-cols-[minmax(0,5.5rem)_minmax(0,1fr)] gap-x-2">
                  <dt class="text-meta font-semibold uppercase tracking-[0.04em] text-ink-subtle">
                    {risk.label}
                  </dt>
                  <dd class="text-body leading-relaxed text-ink">{risk.text}</dd>
                </div>
              )}
            </For>
          </dl>

          <p class="border-l-2 border-line-strong pl-2 text-body leading-relaxed text-ink-muted">
            <span class="font-medium text-ink">Against your Alertmanager. </span>
            {props.knob.amRule}
          </p>

          <Show when={guidance()}>
            {(g) => (
              <Note kind={g().level === "ok" ? "quiet" : "warn"}>
                <span class="uppercase tracking-[0.04em]">
                  {g().level === "inert" ? "Inert" : g().level === "tight" ? "Tight" : "Consistent"}
                  {" · "}
                </span>
                {g().text}
                <Show when={suggestion()}>
                  {(s) => (
                    <Button
                      size="sm"
                      variant="ghost"
                      class="ml-1 align-baseline"
                      onClick={() => ctl().setText(key(), String(s()))}
                    >
                      use {s()}
                      {props.knob.kind === "seconds" ? ` (${duration(s())})` : ""}
                    </Button>
                  )}
                </Show>
              </Note>
            )}
          </Show>

          <Show when={outsideBounds()}>
            <Note kind="warn">
              oto served {servedNum()} for this key, which is outside the {b()?.min}–{b()?.max} range
              it publishes. Values stored before a bound existed are clamped on read rather than
              rejected, so this should not be reachable. It is shown exactly as served rather than
              silently corrected — but oto is not necessarily using this number, and saving anything
              on this screen will require bringing it inside the range.
            </Note>
          </Show>

          <Show when={!outsideBounds() && clampAmbiguous()}>
            <Note kind="quiet">
              This override sits exactly on a bound. Out-of-range values are clamped on read rather
              than rejected, and the API publishes only the effective number — so a value stored
              before this bound existed would look identical to one deliberately set here, and oto
              cannot tell you which this is. Resetting to the default removes the doubt.
            </Note>
          </Show>
        </div>
      </div>
    </li>
  );
};

/* -------------------------------------------------------------------------- */
/* Mention control                                                            */
/* -------------------------------------------------------------------------- */

/**
 * The explicit mention audience, capped at ten.
 *
 * Entries are added and removed rather than typed as a blob, so the count is
 * always visible against its cap and no operator discovers the eleventh entry
 * from a 422. The accepted *shape* of an entry is shown as a hint and validated
 * by the server: re-implementing that pattern here would be a second copy of a
 * rule that only one side enforces.
 */
const MentionListField: Component<{
  readonly id: string;
  readonly value: string;
  readonly disabled: boolean;
  readonly error: string | undefined;
  readonly onChange: (next: string) => void;
}> = (props) => {
  const list = (): readonly string[] => {
    try {
      return asStringList(JSON.parse(props.value));
    } catch {
      return [];
    }
  };

  const emit = (next: readonly string[]): void => props.onChange(JSON.stringify(next));

  const [entry, setEntry] = createSignal("");
  const full = (): boolean => list().length >= MENTION_LIST_MAX;

  const add = (): void => {
    const v = entry().trim();
    if (v === "" || list().includes(v) || full()) return;
    emit([...list(), v]);
    setEntry("");
  };

  return (
    <div class="flex flex-col gap-1.5">
      <Show when={list().length > 0}>
        <ul class="flex flex-wrap gap-1">
          <For each={list()}>
            {(item) => (
              <li class="inline-flex items-center gap-1 rounded-chip border border-line bg-raised px-1 py-px font-mono text-meta text-ink-muted">
                {item}
                <button
                  type="button"
                  class="rounded-chip px-0.5 text-ink-subtle hover:text-ink"
                  aria-label={`Remove ${item}`}
                  disabled={props.disabled}
                  onClick={() => emit(list().filter((x) => x !== item))}
                >
                  ×
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>

      <TextField
        value={entry()}
        disabled={props.disabled || full()}
        validationState={props.error !== undefined ? "invalid" : "valid"}
        onChange={setEntry}
      >
        <TextFieldLabel>Add an entry</TextFieldLabel>
        <div class="flex items-center gap-2">
          <TextFieldInput
            id={props.id}
            class="min-w-0 font-mono"
            placeholder="<!subteam^S01AB2CD3EF>"
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                add();
              }
            }}
          />
          <Button size="sm" disabled={props.disabled || full()} onClick={add}>
            Add
          </Button>
        </div>
        <TextFieldDescription>{MENTION_TOKEN_HINT}</TextFieldDescription>
        <TextFieldErrorMessage role="alert">{props.error}</TextFieldErrorMessage>
      </TextField>

      <p class="text-meta text-ink-subtle" aria-live="polite">
        {list().length} of {MENTION_LIST_MAX} used
        {full() ? " — the cap the server enforces" : ""}.
      </p>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Save bar, leave guard, skeleton                                            */
/* -------------------------------------------------------------------------- */

const SaveBar: Component<{
  readonly dirty: number;
  readonly blocked: number;
  readonly resets: number;
  readonly busy: boolean;
  readonly onSave: () => void;
  readonly onDiscard: () => void;
}> = (props) => (
  <Show when={props.dirty > 0}>
    <div
      role="region"
      aria-label="Unsaved changes"
      class="fixed inset-x-0 bottom-0 z-40 border-t border-line-strong bg-raised motion-safe:oto-enter"
    >
      <div class="mx-auto flex w-full max-w-5xl flex-wrap items-center gap-3 px-4 py-2.5">
        <p class="text-body text-ink" aria-live="polite">
          <span class="font-medium">
            {props.dirty} unsaved change{props.dirty === 1 ? "" : "s"}
          </span>
          <Show when={props.resets > 0}>
            <span class="text-ink-muted">
              {" "}
              · {props.resets} of them {props.resets === 1 ? "removes an override" : "remove overrides"}
            </span>
          </Show>
          <Show when={props.blocked > 0}>
            <span class="text-ink-muted">
              {" "}
              · {props.blocked} outside the range oto accepts
            </span>
          </Show>
        </p>
        <div class="ml-auto flex items-center gap-2">
          <Button size="sm" variant="ghost" disabled={props.busy} onClick={props.onDiscard}>
            Discard
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={props.busy}
            disabled={props.blocked > 0}
            title={
              props.blocked > 0
                ? "Some values are outside the range the server publishes. It would refuse the whole write, so this stops here instead."
                : undefined
            }
            onClick={props.onSave}
          >
            Save {props.dirty} change{props.dirty === 1 ? "" : "s"}
          </Button>
        </div>
      </div>
    </div>
  </Show>
);

const LeaveGuard: Component<{
  readonly pending: BeforeLeaveEventArgs | null;
  readonly count: number;
  readonly onStay: () => void;
  readonly onLeave: () => void;
}> = (props) => (
  <Modal
    open={props.pending !== null}
    onOpenChange={(isOpen) => {
      if (!isOpen) props.onStay();
    }}
  >
    <ModalContent class="max-w-sm">
      <ModalHeader>
        <ModalTitle>Leave without saving?</ModalTitle>
        <ModalDescription>
          These edits change how loud oto is. Nothing has been written yet.
        </ModalDescription>
      </ModalHeader>

      <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
        <p>
          {props.count} change{props.count === 1 ? "" : "s"} on this screen{" "}
          {props.count === 1 ? "has" : "have"} not been sent to oto. Leaving now discards{" "}
          {props.count === 1 ? "it" : "them"}.
        </p>
      </div>

      <ModalFooter>
        <Button size="sm" variant="secondary" onClick={props.onStay}>
          Stay on this page
        </Button>
        <Button size="sm" variant="destructive" onClick={props.onLeave}>
          Discard and leave
        </Button>
      </ModalFooter>
    </ModalContent>
  </Modal>
);

/**
 * The loading state occupies the boxes the real rows will occupy, so nothing
 * jumps when the data lands. On a screen full of number inputs a row that moves
 * under the cursor is a wrong value typed into the wrong knob.
 */
const TuningSkeleton: Component = () => (
  <For each={[3, 3, 3]}>
    {(rows) => (
      <Panel>
        <PanelHeader>
          <Skeleton class="h-2.5 w-40" />
        </PanelHeader>
        <For each={Array.from({ length: rows })}>
          {() => (
            <div class="grid gap-x-5 gap-y-3 border-b border-line px-3 py-3 last:border-b-0 md:grid-cols-[17rem_minmax(0,1fr)]">
              <div class="flex flex-col gap-2">
                <Skeleton class="h-2.5 w-32" />
                <Skeleton class="h-8 w-28" />
                <Skeleton class="h-2 w-40" />
              </div>
              <div class="flex flex-col gap-2">
                <Skeleton class="h-2 w-full" />
                <Skeleton class="h-2 w-11/12" />
                <Skeleton class="h-2 w-10/12" />
                <Skeleton class="h-2 w-9/12" />
              </div>
            </div>
          )}
        </For>
      </Panel>
    )}
  </For>
);
