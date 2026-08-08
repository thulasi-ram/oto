import { Navigate, Route, Router } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { lazy, onMount, type Component, type JSX } from "solid-js";

import { LiveProvider } from "~/api/live";
import { AppShell } from "~/components/AppShell";
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

/** Mounts the `<html data-theme>` effect once, inside the reactive root. */
const Root: Component<{ readonly children?: JSX.Element }> = (props) => {
  onMount(() => installThemeEffect());
  return (
    <LiveProvider>
      <AppShell>{props.children}</AppShell>
    </LiveProvider>
  );
};

const App: Component = () => (
  <QueryClientProvider client={queryClient}>
    <Router root={Root}>
      <Route path="/" component={() => <Navigate href="/alerts" />} />
      <Route path="/alerts" component={AlertsRoute} />
      <Route path="/alerts/:id" component={AlertDetailRoute} />
      <Route path="/groups" component={GroupsRoute} />
      <Route path="/groups/:id" component={GroupDetailRoute} />
      <Route path="/settings/:section" component={SettingsRoute} />
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
