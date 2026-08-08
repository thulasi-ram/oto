/**
 * The things a person can do to an alert.
 *
 * The vocabulary here is governed by SCOPE-BOUNDARY and is not negotiable:
 *
 *   - Acknowledging is a **receipt on a signal** — "a human has seen this". It
 *     is never "take ownership", never "I'm on it", never an assignment. An
 *     acknowledged alert is still firing and every string here says so.
 *   - There is no on-call, no incident, no escalation-as-a-human-process.
 *   - What other tools call MTTR is "firing duration"; oto times the signal and
 *     not anybody's response.
 *
 * Validation is local **and** server-side, and the two are not redundant: the
 * local pass (valibot, mirroring the contract's bounds) stops an obviously bad
 * request; the server's `violations[]` is authoritative and is mapped back onto
 * the exact control that failed, JSON Pointer and all.
 */
import { For, Show, createMemo, createSignal, type Component } from "solid-js";
import { useMutation, useQueryClient } from "@tanstack/solid-query";
import * as v from "valibot";

import { ApiError, violationsByField, orphanViolations } from "~/api/client";
import { ackAlert, commentOnAlert, unackAlert } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertDetail, Occurrence } from "~/api/types";
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import { Button, Field, Input, Textarea, cx } from "~/components/ui/primitives";
import { ErrorBanner } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";

/* -------------------------------------------------------------------------- */
/* Local schemas, mirroring the contract's bounds                             */
/* -------------------------------------------------------------------------- */

/** `AckRequest.note` — optional, and bounded the same way the server bounds it. */
const NoteSchema = v.pipe(
  v.string(),
  v.maxLength(2000, "A note is at most 2000 characters."),
);

const CommentSchema = v.pipe(
  v.string(),
  v.trim(),
  v.minLength(1, "A comment needs some text — the timeline is the record."),
  v.maxLength(10_000, "A comment is at most 10 000 characters."),
);

function firstIssue(result: v.SafeParseResult<v.GenericSchema>): string | undefined {
  return result.success ? undefined : result.issues[0]?.message;
}

/* -------------------------------------------------------------------------- */
/* The action bar                                                             */
/* -------------------------------------------------------------------------- */

export interface AlertActionsProps {
  readonly alert: AlertDetail;
}

export const AlertActions: Component<AlertActionsProps> = (props) => {
  const [ackOpen, setAckOpen] = createSignal(false);
  const [commentOpen, setCommentOpen] = createSignal(false);
  const [snoozeOpen, setSnoozeOpen] = createSignal(false);

  const acked = (): boolean => props.alert.ack_state === "acked";

  /**
   * Acking an occurrence that has already ended is a `412` by contract — the
   * request is valid, the entity is simply in the wrong state. Saying so up
   * front is kinder than letting someone discover it by being refused.
   */
  const occurrenceOpen = (): boolean => {
    const occ = props.alert.current_occurrence;
    return occ !== null && occ !== undefined && (occ.ended_at ?? null) === null;
  };

  return (
    <div class="flex flex-wrap items-center gap-2">
      <Show
        when={acked()}
        fallback={
          <Button
            variant="primary"
            size="sm"
            disabled={!occurrenceOpen()}
            title={
              occurrenceOpen()
                ? "Record that a human has seen this. It stays firing."
                : "This episode has already ended, so there is nothing to acknowledge."
            }
            onClick={() => setAckOpen(true)}
          >
            Acknowledge
          </Button>
        }
      >
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setAckOpen(true)}
          title="Withdraw the receipt. Recorded as a deliberate withdrawal, not as an automatic one."
        >
          Withdraw acknowledgement
        </Button>
      </Show>

      <Button size="sm" onClick={() => setCommentOpen(true)}>
        Comment
      </Button>

      <Button
        size="sm"
        onClick={() => setSnoozeOpen(true)}
        title="Stop oto's own notifications for this alert until a fixed time. It keeps firing and stays visible."
      >
        Snooze
      </Button>

      <AckDialog
        alert={props.alert}
        open={ackOpen()}
        onClose={() => setAckOpen(false)}
        withdrawing={acked()}
      />
      <CommentDialog alert={props.alert} open={commentOpen()} onClose={() => setCommentOpen(false)} />
      <SnoozeDialog alert={props.alert} open={snoozeOpen()} onClose={() => setSnoozeOpen(false)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Acknowledge / withdraw                                                     */
/* -------------------------------------------------------------------------- */

const AckDialog: Component<{
  readonly alert: AlertDetail;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly withdrawing: boolean;
}> = (props) => {
  const client = useQueryClient();
  const [note, setNote] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const localError = createMemo(() =>
    touched() ? firstIssue(v.safeParse(NoteSchema, note())) : undefined,
  );

  const mutation = useMutation(() => ({
    mutationFn: (body: { note: string }): Promise<Occurrence> => {
      // One key per gesture. The server's idempotency promise only holds if the
      // client stops re-minting the key on every retry.
      const key = idempotencyKey();
      return props.withdrawing
        ? unackAlert(props.alert.id, body.note, key)
        : ackAlert(props.alert.id, body.note, key);
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.alerts.all() });
      setNote("");
      setTouched(false);
      props.onClose();
    },
  }));

  const serverErrors = (): ReadonlyMap<string, string> => violationsByField(mutation.error);
  const orphans = (): readonly string[] => orphanViolations(mutation.error, ["note"]);

  return (
    <Dialog
      open={props.open}
      onClose={props.onClose}
      width="sm"
      title={props.withdrawing ? "Withdraw acknowledgement" : "Acknowledge this alert"}
      description={
        props.withdrawing
          ? "Recorded as a deliberate withdrawal, which is distinct from the automatic one that happens when a new episode opens."
          : "A receipt that a human has seen this signal. It does not change the alert: it stays firing until the upstream says otherwise."
      }
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            busy={mutation.isPending}
            disabled={localError() !== undefined}
            onClick={() => {
              setTouched(true);
              if (!v.safeParse(NoteSchema, note()).success) return;
              mutation.mutate({ note: note() });
            }}
          >
            {props.withdrawing ? "Withdraw" : "Acknowledge"}
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
            This episode ended before the request landed, so there is nothing to acknowledge. The
            alert may have resolved while this dialog was open.
          </ErrorBanner>
        </Show>

        <Field
          id="ack-note"
          label="Note (optional)"
          hint="Context for whoever reads the timeline next. Immutable once written."
          error={localError() ?? serverErrors().get("note")}
        >
          {(a) => (
            <Textarea
              {...a}
              value={note()}
              maxLength={2000}
              placeholder="Known deploy, rolling back"
              onInput={(e) => {
                setTouched(true);
                setNote(e.currentTarget.value);
              }}
            />
          )}
        </Field>
      </DialogBody>
    </Dialog>
  );
};

/* -------------------------------------------------------------------------- */
/* Comment                                                                    */
/* -------------------------------------------------------------------------- */

const CommentDialog: Component<{
  readonly alert: AlertDetail;
  readonly open: boolean;
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const [body, setBody] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const localError = createMemo(() =>
    touched() ? firstIssue(v.safeParse(CommentSchema, body())) : undefined,
  );

  const mutation = useMutation(() => ({
    mutationFn: (text: string) => commentOnAlert(props.alert.id, text, idempotencyKey()),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.alerts.all() });
      setBody("");
      setTouched(false);
      props.onClose();
    },
  }));

  const serverErrors = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

  return (
    <Dialog
      open={props.open}
      onClose={props.onClose}
      title="Add a comment"
      description="Comments are events like any other: immutable, attributed, and mirrored into the chat thread. They cannot be edited or deleted, because the timeline is the record."
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            busy={mutation.isPending}
            disabled={v.safeParse(CommentSchema, body()).success === false}
            onClick={() => {
              setTouched(true);
              const parsed = v.safeParse(CommentSchema, body());
              if (!parsed.success) return;
              mutation.mutate(parsed.output);
            }}
          >
            Add comment
          </Button>
        </>
      }
    >
      <DialogBody>
        <Show when={mutation.error !== null}>
          <ErrorBanner error={mutation.error} />
        </Show>
        <Field
          id="comment-body"
          label="Comment"
          required
          error={localError() ?? serverErrors().get("body")}
        >
          {(a) => (
            <Textarea
              {...a}
              value={body()}
              rows={5}
              maxLength={10_000}
              placeholder="What you found, what you changed, what to check next time."
              onInput={(e) => {
                setTouched(true);
                setBody(e.currentTarget.value);
              }}
            />
          )}
        </Field>
      </DialogBody>
    </Dialog>
  );
};

/* -------------------------------------------------------------------------- */
/* Snooze                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * §B.8's bounds, mirrored exactly. Minimum 5 minutes, maximum 30 days, and
 * **there is no indefinite snooze** — an unexpiring snooze is a mute, and mutes
 * are how channels die.
 */
const SNOOZE_MIN_SECONDS = 5 * 60;
const SNOOZE_MAX_SECONDS = 30 * 24 * 3600;

const SNOOZE_PRESETS: readonly { readonly label: string; readonly seconds: number }[] = [
  { label: "30 minutes", seconds: 30 * 60 },
  { label: "1 hour", seconds: 3600 },
  { label: "2 hours", seconds: 2 * 3600 },
  { label: "4 hours", seconds: 4 * 3600 },
  { label: "8 hours", seconds: 8 * 3600 },
  { label: "24 hours", seconds: 24 * 3600 },
  { label: "3 days", seconds: 3 * 24 * 3600 },
  { label: "7 days", seconds: 7 * 24 * 3600 },
];

/**
 * ⚠️ **Contract gap.** `api/openapi/openapi.yaml` publishes no snooze endpoint —
 * the string "snooze" does not appear in it anywhere. SPEC §B.8 specifies the
 * feature and §P-13…P-16 list it as *pending* code amendments, so the API is
 * simply not there yet.
 *
 * The rule is "never invent an endpoint", so this form does not call one. It is
 * built to §B.8's bounds and preset durations and is refused at the submit
 * boundary with the reason stated plainly, rather than being omitted — an
 * operator who is told *why* the quiet button is unavailable is better served
 * than one who is left wondering whether they missed it. When
 * `POST /api/v1/alerts/{id}/snooze` lands, this dialog needs a mutation and
 * nothing else.
 */
const SNOOZE_AVAILABLE = false;

const SnoozeDialog: Component<{
  readonly alert: AlertDetail;
  readonly open: boolean;
  readonly onClose: () => void;
}> = (props) => {
  const [seconds, setSeconds] = createSignal(3600);
  const [note, setNote] = createSignal("");

  const until = (): Date => new Date(Date.now() + seconds() * 1000);

  const outOfBounds = (): string | undefined => {
    if (seconds() < SNOOZE_MIN_SECONDS) return "The shortest snooze is 5 minutes.";
    if (seconds() > SNOOZE_MAX_SECONDS) {
      return "The longest snooze is 30 days. There is no indefinite snooze — that would be a mute, and mutes are how channels go quiet forever.";
    }
    return undefined;
  };

  return (
    <Dialog
      open={props.open}
      onClose={props.onClose}
      title="Snooze notifications"
      description="Stops oto's own notifications for this alert until a fixed time. It is not a silence: nothing changes in your cluster, the alert keeps firing, and it stays visible here in full colour."
      footer={
        <>
          <Button size="sm" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            disabled={!SNOOZE_AVAILABLE || outOfBounds() !== undefined}
            title={
              SNOOZE_AVAILABLE
                ? undefined
                : "The API this needs has not shipped yet — see the note below."
            }
          >
            Snooze until {until().toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })}
          </Button>
        </>
      }
    >
      <DialogBody>
        <Show when={!SNOOZE_AVAILABLE}>
          <ErrorBanner>
            <p class="font-medium">oto's API does not serve snooze yet.</p>
            <p class="mt-1 text-ink-muted">
              SPEC §B.8 specifies it and the UI is built to those bounds, but no snooze operation is
              published in the OpenAPI contract, and this UI never calls an endpoint that is not in
              the contract. The controls below are shown so the shape of the feature is honest, and
              they will work unchanged once the endpoint lands.
            </p>
          </ErrorBanner>
        </Show>

        <fieldset class="min-w-0">
          <legend class="mb-1.5 text-[12px] font-medium text-ink-muted">For how long</legend>
          <div class="flex flex-wrap gap-1">
            <For each={SNOOZE_PRESETS}>
              {(preset) => (
                <label
                  class={cx(
                    "inline-flex cursor-pointer items-center rounded-[4px] border px-2 py-1 text-[12px]",
                    seconds() === preset.seconds
                      ? "border-accent-border bg-accent-fill font-medium text-ink"
                      : "border-line bg-surface text-ink-muted hover:bg-raised",
                  )}
                >
                  <input
                    type="radio"
                    name="snooze-duration"
                    class="sr-only-focusable"
                    checked={seconds() === preset.seconds}
                    onChange={() => setSeconds(preset.seconds)}
                  />
                  {preset.label}
                </label>
              )}
            </For>
          </div>
        </fieldset>

        <Field
          id="snooze-custom"
          label="Or a custom number of minutes"
          hint="Between 5 minutes and 30 days."
          error={outOfBounds()}
        >
          {(a) => (
            <Input
              {...a}
              type="number"
              min={5}
              max={30 * 24 * 60}
              value={Math.round(seconds() / 60)}
              onInput={(e) => {
                const mins = Number.parseInt(e.currentTarget.value, 10);
                if (Number.isFinite(mins)) setSeconds(mins * 60);
              }}
            />
          )}
        </Field>

        <Field
          id="snooze-note"
          label="Note (optional)"
          hint="Why this is going quiet, for whoever sees the banner."
        >
          {(a) => (
            <Input
              {...a}
              value={note()}
              maxLength={2000}
              placeholder="Deploy window, expected until 17:00"
              onInput={(e) => setNote(e.currentTarget.value)}
            />
          )}
        </Field>

        <p class="text-[11px] leading-snug text-ink-subtle">
          A snoozed alert is still firing and is still rendered as firing. Snoozing suppresses every
          notification reason for it — including a rule change — except the messages announcing the
          snooze starting and ending, so it can never go quiet silently.
        </p>
      </DialogBody>
    </Dialog>
  );
};
