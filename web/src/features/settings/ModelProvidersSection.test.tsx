/**
 * The model-provider screen, judged on what happens to the key.
 *
 * ⛔ THE FAILURE THESE TESTS EXIST FOR IS A KEY THAT LEAKS OR GOES MISSING: sent empty instead of
 * omitted, left in the form after a save, or rendered back from a list that never carried it.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { ModelProvidersSection } from "./ModelProvidersSection";
import { modelProvider } from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

function mount(providers: readonly ReturnType<typeof modelProvider>[] = []): FetchStub {
  const net = stubFetch({ "GET /api/v1/model-providers": list(providers) });
  renderScreen(() => <ModelProvidersSection />);
  return net;
}

function type(id: string, value: string): void {
  fireEvent.input(document.querySelector(`#${id}`) as HTMLInputElement, { target: { value } });
}

async function openDialog(): Promise<void> {
  await until(() => expect(screen.getByRole("button", { name: "Add a provider" })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Add a provider" }));
  await until(() => expect(document.querySelector("#provider-name")).toBeTruthy());
}

describe("the model-provider screen", () => {
  it("lists a provider with whether a key is set, and never a key", async () => {
    mount([modelProvider(), modelProvider({ id: "x", name: "local", has_key: false })]);
    await until(() => expect(screen.getByText("gateway")).toBeTruthy());
    expect(screen.getByText("key set")).toBeTruthy();
    expect(screen.getByText("no key")).toBeTruthy();
  });

  it("sends the key with the new provider, then clears it", async () => {
    const net = mount();
    net.on("POST /api/v1/model-providers", { status: 201, json: item(modelProvider()) });
    await openDialog();
    type("provider-name", "gateway");
    type("provider-base-url", "https://llm.internal.example/v1");
    type("provider-model", "gpt-4o");
    type("provider-api-key", "sk-secret");
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }));

    await until(() => expect(net.to("/model-providers").filter((c) => c.method === "POST")).toHaveLength(1));
    const post = net.to("/model-providers").find((c) => c.method === "POST");
    expect(post?.body).toEqual({
      name: "gateway",
      base_url: "https://llm.internal.example/v1",
      model: "gpt-4o",
      api_key: "sk-secret",
    });

    await openDialog();
    expect((document.querySelector("#provider-api-key") as HTMLInputElement).value).toBe("");
  });

  it("omits api_key entirely when none was typed", async () => {
    const net = mount();
    net.on("POST /api/v1/model-providers", { status: 201, json: item(modelProvider({ has_key: false })) });
    await openDialog();
    type("provider-name", "local");
    type("provider-base-url", "http://litellm.svc:4000/v1");
    type("provider-model", "claude");
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }));

    await until(() => expect(net.to("/model-providers").some((c) => c.method === "POST")).toBe(true));
    const post = net.to("/model-providers").find((c) => c.method === "POST");
    expect(post?.body).not.toHaveProperty("api_key");
  });

  it("replaces a key through its own route and never sends the URL or model", async () => {
    const net = mount([modelProvider()]);
    net.on("PUT /api/v1/model-providers/3f1a9c2e-5b7d-4e8f-a0b1-c2d3e4f50617/key", {
      status: 200,
      json: item(modelProvider()),
    });
    await until(() => expect(screen.getByRole("button", { name: "Replace key" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Replace key" }));
    await until(() => expect(document.querySelector("[id^=provider-key-]")).toBeTruthy());
    fireEvent.input(document.querySelector("[id^=provider-key-]") as HTMLInputElement, {
      target: { value: "sk-rotated" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await until(() => expect(net.to("/key")).toHaveLength(1));
    expect(net.to("/key")[0]?.body).toEqual({ api_key: "sk-rotated" });
  });

  it("offers a first key to an endpoint that has none", async () => {
    mount([modelProvider({ has_key: false })]);
    await until(() => expect(screen.getByRole("button", { name: "Set key" })).toBeTruthy());
  });

  it("deletes after confirmation", async () => {
    const net = mount([modelProvider()]);
    net.on("DELETE /api/v1/model-providers/3f1a9c2e-5b7d-4e8f-a0b1-c2d3e4f50617", { status: 204 });
    await until(() => expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await until(() => expect(screen.getByRole("button", { name: "Delete it" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Delete it" }));
    await until(() => expect(net.calls.some((c) => c.method === "DELETE")).toBe(true));
  });

  it("says why a delete was refused, in the server's words", async () => {
    const net = mount([modelProvider()]);
    net.on("DELETE /api/v1/model-providers/3f1a9c2e-5b7d-4e8f-a0b1-c2d3e4f50617", {
      status: 409,
      json: {
        type: "about:blank",
        title: "Conflict",
        status: 409,
        code: "model_provider_in_use",
        detail: "an Investigator version dials this endpoint, so it cannot be deleted",
      },
    });
    await until(() => expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await until(() => expect(screen.getByRole("button", { name: "Delete it" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Delete it" }));
    await until(() =>
      expect(screen.getByText(/an Investigator version dials this endpoint/)).toBeTruthy(),
    );
  });
});
