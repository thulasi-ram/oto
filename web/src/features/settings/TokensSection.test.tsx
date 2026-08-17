/**
 * The credential screens, judged on what an operator walks away holding.
 *
 * ⛔ THE FAILURE THESE TESTS EXIST FOR IS A SECRET THAT NO LONGER EXISTS. Both
 * flows here hand over a value the server has already forgotten — only a sha256
 * is kept — so every ordinary rendering assertion can pass while the operator is
 * left with nothing: a dialog that shows three of the four fields it was given,
 * a copy button that silently fails, a "revoked" chip that reads as instant when
 * the credential is live for another minute. None of those look like bugs from
 * the DOM. So the assertions below are about the *content of what is handed
 * over* and the *warnings attached to it*, not about which elements rendered.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { SourcesSection } from "./SourcesSection";
import { TokensSection } from "./TokensSection";
import { apiToken, cluster, source } from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const SOURCE_ID = "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60";
const TOKEN_ID = "6b2c8e9f-7a0d-4b2c-3e4f-5061728394a5";

/* -------------------------------------------------------------------------- */
/* Personal access tokens                                                     */
/* -------------------------------------------------------------------------- */

function mountTokens(tokens: readonly ReturnType<typeof apiToken>[] = [apiToken()]): FetchStub {
  const net = stubFetch({ "GET /api/v1/api-tokens": list(tokens) });
  renderScreen(() => <TokensSection />);
  return net;
}

describe("the access-token screen", () => {
  it("mints a token and shows the secret the list can never show again", async () => {
    const net = mountTokens([]);
    net.on("POST /api/v1/api-tokens", {
      status: 201,
      json: item({
        token: apiToken({ name: "ci runner", prefix: "oto_pat_Zq7X" }),
        secret: "oto_pat_Zq7XkL4mN1pQ6rS3tU8vW5xY9aB2cD",
      }),
    });

    await until(() => expect(screen.getByRole("button", { name: "Create a token" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Create a token" }));

    await until(() => expect(document.querySelector("#token-name")).toBeTruthy());
    const input = document.querySelector("#token-name") as HTMLInputElement;
    fireEvent.input(input, { target: { value: "ci runner" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    // The secret itself, on screen. This is the whole product of the flow: the
    // list that follows carries only `prefix`, and the server kept only a hash,
    // so a dialog that omitted this would leave the operator with a credential
    // nobody — including oto — can produce again.
    await until(() =>
      expect(screen.getByText("oto_pat_Zq7XkL4mN1pQ6rS3tU8vW5xY9aB2cD")).toBeTruthy(),
    );

    // And the mint carried a fresh idempotency key: the contract answers a
    // reused one with a 409 rather than replaying, because the original answer
    // was a secret oto no longer holds.
    const [mint] = net.calls.filter((c) => c.method === "POST" && c.path.endsWith("/api-tokens"));
    expect(mint?.headers["Idempotency-Key"] ?? mint?.headers["idempotency-key"]).toBeTruthy();
  });

  it("never renders a secret in the list, because the list is never given one", async () => {
    mountTokens([apiToken({ name: "laptop CLI", prefix: "oto_pat_AbCd" })]);

    await until(() => expect(screen.getByText("laptop CLI")).toBeTruthy());
    expect(screen.getByText("oto_pat_AbCd")).toBeTruthy();
    // Not a masked stand-in either. oto stores a hash, so any dotted or starred
    // rendering would be a string that exists nowhere — a fiction that reads as
    // "the secret is here somewhere".
    expect(document.body.textContent).not.toMatch(/[•*]{4}/);
  });

  it("says revocation is not instant before the operator commits to it", async () => {
    mountTokens();

    await until(() => expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    await until(() => expect(document.querySelector('[role="dialog"]')).toBeTruthy());
    const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
    // ⛔ THE ONE SENTENCE THIS DIALOG EXISTS FOR. oto caches credentials, so a
    // revoked token keeps authenticating for up to a minute. The operator most
    // likely to be here is responding to a leak, and "revoked" without the delay
    // would let them conclude the incident closed sixty seconds early.
    expect(dialog.textContent).toMatch(/one minute/i);
    expect(dialog.textContent).toMatch(/not\s+instantly/i);
  });

  it("revokes through the contract's own path and refreshes the list", async () => {
    const net = mountTokens();
    net.on(`DELETE /api/v1/api-tokens/${TOKEN_ID}`, { status: 204 });

    await until(() => expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await until(() => expect(screen.getByRole("button", { name: "Revoke it" })).toBeTruthy());

    net.on("GET /api/v1/api-tokens", list([apiToken({ revoked_at: "2026-08-09T10:00:00.000Z" })]));
    fireEvent.click(screen.getByRole("button", { name: "Revoke it" }));

    await until(() => expect(screen.getByText("revoked")).toBeTruthy());
    expect(net.calls.filter((c) => c.method === "DELETE")).toHaveLength(1);
    // The revoked row loses its button rather than offering an act that is now a
    // no-op dressed as a choice.
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* Ingest tokens: rotation, and the pair the register flow hands over          */
/* -------------------------------------------------------------------------- */

function mountSources(): FetchStub {
  const net = stubFetch({
    "GET /api/v1/sources": list([source()]),
    "GET /api/v1/clusters": list([cluster()]),
  });
  renderScreen(() => <SourcesSection />);
  return net;
}

describe("rotating a source's ingest token", () => {
  it("states the cost — a permanent 401 and lost alerts — before it rotates anything", async () => {
    mountSources();

    await until(() => expect(screen.getByRole("button", { name: "Rotate token" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Rotate token" }));

    await until(() => expect(document.querySelector('[role="dialog"]')).toBeTruthy());
    const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
    // The handler's own three facts, unsoftened: the old token dies now, the 401
    // is permanent to Alertmanager, and what it sends meanwhile is gone rather
    // than queued. An operator rotating because a config file leaked needs all
    // three before confirming, not in a runbook afterwards.
    expect(dialog.textContent).toMatch(/401/);
    expect(dialog.textContent).toMatch(/permanent/i);
    expect(dialog.textContent).toMatch(/lost, not delayed/i);
  });

  it("shows the new token once, and says the source is failing until it is deployed", async () => {
    const net = mountSources();
    const webhook = `http://localhost:8080/api/v1/ingest/alertmanager/${SOURCE_ID}`;
    net.on(`POST /api/v1/sources/${SOURCE_ID}/rotate-token`, {
      json: item({
        source: source(),
        ingest_token: "oto_ingest_R0tAtEd9kZ2mQ7pR4tX1vB6nL0sD3f",
        token_prefix: "oto_ingest_R0tA",
        webhook_url: webhook,
      }),
    });

    await until(() => expect(screen.getByRole("button", { name: "Rotate token" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Rotate token" }));
    await until(() => expect(screen.getByRole("button", { name: "Rotate the token" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Rotate the token" }));

    await until(() =>
      expect(screen.getByText("oto_ingest_R0tAtEd9kZ2mQ7pR4tX1vB6nL0sD3f")).toBeTruthy(),
    );

    // ⛔ THE ROTATED DIALOG IS NOT THE CREATED DIALOG. After a registration
    // nothing is broken yet; after a rotation the upstream is ALREADY failing,
    // and the copy has to carry that difference or the operator reads a routine
    // "here is your token" while alerts are being dropped.
    const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
    expect(dialog.textContent).toMatch(/already rejected/i);
    expect(dialog.textContent).toMatch(/being dropped/i);

    // ⛔ AND THE URL, WHICH THIS DIALOG USED TO WITHHOLD. It told the operator to
    // point their receiver at "oto's ingest URL for this source" while holding
    // that exact URL in the response it was rendering: the return type said
    // `SourceDTO`, the call site cast through `unknown`, and so nothing could
    // notice that two of the four fields went unread. It is the one version of
    // that URL nobody has to assemble, because the server builds it from its own
    // `OTO_HTTP_BASE_URL` rather than from documentation.
    //
    // Asserted here rather than on the register flow because `TokenDialog` is
    // ONE component — the two paths differ only in which sentences head it — and
    // reaching it this way costs a button instead of driving a five-field form
    // and a combobox to prove something about a shared child.
    expect(screen.getByText(webhook)).toBeTruthy();
  });
});
