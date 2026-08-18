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
 * ⛔ IT NEVER RUNS THE REQUEST ITSELF. The caller hands in `onSubmit`, because
 * the receipt is written by a different endpoint depending on what is being
 * acknowledged — `POST /alerts/{id}/ack` for one alert, or
 * `POST /alert-groups/{id}/ack` for a whole case, and `…/unack` for either when
 * `withdrawing` — and the idempotency key is minted here, once per gesture, so a
 * caller cannot re-mint one on a retry.
 *
 * ⚠️ `withdrawing` IS NOT DECORATION AND WAS NOT ALWAYS REACHABLE. The prop and its
 * second generated gate existed for a while with no caller passing them, because
 * the case-scoped `unack` endpoint did not exist: acknowledging a case was a
 * one-way gesture over every one of its members. The dialog was one prop away from
 * working, and the missing half was on the server.
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
 * ⛔ `"case"` HERE IS THE **CORRELATION** — what the UI calls a Case and the API
 * still calls an alert group. It is NOT `AlertCase`, the per-alert firing episode
 * (`internal/alerts/domain/case.go`). The two words collide in the codebase by an
 * accepted decision; on screen they never do, because the episode is only ever
 * called an *episode*.
 */
export type AckSubject = "alert" | "case";

export interface AckDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly subject: AckSubject;
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

const TITLE: Record<AckSubject, string> = {
  alert: "Acknowledge this alert",
  case: "Acknowledge every current member",
};

/**
 * ⭐ THE CASE SENTENCE SAYS WHAT THE FAN-OUT DOES **NOT** COVER. Acknowledging a
 * case is one receipt per currently-joined member, and nothing more: an alert
 * that joins in ten minutes is unacknowledged, because a receipt is a record that
 * a human saw something and cannot be written in advance of their seeing it. An
 * operator who believes otherwise has silenced their own future signal, so the
 * limit is stated at the moment they commit rather than discovered later.
 */
const DESCRIPTION: Record<AckSubject, string> = {
  alert:
    "A receipt that a human has seen this signal. It does not change the alert: it stays firing until the upstream says otherwise.",
  case: "One receipt per alert that has already joined this case. It changes nothing about the signals — they stay firing until the upstream says otherwise — and members that join later are NOT acknowledged, because a receipt is never predictive.",
};

/**
 * ⭐ THE CASE SENTENCE SAYS THE WITHDRAWAL IS A FAN-OUT TOO. Where the case ack
 * writes one receipt per currently-joined member, the case withdrawal removes one
 * per member — it is the same verb read backwards and not a claim over the set, so
 * an alert that joins in ten minutes was never acknowledged and is not "un-acked"
 * either. Members that carry no receipt are simply skipped.
 */
const WITHDRAW_DESCRIPTION: Record<AckSubject, string> = {
  alert:
    "Recorded as a deliberate withdrawal, which is distinct from the automatic one that happens when a new episode opens.",
  case: "Removes the receipt from every alert currently in this case, recorded as a deliberate withdrawal — distinct from the automatic one that happens when a new episode opens. A member that carries no receipt is skipped rather than failing the request.",
};

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
            {props.withdrawing === true ? "Withdraw acknowledgement" : TITLE[props.subject]}
          </ModalTitle>
          <ModalDescription>
            {props.withdrawing === true
              ? WITHDRAW_DESCRIPTION[props.subject]
              : DESCRIPTION[props.subject]}
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
                <Match when={props.withdrawing === true && props.subject === "case"}>
                  There is nothing here to withdraw — this case has no currently-joined member whose
                  episode is still open. It may have resolved while this dialog was open.
                </Match>
                <Match when={props.withdrawing === true}>
                  This episode ended before the request landed, so there is no receipt left to
                  withdraw. The alert may have resolved while this dialog was open.
                </Match>
                <Match when={props.subject === "case"}>
                  There is nothing here to acknowledge — this case has no currently-joined member
                  whose episode is still open. It may have resolved while this dialog was open.
                </Match>
                <Match when={true}>
                  This episode ended before the request landed, so there is nothing to acknowledge.
                  The alert may have resolved while this dialog was open.
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
