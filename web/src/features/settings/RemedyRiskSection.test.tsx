/**
 * The Remedy risk screen (ADR 0054 §3, git-bug eb4f21b), judged on what an operator must leave
 * it knowing: oto ships no rule and every Remedy then needs two; the most severe matching rule
 * wins and an unparseable command is two; the risk model may only raise and never reads the
 * Investigation; a rule that says one lets one person approve alone; and — owner ruling
 * 2026-10-05 — the rules are managed by `oto remedy-rules` from the host shell, so the screen is
 * read-only.
 */
import { screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RemedyRiskSection } from "./RemedyRiskSection";
import type { RemedyRiskRules } from "~/api/types";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/remedy-risk-rules";
const PROVIDER = "11111111-1111-4111-8111-111111111111";

function rules(patch: Partial<RemedyRiskRules> = {}): RemedyRiskRules {
  return {
    rules: [],
    risk_model_provider_id: null,
    written_by_label: null,
    written_at: null,
    reversible_verbs: ["cordon", "rollout restart", "scale"],
    ...patch,
  };
}

function mount(initial: RemedyRiskRules = rules()): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: { json: item(initial) },
    "GET /api/v1/model-providers": {
      json: list([
        {
          id: PROVIDER,
          name: "risk",
          base_url: "https://risk.test/v1",
          model: "risk-m",
          has_key: true,
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:00:00Z",
        },
      ]),
    },
  });
  renderScreen(() => <RemedyRiskSection />);
  return net;
}

describe("the Remedy risk screen", () => {
  it("⭐ starts empty — oto ships no rule — and says every Remedy then needs two", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-no-rules]")).not.toBeNull());
    expect(document.querySelector("[data-no-rules]")!.textContent).toContain("Every Remedy needs two approvals");
    expect(document.body.textContent).toContain("oto ships none");
  });

  it("⭐ says the most severe rule wins, unparseable is two, and the model only raises", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-risk-model-help]")).not.toBeNull());
    const semantics = document.querySelector("[data-semantics]")!.textContent ?? "";
    expect(semantics).toContain("most severe matching rule wins");
    expect(semantics).toContain("sh -c");
    expect(semantics).toContain("two whatever the rules say");
    const model = document.querySelector("[data-risk-model-help]")!.textContent ?? "";
    expect(model).toContain("only ever to raise it to two");
    expect(model).toContain("never the Investigation");
    expect(document.querySelector("[data-one-approval-warning]")!.textContent).toContain(
      "A rule that says one lets one person approve alone.",
    );
  });

  it("⭐ says the rules are managed by `oto remedy-rules` and offers no control that writes them", async () => {
    const net = mount();
    await until(() => expect(document.querySelector("[data-no-rules]")).not.toBeNull());
    expect(document.querySelector("[data-managed-by]")!.textContent).toContain("Managed by oto remedy-rules");
    expect(document.querySelector("[data-managed-by]")!.textContent).toContain(
      "oto remedy-rules apply --org <slug> -f rules.yaml",
    );
    // Owner ruling 2026-10-05 (git-bug eb4f21b): no button, no field, no write.
    expect(screen.queryAllByRole("button").filter((b) => b.textContent !== "Retry")).toHaveLength(0);
    expect(document.querySelectorAll("input, textarea, select")).toHaveLength(0);
    expect(net.to(PATH).every((c) => c.method === "GET")).toBe(true);
  });

  it("shows the applied rules in order, the risk model by name, and who applied them", async () => {
    mount(
      rules({
        rules: [
          {
            name: "restart-payments",
            tool: null,
            verbs: ["rollout restart"],
            kinds: ["deployment"],
            namespaces: ["payments"],
            reversibility: null,
            approvals: 1,
          },
          {
            name: "secrets-need-two",
            tool: "k8s-write__kubectl",
            verbs: [],
            kinds: ["secret"],
            namespaces: [],
            reversibility: "irreversible",
            approvals: 2,
          },
        ],
        risk_model_provider_id: PROVIDER,
        written_by_label: "oto remedy-rules apply",
        written_at: "2026-10-05T09:00:00Z",
      }),
    );
    await until(() => expect(document.querySelectorAll("[data-rule-row]")).toHaveLength(2));
    const [first, second] = [...document.querySelectorAll("[data-rule-row]")];
    expect(first!.textContent).toContain("restart-payments");
    expect(first!.querySelector("[data-approvals]")!.textContent).toBe("One approval");
    expect(first!.textContent).toContain("rollout restart");
    expect(first!.textContent).not.toContain("tool");
    expect(second!.querySelector("[data-approvals]")!.textContent).toBe("Two approvals");
    expect(second!.textContent).toContain("k8s-write__kubectl");
    expect(second!.textContent).toContain("irreversible");
    await until(() => expect(document.querySelector("[data-risk-model]")!.textContent).toContain("risk"));
    expect(document.querySelector("[data-risk-model-help]")!.textContent).toContain("daily token budget");
    expect(document.querySelector("[data-written-by]")!.textContent).toContain("oto remedy-rules apply");
  });
});
