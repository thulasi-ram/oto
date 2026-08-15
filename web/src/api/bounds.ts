/**
 * The contract's own rules, read off the generated schemas at runtime.
 *
 * ⛔ THIS FILE EXISTS SO THAT NO SCREEN EVER RE-TYPES A RULE THE SERVER OWNS —
 * not a bound, not a pattern, not a picklist.
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
 * The walk below reads valibot's own internals (`entries`, `wrapped`, `item`,
 * `pipe`, `requirement`, `options`), which is a private shape. That is
 * deliberate, and this file is the only walk over it — the request-side helpers
 * in `src/test/contract.ts` are re-exports of these accessors, not a second
 * copy — so a valibot upgrade that moves them breaks here loudly rather than
 * quietly making every bound vanish.
 */

/** As much of valibot's internal node shape as this file walks. */
interface Node {
  readonly type?: string;
  readonly entries?: Readonly<Record<string, unknown>>;
  readonly wrapped?: unknown;
  readonly item?: unknown;
  readonly options?: readonly unknown[];
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
        `renamed or dropped it, and the caller must not guess what replaced it.`,
    );
  }
  return unwrap(entry, `\`${key}\``);
}

/**
 * One property of a generated request schema, optionality peeled, as an opaque
 * value. Opaque on purpose: the internal node shape must not leak past this
 * file, so a caller that needs more than the accessors below casts to valibot's
 * public `GenericSchema` and uses valibot's public API from there.
 */
export function fieldOf(objectSchema: unknown, key: string): unknown {
  return field(objectSchema, key);
}

/**
 * What a rule is read off: one property of a request schema, or a whole schema.
 *
 * Some rules are stated by the contract as a named component rather than inline
 * on a request property — `LabelName` carries both its charset and its 1024-byte
 * cap, and `gen-validators.mjs` emits it as its own `LabelNameSchema`. Omitting
 * `key` asks that schema directly, so `~/lib/matchers` can read the same rule the
 * same way a form reads a request body's.
 */
function subject(schema: unknown, key: string | undefined): Node {
  return key === undefined ? unwrap(schema, "the schema") : field(schema, key);
}

/** How a subject is named in a throw, whether it is a property or a whole schema. */
function named(key: string | undefined): string {
  return key === undefined ? "the schema" : `\`${key}\``;
}

/**
 * One valibot action's requirement, as a number.
 *
 * The throw is the point. A missing bound here means the contract or the
 * generator lost it, and the honest response is to say so rather than to render
 * an input the server will reject.
 */
function requirement(schema: unknown, key: string | undefined, action: string): number {
  for (const step of subject(schema, key).pipe ?? []) {
    if (step.type === action && typeof step.requirement === "number") return step.requirement;
  }
  throw new Error(
    `oto: the generated contract declares no \`${action}\` on ${named(key)}. Either the ` +
      `bound was removed from api/openapi/openapi.yaml, or gen-validators.mjs dropped ` +
      `it on the way through — check the YAML before assuming the screen is wrong.`,
  );
}

/** The `minimum` the contract puts on one numeric request property. */
export function minValueOf(objectSchema: unknown, key?: string): number {
  return requirement(objectSchema, key, "min_value");
}

/** The `maximum` the contract puts on one numeric request property. */
export function maxValueOf(objectSchema: unknown, key?: string): number {
  return requirement(objectSchema, key, "max_value");
}

/** The `minLength` / `minItems` the contract puts on one request property. */
export function minLengthOf(objectSchema: unknown, key?: string): number {
  return requirement(objectSchema, key, "min_length");
}

/** The `maxLength` / `maxItems` the contract puts on one request property. */
export function maxLengthOf(objectSchema: unknown, key?: string): number {
  return requirement(objectSchema, key, "max_length");
}

/**
 * The `pattern` the contract puts on one string property — **the object itself**,
 * not a re-typing of its source.
 *
 * ⛔ A HAND-COPIED REGEX IS WORSE THAN A HAND-COPIED NUMBER, because it can be
 * wrong in ways that read as right. `SourcesSection.tsx` spelled the base-URL rule
 * `/^https?:\/\/[^\s]+$/i` plus a separate trailing-slash check: two differences
 * from the contract's single `/^https?:\/\/[^\s]+[^\/]$/`, one of them a *flag*
 * that let `HTTP://…` through the form and into a 422. Returning the generated
 * `RegExp` means the flags travel with the pattern and there is no second copy for
 * a flag to be added to.
 */
export function patternOf(objectSchema: unknown, key?: string): RegExp {
  for (const step of subject(objectSchema, key).pipe ?? []) {
    if (step.type === "regex" && step.requirement instanceof RegExp) return step.requirement;
  }
  throw new Error(
    `oto: the generated contract declares no \`regex\` on ${named(key)}. Either the ` +
      `pattern was removed from api/openapi/openapi.yaml, or gen-validators.mjs dropped ` +
      `it on the way through — check the YAML before assuming the screen is wrong.`,
  );
}

/**
 * The picklist a property resolves to, as a type, with optionality peeled off.
 *
 * This is what makes {@link enumValuesOf} more than a runtime read: the members
 * come through as a union, so a screen that maps them into a `Record<Enum, …>`
 * label table stops compiling the day the contract grows one.
 */
type Options<E> = E extends { readonly options: readonly (infer T)[] }
  ? readonly T[]
  : E extends { readonly wrapped: infer W }
    ? Options<W>
    : E extends { readonly item: infer I }
      ? Options<I>
      : never;

/**
 * The picklist a subject resolves to, at runtime, untyped.
 *
 * `reasons: v.array(v.picklist([...]))` and `verbosity: v.picklist([...])` are
 * the same fact to a caller asking "what may the user choose", so an array is
 * answered with its item's picklist.
 */
export function optionsOf(objectSchema: unknown, key?: string): readonly unknown[] {
  const target = subject(objectSchema, key);
  const peeled = target.type === "array" ? unwrap(target.item, `${named(key)} items`) : target;
  if (peeled.options === undefined) {
    throw new Error(
      `oto: ${named(key)} is not a picklist in the generated contract. Either the contract ` +
        `widened it to a free-form string, or gen-validators.mjs stopped emitting the ` +
        `enum — and a control must not offer a list nobody guarantees.`,
    );
  }
  return peeled.options;
}

/**
 * Every value the contract admits for one property, in the contract's own order.
 *
 * A top-level enum needs nothing from this file — `SourceKindSchema.options` is
 * already the contract's list, and that is the spelling to prefer. This exists for
 * the enums the generator only emits *inline* on a property (`AlertRollupDTO`'s
 * `group_by` is one), which otherwise have no name a screen could import and so
 * get re-typed by hand.
 */
export function enumValuesOf<
  S extends { readonly entries: object },
  K extends keyof S["entries"] & string,
>(objectSchema: S, key: K): Options<S["entries"][K]> {
  return optionsOf(objectSchema, key) as unknown as Options<S["entries"][K]>;
}

/** Both ends of a numeric property, for the common `min={} max={}` pair. */
export interface Range {
  readonly min: number;
  readonly max: number;
}

export function rangeOf(objectSchema: unknown, key: string): Range {
  return { min: minValueOf(objectSchema, key), max: maxValueOf(objectSchema, key) };
}
