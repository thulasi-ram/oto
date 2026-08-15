/**
 * Alertmanager-style label matchers (ADR 0017).
 *
 * `{namespace="payments", severity=~"critical|warning"}` is the native idiom of
 * oto's audience and it translates directly to indexed SQL. It is deliberately
 * **not** a general expression language: ADR 0017 rejects CEL precisely because
 * only a subset of it survives translation, and users then discover the boundary
 * at unpredictable places.
 *
 * The whole expression travels to the server verbatim in the `matcher=` query
 * parameter, which is the only spelling in the contract that can carry a
 * regular-expression operator. All four operators — `=`, `!=`, `=~`, `!~` — are
 * sent.
 *
 * The one boundary that stays client-side is the one ADR 0017 makes binding:
 *
 * > anything untranslatable is rejected at parse time with a precise message —
 * > never silently degraded to a scan.
 *
 * A `=~`/`!~` value that is an **alternation of literals** (`critical|warning`,
 * optionally anchored) is an `IN` list and is served from the label GIN index.
 * A value carrying a metacharacter is a sequential scan over every alert in the
 * org, and the server refuses it at parse time with a `422`. We say so *before*
 * sending rather than pretending it will work, because a filter that works in
 * staging and times out during an incident is worse than one that says no.
 */

import { maxLengthOf, patternOf } from "~/api/bounds";
import { LabelNameSchema, MatcherOpSchema } from "~/api/generated/validators";

/** The contract's `MatcherOp`, not a fifth spelling of it. */
export type MatcherOperator = (typeof MatcherOpSchema.options)[number];

export interface LabelMatcher {
  readonly name: string;
  readonly op: MatcherOperator;
  readonly value: string;
}

export interface ParseError {
  /** Character offset into the input, for a caret in the error message. */
  readonly at: number;
  readonly message: string;
}

export interface ParseResult {
  readonly matchers: readonly LabelMatcher[];
  readonly errors: readonly ParseError[];
}

/**
 * Alertmanager's own label-name rule, and the one the API enforces (§E.3) — read
 * off the contract's `LabelName` rather than re-typed from it.
 *
 * ⛔ A LABEL NAME HAS **TWO** RULES AND THIS FILE ONLY HAD ONE. The charset was
 * copied faithfully; the 1024-character cap beside it was dropped, so a 2000-byte
 * label name parsed cleanly here and then came back as a 422 from a request the
 * user had no way to see coming. Both halves now come from the same object.
 */
const LABEL_NAME = patternOf(LabelNameSchema);
const LABEL_NAME_MAX = maxLengthOf(LabelNameSchema);

/**
 * Operators, longest first — `!=` must win over `!` and `=~` over `=`.
 *
 * The *set* is the contract's; only the ordering is this parser's, and it is a
 * property of tokenising rather than a fact about the API. Sorting by length is
 * what "longest first" means, so an operator the contract adds is tokenised
 * correctly instead of being unparseable until somebody notices this list.
 */
const OPERATORS: readonly MatcherOperator[] = [...MatcherOpSchema.options].sort(
  (a, b) => b.length - a.length,
);

/** Every operator the contract's `matcher=` parameter accepts. All four are sent. */
export const SUPPORTED_OPERATORS: readonly MatcherOperator[] = MatcherOpSchema.options;

export function isRegexOperator(op: MatcherOperator): boolean {
  return op === "=~" || op === "!~";
}

/**
 * The metacharacters that turn a regex matcher into a sequential scan.
 *
 * Verbatim from the contract's `matcher` parameter: `.` `*` `+` `?` `(` `)`
 * `[` `]` `{` `}` `\`. Note what is **absent** — `|` is how an alternation is
 * spelled and `^`/`$` are the anchors an alternation may carry, so all three
 * are served.
 *
 * ⚠️ NOT DERIVABLE. The contract states this list in the *prose* description of
 * the `matcher` query parameter, so `gen-validators.mjs` emits nothing to read it
 * from; there is no generated value for `~/api/bounds` to return.
 */
const REGEX_METACHARACTERS = [".", "*", "+", "?", "(", ")", "[", "]", "{", "}", "\\"] as const;

export interface RegexVerdict {
  /** True when the value is an alternation of literals the index can answer. */
  readonly servable: boolean;
  /** The metacharacters that made it unservable, in the order found. */
  readonly offending: readonly string[];
}

/**
 * Decide whether a `=~`/`!~` value is index-backed or a scan.
 *
 * Servable: an alternation of literal values with optional `^` and `$` anchors.
 * That is exactly an `IN` list, and it compiles to one indexed containment
 * lookup per alternative.
 */
export function classifyRegexValue(value: string): RegexVerdict {
  let body = value;
  if (body.startsWith("^")) body = body.slice(1);
  if (body.endsWith("$")) body = body.slice(0, -1);

  const offending: string[] = [];
  for (const ch of REGEX_METACHARACTERS) {
    if (body.includes(ch) && !offending.includes(ch)) offending.push(ch);
  }
  return { servable: offending.length === 0, offending };
}

/**
 * The refusal, said the way the server says it.
 *
 * It names the boundary and both spellings that do work, because "invalid
 * filter" teaches nobody anything at 3am.
 */
export function describeRegexRefusal(m: LabelMatcher, verdict: RegexVerdict): string {
  const chars = verdict.offending.map((c) => `\`${c}\``).join(" ");
  return (
    `oto refuses this at parse time. ${chars} cannot use the label index, so this would be a ` +
    `sequential scan over every alert in the org — a filter that works in staging and times out ` +
    `during an incident. What is served is an alternation of literal values, optionally anchored: ` +
    `\`${m.name}${m.op}"critical|warning"\`. For a substring search use the free-text box, which is ` +
    `backed by a full-text index.`
  );
}

/**
 * Parse a matcher expression. Surrounding braces are optional, so both
 * `{a="1", b="2"}` and `a="1", b="2"` are accepted.
 *
 * Parsing never throws and always returns whatever it could understand
 * alongside every error, so the input can highlight all the problems at once
 * instead of making the user fix them one keystroke at a time.
 */
export function parseMatchers(input: string): ParseResult {
  const matchers: LabelMatcher[] = [];
  const errors: ParseError[] = [];

  const trimmed = input.trim();
  if (trimmed === "") return { matchers, errors };

  // Offset of `body` within `input`, so error positions point at what the user
  // actually typed rather than at our normalised copy.
  let base = input.indexOf(trimmed);
  let body = trimmed;
  if (body.startsWith("{")) {
    if (!body.endsWith("}")) {
      errors.push({ at: input.length, message: "unclosed `{` — add a closing `}`" });
      body = body.slice(1);
      base += 1;
    } else {
      body = body.slice(1, -1);
      base += 1;
    }
  }

  for (const segment of splitTopLevel(body)) {
    const raw = segment.text;
    const text = raw.trim();
    if (text === "") continue;
    const at = base + segment.start + (raw.length - raw.trimStart().length);

    const found = findOperator(text);
    if (!found) {
      errors.push({
        at,
        message: `\`${text}\` is not a matcher — expected name="value", name!="value", name=~"regex" or name!~"regex"`,
      });
      continue;
    }

    const name = text.slice(0, found.index).trim();
    const rest = text.slice(found.index + found.op.length).trim();

    if (name === "") {
      errors.push({ at, message: "missing label name before the operator" });
      continue;
    }
    if (!LABEL_NAME.test(name)) {
      errors.push({
        at,
        message: `\`${name}\` is not a valid label name — must match ${LABEL_NAME.source}`,
      });
      continue;
    }
    if (name.length > LABEL_NAME_MAX) {
      errors.push({
        at,
        message: `that label name is ${name.length} characters — the API accepts at most ${LABEL_NAME_MAX}`,
      });
      continue;
    }

    const value = unquote(rest);
    if (value === null) {
      errors.push({ at: at + found.index, message: `unterminated quoted value after \`${name}\`` });
      continue;
    }

    matchers.push({ name, op: found.op, value });
  }

  return { matchers, errors };
}

/** Split on commas that are not inside a quoted string. */
function splitTopLevel(s: string): readonly { text: string; start: number }[] {
  const out: { text: string; start: number }[] = [];
  let quote: '"' | "'" | null = null;
  let start = 0;

  for (let i = 0; i < s.length; i += 1) {
    const ch = s[i];
    if (quote !== null) {
      if (ch === "\\") i += 1;
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      continue;
    }
    if (ch === ",") {
      out.push({ text: s.slice(start, i), start });
      start = i + 1;
    }
  }
  out.push({ text: s.slice(start), start });
  return out;
}

function findOperator(text: string): { op: MatcherOperator; index: number } | null {
  let best: { op: MatcherOperator; index: number } | null = null;
  for (const op of OPERATORS) {
    const index = text.indexOf(op);
    if (index < 0) continue;
    // Earliest position wins; on a tie the longer operator wins, which is why
    // OPERATORS is ordered longest-first and we only replace on a strict <.
    if (best === null || index < best.index) best = { op, index };
  }
  return best;
}

/** Strip matching quotes and unescape. Returns `null` if a quote is unterminated. */
function unquote(s: string): string | null {
  if (s === "") return "";
  const q = s[0];
  if (q !== '"' && q !== "'") return s;
  if (s.length < 2 || s[s.length - 1] !== q) return null;

  const inner = s.slice(1, -1);
  let out = "";
  for (let i = 0; i < inner.length; i += 1) {
    const ch = inner[i];
    if (ch === "\\" && i + 1 < inner.length) {
      const next = inner[i + 1] as string;
      out += next === "n" ? "\n" : next === "t" ? "\t" : next;
      i += 1;
    } else {
      out += ch ?? "";
    }
  }
  return out;
}

/** Render matchers back to canonical text, for the URL and for the input. */
export function formatMatchers(matchers: readonly LabelMatcher[]): string {
  if (matchers.length === 0) return "";
  const inner = matchers.map((m) => `${m.name}${m.op}${JSON.stringify(m.value)}`).join(", ");
  return `{${inner}}`;
}

export interface CompileResult {
  /**
   * The `matcher=` value to send, or `null` when there is nothing to filter on.
   * Canonically re-rendered so the wire form never depends on how it was typed.
   */
  readonly matcher: string | null;
  /** Matchers the server refuses at parse time. Non-empty means: do not query. */
  readonly rejected: readonly { matcher: LabelMatcher; reason: string }[];
}

/**
 * Compile matchers into the contract's `matcher=` parameter.
 *
 * All four operators go over the wire. The only thing that does not is a regex
 * the server would refuse anyway — and it is stopped here, with the server's own
 * reasoning, rather than sent to collect a 422. Returning an unfiltered page
 * because we could not express the filter is the failure mode ADR 0017 exists
 * to prevent, and so is a request we already know will fail.
 */
export function compileMatchers(matchers: readonly LabelMatcher[]): CompileResult {
  const servable: LabelMatcher[] = [];
  const rejected: { matcher: LabelMatcher; reason: string }[] = [];

  for (const m of matchers) {
    if (isRegexOperator(m.op)) {
      const verdict = classifyRegexValue(m.value);
      if (!verdict.servable) {
        rejected.push({ matcher: m, reason: describeRegexRefusal(m, verdict) });
        continue;
      }
    }
    servable.push(m);
  }

  return {
    matcher: servable.length === 0 ? null : formatMatchers(servable),
    rejected,
  };
}

/**
 * Worked examples, shown in the input rather than hidden in documentation.
 *
 * Every one of these is index-backed. The last entry is deliberately the
 * refused shape, because the boundary is easier to learn from a counterexample
 * than from a paragraph.
 */
export const MATCHER_EXAMPLES: readonly {
  readonly text: string;
  readonly note: string;
  readonly served: boolean;
}[] = [
  {
    text: '{namespace="payments"}',
    note: "Exact match on one label. Braces are optional.",
    served: true,
  },
  {
    text: 'tier!="canary"',
    note: "Negation. Distinct names AND together; repeated names OR.",
    served: true,
  },
  {
    text: 'severity=~"critical|warning"',
    note: "An alternation of literals — one indexed lookup per value.",
    served: true,
  },
  {
    text: 'tier!~"canary|staging"',
    note: "Excludes two values at once, still from the index.",
    served: true,
  },
  {
    text: 'severity=~"crit.*"',
    note: "Refused: an open-ended pattern is a sequential scan. Use the free-text search instead.",
    served: false,
  },
];

/** Round-trip a selector back into matchers, for hydrating the input from a URL. */
export function selectorToMatchers(
  selector: Readonly<Record<string, string>>,
): readonly LabelMatcher[] {
  const out: LabelMatcher[] = [];
  for (const [key, joined] of Object.entries(selector)) {
    const negated = key.startsWith("!");
    const name = negated ? key.slice(1) : key;
    for (const value of joined.split(",")) {
      if (value === "") continue;
      out.push({ name, op: negated ? "!=" : "=", value });
    }
  }
  return out;
}
