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
 *   - Snoozing holds **oto's own notifications** and says nothing about the
 *     signal, so the button next to it never reads "mute", "silence" or
 *     "resolve", and the alert beside it keeps its state and its severity.
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
import { ackAlert, commentOnAlert, snoozeAlert, unackAlert, unsnoozeAlert } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { AlertDetail, Occurrence, SnoozeRequest } from "~/api/types";
import { Dialog, DialogBody } from "~/components/ui/Dialog";
import { Button, Field, Textarea } from "~/components/ui/primitives";
import { ErrorBanner } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";

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
  const client = useQueryClient();
  const [ackOpen, setAckOpen] = createSignal(false);
  const [commentOpen, setCommentOpen] = createSignal(false);
  const [snoozeOpen, setSnoozeOpen] = createSignal(false);

  const acked = (): boolean => props.alert.ack_state === "acked";

  /** Snooze is a third orthogonal axis, so this is read beside state, not from it. */
  const snoozed = (): boolean => (props.alert.snooze ?? null) !== null;

  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  const unsnooze = useMutation(() => ({
    mutationFn: () => unsnoozeAlert(props.alert.id, undefined, idempotencyKey()),
    onSuccess: invalidate,
  }));

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

      {/* Buttons are never no-ops (§B.8.6): while a snooze holds, the control
          is the one that ends it. */}
      <Show
        when={snoozed()}
        fallback={
          <Button
            size="sm"
            onClick={() => setSnoozeOpen(true)}
            title="Stop oto's own notifications for this alert until a fixed time. It keeps firing, keeps its severity, and stays visible."
          >
            Snooze
          </Button>
        }
      >
        <Button
          size="sm"
          busy={unsnooze.isPending}
          onClick={() => unsnooze.mutate()}
          title="Resume oto's notifications now. The wake-up card reflects the alert's state now, not a replay of what was suppressed."
        >
          Resume notifications
        </Button>
      </Show>

      <Show when={unsnooze.error !== null}>
        <span class="text-[11px] leading-snug text-ink">
          {unsnooze.error instanceof ApiError && unsnooze.error.status === 412
            ? "This alert is not snoozed — it woke before the request landed."
            : (unsnooze.error as Error | null)?.message}
        </span>
      </Show>

      <AckDialog
        alert={props.alert}
        open={ackOpen()}
        onClose={() => setAckOpen(false)}
        withdrawing={acked()}
      />
      <CommentDialog alert={props.alert} open={commentOpen()} onClose={() => setCommentOpen(false)} />
      <SnoozeDialog
        open={snoozeOpen()}
        onClose={() => setSnoozeOpen(false)}
        subject="alert"
        onSubmit={(body: SnoozeRequest, key: string) => snoozeAlert(props.alert.id, body, key)}
        onSuccess={invalidate}
      />
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
