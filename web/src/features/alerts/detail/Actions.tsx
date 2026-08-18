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

import { maxLengthOf, minLengthOf } from "~/api/bounds";
import { ApiError, violationsByField, orphanViolations } from "~/api/client";
import { ackAlert, commentOnAlert, snoozeAlert, unackAlert, unsnoozeAlert } from "~/api/endpoints";
import {
  AckRequestSchema,
  CommentRequestSchema,
  UnackRequestSchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { AlertDetail, Case, SnoozeRequest } from "~/api/types";
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
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { ErrorBanner } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";
import { createFieldError } from "~/lib/validation";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";

/* -------------------------------------------------------------------------- */
/* Form schemas, gated by the generated request schemas                       */
/* -------------------------------------------------------------------------- */

/*
 * SPEC §L.8.1: form schemas stay hand-written — they carry the sentences a
 * person should read — but each one must `v.pipe` into the **generated**
 * request schema, so a form can never accept something the API would reject.
 * The generated schemas come from `api/openapi/openapi.yaml` via gate G4
 * (`npm run gen:validators`), which is why the bounds below are not the last
 * word on anything.
 */

/**
 * ⛔ THE CEILINGS ARE READ, NOT WRITTEN. `2000` and `10_000` were each typed
 * twice by hand — once into a `v.maxLength` and once into a `maxLength`
 * attribute — which is four copies of two numbers the server owns. They come off
 * the generated request schemas, which are the same schemas these forms pipe
 * into below.
 */
const NOTE_MAX = maxLengthOf(AckRequestSchema, "note");
const COMMENT_MIN = minLengthOf(CommentRequestSchema, "body");
const COMMENT_MAX = maxLengthOf(CommentRequestSchema, "body");

/** The per-field rule, so a control can show one sentence about itself. */
const NoteSchema = v.pipe(
  v.string(),
  v.maxLength(NOTE_MAX, `A note is at most ${NOTE_MAX} characters.`),
);

const CommentSchema = v.pipe(
  v.string(),
  v.trim(),
  v.minLength(COMMENT_MIN, "A comment needs some text — the timeline is the record."),
  v.maxLength(COMMENT_MAX, `A comment is at most ${COMMENT_MAX.toLocaleString("en")} characters.`),
);

/**
 * The body an ack or an unack takes. Both endpoints take the same one, and both
 * generated schemas are consulted separately anyway — they are two endpoints and
 * they are free to diverge.
 */
type NoteBody = v.InferInput<typeof AckRequestSchema>;

/**
 * An empty note is *absent*, not `""`. The contract has no "cleared" note, and
 * sending a blank one would put an empty line on the timeline forever.
 */
function toNoteBody(form: { readonly note: string }): NoteBody {
  const note = form.note.trim();
  return note === "" ? {} : { note };
}

const AckFormSchema = v.pipe(
  v.strictObject({ note: NoteSchema }),
  v.transform(toNoteBody),
  AckRequestSchema, // the generated schema is the final gate
);

const UnackFormSchema = v.pipe(
  v.strictObject({ note: NoteSchema }),
  v.transform(toNoteBody),
  UnackRequestSchema, // the generated schema is the final gate
);

const CommentFormSchema = v.pipe(
  v.strictObject({ body: CommentSchema }),
  v.transform((form): v.InferInput<typeof CommentRequestSchema> => ({ body: form.body })),
  CommentRequestSchema, // the generated schema is the final gate
);

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

  const acked = (): boolean => (props.alert.current_case?.ack_state ?? null) === "acked";

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
   * Acking a case that has already ended is a `412` by contract — the
   * request is valid, the entity is simply in the wrong state. Saying so up
   * front is kinder than letting someone discover it by being refused.
   */
  const caseOpen = (): boolean => {
    const ac = props.alert.current_case;
    return ac !== null && ac !== undefined && (ac.ended_at ?? null) === null;
  };

  return (
    /*
     * ⛔ PERSISTENT, NEVER HOVER-REVEALED (§0.4). Every control below is on
     * screen from first paint whether or not a pointer is anywhere near it. A
     * bar that materialises under the cursor, on a screen whose contents change
     * under SSE, is a misclick generator at 3am — and the thing it would
     * misclick is a receipt on a signal or a decision to make oto go quiet.
     *
     * The column, rather than a single wrapping row, is what makes the buttons
     * *stable* as well as visible: `unsnooze`'s refusal is the one failure here
     * with no dialog to land in, and inside the row it used to reflow the
     * buttons sideways the instant it appeared. Now it appears underneath them
     * and nothing moves.
     */
    <div class="flex flex-col items-end gap-sm">
      <div class="flex flex-wrap items-center justify-end gap-sm">
        {/* The primary. It is the one accented control on this screen, and the
            only one an operator should be able to hit without thinking. */}
        <Show when={!acked()}>
          <Button
            variant="default"
            disabled={!caseOpen()}
            title={
              caseOpen()
                ? "Record that a human has seen this. It stays firing."
                : "This episode has already ended, so there is nothing to acknowledge."
            }
            onClick={() => setAckOpen(true)}
          >
            Acknowledge
          </Button>
        </Show>

        <Button variant="secondary" onClick={() => setCommentOpen(true)}>
          Comment
        </Button>

        {/* ⛔ EVERYTHING PAST THIS RULE TAKES SOMETHING AWAY, so it is put out
            of the cursor's way on purpose. Withdrawing removes a receipt other
            people are reading; snoozing makes oto stop saying anything. Neither
            may sit flush against the button people reach for by reflex, and
            neither wears the accent. The gap and the hairline are the whole
            mechanism — both still open a dialog, so neither is one click from
            done. */}
        <span aria-hidden="true" class="mx-xs h-6 w-px shrink-0 bg-line" />

        <Show when={acked()}>
          <Button
            variant="destructive"
            onClick={() => setAckOpen(true)}
            title="Withdraw the receipt. Recorded as a deliberate withdrawal, not as an automatic one."
          >
            Withdraw acknowledgement
          </Button>
        </Show>

        {/* Buttons are never no-ops (§B.8.6): while a snooze holds, the control
            is the one that ends it. Resuming gives notifications back rather
            than taking them away, so it is the one control on this side of the
            rule that is not held at arm's length by its styling. */}
        <Show
          when={snoozed()}
          fallback={
            <Button
              variant="destructive"
              onClick={() => setSnoozeOpen(true)}
              title="Stop oto's own notifications for this alert until a fixed time. It keeps firing, keeps its severity, and stays visible."
            >
              Snooze
            </Button>
          }
        >
          <Button
            variant="secondary"
            busy={unsnooze.isPending}
            onClick={() => unsnooze.mutate()}
            title="Resume oto's notifications now. The wake-up card reflects the alert's state now, not a replay of what was suppressed."
          >
            Resume notifications
          </Button>
        </Show>
      </div>

      {/* Unlike every other failure on this screen, this one has no dialog to
          appear inside — it lands in the bar, under a button that still reads
          "Resume notifications". Without `role="alert"` nothing moves and nothing
          is announced, so a screen reader user presses the button and hears
          silence, which is the one thing oto is not allowed to do about a
          failure. `role="alert"` is the same idiom the other 17 refusal sites
          use (AppShell's sign-out failure, `ErrorBanner`, `Field`). */}
      <Show when={unsnooze.error !== null}>
        <span role="alert" class="max-w-96 text-right text-meta leading-snug text-ink">
          {unsnooze.error instanceof ApiError && unsnooze.error.status === 412
            ? "This alert is not snoozed — it woke before the request landed."
            : (unsnooze.error as Error | null)?.message}
        </span>
      </Show>

      {/* Three Modals mounted side by side, each its own `Modal` root. Unlike
          `Dialog.tsx`'s native `<dialog>` (which has to stay mounted whether
          open or closed so `showModal()`/`close()` have an element to act on,
          which is what forced that file's per-instance `titleId`/`descId`),
          Kobalte's presence-based `ModalContent` only renders the one that is
          actually open — so there is no sibling markup for an
          `aria-labelledby`/`aria-describedby` id to collide with in the first
          place. */}
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

  const localError = createFieldError(NoteSchema, note, touched);

  /** Ack and unack are two endpoints and therefore two generated gates. */
  const formSchema = (): typeof AckFormSchema | typeof UnackFormSchema =>
    props.withdrawing ? UnackFormSchema : AckFormSchema;

  const mutation = useMutation(() => ({
    mutationFn: (body: NoteBody): Promise<Case> => {
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
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      {/* `max-w-96` (24rem) — narrower than `ModalContent`'s 32rem default,
          because this dialog is one note field. NOT `max-w-sm`: a named width
          key resolves against the spacing namespace before the container one,
          which compiled this dialog down to an 8px sliver. See `Modal.tsx`. */}
      <ModalContent class="max-w-96">
        <ModalHeader>
          <ModalTitle>
            {props.withdrawing ? "Withdraw acknowledgement" : "Acknowledge this alert"}
          </ModalTitle>
          <ModalDescription>
            {props.withdrawing
              ? "Recorded as a deliberate withdrawal, which is distinct from the automatic one that happens when a new episode opens."
              : "A receipt that a human has seen this signal. It does not change the alert: it stays firing until the upstream says otherwise."}
          </ModalDescription>
        </ModalHeader>

        <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
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

          <TextField
            value={note()}
            validationState={(localError() ?? serverErrors().get("note")) ? "invalid" : "valid"}
            onChange={(value) => {
              setTouched(true);
              setNote(value);
            }}
          >
            <TextFieldLabel>Note (optional)</TextFieldLabel>
            <TextFieldTextArea maxLength={NOTE_MAX} placeholder="Known deploy, rolling back" />
            <TextFieldDescription>
              Context for whoever reads the timeline next. Immutable once written.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {localError() ?? serverErrors().get("note")}
            </TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={localError() !== undefined}
            onClick={() => {
              setTouched(true);
              const parsed = v.safeParse(formSchema(), { note: note() });
              if (!parsed.success) return;
              mutation.mutate(parsed.output);
            }}
          >
            {props.withdrawing ? "Withdraw" : "Acknowledge"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
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

  const localError = createFieldError(CommentSchema, body, touched);

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

  // Recomputed on every keystroke (it drives `disabled`) and read again at
  // submit time — memoized so the one parse of `body()` is shared instead of
  // done twice.
  const parsed = createMemo(() => v.safeParse(CommentFormSchema, { body: body() }));

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Add a comment</ModalTitle>
          <ModalDescription>
            Comments are events like any other: immutable, attributed, and mirrored into the chat
            thread. They cannot be edited or deleted, because the timeline is the record.
          </ModalDescription>
        </ModalHeader>

        <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <TextField
            value={body()}
            validationState={(localError() ?? serverErrors().get("body")) ? "invalid" : "valid"}
            onChange={(value) => {
              setTouched(true);
              setBody(value);
            }}
          >
            <TextFieldLabel>
              Comment
              <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                *
              </span>
            </TextFieldLabel>
            <TextFieldTextArea
              rows={5}
              maxLength={COMMENT_MAX}
              placeholder="What you found, what you changed, what to check next time."
            />
            <TextFieldErrorMessage role="alert">
              {localError() ?? serverErrors().get("body")}
            </TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={parsed().success === false}
            onClick={() => {
              setTouched(true);
              const result = parsed();
              if (!result.success) return;
              mutation.mutate(result.output.body);
            }}
          >
            Add comment
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
