/**
 * The things a person can do to one alert.
 *
 * ⛔ ACKNOWLEDGING IS NOT HERE, AND ITS ABSENCE IS THE DECISION. A receipt
 * belongs to ONE FIRING — a Case — not to the identity, because an identity
 * outlives its firings and "seen" would go on being true about a firing nobody
 * has looked at. So it is written on the case, and ONLY there: from `/cases` and
 * from `/cases/:id`, both addressed by case id (`POST /api/v1/cases/{id}/ack`).
 * The alert list offers it nowhere — a row there is an identity too. The cases
 * this alert has had are listed on this very screen, each one a link to where
 * its receipt is written.
 *
 * ⭐ SNOOZING **IS** HERE, AND FOR THE MIRROR-IMAGE REASON. A snooze holds oto's
 * own notifications for the IDENTITY until a fixed time: it does not end when the
 * firing you took it from ends, and it covers whatever fires next under the same
 * labels. Its subject is therefore an Alert, and this is the one screen whose
 * whole heading is an Alert. It used to be offered from `/cases/:id` instead,
 * which put a decision that outlives a case behind a case-shaped title — the
 * scope was right and the address was a guess about which one you meant.
 *
 * ⛔ ENDING A SNOOZE IS NOT HERE, AND THAT ASYMMETRY IS DELIBERATE. `Resume` lives
 * on the **Quiet** tab of `/alerts` — the list of what oto is currently not
 * saying — because waking something is only safe when you can see the whole set
 * you are waking it out of. A resume button here would let an alert leave that
 * list from a screen that never showed it was on it.
 *
 * Comment stays. A comment is a note about *this signal* — what this one alert
 * was doing, what its labels said.
 *
 * The vocabulary that remains is governed by SCOPE-BOUNDARY and is not
 * negotiable: there is no on-call, no incident, no escalation-as-a-human-process,
 * and what other tools call MTTR is "firing duration" — oto times the signal and
 * not anybody's response.
 *
 * Validation is local **and** server-side, and the two are not redundant: the
 * local pass (valibot, mirroring the contract's bounds) stops an obviously bad
 * request; the server's `violations[]` is authoritative and is mapped back onto
 * the exact control that failed, JSON Pointer and all.
 */
import { Show, createMemo, createSignal, type Component } from "solid-js";
import { useMutation, useQueryClient } from "@tanstack/solid-query";
import * as v from "valibot";

import { maxLengthOf, minLengthOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import { commentOnAlert, snoozeAlert } from "~/api/endpoints";
import { CommentRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { AlertDetail, SnoozeRequest } from "~/api/types";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";
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
  TextFieldErrorMessage,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { ErrorBanner } from "~/components/ui/states";
import { idempotencyKey } from "~/lib/format";
import { createFieldError } from "~/lib/validation";

/* -------------------------------------------------------------------------- */
/* Form schema, gated by the generated request schema                         */
/* -------------------------------------------------------------------------- */

/*
 * SPEC §L.8.1: the form schema stays hand-written — it carries the sentences a
 * person should read — but it must `v.pipe` into the **generated** request
 * schema, so the form can never accept something the API would reject. The
 * generated schema comes from `api/openapi/openapi.yaml` via gate G4
 * (`npm run gen:validators`), which is why the bounds below are not the last
 * word on anything.
 */

/**
 * ⛔ THE CEILING IS READ, NOT WRITTEN. `10_000` was typed twice by hand — once
 * into a `v.maxLength` and once into a `maxLength` attribute — which is two
 * copies of a number the server owns. They come off the generated request
 * schema, which is the same schema this form pipes into below.
 */
const COMMENT_MIN = minLengthOf(CommentRequestSchema, "body");
const COMMENT_MAX = maxLengthOf(CommentRequestSchema, "body");

const CommentSchema = v.pipe(
  v.string(),
  v.trim(),
  v.minLength(COMMENT_MIN, "A comment needs some text — the timeline is the record."),
  v.maxLength(COMMENT_MAX, `A comment is at most ${COMMENT_MAX.toLocaleString("en")} characters.`),
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
  const [commentOpen, setCommentOpen] = createSignal(false);
  const [snoozeOpen, setSnoozeOpen] = createSignal(false);

  /** Snooze is a third orthogonal axis, so this is read beside state, not from it. */
  const snoozed = (): boolean => (props.alert.snooze ?? null) !== null;

  const invalidate = (): void => {
    void client.invalidateQueries({ queryKey: qk.alerts.all() });
  };

  return (
    /*
     * ⛔ PERSISTENT, NEVER HOVER-REVEALED (§0.4). Every control below is on screen
     * from first paint whether or not a pointer is anywhere near it. A bar that
     * materialises under the cursor, on a screen whose contents change under
     * SSE, is a misclick generator at 3am — and the thing it would misclick is a
     * decision to make oto go quiet.
     *
     * The column, rather than a single wrapping row, is what keeps the buttons
     * stable as well as visible: anything that appears underneath them appears
     * *underneath* them, and nothing moves sideways.
     */
    <div class="flex flex-col items-end gap-sm">
      <div class="flex flex-wrap items-center justify-end gap-sm">
        <Button variant="secondary" onClick={() => setCommentOpen(true)}>
          Comment
        </Button>

        {/* ⛔ EVERYTHING PAST THIS RULE TAKES SOMETHING AWAY, so it is put out of
            the cursor's way on purpose: snoozing makes oto stop saying anything
            about this alert. It may not sit flush against the button people reach
            for by reflex, and the gap plus the hairline are the whole mechanism —
            it opens a dialog, so it is never one click from done. */}
        <span aria-hidden="true" class="mx-xs h-6 w-px shrink-0 bg-line" />

        {/* ⛔ THE BUTTON NAMES THE ALERT BECAUSE THE ALERT IS ITS SUBJECT, and
            §B.8.6 forbids a no-op: snoozing something already quiet is not
            refused by the contract — the incumbent hold is closed and replaced —
            so the control stays live and the title says which of the two it is
            about to do. */}
        <Button
          variant="destructive"
          onClick={() => setSnoozeOpen(true)}
          title={
            snoozed()
              ? "This alert is already quiet. Snoozing again replaces the running hold with a new window; ending one early is on the Quiet tab of the alert list."
              : "Stop oto's own notifications for this alert until a fixed time. It keeps firing, keeps its severity, and stays visible."
          }
        >
          Snooze
        </Button>
      </div>

      <CommentDialog
        alert={props.alert}
        open={commentOpen()}
        onClose={() => setCommentOpen(false)}
      />
      <SnoozeDialog
        open={snoozeOpen()}
        onClose={() => setSnoozeOpen(false)}
        onSubmit={(body: SnoozeRequest, key: string) => snoozeAlert(props.alert.id, body, key)}
        onSuccess={invalidate}
      />
    </div>
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
