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
 */
import { type Component, type JSX } from "solid-js";

import { cx } from "~/components/ui/primitives";
import type { AckState, State } from "~/api/types";

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

const STATE_DOT: Record<State, string> = {
  firing: "bg-firing-solid",
  suppressed: "bg-suppressed-solid",
  resolved: "bg-resolved-solid",
  expired: "bg-expired-solid",
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
    class={cx(
      "inline-flex shrink-0 items-center gap-1 rounded-[3px] border font-medium",
      props.size === "sm" ? "px-1 py-px text-[11px] leading-4" : "px-1.5 py-0.5 text-[12px]",
      STATE_STYLE[props.state],
      props.class,
    )}
    title={STATE_MEANING[props.state]}
  >
    <span
      aria-hidden="true"
      class={cx(
        "size-1.5 shrink-0 rounded-full",
        STATE_DOT[props.state],
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
export const AckChip: Component<{ readonly ackState: AckState; readonly class?: string }> = (
  props,
) =>
  props.ackState === "acked" ? (
    <span
      class={cx(
        "inline-flex shrink-0 items-center gap-1 rounded-[3px] border px-1 py-px",
        "text-[11px] font-medium leading-4",
        "border-acked-border bg-acked-fill text-acked-text",
        props.class,
      )}
      title="Someone has seen this. It is still firing."
    >
      <CheckGlyph />
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
 * The severity glyph. Three distinct shapes, so the distinction survives
 * greyscale, colour blindness and a bad monitor: a filled triangle for
 * critical, an outlined diamond for warning, a small circle for info.
 */
const SeverityGlyph: Component<{ readonly severity: KnownSeverity }> = (props) => {
  const shapes: Record<KnownSeverity, JSX.Element> = {
    critical: <path d="M6 1.2 11.2 10.6H0.8z" fill="currentColor" />,
    warning: <path d="M6 1.4 10.6 6 6 10.6 1.4 6z" fill="none" stroke="currentColor" stroke-width="1.6" />,
    info: <circle cx="6" cy="6" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6" />,
  };
  return (
    <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
      {shapes[props.severity]}
    </svg>
  );
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
      class={cx(
        "inline-flex shrink-0 items-center gap-1 text-[12px]",
        known() ? SEVERITY_COLOUR[known() as KnownSeverity] : "text-ink-subtle",
        props.class,
      )}
      title={`Severity: ${text()}`}
    >
      {known() ? (
        <SeverityGlyph severity={known() as KnownSeverity} />
      ) : (
        <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
          <rect x="2.2" y="2.2" width="7.6" height="7.6" rx="1.4" fill="none" stroke="currentColor" stroke-width="1.4" />
        </svg>
      )}
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
    class={cx(
      "inline-flex shrink-0 items-center gap-1 rounded-[3px] border border-info-border",
      "bg-info-fill px-1 py-px text-[11px] font-medium leading-4 text-info-text",
      props.class,
    )}
    title="oto has damped this as flapping. Notifications become update-only with a periodic digest — nothing is dropped."
  >
    <WaveGlyph />
    Flapping
  </span>
);

/** Storm mode: one root message with a count, per-alert replies suppressed. */
export const StormChip: Component<{ readonly class?: string }> = (props) => (
  <span
    class={cx(
      "inline-flex shrink-0 items-center gap-1 rounded-[3px] border border-info-border",
      "bg-info-fill px-1 py-px text-[11px] font-medium leading-4 text-info-text",
      props.class,
    )}
    title="More alerts joined this group at once than the storm threshold. oto posts one message with a count instead of one per alert."
  >
    Storm
  </span>
);

/* -------------------------------------------------------------------------- */
/* Glyphs                                                                     */
/* -------------------------------------------------------------------------- */

const CheckGlyph: Component = () => (
  <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
    <path
      d="M2.4 6.4 4.8 8.8 9.6 3.2"
      fill="none"
      stroke="currentColor"
      stroke-width="1.7"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);

const WaveGlyph: Component = () => (
  <svg viewBox="0 0 12 12" class="size-3 shrink-0" aria-hidden="true">
    <path
      d="M1 7.5c1.2 0 1.2-3 2.5-3s1.3 3 2.5 3 1.2-3 2.5-3 1.3 3 2.5 3"
      fill="none"
      stroke="currentColor"
      stroke-width="1.4"
      stroke-linecap="round"
    />
  </svg>
);
