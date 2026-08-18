/**
 * Tier B lives here, and almost nowhere else (§M.2).
 *
 * When a saturated colour appears anywhere in oto it means exactly one thing:
 * *this is the state of an alert.* Scarcity is what makes it loud, so this file
 * and the row/timeline status bars are the only places allowed to reach for a
 * `--oto-state-*` token.
 *
 * Two SPEC rules are structural here, not stylistic:
 *   - **U1** colour is never the only channel. Every chip carries a text label.
 *   - **U8** severity is carried by the *icon*; state is carried by the
 *     *colour*. That is the same split as the Slack card, and it is the only
 *     thing the two systems share (§M.6).
 *
 * The marks themselves live in `./glyphs`, which is where §0.3's rule is
 * argued: severity gets the ordinal bar ramp because severity is ordered, and
 * each lifecycle state gets an unrelated shape because lifecycle is not.
 */
import { type Component } from "solid-js";

import { SeverityBars, StateGlyph, WaveGlyph } from "~/components/glyphs";
import { cn } from "~/lib/cn";
import type { AckState, State } from "~/api/types";

export { SeverityBars, StateGlyph, type GlyphState } from "~/components/glyphs";

/* -------------------------------------------------------------------------- */
/* State                                                                      */
/* -------------------------------------------------------------------------- */

/**
 * The token family for each lifecycle state.
 *
 * `resolved` and `expired` are deliberately different colours because they are
 * different facts and are never merged (§E.3). Copy follows §M.1: `expired`
 * says oto stopped hearing about it, and never claims the problem went away.
 */
const STATE_STYLE: Record<State, string> = {
  firing: "border-firing-border bg-firing-fill text-firing-text",
  suppressed: "border-suppressed-border bg-suppressed-fill text-suppressed-text",
  resolved: "border-resolved-border bg-resolved-fill text-resolved-text",
  expired: "border-expired-border bg-expired-fill text-expired-text",
};

export const STATE_LABEL: Record<State, string> = {
  firing: "Firing",
  suppressed: "Suppressed",
  resolved: "Resolved",
  expired: "Expired",
};

export const STATE_MEANING: Record<State, string> = {
  firing: "The upstream is still reporting this.",
  suppressed: "Alertmanager is not delivering this — a silence, an inhibition or a mute window.",
  resolved: "The upstream said this ended.",
  expired: "oto stopped hearing about this. That is not the same as it being fixed.",
};

/** The 3 px status bar on a row (§M.2): calm at a distance, unmistakable close up. */
export const STATE_BAR: Record<State, string> = {
  firing: "bg-firing-solid",
  suppressed: "bg-suppressed-solid",
  resolved: "bg-resolved-solid",
  expired: "bg-expired-solid",
};

export interface StateChipProps {
  readonly state: State;
  readonly ackState?: AckState;
  /** Slow 2 s opacity pulse, U4 — only ever for an unacknowledged critical. */
  readonly urgent?: boolean;
  readonly size?: "sm" | "md";
  readonly class?: string;
}

export const StateChip: Component<StateChipProps> = (props) => (
  <span
    class={cn(
      "inline-flex shrink-0 items-center gap-1 rounded-chip border font-medium",
      props.size === "sm" ? "px-1 py-px text-meta leading-4" : "px-1.5 py-0.5 text-body",
      STATE_STYLE[props.state],
      props.class,
    )}
    title={STATE_MEANING[props.state]}
  >
    {/* §0.3: a shape per state, never a fill ramp. The chip's word carries the
        accessible name, so the mark itself is hidden from assistive tech. */}
    <StateGlyph
      state={props.state}
      class={cn(
        props.size === "sm" ? "size-3" : "size-3.5",
        props.urgent === true ? "oto-pulse" : "",
      )}
    />
    {STATE_LABEL[props.state]}
  </span>
);

/* -------------------------------------------------------------------------- */
/* Acknowledgement                                                            */
/* -------------------------------------------------------------------------- */

/**
 * A receipt on a signal — "a human has seen this" — and nothing more.
 *
 * SCOPE-BOUNDARY §1: the actor is metadata on the alert's row, never the
 * subject of one. So this never says "owned by", never says "assigned to", and
 * an acknowledged alert is still firing.
 */
/**
 * `ackState` is nullable because the ack now comes from an EPISODE, not from the
 * alert row — `current_case?.ack_state` — and an alert that has never
 * fired, or a caller that did not ask for the include, has no episode to read.
 * Absent renders as nothing, which is the same thing `unacked` renders as.
 */
export const AckChip: Component<{
  readonly ackState: AckState | null | undefined;
  readonly class?: string;
}> = (props) =>
  props.ackState === "acked" ? (
    <span
      class={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-chip border px-1 py-px",
        "text-meta font-medium leading-4",
        "border-acked-border bg-acked-fill text-acked-text",
        props.class,
      )}
      title="Someone has seen this. It is still firing."
    >
      <StateGlyph state="acked" tone="inherit" class="size-3" />
      Acked
    </span>
  ) : null;

/* -------------------------------------------------------------------------- */
/* Severity — carried by the icon (U8)                                        */
/* -------------------------------------------------------------------------- */

/**
 * Severity is **not a closed enum** — operators choose their own vocabulary and
 * the API matches whatever the promoted label holds. Unknown values therefore
 * get a neutral presentation rather than being coerced or dropped.
 */
export type KnownSeverity = "critical" | "warning" | "info";

export function normaliseSeverity(severity: string | null | undefined): KnownSeverity | null {
  if (!severity) return null;
  const s = severity.toLowerCase();
  if (s === "critical" || s === "crit" || s === "page" || s === "fatal" || s === "emergency") {
    return "critical";
  }
  if (s === "warning" || s === "warn" || s === "major" || s === "minor") return "warning";
  if (s === "info" || s === "information" || s === "informational" || s === "none") return "info";
  return null;
}

const SEVERITY_COLOUR: Record<KnownSeverity, string> = {
  critical: "text-firing-solid",
  warning: "text-acked-solid",
  info: "text-info-solid",
};

/**
 * §0.3: severity is the one axis that really is ordered, so it — and only it —
 * gets the ordinal encoding: an ascending count of filled bars. The unfilled
 * bars stay drawn at low opacity, so `info` and `critical` ink the same box and
 * a column of severities reads as one ruler rather than three sizes of mark.
 *
 * A missing or unrecognised severity inks zero bars: the ruler is present, the
 * reading is absent. That is a different statement from "info", and an unknown
 * severity is never coerced into one.
 */
const SEVERITY_BARS: Record<KnownSeverity, number> = {
  critical: 3,
  warning: 2,
  info: 1,
};

export interface SeverityMarkProps {
  readonly severity: string | null | undefined;
  /** Show the raw label next to the glyph. */
  readonly withLabel?: boolean;
  readonly class?: string;
}

export const SeverityMark: Component<SeverityMarkProps> = (props) => {
  const known = (): KnownSeverity | null => normaliseSeverity(props.severity);
  const text = (): string => props.severity ?? "no severity";

  return (
    <span
      class={cn(
        "inline-flex shrink-0 items-center gap-1 text-body",
        known() ? SEVERITY_COLOUR[known() as KnownSeverity] : "text-ink-subtle",
        props.class,
      )}
      title={`Severity: ${text()}`}
    >
      <SeverityBars filled={known() === null ? 0 : SEVERITY_BARS[known() as KnownSeverity]} />
      {/* U1: the glyph is never the only channel — the word is always available,
          visually when asked for and to assistive tech always. */}
      {props.withLabel === true ? (
        <span class="truncate font-medium">{text()}</span>
      ) : (
        <span class="sr-only-focusable">{text()}</span>
      )}
    </span>
  );
};

/* -------------------------------------------------------------------------- */
/* Derived signals that are not states                                        */
/* -------------------------------------------------------------------------- */

/**
 * Flapping is a **derived signal, never a state** (`AlertDTO.flap_score`), and
 * it is deliberately visible: oto switches a flapping alert to update-only
 * notification with a digest, and a silent drop would be the thing §B.6 forbids.
 */
export const FlappingChip: Component<{ readonly class?: string }> = (props) => (
  <span
    class={cn(
      "inline-flex shrink-0 items-center gap-1 rounded-chip border border-info-border",
      "bg-info-fill px-1 py-px text-meta font-medium leading-4 text-info-text",
      props.class,
    )}
    title="oto has damped this as flapping. Notifications become update-only with a periodic digest — nothing is dropped."
  >
    <WaveGlyph class="size-3" />
    Flapping
  </span>
);

/** Storm mode: one root message with a count, per-alert replies suppressed. */
export const StormChip: Component<{ readonly class?: string }> = (props) => (
  <span
    class={cn(
      "inline-flex shrink-0 items-center gap-1 rounded-chip border border-info-border",
      "bg-info-fill px-1 py-px text-meta font-medium leading-4 text-info-text",
      props.class,
    )}
    title="More alerts joined this group at once than the storm threshold. oto posts one message with a count instead of one per alert."
  >
    Storm
  </span>
);

