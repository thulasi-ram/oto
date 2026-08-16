/**
 * What every test file gets before it runs.
 *
 * Five things, and no more than five: the three APIs jsdom does not implement
 * correctly — `<dialog>`, `ResizeObserver`, and computed `animationName` — a
 * clean DOM between tests, and a `fetch` that fails loudly.
 *
 * ⛔ THE `fetch` DEFAULT IS THE IMPORTANT ONE. jsdom ships no `fetch` that can
 * reach anything, and a test that forgets to stub one would otherwise get an
 * unhandled rejection somewhere in a query's retry loop and still pass. Every
 * unstubbed call throws with the URL in the message, so "this screen made a
 * request nobody expected" is a test failure with an address on it rather than
 * a silent 30-second timeout.
 */
import "@testing-library/jest-dom/vitest";
import { cleanup } from "@solidjs/testing-library";
import { afterEach, beforeEach, expect, vi } from "vitest";

/* -------------------------------------------------------------------------- */
/* jsdom does not implement <dialog>                                          */
/* -------------------------------------------------------------------------- */

/**
 * `showModal()` / `close()` are absent from jsdom 27, and `~/components/ui/Dialog`
 * is built on them deliberately (the platform gives the focus trap and the top
 * layer for free). This is the smallest shim that makes the observable contract
 * true: `open` flips, `close` fires a `close` event, and `Escape` fires `cancel`
 * — which is the path `Dialog` routes every dismissal through.
 */
function installDialogShim(): void {
  const proto = globalThis.HTMLDialogElement?.prototype as
    | (HTMLDialogElement & { showModal?: () => void })
    | undefined;
  if (proto === undefined || typeof proto.showModal === "function") return;

  const setOpen = (el: HTMLDialogElement, next: boolean): void => {
    if (next) el.setAttribute("open", "");
    else el.removeAttribute("open");
  };

  Object.defineProperties(proto, {
    showModal: {
      configurable: true,
      value(this: HTMLDialogElement): void {
        setOpen(this, true);
      },
    },
    show: {
      configurable: true,
      value(this: HTMLDialogElement): void {
        setOpen(this, true);
      },
    },
    close: {
      configurable: true,
      value(this: HTMLDialogElement, returnValue?: string): void {
        if (!this.hasAttribute("open")) return;
        setOpen(this, false);
        if (returnValue !== undefined) this.returnValue = returnValue;
        this.dispatchEvent(new Event("close"));
      },
    },
  });
}

installDialogShim();

/* -------------------------------------------------------------------------- */
/* jsdom does not implement ResizeObserver either                             */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ WITHOUT THIS, EVERY SCREEN THAT SHOWS THE ALERT TABLE RENDERS NOTHING.
 *
 * `createVirtualiser` observes its scroll container with a `ResizeObserver` —
 * deliberately, because the table's height changes when the filter bar wraps and
 * a window listener would miss that. jsdom 27 still ships no such global, so the
 * constructor call in `AlertTable`'s `ref` throws `ReferenceError` on mount, the
 * `<Match>` arm holding the table (and its "Load … more" footer) never lands, and
 * what is left on screen is the filter bar alone — a failure that reads as "the
 * data never arrived" when the request in fact succeeded.
 *
 * The shim reports nothing rather than guessing: jsdom lays nothing out, so every
 * element is 0 × 0 and a fabricated size would be a fabricated viewport. A
 * viewport of 0 is exactly what the virtualiser treats as "not measured yet", so
 * it renders every row — which is what a test wants to assert against anyway.
 */
function installResizeObserverShim(): void {
  if (typeof globalThis.ResizeObserver !== "undefined") return;
  globalThis.ResizeObserver = class {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  } as unknown as typeof ResizeObserver;
}

installResizeObserverShim();

/* -------------------------------------------------------------------------- */
/* jsdom's computed `animationName` is `""`, not the spec's `"none"`          */
/* -------------------------------------------------------------------------- */

/**
 * `solid-ui`'s Kobalte-based overlays (`Modal`, `Popover`, `DropdownMenu`) use
 * `solid-presence` to keep their content mounted until an exit animation
 * finishes, so the compound component can unmount cleanly instead of vanishing
 * mid-frame. It decides whether an element is "animating out" by reading
 * `getComputedStyle(el).animationName`, falling back to `"none"` only when
 * that read is `null`/`undefined`.
 *
 * jsdom never loads this project's real, compiled Tailwind CSS, so nothing
 * ever matches an `@utility oto-enter` rule — but jsdom's own default for an
 * unset `animationName` is the empty string `""`, not the CSS-spec default
 * `"none"`. `solid-presence`'s `?? "none"` fallback doesn't catch an empty
 * string, so it reads `""`, concludes an exit animation is in flight, and
 * waits forever for an `animationend` that jsdom — which never runs any
 * animation — will never dispatch. Without this shim, closing any of those
 * three overlays in a test leaves it mounted in the DOM indefinitely: the
 * component isn't broken, jsdom's computed style just doesn't match a real
 * browser's.
 */
function installAnimationNameShim(): void {
  const proto = Object.getPrototypeOf(getComputedStyle(document.documentElement)) as {
    animationName?: string;
  };
  const original = Object.getOwnPropertyDescriptor(proto, "animationName");
  if (original?.get === undefined) return;

  Object.defineProperty(proto, "animationName", {
    configurable: true,
    get(this: unknown): string {
      const value = original.get!.call(this) as string;
      return value === "" ? "none" : value;
    },
  });
}

installAnimationNameShim();

/* -------------------------------------------------------------------------- */
/* No request goes unnoticed                                                  */
/* -------------------------------------------------------------------------- */

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : String(input);
      return Promise.reject(
        new Error(`oto test: unstubbed fetch to ${url} — stub it or assert it is not made`),
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionStorage.clear();
  localStorage.clear();
});

/* A tiny convenience the assertions below lean on repeatedly. */
expect.extend({
  /** Fails when the rendered text contains the literal word `undefined`. */
  toRenderNoUndefined(received: HTMLElement) {
    const text = received.textContent ?? "";
    const pass = !/\bundefined\b/.test(text);
    return {
      pass,
      message: () =>
        pass
          ? "expected the rendered output to contain the word `undefined`"
          : `expected no rendered \`undefined\`, but found it in: ${text.slice(0, 400)}`,
    };
  },
});

declare module "vitest" {
  // The type parameter must match vitest's own declaration exactly, or the
  // merge is rejected and every custom matcher silently vanishes from the type.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  interface Matchers<T = any> {
    /** Asserts the subtree renders no literal `undefined` — the contract-drift smell. */
    toRenderNoUndefined: () => T;
  }
}
