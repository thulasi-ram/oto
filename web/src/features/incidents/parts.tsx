/**
 * The two things every Incident surface has to say the same way: what state the
 * Incident is in, and who drew it.
 *
 * They live here, once, because the list and the detail page would otherwise each
 * grow their own sentence for both — and the two facts are exactly the ones whose
 * wording carries the ruling (ADR 0052). A second copy is where "quiet" starts
 * being called "resolved" on one screen and not the other.
 */
import type { Component } from "solid-js";

import type { IncidentAttribution, IncidentState } from "~/api/types";
import { cn } from "~/lib/cn";

/** The word, from the contract's own two values. */
export const INCIDENT_STATE_LABEL: Record<IncidentState, string> = {
  active: "Active",
  quiet: "Quiet",
};

/**
 * What the word means, on hover.
 *
 * ⛔ "QUIET" IS NOT "RESOLVED", AND THE SENTENCE SAYS SO. A quiet Incident is one
 * whose member Cases have all ended; whether the problem is FIXED is a fact about
 * the response, and the response lives in the incident tool the Incident is
 * declared to. The two may disagree, and that is correct (ADR 0052 §3).
 */
export const INCIDENT_STATE_MEANING: Record<IncidentState, string> = {
  active:
    "At least one Case in this Incident is still open. Read off its Cases — no person set this, and none can.",
  quiet:
    "Every Case in this Incident has ended. That is a fact about the signals, not a verdict that the problem is fixed — that lives in the incident tool, not here.",
};

/**
 * The derived state, as a chip.
 *
 * ⛔ IT IS A READING AND NEVER A CONTROL. It is a `span`, not a button, a select
 * or a toggle, and no screen offers a way to change it: the server derives it on
 * every request from whether any member Case is open, and the contract has no
 * request field that could carry it. A control here would be a status a hand can
 * set, which is the response the owner ruled out.
 *
 * Tier A only — the same neutral treatment `CaseStateChip` gives an episode's own
 * state. "Active" is not "firing": an Incident does not fire, its Cases do, and
 * borrowing a firing hue would claim the Incident is a signal.
 */
export const IncidentStateChip: Component<{
  readonly state: IncidentState;
  readonly size?: "sm" | "md";
  readonly class?: string;
}> = (props) => (
  <span
    class={cn(
      "inline-flex shrink-0 items-center rounded-chip border border-line bg-raised font-medium",
      props.state === "active" ? "text-ink" : "text-ink-muted",
      props.size === "sm" ? "px-1 py-px text-meta leading-4" : "px-1.5 py-0.5 text-body",
      props.class,
    )}
    title={INCIDENT_STATE_MEANING[props.state]}
    data-incident-state={props.state}
  >
    {INCIDENT_STATE_LABEL[props.state]}
  </span>
);

/**
 * "Why is this here?", in words.
 *
 * Exactly two answers exist (ADR 0052 §2): a Correlator an operator wrote, or a
 * human who decided. The human's name is the label frozen when they acted —
 * attribution, never ownership, and never aggregated per person.
 */
export function describeAttribution(by: IncidentAttribution): string {
  if (by.kind === "correlator") return "a Correlator";
  const label = by.label;
  return label !== null && label !== undefined && label !== "" ? label : "a person";
}
