/**
 * Bespoke layout surfaces with no solid-ui counterpart, relocated verbatim
 * (in behaviour) out of `primitives.tsx` — `Panel`, `PanelHeader`, `PanelTitle`,
 * `Chip`, `DataRow`. solid-ui doesn't ship a "Panel" or "DataRow" concept, so
 * there is nothing to unify these against; they move here only because
 * `primitives.tsx` itself is being retired once every other export
 * (Button/Input/Textarea/Select/Field/Checkbox/ToggleGroup/Spinner/cx) has
 * been migrated off it by other agents in a later phase.
 *
 * The one change from the original: `cn()` from `~/lib/cn` in place of
 * `primitives.tsx`'s local `cx()` helper. `cx()` is not re-exported from
 * here or anywhere else — every future consumer imports `cn` from `~/lib/cn`
 * directly, the same merger every other solid-ui-derived component in
 * `components/ui/` already uses at its `class` prop boundary.
 */
import type { ParentComponent } from "solid-js";

import { cn } from "~/lib/cn";

export const Panel: ParentComponent<{ readonly class?: string }> = (props) => (
  <section class={cn("rounded-surface border border-line bg-surface", props.class)}>
    {props.children}
  </section>
);

export const PanelHeader: ParentComponent<{ readonly class?: string }> = (props) => (
  <header
    class={cn(
      "flex items-center justify-between gap-3 border-b border-line bg-raised px-3 py-2",
      "rounded-t-surface",
      props.class,
    )}
  >
    {props.children}
  </header>
);

export const PanelTitle: ParentComponent<{ readonly class?: string }> = (props) => (
  <h2
    class={cn(
      "text-meta font-semibold uppercase tracking-[0.08em] text-ink-muted",
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
    class={cn(
      "inline-flex max-w-full items-center gap-1 rounded-chip border border-line bg-raised",
      "px-1 py-px text-meta leading-4 text-ink-muted",
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
  <div class={cn("grid grid-cols-[minmax(0,7.5rem)_minmax(0,1fr)] gap-x-3 gap-y-0.5", props.class)}>
    <dt class="truncate pt-px text-body text-ink-subtle" title={props.term}>
      {props.term}
    </dt>
    <dd class="min-w-0 text-item text-ink">{props.children}</dd>
  </div>
);
