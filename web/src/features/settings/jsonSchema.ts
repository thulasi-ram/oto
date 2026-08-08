/**
 * A small, honest reader for the subset of JSON Schema the provider configs use.
 *
 * The contract serves each provider's schema (draft 2020-12) **verbatim**, and
 * promises those are the same bytes the server validates against. That makes it
 * the single source of truth for the channel form: adding a provider needs a
 * schema file and no UI code at all.
 *
 * The discipline that keeps that promise true is **never guessing**. Anything
 * this reader does not understand is reported as unsupported and rendered as a
 * raw JSON escape hatch rather than approximated by a text box — an approximated
 * control produces a config that passes the form and fails the server, which is
 * exactly the drift the whole arrangement exists to prevent.
 */

export type JsonValue = string | number | boolean | null | JsonValue[] | { [k: string]: JsonValue };

export interface RawSchema {
  readonly [key: string]: unknown;
}

export type FieldKind =
  | "string"
  | "text"
  | "enum"
  | "number"
  | "integer"
  | "boolean"
  | "stringArray"
  | "stringMap"
  | "unsupported";

export interface SchemaField {
  /** The property name, and the JSON Pointer segment the server reports on. */
  readonly name: string;
  readonly kind: FieldKind;
  readonly title: string;
  readonly description: string | null;
  readonly required: boolean;
  readonly default: JsonValue | undefined;
  readonly enumValues: readonly string[];
  readonly minimum: number | null;
  readonly maximum: number | null;
  readonly minLength: number | null;
  readonly maxLength: number | null;
  readonly pattern: string | null;
  readonly format: string | null;
  /** Present for `unsupported`, so the escape hatch can explain itself. */
  readonly reason: string | null;
}

function str(v: unknown): string | null {
  return typeof v === "string" ? v : null;
}

function num(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function obj(v: unknown): Record<string, unknown> | null {
  return typeof v === "object" && v !== null && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : null;
}

/** `type` may be a string or an array (`["string","null"]`); take the first non-null. */
function typeOf(schema: Record<string, unknown>): string | null {
  const t = schema["type"];
  if (typeof t === "string") return t;
  if (Array.isArray(t)) {
    const first = t.find((x) => typeof x === "string" && x !== "null");
    return typeof first === "string" ? first : null;
  }
  return null;
}

function humanise(name: string): string {
  return name
    .replace(/[_-]+/g, " ")
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .replace(/^./, (c) => c.toUpperCase());
}

function classify(schema: Record<string, unknown>): { kind: FieldKind; reason: string | null } {
  const enumValues = schema["enum"];
  if (Array.isArray(enumValues) && enumValues.every((x) => typeof x === "string")) {
    return { kind: "enum", reason: null };
  }

  switch (typeOf(schema)) {
    case "string": {
      const maxLength = num(schema["maxLength"]);
      // A long free-text field wants a textarea. A `format` or a `pattern` says
      // the value is a single structured token — a URL is 2 kB-capable and
      // still belongs on one line — so those stay as inputs regardless.
      const structured = schema["format"] !== undefined || schema["pattern"] !== undefined;
      const multiline = !structured && maxLength !== null && maxLength > 512;
      return { kind: multiline ? "text" : "string", reason: null };
    }
    case "integer":
      return { kind: "integer", reason: null };
    case "number":
      return { kind: "number", reason: null };
    case "boolean":
      return { kind: "boolean", reason: null };
    case "array": {
      const items = obj(schema["items"]);
      if (items !== null && typeOf(items) === "string") return { kind: "stringArray", reason: null };
      return { kind: "unsupported", reason: "an array of non-string items" };
    }
    case "object": {
      const additional = obj(schema["additionalProperties"]);
      if (additional !== null && typeOf(additional) === "string") {
        return { kind: "stringMap", reason: null };
      }
      if (obj(schema["properties"]) !== null) {
        return { kind: "unsupported", reason: "a nested object" };
      }
      return { kind: "unsupported", reason: "an object without a declared shape" };
    }
    default:
      if (schema["oneOf"] !== undefined || schema["anyOf"] !== undefined) {
        return { kind: "unsupported", reason: "a oneOf/anyOf union" };
      }
      if (schema["$ref"] !== undefined) {
        return { kind: "unsupported", reason: "a $ref this form does not resolve" };
      }
      return { kind: "unsupported", reason: "an untyped schema" };
  }
}

/** Flatten a provider's top-level object schema into an ordered field list. */
export function readFields(schema: RawSchema | null | undefined): readonly SchemaField[] {
  const root = obj(schema);
  if (root === null) return [];
  const properties = obj(root["properties"]);
  if (properties === null) return [];

  const requiredList = Array.isArray(root["required"])
    ? new Set(root["required"].filter((x): x is string => typeof x === "string"))
    : new Set<string>();

  const out: SchemaField[] = [];
  for (const [name, rawProp] of Object.entries(properties)) {
    const prop = obj(rawProp);
    if (prop === null) continue;
    const { kind, reason } = classify(prop);
    const enumValues = Array.isArray(prop["enum"])
      ? prop["enum"].filter((x): x is string => typeof x === "string")
      : [];

    out.push({
      name,
      kind,
      title: str(prop["title"]) ?? humanise(name),
      description: str(prop["description"]),
      required: requiredList.has(name),
      default: (prop["default"] as JsonValue | undefined) ?? undefined,
      enumValues,
      minimum: num(prop["minimum"]),
      maximum: num(prop["maximum"]),
      minLength: num(prop["minLength"]),
      maxLength: num(prop["maxLength"]),
      pattern: str(prop["pattern"]),
      format: str(prop["format"]),
      reason,
    });
  }

  // Required first, then declaration order. A form that asks for the mandatory
  // things first is faster to fill and harder to submit half-done.
  return out.sort((a, b) => Number(b.required) - Number(a.required));
}

/** The starting value for a field: its schema default, else an empty of its kind. */
export function initialValue(field: SchemaField): JsonValue {
  if (field.default !== undefined) return field.default;
  switch (field.kind) {
    case "boolean":
      return false;
    case "integer":
    case "number":
      return field.minimum ?? 0;
    case "stringArray":
      return [];
    case "stringMap":
      return {};
    case "enum":
      return field.enumValues[0] ?? "";
    default:
      return "";
  }
}

export function initialConfig(fields: readonly SchemaField[]): Record<string, JsonValue> {
  const out: Record<string, JsonValue> = {};
  for (const field of fields) out[field.name] = initialValue(field);
  return out;
}

/* -------------------------------------------------------------------------- */
/* Local pre-validation                                                       */
/* -------------------------------------------------------------------------- */

/**
 * A local check that mirrors the schema's own constraints.
 *
 * It is a **courtesy, not an authority**: the server validates against these
 * same bytes and its `violations[]` always wins. This exists so an obviously
 * wrong value is caught before a round trip, not so the UI can claim a config
 * is valid.
 */
export function validateField(field: SchemaField, value: JsonValue): string | undefined {
  const empty =
    value === "" ||
    value === null ||
    value === undefined ||
    (Array.isArray(value) && value.length === 0);

  if (field.required && empty) return `${field.title} is required.`;
  if (empty) return undefined;

  if (typeof value === "string") {
    if (field.minLength !== null && value.length < field.minLength) {
      return `At least ${field.minLength} characters.`;
    }
    if (field.maxLength !== null && value.length > field.maxLength) {
      return `At most ${field.maxLength} characters.`;
    }
    if (field.pattern !== null) {
      try {
        if (!new RegExp(field.pattern).test(value)) {
          return `Does not match the required pattern ${field.pattern}.`;
        }
      } catch {
        // An unsupported regex flavour is the schema's problem, not the user's.
        // Let the server decide rather than blocking a possibly-valid value.
      }
    }
    if (field.format === "uri" && !/^https?:\/\/\S+$/i.test(value)) {
      return "Must be an absolute http or https URL.";
    }
    if (field.kind === "enum" && !field.enumValues.includes(value)) {
      return `Must be one of: ${field.enumValues.join(", ")}.`;
    }
  }

  if (typeof value === "number") {
    if (field.minimum !== null && value < field.minimum) return `At least ${field.minimum}.`;
    if (field.maximum !== null && value > field.maximum) return `At most ${field.maximum}.`;
    if (field.kind === "integer" && !Number.isInteger(value)) return "Must be a whole number.";
  }

  return undefined;
}

export function validateConfig(
  fields: readonly SchemaField[],
  config: Readonly<Record<string, JsonValue>>,
): ReadonlyMap<string, string> {
  const out = new Map<string, string>();
  for (const field of fields) {
    const message = validateField(field, config[field.name] ?? "");
    if (message !== undefined) out.set(field.name, message);
  }
  return out;
}

/**
 * Strip the values the schema would reject as empty optionals.
 *
 * Sending `""` for an omitted optional string is not the same as omitting it,
 * and providers that check `minLength` on an optional field would reject the
 * former. The form omits rather than blanks.
 */
export function cleanConfig(
  fields: readonly SchemaField[],
  config: Readonly<Record<string, JsonValue>>,
): Record<string, JsonValue> {
  const out: Record<string, JsonValue> = {};
  for (const field of fields) {
    const value = config[field.name];
    if (value === undefined) continue;
    if (!field.required && (value === "" || (Array.isArray(value) && value.length === 0))) continue;
    out[field.name] = value;
  }
  return out;
}
