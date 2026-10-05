/**
 * ADR 0056 §1, "staleness is shown": how long since upstream last said anything
 * about a Case, and — while it is open — whether it can expire and, when it
 * cannot, why. Facts only. Nothing here offers a way to end a Case, and nothing
 * here decides one: it reads the same facts the reaper reads and says them.
 *
 * ⭐ THE RULE IS THE REAPER'S, RESTATED IN ONE PLACE. A Case expires as `timeout`
 * (upstream's end time passed with no word) or `silent` (its source's max silence
 * passed with no word) only under its cluster's ONE live source, and only while
 * that source is healthy (§B.4). With no live source left and one removed, it
 * expires as `source_removed` on the next pass. Every other shape is held open,
 * and the screen says which shape it is in.
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
  /** Its cluster's last source was removed: it expires on the next pass. */
  | { readonly kind: "source_removed" }
  /** An HA pair or more: the reaper cannot tell which source carried it. */
  | { readonly kind: "several_sources"; readonly live: number }
  /** Its one source is not healthy: the §B.4 guard holds it. */
  | { readonly kind: "not_healthy"; readonly source: string }
  /** Healthy, but no upstream end time and max silence off. */
  | { readonly kind: "no_end_time"; readonly source: string }
  /** Healthy, and at least one of the two clocks is running. */
  | {
      readonly kind: "can_expire";
      readonly source: string;
      /** The source's max silence, when it is on. */
      readonly maxSilenceS: number | null;
      /** `last_observed_at + max silence`: when it expires as silent with no word. */
      readonly silentAt: string | null;
      /** Upstream's own end time, past which it expires as timed out. */
      readonly endsAt: string | null;
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
  if (s.live > 1 || s.source === null) return { kind: "several_sources", live: s.live };

  const source = s.source.name;
  if (!s.source.healthy) return { kind: "not_healthy", source };

  const maxSilenceS = s.source.max_silence_seconds;
  const endsAt = c.source_ends_at ?? null;
  if (maxSilenceS === null && endsAt === null) return { kind: "no_end_time", source };

  const heard = parseTs(c.last_observed_at);
  const silentAt =
    maxSilenceS === null || heard === null
      ? null
      : new Date(heard.getTime() + maxSilenceS * 1000).toISOString();
  return { kind: "can_expire", source, maxSilenceS, silentAt, endsAt };
}

/** True for the shapes in which the Case stays open until upstream resolves it. */
export function isHeld(e: Expiry | null): boolean {
  return (
    e !== null &&
    (e.kind === "no_live_source" ||
      e.kind === "several_sources" ||
      e.kind === "not_healthy" ||
      e.kind === "no_end_time")
  );
}

/** `3h ago`, or `just now` — never `now ago`. */
export function ago(ts: string | null | undefined, now: number): string {
  const rel = relativeTime(ts, now);
  return rel === "now" ? "just now" : `${rel} ago`;
}

/** A future instant as `in 21h`, a past one as `3m ago`. */
function when(ts: string, now: number): string {
  const rel = relativeTime(ts, now);
  return rel === "now" ? "now" : rel.startsWith("in ") ? rel : `${rel} ago`;
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
        short: "expiring: its source was removed",
        long: "Its cluster's last source was removed. oto expires it as source removed on the reaper's next pass.",
      };
    case "several_sources":
      return {
        short: `cannot expire: ${e.live} live sources`,
        long: `Cannot expire: ${e.live} live sources feed its cluster, and oto expires a case only under exactly one, because it cannot tell which of them carried this alert. It stays open until upstream resolves it.`,
      };
    case "not_healthy":
      return {
        short: `held: ${e.source} is not healthy`,
        long: `Held: its source ${e.source} is not healthy, so oto cannot tell silence from an outage. It cannot expire until the source recovers; upstream can still resolve it.`,
      };
    case "no_end_time":
      return {
        short: "cannot expire: no end time, max silence off",
        long: `Cannot expire: upstream gave no end time, and max silence is off for ${e.source}. It stays open until upstream resolves it.`,
      };
    case "can_expire": {
      const parts: string[] = [];
      let short = "";
      if (e.maxSilenceS !== null && e.silentAt !== null) {
        const after = duration(e.maxSilenceS);
        const due = (parseTs(e.silentAt)?.getTime() ?? 0) <= now;
        short = `expires as silent after ${after} without word`;
        parts.push(
          due
            ? `Expires as silent after ${after} without word: that has passed, so it expires on the reaper's next pass unless upstream speaks first.`
            : `Expires as silent after ${after} without word: ${when(e.silentAt, now)} (${absoluteTime(e.silentAt)}) if nothing arrives first.`,
        );
      }
      if (e.endsAt !== null) {
        if (short === "") short = `upstream end time ${when(e.endsAt, now)}`;
        parts.push(
          `Upstream gave an end time of ${absoluteTime(e.endsAt)}. If it passes with no word, oto expires it as timed out.`,
        );
      }
      return { short, long: parts.join(" ") };
    }
  }
}
