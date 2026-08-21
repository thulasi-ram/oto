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
 * argued: severity gets an ordinal variable because severity is ordered — the
 * WEIGHT of oto's own traced ensō — and each lifecycle state gets an unrelated
 * shape because lifecycle is not ordered at all.
 */
import { type Component } from "solid-js";

import { SeverityEnso, StateGlyph } from "~/components/glyphs";
import { cn } from "~/lib/cn";
import type { AckState, Case, ResolveReason, State } from "~/api/types";

export { SeverityEnso, StateGlyph, type GlyphState } from "~/components/glyphs";

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

/**
 * ⛔ THERE IS NO `ackState` HERE, AND ITS ABSENCE IS STRUCTURAL. This chip renders
 * one of the four lifecycle states, and `acked` is not a fifth: a receipt belongs
 * to ONE FIRING — a Case — while a state describes the signal. A prop for it sat
 * on this interface unread for a while, which is how "Acked" would have arrived
 * back on an alert as a state rather than as the separate annotation `AckChip` is.
 */
export interface StateChipProps {
  readonly state: State;
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
/* A CASE's state — deliberately the one chip in this file with no hue        */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ AN EPISODE'S STATE IS TIER A, AND IT SITS HERE SO THE CONTRAST IS UNMISSABLE.
 *
 * `StateChip` above renders the four things an ALERT can be, and §M.2 spends a
 * saturated colour on them because they are the one fact in the product worth
 * shouting. A **Case** says something much smaller about itself: it is running,
 * or it has ended. `firing` and `suppressed` are not values missing from that
 * pair — they are facts about the ALERT, because a silence mutes a label set and
 * not one episode of it — so an episode never wears the alert's badge.
 *
 * Two hues for `open`/`closed` would also be two more hues. §M.7 names the state
 * badge, the row status and the timeline marker as the only surfaces that may
 * spend one, and "is the episode I am reading still running" is none of them:
 * after the fifth reasonable exception the colour that was supposed to mean *act
 * now* means *this is a UI*. So this is a neutral chip carrying a word, and the
 * word is the whole channel (U1 is satisfied by there being no colour to miss).
 *
 * ⭐ WHAT IS FIRING IS STILL ON SCREEN — READ OFF THE ALERT. A case row reaches
 * `case.alert.state` and renders it with `StateChip`, which is the honest place
 * that fact was always coming from. This chip answers the other question.
 */
export type CaseState = Case["state"];

/** `Ended`, never `Closed`: an operator reads a signal's life, not a ticket's. */
export const CASE_STATE_LABEL: Record<CaseState, string> = {
  open: "Open",
  closed: "Ended",
};

/**
 * ⛔ `upstream` AND `timeout` ARE NEVER MERGED (§E.3, §M.1). "The upstream said
 * it ended" and "oto stopped hearing about it" are different facts, and only the
 * first one is a resolution — the copy for `timeout` must never claim otherwise.
 */
const OUTCOME_WORD: Record<NonNullable<ResolveReason>, string> = {
  upstream: "resolved",
  timeout: "timed out",
};

const OUTCOME_MEANING: Record<NonNullable<ResolveReason>, string> = {
  upstream: "This firing ended because the upstream said the alert resolved.",
  timeout:
    "This firing ended because oto stopped hearing about it while the source was healthy. That is not the same as it being fixed.",
};

const CASE_STATE_MEANING: Record<CaseState, string> = {
  open: "This firing is still running. It ends when the alert resolves upstream or oto stops hearing about it, and it is never reopened — a re-fire starts the next episode.",
  closed: "This firing has ended. A case is terminal: a re-fire starts the next episode.",
};

/*
 * ⛔⛔ THERE IS NO `CASE_STATE_RAIL` HERE ANY MORE, AND THE DELETION IS THE POINT.
 *
 * It was the neutral twin of `STATE_BAR` above: a 3 px bar down the left edge of
 * a case row, `bg-ink-muted` when the episode was open and `bg-line-strong` when
 * it had ended. `STATE_BAR` earns its place — it carries the ALERT's state, the
 * one fact §M.2 spends a saturated colour on, and `chromeExceptions` names the two
 * files allowed to draw it. This one carried a BOOLEAN, in grey, immediately beside
 * a `CaseStateChip` already saying `Open` or `Ended · resolved` in words.
 *
 * It is worth being precise about how it got there, because the shape recurs: the
 * rail originally WAS the lifecycle bar, keyed on four states and coloured. That
 * was correctly refused on this surface — §M.7 does not list a case row — and what
 * survived the refusal was the geometry with the meaning taken out. A tint that
 * survives review by being decolourised is not a status any more; it is a thing to
 * decode with nothing behind it, and it reads as decoration precisely because that
 * is what it had become.
 *
 * The two rows that drew it now say the same thing better and cheaper:
 *   - `/cases` washes the whole ENDED row (`bg-sunken`, and the title one ink
 *     tier down), which is a property of the row rather than a mark beside it.
 *   - `CasePanel` keeps its one left-edge mark for "this is the case you are
 *     reading" and lets the chip carry the state, because two neutral marks in one
 *     gutter meaning two different things is worse than either alone.
 */

export interface CaseStateChipProps {
  readonly state: CaseState;
  /**
   * How a closed episode ended, when the surface has nowhere else to say it.
   * Omit it where the row already spells the outcome out in prose — saying it
   * twice in two different vocabularies is worse than saying it once.
   */
  readonly resolveReason?: ResolveReason | undefined;
  readonly size?: "sm" | "md";
  readonly class?: string;
}

export const CaseStateChip: Component<CaseStateChipProps> = (props) => {
  const outcome = (): NonNullable<ResolveReason> | null => {
    const reason = props.resolveReason;
    if (props.state !== "closed" || reason === null || reason === undefined) return null;
    return reason;
  };

  const word = (): string => {
    const reason = outcome();
    return reason === null
      ? CASE_STATE_LABEL[props.state]
      : `${CASE_STATE_LABEL.closed} · ${OUTCOME_WORD[reason]}`;
  };

  const meaning = (): string => {
    const reason = outcome();
    return reason === null ? CASE_STATE_MEANING[props.state] : OUTCOME_MEANING[reason];
  };

  return (
    <span
      class={cn(
        "inline-flex shrink-0 items-center rounded-chip border border-line bg-raised font-medium text-ink-muted",
        props.size === "sm" ? "px-1 py-px text-meta leading-4" : "px-1.5 py-0.5 text-body",
        props.class,
      )}
      title={meaning()}
    >
      {word()}
    </span>
  );
};

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
 * ⛔ ONLY A **CASE** MAY WEAR THIS, AND THAT IS NOT A STYLE RULE. The ack comes
 * from an EPISODE — one contiguous firing — so this chip is legitimate on
 * `/cases`, on `/cases/:id`, and nowhere an ALERT is the subject. It used to be
 * rendered on the alert row and in the alert detail's header off
 * `current_case?.ack_state`, which read as "this alert is acknowledged" when the
 * fact was "somebody signed for whichever firing happens to be open" — and an
 * identity outlives its firings, so the claim would go on being displayed about a
 * firing nobody had looked at.
 *
 * `ackState` is nullable because a case list may be rendered before its state is
 * known. Absent renders as nothing, which is the same thing `unacked` renders as.
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

/**
 * The hue each known severity is drawn in.
 *
 * ⭐ EXPORTED, AND THE EXPORT IS THE POINT. This file is one of the four
 * `chromeExceptions` in `test/design/chrome_test.go` allowed to name a Tier-B
 * utility, and the gate is a per-FILE permission: any other file that wants a
 * severity in colour either earns its own standing exception — which is how §M.2's
 * scarcity dies, one reasonable entry at a time — or takes the mapping from here.
 * Taking it from here is also simply truer, because there is then exactly one
 * answer to "what colour is a warning" rather than two that agree today.
 *
 * ⛔ IT IS NOT A DEFAULT. A caller with no severity draws `text-ink-subtle`, which
 * is Tier A and needs no permission: an unknown severity is the absence of a
 * reading, not a quiet reading, and must never be coloured as though it were one.
 */
export const SEVERITY_TONE: Record<KnownSeverity, string> = {
  critical: "text-firing-solid",
  warning: "text-acked-solid",
  info: "text-info-solid",
};

/**
 * §0.3: severity is the one axis that really is ordered, so it — and only it —
 * gets the ordinal encoding, which is now the WEIGHT of the traced ensō's brush
 * stroke (see `./glyphs` for why there are two weights and not four, and why
 * `info` versus `warning` is left to hue).
 *
 * A missing or unrecognised severity ranks `0` and draws the faint ring: the mark
 * is present, the reading is absent. That is a different statement from "info",
 * and an unknown severity is never coerced into one — `severity` is a free
 * vocabulary and a deployment spelling it `sev1` must not be told it means
 * something.
 */
const SEVERITY_LEVEL: Record<KnownSeverity, number> = {
  critical: 3,
  warning: 2,
  info: 1,
};

/** The rank a severity inks, for the callers that filter by one. */
export function severityLevel(severity: string | null | undefined): number {
  const known = normaliseSeverity(severity);
  return known === null ? 0 : SEVERITY_LEVEL[known];
}

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
        known() ? SEVERITY_TONE[known() as KnownSeverity] : "text-ink-subtle",
        props.class,
      )}
      title={`Severity: ${text()}`}
    >
      <SeverityEnso level={known() === null ? 0 : SEVERITY_LEVEL[known() as KnownSeverity]} />
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

/*
 * ⛔ THERE IS NO CHIP HERE ANY MORE, AND THE ABSENCE IS THE DECISION.
 *
 * `FlappingChip` and `StormChip` both said "oto is damping this right now", and
 * neither statement is one oto can still make.
 *
 *   - **Storm damping is removed** (ADR 0042, migration `00059`). Nothing
 *     evaluates a storm, `alert_groups.storm_mode` is dropped and the field is
 *     gone from the contract, so a chip reading it had nothing to read.
 *   - **The flap detector is blind, not live** (ADR 0041 Amendment 1). A flap
 *     damped by the case retention window W appends no `case.opened` /
 *     `case.resolved` pair, and those are exactly the events `flap_score`
 *     counts — so `is_flapping` reads false precisely when an alert flaps.
 *     Flap noise is removed at case formation now, and a chip claiming
 *     "notifications became update-only with a digest" described a gate that no
 *     longer exists.
 *
 * The `alert.flapping_*` and `group.storm_*` timeline events were kept for one
 * more cut so that history could still render, and they are gone too: migration
 * `00060` narrows `ev_type_ck` to refuse all four spellings, so no timeline can
 * carry one and `features/alerts/detail/eventKinds` no longer names them.
 */

