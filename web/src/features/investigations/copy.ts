/**
 * What an Investigation's states, reasons and Step outcomes say, in words.
 *
 * ⛔ EVERY MAP IS TYPED AGAINST THE CONTRACT'S OWN ENUM, never
 * `Record<string, string>` — the lesson `notifications/vocabulary.ts` paid for:
 * a loose map lost a key, the lookup fell through to the wire token, and the one
 * screen whose job was explaining a silence rendered `snoozed` where a sentence
 * belongs. A status, reason or outcome the server adds is a build failure here.
 *
 * ⭐ THE SENTENCES SAY WHAT WAS RECORDED, NOT WHAT TO FEEL. ADR 0053 §6 makes
 * every control "recorded, never silent", so each reason below names the control
 * that ended the run and what was kept — a partial Finding, or nothing. None of
 * them says or implies that anyone was or was not told: a Finding never decides
 * that (ADR 0053 §2), and copy that hinted otherwise would teach the opposite.
 */
import type { InvestigationReason, InvestigationStatus, StepOutcome } from "~/api/types";

/** The status as a short word, beside the Investigator's name. */
export const STATUS_LABEL: Record<InvestigationStatus, string> = {
  queued: "waiting to start",
  running: "running",
  completed: "completed",
  exhausted: "stopped at a budget",
  failed: "failed",
  skipped: "skipped",
};

/** Whether a run can still change — the panel polls exactly while this is true. */
export const IN_PROGRESS: Record<InvestigationStatus, boolean> = {
  queued: true,
  running: true,
  completed: false,
  exhausted: false,
  failed: false,
  skipped: false,
};

/**
 * Why a run ended any way but `completed`, as a sentence.
 *
 * The contract also carries `reason_detail`, the server's own sentence about
 * THIS run; it is shown beneath this one, verbatim, because it may name the
 * endpoint's error and nothing here can.
 */
export const REASON_SENTENCE: Record<InvestigationReason, string> = {
  step_budget:
    "It made as many Tool calls as its step budget allows, and stopped before concluding. What it had reached is kept, marked partial.",
  token_budget:
    "It spent its whole token budget, and stopped before concluding. What it had reached is kept, marked partial.",
  wall_time_budget:
    "It ran for as long as its wall-time budget allows, and stopped before concluding. What it had reached is kept, marked partial.",
  usage_missing:
    "The model reported no token usage, so the run could not be budgeted and was stopped rather than run unmetered.",
  model_error: "The model endpoint answered with an error.",
  model_changed:
    "The endpoint no longer reports the model this Investigator version pinned, so the run was stopped rather than answered by a different model.",
  subject_gone: "The Case or Incident it was asked about could no longer be read.",
  interrupted:
    "Its worker stopped part-way through. It is not run again on its own, which would spend its tokens twice — ask again if it is still worth asking.",
  internal: "oto failed while running it. This is a fault on oto's side.",
  disabled:
    "Investigations are switched off — for this organisation, or for this Investigator — so nothing ran. The request is still recorded.",
  budget:
    "This organisation had already spent its daily Investigation token budget, so nothing ran. The request is still recorded; the budget resets at midnight UTC.",
};

/** What came of one Tool call: the word, and what it meant for the run. */
export const OUTCOME: Record<StepOutcome, { readonly label: string; readonly note: string }> = {
  ok: { label: "answered", note: "The Tool answered and the model was given its result." },
  refused: {
    label: "refused",
    note: "Not called: the Tool is outside this Investigator's allowlist, does not exist, or the step budget was spent. The model was told so.",
  },
  timeout: {
    label: "timed out",
    note: "The call ran past its per-call timeout. The model was told so, and the run continued.",
  },
  truncated: {
    label: "truncated",
    note: "The result was cut to the per-call size cap before the model saw it. The run continued.",
  },
  failed: {
    label: "failed",
    note: "The call failed. The model was told so, and the run continued.",
  },
};

/**
 * A model turn's `finish_reason` that is worth a word of its own. The field is
 * the endpoint's free text, so only the one that changes how the turn reads is
 * named; anything else is shown as the endpoint wrote it.
 */
export function finishNote(reason: string | null): string | null {
  if (reason === "length") return "cut off at the model's output limit";
  return null;
}
