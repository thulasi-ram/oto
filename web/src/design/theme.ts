import { createSignal, createEffect } from "solid-js";

/**
 * Theme and density preferences.
 *
 * SPEC §M.3 U3: **dark is the default** for an ops tool, with an explicit light
 * option, and `prefers-color-scheme` respected on first load. "system" therefore
 * resolves to light only when the OS says light out loud; absence of a stated
 * preference falls through to dark.
 *
 * The same resolution is duplicated as an inline boot script in index.html so
 * the first paint is already correct — a theme flash is a layout shift with
 * extra steps.
 */

export type ThemePreference = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";
export type Density = "comfortable" | "compact";

const THEME_KEY = "oto.theme";
const DENSITY_KEY = "oto.density";

function readStored<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    if (raw !== null && (allowed as readonly string[]).includes(raw)) return raw as T;
  } catch {
    /* private mode, storage disabled — the default is still correct */
  }
  return fallback;
}

export function resolveTheme(pref: ThemePreference): ResolvedTheme {
  if (pref !== "system") return pref;
  if (typeof window === "undefined" || !window.matchMedia) return "dark";
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

const [themePreference, setThemePreference] = createSignal<ThemePreference>(
  readStored(THEME_KEY, ["system", "light", "dark"] as const, "system"),
);
const [systemTheme, setSystemTheme] = createSignal<ResolvedTheme>(resolveTheme("system"));
const [density, setDensityInternal] = createSignal<Density>(
  readStored(DENSITY_KEY, ["comfortable", "compact"] as const, "comfortable"),
);

if (typeof window !== "undefined" && window.matchMedia) {
  const mql = window.matchMedia("(prefers-color-scheme: light)");
  const onChange = (): void => {
    setSystemTheme(mql.matches ? "light" : "dark");
  };
  mql.addEventListener("change", onChange);
}

export const theme = (): ResolvedTheme => {
  const pref = themePreference();
  return pref === "system" ? systemTheme() : pref;
};

export { themePreference, density };

export function setTheme(pref: ThemePreference): void {
  setThemePreference(pref);
  try {
    localStorage.setItem(THEME_KEY, pref);
  } catch {
    /* ignore */
  }
}

export function setDensity(next: Density): void {
  setDensityInternal(next);
  try {
    localStorage.setItem(DENSITY_KEY, next);
  } catch {
    /* ignore */
  }
}

/** Mount once from the app root: keeps <html> in sync with the signals. */
export function installThemeEffect(): void {
  createEffect(() => {
    document.documentElement.dataset["theme"] = theme();
  });
  createEffect(() => {
    document.documentElement.dataset["density"] = density();
  });
}

/** True when the user has asked the platform for less motion (U4). */
export function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || !window.matchMedia) return false;
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}
