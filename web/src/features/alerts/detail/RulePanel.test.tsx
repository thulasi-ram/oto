/**
 * "Rule at fire time" when there is no rule — TICKET 015b25b.
 *
 * ⛔ THE ABSENCE OF A SNAPSHOT IS NOT A FAILURE AND MUST NOT LOOK LIKE ONE.
 * Four alerts in five ingested through the real webhook have no rule bound to
 * them: a Grafana-sourced alert, one fired by hand, an Alertmanager whose
 * Prometheus is unreachable, a `generatorURL` with no `g0.expr` in it. The
 * server used to report all of them as `422 rules_invalid_id` — "a rule key's
 * source id must be a UUID" — and this panel dutifully rendered a red
 * "Validation failed" box with a **Try again** button and a request id, on the
 * one screen the README's headline promise points at.
 *
 * Every part of that was a lie in a different direction: it was not a
 * validation failure, nothing the operator did caused it, the sentence was
 * vocabulary from a database index, and the retry could never succeed. The
 * alert LIST has always shown these same alerts as a plain em-dash. These tests
 * pin the panel to that same calm, at the size a panel affords.
 *
 * The server body is stubbed at the TRANSPORT, so the real `~/api/client`, its
 * envelope decoding and the real `getAlertRuleHistory` all run: a test that
 * mocked the endpoint module could not tell a 200 carrying `current: null` from
 * a 422, which is the exact distinction under test.
 */
import { screen } from "@solidjs/testing-library";
import { useQuery } from "@tanstack/solid-query";
import { describe, expect, it } from "vitest";

import { RulePanel } from "./RuleDrift";
import { getAlertRuleHistory } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { RuleHistory } from "~/api/types";
import { ruleSnapshot } from "~/test/fixtures";
import { item, renderScreen, stubFetch, until } from "~/test/harness";

const ID = "0198f3c2-1111-7111-8111-111111111111";
const PATH = `/api/v1/alerts/${ID}/rule`;

/**
 * The body a server answers for an alert nothing was ever captured for.
 *
 * `current: null` and `versions: []` are what the contract already declares —
 * `RuleHistoryDTO.current` is `oneOf [RuleSnapshotDTO, null]` — so no schema
 * change was needed to say this, only a server that stopped calling it invalid.
 * `rule_key` still carries the alertname, because that much oto CAN name from
 * the alert itself; the nil `source_id` is the honest admission that it cannot
 * name the source.
 */
const NO_RULE: RuleHistory = {
  rule_key: {
    source_id: "00000000-0000-0000-0000-000000000000",
    rule_name: "HighErrorRate",
  },
  current: null,
  change: null,
  versions: [],
};

/** Render the panel over whatever the server currently answers at `PATH`. */
function mount(body: unknown): void {
  stubFetch({ [`GET ${PATH}`]: () => ({ json: item(body) }) });

  const Screen = () => {
    const rule = useQuery(() => ({
      queryKey: qk.alerts.rule(ID),
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        getAlertRuleHistory(ID, {}, { signal }),
    }));
    return <>{rule.data ? <RulePanel history={rule.data} /> : <p>loading</p>}</>;
  };

  renderScreen(() => <Screen />);
}

describe("the rule panel with nothing to show", () => {
  it("says oto captured no rule, in words an operator can act on", async () => {
    mount(NO_RULE);

    await until(() => screen.getByText(/oto captured no rule for this alert/i));

    // The ordinary causes are named, because "is this broken or is this just
    // how my alert works?" is the only question the operator actually has.
    expect(screen.getByText(/ordinary outcome, not a failure/i)).toBeTruthy();
    expect(screen.getByText(/Grafana/i)).toBeTruthy();
  });

  it("offers no retry, because there is nothing to retry", async () => {
    mount(NO_RULE);

    await until(() => screen.getByText(/oto captured no rule for this alert/i));

    // ⛔ The button was the worst part of the old rendering. A retry that can
    // never succeed is worse than no affordance at all: it reads as "oto is
    // flaky, press again" when the truth is "there is no rule here".
    expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
    expect(screen.queryByText(/try again/i)).toBeNull();
  });

  it("shows no error box and none of the server's internal vocabulary", async () => {
    mount(NO_RULE);

    await until(() => screen.getByText(/oto captured no rule for this alert/i));

    // `ErrorState` is the only thing on this panel that carries role=alert.
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/validation failed/i)).toBeNull();
    expect(screen.queryByText(/must be a UUID/i)).toBeNull();
    expect(screen.queryByText(/request [0-9a-f]/i)).toBeNull();
  });

  it("still names the panel, so the absence is an answer and not a missing section", async () => {
    mount(NO_RULE);

    await until(() => screen.getByText(/oto captured no rule for this alert/i));

    // A panel that vanished would be indistinguishable from one that never
    // loaded — the same "oto's silence is ambiguous" failure in miniature.
    expect(screen.getByText("Rule at fire time")).toBeTruthy();
    // …and no version count, because there are no versions to count.
    expect(screen.queryByText(/versions? captured/i)).toBeNull();
  });

  it("keeps the ordinary path intact: a captured rule still renders its expression", async () => {
    // The guard on the guard. A fallback broad enough to swallow a real
    // snapshot would pass every assertion above and lose the product.
    const captured = ruleSnapshot({ expr: "up == 0" });
    mount({
      rule_key: {
        source_id: captured.source_id,
        rule_name: captured.rule_name,
        rule_group: captured.rule_group,
        rule_file: captured.rule_file,
      },
      current: captured,
      change: null,
      versions: [captured],
    } satisfies RuleHistory);

    await until(() => screen.getByText("up == 0"));

    expect(screen.queryByText(/oto captured no rule/i)).toBeNull();
    expect(screen.getByText("1 version captured")).toBeTruthy();
  });
});
