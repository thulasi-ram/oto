/**
 * oto's quiet button (§B.8), shared by one alert and the case-wide fan-out.
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

import { maxLengthOf, maxValueOf, minValueOf } from "~/api/bounds";
import { ApiError, orphanViolations, violationsByField } from "~/api/client";
import { SnoozeRequestSchema } from "~/api/generated/validators";
import type { SnoozeRequest } from "~/api/types";
import { Button } from "~/components/ui/Button";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { ErrorBanner } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { absoluteTime, duration as fmtDuration, idempotencyKey } from "~/lib/format";
import { createFieldError } from "~/lib/validation";

/* -------------------------------------------------------------------------- */
/* §B.8's bounds and presets, mirrored exactly                                */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ READ FROM `SnoozeRequestSchema`, NOT WRITTEN HERE. §B.8 says the bounds are
 * "identical in this document, in the domain constructor and in the database
 * CHECK constraint" — a fourth copy in the browser is the one that can drift
 * without anything noticing, and it is the copy that decides what an operator is
 * allowed to type. `5 * 60` and `30 * 24 * 3600` were exactly that copy.
 */
export const SNOOZE_MIN_SECONDS = minValueOf(SnoozeRequestSchema, "duration_seconds");
export const SNOOZE_MAX_SECONDS = maxValueOf(SnoozeRequestSchema, "duration_seconds");

/** The note ceiling the server answers 422 with, for the control and the schema. */
const NOTE_MAX = maxLengthOf(SnoozeRequestSchema, "note");

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
/* Form schemas, gated by the generated request schema                        */
/* -------------------------------------------------------------------------- */

/*
 * SPEC §L.8.1: the form schema stays hand-written, because the sentences below
 * are the whole point of it — but it `v.pipe`s into the **generated**
 * `SnoozeRequestSchema` as its final gate, so this form cannot construct a body
 * the API would reject. The generated schema comes from
 * `api/openapi/openapi.yaml` via gate G4 (`npm run gen:validators`).
 */

const NoteSchema = v.pipe(
  v.string(),
  v.maxLength(NOTE_MAX, `A note is at most ${NOTE_MAX} characters.`),
);

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

/** The whole dialog, as one value, so the final gate sees the real request. */
export interface SnoozeForm {
  readonly mode: "duration" | "until";
  readonly seconds: number;
  readonly until: string;
  readonly note: string;
}

/**
 * Exactly one of `until` and `duration_seconds` — both is a 422 and neither is
 * a 422, because there is no indefinite snooze and therefore no default window.
 * The mode toggle is what makes that structural rather than a rule someone has
 * to remember.
 *
 * An unreadable `datetime-local` value becomes `""` rather than throwing, so the
 * generated schema's `isoTimestamp` is what reports it.
 */
function toSnoozeRequest(form: SnoozeForm): v.InferInput<typeof SnoozeRequestSchema> {
  const at = new Date(form.until);
  const body: v.InferInput<typeof SnoozeRequestSchema> =
    form.mode === "duration"
      ? { duration_seconds: form.seconds }
      : { until: Number.isNaN(at.getTime()) ? "" : at.toISOString() };
  const text = form.note.trim();
  return text === "" ? body : { ...body, note: text };
}

const SnoozeFormSchema = v.pipe(
  v.strictObject({
    mode: v.picklist(["duration", "until"]),
    seconds: v.number("Give a number of minutes."),
    until: v.string(),
    note: NoteSchema,
  }),
  // Only the branch actually being sent is checked, which is why neither field
  // carries its bounds above. `duration_seconds` needs nothing here: the
  // generated schema already holds it to the contract's 300…2592000. The
  // absolute form does, because the 5-minute/30-day window on `until` is prose
  // in the contract and prose does not generate.
  v.check(
    (form) => form.mode !== "until" || v.safeParse(UntilSchema, form.until).success,
    "Pick a time between 5 minutes and 30 days from now.",
  ),
  v.transform(toSnoozeRequest),
  SnoozeRequestSchema, // the generated schema is the final gate
);

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

/**
 * ⛔ `"case"` HERE IS THE **CORRELATION** — what the UI calls a Case and the API
 * still calls an alert group. It is NOT `AlertCase`, the per-alert firing episode
 * (`internal/alerts/domain/case.go`). The two words collide in the codebase by an
 * accepted decision; on screen they never do, because the episode is only ever
 * called an *episode*.
 */
export type SnoozeSubject = "alert" | "case";

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
  case: "Stops oto's own notifications for every alert currently in this case, until a fixed time. Alerts that join later are not snoozed — a snooze is never predictive.",
};

export const SnoozeDialog: Component<SnoozeDialogProps> = (props) => {
  const [mode, setMode] = createSignal<"duration" | "until">("duration");
  const [seconds, setSeconds] = createSignal(3600);
  const [until, setUntil] = createSignal(toLocalInputValue(new Date(Date.now() + 3_600_000)));
  const [note, setNote] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const durationError = createFieldError(DurationSchema, seconds, () => mode() === "duration" && touched());
  const untilError = createFieldError(UntilSchema, until, () => mode() === "until" && touched());
  const noteError = createFieldError(NoteSchema, note, touched);

  const form = (): SnoozeForm => ({
    mode: mode(),
    seconds: seconds(),
    until: until(),
    note: note(),
  });

  /**
   * One parse, through the generated request schema. The per-field schemas
   * above only decide which sentence a control shows; this decides whether the
   * request may leave the browser at all.
   */
  const gated = createMemo(() => v.safeParse(SnoozeFormSchema, form()));

  const valid = (): boolean => gated().success;

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
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {props.subject === "case" ? "Snooze every current member" : "Snooze notifications"}
          </ModalTitle>
          <ModalDescription>{SUBJECT_DESCRIPTION[props.subject]}</ModalDescription>
        </ModalHeader>

        <div class="flex flex-col gap-md text-item leading-relaxed text-ink">
          <Show when={mutation.error !== null && orphans().length > 0}>
            <ErrorBanner>
              <For each={orphans()}>{(msg) => <p>{msg}</p>}</For>
            </ErrorBanner>
          </Show>

          <Show when={mutation.error instanceof ApiError && mutation.error.status === 412}>
            <ErrorBanner>
              {props.subject === "case"
                ? "There is nothing here to snooze — this case has no currently-joined member. A snooze is never predictive, so there is nothing for it to attach to."
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
            <legend class="mb-xs text-body font-medium text-ink-muted">For how long</legend>
            <div class="flex flex-wrap gap-2xs">
              <For each={SNOOZE_PRESETS}>
                {(preset) => (
                  <label
                    class={cn(
                      "inline-flex cursor-pointer items-center rounded-control border px-sm py-2xs text-body",
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

          <TextField
            value={String(Math.round(seconds() / 60))}
            validationState={
              (durationError() ?? serverErrors().get("duration_seconds")) ? "invalid" : "valid"
            }
            onChange={(value) => {
              setTouched(true);
              setMode("duration");
              const mins = Number.parseInt(value, 10);
              setSeconds(Number.isFinite(mins) ? mins * 60 : Number.NaN);
            }}
          >
            <TextFieldLabel>Or a custom number of minutes</TextFieldLabel>
            <TextFieldInput type="number" min={SNOOZE_MIN_SECONDS / 60} max={SNOOZE_MAX_SECONDS / 60} />
            <TextFieldDescription>
              {`Between 5 minutes and 30 days. Currently ${fmtDuration(seconds())}.`}
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {durationError() ?? serverErrors().get("duration_seconds")}
            </TextFieldErrorMessage>
          </TextField>

          {/* The absolute form. Choosing it sends `until` and *only* `until`. */}
          <TextField
            value={until()}
            validationState={(untilError() ?? serverErrors().get("until")) ? "invalid" : "valid"}
            onChange={(value) => {
              setTouched(true);
              setMode("until");
              setUntil(value);
            }}
          >
            <TextFieldLabel>Or until an exact time</TextFieldLabel>
            <TextFieldInput type="datetime-local" />
            <TextFieldDescription>
              {mode() === "until"
                ? `Notifications resume ${absoluteTime(new Date(until()).toString())}.`
                : "Pick a time to switch to the absolute form. Exactly one of the two is ever sent."}
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {untilError() ?? serverErrors().get("until")}
            </TextFieldErrorMessage>
          </TextField>

          <TextField
            value={note()}
            validationState={(noteError() ?? serverErrors().get("note")) ? "invalid" : "valid"}
            onChange={(value) => {
              setTouched(true);
              setNote(value);
            }}
          >
            <TextFieldLabel>Note (optional)</TextFieldLabel>
            <TextFieldTextArea
              rows={2}
              maxLength={NOTE_MAX}
              placeholder="Deploy window, expected until 17:00"
            />
            <TextFieldDescription>
              Why this is going quiet. Shown wherever the snooze is shown, and kept in the history
              afterwards.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {noteError() ?? serverErrors().get("note")}
            </TextFieldErrorMessage>
          </TextField>

          <p class="text-meta leading-snug text-ink-subtle">
            {props.subject === "case"
              ? "Every member stays firing, stays whatever severity it was, and stays in the default alert list. A member that cannot be snoozed is skipped rather than failing the request."
              : "A snoozed alert is still firing and is still rendered as firing."}{" "}
            Snoozing suppresses every notification reason for it — including a rule change — except the
            messages announcing the snooze starting and ending, so it can never go quiet silently. Every
            snooze is attributed and stays in the history afterwards.
          </p>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={touched() && !valid()}
            onClick={() => {
              setTouched(true);
              const parsed = gated();
              if (!parsed.success) return;
              mutation.mutate(parsed.output);
            }}
          >
            Hold notifications until {endsLabel()}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
