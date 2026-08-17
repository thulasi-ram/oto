/**
 * A screen's own sections, rendered **inside** the shell's primary nav.
 *
 * ⭐ THEY HANG UNDER THEIR PARENT DESTINATION, NOT IN A ZONE OF THEIR OWN. This
 * used to be a separate block at the bottom of the rail, under a hairline: the
 * rail read Alerts / Groups / Notifications / Settings, then a rule, then
 * Policies / Activity log floating unattached. Nothing on screen said which of
 * the four those two belonged to — they read as a second, peer-level list that
 * happened to change when you navigated. Indented directly beneath the
 * destination that owns them, the answer is the layout.
 *
 * The screen cannot reach into the shell directly: `AppShell` is a layout route
 * that must never remount (remounting it tears down the SSE stream), so the rail
 * is rendered once and screens hand it content instead. A screen renders
 * `<SidebarPanel>` anywhere in its own tree; the nav places it under whichever
 * `NAV` entry matches the current path, and it is withdrawn automatically when
 * the screen unmounts.
 *
 * Deliberately a signal rather than a `<Portal>`: a portal needs its mount node
 * to already exist, which couples every screen to the shell's DOM order and
 * breaks the moment the rail is conditionally rendered (collapsed, mobile).
 */
import { A } from "@solidjs/router";
import {
  createContext,
  createSignal,
  onCleanup,
  useContext,
  type Accessor,
  type JSX,
} from "solid-js";

import { cn } from "~/lib/cn";

type PanelSource = () => JSX.Element;

interface SidebarSlotApi {
  /** What the nav should currently show under the active destination, if anything. */
  panel: Accessor<PanelSource | null>;
  /** Screens do not call this — `<SidebarPanel>` does. */
  provide: (source: PanelSource) => void;
  withdraw: (source: PanelSource) => void;
}

const SidebarSlotContext = createContext<SidebarSlotApi>();

/**
 * Created once, by `AppShell`. Survives navigation because the shell does.
 */
export function createSidebarSlot(): SidebarSlotApi {
  const [panel, setPanel] = createSignal<PanelSource | null>(null);

  return {
    panel,
    provide(source) {
      // Store the function itself, not its result: `setSignal(fn)` would treat
      // it as an updater. The extra arrow keeps it a value.
      setPanel(() => source);
    },
    withdraw(source) {
      // Only clear if this screen is still the occupant. During a route change
      // the incoming screen mounts before the outgoing one cleans up, so an
      // unguarded clear would blank the rail the new screen just filled.
      setPanel((current) => (current === source ? null : current));
    },
  };
}

export function SidebarSlotProvider(props: {
  value: SidebarSlotApi;
  children: JSX.Element;
}): JSX.Element {
  return (
    <SidebarSlotContext.Provider value={props.value}>
      {props.children}
    </SidebarSlotContext.Provider>
  );
}

/**
 * Render this anywhere inside a screen to hand the rail its section list.
 * It renders nothing where it sits — the shell decides where it appears.
 *
 * ⛔ WHAT GOES IN IS A LIST OF `<SubNavLink>`s AND NOT A `<nav>`. It is placed
 * inside `AppShell`'s own `<nav aria-label="Primary">` now, so a screen wrapping
 * its sections in a second landmark would nest one navigation region inside
 * another and announce the same links twice over.
 *
 * Outside the authenticated shell (login, the `/proto/*` design routes) there is
 * no rail, so this is a no-op rather than an error.
 */
export function SidebarPanel(props: { children: JSX.Element }): JSX.Element {
  const slot = useContext(SidebarSlotContext);
  if (!slot) return null;

  const source: PanelSource = () => props.children;
  slot.provide(source);
  onCleanup(() => slot.withdraw(source));

  return null;
}

/** For `AppShell` only, to read what the current screen contributed. */
export function useSidebarSlot(): SidebarSlotApi | undefined {
  return useContext(SidebarSlotContext);
}

/**
 * One section, drawn as a child of the destination above it.
 *
 * ⭐ THE INDENT IS THE WHOLE POINT AND IT IS `pl-xl`, NOT A NUDGE. A parent row
 * puts its text at 14px (`border-l-2` + `px-md`); a child at 26px is far enough
 * to read as *contained by* the row above rather than as a row that merely lines
 * up badly with it. The 2px rail is drawn at rest in `border-transparent` for
 * the same reason the parent rows draw theirs — selecting a section must not
 * shift its own text two pixels sideways.
 *
 * ⛔ THE CHILD CARRIES THE ONE ACCENT MARK, AND THE PARENT GIVES IT UP. When a
 * destination is expanded, `AppShell` renders it with weight alone: two accent
 * rails stacked in one column would be two answers to "where am I", and §0.6
 * spends saturation on exactly one thing at a time. The parent still reads as
 * current — it is `text-ink` and medium against four muted siblings — while the
 * accent says which section of it you are actually on.
 *
 * It lives here rather than in either route because both routes draw this list,
 * they sit in the same pixels on different paths, and two copies of a class
 * string is how they would come to disagree. That has already happened once in
 * this app (`rhythm.ts`'s `LABEL`).
 */
export function SubNavLink(props: {
  readonly href: string;
  readonly current: boolean;
  readonly children: JSX.Element;
}): JSX.Element {
  return (
    <A
      href={props.href}
      aria-current={props.current ? "page" : undefined}
      class={cn(
        "flex h-8 shrink-0 items-center border-l-2 py-0 pl-xl pr-md text-item",
        "transition-colors duration-100",
        props.current
          ? "border-accent bg-raised font-medium text-ink"
          : "border-transparent text-ink-muted hover:bg-raised hover:text-ink",
      )}
    >
      {props.children}
    </A>
  );
}
