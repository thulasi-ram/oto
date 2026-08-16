/**
 * solid-ui's Checkbox (https://solid-ui.com/docs/components/checkbox), ported
 * onto `@kobalte/core/checkbox`.
 *
 * This is a distinct component from `primitives.tsx`'s `Checkbox` (a plain
 * native `<input type="checkbox">` wrapped in a `<label>`) — both now exist
 * side by side. Reach for this one where Kobalte's `indeterminate` state is
 * worth the extra pieces.
 *
 * The upstream registry ships only `Root`/`Input`/`Control`/`Indicator` — no
 * bundled `CheckboxLabel`. Its own demo wires a plain `<label for={id-input}>`
 * next to the checkbox rather than a compound label part (Kobalte suffixes
 * the auto-generated input id with `-input`), so there is nothing to port for
 * a "Label sub-part" here; a consumer supplies its own `<label for={\`${id}-input\`}>`
 * exactly as upstream's demo does.
 *
 * Retheme notes (see `Button.tsx`/`TextField.tsx` for the fuller rationale):
 *   - `border-primary` -> `border-line-strong` (Tier A, unchecked box).
 *   - `rounded-sm` -> `rounded-control` (this is a control, not an inline chip).
 *   - `data-[checked]:bg-primary`/`data-[indeterminate]:bg-primary` and their
 *     `-foreground` pair -> `bg-accent`/`text-ink-inverse`, the same mapping
 *     `Button.tsx`'s primary variant uses.
 *   - `disabled:opacity-50` -> `disabled:opacity-45`, matching `Button.tsx`.
 *   - `ring-offset-background` dropped (no offset colour token needed once
 *     the ring itself is retthemed, below).
 *   - `peer-focus-visible:ring-2 ring-ring ring-offset-2` KEPT, unlike the
 *     ring classes every other port here drops for "the global `:focus-visible`
 *     rule already rings it (U7)". That rationale assumes the element that
 *     receives focus is the element the user sees. Here it is not: Kobalte's
 *     `Checkbox.Input` renders with `@kobalte/utils`'s visually-hidden styles
 *     (`clip: rect(0,0,0,0)`, 1x1px, `overflow: hidden`) rather than an
 *     invisible overlay the same size as `Control`, so an outline on the real
 *     focus target would be clipped to nothing. The `peer-focus-visible` ring
 *     on the visible `Control` is the only way focus becomes visible at all,
 *     so it stays — retargeted from `ring-ring` to oto's own `--color-focus`
 *     token (`ring-focus`), the same token `index.css`'s global rule paints.
 */
import type { ValidComponent } from "solid-js";
import { Match, splitProps, Switch } from "solid-js";

import * as CheckboxPrimitive from "@kobalte/core/checkbox";
import type { PolymorphicProps } from "@kobalte/core/polymorphic";

import { cn } from "~/lib/cn";

type CheckboxRootProps<T extends ValidComponent = "div"> = CheckboxPrimitive.CheckboxRootProps<T> & {
  class?: string | undefined;
};

export const Checkbox = <T extends ValidComponent = "div">(
  props: PolymorphicProps<T, CheckboxRootProps<T>>,
) => {
  const [local, others] = splitProps(props as CheckboxRootProps, ["class"]);
  return (
    <CheckboxPrimitive.Root
      class={cn("items-top group relative flex space-x-2", local.class)}
      {...others}
    >
      <CheckboxPrimitive.Input class="peer" />
      <CheckboxPrimitive.Control
        class={cn(
          "size-3.5 shrink-0 rounded-control border border-line-strong " +
            "disabled:cursor-not-allowed disabled:opacity-45 " +
            "peer-focus-visible:outline-none peer-focus-visible:ring-2 peer-focus-visible:ring-focus " +
            "peer-focus-visible:ring-offset-2 " +
            "data-[checked]:border-none data-[indeterminate]:border-none " +
            "data-[checked]:bg-accent data-[indeterminate]:bg-accent " +
            "data-[checked]:text-ink-inverse data-[indeterminate]:text-ink-inverse",
        )}
      >
        <CheckboxPrimitive.Indicator>
          <Switch>
            <Match when={!others.indeterminate}>
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
                stroke-linecap="round"
                stroke-linejoin="round"
                class="size-3.5"
              >
                <path d="M5 12l5 5l10 -10" />
              </svg>
            </Match>
            <Match when={others.indeterminate}>
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
                stroke-linecap="round"
                stroke-linejoin="round"
                class="size-3.5"
              >
                <path d="M5 12l14 0" />
              </svg>
            </Match>
          </Switch>
        </CheckboxPrimitive.Indicator>
      </CheckboxPrimitive.Control>
    </CheckboxPrimitive.Root>
  );
};
