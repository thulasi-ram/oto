/**
 * The one door every request goes through.
 *
 * Two properties are load-bearing and both are tested here as refusals rather
 * than as happy paths:
 *
 *   1. **A body the contract does not describe is an error, not data.** The
 *      envelope checks exist so that a server which starts answering a bare
 *      array — or HTML from a misconfigured proxy — fails at the boundary with
 *      the path in the message, instead of handing `undefined` to a screen that
 *      renders it.
 *   2. **A network failure is not an HTTP status.** Status 0 is the honest
 *      encoding, and the banner that reads "Cannot reach oto." depends on it.
 *
 * `violationsByField` / `orphanViolations` are the other half: a server
 * violation that lands on no control must reach the form-level slot rather than
 * being swallowed, because a 422 nobody can see is a form that refuses to save
 * and will not say why.
 */
import { describe, expect, it, vi } from "vitest";

import {
  ApiContractError,
  ApiError,
  buildQueryString,
  del,
  emptyPage,
  getItem,
  getItemEnvelope,
  getList,
  getUnpagedList,
  orphanViolations,
  patchItem,
  postItem,
  violationsByField,
} from "./client";
import { item, list, stubFetch, unpaged, validationFailed } from "~/test/harness";

describe("buildQueryString", () => {
  it("omits empty values so a default filter never reaches the wire", () => {
    expect(
      buildQueryString({
        query: { limit: 50, cursor: null, q: "", flapping: false, state: [] },
      }),
    ).toBe("?limit=50&flapping=false");
  });

  it("comma-joins arrays, which is `style: form, explode: false`", () => {
    expect(buildQueryString({ query: { state: ["firing", "suppressed"] } })).toBe(
      "?state=firing%2Csuppressed",
    );
  });

  it("renders the label selector as deepObject, negation marker and all", () => {
    // OpenAPI cannot express the `!` marker (§E.3), so it travels in the key.
    const qs = buildQueryString({ label: { team: "core", "!tier": "canary" } });
    const p = new URLSearchParams(qs.slice(1));
    expect(p.get("label[team]")).toBe("core");
    expect(p.get("label[!tier]")).toBe("canary");
  });
});

describe("ApiError", () => {
  it("carries the problem's own code, and falls back to one derived from the status", () => {
    const withBody = new ApiError(422, { code: "validation_failed", title: "t", status: 422 } as never, "x");
    expect(withBody.code).toBe("validation_failed");
    expect(new ApiError(412, null, "412 Precondition Failed").code).toBe("precondition_failed");
    expect(new ApiError(500, null, "500 Internal Server Error").code).toBe("internal_error");
  });

  it("says which failures are worth retrying and which are not", () => {
    expect(new ApiError(429, null, "").retryable).toBe(true);
    expect(new ApiError(503, null, "").retryable).toBe(true);
    // A 422 will be refused identically forever; retrying it is a lie about progress.
    expect(new ApiError(422, null, "").retryable).toBe(false);
    expect(new ApiError(412, null, "").retryable).toBe(false);
  });

  it("never doubles the status into the headline, and names an unreachable server", () => {
    expect(new ApiError(500, null, "500 Internal Server Error").headline).toBe(
      "500 Internal Server Error",
    );
    expect(new ApiError(0, null, "fetch failed").headline).toBe("Cannot reach oto.");
  });
});

describe("violations", () => {
  const err = new ApiError(
    422,
    {
      title: "Validation failed",
      status: 422,
      code: "validation_failed",
      violations: [
        { field: "matchers/0/name", code: "pattern", message: "not a label name" },
        { field: "matchers/0/name", code: "pattern", message: "a second opinion" },
        { field: "reasons", code: "enum", message: "unknown reason" },
      ],
    } as never,
    "x",
  );

  it("maps JSON-pointer paths onto control names, first violation winning", () => {
    const byField = violationsByField(err);
    expect(byField.get("matchers.0.name")).toBe("not a label name");
    expect(byField.get("reasons")).toBe("unknown reason");
  });

  it("surfaces violations that landed on no control instead of swallowing them", () => {
    // `reasons` is a known control here; `matchers/0/name` is not, so it must
    // reach the form-level slot rather than disappearing.
    const orphans = orphanViolations(err, ["reasons"]);
    expect(orphans).toEqual([
      "matchers/0/name: not a label name",
      "matchers/0/name: a second opinion",
    ]);
  });

  it("is empty for anything that is not an ApiError", () => {
    expect(violationsByField(new Error("boom")).size).toBe(0);
    expect(orphanViolations(null, ["a"])).toEqual([]);
  });
});

describe("the request primitive", () => {
  it("unwraps the item envelope and sends the idempotency key exactly once", async () => {
    const fetchStub = stubFetch({
      "POST /api/v1/alerts/a1/ack": item({ id: "ac-1", state: "firing" }),
    });

    const ac = await postItem<{ id: string }>("/api/v1/alerts/a1/ack", { note: "seen" }, {
      idempotencyKey: "key-1",
    });

    expect(ac.id).toBe("ac-1");
    const call = fetchStub.calls[0];
    expect(call?.method).toBe("POST");
    expect(call?.headers["Idempotency-Key"]).toBe("key-1");
    expect(call?.headers["Content-Type"]).toBe("application/json");
    expect(call?.body).toEqual({ note: "seen" });
  });

  it("keeps `meta` when the caller asked for the envelope", async () => {
    stubFetch({ "GET /api/v1/version": item({ version: "1.2.3" }) });
    const env = await getItemEnvelope<{ version: string }>("/api/v1/version");
    expect(env.data.version).toBe("1.2.3");
    expect(env.meta.request_id).toBe("01JD8Z2K7M3TQ9");
  });

  it("refuses a list response with no keyset `page`, because a cursor cannot be invented", async () => {
    stubFetch({ "GET /api/v1/alerts": { json: { data: [], meta: { request_id: "r" } } } });
    await expect(getList("/api/v1/alerts")).rejects.toBeInstanceOf(ApiContractError);
  });

  it("refuses an item response with no `data` key", async () => {
    stubFetch({ "GET /api/v1/me": { json: { id: "u1" } } });
    await expect(getItem("/api/v1/me")).rejects.toBeInstanceOf(ApiContractError);
  });

  it("refuses a body that is not JSON at all — the misconfigured-proxy case", async () => {
    stubFetch({
      "GET /api/v1/alerts": { text: "<!doctype html><title>nginx</title>", contentType: "text/html" },
    });
    await expect(getList("/api/v1/alerts")).rejects.toThrow(/not JSON/);
  });

  it("accepts the unpaged collections the contract serves without a cursor", async () => {
    stubFetch({ "GET /api/v1/channel-types": unpaged([{ type: "slack" }, { type: "webhook" }]) });
    const types = await getUnpagedList<{ type: string }>("/api/v1/channel-types");
    expect(types.map((t) => t.type)).toEqual(["slack", "webhook"]);
  });

  it("turns a problem+json body into an ApiError with its violations intact", async () => {
    stubFetch({
      "PATCH /api/v1/org/settings": validationFailed({
        field: "refire_grace_s",
        code: "min",
        message: "below the range oto accepts",
      }),
    });

    const err = await patchItem("/api/v1/org/settings", { refire_grace_s: 1 }).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).code).toBe("validation_failed");
    expect(violationsByField(err).get("refire_grace_s")).toBe("below the range oto accepts");
  });

  it("keeps a bodyless error usable — the 412 with nothing in it", async () => {
    stubFetch({ "POST /api/v1/alerts/a1/unsnooze": { status: 412, text: "" } });
    const err = await postItem("/api/v1/alerts/a1/unsnooze", {}).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(412);
    expect((err as ApiError).code).toBe("precondition_failed");
    expect((err as ApiError).violations).toEqual([]);
  });

  it("reports a network failure as status 0, never as a fabricated 5xx", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))),
    );
    const err = await getList("/api/v1/alerts").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(0);
    expect((err as ApiError).headline).toBe("Cannot reach oto.");
  });

  it("lets an abort propagate as an abort, so a cancelled query is not an error banner", async () => {
    const abort = new DOMException("aborted", "AbortError");
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(abort)),
    );
    await expect(getList("/api/v1/alerts")).rejects.toBe(abort);
  });

  it("treats a 204 as success with nothing to read", async () => {
    stubFetch({ "DELETE /api/v1/channels/c1": { status: 204 } });
    await expect(del("/api/v1/channels/c1")).resolves.toBeUndefined();
  });

  it("hands the placeholder page a shape callers can render without a null check", () => {
    const page = emptyPage<{ id: string }>();
    expect(page.data).toEqual([]);
    expect(page.page.has_more).toBe(false);
    expect(page.page.next_cursor).toBeNull();
  });

  it("passes the query and label bags through to the URL", async () => {
    const fetchStub = stubFetch({ "GET /api/v1/alerts": list([]) });
    await getList("/api/v1/alerts", {
      query: { state: ["firing"], limit: 25 },
      label: { team: "core" },
    });
    const call = fetchStub.calls[0];
    expect(call?.search.get("state")).toBe("firing");
    expect(call?.search.get("limit")).toBe("25");
    expect(call?.search.get("label[team]")).toBe("core");
  });
});
