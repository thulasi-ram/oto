/**
 * The small set of controls everything else is built from.
 *
 * Rules that apply to every control here:
 *   - Tier A only (§M.2). Nothing in this file may reference a `--oto-state-*`
 *     token; state colour belongs to state components, and its scarcity is what
 *     makes it loud.
 *   - Focus is never removed (U7). The global `:focus-visible` ring in
 *     index.css does the work, so no control sets `outline: none`.
 *   - Native elements wherever a native element exists. A `<select>` that a
 *     screen reader and a keyboard already understand beats a div we would have
 *     to teach them about.
 */
import {
  createEffect,
  splitProps,
  type Component,
  type JSX,
  type ParentComponent,
} from "solid-js";

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

/* -------------------------------------------------------------------------- */
/* Button                                                                     */
/* -------------------------------------------------------------------------- */

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md";

export interface ButtonProps extends JSX.ButtonHTMLAttributes<HTMLButtonElement> {
  readonly variant?: ButtonVariant;
  readonly size?: ButtonSize;
  /** Renders a spinner and blocks the click without changing the button's width. */
  readonly busy?: boolean;
}

const BUTTON_BASE =
  "inline-flex items-center justify-center gap-1.5 rounded-[4px] border font-medium " +
  "whitespace-nowrap transition-colors duration-100 " +
  "disabled:cursor-not-allowed disabled:opacity-45";

const BUTTON_VARIANT: Record<ButtonVariant, string> = {
  primary:
    "border-transparent bg-accent text-ink-inverse hover:bg-accent-hover active:bg-accent-hover",
  secondary: "border-line-strong bg-surface text-ink hover:bg-raised",
  ghost: "border-transparent bg-transparent text-ink-muted hover:bg-raised hover:text-ink",
  // "danger" is a *destructive-intent* affordance, not an alert state. It stays
  // in Tier A and signals with weight and a strong border, never with a state hue.
  danger: "border-line-strong bg-surface text-ink hover:bg-sunken font-semibold",
};

const BUTTON_SIZE: Record<ButtonSize, string> = {
  sm: "h-7 px-2 text-[12px]",
  md: "h-8 px-3 text-[13px]",
};

export const Button: Component<ButtonProps> = (props) => {
  const [local, rest] = splitProps(props, ["variant", "size", "busy", "class", "children"]);
  return (
    <button
      type="button"
      {...rest}
      aria-busy={local.busy === true ? "true" : undefined}
      disabled={rest.disabled === true || local.busy === true}
      class={cx(
        BUTTON_BASE,
        BUTTON_VARIANT[local.variant ?? "secondary"],
        BUTTON_SIZE[local.size ?? "md"],
        local.class,
      )}
    >
      {local.busy === true ? <Spinner /> : null}
      {local.children}
    </button>
  );
};

/** A 12 px indeterminate spinner. Under reduced motion it stops and shows a dot. */
export const Spinner: Component<{ readonly class?: string }> = (props) => (
  <span
    aria-hidden="true"
    class={cx(
      "inline-block size-3 shrink-0 rounded-full border-[1.5px] border-current",
      "border-r-transparent motion-safe:animate-spin motion-reduce:border-r-current",
      "motion-reduce:opacity-60",
      props.class,
    )}
  />
);

/* -------------------------------------------------------------------------- */
/* Text inputs                                                                */
/* -------------------------------------------------------------------------- */

const FIELD_BASE =
  "w-full rounded-[4px] border border-line-strong bg-surface text-ink " +
  "placeholder:text-ink-subtle " +
  "disabled:cursor-not-allowed disabled:bg-sunken disabled:opacity-60 " +
  "aria-[invalid=true]:border-line-strong aria-[invalid=true]:ring-1 " +
  "aria-[invalid=true]:ring-accent-border";

export interface InputProps extends JSX.InputHTMLAttributes<HTMLInputElement> {
  readonly invalid?: boolean;
  readonly mono?: boolean;
}

export const Input: Component<InputProps> = (props) => {
  const [local, rest] = splitProps(props, ["invalid", "mono", "class"]);
  return (
    <input
      {...rest}
      aria-invalid={local.invalid === true ? "true" : undefined}
      class={cx(
        FIELD_BASE,
        "h-8 px-2 text-[13px]",
        local.mono === true ? "font-mono" : "",
        local.class,
      )}
    />
  );
};

export interface TextareaProps extends JSX.TextareaHTMLAttributes<HTMLTextAreaElement> {
  readonly invalid?: boolean;
}

export const Textarea: Component<TextareaProps> = (props) => {
  const [local, rest] = splitProps(props, ["invalid", "class"]);
  return (
    <textarea
      {...rest}
      aria-invalid={local.invalid === true ? "true" : undefined}
      class={cx(FIELD_BASE, "min-h-16 resize-y px-2 py-1.5 text-[13px] leading-relaxed", local.class)}
    />
  );
};

export interface SelectProps extends JSX.SelectHTMLAttributes<HTMLSelectElement> {
  readonly invalid?: boolean;
}

/**
 * `prop:value` rather than `value` is load-bearing.
 *
 * When the options come from a `<For>`, Solid inserts the children *after* it
 * assigns the attributes, so a plain `value` is applied to a `<select>` that is
 * still empty and the browser silently falls back to the first option. Setting
 * the DOM property in an effect — which `prop:` does — runs after the children
 * exist, so the bound value actually sticks.
 */
export const Select: Component<SelectProps> = (props) => {
  const [local, rest] = splitProps(props, ["invalid", "class", "children", "value"]);
  let el: HTMLSelectElement | undefined;

  // The assignment has to happen after the options exist, and it has to re-run
  // when either the value or the option list changes — hence an effect that
  // reads both rather than a `value` attribute on the element.
  createEffect(() => {
    void local.children;
    const next = local.value === undefined || local.value === null ? "" : String(local.value);
    if (el && el.value !== next) el.value = next;
  });

  return (
    <select
      ref={el}
      {...rest}
      aria-invalid={local.invalid === true ? "true" : undefined}
      class={cx(FIELD_BASE, "h-8 cursor-pointer px-1.5 text-[13px]", local.class)}
    >
      {local.children}
    </select>
  );
};

/* -------------------------------------------------------------------------- */
/* Field — label, control, hint, error                                        */
/* -------------------------------------------------------------------------- */

export interface FieldProps {
  readonly id: string;
  readonly label: string;
  readonly hint?: string | undefined;
  /** A server violation or a local validation failure. Rendered, never swallowed. */
  readonly error?: string | undefined;
  readonly required?: boolean;
  readonly children: (a: {
    readonly id: string;
    readonly "aria-describedby": string | undefined;
    readonly "aria-invalid": "true" | undefined;
  }) => JSX.Element;
}

export const Field: Component<FieldProps> = (props) => {
  const hintId = (): string => `${props.id}-hint`;
  const errorId = (): string => `${props.id}-error`;
  const describedBy = (): string | undefined => {
    const ids = [props.hint ? hintId() : null, props.error ? errorId() : null].filter(Boolean);
    return ids.length > 0 ? ids.join(" ") : undefined;
  };

  return (
    <div class="flex flex-col gap-1">
      <label for={props.id} class="text-[12px] font-medium text-ink-muted">
        {props.label}
        {props.required === true ? (
          <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
            *
          </span>
        ) : null}
      </label>
      {props.children({
        id: props.id,
        "aria-describedby": describedBy(),
        "aria-invalid": props.error ? "true" : undefined,
      })}
      {props.hint ? (
        <p id={hintId()} class="text-[11px] leading-snug text-ink-subtle">
          {props.hint}
        </p>
      ) : null}
      {props.error ? (
        <p id={errorId()} class="text-[11px] font-medium leading-snug text-ink" role="alert">
          <span
            aria-hidden="true"
            class="mr-1 inline-block size-1.5 rounded-full bg-accent align-middle"
          />
          {props.error}
        </p>
      ) : null}
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Checkbox and toggle group                                                  */
/* -------------------------------------------------------------------------- */

export interface CheckboxProps {
  readonly id?: string;
  readonly checked: boolean;
  readonly onChange: (next: boolean) => void;
  readonly label: JSX.Element;
  readonly disabled?: boolean;
}

export const Checkbox: Component<CheckboxProps> = (props) => (
  <label
    class={cx(
      "inline-flex cursor-pointer select-none items-center gap-1.5 text-[13px] text-ink",
      props.disabled === true ? "cursor-not-allowed opacity-50" : "",
    )}
  >
    <input
      type="checkbox"
      id={props.id}
      checked={props.checked}
      disabled={props.disabled}
      onChange={(e) => props.onChange(e.currentTarget.checked)}
      class="size-3.5 cursor-pointer accent-[var(--oto-accent)]"
    />
    {props.label}
  </label>
);

/**
 * A segmented multi-select rendered as real checkboxes inside a fieldset, so a
 * screen reader announces the group name and the checked count without any ARIA
 * of our own.
 */
export interface ToggleGroupProps<T extends string> {
  readonly legend: string;
  readonly options: readonly { readonly value: T; readonly label: string }[];
  readonly selected: readonly T[];
  readonly onChange: (next: readonly T[]) => void;
  /**
   * Show the legend. Off in the dense filter bar, where the chips are
   * self-describing and the space is worth more; on in forms, where a group of
   * unlabelled chips is a guessing game.
   */
  readonly showLegend?: boolean;
}

export function ToggleGroup<T extends string>(props: ToggleGroupProps<T>): JSX.Element {
  const toggle = (value: T): void => {
    const has = props.selected.includes(value);
    props.onChange(has ? props.selected.filter((v) => v !== value) : [...props.selected, value]);
  };

  return (
    <fieldset class="min-w-0">
      <legend
        class={
          props.showLegend === true
            ? "mb-1 text-[12px] font-medium text-ink-muted"
            : "sr-only-focusable"
        }
      >
        {props.legend}
      </legend>
      <div class="flex flex-wrap items-center gap-1" role="group" aria-label={props.legend}>
        {props.options.map((opt) => {
          const active = (): boolean => props.selected.includes(opt.value);
          return (
            <label
              class={cx(
                "inline-flex cursor-pointer items-center gap-1 rounded-[4px] border px-1.5 py-0.5",
                "text-[12px] transition-colors duration-100",
                active()
                  ? "border-accent-border bg-accent-fill text-ink"
                  : "border-line bg-surface text-ink-muted hover:bg-raised",
              )}
            >
              <input
                type="checkbox"
                class="sr-only-focusable size-3 accent-[var(--oto-accent)]"
                checked={active()}
                onChange={() => toggle(opt.value)}
              />
              {opt.label}
            </label>
          );
        })}
      </div>
    </fieldset>
  );
}

/* -------------------------------------------------------------------------- */
/* Surfaces                                                                   */
/* -------------------------------------------------------------------------- */

export const Panel: ParentComponent<{ readonly class?: string }> = (props) => (
  <section class={cx("rounded-[6px] border border-line bg-surface", props.class)}>
    {props.children}
  </section>
);

export const PanelHeader: ParentComponent<{ readonly class?: string }> = (props) => (
  <header
    class={cx(
      "flex items-center justify-between gap-3 border-b border-line bg-raised px-3 py-2",
      "rounded-t-[6px]",
      props.class,
    )}
  >
    {props.children}
  </header>
);

export const PanelTitle: ParentComponent<{ readonly class?: string }> = (props) => (
  <h2
    class={cx(
      "text-[11px] font-semibold uppercase tracking-[0.08em] text-ink-muted",
      props.class,
    )}
  >
    {props.children}
  </h2>
);

/** A neutral chip. Tier A only — never used to carry a state. */
export const Chip: ParentComponent<{
  readonly class?: string;
  readonly title?: string;
  readonly mono?: boolean;
}> = (props) => (
  <span
    title={props.title}
    class={cx(
      "inline-flex max-w-full items-center gap-1 rounded-[3px] border border-line bg-raised",
      "px-1 py-px text-[11px] leading-4 text-ink-muted",
      props.mono === true ? "font-mono" : "",
      props.class,
    )}
  >
    {props.children}
  </span>
);

/** A definition row: fixed-width term, wrapping value. Used all over detail. */
export const DataRow: ParentComponent<{ readonly term: string; readonly class?: string }> = (
  props,
) => (
  <div class={cx("grid grid-cols-[minmax(0,7.5rem)_minmax(0,1fr)] gap-x-3 gap-y-0.5", props.class)}>
    <dt class="truncate pt-px text-[12px] text-ink-subtle" title={props.term}>
      {props.term}
    </dt>
    <dd class="min-w-0 text-[13px] text-ink">{props.children}</dd>
  </div>
);
