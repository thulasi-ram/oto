/**
 * ADR 0056 §1, "staleness is shown": how long since upstream last said anything
 * about a Case, and — while it is open — whether it can expire and, when it
 * cannot, why. Facts only. Nothing here offers a way to end a Case, and nothing
 * here decides one: it reads the same facts the reaper reads and says them.
 *
 * ⭐ THE RULE IS THE REAPER'S, RESTATED IN ONE PLACE (owner ruling R1). A Case
 * expires as `timeout` (upstream's end time plus the org's resolve grace passed
 * with no word) or `silent` (the cluster's max silence passed with no word) only
 * while its cluster has at least one live source and EVERY live source is
 * healthy (§B.4) — an HA pair is two witnesses, and the one oto cannot see might
 * be the one still carrying the alert. The cluster's max silence is the longest
 * of its live sources', or off when any one of them turned it off. With no live
 * source left and one removed, it expires as `source_removed` once a resolve
 * grace has passed since the removal. Every other shape is held open, and the
 * screen says which shape it is in, naming the sources that hold it.
 *
 * ⚠️ `silent` AND `source_removed` SHIP BEHIND A DEPLOYMENT FLAG
 * (`jobs.expire_silent_and_removed`, off by default in this release), and the
 * API does not say whether it is on. So every forecast of either is worded "if
 * enabled" — a screen that promised an expiry the reaper is not running would
 * be the one lie this module exists to avoid. `timeout` is not behind the flag
 * and is forecast plainly.
 *
 * ⛔ ACK AND SNOOZE ARE NOT INPUTS. A receipt and a held notification are facts
 * about people and about what oto says; neither changes whether upstream is
 * still speaking about the signal, so an acked or snoozed Case reads exactly as
 * any other open one does here.
 */
import type { Case, CaseSources } from "~/api/types";
import { absoluteTime, duration, parseTs, relativeTime } from "~/lib/format";

/** The Case fields this module reads. Both case shapes carry all of them. */
export interface StalenessInput {
  readonly state: Case["state"];
  readonly last_observed_at: string;
  readonly source_ends_at?: string | null;
  readonly sources?: CaseSources | null;
}

/** What an open Case's sources say about its expiry. */
export type Expiry =
  /** `sources` was not read: say nothing rather than guess. */
  | { readonly kind: "unknown" }
  /** No live source and none removed: nothing left can say it ended. */
  | { readonly kind: "no_live_source" }
  /** Its cluster's last source was removed: it expires a resolve grace later, if enabled. */
  | { readonly kind: "source_removed" }
  /**
   * Some live source is not healthy: the §B.4 guard holds it. `sources` names
   * the listed ones that are not — empty when the guard's doubt is about one
   * the list does not carry.
   */
  | { readonly kind: "not_healthy"; readonly live: number; readonly sources: readonly string[] }
  /** Every source healthy, but no upstream end time and max silence off. */
  | { readonly kind: "no_end_time"; readonly live: number; readonly silenceOff: readonly string[] }
  /** Every source healthy, and at least one of the two clocks is running. */
  | {
      readonly kind: "can_expire";
      readonly live: number;
      /** The cluster's effective max silence, when it is on. */
      readonly maxSilenceS: number | null;
      /** `last_observed_at + max silence`: when it expires as silent with no word. */
      readonly silentAt: string | null;
      /** Upstream's own end time, a resolve grace past which it expires as timed out. */
      readonly endsAt: string | null;
      /** The live sources that turned max silence off, when it is off. */
      readonly silenceOff: readonly string[];
    };

/**
 * Whether an OPEN Case can expire, and why not. `null` for an ended Case: its
 * reason is the chip's to say (`Ended · expired: silent`), not a forecast.
 */
export function caseExpiry(c: StalenessInput): Expiry | null {
  if (c.state !== "open") return null;
  const s = c.sources;
  if (s === null || s === undefined) return { kind: "unknown" };

  if (s.live === 0) return s.removed > 0 ? { kind: "source_removed" } : { kind: "no_live_source" };

  const live = s.live;
  if (!s.all_healthy) {
    const sources = s.live_sources.filter((x) => !x.healthy).map((x) => x.name);
    return { kind: "not_healthy", live, sources };
  }

  const maxSilenceS = s.max_silence_seconds;
  const silenceOff =
    maxSilenceS === null
      ? s.live_sources.filter((x) => x.max_silence_seconds === null).map((x) => x.name)
      : [];
  const endsAt = c.source_ends_at ?? null;
  if (maxSilenceS === null && endsAt === null) return { kind: "no_end_time", live, silenceOff };

  const heard = parseTs(c.last_observed_at);
  const silentAt =
    maxSilenceS === null || heard === null
      ? null
      : new Date(heard.getTime() + maxSilenceS * 1000).toISOString();
  return { kind: "can_expire", live, maxSilenceS, silentAt, endsAt, silenceOff };
}

/** True for the shapes in which the Case stays open until upstream resolves it. */
export function isHeld(e: Expiry | null): boolean {
  return (
    e !== null &&
    (e.kind === "no_live_source" || e.kind === "not_healthy" || e.kind === "no_end_time")
  );
}

/**
 * The clause every `silent` and `source_removed` forecast carries, because the
 * screen cannot know whether the deployment runs them (see the file header).
 */
const IF_ENABLED =
  "if this deployment has turned on the silent and source-removed expiries (jobs.expire_silent_and_removed, off by default in this release)";

/** A future instant as `in 21h`, a past one as `3m ago`. */
function when(ts: string, now: number): string {
  const rel = relativeTime(ts, now);
  return rel === "now" ? "now" : rel.startsWith("in ") ? rel : `${rel} ago`;
}

/** `a`, `a and b`, `a, b and c`. */
function names(xs: readonly string[]): string {
  if (xs.length <= 1) return xs[0] ?? "";
  return `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`;
}

/** Who turned max silence off, in a sentence about the cluster. */
function silenceOffFor(live: number, off: readonly string[]): string {
  if (off.length === 0) return "a source on its cluster, which turns it off for the whole cluster";
  if (live === 1) return off[0]!;
  return `${names(off)}, which turns it off for its whole cluster`;
}

/**
 * What the screen says about an open Case's expiry: `short` for a list row's
 * meta line, `long` for the Case's own screen and the row's tooltip. `null`
 * means say nothing — an ended Case, or one whose sources were not read.
 */
export function expiryNote(
  e: Expiry | null,
  now: number,
): { readonly short: string; readonly long: string } | null {
  if (e === null) return null;
  switch (e.kind) {
    case "unknown":
      return null;
    case "no_live_source":
      return {
        short: "cannot expire: no live source",
        long: "Cannot expire: its cluster has no live source, so nothing is left that could say it ended. It stays open until upstream resolves it, which needs a source registered for its cluster.",
      };
    case "source_removed":
      return {
        short: "source removed: expires if enabled",
        long: `Its cluster's last source was removed. ${capitalise(IF_ENABLED)}, oto expires it as source removed on a reaper pass once the org's resolve grace has passed since the removal. Otherwise it stays open until upstream resolves it, which needs a source registered for its cluster.`,
      };
    case "not_healthy": {
      const n = e.sources.length;
      if (e.live === 1 && n === 1) {
        return {
          short: `held: ${e.sources[0]} is not healthy`,
          long: `Held: its source ${e.sources[0]} is not healthy, so oto cannot tell silence from an outage. It cannot expire until the source recovers; upstream can still resolve it.`,
        };
      }
      const who = n === 0 ? "a live source on its cluster" : names(e.sources);
      const verb = n > 1 ? "are" : "is";
      return {
        short: n === 0 ? "held: a source is not healthy" : `held: ${who} ${verb} not healthy`,
        long: `Held: ${who} ${verb} not healthy. oto expires a case only while every live source on its cluster is healthy: it cannot tell silence from an outage, and the source it cannot see might be the one still carrying this alert. It cannot expire until ${n > 1 ? "they recover" : "that source recovers"}; upstream can still resolve it.`,
      };
    }
    case "no_end_time":
      return {
        short: "cannot expire: no end time, max silence off",
        long: `Cannot expire: upstream gave no end time, and max silence is off for ${silenceOffFor(e.live, e.silenceOff)}. It stays open until upstream resolves it.`,
      };
    case "can_expire": {
      const parts: string[] = [];
      let short = "";
      // `timeout` first: it is not behind the flag, so it is the one forecast
      // the screen can make without a condition.
      if (e.endsAt !== null) {
        const passed = (parseTs(e.endsAt)?.getTime() ?? 0) <= now;
        short = `upstream end time ${when(e.endsAt, now)}`;
        parts.push(
          passed
            ? `Upstream's end time of ${absoluteTime(e.endsAt)} has passed. oto expires it as timed out on a reaper pass once the org's resolve grace has passed, unless upstream speaks first.`
            : `Upstream gave an end time of ${absoluteTime(e.endsAt)}. Once that time and then the org's resolve grace have passed with no word, oto expires it as timed out.`,
        );
      }
      if (e.maxSilenceS !== null && e.silentAt !== null) {
        const after = duration(e.maxSilenceS);
        const due = (parseTs(e.silentAt)?.getTime() ?? 0) <= now;
        if (short === "") short = `expires as silent after ${after} without word, if enabled`;
        parts.push(
          due
            ? `${capitalise(IF_ENABLED)}, it expires as silent after ${after} without word: that has passed, so it expires on the reaper's next pass unless upstream speaks first.`
            : `${capitalise(IF_ENABLED)}, it expires as silent after ${after} without word: ${when(e.silentAt, now)} (${absoluteTime(e.silentAt)}) if nothing arrives first.`,
        );
      } else {
        parts.push(
          `Max silence is off for ${silenceOffFor(e.live, e.silenceOff)}, so it cannot expire as silent.`,
        );
      }
      return { short, long: parts.join(" ") };
    }
  }
}

function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
