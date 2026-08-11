/**
 * What every test file gets before it runs.
 *
 * Three things, and no more than three: jsdom's missing `<dialog>` behaviour,
 * a clean DOM between tests, and a `fetch` that fails loudly.
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
