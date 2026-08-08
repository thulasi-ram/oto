/**
 * Formatting for an operational surface.
 *
 * Two rules run through all of it. First, **never round away a fact**: a
 * timestamp shown as "2m ago" always carries its absolute value in a `title`.
 * Second, **never claim certainty we do not have** (§M.1) — `expired` is "oto
 * stopped hearing about this", not "resolved".
 */

/** Milliseconds. Named so the arithmetic below reads as English. */
const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

export function parseTs(ts: string | null | undefined): Date | null {
  if (!ts) return null;
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? null : d;
}

/**
 * A compact relative time: `now`, `4s`, `12m`, `3h`, `6d`, `11w`.
 *
 * Deliberately unit-suffixed rather than prose — an operator scanning a dense
 * table reads `12m` faster than "12 minutes ago", and it never wraps a column.
 */
export function relativeTime(ts: string | null | undefined, now: number = Date.now()): string {
  const d = parseTs(ts);
  if (!d) return "—";
  const delta = now - d.getTime();
  const future = delta < 0;
  const abs = Math.abs(delta);

  let out: string;
  if (abs < 5 * SECOND) out = "now";
  else if (abs < MINUTE) out = `${Math.floor(abs / SECOND)}s`;
  else if (abs < HOUR) out = `${Math.floor(abs / MINUTE)}m`;
  else if (abs < DAY) out = `${Math.floor(abs / HOUR)}h`;
  else if (abs < 28 * DAY) out = `${Math.floor(abs / DAY)}d`;
  else out = `${Math.floor(abs / (7 * DAY))}w`;

  if (out === "now") return out;
  return future ? `in ${out}` : out;
}

/** The absolute value, always available behind a `title`. Local zone, with offset. */
export function absoluteTime(ts: string | null | undefined): string {
  const d = parseTs(ts);
  if (!d) return "unknown";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  });
}

/** `14:03:22` — the timeline gutter, where the date is already established. */
export function clockTime(ts: string | null | undefined): string {
  const d = parseTs(ts);
  if (!d) return "--:--:--";
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

/** `Thu 7 Aug 2026` — the timeline's day separator. */
export function calendarDay(ts: string | null | undefined): string {
  const d = parseTs(ts);
  if (!d) return "unknown date";
  return d.toLocaleDateString(undefined, {
    weekday: "short",
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

/** True when two timestamps fall on different local calendar days. */
export function differentDay(a: string | null | undefined, b: string | null | undefined): boolean {
  const da = parseTs(a);
  const db = parseTs(b);
  if (!da || !db) return true;
  return da.toDateString() !== db.toDateString();
}

/**
 * A duration in seconds rendered the way Prometheus writes it: `10m`, `1h30m`,
 * `45s`. This is what `for:` clauses and firing durations use.
 *
 * Note the vocabulary: what other tools call MTTR, oto calls **firing
 * duration**, because oto measures the signal and not anybody's response.
 */
export function duration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return "—";
  const s = Math.max(0, Math.round(seconds));
  if (s === 0) return "0s";
  if (s < 60) return `${s}s`;

  const d = Math.floor(s / 86_400);
  const h = Math.floor((s % 86_400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const rem = s % 60;

  const parts: string[] = [];
  if (d > 0) parts.push(`${d}d`);
  if (h > 0) parts.push(`${h}h`);
  if (m > 0 && d === 0) parts.push(`${m}m`);
  if (rem > 0 && d === 0 && h === 0) parts.push(`${rem}s`);
  return parts.join("") || "0s";
}

/** The elapsed span between two instants, as a duration. */
export function elapsed(
  from: string | null | undefined,
  to: string | null | undefined = null,
  now: number = Date.now(),
): string {
  const a = parseTs(from);
  if (!a) return "—";
  const b = parseTs(to)?.getTime() ?? now;
  return duration((b - a.getTime()) / 1000);
}

/**
 * Clock skew between the upstream claim and oto's own clock (§E timeline).
 *
 * We surface it rather than correct it: an alert whose `occurred_at` is two
 * minutes ahead of `recorded_at` is telling you something true about the
 * cluster, and silently normalising it destroys the evidence.
 */
export const SKEW_THRESHOLD_MS = 2000;

export function skewMs(occurredAt: string, recordedAt: string): number {
  const o = parseTs(occurredAt);
  const r = parseTs(recordedAt);
  if (!o || !r) return 0;
  return o.getTime() - r.getTime();
}

export function skewIsNotable(occurredAt: string, recordedAt: string): boolean {
  return Math.abs(skewMs(occurredAt, recordedAt)) >= SKEW_THRESHOLD_MS;
}

/** `ahead by 4s` / `behind by 1m` — always says which way, never just "skew". */
export function describeSkew(occurredAt: string, recordedAt: string): string {
  const ms = skewMs(occurredAt, recordedAt);
  const dir = ms > 0 ? "ahead of" : "behind";
  return `Upstream clock ${dir} oto by ${duration(Math.abs(ms) / 1000)}`;
}

/** Thousands separators, for counts that can reach five figures. */
export function count(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  return n.toLocaleString();
}

/** Truncate for a dense cell, preserving the head, with an ellipsis character. */
export function truncate(s: string, max: number): string {
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`;
}

/** `{a="1", b="2"}` — the canonical one-line rendering of a label set. */
export function formatLabels(labels: Readonly<Record<string, string>>): string {
  const entries = Object.entries(labels).sort(([a], [b]) => a.localeCompare(b));
  return `{${entries.map(([k, v]) => `${k}=${JSON.stringify(v)}`).join(", ")}}`;
}

/** A stable, readable id fragment for `aria-describedby` and friends. */
export function shortId(id: string): string {
  return id.length <= 8 ? id : id.slice(-8);
}

/**
 * A client-generated idempotency key. Making a retried mutation safe is the
 * server's promise; supplying a stable key per user gesture is ours.
 */
export function idempotencyKey(): string {
  return globalThis.crypto.randomUUID();
}
