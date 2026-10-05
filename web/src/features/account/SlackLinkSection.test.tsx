/**
 * Linking a Slack account to yourself (git-bug a556a5c), judged on the one thing
 * that matters: nothing is linked until the person has SEEN which Slack account
 * the code names and confirmed it, and the request never names anybody but them.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { SlackLinkSection } from "./SlackLinkSection";
import { item, list, problem, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const IDENTITY_ID = "019fe2a1-5d1e-7c00-8000-00000000a556";

const preview = {
  team_id: "T9TK3CUKW",
  slack_user_id: "U0123456789",
  handle: "ram",
  expires_at: "2026-10-05T12:10:00Z",
  already_yours: false,
};

const linked = {
  id: IDENTITY_ID,
  team_id: "T9TK3CUKW",
  slack_user_id: "U0123456789",
  handle: "ram",
  linked_at: "2026-10-05T12:01:00Z",
};

function mount(identities: readonly (typeof linked)[] = []): FetchStub {
  const net = stubFetch({ "GET /api/v1/me/slack-identities": list(identities) });
  renderScreen(() => <SlackLinkSection />);
  return net;
}

function enter(code: string): void {
  const input = document.querySelector("#slack-link-code") as HTMLInputElement;
  fireEvent.input(input, { target: { value: code } });
  fireEvent.click(screen.getByRole("button", { name: "Check code" }));
}

describe("the Slack link screen", () => {
  it("says a code is a credential before one is entered", async () => {
    mount();
    await until(() => expect(document.querySelector("#slack-link-code")).toBeTruthy());
    expect(document.body.textContent).toMatch(/credential/);
    expect(document.body.textContent).toMatch(/count as them/);
  });

  it("names the Slack account and links only after the person confirms it", async () => {
    const net = mount();
    net.on("POST /api/v1/me/slack-identities/preview", item(preview));
    net.on("POST /api/v1/me/slack-identities", { status: 201, json: item(linked) });

    await until(() => expect(document.querySelector("#slack-link-code")).toBeTruthy());
    enter("abcde-fghjk");

    await until(() => expect(document.querySelector("[data-confirm-copy]")).toBeTruthy());
    const copy = document.querySelector("[data-confirm-copy]")?.textContent ?? "";
    expect(copy).toContain("@ram (U0123456789)");
    expect(copy).toContain("T9TK3CUKW");
    expect(copy).toContain("Clicks from this Slack account will count as you.");
    // ⛔ The preview did not link.
    expect(net.to("/me/slack-identities").filter((c) => c.method === "POST")).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Link to me" }));
    await until(() => expect(screen.getByRole("status").textContent).toContain("now count as you"));

    const [confirm] = net.to("/me/slack-identities").filter((c) => c.method === "POST");
    // ⛔ The body is the code and nothing else — there is no user to name.
    expect(confirm?.body).toEqual({ code: "abcde-fghjk" });
    const [checked] = net.to("/preview");
    expect(checked?.body).toEqual({ code: "abcde-fghjk" });
  });

  it("shows the server's refusal and links nothing", async () => {
    const net = mount();
    net.on(
      "POST /api/v1/me/slack-identities/preview",
      problem(409, "slack_identity_linked_elsewhere", {
        detail: "this Slack account is already linked to another oto user",
      }),
    );
    await until(() => expect(document.querySelector("#slack-link-code")).toBeTruthy());
    enter("ABCDE-FGHJK");
    await until(() =>
      expect(screen.getByRole("alert").textContent).toContain("already linked to another oto user"),
    );
    expect(screen.queryByRole("button", { name: "Link to me" })).toBeNull();
  });

  it("cancelling a preview links nothing", async () => {
    const net = mount();
    net.on("POST /api/v1/me/slack-identities/preview", item(preview));
    await until(() => expect(document.querySelector("#slack-link-code")).toBeTruthy());
    enter("ABCDE-FGHJK");
    await until(() => expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await until(() => expect(document.querySelector("#slack-link-code")).toBeTruthy());
    expect(net.to("/me/slack-identities").filter((c) => c.method === "POST")).toHaveLength(0);
  });

  it("unlinks one of my Slack accounts after saying what that means", async () => {
    const net = mount([linked]);
    net.on(`DELETE /api/v1/me/slack-identities/${IDENTITY_ID}`, { status: 204 });
    await until(() => expect(screen.getByText("@ram (U0123456789)")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Unlink" }));
    await until(() => expect(screen.getByText("Its Slack clicks will count as nobody.")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Unlink" }));
    await until(() =>
      expect(net.calls.some((c) => c.method === "DELETE" && c.path.endsWith(IDENTITY_ID))).toBe(true),
    );
  });
});
