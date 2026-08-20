/**
 * The receipt dialog, shared the way `SnoozeDialog` is shared.
 *
 * The vocabulary here is governed by SCOPE-BOUNDARY and is not negotiable:
 * acknowledging is a **receipt on a signal** — "a human has seen this". It is
 * never "take ownership", never "I'm on it", never an assignment. An acknowledged
 * alert is still firing and every string below says so.
 *
 * ⛔ IT LIVES HERE RATHER THAN ON THE SCREEN THAT OPENS IT, because two screens
 * would otherwise grow two dialogs that agree today and drift tomorrow — and what
 * would drift is the sentence explaining that a receipt does not change the
 * signal. `SnoozeDialog` beside it made the same call for the same reason.
 *
 * ⛔ IT NEVER RUNS THE REQUEST ITSELF. The caller hands in `onSubmit` — `POST
 * /cases/{id}/ack`, or `…/unack` when `withdrawing` — and the idempotency key is
 * minted here, once per gesture, so a caller cannot re-mint one on a retry.
 *
 * ⚠️ `withdrawing` GATES THE REQUEST ON THE CONTRACT'S `UnackRequest` rather than
 * `AckRequest`, which is a second generated schema and not a cosmetic flag.
 *
 * Validation is local **and** server-side, and the two are not redundant: the
 * local pass (valibot, mirroring the contract's bounds) stops an obviously bad
 * request; the server's `violations[]` is authoritative and is mapped back onto
 * the exact control that failed, JSON Pointer and all.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation } from "@tanstack/solid-query";
import * as v from "valibot";

import { maxLengthOf } from "~/api/bounds";
import { ApiError, orphanViolations, violationsByField } from "~/api/client";
import { AckRequestSchema, UnackRequestSchema } from "~/api/generated/validators";
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

/* -------------------------------------------------------------------------- */
/* Form schemas, gated by the generated request schemas                       */
/* -------------------------------------------------------------------------- */

/*
 * SPEC §L.8.1: form schemas stay hand-written — they carry the sentences a
 * person should read — but each one must `v.pipe` into the **generated** request
 * schema, so a form can never accept something the API would reject. The
 * generated schemas come from `api/openapi/openapi.yaml` via gate G4
 * (`npm run gen:validators`), which is why the bound below is not the last word
 * on anything.
 */

/**
 * ⛔ THE CEILING IS READ, NOT WRITTEN. `2000` was typed twice by hand — once into
 * a `v.maxLength` and once into a `maxLength` attribute — which is two copies of a
 * number the server owns. It comes off the generated request schema, which is the
 * same schema this form pipes into below.
 */
const NOTE_MAX = maxLengthOf(AckRequestSchema, "note");

/** The per-field rule, so the control can show one sentence about itself. */
const NoteSchema = v.pipe(
  v.string(),
  v.maxLength(NOTE_MAX, `A note is at most ${NOTE_MAX} characters.`),
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

/* -------------------------------------------------------------------------- */
/* The dialog                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ A CASE IS THE ONLY SUBJECT, AND SINCE git-bug 7570090 IT IS THE ONLY ONE
 * THERE HAS EVER BEEN A SECOND CANDIDATE FOR. A Case is one contiguous firing
 * episode of one alert, and it is the thing a human can have seen — which is
 * why the endpoint is `POST /cases/{id}/ack` and why the receipt clears itself
 * when the next episode opens. There is no receipt on an Alert: an identity
 * outlives its firings, so "seen" would go on saying so about a firing nobody
 * has looked at. The `subject` prop that used to pick between a case and an
 * AlertGroup fan-out went with the AlertGroup.
 */
export interface AckDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  /** Withdrawing a receipt rather than writing one. Gated by a second schema. */
  readonly withdrawing?: boolean;
  /**
   * Runs the real mutation. One idempotency key per gesture, minted here — the
   * server's idempotency promise only holds if the client stops re-minting the
   * key on every retry.
   */
  readonly onSubmit: (note: string | undefined, key: string) => Promise<unknown>;
  /** Invalidate whatever the caller needs invalidated. */
  readonly onSuccess: () => void;
}

const TITLE = "Acknowledge this case";

const DESCRIPTION =
  "A receipt that a human has seen this firing. It does not change the alert: it stays firing until the upstream says otherwise, and the receipt clears itself when the next firing opens.";

const WITHDRAW_DESCRIPTION =
  "Recorded as a deliberate withdrawal, which is distinct from the automatic one that happens when the next firing opens.";

export const AckDialog: Component<AckDialogProps> = (props) => {
  const [note, setNote] = createSignal("");
  const [touched, setTouched] = createSignal(false);

  const localError = createFieldError(NoteSchema, note, touched);

  /** Ack and unack are two endpoints and therefore two generated gates. */
  const formSchema = (): typeof AckFormSchema | typeof UnackFormSchema =>
    props.withdrawing === true ? UnackFormSchema : AckFormSchema;

  const mutation = useMutation(() => ({
    mutationFn: (body: NoteBody): Promise<unknown> =>
      props.onSubmit(body.note, idempotencyKey()),
    onSuccess: () => {
      setNote("");
      setTouched(false);
      props.onSuccess();
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
            {props.withdrawing === true ? "Withdraw acknowledgement" : TITLE}
          </ModalTitle>
          <ModalDescription>
            {props.withdrawing === true ? WITHDRAW_DESCRIPTION : DESCRIPTION}
          </ModalDescription>
        </ModalHeader>

        <div class="flex flex-col gap-3 text-item leading-relaxed text-ink">
          <Show when={mutation.error !== null && orphans().length > 0}>
            <ErrorBanner>
              <For each={orphans()}>{(msg) => <p>{msg}</p>}</For>
            </ErrorBanner>
          </Show>

          {/* ⛔ THE 412 SENTENCE NAMES THE VERB THAT WAS REFUSED. A withdrawal
              answered with "there is nothing here to acknowledge" tells the operator
              the opposite of what happened, and `no_open_case` is exactly the
              refusal both directions share — so the word has to come from the mode
              rather than from the status. */}
          <Show when={mutation.error instanceof ApiError && mutation.error.status === 412}>
            <ErrorBanner>
              <Switch>
                <Match when={props.withdrawing === true}>
                  This case ended before the request landed, so there is no receipt left to
                  withdraw. The alert may have resolved while this dialog was open.
                </Match>
                <Match when={true}>
                  This case ended before the request landed, so there is nothing to acknowledge. The
                  alert may have resolved while this dialog was open.
                </Match>
              </Switch>
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
            {props.withdrawing === true ? "Withdraw" : "Acknowledge"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
