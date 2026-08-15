/**
 * The one piece of valibot glue every alert dialog's local validation kept
 * re-deriving: turn a `safeParse` result into the single message a control
 * shows, and gate that behind "has this field even been touched yet".
 *
 * This is local (client-side) validation only — it exists to catch an
 * obviously bad value before a request leaves the browser. The server's
 * `violations[]` stays authoritative; see SnoozeDialog.tsx and Actions.tsx for
 * why the two are not redundant.
 */
import { createMemo, type Accessor } from "solid-js";
import * as v from "valibot";

/** The first message out of a `safeParse` result, or `undefined` if it passed. */
export function firstIssue(result: v.SafeParseResult<v.GenericSchema>): string | undefined {
  return result.success ? undefined : result.issues[0]?.message;
}

/**
 * A field's error, live: `undefined` until `active` says the field should be
 * judged — typically "touched, and (for a multi-mode form) the active
 * branch" — then the first valibot message `schema` has for `value`.
 */
export function createFieldError<T extends v.GenericSchema>(
  schema: T,
  value: Accessor<v.InferInput<T>>,
  active: Accessor<boolean>,
): Accessor<string | undefined> {
  return createMemo(() => (active() ? firstIssue(v.safeParse(schema, value())) : undefined));
}
