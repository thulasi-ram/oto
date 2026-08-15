/**
 * The door, and the four promises it makes.
 *
 * Every case here is a clause of the defect this screen closes: an
 * unauthenticated visitor used to reach `/alerts`, watch every request answer
 * 401, and sit in front of skeleton rows forever with no control anywhere that
 * led to signing in. The server was complete the whole time; the product had no
 * door.
 *
 * `fetch` is stubbed at the transport, so the real `~/api/client` runs — its
 * `Problem` decoding, its `credentials: "include"`, and the 401 publication that
 * `session.tsx` listens to are all under test rather than mocked away.
 */
import { MemoryRouter, Route, createMemoryHistory } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Component } from "solid-js";

import { RequireSession, SessionProvider, useSession, type Session } from "~/api/session";
import LoginRoute from "~/routes/login";
import { item, problem, stubFetch, until, type FetchStub } from "~/test/harness";

/* -------------------------------------------------------------------------- */
/* A two-route world: the door, and one room behind it                        */
/* -------------------------------------------------------------------------- */

const ME = item({
  principal_kind: "user",
  user: {
    id: "11111111-1111-4111-8111-111111111111",
    email: "operator@example.test",
    display_name: "Operator",
  },
  org: { id: "22222222-2222-4222-8222-222222222222", slug: "acme", name: "Acme", settings: {} },
});

/**
 * Render the smallest app that can express the defect: a public `/login` and a
 * guarded room. The room's body is a sentinel string, because "did the guard let
 * them in" is the only thing these tests need to see.
 */
// ⛔ `gcTime` IS A PARAMETER BECAUSE THE DEFAULT OF 0 HID A REAL BUG. A client
// that evicts the instant a query unmounts cannot demonstrate a cache surviving a
// sign-out — which is the one property that made the cross-account leak possible.
// The app's real client is module-scoped with a five minute gcTime.
function renderApp(at: string, opts: { readonly gcTime?: number } = {}): QueryClient {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: opts.gcTime ?? 0, staleTime: 0 },
      mutations: { retry: 0 },
    },
  });
  const history = createMemoryHistory();
  history.set({ value: at });

  render(() => (
    <QueryClientProvider client={client}>
      <MemoryRouter history={history} root={(p) => <SessionProvider>{p.children}</SessionProvider>}>
        <Route path="/login" component={LoginRoute} />
        <Route
          path="/alerts"
          component={() => (
            <RequireSession>
              <p>the alert list</p>
              <SessionProbe />
            </RequireSession>
          )}
        />
      </MemoryRouter>
    </QueryClientProvider>
  ));
  return client;
}

// A handle on the session from inside the guarded tree, so a test can drive the
// real `signOut` rather than a reimplementation of it.
let probed: Session | undefined;
const SessionProbe: Component = () => {
  probed = useSession();
  return null;
};

/** Fill both fields and submit, the way an operator does. */
async function fillAndSubmit(email: string, password: string): Promise<void> {
  await until(() => expect(screen.getByLabelText(/email/i)).toBeTruthy());
  fireEvent.input(screen.getByLabelText(/email/i), { target: { value: email } });
  fireEvent.input(screen.getByLabelText(/password/i), { target: { value: password } });
  fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

describe("the login screen", () => {
  let http: FetchStub;

  beforeEach(() => {
    vi.unstubAllGlobals();
    http = stubFetch({});
  });

  it("sends an unauthenticated visitor to the door instead of an endless skeleton", async () => {
    // The whole defect in one stub: /me says nobody.
    http.on("GET /api/v1/me", problem(401, "unauthenticated"));

    renderApp("/alerts");

    await until(() => expect(screen.getByRole("heading", { name: /sign in to oto/i })).toBeTruthy());
    // And critically, the guarded screen never mounted, so it never fired the
    // request that used to 401 and paint skeleton rows behind "Stale, retry in 2s".
    expect(screen.queryByText("the alert list")).toBeNull();
  });

  it("lets a signed-in visitor straight through", async () => {
    http.on("GET /api/v1/me", ME);

    renderApp("/alerts");

    await until(() => expect(screen.getByText("the alert list")).toBeTruthy());
  });

  it("posts the credentials and lands on the alert list", async () => {
    http.on("GET /api/v1/me", problem(401, "unauthenticated"));
    http.on("POST /api/v1/auth/login", ME);

    renderApp("/login");
    await fillAndSubmit("operator@example.test", "correct-horse-battery-staple");

    await until(() => expect(screen.getByText("the alert list")).toBeTruthy());

    const posted = http.to("/auth/login");
    expect(posted).toHaveLength(1);
    expect(posted[0]?.body).toEqual({
      email: "operator@example.test",
      password: "correct-horse-battery-staple",
    });
    // The principal came back with the login, so nothing re-asks who just signed in.
    expect(http.to("/api/v1/me")).toHaveLength(1);
  });

  it("says something honest and unspecific when the details are refused", async () => {
    http.on("GET /api/v1/me", problem(401, "unauthenticated"));
    http.on("POST /api/v1/auth/login", problem(401, "unauthenticated"));

    renderApp("/login");
    await fillAndSubmit("nobody@example.test", "wrong");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/not accepted/i);
    // ⛔ The contract promises a 401 that does not reveal whether the account
    // exists. A form that said "no such user" would hand enumeration back.
    expect(alert.textContent).not.toMatch(/exist|unknown user|no such/i);
    // And a refused login must NOT read as "your session ended" — the visitor
    // stays on the form they are trying to use.
    expect(screen.getByRole("heading", { name: /sign in to oto/i })).toBeTruthy();
  });

  it("renders the rate limiter rather than failing blankly", async () => {
    http.on("GET /api/v1/me", problem(401, "unauthenticated"));
    http.on(
      "POST /api/v1/auth/login",
      problem(429, "rate_limited", { retry_after_seconds: 30 } as never),
    );

    renderApp("/login");
    await fillAndSubmit("operator@example.test", "again");

    const alert = await screen.findByRole("alert");
    // The limiter is real and tested server-side. Telling the operator to keep
    // trying would spend a budget that is already counting against them.
    expect(alert.textContent).toMatch(/too many attempts/i);
    expect(alert.textContent).toMatch(/30s/);
  });

  // ⛔ THESE TWO WERE FOUND BY A RED TEAM, NOT BY THIS FILE, and the reason is
  // worth keeping: every test above drives the app from OUTSIDE the session —
  // boot, login, refusal, expiry. None of them signed out. The one action a
  // signed-in operator takes to protect themselves was the one path with no
  // coverage at all, and it had two security defects in it.

  it("evicts the previous account's cached data on sign out", async () => {
    http.on("GET /api/v1/me", ME);
    http.on("POST /api/v1/auth/logout", { status: 204 });

    // A realistic cache, not the gcTime: 0 that made this invisible.
    const client = renderApp("/alerts", { gcTime: 5 * 60_000 });
    await until(() => expect(screen.getByText("the alert list")).toBeTruthy());

    // Stand in for a loaded screen: a key with no org or user in it, which is
    // every key this app uses.
    client.setQueryData(["alerts", "list", {}], { data: [{ id: "org-a-secret" }] });
    expect(client.getQueryData(["alerts", "list", {}])).toBeTruthy();

    await probed!.signOut();

    expect(client.getQueryData(["alerts", "list", {}])).toBeUndefined();
    expect(client.getQueryCache().getAll()).toHaveLength(0);
    // invalidateQueries would leave the payload in place and merely mark it
    // stale — and stale data still renders synchronously to the next account.
  });

  it("keeps the operator signed in when the revoke fails", async () => {
    http.on("GET /api/v1/me", ME);
    http.on("POST /api/v1/auth/logout", problem(503, "unavailable"));

    renderApp("/alerts");
    await until(() => expect(screen.getByText("the alert list")).toBeTruthy());

    // The revoke and the Set-Cookie clear are ONE response. A failed logout
    // means the cookie is still in the jar and still resolves, so reporting
    // success would send the operator away while the session is live — and the
    // next person to press F5 lands inside their account.
    await expect(probed!.signOut()).rejects.toBeTruthy();

    expect(probed!.me()).not.toBeNull();
    expect(screen.getByText("the alert list")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: /sign in to oto/i })).toBeNull();
  });

  it("returns a visitor to the door when the session dies mid-visit", async () => {
    http.on("GET /api/v1/me", ME);

    renderApp("/alerts");
    await until(() => expect(screen.getByText("the alert list")).toBeTruthy());

    // A later request answers 401 — an expired session, or a logout in another
    // tab. `client.ts` publishes it and the guard reacts.
    const { getItem } = await import("~/api/client");
    http.on("GET /api/v1/alerts", problem(401, "unauthenticated"));
    await getItem("/api/v1/alerts").catch(() => undefined);

    await until(() => expect(screen.getByRole("heading", { name: /sign in to oto/i })).toBeTruthy());
  });
});
