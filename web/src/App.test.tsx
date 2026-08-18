/**
 * The authenticated area is ONE place, and a navigation moves inside it.
 *
 * Every authenticated route used to construct its own `<Authenticated>` —
 * `component={() => <Authenticated>{<AlertsRoute />}</Authenticated>}`, five
 * times over. Solid reconciles nothing across sibling route components, so each
 * of the five was a separate tree and every nav click disposed the shell and
 * built a fresh one. Two things went with it, and only one of them is visible:
 *
 *   1. **The stream.** `LiveProvider` constructs its `AlertStream` per instance
 *      and `close()`s it from `onCleanup` — which aborts the in-flight request,
 *      clears the stall interval and drops the `online`/`offline`/
 *      `visibilitychange` listeners. The replacement provider constructs a NEW
 *      stream, restores the resume point out of `sessionStorage` in the
 *      constructor, and therefore opens in `reconnecting` rather than
 *      `connecting` — the word **Stale** in the header — while the server
 *      replays every frame past the resume point. One HTTP stream closed and
 *      another opened, per click, for no reason the operator asked for.
 *   2. **The shell's own state**, which is the harm the operator actually feels.
 *      `SnoozeBanner`'s dismissal set had to live at MODULE scope with an
 *      exported reset hook, purely so a dismissed strip would not re-open over
 *      the identical holds on the first nav click.
 *
 * So the property under test is not "the app renders". It is that the shell
 * survives a navigation, and the first two cases below assert it the only way
 * jsdom can: by NODE IDENTITY. `getByRole` and `getByText` pass under either
 * shape — a rebuilt header renders exactly the same markup — and what separates
 * them is whether the `<header>` on screen after the click is the one that was on
 * screen before it.
 *
 * The last two cases are the other half of the same claim, and they are here
 * because the comments in `App.tsx` lean on them hardest. A layout route that
 * never tore down would be as wrong as one that always did: `/` is OUTSIDE the
 * layout precisely so that a path nobody stays on opens no stream, and `/login`
 * is outside it so that signing out takes the shell and the connection with it.
 * Surviving a nav and surviving a sign-out are opposite requirements, and only
 * asserting both says the boundary is in the right place.
 *
 * ⛔ IT MOUNTS `routes` FROM `App.tsx`, NOT A COPY OF IT. A route tree declared
 * in this file would prove that a layout route works, which nobody doubts. The
 * claim worth defending is that oto's five authenticated paths are children of
 * one, and only the real definitions can carry it: re-wrapping any one route in
 * its own `<Authenticated>` has to fail here.
 *
 * What is NOT the real thing is the two shells around those routes, on purpose.
 * The browser `Router` is a `MemoryRouter` so the location is the test's to set,
 * and the app's `Root` is reduced to its `SessionProvider` because `Root`'s other
 * half is an `ErrorBoundary` — which would catch a throw from any screen below
 * and render a tidy error state, turning a real failure into a passing test.
 */
import { MemoryRouter, createMemoryHistory } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { describe, expect, it, vi } from "vitest";

import { routes } from "./App";
import { SessionProvider } from "~/api/session";
import type { ActiveSnooze } from "~/api/types";
import { frame, snooze, source, sse } from "~/test/fixtures";
import { flush, item, list, stubFetch, unpaged, until, type FetchStub } from "~/test/harness";

/* -------------------------------------------------------------------------- */
/* A world with a door, a shell and two screens behind it                     */
/* -------------------------------------------------------------------------- */

const ME = item({
  principal_kind: "user",
  user: {
    id: "11111111-1111-4111-8111-111111111111",
    email: "operator@example.test",
    display_name: "Operator",
  },
  org: { id: "22222222-2222-4222-8222-222222222222", slug: "acme", name: "Acme", settings: {} },
  search: { partial_match_enabled: false },
});

/** One hold, in the shape `GET /snoozes` serves it. */
function held(id: string, alertname: string): ActiveSnooze {
  return {
    ...snooze({ id }),
    snoozed_until: new Date(Date.now() + 3_600_000).toISOString(),
    alert_id: `alert-of-${id}`,
    alert_key: `key-of-${id}`,
    alert: {
      id: `alert-of-${id}`,
      alert_key: `key-of-${id}`,
      alertname,
      cluster_key: "prod-eu",
      state: "firing",
      ack_state: "unacked",
    },
    remaining_seconds: 3600,
  } as ActiveSnooze;
}

interface World {
  /** Every request the app made that was not the stream. */
  readonly http: FetchStub;
  /** How many times `/api/v1/stream` has been opened. The whole point. */
  readonly opened: () => number;
  /** Be the server on the connection opened `nth` (1-based). */
  readonly push: (nth: number, text: string) => void;
  /**
   * Whether connection `nth` (1-based) was aborted.
   *
   * `AlertStream` passes an `AbortSignal` into its `fetch` and `close()` fires
   * it, so this is the request-level view of `LiveProvider`'s `onCleanup` — the
   * difference between a provider that was disposed and one whose node merely
   * left the document with the socket still open.
   */
  readonly aborted: (nth: number) => boolean;
}

/**
 * Stub `fetch` for both transports at once.
 *
 * `/api/v1/stream` cannot go through the route table: it is not JSON and it must
 * never finish — a body that ends is a disconnection, and `AlertStream` would
 * schedule a retry and open a second connection this test would then blame on the
 * router. So it gets the `ReadableStream` shape `live.test.tsx` uses, held open
 * with its controller in hand, and everything else falls through to the real route
 * table, whose unrouted-call error is what makes "this screen asked for something
 * nobody expected" read as a finding.
 */
function world(opts: { readonly snoozes?: readonly ActiveSnooze[] } = {}): World {
  const http = stubFetch({
    "GET /api/v1/me": ME,

    // The shell.
    "GET /api/v1/sources": list([source()]),
    "GET /api/v1/snoozes": list(opts.snoozes ?? []),

    // `/alerts`, and everything its filter bar reaches for.
    "GET /api/v1/clusters": list([]),
    "GET /api/v1/labels": unpaged([]),
    "GET /api/v1/alerts": list([]),

    // `/cases`.
    "GET /api/v1/alert-groups": list([]),

    // `/notifications/policies`.
    "GET /api/v1/notification-policies": list([]),
    "GET /api/v1/channels": list([]),
  });

  const json = globalThis.fetch as typeof fetch;
  const wires: ((text: string) => void)[] = [];
  const signals: (AbortSignal | undefined)[] = [];

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === "string" ? input : String(input), "http://oto.test");
      if (url.pathname !== "/api/v1/stream") return json(input, init);

      const encoder = new TextEncoder();
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
        },
      });
      wires.push((t) => controller?.enqueue(encoder.encode(t)));
      signals.push(init?.signal ?? undefined);
      return Promise.resolve({ ok: true, status: 200, body });
    }),
  );

  return {
    http,
    opened: () => wires.length,
    push: (nth, text) => wires[nth - 1]?.(text),
    aborted: (nth) => signals[nth - 1]?.aborted === true,
  };
}

/** Mount the app's own route tree at `at`, and hand back the history to drive. */
function mount(at: string): ReturnType<typeof createMemoryHistory> {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0, staleTime: 0 },
      mutations: { retry: false },
    },
  });

  const history = createMemoryHistory();
  history.set({ value: at });

  render(() => (
    <QueryClientProvider client={client}>
      <MemoryRouter history={history} root={(p) => <SessionProvider>{p.children}</SessionProvider>}>
        {routes()}
      </MemoryRouter>
    </QueryClientProvider>
  ));

  return history;
}

/**
 * Navigate the way the header's own links do — through the router, not through a
 * remount. The history is driven directly rather than by clicking `<A>`: the
 * anchor path adds jsdom's delegated-click plumbing to the thing under test and
 * changes nothing about it, since a link click ends in exactly this call.
 */
async function navigate(
  history: ReturnType<typeof createMemoryHistory>,
  to: string,
  arrived: string,
): Promise<void> {
  history.set({ value: to });
  await until(() => expect(document.getElementById(arrived)).not.toBeNull());
  await flush();
}

/* `#alert-q` is the alert list's search box and `#case-q` is the case list's:
   one id each, present only while that screen is the outlet. */
const ALERTS = "alert-q";
const CASES = "case-q";

/* The door's email field: present only while `/login` is the whole page. */
const LOGIN = "login-email";

/* -------------------------------------------------------------------------- */

describe("the shell is a layout route, not five wrappers", () => {
  it("keeps the same shell and the same stream across a navigation", async () => {
    const w = world();
    const history = mount("/alerts");

    await until(() => expect(document.getElementById(ALERTS)).not.toBeNull());
    await until(() => expect(w.opened()).toBe(1));

    // Taken while `/alerts` is the outlet. Under the old shape these two nodes
    // are discarded by the click below and replaced by identical-looking ones.
    const header = document.querySelector("header");
    const main = document.getElementById("main");
    expect(header).not.toBeNull();
    expect(main).not.toBeNull();

    await navigate(history, "/cases", CASES);

    // The outlet really did change: this is a navigation, not a no-op.
    expect(document.getElementById(ALERTS)).toBeNull();

    // ⛔ THE ASSERTION. The same `<header>` and the same `<main>`, still in the
    // document — so `AppShell` was never torn down, and neither was the
    // `LiveProvider` above it or any state either of them holds.
    expect(document.querySelector("header")).toBe(header);
    expect(header!.isConnected).toBe(true);
    expect(document.getElementById("main")).toBe(main);

    // Standing, not frozen: the surviving header re-rendered its own active mark.
    expect(screen.getByRole("link", { name: "Cases" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Alerts" })).not.toHaveAttribute("aria-current");

    // ⛔ AND THE STREAM WAS NOT REOPENED. One connection, still the first one.
    expect(w.opened()).toBe(1);

    // Still a live connection rather than a surviving corpse: a frame pushed down
    // connection #1 must still reach the cache on the screen the nav arrived at.
    const before = w.http.to("/api/v1/alert-groups").length;
    w.push(1, sse(frame(1, "case.upserted", { id: "a-case" })));
    await until(() => expect(w.http.to("/api/v1/alert-groups").length).toBeGreaterThan(before));
    expect(w.opened()).toBe(1);
  });

  it("leaves a dismissed strip dismissed, which is what the module-state hack bought", async () => {
    // ⛔ THIS IS THE OPERATOR-FACING HALF, AND THE REASON `dismissedSnoozes` CAN
    // BE A COMPONENT SIGNAL AGAIN. The read set used to sit at module scope with
    // an exported `resetDismissedSnoozes` so one test's Dismiss could not silence
    // the next — both of which existed only because the shell was rebuilt per
    // route. Component state now survives the navigation because the component
    // does, and this case is what says so: move the signal back out and it still
    // passes, but put the five `<Authenticated>` wrappers back and it fails.
    const w = world({ snoozes: [held("s1", "HighErrorRate")] });
    const history = mount("/alerts");

    await until(() => expect(document.body.textContent).toContain("holding notifications on"));
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await flush();
    expect(document.body.textContent).not.toContain("holding notifications on");

    await navigate(history, "/cases", CASES);

    // A hold the operator has already read is not news on the next screen.
    expect(document.body.textContent).not.toContain("holding notifications on");
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
    expect(w.opened()).toBe(1);
  });

  it("sends / to /alerts, and opens no stream on the way through", async () => {
    const w = world();
    const history = mount("/");

    await until(() => expect(document.getElementById(ALERTS)).not.toBeNull());
    await flush();

    // The redirect happened, and it replaced the location rather than layering
    // a second entry the Back button would fall into.
    expect(history.get()).toBe("/alerts");

    // ⛔ THE ASSERTION, AND THE REASON `/` IS A LEAF OUTSIDE THE LAYOUT. If the
    // redirect lived inside `Authenticated`, landing on `/` would mount the
    // shell, open a stream and dispose both one tick later on the way to
    // `/alerts` — the remount bug in miniature, and invisible to any check that
    // only looks at what is on screen when the dust settles. ONE connection,
    // opened by `/alerts`, is what says the pass-through cost nothing.
    expect(w.opened()).toBe(1);
  });

  /**
   * ⛔ `/groups` IS A DELIVERED LINK, NOT A LEGACY PATH SOMEBODY FORGOT TO DELETE.
   * `internal/notification/service/view.go` mints `baseURL + "/cases/" + id` now,
   * but it minted `"/groups/" + id` into every Slack card and webhook payload oto
   * sent before that, so a year of chat history points here. Renaming the screen to
   * `/cases` is a vocabulary decision; letting those links 404 would be a data-loss
   * one, and the difference is entirely these two routes.
   */
  it("still answers the /groups links already sitting in Slack, and carries the id across", async () => {
    const w = world();
    const history = mount("/groups");

    await until(() => expect(document.getElementById(CASES)).not.toBeNull());
    // Replaced, not pushed: an operator arriving from a Slack card and pressing
    // Back should land back in Slack, not on a URL that bounces them forward.
    expect(history.get()).toBe("/cases");

    const id = "0f1e2d3c-4b5a-4697-8899-aabbccddeeff";
    // ⛔ THE ID SURVIVES. A redirect that dropped it would land the operator on
    // the case LIST — a screen that looks like it worked, from a card that was
    // about one specific case.
    history.set({ value: `/groups/${id}` });
    await until(() => expect(history.get()).toBe(`/cases/${id}`));

    expect(w.opened()).toBe(1);
  });

  /**
   * ⛔ THE CARD'S SECOND BUTTON IS A SUB-PATH, AND IT HAS ALWAYS BEEN ONE.
   * `view.go` builds Timeline as `links.group + "/timeline"`, so the moment the
   * base became `/cases/` the deep link became `/cases/<id>/timeline` — and
   * `/cases/:id` is a leaf, so without its own wildcard that link resolves to the
   * not-found sentence. A Slack button that lands on "That page does not exist" is
   * the same defect as one that 404s, arrived at from the other direction.
   */
  it("lands a deep link one segment past a case on the case itself", async () => {
    const w = world();
    const id = "0f1e2d3c-4b5a-4697-8899-aabbccddeeff";
    const history = mount(`/cases/${id}/timeline`);

    await until(() => expect(history.get()).toBe(`/cases/${id}`));
    // The not-found sentence is what this route exists to keep off the screen.
    expect(document.body.textContent ?? "").not.toContain("That page does not exist.");

    expect(w.opened()).toBe(1);
  });

  it("takes the shell and the stream down on the way out to /login", async () => {
    const w = world();
    const history = mount("/alerts");

    await until(() => expect(document.getElementById(ALERTS)).not.toBeNull());
    await until(() => expect(w.opened()).toBe(1));

    const header = document.querySelector("header");
    expect(header).not.toBeNull();

    // Exactly what `SignOut` does on success: `navigate("/login", { replace: true })`.
    await navigate(history, "/login", LOGIN);

    // ⛔ THE ASSERTION, AND IT IS THE OPPOSITE OF THE ONE ABOVE. `/login` is
    // outside the layout route, so this navigation must NOT be reconciled into
    // the surviving shell: the header is gone from the document and the node
    // itself is detached, which is `AppShell` disposed rather than merely
    // hidden. A sign-out that left the shell standing would leave the previous
    // operator's alert rows on screen behind the door.
    expect(document.querySelector("header")).toBeNull();
    expect(header!.isConnected).toBe(false);

    // And `LiveProvider`'s `onCleanup` ran: the connection the shell opened is
    // aborted, not left draining into a provider nobody can read. A stream that
    // outlived the sign-out would keep an authenticated request in flight for a
    // session the operator has just revoked.
    expect(w.aborted(1)).toBe(true);
    expect(w.opened()).toBe(1);
  });
});

/* -------------------------------------------------------------------------- */

/**
 * A screen's sections belong to the destination above them, and the rail has to
 * say so structurally rather than by being read top to bottom.
 *
 * ⛔ THE FAILURE THIS GUARDS IS INVISIBLE TO A SCREENSHOT DIFF. The sections used
 * to render in a block of their own at the foot of the rail, under a hairline:
 * every link was present, every link worked, and nothing on screen — or in the
 * accessibility tree — said which of the four destinations owned them. They read
 * as a second, peer-level list that happened to change when you navigated. So
 * the assertions below are about CONTAINMENT and about which node claims to be
 * the current page, neither of which a "the link is on screen" check can see.
 */
describe("a screen's sections hang under the destination that owns them", () => {
  it("puts them inside the primary nav, under their parent", async () => {
    const w = world();
    mount("/notifications/policies");

    const nav = (): HTMLElement | null => document.querySelector('nav[aria-label="Primary"]');
    await until(() =>
      expect(screen.getByRole("link", { name: "Activity log" })).toBeTruthy(),
    );

    // ⛔ INSIDE the one navigation landmark, not beside it. `contains` is the
    // whole assertion: the old shape put these in a sibling `<nav>` further down
    // the rail, which passes any check that merely finds the link.
    for (const label of ["Policies", "Activity log"]) {
      expect(
        nav()?.contains(screen.getByRole("link", { name: label })),
        `\`${label}\` is not inside the primary nav — it is a detached list again`,
      ).toBe(true);
    }

    // And exactly one navigation landmark: the section list contributes no
    // `<nav>` of its own, or a screen reader would announce these links twice —
    // once as "Primary", once as whatever the nested region called itself.
    expect(document.querySelectorAll("nav").length).toBe(1);

    // ⛔ ONE PAGE, ONE `aria-current`. The parent is where you are; the child is
    // the precise answer, and only the precise answer claims the attribute.
    expect(screen.getByRole("link", { name: "Policies" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Notifications" })).not.toHaveAttribute(
      "aria-current",
    );
    expect(w.opened()).toBe(1);
  });

  it("withdraws them when the screen that contributed them leaves", async () => {
    world();
    const history = mount("/notifications/policies");

    await until(() => expect(screen.getByRole("link", { name: "Activity log" })).toBeTruthy());
    await navigate(history, "/alerts", ALERTS);

    // `/alerts` contributes nothing, so the rail is four destinations and no
    // children — and "Notifications" gets its own accent mark back the moment it
    // has no child to hand it to (asserted through `aria-current`, which is the
    // same decision expressed where a test can see it).
    expect(screen.queryByRole("link", { name: "Activity log" })).toBeNull();
    expect(screen.getByRole("link", { name: "Alerts" })).toHaveAttribute("aria-current", "page");
  });
});
