/**
 * Alertmanager-style label matchers (ADR 0017).
 *
 * `{namespace="payments", team!="canary"}` is the native idiom of oto's
 * audience and it translates directly to indexed SQL. It is deliberately **not**
 * a general expression language: ADR 0017 rejects CEL precisely because only a
 * subset of it survives translation, and users then discover the boundary at
 * unpredictable places.
 *
 * The same ADR binds this parser's most important behaviour:
 *
 * > If CEL is ever added, anything untranslatable is rejected at parse time with
 * > a precise message — never silently degraded to a scan.
 *
 * That rule applies here today. `GET /api/v1/alerts` exposes `label[k]=v` for
 * equality and `label[!k]=v` for negation, and **nothing else** (§E.3). So `=~`
 * and `!~` parse cleanly, are recognised as real matcher syntax, and are then
 * refused with an explanation — rather than being quietly dropped, which would
 * return an unfiltered list and start the dashboard lying about what it shows.
 */

export type MatcherOperator = "=" | "!=" | "=~" | "!~";

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

/** Alertmanager's own label-name rule, and the one the API enforces (§E.3). */
const LABEL_NAME = /^[a-zA-Z_][a-zA-Z0-9_]*$/;

/** Operators, longest first — `!=` must win over `!` and `=~` over `=`. */
const OPERATORS: readonly MatcherOperator[] = ["=~", "!~", "!=", "="];

/**
 * The operators the alert API can actually serve. Keep this list next to the
 * failure message: when the contract grows regex support, both change together.
 */
export const SUPPORTED_OPERATORS: readonly MatcherOperator[] = ["=", "!="];

export function isSupported(op: MatcherOperator): boolean {
  return SUPPORTED_OPERATORS.includes(op);
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
        message: `\`${name}\` is not a valid label name — must match [a-zA-Z_][a-zA-Z0-9_]*`,
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
  /** Ready to hand to the client as its `label` deepObject parameter. */
  readonly selector: Readonly<Record<string, string>>;
  /** Matchers the contract cannot express. Non-empty means: do not query. */
  readonly rejected: readonly { matcher: LabelMatcher; reason: string }[];
}

/**
 * Compile matchers to the `label[…]` selector §E.3 defines.
 *
 * Repeated equality matchers on one name OR together, which is exactly the
 * contract's comma semantics: `{team="core", team="platform"}` becomes
 * `label[team]=core,platform`. Distinct names AND together.
 *
 * A regex matcher is **rejected, never approximated**. Returning an unfiltered
 * page because we could not express the filter is the failure mode ADR 0017
 * exists to prevent.
 */
export function compileMatchers(matchers: readonly LabelMatcher[]): CompileResult {
  const positive = new Map<string, string[]>();
  const negative = new Map<string, string[]>();
  const rejected: { matcher: LabelMatcher; reason: string }[] = [];

  for (const m of matchers) {
    if (!isSupported(m.op)) {
      rejected.push({
        matcher: m,
        reason:
          `\`${m.op}\` is valid matcher syntax, but the alert API filters on equality only. ` +
          `Rewrite it with \`=\` or \`!=\`, or use the free-text search.`,
      });
      continue;
    }
    if (m.value.includes(",")) {
      rejected.push({
        matcher: m,
        reason:
          "a comma inside a value is ambiguous — the API reads commas as OR. " +
          "Write one matcher per value instead.",
      });
      continue;
    }
    const bucket = m.op === "=" ? positive : negative;
    const existing = bucket.get(m.name);
    if (existing) existing.push(m.value);
    else bucket.set(m.name, [m.value]);
  }

  const selector: Record<string, string> = {};
  for (const [name, values] of positive) selector[name] = values.join(",");
  for (const [name, values] of negative) selector[`!${name}`] = values.join(",");

  return { selector, rejected };
}

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
