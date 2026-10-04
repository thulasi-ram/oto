/**
 * The Classification screen (ADR 0053 §5, git-bug 4298aa0), judged on the three things
 * an operator must leave it knowing: oto ships no classes, `unclassified` is not theirs
 * to write, and paging on a classification is paging on a model's judgement.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { ClassificationSection } from "./ClassificationSection";
import { item, problem, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/investigation-classes";

function mount(classes: readonly { name: string; description: string }[] = []): FetchStub {
  const net = stubFetch({ [`GET ${PATH}`]: { json: item({ classes }) } });
  renderScreen(() => <ClassificationSection />);
  return net;
}

describe("the Classification screen", () => {
  it("⭐ starts empty — oto ships no classes — and says Findings are then unclassified by nobody", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-no-classes]")).not.toBeNull());
    expect(screen.queryByDisplayValue("noise")).toBeNull();
    expect(document.body.textContent).toContain("oto ships none");
    expect(document.body.textContent).toContain("Findings carry no");
  });

  it("⚠️ says plainly that paging on a classification is paging on a model's judgement", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-paging-warning]")).not.toBeNull());
    expect(document.querySelector("[data-paging-warning]")!.textContent).toContain(
      "Paging on a classification is paging on a model's judgement.",
    );
  });

  it("saves the whole set, in order, and shows what the server stored", async () => {
    const net = mount([{ name: "capacity", description: "Something ran out." }]);
    net.on(`PUT ${PATH}`, {
      json: item({
        classes: [
          { name: "capacity", description: "Something ran out." },
          { name: "deploy-regression", description: "" },
        ],
      }),
    });

    await until(() => expect(screen.getByDisplayValue("capacity")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a class" }));
    const name = screen.getByLabelText("Class 2 name") as HTMLInputElement;
    fireEvent.input(name, { target: { value: "deploy-regression" } });
    fireEvent.click(screen.getByRole("button", { name: "Save classes" }));

    await until(() => expect(net.to(PATH).filter((c) => c.method === "PUT")).toHaveLength(1));
    const put = net.to(PATH).find((c) => c.method === "PUT")!;
    expect(put.body).toEqual({
      classes: [
        { name: "capacity", description: "Something ran out." },
        { name: "deploy-regression", description: "" },
      ],
    });
    await until(() =>
      expect(
        (
          screen.getByRole("button", {
            name: "Save classes",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(true),
    );
  });

  it("puts the server's refusal of `unclassified` on the row that wrote it", async () => {
    const net = mount();
    net.on(`PUT ${PATH}`, () =>
      problem(422, "investigation_classes_invalid", {
        violations: [
          {
            field: "classes/0/name",
            code: "reserved",
            message: "unclassified is always admissible and is not a class you write",
          },
        ],
      }),
    );
    await until(() => expect(screen.getByRole("button", { name: "Add a class" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a class" }));
    fireEvent.input(screen.getByLabelText("Class 1 name"), {
      target: { value: "unclassified" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save classes" }));

    await until(() =>
      expect(
        screen.getByText("unclassified is always admissible and is not a class you write"),
      ).toBeTruthy(),
    );
  });
});
