/**
 * A fixed-height row virtualiser, in about forty lines.
 *
 * The alert list pages at up to 200 rows and appends, so an operator working a
 * storm can accumulate thousands. Rows are already exactly `--oto-row-h` (U6
 * makes row height a token, not a guess), which means the cheap, exact
 * virtualisation strategy is available and the expensive measuring kind is not
 * needed.
 *
 * It deliberately does nothing until the list is big enough to matter:
 * virtualising forty rows costs more than it saves and breaks in-page find.
 */
import { createSignal, onCleanup, type Accessor } from "solid-js";

/** Below this, render everything. Ctrl-F should work on a normal-sized list. */
export const VIRTUALISE_ABOVE = 150;

/** Extra rows above and below the viewport, so a fast scroll never shows a gap. */
const OVERSCAN = 12;

export interface VirtualWindow {
  readonly start: number;
  readonly end: number;
  readonly padTop: number;
  readonly padBottom: number;
  readonly virtualised: boolean;
}

export interface VirtualOptions {
  readonly count: Accessor<number>;
  readonly rowHeight: Accessor<number>;
}

export interface Virtualiser {
  readonly window: Accessor<VirtualWindow>;
  /** Attach to the scrolling element. */
  readonly attach: (el: HTMLElement) => void;
}

export function createVirtualiser(opts: VirtualOptions): Virtualiser {
  const [scrollTop, setScrollTop] = createSignal(0);
  const [viewport, setViewport] = createSignal(0);

  const attach = (el: HTMLElement): void => {
    const onScroll = (): void => {
      setScrollTop(el.scrollTop);
    };
    el.addEventListener("scroll", onScroll, { passive: true });

    // ResizeObserver rather than a window listener: the table's height changes
    // when the filter bar wraps, and window resize would miss that entirely.
    const ro = new ResizeObserver(() => setViewport(el.clientHeight));
    ro.observe(el);
    setViewport(el.clientHeight);

    onCleanup(() => {
      el.removeEventListener("scroll", onScroll);
      ro.disconnect();
    });
  };

  const window = (): VirtualWindow => {
    const count = opts.count();
    const h = Math.max(1, opts.rowHeight());

    if (count <= VIRTUALISE_ABOVE || viewport() === 0) {
      return { start: 0, end: count, padTop: 0, padBottom: 0, virtualised: false };
    }

    const first = Math.max(0, Math.floor(scrollTop() / h) - OVERSCAN);
    const visible = Math.ceil(viewport() / h) + OVERSCAN * 2;
    const last = Math.min(count, first + visible);

    return {
      start: first,
      end: last,
      padTop: first * h,
      padBottom: (count - last) * h,
      virtualised: true,
    };
  };

  return { window, attach };
}

/** Read the current `--oto-row-h`, so JS and CSS can never disagree about it. */
export function readRowHeight(): number {
  if (typeof globalThis.document === "undefined") return 36;
  const raw = getComputedStyle(document.documentElement).getPropertyValue("--oto-row-h");
  const n = Number.parseFloat(raw);
  return Number.isFinite(n) && n > 0 ? n : 36;
}
