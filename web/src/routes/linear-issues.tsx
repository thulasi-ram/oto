/**
 * `/proto/linear-issues` — an isolated Linear.app-grade issue list rendered in a
 * Bauhaus visual language. See `~/features/linear-proto/SPEC.md`; that file is
 * authoritative for every measurement and hue on this screen.
 *
 * Deliberately not linked from AppShell nav; reachable only by direct URL.
 * Never merge this palette with oto's own tokens (design/tokens.css), and never
 * let oto's own alert screens import from this feature.
 *
 * This module is the shell and nothing else. It owns exactly three things:
 * the store, the frame the three regions sit in, and the two shortcuts that
 * belong to no single region (⌘K and `[`). Every decision about what is on
 * screen lives in the store; every decision about how a region looks lives in
 * that region's own component.
 */
import { onCleanup, type Component, type JSX } from "solid-js";

import "~/features/linear-proto/linear-proto.css";
import { CommandPalette } from "~/features/linear-proto/CommandPalette";
import { IssueList, isOverlayOpen } from "~/features/linear-proto/IssueList";
import { MOCK_ISSUES } from "~/features/linear-proto/mockData";
import { SidePanel } from "~/features/linear-proto/SidePanel";
import { ViewHeader } from "~/features/linear-proto/ViewHeader";
import { createIssueViewStore, IssueViewProvider } from "~/features/linear-proto/store";
import type { Issue } from "~/features/linear-proto/types";

/**
 * The store's source accessor is called inside a memo, so it must hand back a
 * stable array rather than build one per read. `MOCK_ISSUES` is `readonly`;
 * this is the one copy that widens it, made once at module scope.
 */
const ISSUES: Issue[] = [...MOCK_ISSUES];

/** SPEC.md §6: shortcuts must never fire while the user is entering text. */
function isTypingContext(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
}

const LinearIssuesRoute: Component = (): JSX.Element => {
  const store = createIssueViewStore(() => ISSUES);

  /**
   * Only the two shortcuts with no owning region live here. Row-level keys
   * (↑/↓, x, p, s, a) belong to the list and are bound there, so this listener
   * must not claim them.
   */
  const onKeyDown = (event: KeyboardEvent): void => {
    // ⌘K/Ctrl+K is deliberately exempt from the typing guard: a modifier chord
    // cannot be mistaken for typed text, and exempting it is what lets the
    // palette's own search input toggle the palette back shut.
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      store.setPaletteOpen((open) => !open);
      return;
    }

    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (store.paletteOpen()) return;
    if (isTypingContext(event.target)) return;
    // An open Kobalte overlay (a row's quick-action menu, a filter popover) owns
    // the keyboard. Collapsing the panel out from under it is never what the
    // keypress meant. Same probe the list uses, so the two can never disagree.
    if (isOverlayOpen()) return;

    if (event.key === "[") {
      event.preventDefault();
      store.togglePanel();
    }
  };

  window.addEventListener("keydown", onKeyDown);
  onCleanup(() => window.removeEventListener("keydown", onKeyDown));

  return (
    <IssueViewProvider store={store}>
      <div class="linear-proto flex h-screen w-full overflow-hidden bg-[var(--lp-canvas)] text-[var(--lp-text)]">
        <SidePanel />

        {/* min-w-0 is load-bearing: without it the title column's `truncate`
            cannot shrink and long titles push the trailing columns off-screen. */}
        <div class="flex min-w-0 flex-1 flex-col">
          <ViewHeader />
          {/* Deliberately NOT a scroll container. `IssueList` owns the only
              scrollport on the screen; a second `overflow-y-auto` here would
              become the nearest scrollport for the list's sticky group headers
              while never itself overflowing, which kills their pinning. This
              div only hands the list a definite height to size against — the
              root is overflow-hidden so the page itself never scrolls. */}
          <div class="flex min-h-0 flex-1 flex-col">
            <IssueList />
          </div>
        </div>

        <CommandPalette />
      </div>
    </IssueViewProvider>
  );
};

export default LinearIssuesRoute;
