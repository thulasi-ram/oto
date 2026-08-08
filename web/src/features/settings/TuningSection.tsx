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
 *   2. **The Alertmanager relationship is inline and live.** Almost every value
 *      here is only meaningful as a multiple of the customer's own
 *      `group_wait` / `group_interval` / `repeat_interval` and their rules'
 *      `for:`. A re-fire grace shorter than `group_interval` is unreachable; a
 *      flap threshold of 5-in-30m is unreachable for a rule with `for: 10m`.
 *      oto cannot read those four numbers, so the operator enters them once and
 *      every knob is checked against them on each keystroke.
 *
 *   3. **Origin is the primary fact, not a footnote.** An operator debugging a
 *      noisy Slack needs to see instantly which values are theirs. Each knob is
 *      badged, the count is at the top, and there is a filter that hides
 *      everything stock.
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

import { orphanViolations, violationsByField } from "~/api/client";
import { getOrgSettings, updateOrgSettings } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type {
  OrgSettingsView,
  SettingBound,
  SettingOrigin,
  UpdateOrgSettingsRequest,
} from "~/api/types";
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import {
  Button,
  Checkbox,
  Field,
  Input,
  Panel,
  PanelHeader,
  PanelTitle,
  Select,
  cx,
} from "~/components/ui/primitives";
import { ErrorBanner, ErrorState, Skeleton } from "~/components/ui/states";
import { duration } from "~/lib/format";
import {
  AM_DEFAULTS,
  AM_FIELDS,
  KNOBS,
  KNOB_GROUPS,
  MENTION_LIST_MAX,
  MENTION_MODE_OPTIONS,
  MENTION_TOKEN_HINT,
  SEVERITY_OPTIONS,
  VERBOSITY_OPTIONS,
  isNumeric,
  readValue,
  unitSuffix,
  type AmRef,
  type Guidance,
  type KnobCopy,
  type KnobKey,
} from "./tuningCopy";

/* -------------------------------------------------------------------------- */
/* The Alertmanager reference, held locally                                   */
/* -------------------------------------------------------------------------- */

/**
 * oto has no access to `alertmanager.yml` and the API carries nothing about the
 * customer's route timing, so these four numbers live in this browser. That is a
 * real limitation and the panel says so rather than implying oto knows.
 */
const AM_STORAGE_KEY = "oto.tuning.alertmanager.v1";

function loadAmRef(): AmRef {
  try {
    const raw = globalThis.localStorage?.getItem(AM_STORAGE_KEY);
    if (raw === null || raw === undefined) return AM_DEFAULTS;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return AM_DEFAULTS;
    const rec = parsed as Record<string, unknown>;
    const pick = (k: keyof AmRef, fallback: number): number => {
      const v = rec[k];
      return typeof v === "number" && Number.isFinite(v) && v > 0 ? v : fallback;
    };
    return {
      group_wait_s: pick("group_wait_s", AM_DEFAULTS.group_wait_s),
      group_interval_s: pick("group_interval_s", AM_DEFAULTS.group_interval_s),
      repeat_interval_s: pick("repeat_interval_s", AM_DEFAULTS.repeat_interval_s),
      rule_for_s: pick("rule_for_s", AM_DEFAULTS.rule_for_s),
      confirmed: rec["confirmed"] === true,
    };
  } catch {
    return AM_DEFAULTS;
  }
}

function saveAmRef(next: AmRef): void {
  try {
    globalThis.localStorage?.setItem(AM_STORAGE_KEY, JSON.stringify(next));
  } catch {
    /* A browser with storage denied still gets a working screen for this session. */
  }
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
  readonly am: () => AmRef;
  /** Present in the served payload at all. False for a key the contract dropped. */
  readonly supported: (key: KnobKey) => boolean;
  readonly origin: (key: KnobKey) => SettingOrigin | null;
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

  const [am, setAm] = createSignal<AmRef>(loadAmRef());
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
      if (resets().includes(key)) continue;
      const raw = edits()[key];
      if (raw === undefined) continue;
      const parsed = parseFor(key, raw);
      if (parsed === undefined) continue;
      if (same(parsed, served(key))) continue;
      body[key] = parsed;
    }
    const queued = resets().filter((k) => supported(k));
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
        value={am()}
        onChange={(next) => {
          setAm(next);
          saveAmRef(next);
        }}
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
            onlyOverrides={onlyOverrides()}
            setOnlyOverrides={setOnlyOverrides}
          />

          <Show when={save.error !== null}>
            <ErrorBanner error={save.error}>
              <div class="flex flex-col gap-1">
                <span class="font-medium">oto refused the write, and nothing was changed.</span>
                <span class="text-ink-muted">
                  The bounds are enforced on the server against the merged state, so a value can be
                  refused even though every field on this screen looked legal on its own.
                </span>
                <For each={orphans()}>{(o) => <span class="text-ink-muted">{o}</span>}</For>
              </div>
            </ErrorBanner>
          </Show>

          <For each={KNOB_GROUPS}>
            {(group) => {
              const keys = (): readonly KnobKey[] =>
                group.keys.filter(
                  (k) => supported(k) && (!onlyOverrides() || origin(k) === "org" || dirty(k)),
                );
              return (
                <Show when={keys().length > 0}>
                  <Panel>
                    <PanelHeader class="flex-col items-start gap-0.5">
                      <PanelTitle>{group.title}</PanelTitle>
                      <p class="text-[11px] leading-snug text-ink-muted">{group.blurb}</p>
                    </PanelHeader>
                    <ul>
                      <For each={keys()}>{(key) => <KnobRow knob={KNOBS[key]} ctl={ctl} />}</For>
                    </ul>
                  </Panel>
                </Show>
              );
            }}
          </For>

          <Show when={onlyOverrides() && overrideKeys().length === 0 && dirtyKeys().length === 0}>
            <Panel class="px-3 py-8 text-center">
              <p class="text-[13px] font-medium text-ink">This org has changed nothing.</p>
              <p class="mx-auto mt-1 max-w-md text-[12px] leading-relaxed text-ink-muted">
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

const AlertmanagerPanel: Component<{
  readonly value: AmRef;
  readonly onChange: (next: AmRef) => void;
}> = (props) => {
  const set = (key: keyof AmRef, raw: string): void => {
    const n = Number.parseInt(raw.trim(), 10);
    if (!Number.isFinite(n) || n <= 0) return;
    props.onChange({ ...props.value, [key]: n, confirmed: true });
  };

  return (
    <Panel>
      <PanelHeader class="flex-col items-start gap-0.5">
        <PanelTitle>Your Alertmanager</PanelTitle>
        <p class="text-[11px] leading-snug text-ink-muted">
          Read these out of the route your oto receiver is attached to, parents included. Every
          duration below this panel is a multiple of them, not an absolute time.
        </p>
      </PanelHeader>

      <div class="border-b border-line px-3 py-2">
        <p class="text-[12px] leading-relaxed text-ink-muted">
          <span class="font-medium text-ink">oto cannot read these.</span> It has no access to your{" "}
          <code class="font-mono text-[11px] text-ink">alertmanager.yml</code> and no API that
          carries your route timing, so these four numbers are entered here and kept in this browser
          only. They are never sent anywhere and they change nothing — they exist so the guidance
          below can be arithmetic instead of a guess.
          <Show when={!props.value.confirmed}>
            <span class="mt-1 block rounded-[4px] border border-line-strong bg-raised px-2 py-1 font-medium text-ink">
              Assumed, not yours. These are Alertmanager's own defaults. Until you enter your real
              values every verdict below is about a cluster that may not be yours.
            </span>
          </Show>
        </p>
      </div>

      <div class="grid gap-3 px-3 py-3 md:grid-cols-2">
        <For each={AM_FIELDS}>
          {(f) => (
            <Field
              id={`am-${f.key}`}
              label={`${f.label} — ${duration(props.value[f.key])}`}
              hint={f.why}
            >
              {(a) => (
                <div class="flex items-center gap-2">
                  <Input
                    {...a}
                    type="number"
                    min={1}
                    inputmode="numeric"
                    class="max-w-28"
                    value={String(props.value[f.key])}
                    onInput={(e) => set(f.key, e.currentTarget.value)}
                  />
                  <span class="shrink-0 text-[11px] text-ink-subtle">seconds</span>
                </div>
              )}
            </Field>
          )}
        </For>
      </div>

      <div class="flex items-center justify-between gap-3 border-t border-line bg-raised px-3 py-2">
        <p class="text-[11px] leading-snug text-ink-subtle">
          One more thing no number can capture: if your{" "}
          <code class="font-mono text-ink-muted">group_by</code> contains a per-replica label such as{" "}
          <code class="font-mono text-ink-muted">instance</code> or{" "}
          <code class="font-mono text-ink-muted">pod</code>, storm collapse is unreachable at any
          threshold, because no group ever accumulates members. That fix is in{" "}
          <code class="font-mono text-ink-muted">alertmanager.yml</code>, not on this screen.
        </p>
        <Button
          size="sm"
          variant="ghost"
          class="shrink-0"
          onClick={() => props.onChange(AM_DEFAULTS)}
        >
          Use Alertmanager defaults
        </Button>
      </div>
    </Panel>
  );
};

/* -------------------------------------------------------------------------- */
/* Origin summary                                                             */
/* -------------------------------------------------------------------------- */

const OriginSummary: Component<{
  readonly total: number;
  readonly overrides: number;
  readonly onlyOverrides: boolean;
  readonly setOnlyOverrides: (next: boolean) => void;
}> = (props) => (
  <div class="flex flex-wrap items-center justify-between gap-3 rounded-[6px] border border-line bg-raised px-3 py-2">
    <p class="text-[12px] leading-snug text-ink">
      <span class="font-medium">
        {props.overrides} of {props.total}
      </span>{" "}
      {props.overrides === 1 ? "value is" : "values are"} this org's own. The rest are oto's shipped
      defaults and will follow them if oto moves them.
    </p>
    <Checkbox
      checked={props.onlyOverrides}
      onChange={props.setOnlyOverrides}
      label={<span class="text-[12px]">Show only what this org has changed</span>}
    />
  </div>
);

/* -------------------------------------------------------------------------- */
/* One knob                                                                   */
/* -------------------------------------------------------------------------- */

const OriginBadge: Component<{ readonly origin: SettingOrigin | null }> = (props) => (
  <span
    class={cx(
      "inline-flex shrink-0 items-center gap-1 rounded-[3px] border px-1.5 py-px text-[11px] leading-4",
      props.origin === "org"
        ? "border-accent-border bg-accent-fill font-semibold text-ink"
        : "border-line bg-surface text-ink-subtle",
    )}
    title={
      props.origin === "org"
        ? "This org wrote this value. It will not follow oto's shipped default if that moves."
        : "oto's shipped default is in force. This org has never written this key."
    }
  >
    <span
      aria-hidden="true"
      class={cx(
        "size-1.5 rounded-full",
        props.origin === "org" ? "bg-accent" : "border border-line-strong",
      )}
    />
    {props.origin === "org" ? "override" : "oto default"}
  </span>
);

const Note: Component<{ readonly kind: "warn" | "quiet"; readonly children: JSX.Element }> = (
  props,
) => (
  // Tier A only (SPEC §M.2). A warning on a settings screen is not an alert
  // state, so it signals with a border, weight and a word — never with a state
  // hue. Spending a saturated colour here is what makes a firing row stop
  // reading as urgent.
  <p
    class={cx(
      "rounded-[4px] border px-2 py-1 text-[11px] leading-snug",
      props.kind === "warn"
        ? "border-line-strong bg-raised font-medium text-ink"
        : "border-line bg-sunken text-ink-muted",
    )}
  >
    {props.children}
  </p>
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

  const guidance = (): Guidance | null => {
    const g = props.knob.guide;
    if (g === undefined) return null;
    const v = ctl().num(key());
    if (!Number.isFinite(v)) return null;
    const result = g(v, ctl().am(), (k) => ctl().num(k));
    return result.level === "ok" && !ctl().am().confirmed ? null : result;
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
      class={cx(
        "border-b border-line px-3 py-3 last:border-b-0",
        ctl().dirty(key()) ? "bg-accent-fill/40" : "",
      )}
    >
      <div class="grid gap-x-5 gap-y-3 md:grid-cols-[17rem_minmax(0,1fr)]">
        {/* ---- control column ---- */}
        <div class="flex min-w-0 flex-col gap-2">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-[13px] font-medium text-ink">{props.knob.label}</span>
            <OriginBadge origin={ctl().origin(key())} />
          </div>

          <Switch>
            <Match when={props.knob.kind === "boolean"}>
              <Checkbox
                id={id()}
                checked={ctl().text(key()) === "true"}
                disabled={ctl().resetQueued(key())}
                onChange={(next) => ctl().setText(key(), next ? "true" : "false")}
                label={
                  <span class="text-[12px]">
                    {ctl().text(key()) === "true"
                      ? "On — the resolve is broadcast into the channel"
                      : "Off — the resolve stays in the thread"}
                  </span>
                }
              />
            </Match>

            <Match when={props.knob.kind === "verbosity"}>
              <Field id={id()} label="Value" error={error()}>
                {(a) => (
                  <Select
                    {...a}
                    value={ctl().text(key())}
                    disabled={ctl().resetQueued(key())}
                    onChange={(e) => ctl().setText(key(), e.currentTarget.value)}
                  >
                    <For each={VERBOSITY_OPTIONS}>
                      {(o) => <option value={o.value}>{o.label}</option>}
                    </For>
                  </Select>
                )}
              </Field>
            </Match>

            <Match when={props.knob.kind === "mentionMode" || props.knob.kind === "severity"}>
              <Field id={id()} label="Value" error={error()}>
                {(a) => (
                  <Select
                    {...a}
                    value={ctl().text(key())}
                    disabled={ctl().resetQueued(key())}
                    onChange={(e) => ctl().setText(key(), e.currentTarget.value)}
                  >
                    <For
                      each={
                        props.knob.kind === "severity" ? SEVERITY_OPTIONS : MENTION_MODE_OPTIONS
                      }
                    >
                      {(o) => <option value={o.value}>{o.label}</option>}
                    </For>
                  </Select>
                )}
              </Field>
            </Match>

            <Match when={props.knob.kind === "mentionList"}>
              <MentionListField
                id={id()}
                value={ctl().text(key())}
                disabled={ctl().resetQueued(key())}
                error={error()}
                onChange={(next) => ctl().setText(key(), next)}
              />
            </Match>

            <Match when={numeric()}>
              <Field id={id()} label="Value" error={error()}>
                {(a) => (
                  <div class="flex items-center gap-2">
                    <Input
                      {...a}
                      type="number"
                      inputmode="numeric"
                      class="max-w-28"
                      min={b()?.min}
                      max={b()?.max}
                      disabled={ctl().resetQueued(key())}
                      value={ctl().text(key())}
                      onInput={(e) => ctl().setText(key(), e.currentTarget.value)}
                    />
                    <span class="shrink-0 text-[11px] text-ink-subtle">
                      {unitSuffix(props.knob)}
                    </span>
                  </div>
                )}
              </Field>
            </Match>
          </Switch>

          <Show when={numeric() && Number.isFinite(ctl().num(key()))}>
            <p class="text-[11px] text-ink-subtle">
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

          <div class="flex flex-wrap items-center gap-2">
            <Show when={ctl().origin(key()) === "org"}>
              <Button
                size="sm"
                variant={ctl().resetQueued(key()) ? "primary" : "secondary"}
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
          <p class="text-[12px] leading-relaxed text-ink-muted">{props.knob.what}</p>

          <dl class="flex flex-col gap-1.5">
            <For each={props.knob.risks}>
              {(risk) => (
                <div class="grid grid-cols-[minmax(0,5.5rem)_minmax(0,1fr)] gap-x-2">
                  <dt class="text-[11px] font-semibold uppercase tracking-[0.04em] text-ink-subtle">
                    {risk.label}
                  </dt>
                  <dd class="text-[12px] leading-relaxed text-ink">{risk.text}</dd>
                </div>
              )}
            </For>
          </dl>

          <p class="border-l-2 border-line-strong pl-2 text-[12px] leading-relaxed text-ink-muted">
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
              <li class="inline-flex items-center gap-1 rounded-[3px] border border-line bg-raised px-1 py-px font-mono text-[11px] text-ink-muted">
                {item}
                <button
                  type="button"
                  class="rounded-[2px] px-0.5 text-ink-subtle hover:text-ink"
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

      <Field id={props.id} label="Add an entry" hint={MENTION_TOKEN_HINT} error={props.error}>
        {(a) => (
          <div class="flex items-center gap-2">
            <Input
              {...a}
              mono
              class="min-w-0"
              placeholder="<!subteam^S01AB2CD3EF>"
              value={entry()}
              disabled={props.disabled || full()}
              onInput={(e) => setEntry(e.currentTarget.value)}
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
        )}
      </Field>

      <p class="text-[11px] text-ink-subtle" aria-live="polite">
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
        <p class="text-[12px] text-ink" aria-live="polite">
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
            variant="primary"
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
  <Dialog
    open={props.pending !== null}
    onClose={props.onStay}
    width="sm"
    title="Leave without saving?"
    description="These edits change how loud oto is. Nothing has been written yet."
    footer={
      <>
        <Button size="sm" onClick={props.onStay}>
          Stay on this page
        </Button>
        <Button size="sm" variant="danger" onClick={props.onLeave}>
          Discard and leave
        </Button>
      </>
    }
  >
    <DialogBody>
      <p>
        {props.count} change{props.count === 1 ? "" : "s"} on this screen{" "}
        {props.count === 1 ? "has" : "have"} not been sent to oto. Leaving now discards{" "}
        {props.count === 1 ? "it" : "them"}.
      </p>
    </DialogBody>
  </Dialog>
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
