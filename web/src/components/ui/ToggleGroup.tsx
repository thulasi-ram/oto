/**
 * A multi-select segmented control, built directly on `@kobalte/core/toggle-group`
 * rather than ported from solid-ui.com's registry.
 *
 * ⛔⛔ IT IS A FORM FIELD NOW, AND IT IS NOT A FILTER. It has exactly one caller
 * left — the policy editor in `PoliciesSection`, where the operator is choosing
 * which suppression reasons a policy CARRIES — and that is the shape it is right
 * for: a bounded set of facts being authored, all of them worth seeing at once, in
 * a form that already has a label and a submit button.
 *
 * Every FILTER that used to be one of these is a checkbox dropdown now
 * (`~/components/ui/FilterMenu`). The reason is in that file's header and it is
 * worth not relearning: a row of pills with one lit says *"you are in one of
 * these"*, which is a tab strip's sentence, and a filter's sentence is *"the list
 * is narrowed to these"*. The two disagree the moment two pills are on — which for
 * `Open + Ended`, or `failed + dead`, is the normal case rather than the edge one.
 * Five screens had four different idioms for it.
 *
 * So: authoring a value, in a form → this. Narrowing a list → `FilterMenu`.
 *
 * solid-ui *does* ship a `toggle-group.tsx` (https://solid-ui.com/docs/components/toggle-group),
 * but it is not a faithful port here for two reasons:
 *
 *   1. Its `ToggleGroupItem` depends on a sibling `toggle.tsx` for `toggleVariants`
 *      (a shadcn toolbar-button cva map — `bg-transparent`/`border-input`, sized
 *      for a bold/italic/underline toolbar). Nothing else in this app needs a
 *      standalone `Toggle`, and that map doesn't match what this control has
 *      always looked like here: the segmented chip styling `primitives.tsx`'s
 *      `ToggleGroup` already uses (`rounded-control border`, `bg-accent-fill`
 *      on the active chip). This file ports that visual language forward
 *      instead of shadcn's toolbar-button one, keyed off Kobalte's own
 *      `data-[pressed]` attribute in place of the old component's manual
 *      `active()` check.
 *   2. Neither solid-ui's port nor Kobalte's primitive gives the group an
 *      accessible name — `ToggleGroupPrimitive.Root` renders `role="group"`
 *      with nothing pointing at a label. `primitives.tsx`'s `ToggleGroup`
 *      solved this with a real `<fieldset>`/`<legend>` pair (and a `showLegend`
 *      prop for the dense filter bar, where the chips are self-describing and
 *      an always-visible legend would cost more space than it earns — see
 *      that file's own comment on why a *second*, redundant `role="group"`
 *      made things worse, not better). A `role="group"` div can't contain a
 *      `<legend>`, so the same contract is reproduced with a labelled `<span>`
 *      wired to the root via `aria-labelledby` instead — this is the same
 *      "an unlabelled group is a guessing game" lesson the FilterBar `TextField`
 *      already ran into earlier in this migration, and it is required here
 *      too: `legend` is a required prop, not optional.
 *
 * `multiple` selection is Kobalte's own `ToggleGroupRootOptions` union — pass
 * `multiple` with `value`/`onChange` typed as `string[]`, exactly as the old
 * `ToggleGroup<T>`'s `selected: readonly T[]` did, just without the generic
 * (Kobalte's value type is `string[]`, so a caller narrows at the edges).
 *
 * Retheme notes (see `Button.tsx`/`DropdownMenu.tsx` for the fuller rationale):
 *   - `rounded-md`/`rounded-sm` -> `rounded-control` (an inline chip-sized control).
 *   - `text-sm` -> `text-body`, matching the chip text size `primitives.tsx` uses.
 *   - `focus-visible:ring-2 ring-ring ring-offset-2` dropped — the global
 *     `:focus-visible` rule already rings the item, a real `<button>` (U7).
 *   - the active/pressed treatment mirrors `primitives.tsx` exactly:
 *     `border-accent-border bg-accent-fill text-ink` — Tier A only, never a
 *     state hue (§M.2).
 */
import type { JSX, ValidComponent } from "solid-js";
import { createUniqueId, splitProps } from "solid-js";

import type { PolymorphicProps } from "@kobalte/core/polymorphic";
import * as ToggleGroupPrimitive from "@kobalte/core/toggle-group";

import { cn } from "~/lib/cn";

type ToggleGroupRootProps<T extends ValidComponent = "div"> =
  ToggleGroupPrimitive.ToggleGroupRootProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
    /** The group's accessible name. Required — see the header comment on why. */
    legend: string;
    /**
     * Show the legend. Off in the dense filter bar, where the chips are
     * self-describing and the space is worth more; on in forms, where a group
     * of unlabelled chips is a guessing game. Mirrors `primitives.tsx`'s
     * `ToggleGroup` prop of the same name and default.
     */
    showLegend?: boolean;
  };

export const ToggleGroup = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, ToggleGroupRootProps<T>>,
) => {
  const [local, others] = splitProps(props as ToggleGroupRootProps, [
    "class",
    "children",
    "legend",
    "showLegend",
  ]);
  const legendId = createUniqueId();

  return (
    <div class="min-w-0">
      <span
        id={legendId}
        class={
          local.showLegend === true
            ? "mb-1 block text-body font-medium text-ink-muted"
            : "sr-only-focusable"
        }
      >
        {local.legend}
      </span>
      <ToggleGroupPrimitive.Root
        aria-labelledby={legendId}
        class={cn(
          // A segmented well, not a row of individually-bordered boxes — the
          // same track/chip relationship `Tabs.tsx`'s `TabsList` already uses.
          // One border around the group reads as one control with several
          // values; a border around every pill reads as several controls that
          // happen to be neighbours.
          "flex flex-wrap items-center gap-2xs rounded-control bg-sunken p-2xs",
          local.class,
        )}
        {...others}
      >
        {local.children}
      </ToggleGroupPrimitive.Root>
    </div>
  );
};

type ToggleGroupItemProps<T extends ValidComponent = "button"> =
  ToggleGroupPrimitive.ToggleGroupItemProps<T> & {
    class?: string | undefined;
    children?: JSX.Element;
  };

export const ToggleGroupItem = <T extends ValidComponent = "button">(
  props: PolymorphicProps<T, ToggleGroupItemProps<T>>,
) => {
  const [local, others] = splitProps(props as ToggleGroupItemProps, ["class", "children"]);
  return (
    <ToggleGroupPrimitive.Item
      class={cn(
        // No border of its own — the well around the group is the only
        // outline this control needs; resting state is transparent so the
        // well's `bg-sunken` shows through between pills. The pressed state
        // keeps its accent tint (Tier A, never a state hue, §M.2) — unlike a
        // tab, a pressed filter chip is a fact worth seeing at a glance, not
        // just "which view am I in".
        "inline-flex cursor-pointer items-center gap-2xs rounded-control " +
          "px-xs py-2xs text-body text-ink-muted transition-colors duration-100 " +
          "hover:bg-raised hover:text-ink " +
          "data-[pressed]:bg-accent-fill data-[pressed]:text-ink " +
          "disabled:cursor-not-allowed disabled:opacity-45",
        local.class,
      )}
      {...others}
    >
      {local.children}
    </ToggleGroupPrimitive.Item>
  );
};
