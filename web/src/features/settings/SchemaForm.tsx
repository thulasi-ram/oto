/**
 * The channel configuration form, rendered from the provider's own JSON Schema.
 *
 * There is no per-provider UI code anywhere in oto, and that is the point: the
 * server validates against these exact bytes, the form renders from these exact
 * bytes, and a new provider therefore needs a schema file and nothing else. Any
 * hand-written form here would be a second copy of the rules and would start
 * drifting on the day it was written.
 *
 * Server violations are mapped back by JSON Pointer — `config/conversation_id`
 * becomes `config.conversation_id` and lands on that exact control — which is
 * what makes "a `#channel-name` where a channel ID belongs" highlight the right
 * box instead of showing a form-level shrug.
 */
import { For, Show, type Component } from "solid-js";

import { Checkbox, Field, Input, Select, Textarea } from "~/components/ui/primitives";
import {
  validateField,
  type JsonValue,
  type SchemaField,
} from "./jsonSchema";

export interface SchemaFormProps {
  readonly fields: readonly SchemaField[];
  readonly value: Readonly<Record<string, JsonValue>>;
  readonly onChange: (name: string, next: JsonValue) => void;
  /** Server violations keyed by control name, e.g. `config.conversation_id`. */
  readonly violations: ReadonlyMap<string, string>;
  /** Prefix for control names and ids — `config` for a channel's config object. */
  readonly prefix: string;
  readonly showErrors: boolean;
}

export const SchemaForm: Component<SchemaFormProps> = (props) => (
  <div class="flex flex-col gap-3">
    <Show
      when={props.fields.length > 0}
      fallback={
        <p class="text-[12px] text-ink-muted">
          This provider declares no configuration. That is the schema's statement, not a loading
          state.
        </p>
      }
    >
      <For each={props.fields}>
        {(field) => (
          <SchemaFieldControl
            field={field}
            value={props.value[field.name] ?? ""}
            onChange={(next) => props.onChange(field.name, next)}
            error={
              props.violations.get(`${props.prefix}.${field.name}`) ??
              (props.showErrors
                ? validateField(field, props.value[field.name] ?? "")
                : undefined)
            }
            id={`${props.prefix}-${field.name}`}
          />
        )}
      </For>
    </Show>
  </div>
);

const SchemaFieldControl: Component<{
  readonly field: SchemaField;
  readonly value: JsonValue;
  readonly onChange: (next: JsonValue) => void;
  readonly error: string | undefined;
  readonly id: string;
}> = (props) => {
  const f = (): SchemaField => props.field;

  /** The bounds, said out loud. A schema constraint the user cannot see is a trap. */
  const hint = (): string | undefined => {
    const parts: string[] = [];
    if (f().description !== null) parts.push(f().description as string);
    if (f().minimum !== null || f().maximum !== null) {
      parts.push(
        `Between ${f().minimum ?? "any"} and ${f().maximum ?? "any"}.`,
      );
    }
    if (f().maxLength !== null && f().kind !== "text") {
      parts.push(`Up to ${f().maxLength} characters.`);
    }
    if (f().format !== null) parts.push(`Format: ${f().format}.`);
    return parts.length > 0 ? parts.join(" ") : undefined;
  };

  if (f().kind === "boolean") {
    return (
      <div class="flex flex-col gap-1">
        <Checkbox
          id={props.id}
          checked={props.value === true}
          onChange={(next) => props.onChange(next)}
          label={<span class="font-medium">{f().title}</span>}
        />
        <Show when={hint()}>
          <p class="pl-5 text-[11px] leading-snug text-ink-subtle">{hint()}</p>
        </Show>
        <Show when={props.error}>
          <p class="pl-5 text-[11px] font-medium text-ink" role="alert">
            {props.error}
          </p>
        </Show>
      </div>
    );
  }

  return (
    <Field
      id={props.id}
      label={f().title}
      required={f().required}
      hint={hint()}
      error={props.error}
    >
      {(a) => (
        <>
          <Show when={f().kind === "enum"}>
            <Select
              {...a}
              value={typeof props.value === "string" ? props.value : ""}
              onChange={(e) => props.onChange(e.currentTarget.value)}
            >
              <Show when={!f().required}>
                <option value="">— not set —</option>
              </Show>
              <For each={f().enumValues}>{(opt) => <option value={opt}>{opt}</option>}</For>
            </Select>
          </Show>

          <Show when={f().kind === "string"}>
            <Input
              {...a}
              mono={f().format === "uri" || f().pattern !== null}
              value={typeof props.value === "string" ? props.value : ""}
              maxLength={f().maxLength ?? undefined}
              onInput={(e) => props.onChange(e.currentTarget.value)}
            />
          </Show>

          <Show when={f().kind === "text"}>
            <Textarea
              {...a}
              value={typeof props.value === "string" ? props.value : ""}
              maxLength={f().maxLength ?? undefined}
              onInput={(e) => props.onChange(e.currentTarget.value)}
            />
          </Show>

          <Show when={f().kind === "integer" || f().kind === "number"}>
            <Input
              {...a}
              type="number"
              min={f().minimum ?? undefined}
              max={f().maximum ?? undefined}
              step={f().kind === "integer" ? 1 : "any"}
              value={typeof props.value === "number" ? String(props.value) : ""}
              onInput={(e) => {
                const parsed =
                  f().kind === "integer"
                    ? Number.parseInt(e.currentTarget.value, 10)
                    : Number.parseFloat(e.currentTarget.value);
                props.onChange(Number.isFinite(parsed) ? parsed : "");
              }}
            />
          </Show>

          {/* A list of strings, one per line. Simpler to paste into than a chip
              editor and impossible to get stuck in. */}
          <Show when={f().kind === "stringArray"}>
            <Textarea
              {...a}
              value={Array.isArray(props.value) ? props.value.join("\n") : ""}
              placeholder="One value per line"
              onInput={(e) =>
                props.onChange(
                  e.currentTarget.value
                    .split("\n")
                    .map((s) => s.trim())
                    .filter((s) => s !== ""),
                )
              }
            />
          </Show>

          {/* `key: value` per line — the shape webhook headers actually take. */}
          <Show when={f().kind === "stringMap"}>
            <Textarea
              {...a}
              value={
                typeof props.value === "object" && props.value !== null && !Array.isArray(props.value)
                  ? Object.entries(props.value)
                      .map(([k, val]) => `${k}: ${String(val)}`)
                      .join("\n")
                  : ""
              }
              placeholder={"X-Example: value\nOne per line"}
              onInput={(e) => {
                const map: Record<string, JsonValue> = {};
                for (const line of e.currentTarget.value.split("\n")) {
                  const idx = line.indexOf(":");
                  if (idx <= 0) continue;
                  const k = line.slice(0, idx).trim();
                  if (k !== "") map[k] = line.slice(idx + 1).trim();
                }
                props.onChange(map);
              }}
            />
          </Show>

          {/* The escape hatch. Rendering an unknown schema shape as a plain text
              box would produce a config that passes here and fails the server —
              so it says what it does not understand and takes raw JSON. */}
          <Show when={f().kind === "unsupported"}>
            <div class="flex flex-col gap-1">
              <p class="text-[11px] leading-snug text-ink-muted">
                This form does not render {f().reason ?? "this shape"}, so it will not pretend to.
                Enter the value as JSON — the server validates it against the same schema either way.
              </p>
              <Textarea
                {...a}
                class="font-mono"
                value={props.value === "" ? "" : JSON.stringify(props.value, null, 2)}
                onInput={(e) => {
                  const raw = e.currentTarget.value;
                  if (raw.trim() === "") {
                    props.onChange("");
                    return;
                  }
                  try {
                    props.onChange(JSON.parse(raw) as JsonValue);
                  } catch {
                    // Keep the text the user typed; the error surfaces on submit
                    // rather than deleting their work mid-keystroke.
                    props.onChange(raw);
                  }
                }}
              />
            </div>
          </Show>
        </>
      )}
    </Field>
  );
};
