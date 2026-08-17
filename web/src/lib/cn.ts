/**
 * The class-name merger every solid-ui-derived component in `components/ui/`
 * uses at its `class` prop boundary.
 *
 * `clsx` collapses the conditional/array/object forms callers pass down to a
 * single string; `twMerge` then resolves same-axis Tailwind utility
 * collisions in favour of the last one written (so `cn(buttonVariants({...}),
 * local.class)` lets a caller's `class` override a variant's default without
 * both ending up in the class list at once, e.g. `px-3 px-2` picking neither).
 *
 * This mirrors solid-ui's own `~/lib/utils#cn` verbatim (see
 * https://solid-ui.com — `apps/docs/src/lib/utils.ts`) so components ported
 * from the registry work unmodified. The app's old hand-rolled `cx` (which
 * only joined truthy strings and did not de-duplicate conflicting utilities)
 * has been retired in favor of this one, now that every component uses it.
 */
import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

/**
 * oto's own scales, taught to tailwind-merge.
 *
 * `twMerge` only knows Tailwind's stock scales, so every oto step is invisible
 * to it — and the two failure modes are opposite and both bad:
 *
 *  - **Type.** `text-item` does not match the font-size group's `text-xs…9xl`
 *    ladder, so it falls through to the catch-all text-COLOUR group, which
 *    matches any `text-*`. `cn("text-item", "text-ink-muted")` then reads as
 *    one axis and silently drops the size, leaving the element at 16px.
 *  - **Spacing and radius.** `px-lg` matches no group at all, so it never
 *    collides with `px-3`. Both survive the merge and the winner is decided by
 *    CSS emission order rather than by call-site intent — an override that
 *    looks correct in review and is not.
 *
 * Registering the scales here makes an oto step and its Tailwind-numeric
 * equivalent the same axis, so last-one-wins works the way every call site
 * already assumes. tailwind-merge v3 keys these by Tailwind v4 theme namespace.
 */
const twMerge = extendTailwindMerge({
  extend: {
    theme: {
      text: ["micro", "meta", "body", "item", "title", "page"],
      spacing: ["2xs", "xs", "sm", "md", "lg", "xl"],
      radius: ["chip", "control", "surface"],
    },
  },
});

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
