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
import { A, useLocation } from "@solidjs/router";
import { Show, createMemo, type JSX, type ParentComponent } from "solid-js";

import { describeConnection, useLive } from "~/api/live";
import { Countdown } from "~/components/Time";
import { Chime } from "~/components/ui/Chime";
import { Button, cx } from "~/components/ui/primitives";
import { ShellBanner, SnoozeBanner, SourceReachBanner } from "~/components/ui/ShellBanner";
import { UserMenu } from "~/components/UserMenu";

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
  { href: "/settings/sources", label: "Admin Settings", prefix: "/settings" },
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
          <UserMenu />
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
