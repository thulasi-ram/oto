/**
 * The server's own truth, read at test time.
 *
 * ⛔ THIS FILE EXISTS SO THAT NO TEST EVER RE-TYPES AN ENUM. A settings screen
 * that hand-copies `NotificationReason` and a test that hand-copies the same
 * list agree with each other and with nothing else: the pair passes forever
 * while the server moves underneath them, which is exactly how
 * `PoliciesSection.tsx` came to offer a reason the server answers 422 for and to
 * render `undefined` for three it accepts. So the expectations are *derived*:
 *
 *   - **Enum values and object key sets** come from `src/api/generated/schema.d.ts`,
 *     which `npm run generate` produces from `api/openapi/openapi.yaml` and CI's
 *     gate G3 asserts is byte-identical to the checked-in copy. It is the closest
 *     thing to the server that lives inside `web/`.
 *   - **Numeric bounds** come from `api/openapi/openapi.yaml` directly, because
 *     `openapi-typescript` erases `minimum` / `maximum` / `maxItems` — a TypeScript
 *     `number` cannot carry a range. The same file the generator reads, read the
 *     same way `npm run generate:check` reads it.
 *
 * Both are parsed rather than imported because the facts are *types*, and types
 * do not survive to runtime. The parsers below are deliberately strict: an
 * unreadable schema throws with the name it was looking for, so a generator
 * upgrade that changes the emitted shape fails here loudly instead of silently
 * returning an empty set that every assertion then passes against.
 */
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { fieldOf, maxLengthOf, optionsOf, rangeOf } from "~/api/bounds";

const HERE = dirname(fileURLToPath(import.meta.url));

const SCHEMA_DTS = resolve(HERE, "../api/generated/schema.d.ts");
const OPENAPI_YAML = resolve(HERE, "../../../api/openapi/openapi.yaml");

function read(path: string, what: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch (cause) {
    throw new Error(
      `oto test: cannot read ${what} at ${path}. ` +
        `The contract tests are derived from it and must not fall back to a copy. (${String(cause)})`,
    );
  }
}

let schemaSource: string | null = null;
function schema(): string {
  schemaSource ??= read(SCHEMA_DTS, "the generated TypeScript contract");
  return schemaSource;
}

let openapiSource: string | null = null;
function openapi(): string {
  openapiSource ??= read(OPENAPI_YAML, "the OpenAPI contract");
  return openapiSource;
}

/* -------------------------------------------------------------------------- */
/* Enums out of the generated .d.ts                                           */
/* -------------------------------------------------------------------------- */

/** One quoted member, or the bare `null` a nullable enum ends with. */
const MEMBER = String.raw`(?:"(?:[^"\\]|\\.)*"|null)`;

/**
 * Every value of a generated string-union enum, in declaration order.
 *
 * `null` is dropped: it is the *absence* of a value, and no UI ever renders a
 * label for it. Use {@link enumIsNullable} when that distinction matters.
 */
export function enumValues(name: string): readonly string[] {
  const line = enumLine(name);
  const out: string[] = [];
  for (const m of line.matchAll(new RegExp(MEMBER, "g"))) {
    const raw = m[0];
    if (raw === "null") continue;
    out.push(JSON.parse(raw) as string);
  }
  if (out.length === 0) {
    throw new Error(`oto test: \`${name}\` in the generated contract has no string members`);
  }
  return out;
}

/** True when the contract admits `null` for this enum — a real, separate case. */
export function enumIsNullable(name: string): boolean {
  return /(^|\|)\s*null\s*$/.test(enumLine(name));
}

function enumLine(name: string): string {
  const re = new RegExp(
    String.raw`^\s*${name}:\s*(${MEMBER}(?:\s*\|\s*${MEMBER})*);\s*$`,
    "m",
  );
  const m = re.exec(schema());
  if (m?.[1] === undefined) {
    throw new Error(
      `oto test: no string-union named \`${name}\` in the generated contract. ` +
        `Either the contract renamed it or openapi-typescript changed its output shape; ` +
        `either way this test must not guess.`,
    );
  }
  return m[1];
}

/* -------------------------------------------------------------------------- */
/* Object key sets out of the generated .d.ts                                 */
/* -------------------------------------------------------------------------- */

/**
 * The top-level property names of a generated object type, in declaration order.
 *
 * Nested objects are skipped rather than flattened — the callers want "which
 * keys does this request carry", not a path list.
 */
export function objectKeys(name: string): readonly string[] {
  const src = schema();
  const open = new RegExp(String.raw`^(\s*)${name}:\s*\{\s*$`, "m").exec(src);
  if (open === null || open[1] === undefined) {
    throw new Error(`oto test: no object type named \`${name}\` in the generated contract`);
  }

  const indent = open[1];
  const lines = src.slice(open.index + open[0].length).split("\n");
  const propIndent = `${indent}    `;
  const closing = `${indent}};`;

  const out: string[] = [];
  for (const line of lines) {
    if (line === closing) return out;
    const m = new RegExp(String.raw`^${propIndent}([A-Za-z_][A-Za-z0-9_]*)\??:`).exec(line);
    if (m?.[1] !== undefined) out.push(m[1]);
  }
  throw new Error(`oto test: the \`${name}\` block in the generated contract is unterminated`);
}

/* -------------------------------------------------------------------------- */
/* Numeric bounds out of the OpenAPI document                                 */
/* -------------------------------------------------------------------------- */

export interface Bound {
  readonly min: number;
  readonly max: number;
}

/* -------------------------------------------------------------------------- */
/* Rules out of the generated valibot schemas                                 */
/* -------------------------------------------------------------------------- */

/**
 * The same facts as above, but for request bodies, read off the *runtime*
 * schemas in `src/api/generated/validators.ts`.
 *
 * `enumValues` and `integerBounds` answer for responses, where the fact only
 * exists as a type or as YAML. A request body is different: gate G4 emits a
 * valibot schema that the forms themselves `v.pipe` into, so the bound is a
 * value sitting in memory and the honest way to assert on a form's copy of it is
 * to read the original rather than to re-type it here.
 *
 * These are `~/api/bounds`' accessors under the names the tests use — the same
 * walk over valibot's private internals the screens read their bounds through,
 * not a second copy of it. Sharing the walker is safe precisely because the
 * oracle side of every assertion stays separate: `enumValues`, `objectKeys` and
 * `integerBounds` above scan the contract's *text* (schema.d.ts, openapi.yaml)
 * with no valibot anywhere in the path, so a walker bug cannot sit on both
 * sides of a comparison and agree with itself.
 */

/** One property of a generated request schema, with its optionality peeled off. */
export function requestField(objectSchema: unknown, key: string): unknown {
  return fieldOf(objectSchema, key);
}

/** The `minimum`/`maximum` the contract puts on one request property. */
export function requestRange(objectSchema: unknown, key: string): Bound {
  return rangeOf(objectSchema, key);
}

/** The `maxLength` the contract puts on one request property. */
export function requestMaxLength(objectSchema: unknown, key: string): number {
  return maxLengthOf(objectSchema, key);
}

/** Every value a picklist admits — the property's own, or its array item's. */
export function requestOptions(objectSchema: unknown, key: string): readonly string[] {
  return optionsOf(objectSchema, key).map(String);
}

/**
 * Every `minimum`/`maximum` pair declared under one schema's `properties`.
 *
 * A hand-rolled walk rather than a YAML dependency: the document is written with
 * a fixed two-space ladder (`  <schema>:` → `    properties:` → `      <key>:` →
 * `        minimum:`), the generator already depends on that being true, and one
 * more npm package in `web/` to read eleven integers is a poor trade. If the
 * document's shape ever moves, every caller of this gets an empty map and the
 * assertions below fail on the count — which is the failure we want.
 */
export function integerBounds(schemaName: string): ReadonlyMap<string, Bound> {
  const body = schemaBlock(schemaName);
  const props = childBlock(body, "properties");
  const out = new Map<string, Bound>();

  for (const [key, block] of entriesOf(props)) {
    const min = scalar(block, "minimum");
    const max = scalar(block, "maximum");
    if (min !== null && max !== null) out.set(key, { min, max });
  }
  return out;
}

/** One `maxItems:` under one property of one schema — the mention-list cap. */
export function maxItems(schemaName: string, property: string): number {
  const props = childBlock(schemaBlock(schemaName), "properties");
  const block = new Map(entriesOf(props)).get(property);
  if (block === undefined) {
    throw new Error(`oto test: \`${schemaName}.${property}\` is not in the OpenAPI document`);
  }
  const n = scalar(block, "maxItems");
  if (n === null) {
    throw new Error(`oto test: \`${schemaName}.${property}\` declares no maxItems`);
  }
  return n;
}

/** The lines of one named schema under `components: schemas:`, without its header. */
function schemaBlock(name: string): readonly string[] {
  const lines = openapi().split("\n");
  const start = lines.findIndex((l) => new RegExp(String.raw`^(\s+)${name}:\s*$`).test(l));
  if (start < 0) throw new Error(`oto test: no schema named \`${name}\` in the OpenAPI document`);
  const indent = indentOf(lines[start] ?? "");

  const out: string[] = [];
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i] ?? "";
    if (line.trim() === "") {
      out.push(line);
      continue;
    }
    if (indentOf(line) <= indent) break;
    out.push(line);
  }
  return out;
}

/** The lines under one key inside a block, without its header. */
function childBlock(block: readonly string[], key: string): readonly string[] {
  const start = block.findIndex((l) => new RegExp(String.raw`^(\s+)${key}:\s*$`).test(l));
  if (start < 0) throw new Error(`oto test: no \`${key}:\` inside the block`);
  const indent = indentOf(block[start] ?? "");
  const out: string[] = [];
  for (let i = start + 1; i < block.length; i += 1) {
    const line = block[i] ?? "";
    if (line.trim() === "") continue;
    if (indentOf(line) <= indent) break;
    out.push(line);
  }
  return out;
}

/** `name: → its own sub-block`, for every immediate child of a block. */
function entriesOf(block: readonly string[]): readonly (readonly [string, readonly string[]])[] {
  const own = Math.min(...block.filter((l) => l.trim() !== "").map(indentOf));
  const out: [string, string[]][] = [];
  for (const line of block) {
    if (line.trim() === "") continue;
    const m = /^\s*([A-Za-z_][A-Za-z0-9_]*):\s*$/.exec(line);
    if (indentOf(line) === own && m?.[1] !== undefined) {
      out.push([m[1], []]);
      continue;
    }
    if (indentOf(line) > own) out.at(-1)?.[1].push(line);
  }
  return out;
}

function scalar(block: readonly string[], key: string): number | null {
  for (const line of block) {
    const m = new RegExp(String.raw`^\s*${key}:\s*(-?\d+)\s*$`).exec(line);
    if (m?.[1] !== undefined) return Number.parseInt(m[1], 10);
  }
  return null;
}

function indentOf(line: string): number {
  return line.length - line.trimStart().length;
}
