/**
 * The signed-in operator's own menu: who they are, and the handful of controls
 * that are theirs alone — display preferences and the door out.
 *
 * These three used to sit side by side in the header as always-visible
 * controls. That was fine when there were three; it stopped being fine once
 * "Settings" in the primary nav grew a second, unrelated meaning (org-level
 * admin configuration) that this menu exists to disambiguate from. So they are
 * consolidated here, behind one disclosure trigger showing who is signed in.
 *
 * Built on the native Popover API (`popover` + `popovertarget`) rather than a
 * hand-rolled `div` with outside-click/Escape listeners — the same reach for
 * the platform primitive this codebase already applies to `Dialog.tsx` and its
 * native `<dialog>`. The browser gives us top-layer stacking and light-dismiss
 * (click-outside, Escape) for free; a hand-rolled version would reimplement
 * both and inevitably reimplement them worse.
 *
 * Positioning: the trigger and the panel share a `relative` wrapper, and the
 * panel overrides the popover's default `position: fixed` (which centers it in
 * the viewport) with `position: absolute`, anchored to the wrapper. A
 * top-layer element's containing block is still resolved from its place in the
 * DOM, so this ordinary CSS trick works without CSS anchor positioning.
 */
import { useNavigate } from "@solidjs/router";
import { Show, createSignal, type Component, type JSX } from "solid-js";

import { useSession } from "~/api/session";
import { Button, cx } from "~/components/ui/primitives";
import {
  density,
  setDensity,
  setTheme,
  theme,
  themePreference,
  type ThemePreference,
} from "~/design/theme";

/* -------------------------------------------------------------------------- */
/* Preferences                                                                */
/* -------------------------------------------------------------------------- */

const THEME_ORDER: readonly ThemePreference[] = ["system", "light", "dark"];

const ThemeToggle = (): JSX.Element => {
  const next = (): ThemePreference =>
    THEME_ORDER[(THEME_ORDER.indexOf(themePreference()) + 1) % THEME_ORDER.length] ?? "system";

  return (
    <Button
      size="sm"
      variant="ghost"
      onClick={() => setTheme(next())}
      title={`Theme: ${themePreference()} (currently ${theme()}). Click for ${next()}.`}
      aria-label={`Theme: ${themePreference()}, currently rendering ${theme()}. Switch to ${next()}.`}
    >
      <Show when={theme() === "dark"} fallback={<SunGlyph />}>
        <MoonGlyph />
      </Show>
      <span class="capitalize">{themePreference()}</span>
    </Button>
  );
};

const DensityToggle = (): JSX.Element => (
  <Button
    size="sm"
    variant="ghost"
    onClick={() => setDensity(density() === "compact" ? "comfortable" : "compact")}
    title={`Row density: ${density()}`}
    aria-label={`Row density: ${density()}. Switch to ${
      density() === "compact" ? "comfortable" : "compact"
    }.`}
  >
    <DensityGlyph compact={density() === "compact"} />
    <span class="capitalize">{density()}</span>
  </Button>
);

/**
 * Sign out, labelled with who is signed in.
 *
 * The principal is already in hand — `/me` answered before this shell mounted —
 * so naming it costs nothing and answers "which account am I in?" without a
 * settings trip. The button does not confirm: a sign-out is one click to undo.
 */
const SignOut: Component = () => {
  const session = useSession();
  const navigate = useNavigate();
  const [busy, setBusy] = createSignal(false);

  // ⛔ THIS DOES NOT CLEAR ON NAVIGATION, AND MUST NOT. The shell is a layout
  // route now, so this signal follows the operator from screen to screen — which
  // looks like stale state and is not. It reports a standing FACT, not the
  // outcome of a gesture: the session is still live. Only two things can make it
  // false, and both take the whole shell with them — a sign-out that succeeds
  // (which leaves for /login) or a session that expires (which `RequireSession`
  // bounces). Clearing it on a nav click would delete the one sentence telling
  // the operator not to walk away from this machine, in response to a gesture
  // that did nothing about it. A retry clears it below, at the point it is
  // actually being re-answered.
  const [failed, setFailed] = createSignal(false);

  const go = async (): Promise<void> => {
    setBusy(true);
    setFailed(false);
    try {
      await session.signOut();
      navigate("/login", { replace: true });
    } catch {
      // ⛔ STILL SIGNED IN, AND THE UI MUST SAY SO. The revoke and the cookie
      // clear are one response, so a failure means the session is live. Bouncing
      // to /login here would be the dangerous lie: the operator walks away and
      // the next person's refresh resolves the surviving cookie straight back
      // into this account.
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Show when={failed()}>
        <span role="alert" class="text-meta font-medium text-ink">
          Still signed in — sign-out failed. Try again.
        </span>
      </Show>
      <button
        type="button"
        onClick={() => void go()}
        disabled={busy()}
        title={session.me()?.user?.email ?? undefined}
        class="rounded-control px-1.5 py-1 text-meta font-medium text-ink-muted transition-colors duration-100 hover:bg-raised hover:text-ink disabled:cursor-not-allowed disabled:opacity-45"
      >
        Sign out
      </button>
    </>
  );
};

/* -------------------------------------------------------------------------- */
/* The menu                                                                   */
/* -------------------------------------------------------------------------- */

/** Up to two initials out of the mailbox part of an email, for a compact trigger. */
function initials(email: string | undefined): string {
  const local = (email ?? "").split("@")[0] ?? "";
  const parts = local.split(/[._+-]+/).filter(Boolean);
  const letters =
    parts.length >= 2 ? [parts[0]?.[0], parts[1]?.[0]] : [local[0], local[1]];
  const out = letters.filter((c): c is string => Boolean(c)).join("").toUpperCase();
  return out.length > 0 ? out : "?";
}

const MENU_ID = "user-menu";

export const UserMenu: Component = () => {
  const session = useSession();
  const email = (): string | undefined => session.me()?.user?.email ?? undefined;

  return (
    <div class="relative">
      <Button
        id={`${MENU_ID}-trigger`}
        popovertarget={MENU_ID}
        popovertargetaction="toggle"
        size="sm"
        variant="ghost"
        title={email()}
        aria-label={email() ? `Account menu for ${email()}` : "Account menu"}
      >
        <span
          aria-hidden="true"
          class="inline-flex size-4.5 shrink-0 items-center justify-center rounded-full bg-accent-fill text-meta font-semibold text-ink"
        >
          {initials(email())}
        </span>
        <ChevronGlyph />
      </Button>

      <div
        id={MENU_ID}
        popover
        class={cx(
          "absolute inset-auto right-0 top-full m-0 mt-1.5 w-64 flex-col gap-1",
          "rounded-surface border border-line bg-surface p-2 text-ink",
          "shadow-[0_16px_48px_-12px_rgb(0_0_0_/_0.35)]",
          // The UA stylesheet's `[popover]:popover-open` is `display: block` by
          // default, not flex — the popover's own open/closed state drives
          // whether this renders at all, so `flex` only needs to apply while open.
          "open:flex",
        )}
      >
        <p class="truncate px-1 py-1 text-meta text-ink-subtle">{email() ?? "Signed in"}</p>
        <div class="h-px bg-line" aria-hidden="true" />
        <ThemeToggle />
        <DensityToggle />
        <div class="h-px bg-line" aria-hidden="true" />
        <SignOut />
      </div>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Glyphs                                                                     */
/* -------------------------------------------------------------------------- */

const ChevronGlyph = (): JSX.Element => (
  <svg viewBox="0 0 14 14" class="size-3" aria-hidden="true">
    <path
      d="M3.5 5.25 7 8.75l3.5-3.5"
      fill="none"
      stroke="currentColor"
      stroke-width="1.3"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);

const SunGlyph = (): JSX.Element => (
  <svg viewBox="0 0 14 14" class="size-3.5" aria-hidden="true">
    <circle cx="7" cy="7" r="2.6" fill="none" stroke="currentColor" stroke-width="1.3" />
    <path
      d="M7 1.2v1.6M7 11.2v1.6M1.2 7h1.6M11.2 7h1.6M3 3l1.1 1.1M9.9 9.9 11 11M11 3 9.9 4.1M4.1 9.9 3 11"
      stroke="currentColor"
      stroke-width="1.3"
      stroke-linecap="round"
    />
  </svg>
);

const MoonGlyph = (): JSX.Element => (
  <svg viewBox="0 0 14 14" class="size-3.5" aria-hidden="true">
    <path
      d="M11.4 8.6A5 5 0 0 1 5.4 2.6a5 5 0 1 0 6 6Z"
      fill="none"
      stroke="currentColor"
      stroke-width="1.3"
      stroke-linejoin="round"
    />
  </svg>
);

const DensityGlyph = (props: { readonly compact: boolean }): JSX.Element => (
  <svg viewBox="0 0 14 14" class="size-3.5" aria-hidden="true">
    <Show
      when={props.compact}
      fallback={
        <path d="M2 4h10M2 7h10M2 10h10" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
      }
    >
      <path
        d="M2 3h10M2 5.5h10M2 8h10M2 10.5h10"
        stroke="currentColor"
        stroke-width="1.3"
        stroke-linecap="round"
      />
    </Show>
  </svg>
);
