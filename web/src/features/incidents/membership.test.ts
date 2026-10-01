/**
 * `drawOver` — the draw, then one move per held Case.
 *
 * ⭐ EVERY HELD CASE IS ATTEMPTED. Each move is its own transaction on the
 * server, so a refusal of one says nothing about the next; a loop that stopped
 * at the first failure would leave later picked Cases unmoved AND unmentioned.
 * The tests below hold it to attempting all of them and naming every one that
 * stayed behind.
 */
import { describe, expect, it } from "vitest";

import { PartialDraw, drawOver, type PickedCase } from "./membership";
import { incident, incidentDetail } from "~/test/fixtures";
import { item, problem, stubFetch } from "~/test/harness";

const FREE: PickedCase = { id: "case-1", number: 412, in: null };
const HELD_A: PickedCase = { id: "case-2", number: 413, in: incident({ number: 3 }) };
const HELD_B: PickedCase = { id: "case-3", number: 414, in: incident({ number: 5 }) };

function stubDraw() {
  return stubFetch({
    "POST /api/v1/incidents": { status: 201, json: item(incidentDetail({ number: 9 })) },
  });
}

describe("drawOver", () => {
  it("⭐ attempts every held move after one fails, and names only the Case that stayed", async () => {
    const net = stubDraw();
    net.on("POST /api/v1/incidents/3/cases/case-2/move", () =>
      problem(404, "incident_member_not_found", { detail: "Case #413 is not in Incident #3." }),
    );
    net.on("POST /api/v1/incidents/5/cases/case-3/move", () => ({
      json: item(incidentDetail({ number: 9 })),
    }));

    const err = await drawOver([FREE, HELD_A, HELD_B]).catch((e: unknown) => e);

    // The second move was still sent, though the first was refused.
    expect(net.to("/move").map((c) => c.path)).toEqual([
      "/api/v1/incidents/3/cases/case-2/move",
      "/api/v1/incidents/5/cases/case-3/move",
    ]);
    expect(err).toBeInstanceOf(PartialDraw);
    const partial = err as PartialDraw;
    expect(partial.drawn.number).toBe(9);
    expect(partial.stranded.map((s) => s.c.id)).toEqual(["case-2"]);
    expect(partial.message).toBe(
      "Incident #9 was drawn, but Case #413 is still in Incident #3: Case #413 is not in Incident #3.",
    );
  });

  it("names EVERY stranded Case when more than one move fails", async () => {
    const net = stubDraw();
    net.on("POST /api/v1/incidents/3/cases/case-2/move", () =>
      problem(404, "incident_member_not_found", { detail: "Case #413 is not in Incident #3." }),
    );
    net.on("POST /api/v1/incidents/5/cases/case-3/move", () =>
      problem(404, "incident_member_not_found", { detail: "Case #414 is not in Incident #5." }),
    );

    const err = await drawOver([FREE, HELD_A, HELD_B]).catch((e: unknown) => e);

    expect(net.to("/move")).toHaveLength(2);
    expect(err).toBeInstanceOf(PartialDraw);
    const partial = err as PartialDraw;
    expect(partial.stranded.map((s) => s.c.number)).toEqual([413, 414]);
    expect(partial.message).toMatch(/^Incident #9 was drawn, but /);
    expect(partial.message).toMatch(/Case #413 is still in Incident #3: Case #413 is not in Incident #3\./);
    expect(partial.message).toMatch(/Case #414 is still in Incident #5: Case #414 is not in Incident #5\./);
  });

  it("resolves with the drawn Incident when every move lands", async () => {
    const net = stubDraw();
    net.on("POST /api/v1/incidents/3/cases/case-2/move", () => ({
      json: item(incidentDetail({ number: 9 })),
    }));

    const drawn = await drawOver([FREE, HELD_A]);

    expect(drawn.number).toBe(9);
    expect(net.to("/move")).toHaveLength(1);
  });
});
