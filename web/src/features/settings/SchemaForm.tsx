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

import { Checkbox } from "~/components/ui/Checkbox";
import {
  Select,
  SelectContent,
  SelectDescription,
  SelectErrorMessage,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
  TextFieldTextArea,
} from "~/components/ui/TextField";
import { cn } from "~/lib/cn";
import {
  validateField,
  type JsonValue,
  type SchemaField,
} from "./jsonSchema";
import { CHECK_LABEL, CHECK_ROW, FIELD, FORM, HELP } from "./rhythm";

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
  <div class={FORM}>
    <Show
      when={props.fields.length > 0}
      fallback={
        <p class={HELP}>
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

  /** The required-field asterisk, exactly as `primitives.tsx`'s `Field` rendered it. */
  const requiredMark = () =>
    f().required ? (
      <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
        *
      </span>
    ) : null;

  if (f().kind === "boolean") {
    return (
      <div class={FIELD}>
        {/* `gap-xs` is 6px, so the 14px box plus the gap puts the label's left
            edge at exactly the 20px the help text below indents to. */}
        <div class={CHECK_ROW}>
          <Checkbox
            id={props.id}
            checked={props.value === true}
            onChange={(next) => props.onChange(next)}
          />
          <label for={`${props.id}-input`} class={CHECK_LABEL}>
            <span class="font-medium">{f().title}</span>
          </label>
        </div>
        <Show when={hint()}>
          <p class={cn("pl-5", HELP)}>{hint()}</p>
        </Show>
        <Show when={props.error}>
          <p class="pl-5 text-meta font-medium text-ink" role="alert">
            {props.error}
          </p>
        </Show>
      </div>
    );
  }

  if (f().kind === "enum") {
    const options = (): string[] =>
      f().required ? [...f().enumValues] : ["", ...f().enumValues];

    return (
      <Select
        id={props.id}
        class={FIELD}
        options={options()}
        value={typeof props.value === "string" ? props.value : ""}
        onChange={(next) => props.onChange(next ?? "")}
        validationState={props.error ? "invalid" : "valid"}
        required={f().required}
        placeholder="— not set —"
        itemComponent={(item) => (
          <SelectItem item={item.item}>
            {item.item.rawValue === "" ? "— not set —" : item.item.rawValue}
          </SelectItem>
        )}
      >
        <SelectLabel>
          {f().title}
          {requiredMark()}
        </SelectLabel>
        <SelectTrigger>
          <SelectValue>{(state) => String(state.selectedOption())}</SelectValue>
        </SelectTrigger>
        <SelectContent />
        <Show when={hint()}>
          <SelectDescription class={HELP}>{hint()}</SelectDescription>
        </Show>
        <SelectErrorMessage role="alert">
          <span
            aria-hidden="true"
            class="mr-1 inline-block size-1.5 rounded-full bg-accent align-middle"
          />
          {props.error}
        </SelectErrorMessage>
      </Select>
    );
  }

  /**
   * Every remaining kind is a single text-family control — a plain input, a
   * textarea, or a textarea standing in for a structured value (a list, a
   * map, or raw JSON for whatever the reader would not guess at). They share
   * one `TextField` root because exactly one `Show` branch below is ever
   * mounted for a given field, so there is only ever one control reading the
   * root's `value`/`onChange`.
   */
  const rootValue = (): string => {
    switch (f().kind) {
      case "integer":
      case "number":
        return typeof props.value === "number" ? String(props.value) : "";
      case "stringArray":
        return Array.isArray(props.value) ? props.value.join("\n") : "";
      case "stringMap":
        return typeof props.value === "object" &&
          props.value !== null &&
          !Array.isArray(props.value)
          ? Object.entries(props.value)
              .map(([k, val]) => `${k}: ${String(val)}`)
              .join("\n")
          : "";
      case "unsupported":
        return props.value === "" ? "" : JSON.stringify(props.value, null, 2);
      default:
        return typeof props.value === "string" ? props.value : "";
    }
  };

  const rootOnChange = (raw: string): void => {
    switch (f().kind) {
      case "integer":
      case "number": {
        const parsed =
          f().kind === "integer" ? Number.parseInt(raw, 10) : Number.parseFloat(raw);
        props.onChange(Number.isFinite(parsed) ? parsed : "");
        return;
      }
      case "stringArray":
        props.onChange(
          raw
            .split("\n")
            .map((s) => s.trim())
            .filter((s) => s !== ""),
        );
        return;
      case "stringMap": {
        const map: Record<string, JsonValue> = {};
        for (const line of raw.split("\n")) {
          const idx = line.indexOf(":");
          if (idx <= 0) continue;
          const k = line.slice(0, idx).trim();
          if (k !== "") map[k] = line.slice(idx + 1).trim();
        }
        props.onChange(map);
        return;
      }
      case "unsupported": {
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
        return;
      }
      default:
        props.onChange(raw);
    }
  };

  return (
    <TextField
      id={props.id}
      class={FIELD}
      value={rootValue()}
      onChange={rootOnChange}
      validationState={props.error ? "invalid" : "valid"}
      required={f().required}
    >
      <TextFieldLabel>
        {f().title}
        {requiredMark()}
      </TextFieldLabel>

      <Show when={f().kind === "string"}>
        <TextFieldInput
          class={f().format === "uri" || f().pattern !== null ? "font-mono" : undefined}
          maxLength={f().maxLength ?? undefined}
        />
      </Show>

      <Show when={f().kind === "text"}>
        <TextFieldTextArea maxLength={f().maxLength ?? undefined} />
      </Show>

      <Show when={f().kind === "integer" || f().kind === "number"}>
        <TextFieldInput
          type="number"
          min={f().minimum ?? undefined}
          max={f().maximum ?? undefined}
          step={f().kind === "integer" ? 1 : "any"}
        />
      </Show>

      {/* A list of strings, one per line. Simpler to paste into than a chip
          editor and impossible to get stuck in. */}
      <Show when={f().kind === "stringArray"}>
        <TextFieldTextArea placeholder="One value per line" />
      </Show>

      {/* `key: value` per line — the shape webhook headers actually take. */}
      <Show when={f().kind === "stringMap"}>
        <TextFieldTextArea placeholder={"X-Example: value\nOne per line"} />
      </Show>

      {/* The escape hatch. Rendering an unknown schema shape as a plain text
          box would produce a config that passes here and fails the server —
          so it says what it does not understand and takes raw JSON. */}
      <Show when={f().kind === "unsupported"}>
        <p class={HELP}>
          This form does not render {f().reason ?? "this shape"}, so it will not pretend to.
          Enter the value as JSON — the server validates it against the same schema either way.
        </p>
        <TextFieldTextArea class="font-mono" />
      </Show>

      <Show when={hint()}>
        <TextFieldDescription class={HELP}>{hint()}</TextFieldDescription>
      </Show>

      <TextFieldErrorMessage role="alert">
        <span
          aria-hidden="true"
          class="mr-1 inline-block size-1.5 rounded-full bg-accent align-middle"
        />
        {props.error}
      </TextFieldErrorMessage>
    </TextField>
  );
};
