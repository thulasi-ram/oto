/**
 * The Investigator screen, judged on what it sends and what it admits.
 *
 * ⛔ THE FAILURES THESE TESTS EXIST FOR: a write ToolServer's Tool offered to a model, a version
 * written without the operator being told, and an edit that sends the whole form back and so
 * versions an Investigator when only its switch moved.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { InvestigatorsSection } from "./InvestigatorsSection";
import {
  investigator,
  investigatorDetail,
  modelProvider,
  toolServer,
  toolServerTool,
} from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const INV_ID = "7e6d5c4b-3a29-4180-9716-5c4b3a291807";
const READ = "5c4b3a29-1807-4f6e-8d5c-4b3a29180716";
const WRITE = "6d5c4b3a-2918-4071-a65d-5c4b3a291808";

function mount(invs: readonly ReturnType<typeof investigator>[] = [investigator()]): FetchStub {
  const net = stubFetch({
    "GET /api/v1/investigators": list(invs),
    "GET /api/v1/model-providers": list([modelProvider({ id: "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a" })]),
    "GET /api/v1/tool-servers": list([
      toolServer({ id: READ, name: "k8s", access: "read" }),
      toolServer({ id: WRITE, name: "kubectl", access: "write" }),
    ]),
    [`GET /api/v1/tool-servers/${READ}/tools`]: list([
      toolServerTool(),
      toolServerTool({ name: "bad", qualified_name: "k8s__bad", usable: false, unusable_reason: "not read-only" }),
    ]),
    [`GET /api/v1/tool-servers/${WRITE}/tools`]: list([
      toolServerTool({ name: "apply", qualified_name: "kubectl__apply" }),
    ]),
  });
  renderScreen(() => <InvestigatorsSection />);
  return net;
}

async function openEdit(): Promise<void> {
  await until(() => expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  await until(() => expect(document.querySelector("#investigator-prompt")).toBeTruthy());
}

describe("the Investigator screen", () => {
  it("lists an Investigator with its version, model and switch", async () => {
    mount([investigator(), investigator({ id: INV_ID, name: "second", enabled: false, investigates_incidents: true })]);
    await until(() => expect(screen.getByText("firstlook")).toBeTruthy());
    expect(screen.getAllByText("v3").length).toBe(2);
    expect(screen.getByText("disabled")).toBeTruthy();
    expect(screen.getByText("auto on Incidents")).toBeTruthy();
  });

  it("offers a read ToolServer's usable Tools and never a write one's", async () => {
    mount();
    await openEdit();
    await until(() => expect(screen.getByText("k8s__pods_list")).toBeTruthy());
    // Not usable on a read server: not offered. A write server: not offered at all.
    expect(screen.queryByText("k8s__bad")).toBeNull();
    expect(screen.queryByText("kubectl__apply")).toBeNull();
    expect(screen.getByText(/never offered/)).toBeTruthy();
  });

  it("sends only what changed, so a switch never versions the Investigator", async () => {
    const net = mount();
    net.on(`PATCH /api/v1/investigators/${investigator().id}`, { status: 200, json: item(investigatorDetail({ enabled: false })) });
    await until(() => expect(screen.getByRole("button", { name: "Switch off" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Switch off" }));
    await until(() => expect(net.calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(net.calls.find((c) => c.method === "PATCH")?.body).toEqual({ enabled: false });
  });

  it("says that a prompt edit writes the next version, and sends the prompt alone", async () => {
    const net = mount();
    net.on(`PATCH /api/v1/investigators/${investigator().id}`, { status: 200, json: item(investigatorDetail()) });
    await openEdit();
    expect(screen.queryByText(/This change writes/)).toBeNull();

    fireEvent.input(document.querySelector("#investigator-prompt") as HTMLTextAreaElement, {
      target: { value: "A new prompt." },
    });
    await until(() => expect(screen.getByText(/version 4/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await until(() => expect(net.calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(net.calls.find((c) => c.method === "PATCH")?.body).toEqual({ prompt: "A new prompt." });
  });

  it("keeps a held Tool that nothing offers visible, so it can be dropped", async () => {
    mount([
      investigator({
        current_version: { ...investigator().current_version, tools: ["gone__thing", "oto_case_timeline"] },
      }),
    ]);
    await openEdit();
    await until(() => expect(screen.getByText("gone__thing")).toBeTruthy());
    expect(screen.getByText("Held but not offered")).toBeTruthy();
  });

  it("disables Save until something changed", async () => {
    mount();
    await openEdit();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("points at Model providers when there is none to choose", async () => {
    const net = mount([]);
    net.on("GET /api/v1/model-providers", list([]));
    await until(() => expect(screen.getByRole("button", { name: "Add an Investigator" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add an Investigator" }));
    await until(() => expect(screen.getByText(/Add one first/)).toBeTruthy());
  });

  it("shows every version when asked", async () => {
    const net = mount();
    net.on(
      `GET /api/v1/investigators/${investigator().id}`,
      item(
        investigatorDetail({
          versions: [
            investigator().current_version,
            { ...investigator().current_version, version: 2, prompt: "Older prompt." },
          ],
        }),
      ),
    );
    await until(() => expect(screen.getByRole("button", { name: "Versions" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Versions" }));
    await until(() => expect(screen.getByText("Older prompt.")).toBeTruthy());
  });
});
