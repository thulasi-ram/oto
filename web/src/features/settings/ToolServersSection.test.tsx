/**
 * The tool-server screen, judged on the trust boundary it asks the operator to declare.
 *
 * ⛔ THE FAILURE THESE TESTS EXIST FOR IS A GUESSED `access`. The contract gives it no default, and a
 * form that preselected "read" would put a write Tool in a model's hands whenever somebody clicked
 * through, so the first test is that Create cannot be pressed until a choice is made.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { ToolServersSection } from "./ToolServersSection";
import { toolServer, toolServerTool } from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const ID = "5c4b3a29-1807-4f6e-8d5c-4b3a29180716";

function mount(servers: readonly ReturnType<typeof toolServer>[] = []): FetchStub {
  const net = stubFetch({
    "GET /api/v1/tool-servers": list(servers),
    [`GET /api/v1/tool-servers/${ID}/tools`]: list([
      toolServerTool(),
      toolServerTool({
        name: "pods_delete",
        qualified_name: "k8s__pods_delete",
        description: "Delete a pod.",
        read_only_hint: false,
        usable: false,
        unusable_reason: "the ToolServer says this Tool is not read-only",
      }),
    ]),
  });
  renderScreen(() => <ToolServersSection />);
  return net;
}

function type(id: string, value: string): void {
  fireEvent.input(document.querySelector(`#${id}`) as HTMLInputElement, { target: { value } });
}

describe("the tool-server screen", () => {
  it("lists a server with its declared access and whether a token is held", async () => {
    mount([toolServer(), toolServer({ id: "x", name: "vm", access: "write", has_token: false })]);
    await until(() => expect(screen.getByText("k8s")).toBeTruthy());
    expect(screen.getByText("read")).toBeTruthy();
    expect(screen.getByText("write")).toBeTruthy();
    expect(screen.getByText("token set")).toBeTruthy();
    expect(screen.getByText("no token")).toBeTruthy();
  });

  it("will not create a server until access has been chosen", async () => {
    const net = mount();
    await until(() => expect(screen.getByRole("button", { name: "Add a tool server" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a tool server" }));
    await until(() => expect(document.querySelector("#toolserver-name")).toBeTruthy());
    type("toolserver-name", "k8s");
    type("toolserver-url", "https://mcp.example/mcp");

    const add = screen.getByRole("button", { name: "Add tool server" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    fireEvent.click(add);
    expect(net.calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("shows why a Tool cannot be held, and discovers on request", async () => {
    const net = mount([toolServer()]);
    net.on(`POST /api/v1/tool-servers/${ID}/discover`, list([toolServerTool()]));
    await until(() => expect(screen.getByRole("button", { name: "Tools" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    await until(() => expect(screen.getByText("k8s__pods_delete")).toBeTruthy());
    expect(screen.getAllByText(/not read-only/).length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "Discover" }));
    await until(() => expect(net.calls.some((c) => c.method === "POST" && c.path.endsWith("/discover"))).toBe(true));
  });

  it("says a failed discovery in the server's words", async () => {
    mount([
      toolServer({
        discovery_failed_at: "2026-10-02T10:00:00Z",
        discovery_error: "the ToolServer could not be reached",
      }),
    ]);
    await until(() => expect(screen.getByText(/the ToolServer could not be reached/)).toBeTruthy());
  });

  it("sends the declared access, and omits the token when none was typed", async () => {
    const net = mount();
    net.on("POST /api/v1/tool-servers", { status: 201, json: item(toolServer({ has_token: false })) });
    await until(() => expect(screen.getByRole("button", { name: "Add a tool server" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a tool server" }));
    await until(() => expect(document.querySelector("#toolserver-name")).toBeTruthy());
    type("toolserver-name", "k8s");
    type("toolserver-url", "https://mcp.example/mcp");

    // Kobalte's hidden native <select> is what carries the choice in a DOM without pointer events.
    const selects = Array.from(document.querySelectorAll("select")) as HTMLSelectElement[];
    const access = selects.find((el) => Array.from(el.options).some((o) => o.value === "write"));
    expect(access).toBeTruthy();
    fireEvent.change(access as HTMLSelectElement, { target: { value: "write" } });

    await until(() =>
      expect((screen.getByRole("button", { name: "Add tool server" }) as HTMLButtonElement).disabled).toBe(false),
    );
    fireEvent.click(screen.getByRole("button", { name: "Add tool server" }));
    await until(() => expect(net.calls.some((c) => c.method === "POST")).toBe(true));
    const body = net.calls.find((c) => c.method === "POST")?.body as Record<string, unknown>;
    expect(body["access"]).toBe("write");
    expect(body).not.toHaveProperty("token");
  });
});
