import { describe, expect, it, vi, afterEach } from "vitest";

import { ApiError, getHealth } from "~/api/client";

function mockFetch(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("getHealth", () => {
  it("parses a healthy response", async () => {
    mockFetch(200, { status: "ok", service: "oto", version: "dev" });
    await expect(getHealth()).resolves.toEqual({
      status: "ok",
      service: "oto",
      version: "dev",
    });
  });

  it("raises an ApiError carrying the problem+json detail", async () => {
    mockFetch(503, {
      type: "https://oto.dev/errors/unavailable",
      title: "Service unavailable",
      status: 503,
      detail: "database unreachable",
    });
    await expect(getHealth()).rejects.toBeInstanceOf(ApiError);
  });
});
