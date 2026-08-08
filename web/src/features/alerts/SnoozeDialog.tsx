/**
 * oto's quiet button (§B.8), shared by the alert detail and the group fan-out.
 *
 * The vocabulary is governed by SCOPE-BOUNDARY and §B.8, and none of it is
 * decoration:
 *
 *   - Snooze suppresses **oto's own notifications**. It writes nothing into
 *     Alertmanager, creates no silence, and changes nothing about the signal.
 *   - It is **not a state**. A snoozed critical alert is still critical and
 *     still firing, and every surface keeps rendering it that way — so nothing
 *     here says "mute", "silence", "resolve" or "ignore".
 *   - It **always ends**. Minimum 5 minutes, maximum 30 days, no indefinite
 *     option anywhere in this form, because an unexpiring snooze is a mute and
 *     mutes are how channels go quiet forever.
 *   - It **announces itself**: starting and ending each enqueue a notification,
 *     so a channel is told it is going quiet and told again when it wakes.
 *
 * Validation is local **and** server-side, and the two are not redundant: the
 * local pass (valibot, mirroring the contract's bounds) stops an obviously bad
 * request; the server's `violations[]` is authoritative and is mapped back onto
 * the exact control that failed.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";
import { useMutation } from "@tanstack/solid-query";
import * as v from "valibot";

import { ApiError, orphanViolations, violationsByField } from "~/api/client";
import type { SnoozeRequest } from "~/api/types";
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import { Button, Field, Input, Textarea, cx } from "~/components/ui/primitives";
import { ErrorBanner } from "~/components/ui/states";
import { absoluteTime, duration as fmtDuration, idempotencyKey } from "~/lib/format";

/* -------------------------------------------------------------------------- */
/* §B.8's bounds and presets, mirrored exactly                                */
/* -------------------------------------------------------------------------- */

export const SNOOZE_MIN_SECONDS = 5 * 60;
export const SNOOZE_MAX_SECONDS = 30 * 24 * 3600;

/**
 * The five presets §B.8.3 makes binding, and the five values the contract's
 * `duration_seconds` documents: 1800, 3600, 14400, 86400, 604800.
 */
export const SNOOZE_PRESETS: readonly { readonly label: string; readonly seconds: number }[] = [
  { label: "30 minutes", seconds: 1800 },
  { label: "1 hour", seconds: 3600 },
  { label: "4 hours", seconds: 14_400 },
  { label: "24 hours", seconds: 86_400 },
  { label: "7 days", seconds: 604_800 },
];

/* -------------------------------------------------------------------------- */
/* Local schemas, mirroring the contract's bounds                             */
/* -------------------------------------------------------------------------- */

const NoteSchema = v.pipe(v.string(), v.maxLength(2000, "A note is at most 2000 characters."));

const TOO_LONG =
  "The longest snooze is 30 days. There is no indefinite snooze — that would be a mute, and mutes are how channels go quiet forever.";

const DurationSchema = v.pipe(
  v.number("Give a number of minutes."),
  v.integer("A snooze window is a whole number of minutes."),
  v.minValue(SNOOZE_MIN_SECONDS, "The shortest snooze is 5 minutes."),
  v.maxValue(SNOOZE_MAX_SECONDS, TOO_LONG),
);

function secondsUntil(local: string): number {
  const t = new Date(local).getTime();
  return Number.isNaN(t) ? Number.NaN : Math.round((t - Date.now()) / 1000);
}

const UntilSchema = v.pipe(
  v.string(),
  v.minLength(1, "Pick the instant notifications resume. There is no indefinite snooze."),
  v.check((s) => !Number.isNaN(secondsUntil(s)), "oto could not read that as a time."),
  v.check((s) => secondsUntil(s) >= SNOOZE_MIN_SECONDS, "The shortest snooze is 5 minutes from now."),
  v.check((s) => secondsUntil(s) <= SNOOZE_MAX_SECONDS, TOO_LONG),
);

function firstIssue(result: v.SafeParseResult<v.GenericSchema>): string | undefined {
  return result.success ? undefined : result.issues[0]?.message;
}

/** `2026-08-07T17:00` — what `<input type="datetime-local">` wants. */
function toLocalInputValue(at: Date): string {
  const pad = (n: number): string => String(n).padStart(2, "0");
  return (
    `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}` +
    `T${pad(at.getHours())}:${pad(at.getMinutes())}`
  );
}

/* -------------------------------------------------------------------------- */
/* The dialog                                                                 */
/* -------------------------------------------------------------------------- */

export type SnoozeSubject = "alert" | "group";

export interface SnoozeDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly subject: SnoozeSubject;
  /** Runs the real mutation. One idempotency key per gesture, minted here. */
  readonly onSubmit: (body: SnoozeRequest, key: string) => Promise<unknown>;
  /** Invalidate whatever the caller needs invalidated. */
  readonly onSuccess: () => void;
}

const SUBJECT_DESCRIPTION: Record<SnoozeSubject, string> = {
  alert:
    "Stops oto's own notifications for this alert until a fixed time. It is not a silence: nothing changes in your cluster, the alert keeps firing, and it stays visible here at full severity.",
  group:
    "Stops oto's own notifications for every alert currently in this group, until a fixed time. Alerts that join later are not snoozed — a snooze is never predictive.",
};

export const SnoozeDialog: Component<SnoozeDialogProps> = (props) => {
  const [mode, setMode] = createSignal<"duration" | "until">("duration");
  const [seconds, setSeconds] = createSignal(3600);
  const [until, setUntil] = createSignal(toLocalInputValue(new Date(Date.now() + 3_600_000)));
  const [note, setNote] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const durationError = createMemo(() =>
    mode() === "duration" && touched() ? firstIssue(v.safeParse(DurationSchema, seconds())) : undefined,
  );
  const untilError = createMemo(() =>
    mode() === "until" && touched() ? firstIssue(v.safeParse(UntilSchema, until())) : undefined,
  );
  const noteError = createMemo(() =>
    touched() ? firstIssue(v.safeParse(NoteSchema, note())) : undefined,
  );

  const valid = (): boolean => {
    if (!v.safeParse(NoteSchema, note()).success) return false;
    return mode() === "duration"
      ? v.safeParse(DurationSchema, seconds()).success
      : v.safeParse(UntilSchema, until()).success;
  };

  /**
   * Exactly one of `until` and `duration_seconds` — both is a 422 and neither
   * is a 422, because there is no indefinite snooze and therefore no default
   * window. The mode toggle is what makes that structural rather than a rule
   * someone has to remember.
   */
  const buildBody = (): SnoozeRequest => {
    const body: SnoozeRequest =
      mode() === "duration"
        ? { duration_seconds: seconds() }
        : { until: new Date(until()).toISOString() };
    const text = note().trim();
    return text === "" ? body : { ...body, note: text };
  };

  const mutation = useMutation(() => ({
    mutationFn: (body: SnoozeRequest) => props.onSubmit(body, idempotencyKey()),
    onSuccess: () => {
      setTouched(false);
      setNote("");
      props.onSuccess();
      props.onClose();
    },
  }));

  const serverErrors = (): ReadonlyMap<string, string> => violationsByField(mutation.error);
  const orphans = (): readonly string[] =>
    orphanViolations(mutation.error, ["until", "duration_seconds", "note"]);

  /** Resolved against this browser's clock, for the label only — oto resolves
   * `duration_seconds` against its own, so a skewed client cannot talk its way
   * outside the window. */
  const endsAt = (): Date =>
    mode() === "duration" ? new Date(Date.now() + seconds() * 1000) : new Date(until());

  const endsLabel = (): string => {
    const at = endsAt();
    if (Number.isNaN(at.getTime())) return "…";
    return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  };

  return (
    <Dialog
      open={props.open}
      onClose={props.onClose}
      title={props.subject === "group" ? "Snooze every current member" : "Snooze notifications"}
      description={SUBJECT_DESCRIPTION[props.subject]}
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            busy={mutation.isPending}
            disabled={touched() && !valid()}
            onClick={() => {
              setTouched(true);
              if (!valid()) return;
              mutation.mutate(buildBody());
            }}
          >
            Hold notifications until {endsLabel()}
          </Button>
        </>
      }
    >
      <DialogBody>
        <Show when={mutation.error !== null && orphans().length > 0}>
          <ErrorBanner>
            <For each={orphans()}>{(msg) => <p>{msg}</p>}</For>
          </ErrorBanner>
        </Show>

        <Show when={mutation.error instanceof ApiError && mutation.error.status === 412}>
          <ErrorBanner>
            {props.subject === "group"
              ? "There is nothing here to snooze — this group has no currently-joined member. A snooze is never predictive, so there is nothing for it to attach to."
              : "This alert is in the wrong state for a snooze. The request itself was fine; the entity moved while the dialog was open."}
          </ErrorBanner>
        </Show>

        <Show
          when={
            mutation.error instanceof ApiError &&
            mutation.error.status !== 412 &&
            mutation.error.status !== 422
          }
        >
          <ErrorBanner error={mutation.error} />
        </Show>

        <fieldset class="min-w-0">
          <legend class="mb-1.5 text-[12px] font-medium text-ink-muted">For how long</legend>
          <div class="flex flex-wrap gap-1">
            <For each={SNOOZE_PRESETS}>
              {(preset) => (
                <label
                  class={cx(
                    "inline-flex cursor-pointer items-center rounded-[4px] border px-2 py-1 text-[12px]",
                    mode() === "duration" && seconds() === preset.seconds
                      ? "border-accent-border bg-accent-fill font-medium text-ink"
                      : "border-line bg-surface text-ink-muted hover:bg-raised",
                  )}
                >
                  <input
                    type="radio"
                    name="snooze-duration"
                    class="sr-only-focusable"
                    checked={mode() === "duration" && seconds() === preset.seconds}
                    onChange={() => {
                      setMode("duration");
                      setSeconds(preset.seconds);
                    }}
                  />
                  {preset.label}
                </label>
              )}
            </For>
          </div>
        </fieldset>

        <Field
          id="snooze-minutes"
          label="Or a custom number of minutes"
          hint={`Between 5 minutes and 30 days. Currently ${fmtDuration(seconds())}.`}
          error={durationError() ?? serverErrors().get("duration_seconds")}
        >
          {(a) => (
            <Input
              {...a}
              type="number"
              min={5}
              max={30 * 24 * 60}
              value={Math.round(seconds() / 60)}
              onInput={(e) => {
                setTouched(true);
                setMode("duration");
                const mins = Number.parseInt(e.currentTarget.value, 10);
                setSeconds(Number.isFinite(mins) ? mins * 60 : Number.NaN);
              }}
            />
          )}
        </Field>

        {/* The absolute form. Choosing it sends `until` and *only* `until`. */}
        <Field
          id="snooze-until"
          label="Or until an exact time"
          hint={
            mode() === "until"
              ? `Notifications resume ${absoluteTime(new Date(until()).toString())}.`
              : "Pick a time to switch to the absolute form. Exactly one of the two is ever sent."
          }
          error={untilError() ?? serverErrors().get("until")}
        >
          {(a) => (
            <Input
              {...a}
              type="datetime-local"
              value={until()}
              onInput={(e) => {
                setTouched(true);
                setMode("until");
                setUntil(e.currentTarget.value);
              }}
            />
          )}
        </Field>

        <Field
          id="snooze-note"
          label="Note (optional)"
          hint="Why this is going quiet. Shown wherever the snooze is shown, and kept in the history afterwards."
          error={noteError() ?? serverErrors().get("note")}
        >
          {(a) => (
            <Textarea
              {...a}
              value={note()}
              rows={2}
              maxLength={2000}
              placeholder="Deploy window, expected until 17:00"
              onInput={(e) => {
                setTouched(true);
                setNote(e.currentTarget.value);
              }}
            />
          )}
        </Field>

        <p class="text-[11px] leading-snug text-ink-subtle">
          {props.subject === "group"
            ? "Every member stays firing, stays whatever severity it was, and stays in the default alert list. A member that cannot be snoozed is skipped rather than failing the request."
            : "A snoozed alert is still firing and is still rendered as firing."}{" "}
          Snoozing suppresses every notification reason for it — including a rule change — except the
          messages announcing the snooze starting and ending, so it can never go quiet silently. Every
          snooze is attributed and stays in the history afterwards.
        </p>
      </DialogBody>
    </Dialog>
  );
};
