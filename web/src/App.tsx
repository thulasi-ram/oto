import { Navigate, Route, Router } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { ErrorBoundary, lazy, onMount, type Component, type JSX } from "solid-js";

import { LiveProvider } from "~/api/live";
import { RequireSession, SessionProvider } from "~/api/session";
import { AppShell } from "~/components/AppShell";
import { ErrorState } from "~/components/ui/states";
import { installThemeEffect } from "~/design/theme";

/**
 * Cache policy.
 *
 * `staleTime` is short because this is an operational surface and a stale row
 * is a wrong row. It is not zero, because the live stream already invalidates
 * on change (§E.4) and refetching on every mount as well would double the
 * traffic for no extra truth.
 *
 * Retries are off for mutations. A retried acknowledgement is only safe with an
 * idempotency key, the key is minted per user gesture, and a blind library
 * retry would reuse it in a way the user never asked for.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: true,
    },
    mutations: { retry: 0 },
  },
});

const AlertsRoute = lazy(() => import("~/routes/alerts"));
const AlertDetailRoute = lazy(() => import("~/routes/alert-detail"));
const GroupsRoute = lazy(() => import("~/routes/groups"));
const GroupDetailRoute = lazy(() => import("~/routes/group-detail"));
const SettingsRoute = lazy(() => import("~/routes/settings"));
const LoginRoute = lazy(() => import("~/routes/login"));

/**
 * Mounts the `<html data-theme>` effect once, inside the reactive root.
 *
 * ⛔ THE SESSION PROVIDER IS OUTSIDE `LiveProvider`, and the order is binding.
 * The live stream is an authenticated SSE connection: opening it before the boot
 * probe has answered means an unauthenticated visitor's first act is a 401 on a
 * stream that then retries, which is half of the "Stale, retry in 2s" badge the
 * login gap used to leave on screen.
 */
const Root: Component<{ readonly children?: JSX.Element }> = (props) => {
  onMount(() => installThemeEffect());
  return (
    // The last resort, and the reason it exists: several modules read their
    // validation rules off the generated contract at IMPORT time
    // (`patternOf`/`maxLengthOf`/`enumValuesOf` in `api/bounds.ts`), and those
    // accessors THROW rather than fall back to a stale literal. That is the right
    // trade — a rule that silently disagrees with the server is worse than a loud
    // failure — but without a boundary the throw happens inside a `lazy()` chunk
    // and Solid paints an empty document. A blank screen tells an operator
    // nothing; `ErrorState` at least renders the message.
    <ErrorBoundary fallback={(err) => <ErrorState error={err} />}>
      <SessionProvider>{props.children}</SessionProvider>
    </ErrorBoundary>
  );
};

/**
 * Everything behind the door: the shell, the live stream and the screens.
 *
 * `RequireSession` renders none of it until `/me` has answered, so no screen can
 * fire the request that would 401 and paint a skeleton forever.
 */
const Authenticated: Component<{ readonly children?: JSX.Element }> = (props) => (
  <RequireSession>
    <LiveProvider>
      <AppShell>{props.children}</AppShell>
    </LiveProvider>
  </RequireSession>
);

const App: Component = () => (
  <QueryClientProvider client={queryClient}>
    <Router root={Root}>
      <Route path="/login" component={LoginRoute} />
      <Route path="/" component={() => <Navigate href="/alerts" />} />
      <Route path="/alerts" component={() => <Authenticated>{<AlertsRoute />}</Authenticated>} />
      <Route
        path="/alerts/:id"
        component={() => <Authenticated>{<AlertDetailRoute />}</Authenticated>}
      />
      <Route path="/groups" component={() => <Authenticated>{<GroupsRoute />}</Authenticated>} />
      <Route
        path="/groups/:id"
        component={() => <Authenticated>{<GroupDetailRoute />}</Authenticated>}
      />
      <Route
        path="/settings/:section"
        component={() => <Authenticated>{<SettingsRoute />}</Authenticated>}
      />
      <Route
        path="*"
        component={() => (
          <div class="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center">
            <p class="text-[14px] font-medium text-ink">That page does not exist.</p>
            <p class="text-[12px] text-ink-muted">
              The link may be from an older version of oto, or mistyped.
            </p>
          </div>
        )}
      />
    </Router>
  </QueryClientProvider>
);

export default App;
