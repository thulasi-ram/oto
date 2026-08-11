/**
 * The contract's own bounds, read off the generated schemas at runtime.
 *
 * ⛔ THIS FILE EXISTS SO THAT NO SCREEN EVER RE-TYPES A NUMBER THE SERVER OWNS.
 * `src/api/generated/validators.ts` is produced from `api/openapi/openapi.yaml`
 * by gate G4 and every form already `v.pipe`s into it — so the bound the server
 * will reject with is a *value sitting in memory*, and a form that writes `120`
 * into a `maxLength` attribute is choosing to hold a second, unmaintained copy
 * of it. `PoliciesSection.tsx` is the reason: a hand-copied enum there produced
 * a 422 nobody could explain, and the fix is not "copy more carefully".
 *
 * `TuningSection.tsx` is the shape being generalised here. It reads every bound
 * off the payload the server sent and hardcodes nothing; the settings screens
 * whose bounds are *not* served per-request read them from here instead.
 *
 * ⚠️ DERIVED IS ONLY AS GOOD AS THE GENERATOR. If `openapi.yaml` states a bound
 * that `npm run gen:validators` does not emit, this file cannot invent it —
 * which is why every accessor **throws with the name it was looking for**
 * instead of returning `undefined`. A bound the generator dropped must fail
 * loudly at import, where somebody will see it, rather than silently unbounding
 * a control.
 *
 * The walk below reads valibot's own internals (`entries`, `wrapped`, `pipe`,
 * `requirement`), which is a private shape. That is deliberate and it is the
 * same walk `src/test/contract.ts` does: a valibot upgrade that moves them
 * breaks here loudly rather than quietly making every bound vanish.
 */

/** As much of valibot's internal node shape as this file walks. */
interface Node {
  readonly type?: string;
  readonly entries?: Readonly<Record<string, unknown>>;
  readonly wrapped?: unknown;
  readonly pipe?: readonly { readonly type?: string; readonly requirement?: unknown }[];
}

function node(schema: unknown, what: string): Node {
  if (typeof schema !== "object" || schema === null) {
    throw new Error(`oto: ${what} is not a valibot schema`);
  }
  return schema as Node;
}

/** Peel `optional` / `exact_optional` / `nullable` / `nullish` off a property. */
function unwrap(schema: unknown, what: string): Node {
  let current = node(schema, what);
  for (let depth = 0; depth < 8 && current.wrapped !== undefined; depth += 1) {
    current = node(current.wrapped, what);
  }
  return current;
}

/** One property of a generated request schema, with its optionality peeled off. */
function field(objectSchema: unknown, key: string): Node {
  const entry = node(objectSchema, `the schema holding \`${key}\``).entries?.[key];
  if (entry === undefined) {
    throw new Error(
      `oto: the generated request schema has no \`${key}\` property. The contract ` +
        `renamed or dropped it, and a screen must not guess what replaced it.`,
    );
  }
  return unwrap(entry, `\`${key}\``);
}

/**
 * One valibot action's requirement, as a number.
 *
 * The throw is the point. A missing bound here means the contract or the
 * generator lost it, and the honest response is to say so rather than to render
 * an input the server will reject.
 */
function requirement(objectSchema: unknown, key: string, action: string): number {
  for (const step of field(objectSchema, key).pipe ?? []) {
    if (step.type === action && typeof step.requirement === "number") return step.requirement;
  }
  throw new Error(
    `oto: the generated contract declares no \`${action}\` on \`${key}\`. Either the ` +
      `bound was removed from api/openapi/openapi.yaml, or gen-validators.mjs dropped ` +
      `it on the way through — check the YAML before assuming the screen is wrong.`,
  );
}

/** The `minimum` the contract puts on one numeric request property. */
export function minValueOf(objectSchema: unknown, key: string): number {
  return requirement(objectSchema, key, "min_value");
}

/** The `maximum` the contract puts on one numeric request property. */
export function maxValueOf(objectSchema: unknown, key: string): number {
  return requirement(objectSchema, key, "max_value");
}

/** The `minLength` / `minItems` the contract puts on one request property. */
export function minLengthOf(objectSchema: unknown, key: string): number {
  return requirement(objectSchema, key, "min_length");
}

/** The `maxLength` / `maxItems` the contract puts on one request property. */
export function maxLengthOf(objectSchema: unknown, key: string): number {
  return requirement(objectSchema, key, "max_length");
}

/** Both ends of a numeric property, for the common `min={} max={}` pair. */
export interface Range {
  readonly min: number;
  readonly max: number;
}

export function rangeOf(objectSchema: unknown, key: string): Range {
  return { min: minValueOf(objectSchema, key), max: maxValueOf(objectSchema, key) };
}
