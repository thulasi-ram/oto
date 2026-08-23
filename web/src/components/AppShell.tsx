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
 * ride in the shell so they reach every route, not just the settings screen
 * nobody is on when it matters.
 *
 * # There is ONE NAVIGATION chrome, and it is the left rail (PORTING-SPEC §5,
 * # as amended by ADR 0033)
 *
 * The shell used to draw a top bar *and* leave the screens below to grow rails of
 * their own — settings had one, the alert list was growing one. Two chromes doing
 * one job was the most visible seam in the port: "where I can go" and "what I am
 * filtering" sat in different planes, and the second rail stole the width the
 * first one had already spent.
 *
 * So the bar is retired rather than joined. The rail holds, top to bottom: the
 * brand, the destinations, whatever the current screen contributes, and the two
 * standing facts (connection, identity). The aggregated banner strip moves to the
 * top of the content column, where the thing it qualifies actually is.
 *
 * ⭐ WHAT §5 ACTUALLY FORBIDS IS A SECOND *RAIL*, NOT A CONTROL IN THE COLUMN.
 * ADR 0033 moves the alert screen's filters out of the contextual zone and into a
 * toolbar above the table. That does not reopen the seam §5 closed: the rule was
 * ever only about a second vertical plane competing with this one for width and
 * for the answer to "where am I". A toolbar is neither — it sits *inside* the
 * content column, spans exactly the table it filters, and cannot be mistaken for
 * a place to go. What a screen may still contribute to the rail is what genuinely
 * reads as DESTINATIONS — settings' section list, and notifications' (ADR 0034).
 * Those no longer sit in a zone of their own behind a hairline: ADR 0034 nests
 * them inside the nav below, indented under the entry that owns them, so the rail
 * is one list at two depths rather than two lists at one. `/alerts` contributes
 * nothing and simply has no children under it.
 *
 * ⛔ THE `<header>` IS THE BRAND BLOCK, AND IT IS LOAD-BEARING FOR `App.test.tsx`.
 * That suite asserts shell survival by NODE IDENTITY — the same `<header>` and the
 * same `<main>` before and after a navigation — and screens draw `<header>`s of
 * their own (`routes/alerts.tsx` has one). The shell's must therefore precede them
 * in document order, which is what putting it inside the rail buys.
 */
import { A, useLocation } from "@solidjs/router";
import {
  For,
  Show,
  createMemo,
  type Accessor,
  type JSX,
  type ParentComponent,
} from "solid-js";

import { describeConnection, useLive } from "~/api/live";
import { createSidebarSlot, SidebarSlotProvider, SubNavLink } from "~/components/SidebarSlot";
import { Countdown } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { ShellBanner, SnoozeBanner, SourceReachBanner } from "~/components/ui/ShellBanner";
import { UserMenu } from "~/components/UserMenu";
import { IconMark } from "~/components/IconMark";
import { Wordmark } from "~/components/Wordmark";
import { cn } from "~/lib/cn";

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
        class={cn(
          "inline-flex items-center gap-1.5 rounded-control border px-1.5 py-0.5 text-meta",
          live.state() === "live"
            ? "border-line bg-surface text-ink-muted"
            : "border-line-strong bg-raised font-medium text-ink",
        )}
        title={describeConnection(live.state(), live.detail())}
      >
        <span aria-hidden="true" class={cn("size-1.5 shrink-0 rounded-full", dot())} />
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

/**
 * A heading over destinations, and **not a destination itself**.
 *
 * ⛔ IT IS DELIBERATELY NOT A LINK. "Alerts" the section and "Alerts" the screen
 * are two different things with one word, and the moment the heading is
 * clickable an operator has two rows that both claim to be the alert list —
 * which is the shape ADR 0034 already refused for `/notifications`, arrived at
 * from the other side. A heading that goes nowhere can never claim
 * `aria-current`, so the one-answer-to-"where am I" rule holds structurally
 * rather than by a condition somebody has to keep true.
 */
interface NavSection {
  readonly label: string;
  readonly children: readonly NavItem[];
}

type NavEntry = NavItem | NavSection;

const isSection = (entry: NavEntry): entry is NavSection => "children" in entry;

const NAV: readonly NavEntry[] = [
  {
    // ⭐ ONE SECTION, TWO OBJECTS, AND CASES COME FIRST. An Alert is an identity
    // — the label set, which outlives every one of its firings. A Case is one
    // firing episode of one Alert, and it is what a human acknowledges, so the
    // section opens on the thing there is something to *do* about and offers the
    // identities second. Leading with Alerts taught the eye that the identity
    // was the operational object, which is backwards: nobody acknowledges a
    // label set.
    label: "Alerts",
    children: [
      { href: "/cases", label: "Cases", prefix: "/cases" },
      { href: "/alerts", label: "Alerts", prefix: "/alerts" },
    ],
  },
  // ADR 0034. The BARE path, not `/notifications/policies` — the route redirects
  // to its first section, and an href the location never exactly equals is what
  // stops `<A>` from stamping its own `aria-current="page"` on a row whose child
  // is the real answer (see `App.tsx`, and the ⛔ on `Nav` below).
  { href: "/notifications", label: "Notifications", prefix: "/notifications" },
  { href: "/settings", label: "Settings", prefix: "/settings" },
];

/**
 * The places the product has — some of them grouped under a heading — and,
 * indented beneath whichever one you are in, the sections that place contributed.
 *
 * ⭐ THE RAIL NESTS TWICE OVER, AND BOTH NESTINGS ARE THE SAME IDEA. `NAV` may
 * carry a heading with destinations under it (Alerts → Cases, Alerts), and a
 * destination may carry the sections its screen contributed. Both are drawn with
 * `SubNavLink`, the component the screens already use, so "indented under" means
 * exactly one thing however it arose.
 *
 * ⭐ THE SECTIONS ARE INSIDE THIS NAV, NOT IN A ZONE UNDER IT. They used to sit
 * in a block of their own at the bottom of the rail behind a hairline, which
 * left Policies / Activity log floating unattached below four peer destinations:
 * nothing on screen said which of the four owned them, and they read as a second
 * list that merely happened to change when you navigated. Nested under their
 * parent, the containment *is* the layout, and the answer needs no hairline to
 * explain it.
 *
 * The 2px accent rail is drawn AT REST TOO, in `border-transparent`. A rail that
 * only exists on the active row would push that row's text two pixels right on
 * selection — the whole rail would shiver on every navigation. `SubNavLink` uses
 * the identical recipe one indent in, on purpose: these interleave now, and two
 * near-identical rows built two ways is exactly how a list starts to look wrong
 * without anyone being able to say why.
 *
 * ⛔ AN EXPANDED PARENT DOES NOT TAKE `aria-current="page"` AND DOES NOT TAKE THE
 * ACCENT. Its child does, and there is only one page. A parent claiming to be the
 * current page while the row under it also claims it announces two destinations
 * for one location; visually it would stack two accent rails, which is two
 * answers to "where am I" (§0.6 spends saturation on one thing at a time). The
 * parent still reads as where you are — `text-ink` and medium against three muted
 * siblings — it just stops being the *precise* answer when a finer one exists.
 */
const Nav = (props: {
  /**
   * The screen's section list, still UNCALLED.
   *
   * ⛔ THE ACCESSOR IS PASSED, NEVER THE RENDERED ELEMENT. Two places below need
   * to know whether sections exist — the parent row's styling and the `<Show>` —
   * and handing in `panel()()` would mean *building* the list to answer the
   * first question and building it again to answer the second. Asking whether
   * the source is null is free; calling it is not.
   */
  readonly panel: Accessor<(() => JSX.Element) | null>;
}): JSX.Element => {
  const location = useLocation();
  const active = (prefix: string): boolean => location.pathname.startsWith(prefix);
  /** Active AND the screen handed us sections to draw under it. */
  const expanded = (prefix: string): boolean => active(prefix) && props.panel() !== null;

  /**
   * One destination, at whichever depth it sits.
   *
   * A top-level entry keeps the row this rail has always drawn; one inside a
   * section reuses `SubNavLink`, which is the very component the screens'
   * own section lists use. That is the point: the rail is one list at two
   * depths, and a nested destination must not be a third way of drawing a row.
   */
  const Destination = (p: { readonly item: NavItem; readonly nested: boolean }): JSX.Element => (
    <>
      <Show
        when={p.nested}
        fallback={
          <A
            href={p.item.href}
            aria-current={
              active(p.item.prefix) && !expanded(p.item.prefix) ? "page" : undefined
            }
            class={cn(
              "flex h-9 shrink-0 items-center border-l-2 px-md text-item",
              "transition-colors duration-100",
              active(p.item.prefix)
                ? expanded(p.item.prefix)
                  ? "border-transparent font-medium text-ink"
                  : "border-accent bg-raised font-medium text-ink"
                : "border-transparent text-ink-muted hover:bg-raised hover:text-ink",
            )}
          >
            {p.item.label}
          </A>
        }
      >
        <SubNavLink
          href={p.item.href}
          current={active(p.item.prefix) && !expanded(p.item.prefix)}
        >
          {p.item.label}
        </SubNavLink>
      </Show>

      {/* Only the matching destination renders them, so the list appears
          exactly once however many entries `NAV` grows.

          A destination that is itself nested pushes its screen's sections one
          further step in, so three depths stay three depths: without the extra
          indent a section contributed by `/cases` would sit at exactly the
          indent of `/cases` itself and read as its sibling. */}
      <Show when={expanded(p.item.prefix) && props.panel()}>
        {(panel) => (
          <div class={cn("flex flex-col gap-2xs", p.nested && "pl-md")}>{panel()()}</div>
        )}
      </Show>
    </>
  );

  return (
    <nav aria-label="Primary" class="flex shrink-0 flex-col gap-2xs pb-sm">
      <For each={NAV}>
        {(entry) => (
          <Show
            when={isSection(entry) ? entry : null}
            fallback={<Destination item={entry as NavItem} nested={false} />}
          >
            {(section) => (
              <>
                {/* The heading keeps the parent row's geometry — same height,
                    same 2px rail drawn transparent at rest, same gutter — so the
                    list does not step sideways where a section begins. What it
                    does not keep is the interaction: it is a `<div>`, so there is
                    nothing to click and nothing that could claim to be the
                    current page. It reads as where you are the way an expanded
                    parent always has, by weight against its muted siblings. */}
                <div
                  class={cn(
                    "flex h-9 shrink-0 select-none items-center border-l-2 border-transparent px-md text-item",
                    section().children.some((c) => active(c.prefix))
                      ? "font-medium text-ink"
                      : "text-ink-muted",
                  )}
                >
                  {section().label}
                </div>
                <For each={section().children}>
                  {(child) => <Destination item={child} nested />}
                </For>
              </>
            )}
          </Show>
        )}
      </For>
    </nav>
  );
};

/* -------------------------------------------------------------------------- */
/* The shell                                                                  */
/* -------------------------------------------------------------------------- */

export const AppShell: ParentComponent = (props) => {
  // Read once per render pass so the chrome does not thrash on every frame.
  const year = createMemo(() => new Date().getFullYear());

  /**
   * The rail's contextual half, created exactly once.
   *
   * ⛔ IT ONLY GETS TO BE A SIGNAL BECAUSE THE SHELL IS A LAYOUT ROUTE. Screens
   * hand their sections here with `<SidebarPanel>` rather than rendering an
   * `<aside>` of their own, so there is one rail on screen at any time and the
   * shell — and the SSE stream above it — is untouched by the handover.
   */
  const slot = createSidebarSlot();

  return (
    /**
     * Exactly one viewport tall, and the body never scrolls. Every scrolling
     * region inside is therefore its own container — which is what the alert
     * table's virtualiser measures against, and what keeps a sticky table
     * header sticky to the table rather than to the document.
     *
     * The axis is now horizontal — rail beside content — and the `h-screen` /
     * `min-h-0` / `overflow-hidden` chain is carried down the content column
     * unchanged, so `<main>` still hands its child a definite height.
     *
     * ⛔ `relative` IS LOAD-BEARING, NOT DECORATION. `overflow-hidden` only
     * clips descendants whose containing block is inside the clipping box, and
     * an absolutely positioned element resolves its containing block against
     * the nearest POSITIONED ancestor. With none, that ancestor was the initial
     * containing block — so every `sr-only-focusable` span the screens render
     * (one per case row, per state chip, per table caption) was laid out
     * against the document rather than against the shell, sailed straight
     * through this `overflow-hidden`, and grew the document past one viewport.
     * The window then scrolled, and what it scrolled to was a dead band of page
     * background below the footer. Raising the banner stack pushed those spans
     * further down and made the band taller, which is why it read as a banner
     * bug. One positioned ancestor here closes the escape hatch for every
     * absolutely positioned descendant at once.
     */
    <div class="relative flex h-screen overflow-hidden bg-bg">
      {/* Keyboard users get out of the rail in one tab, always. */}
      <a
        href="#main"
        class="sr-only-focusable absolute left-2 top-2 z-50 rounded-control border border-line-strong bg-surface px-3 py-1.5 text-item font-medium text-ink"
      >
        Skip to content
      </a>

      {/* THE one rail. A screen that wants rail space hands its sections to the
          nav below through `<SidebarPanel>`; it never draws a second `<aside>`
          beside this one. */}
      <aside class="flex w-64 shrink-0 flex-col overflow-hidden border-r border-line bg-surface">
        {/* §2 puts the chrome band at `h-14`, and the brand keeps it so the rail's
            first row lines up with whatever the content column starts with. */}
        <header class="flex h-14 shrink-0 items-center px-md">
          <A href="/alerts" class="flex shrink-0 items-center gap-xs" aria-label="oto — home">
            {/* ⭐ THE HORIZONTAL LOCKUP: THE MARK, THEN THE SIGNATURE BESIDE IT.
                Both are cuts of the same drawing, taken from the brand sheet
                rather than assembled here — see `IconMark.tsx` on how the mark
                is lifted out of the composed logo's own mask.

                The stacked composition (`~/components/Logo`, ensō + bell +
                signature inside the ring) is the wrong shape for this band: it
                is square, so a header that is 56 px tall and 232 px wide can
                only ever render it small, and at that size the signature inside
                the ring stops resolving as writing. Split into two, the same
                drawing uses the width instead of fighting the height — the ring
                gets 36 px, which is enough for the bell inside it to read, and
                the signature sits beside it at 20 px, near the cap height of
                the nav below. The composed version keeps the login screen,
                where there is room for it to be square.

                `text-ink` for both, never the accent. §M.2 keeps saturation for
                state, and a brand mark in the accent hue at the top of every
                screen is the most persistent way there is to teach the eye that
                colour in oto does not mean anything (§0.6).

                The link's own `aria-label` is the accessible name; both marks
                are `aria-hidden`, so the product is named exactly once. */}
            <IconMark class="size-9 text-ink" />
            <Wordmark class="h-5 text-ink" />
          </A>
        </header>

        {/* The nav scrolls, and it scrolls independently of the content column:
            a screen with a tall section list must not be able to push the
            connection badge off the bottom, and must not drag the table beside
            it either. `flex-1` is unconditional so the footer stays pinned on a
            screen that contributes no sections at all (`/alerts`).

            The hairline that used to separate the sections from the nav is gone
            with the zone it separated. A rule between a parent and its own
            children would be arguing against the indent. */}
        <div class="min-h-0 flex-1 overflow-y-auto">
          <Nav panel={slot.panel} />
        </div>

        {/* The two standing facts, pinned to the bottom: whether what is on
            screen is actually live, and who oto thinks you are. Stacked rather
            than set side by side — at `w-64` the badge grows a Reconnect button
            when it has bad news, and a row that reflows on a disconnection is a
            row that moves the menu out from under the cursor. */}
        <footer class="flex shrink-0 flex-col items-start gap-sm border-t border-line px-md py-sm">
          <ConnectionBadge />
          <UserMenu />
        </footer>
      </aside>

      <div class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
        {/* The aggregated strip, at the top of the column whose contents it
            qualifies. It used to hang under the retired top bar; nothing about
            it changed in the move except its parent, which is deliberate — the
            three components are rendered here unconditionally, in one node that
            is built with the shell and never rebuilt, so every live region below
            is mounted from the first frame and stays the same node for the life
            of the session.

            Ordered by how much of the screen each one calls into question: the
            live stream first, then whether oto can still see every source, then
            what oto is deliberately keeping quiet about. Each renders nothing
            visible when it has nothing to say, so the happy path is a flat edge
            and the table below never shifts.

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
        <div class="shrink-0">
          <ResyncBanner />
          <SourceReachBanner />
          <SnoozeBanner />
        </div>

        {/* ⭐ THE PAGE GUTTER LIVES HERE AND NOWHERE ELSE (§2: `px-lg` → `px-xl`).
            This and the footnote below carry the identical `px-xl`, so whatever
            the screen puts at its own left edge and the footnote line up on one
            column — and a screen that wants symmetric gutters gets them by
            adding NOTHING of its own horizontally, rather than by pairing a left
            pad here with a right pad down there.

            ⛔ THE CLASS STRING IS THE VIRTUALISER'S CONTRACT. `min-h-0 flex-1`
            under a column that is itself `min-h-0` is what gives this element a
            definite height instead of one derived from its content, and
            `overflow-hidden` is what forces the scroll into the screen's own
            container where `readRowHeight()` can measure it. */}
        <main id="main" class="flex min-h-0 flex-1 flex-col overflow-hidden px-xl">
          {/* Screens reach the rail through here and nowhere else. The provider
              renders no node of its own, so `<main>`'s child is still the
              screen's own root and the height chain above is unbroken. */}
          <SidebarSlotProvider value={slot}>{props.children}</SidebarSlotProvider>
        </main>

        <footer class="shrink-0 border-t border-line px-xl py-xs text-micro text-ink-subtle">
          oto · alert history for Prometheus · {year()} — oto records what your cluster reported.{" "}
          {/* vocab:allow — the footer states the scope boundary to the operator; it denies the concept it names. */}
          It does not page anyone and it does not know who is on call.
        </footer>
      </div>
    </div>
  );
};
