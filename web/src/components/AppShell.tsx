/**
 * The frame every screen hangs in.
 *
 * It carries four things and deliberately nothing else: where you are, whether
 * what you are looking at is actually live, whether it is *complete*, and the two
 * display preferences an operational surface genuinely needs (theme and density).
 *
 * The honesty machinery is the load-bearing part. A UI that renders stale rows
 * while implying they are current is the exact failure oto exists to prevent, so
 * this never says "live" unless frames are arriving; and a calm list is only
 * evidence of calm when oto can still see every source and is not holding its
 * tongue, which is what the two banners below `ResyncBanner` are for. All three
 * ride in the header so they reach every route, not just the settings screen
 * nobody is on when it matters.
 */
import { A, useLocation, useNavigate } from "@solidjs/router";
import {
  Show,
  createMemo,
  createSignal,
  type Component,
  type JSX,
  type ParentComponent,
} from "solid-js";

import { describeConnection, useLive } from "~/api/live";
import { useSession } from "~/api/session";
import { Countdown } from "~/components/Time";
import { Chime } from "~/components/ui/Chime";
import { Button, cx } from "~/components/ui/primitives";
import { ShellBanner, SnoozeBanner, SourceReachBanner } from "~/components/ui/ShellBanner";
import {
  density,
  setDensity,
  setTheme,
  theme,
  themePreference,
  type ThemePreference,
} from "~/design/theme";

/* -------------------------------------------------------------------------- */
/* Connection                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * Honest connection state (§E.4).
 *
 * Note what colour it is *not*: connection health is a fact about the browser,
 * not the state of an alert, so it stays in Tier A. Spending a state hue here
 * would devalue the scarcity that makes a firing row loud (§M.2).
 */
const ConnectionBadge = (): JSX.Element => {
  const live = useLive();

  const dot = (): string => {
    switch (live.state()) {
      case "live":
        return "bg-accent";
      case "connecting":
        return "bg-ink-subtle motion-safe:animate-pulse";
      case "reconnecting":
      case "offline":
        return "bg-ink-subtle";
      default:
        return "bg-line-strong";
    }
  };

  const label = (): string => {
    switch (live.state()) {
      case "live":
        return "Live";
      case "connecting":
        return "Connecting";
      case "reconnecting":
        return "Stale";
      case "offline":
        return "Offline";
      default:
        return "Not connected";
    }
  };

  return (
    <div class="flex items-center gap-2">
      {/* One polite live region for the whole app: a connection change is worth
          announcing once, and never worth interrupting for. */}
      <span
        class={cx(
          "inline-flex items-center gap-1.5 rounded-control border px-1.5 py-0.5 text-meta",
          live.state() === "live"
            ? "border-line bg-surface text-ink-muted"
            : "border-line-strong bg-raised font-medium text-ink",
        )}
        title={describeConnection(live.state(), live.detail())}
      >
        <span aria-hidden="true" class={cx("size-1.5 shrink-0 rounded-full", dot())} />
        <span aria-live="polite" aria-atomic="true">
          {label()}
        </span>
        <Show when={live.state() === "reconnecting" && live.detail().retryAt !== null}>
          <span class="text-ink-subtle">
            · retry in <Countdown until={live.detail().retryAt} />
          </span>
        </Show>
      </span>

      <Show when={live.state() === "reconnecting" || live.state() === "offline"}>
        <Button size="sm" variant="ghost" onClick={() => live.reconnect()}>
          Reconnect
        </Button>
      </Show>
    </div>
  );
};

/**
 * The server told us our incremental state is untrustworthy. That is not a
 * cosmetic event — everything on screen may be wrong — so it gets a persistent
 * banner rather than a toast that can be missed.
 *
 * Exported for `ShellBanner.test.tsx` only. It is the one strip with no query
 * behind it — the stream itself is its input — so the only way to test it is to
 * mount it under a `LiveProvider` and be the server.
 */
export const ResyncBanner = (): JSX.Element => {
  const live = useLive();
  return (
    // `when` on the strip rather than a `<Show>` around it, so the polite region
    // is mounted from the first frame and the resync is a mutation *inside* a
    // region that already existed. A region that arrives already holding this
    // sentence is one screen readers commonly never speak, and this is the
    // sentence that says everything on screen may have been wrong.
    <ShellBanner
      when={live.detail().resyncReason !== null}
      onDismiss={() => live.acknowledgeResync()}
    >
      <span>
        oto could not keep this page's live updates in order
        {live.detail().resyncReason === "replay_window_exceeded"
          ? " — this tab was away longer than the 24-hour replay window"
          : " — the update buffer overflowed"}
        . The data below has been refetched.
      </span>
    </ShellBanner>
  );
};

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

/* -------------------------------------------------------------------------- */
/* Navigation                                                                 */
/* -------------------------------------------------------------------------- */

interface NavItem {
  readonly href: string;
  readonly label: string;
  /** Match this prefix for the active state, not just the exact path. */
  readonly prefix: string;
}

const NAV: readonly NavItem[] = [
  { href: "/alerts", label: "Alerts", prefix: "/alerts" },
  { href: "/groups", label: "Groups", prefix: "/groups" },
  { href: "/settings/sources", label: "Settings", prefix: "/settings" },
];

const Nav = (): JSX.Element => {
  const location = useLocation();
  const active = (prefix: string): boolean => location.pathname.startsWith(prefix);

  return (
    <nav aria-label="Primary" class="flex items-center gap-0.5">
      {NAV.map((item) => (
        <A
          href={item.href}
          aria-current={active(item.prefix) ? "page" : undefined}
          class={cx(
            "rounded-control px-2 py-1 text-item transition-colors duration-100",
            active(item.prefix)
              ? "bg-accent-fill font-medium text-ink"
              : "text-ink-muted hover:bg-raised hover:text-ink",
          )}
        >
          {item.label}
        </A>
      ))}
    </nav>
  );
};

/* -------------------------------------------------------------------------- */
/* The shell                                                                  */
/* -------------------------------------------------------------------------- */

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

export const AppShell: ParentComponent = (props) => {
  // Read once per render pass so the header does not thrash on every frame.
  const year = createMemo(() => new Date().getFullYear());

  return (
    /**
     * Exactly one viewport tall, and the body never scrolls. Every scrolling
     * region inside is therefore its own container — which is what the alert
     * table's virtualiser measures against, and what keeps a sticky table
     * header sticky to the table rather than to the document.
     */
    <div class="flex h-screen flex-col overflow-hidden bg-bg">
      {/* Keyboard users get out of the header in one tab, always. */}
      <a
        href="#main"
        class="sr-only-focusable absolute left-2 top-2 z-50 rounded-control border border-line-strong bg-surface px-3 py-1.5 text-item font-medium text-ink"
      >
        Skip to content
      </a>

      <header class="z-30 shrink-0 border-b border-line bg-surface">
        <div class="flex h-11 items-center gap-4 px-4">
          <A href="/alerts" class="flex shrink-0 items-center gap-1.5" aria-label="oto — home">
            {/* The chime — 音. The only piece of brand art in the chrome. */}
            <Chime size="mark" class="text-accent" />
            <span class="font-mono text-title font-bold tracking-tight text-ink">oto</span>
          </A>
          <Nav />
          <div class="flex-1" />
          <ConnectionBadge />
          <div class="h-4 w-px bg-line" aria-hidden="true" />
          <DensityToggle />
          <ThemeToggle />
          <SignOut />
        </div>
        {/* Ordered by how much of the screen each one calls into question: the
            live stream first, then whether oto can still see every source, then
            what oto is deliberately keeping quiet about. Each renders nothing
            visible when it has nothing to say, so the happy path is one flat
            header and the table below never shifts.

            All three keep a node mounted while silent so the strip's arrival is
            an update to something already there rather than a new element the
            reader never hears about — but only two of those nodes announce.
            ResyncBanner and SourceReachBanner keep a live REGION, because their
            text is static: it arrives once, is read once, and does not change
            again while it is up. SnoozeBanner keeps a SILENT node instead
            (`announce={false}`), because its rows carry `RelativeTime`
            countdowns, and a polite region wrapped around a ticking clock
            re-reads the whole banner every time the clock moves. See
            `ShellBanner.tsx` for the mounted-region argument at length. */}
        <ResyncBanner />
        <SourceReachBanner />
        <SnoozeBanner />
      </header>

      <main id="main" class="flex min-h-0 flex-1 flex-col overflow-hidden">
        {props.children}
      </main>

      <footer class="shrink-0 border-t border-line px-4 py-1.5 text-micro text-ink-subtle">
        oto · alert history for Prometheus · {year()} — oto records what your cluster reported.{" "}
        {/* vocab:allow — the footer states the scope boundary to the operator; it denies the concept it names. */}
        It does not page anyone and it does not know who is on call.
      </footer>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Glyphs                                                                     */
/* -------------------------------------------------------------------------- */

/* The chime lives in `~/components/ui/Chime` — one component, two sizes, and a
   per-size stroke that is optical compensation rather than an inconsistency. */

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
