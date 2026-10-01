/**
 * Which Incident a Case is in, and drawing one over Cases some of which already
 * are — the two halves of git-bug f89c9cc, shared by `/cases` and `/cases/:id`.
 *
 * ⛔ A CASE IS IN AT MOST ONE INCIDENT (ADR 0052 §4), and the server holds that
 * line with a partial unique index: a draw or an add naming a Case that is
 * already in one is refused whole with `409 case_in_incident`. So a screen that
 * lets a human pick Cases has to know, BEFORE the press, which of them are
 * already in a story — and for those the honest verb is MOVE, never a second
 * membership. Everything here is that one rule, said ahead of time.
 */
import { createIncident, getCaseIncident, moveIncidentCase } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Incident, IncidentDetail } from "~/api/types";
import { idempotencyKey } from "~/lib/format";

/**
 * The query for the Incident one Case is in now — `null` when it is in none.
 *
 * Keyed under `["incidents"]`, so a membership write anywhere, and every stream
 * frame that already refreshes the Incident pages, refreshes this as well.
 */
export function caseIncidentQuery(caseId: string) {
  return {
    queryKey: qk.incidents.holding(caseId),
    queryFn: ({ signal }: { signal: AbortSignal }): Promise<Incident | null> =>
      getCaseIncident(caseId, { signal }),
  };
}

/** A typed Incident number, or `null` when the text cannot name one. */
export function parseIncidentNumber(raw: string): number | null {
  const s = raw.trim().replace(/^#/, "");
  if (!/^[1-9][0-9]{0,17}$/.test(s)) return null;
  const n = Number(s);
  return Number.isSafeInteger(n) ? n : null;
}

/** One picked Case, as much of it as a sentence about it needs. */
export interface PickedCase {
  readonly id: string;
  readonly number: number;
  /** The Incident it is in now, or `null`. */
  readonly in: Incident | null;
}

/**
 * The draw succeeded and at least one move after it did not.
 *
 * ⚠️ THE TWO HALVES ARE TWO REQUESTS, so this state exists and has to be said
 * plainly: the Incident IS drawn — over every Case that was in none — and the
 * Case named here is still where it was. Nothing is rolled back, because the
 * draw is a fact on several Cases' timelines already; the operator is told which
 * Case did not come along and where the new Incident is.
 */
export class PartialDraw extends Error {
  constructor(
    readonly drawn: IncidentDetail,
    readonly stranded: PickedCase,
    readonly reason: unknown,
  ) {
    super(
      `Incident #${drawn.number} was drawn, but Case #${stranded.number} is still in Incident #${
        stranded.in?.number ?? "?"
      }: ${reason instanceof Error ? reason.message : String(reason)}`,
    );
    this.name = "PartialDraw";
  }
}

/**
 * Draw one Incident over the picked Cases, MOVING the ones already in another.
 *
 * ⭐ THE CASES IN NONE ARE DRAWN; THE CASES IN ONE ARE MOVED IN AFTER. The server
 * refuses a draw naming a held Case, and that refusal is right — a second
 * membership is the thing ADR 0052 forbids — so the held ones are taken out of
 * their Incident and put into the new one with `…/move`, which is ONE
 * transaction per Case and leaves ONE `incident.case_moved` fact on its timeline:
 * the decision the human made, not a removal and an add.
 *
 * ⛔ AT LEAST ONE PICKED CASE MUST BE IN NO INCIDENT. A draw names the Cases it
 * starts with and an Incident cannot be drawn empty, so a selection whose every
 * Case is already in a story has nothing to draw over; the screen says so and
 * offers no press rather than inventing a removal to make room.
 */
export async function drawOver(picked: readonly PickedCase[]): Promise<IncidentDetail> {
  const free = picked.filter((c) => c.in === null);
  const held = picked.filter((c) => c.in !== null);
  if (free.length === 0) {
    throw new Error(
      "Every selected Case is already in an Incident, and an Incident is drawn over at least one Case that is in none.",
    );
  }
  const drawn = await createIncident(
    free.map((c) => c.id),
    idempotencyKey(),
  );
  for (const c of held) {
    try {
      await moveIncidentCase((c.in as Incident).number, c.id, drawn.number, idempotencyKey());
    } catch (err) {
      throw new PartialDraw(drawn, c, err);
    }
  }
  return drawn;
}
