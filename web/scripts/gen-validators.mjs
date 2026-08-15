#!/usr/bin/env node
/**
 * Gate G4 of SPEC §L.8.1 — OpenAPI → valibot.
 *
 * Reads `api/openapi/openapi.yaml`, walks `components.schemas`, and emits one
 * exported valibot schema constant per component schema into
 * `web/src/api/generated/validators.ts`. The output is CHECKED IN; CI regenerates
 * it and fails on any diff (`npm run gen:validators:check`), exactly as gate G3
 * does for `schema.d.ts`.
 *
 * Why a generator at all: SPEC §L.8 forbids hand-written valibot schemas for API
 * responses, because a hand-written one is a second source of truth that drifts
 * silently. Forms stay hand-written, but each form schema must `v.pipe` into the
 * generated `*Request` schema, so a form can never accept something the API
 * would reject.
 *
 * ── The response/request split ───────────────────────────────────────────────
 * SPEC §L.8: "Response schemas use `v.looseObject`, form schemas use
 * `v.strictObject`. Additive server changes must never break a deployed UI; a
 * typo in a form must never reach the server."
 *
 * THE RULE, stated plainly: **a component schema whose name ends in `Request` is
 * a request and emits `v.strictObject`; every other schema is a response and
 * emits `v.looseObject`.** The mode is decided once at the top-level component
 * and inherited by every *inline* sub-object of that component. It is NOT
 * propagated through `$ref`: a `$ref` always resolves to that component's own
 * constant, which has that component's own mode.
 *
 * Two consequences of the rule, deliberate and recorded here so they are not
 * mistaken for bugs:
 *
 *  1. `additionalProperties: false` on a *response* schema is ignored. The
 *     contract says a great many response envelopes are closed; SPEC says a
 *     deployed UI must not break when the server adds a field. SPEC wins.
 *  2. A schema shared between a request and a response — `MatcherDTO`,
 *     `ThrottleDTO`, `CredentialInput`, `LabelMap` — is loose, because its name
 *     does not end in `Request`. A typo *inside* a matcher therefore is not
 *     caught by the generated request schema. Splitting them would mean two
 *     constants per shared schema and a name the contract does not have.
 *
 * ── Determinism ──────────────────────────────────────────────────────────────
 * Same input, byte-identical output. Property order comes from the YAML document
 * order; constants are emitted in a dependency-first topological order derived by
 * DFS in declaration order; pipe actions are emitted in a fixed keyword order.
 * Nothing is sorted by hash, and nothing depends on filesystem or clock.
 */

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import YAML from "yaml";

const HERE = dirname(fileURLToPath(import.meta.url));
const SPEC_PATH = resolve(HERE, "../../api/openapi/openapi.yaml");
const DEFAULT_OUT = resolve(HERE, "../src/api/generated/validators.ts");

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

/** Every place we could not express the contract, so the gap is visible. */
const unexpressed = [];

/** Records a gap and returns the comment text (no `//`) for the caller to place. */
function note(path, what) {
  unexpressed.push({ path, what });
  return `UNEXPRESSED: ${what} — ${path}`;
}

/** An expression that stands in for something the contract states and valibot cannot. */
function unknownWith(path, what) {
  return `// ${note(path, what)}\nv.unknown()`;
}

function lit(value) {
  return JSON.stringify(value);
}

/** `type` may be a string or a list; always work with a list. */
function typeList(node) {
  if (node.type === undefined) return [];
  return Array.isArray(node.type) ? node.type : [node.type];
}

function refName(node) {
  return typeof node.$ref === "string" ? node.$ref.split("/").pop() : null;
}

/**
 * A JS regex literal for an OpenAPI `pattern`.
 *
 * Returns null when the pattern is not a JS-compilable regex — an RE2 construct
 * JS does not have, say — so the caller can record the gap instead of emitting
 * something that throws at import time.
 */
function regexLiteral(pattern) {
  try {
    new RegExp(pattern);
  } catch {
    return null;
  }
  // Escape only unescaped forward slashes, so the literal terminates correctly.
  let out = "";
  for (let i = 0; i < pattern.length; i++) {
    const ch = pattern[i];
    if (ch === "\\") {
      out += ch + (pattern[i + 1] ?? "");
      i++;
      continue;
    }
    out += ch === "/" ? "\\/" : ch;
  }
  return `/${out}/`;
}

function indentBlock(text, pad) {
  return text
    .split("\n")
    .map((line) => (line === "" ? line : pad + line))
    .join("\n");
}

/**
 * `v.pipe(base, a, b)` — or just `base` when there is nothing to add.
 *
 * `notes` are rendered as trailing `// UNEXPRESSED:` comment lines inside the
 * call, so a constraint the contract states but valibot cannot enforce is
 * visible at exactly the schema it belongs to.
 */
function pipe(base, actions, notes = []) {
  if (actions.length === 0 && notes.length === 0) return base;
  const body = [base, ...actions].map((part) => `${part},`).join("\n");
  const inner = notes.length === 0 ? body : `${body}\n${notes.map((n) => `// ${n}`).join("\n")}`;
  return `v.pipe(\n${indentBlock(inner, "  ")}\n)`;
}

function callList(fn, parts) {
  if (parts.length === 0) return `${fn}([])`;
  return `${fn}([\n${indentBlock(parts.join(",\n"), "  ")},\n])`;
}

/* -------------------------------------------------------------------------- */
/* The contract                                                               */
/* -------------------------------------------------------------------------- */

const doc = YAML.parse(readFileSync(SPEC_PATH, "utf8"));
const schemas = doc?.components?.schemas;
if (!schemas || typeof schemas !== "object") {
  throw new Error(`no components.schemas in ${SPEC_PATH}`);
}
const names = Object.keys(schemas);

/** `SourceDTO` → `SourceDTOSchema`. SPEC's own example is `CreatePolicyRequestSchema`. */
function constName(schemaName) {
  return `${schemaName}Schema`;
}

/** Direct `$ref` targets of a node, in document order, deduplicated. */
function directRefs(node, out = []) {
  if (!node || typeof node !== "object") return out;
  if (Array.isArray(node)) {
    for (const item of node) directRefs(item, out);
    return out;
  }
  const target = refName(node);
  if (target !== null) {
    if (!out.includes(target)) out.push(target);
    return out;
  }
  for (const value of Object.values(node)) {
    if (value && typeof value === "object") directRefs(value, out);
  }
  return out;
}

const deps = new Map(names.map((n) => [n, directRefs(schemas[n]).filter((d) => names.includes(d))]));

/**
 * Dependency-first order, plus the set of edges that close a cycle.
 *
 * With no cycles every reference can be a plain identifier, which typechecks
 * without annotation. A genuine cycle gets `v.lazy(() => XSchema)` on the
 * back-edge and an explicit `v.GenericSchema` annotation on the constants
 * involved, because TypeScript cannot infer a circular type.
 */
const order = [];
const backEdges = new Set(); // "From→To"
const cyclic = new Set();
{
  const state = new Map(); // undefined | "open" | "done"
  const visit = (name, stack) => {
    state.set(name, "open");
    stack.push(name);
    for (const dep of deps.get(name) ?? []) {
      const s = state.get(dep);
      if (s === "open") {
        backEdges.add(`${name}→${dep}`);
        for (const member of stack.slice(stack.indexOf(dep))) cyclic.add(member);
      } else if (s === undefined) {
        visit(dep, stack);
      }
    }
    stack.pop();
    state.set(name, "done");
    order.push(name);
  };
  for (const name of names) if (state.get(name) === undefined) visit(name, []);
}

/* -------------------------------------------------------------------------- */
/* Schema → valibot expression                                                */
/* -------------------------------------------------------------------------- */

/** Does this node ultimately describe a scalar? Used to police `uniqueItems`. */
function isScalar(node, seen = new Set()) {
  if (!node || typeof node !== "object") return false;
  const target = refName(node);
  if (target !== null) {
    if (seen.has(target) || !names.includes(target)) return false;
    seen.add(target);
    return isScalar(schemas[target], seen);
  }
  const types = typeList(node).filter((t) => t !== "null");
  if (types.length === 0) return Array.isArray(node.enum);
  return types.every((t) => t === "string" || t === "integer" || t === "number" || t === "boolean");
}

/**
 * Build the valibot expression for one schema node.
 *
 * `ctx.owner` is the top-level component being emitted (for `// UNEXPRESSED:`
 * paths and for back-edge detection), `ctx.strict` is the request/response mode
 * described at the top of this file.
 */
function build(node, ctx, path) {
  if (node === true) return "v.unknown()";
  if (node === false || node === null || typeof node !== "object") {
    return unknownWith(path, `schema is the literal ${lit(node)}`);
  }

  const target = refName(node);
  if (target !== null) {
    if (!names.includes(target)) {
      return unknownWith(path, `$ref outside components.schemas: ${node.$ref}`);
    }
    return backEdges.has(`${ctx.owner}→${target}`)
      ? `v.lazy(() => ${constName(target)})`
      : constName(target);
  }

  // ---- composition ------------------------------------------------------
  if (Array.isArray(node.allOf)) {
    const parts = node.allOf.map((branch, i) => build(branch, ctx, `${path}/allOf/${i}`));
    if (node.properties !== undefined || node.additionalProperties !== undefined) {
      parts.push(buildObject(node, ctx, path));
    }
    return parts.length === 1 ? parts[0] : callList("v.intersect", parts);
  }

  const anyKeyword = Array.isArray(node.oneOf) ? "oneOf" : Array.isArray(node.anyOf) ? "anyOf" : null;
  if (anyKeyword !== null) {
    const branches = node[anyKeyword];
    const nullable = branches.some((b) => b && typeof b === "object" && typeList(b).includes("null"));
    const real = branches.filter((b) => !(b && typeof b === "object" && typeList(b).includes("null")));
    const parts = real.map((b, i) => build(b, ctx, `${path}/${anyKeyword}/${i}`));
    // `discriminator` is not modelled separately: `v.union` accepts exactly the
    // same set of values, it just reports every branch's issues on failure.
    let expr = parts.length === 0 ? "v.null()" : parts.length === 1 ? parts[0] : callList("v.union", parts);
    if (parts.length > 0 && nullable) expr = `v.nullable(${expr})`;
    return expr;
  }

  const types = typeList(node);
  const nullable = types.includes("null") || (Array.isArray(node.enum) && node.enum.includes(null));
  const concrete = types.filter((t) => t !== "null");

  if (Array.isArray(node.enum)) {
    const values = node.enum.filter((value) => value !== null);
    if (values.length === 0) return "v.null()";
    const picklist = `v.picklist([${values.map(lit).join(", ")}])`;
    return nullable ? `v.nullable(${picklist})` : picklist;
  }
  if (concrete.length === 0) return nullable ? "v.null()" : "v.unknown()";

  let expr;
  if (concrete.length > 1) {
    expr = unknownWith(path, `a union of primitive types ${lit(concrete)}`);
  } else {
    switch (concrete[0]) {
      case "string":
        expr = buildString(node, path);
        break;
      case "integer":
      case "number":
        expr = buildNumber(node, concrete[0]);
        break;
      case "boolean":
        expr = "v.boolean()";
        break;
      case "array":
        expr = buildArray(node, ctx, path);
        break;
      case "object":
        expr = buildObject(node, ctx, path);
        break;
      default:
        expr = unknownWith(path, `an unknown type ${lit(concrete[0])}`);
    }
  }

  return nullable ? `v.nullable(${expr})` : expr;
}

function buildString(node, path) {
  const actions = [];
  const notes = [];
  switch (node.format) {
    case "uuid":
      actions.push("v.uuid()");
      break;
    case "date-time":
      actions.push("v.isoTimestamp()");
      break;
    case "uri":
      actions.push("v.url()");
      break;
    case "email":
      actions.push("v.email()");
      break;
    default:
      break;
  }
  if (typeof node.minLength === "number") actions.push(`v.minLength(${node.minLength})`);
  if (typeof node.maxLength === "number") actions.push(`v.maxLength(${node.maxLength})`);
  if (typeof node.pattern === "string") {
    const re = regexLiteral(node.pattern);
    if (re === null) notes.push(note(path, `pattern is not a JavaScript regex: ${node.pattern}`));
    else actions.push(`v.regex(${re})`);
  }
  return pipe("v.string()", actions, notes);
}

function buildNumber(node, kind) {
  const actions = [];
  if (kind === "integer") actions.push("v.integer()");
  if (typeof node.minimum === "number") actions.push(`v.minValue(${node.minimum})`);
  if (typeof node.maximum === "number") actions.push(`v.maxValue(${node.maximum})`);
  if (typeof node.exclusiveMinimum === "number") actions.push(`v.gtValue(${node.exclusiveMinimum})`);
  if (typeof node.exclusiveMaximum === "number") actions.push(`v.ltValue(${node.exclusiveMaximum})`);
  return pipe("v.number()", actions);
}

function buildArray(node, ctx, path) {
  const items =
    node.items === undefined ? "v.unknown()" : build(node.items, ctx, `${path}/items`);
  const actions = [];
  const notes = [];
  if (typeof node.minItems === "number") actions.push(`v.minLength(${node.minItems})`);
  if (typeof node.maxItems === "number") actions.push(`v.maxLength(${node.maxItems})`);
  if (node.uniqueItems === true) {
    if (node.items !== undefined && isScalar(node.items)) {
      actions.push(
        `v.check((items) => new Set(items).size === items.length, "must not contain duplicates")`,
      );
    } else {
      notes.push(note(path, "uniqueItems over non-scalar items"));
    }
  }
  return pipe(`v.array(${items})`, actions, notes);
}

function buildObject(node, ctx, path) {
  const properties = node.properties;
  const required = new Set(Array.isArray(node.required) ? node.required : []);

  // A map, not a record type: `additionalProperties` is a schema and there are
  // no fixed properties.
  if (properties === undefined) {
    const value =
      node.additionalProperties && typeof node.additionalProperties === "object"
        ? build(node.additionalProperties, ctx, `${path}/additionalProperties`)
        : "v.unknown()";
    const base =
      node.additionalProperties === false ? "v.strictObject({})" : `v.record(v.string(), ${value})`;
    return pipe(base, entryCountActions(node));
  }

  const lines = [];
  for (const [key, child] of Object.entries(properties)) {
    let expr = build(child, ctx, `${path}/properties/${key}`);
    // `v.exactOptional`, not `v.optional`: JSON has no `undefined`, an absent
    // property is absent, and the repo compiles with `exactOptionalPropertyTypes`
    // — so `key?: T` is the type that lines up with the G3 output in
    // `schema.d.ts`. `v.optional` would produce `key?: T | undefined` and every
    // form would need a cast to reach an endpoint.
    //
    // A property carrying a `default` is optional on the wire and present after
    // parsing, which is exactly what `v.exactOptional(schema, value)` means —
    // and it is the same call openapi-typescript makes when it types a defaulted
    // property as required, so G3 and G4 agree about the shape.
    if (!required.has(key)) {
      const fallback = defaultLiteral(child);
      expr = fallback === null ? `v.exactOptional(${expr})` : `v.exactOptional(${expr}, ${fallback})`;
    }
    lines.push(`${lit(key)}: ${expr},`);
  }

  const fn = ctx.strict ? "v.strictObject" : "v.looseObject";
  const base =
    lines.length === 0 ? `${fn}({})` : `${fn}({\n${indentBlock(lines.join("\n"), "  ")}\n})`;
  return pipe(base, entryCountActions(node));
}

/**
 * The `default` for a property, as a valibot fallback argument, or null when the
 * property declares none.
 *
 * Composite defaults are emitted as a getter so two parses never share one
 * mutable array.
 */
function defaultLiteral(node) {
  if (!node || typeof node !== "object" || Array.isArray(node)) return null;
  if (!Object.prototype.hasOwnProperty.call(node, "default")) return null;
  const value = node.default;
  return value !== null && typeof value === "object" ? `() => (${lit(value)})` : lit(value);
}

/** `minProperties` / `maxProperties` — "at least one field", "at most 64 labels". */
function entryCountActions(node) {
  const actions = [];
  if (typeof node.minProperties === "number") {
    actions.push(
      `v.check((value) => Object.keys(value).length >= ${node.minProperties}, "at least ${node.minProperties} propert${node.minProperties === 1 ? "y" : "ies"} required")`,
    );
  }
  if (typeof node.maxProperties === "number") {
    actions.push(
      `v.check((value) => Object.keys(value).length <= ${node.maxProperties}, "at most ${node.maxProperties} properties allowed")`,
    );
  }
  return actions;
}

/* -------------------------------------------------------------------------- */
/* Emit                                                                       */
/* -------------------------------------------------------------------------- */

const HEADER = `/**
 * GENERATED FILE — DO NOT EDIT BY HAND.
 *
 * Produced by \`npm run gen:validators\` (web/scripts/gen-validators.mjs) from
 * \`api/openapi/openapi.yaml\`. This file is checked in, and CI runs
 * \`npm run gen:validators:check\` and fails on any diff. That is **gate G4 of
 * SPEC §L.8.1**, and it is why no hand-written valibot schema may describe an
 * API response.
 *
 * Editing this file by hand will be reverted by the next generate, and will fail
 * CI in the meantime.
 *
 * Response schemas are \`v.looseObject\` and request schemas are
 * \`v.strictObject\` (SPEC §L.8): an additive server change must never break a
 * deployed UI, and a typo in a form must never reach the server. A schema whose
 * name ends in \`Request\` is a request; everything else is a response.
 *
 * Forms stay hand-written, and each one must \`v.pipe\` into the matching
 * \`*RequestSchema\` below as its final gate.
 */
/* eslint-disable */

import * as v from "valibot";
`;

const blocks = [];
for (const name of order) {
  const node = schemas[name];
  const ctx = { owner: name, strict: name.endsWith("Request") };
  const expr = build(node, ctx, `#/components/schemas/${name}`);
  const annotation = cyclic.has(name) ? ": v.GenericSchema" : "";
  blocks.push(`export const ${constName(name)}${annotation} = ${expr};`);
}

const outPath = process.argv[2] ? resolve(process.cwd(), process.argv[2]) : DEFAULT_OUT;
mkdirSync(dirname(outPath), { recursive: true });
writeFileSync(outPath, `${HEADER}\n${blocks.join("\n\n")}\n`, "utf8");

const rel = outPath.replace(`${resolve(HERE, "..")}/`, "");
process.stderr.write(
  `gen-validators: ${order.length} schemas -> ${rel}` +
    (unexpressed.length > 0 ? ` (${unexpressed.length} UNEXPRESSED)\n` : "\n"),
);
for (const gap of unexpressed) process.stderr.write(`  UNEXPRESSED ${gap.path}: ${gap.what}\n`);
