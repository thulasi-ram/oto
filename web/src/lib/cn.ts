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
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
