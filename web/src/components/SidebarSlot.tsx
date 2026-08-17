/**
 * The contextual half of the shell's left rail.
 *
 * `AppShell` owns the rail and renders the high-level destinations (Alerts,
 * Groups, Settings). Everything *below* that is contributed by whichever screen
 * is mounted — the alert list's filter/group/sort sections, settings' section
 * list, and so on.
 *
 * The screen cannot reach into the shell directly: `AppShell` is a layout route
 * that must never remount (remounting it tears down the SSE stream), so the rail
 * is rendered once and screens hand it content instead. A screen renders
 * `<SidebarPanel>` anywhere in its own tree; the rail displays it, and it is
 * withdrawn automatically when the screen unmounts.
 *
 * Deliberately a signal rather than a `<Portal>`: a portal needs its mount node
 * to already exist, which couples every screen to the shell's DOM order and
 * breaks the moment the rail is conditionally rendered (collapsed, mobile).
 */
import {
  createContext,
  createSignal,
  onCleanup,
  useContext,
  type Accessor,
  type JSX,
} from "solid-js";

type PanelSource = () => JSX.Element;

interface SidebarSlotApi {
  /** What the rail should currently show beneath the nav, if anything. */
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
 * Render this anywhere inside a screen to fill the rail's contextual zone.
 * It renders nothing where it sits — the shell decides where it appears.
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
