/**
 * The things a person can do to one alert.
 *
 * ⛔ ACKNOWLEDGING AND SNOOZING ARE NOT HERE ANY MORE. THEY LIVE ON THE CASE
 * (`routes/case-detail.tsx`), and moving them was the point rather than a side
 * effect. A case is the unit a human actually responds to — forty pods
 * crash-looping is one thing happening, not forty — so a receipt and a decision
 * to go quiet are decisions about the case. Offering them per alert invited an
 * operator to acknowledge forty rows one at a time and to leave the
 * thirty-ninth un-acked, which reads to everyone else as "nobody has looked at
 * this yet". Both controls are wired to the group-scoped endpoints there
 * (`POST /api/v1/alert-groups/{id}/ack|snooze|unsnooze`), which fan out one
 * receipt or one quiet period per currently-joined member.
 *
 * Comment stays. A comment is a note about *this signal* — what this one alert
 * was doing, what its labels said — and it is the one thing here that does not
 * generalise to the case: fanning one sentence out across forty members would
 * write forty copies of a remark that was true of one of them.
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
import { commentOnAlert } from "~/api/endpoints";
import { CommentRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { AlertDetail } from "~/api/types";
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
  const [commentOpen, setCommentOpen] = createSignal(false);

  return (
    /*
     * ⛔ PERSISTENT, NEVER HOVER-REVEALED (§0.4). The control below is on screen
     * from first paint whether or not a pointer is anywhere near it. A bar that
     * materialises under the cursor, on a screen whose contents change under
     * SSE, is a misclick generator at 3am.
     *
     * The column survives the loss of its siblings on purpose: the alert detail
     * gains controls again the moment something belongs to one signal rather
     * than to the case above it, and a `flex-col` that already reflows nothing
     * is what stops the next one arriving as a layout change.
     */
    <div class="flex flex-col items-end gap-sm">
      <div class="flex flex-wrap items-center justify-end gap-sm">
        <Button variant="secondary" onClick={() => setCommentOpen(true)}>
          Comment
        </Button>
      </div>

      <CommentDialog
        alert={props.alert}
        open={commentOpen()}
        onClose={() => setCommentOpen(false)}
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
