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

import { RequireSession, SessionProvider } from "~/api/session";
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
function renderApp(at: string): void {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0, staleTime: 0 }, mutations: { retry: 0 } },
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
            </RequireSession>
          )}
        />
      </MemoryRouter>
    </QueryClientProvider>
  ));
}

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
