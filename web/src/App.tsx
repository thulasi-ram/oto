import { Navigate, Route, Router, useParams } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { ErrorBoundary, lazy, onMount, type Component, type JSX } from "solid-js";

import { LiveProvider } from "~/api/live";
import { DEFAULT_STALE_MS } from "~/api/queries";
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
 * traffic for no extra truth. It is stated in `api/queries.ts`, with the
 * per-resource policies it is the floor under — a default is a freshness policy
 * too, and keeping it here would put it out of reach of the one test that
 * checks every entry has exactly one.
 *
 * Retries are off for mutations. A retried acknowledgement is only safe with an
 * idempotency key, the key is minted per user gesture, and a blind library
 * retry would reuse it in a way the user never asked for.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: DEFAULT_STALE_MS,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: true,
    },
    mutations: { retry: 0 },
  },
});

const AlertsRoute = lazy(() => import("~/routes/alerts"));
const AlertDetailRoute = lazy(() => import("~/routes/alert-detail"));
const CasesRoute = lazy(() => import("~/routes/cases"));
const CaseDetailRoute = lazy(() => import("~/routes/case-detail"));
const GroupsRoute = lazy(() => import("~/routes/groups"));
const GroupDetailRoute = lazy(() => import("~/routes/group-detail"));
const NotificationsRoute = lazy(() => import("~/routes/notifications"));
const SettingsRoute = lazy(() => import("~/routes/settings"));
const LoginRoute = lazy(() => import("~/routes/login"));
const LinearIssuesRoute = lazy(() => import("~/routes/linear-issues"));
const ProtoAlertsPreviewRoute = lazy(() => import("~/routes/proto-alerts-preview"));
const ProtoUiPreviewRoute = lazy(() => import("~/routes/proto-ui-preview"));
const ProtoSettingsPreviewRoute = lazy(() => import("~/routes/proto-settings-preview"));

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
 *
 * ⛔ IT IS A LAYOUT ROUTE'S COMPONENT, AND NEVER A WRAPPER A SCREEN PUTS AROUND
 * ITSELF. `props.children` here is the router's outlet, so navigating between the
 * six screens below swaps only the outlet and leaves this subtree standing.
 * Every route used to build its own `<Authenticated>` — five sibling route
 * components, five independent constructions of the same tree — and Solid does
 * not reconcile across siblings, so each nav click DISPOSED the shell and built a
 * fresh one. Two things were lost with it. `LiveProvider` constructs its
 * `AlertStream` per instance and `close()`s it on cleanup, so every click aborted
 * one `/api/v1/stream` request and opened another, re-entering `reconnecting` —
 * the word **Stale** in the header — until the replay caught up. And any state
 * the shell held reset, which is why `SnoozeBanner`'s read set had to be module
 * state to survive a navigation it should never have been exposed to.
 */
const Authenticated: Component<{ readonly children?: JSX.Element }> = (props) => (
  <RequireSession>
    <LiveProvider>
      <AppShell>{props.children}</AppShell>
    </LiveProvider>
  </RequireSession>
);

/**
 * `/groups/:id/<anything>` → `/groups/:id`, carrying the id across.
 *
 * ⛔ THE WILDCARD IS NOT TIDINESS, IT IS A DELIVERED LINK. A Slack card's
 * Timeline button is `links.group + "/timeline"`
 * (`internal/notification/service/view.go`), so every card oto has ever posted
 * carries `/groups/<id>/timeline` — a path a leaf route answers with the `*`
 * sentence rather than with the screen. The group's timeline *is* on the group
 * screen, so the sub-path lands there.
 *
 * A component rather than `<Navigate href={fn}>` because the target depends on a
 * route param, and it replaces the history entry rather than pushing one: an
 * operator who arrives from a Slack card and hits Back should land back in Slack,
 * not on a URL that only ever bounces them forward again.
 */
const RedirectToGroup: Component = () => {
  const params = useParams<{ readonly id: string }>();
  return <Navigate href={`/groups/${params.id}`} />;
};

const NotFound: Component = () => (
  <div class="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center">
    <p class="text-title font-medium text-ink">That page does not exist.</p>
    <p class="text-body text-ink-muted">
      The link may be from an older version of oto, or mistyped.
    </p>
  </div>
);

/**
 * The route table, as one value.
 *
 * Exported because the claim the layout route makes — that the shell survives a
 * navigation — is a claim about THIS tree and cannot be checked against a copy of
 * it declared in a test file. `App.test.tsx` mounts exactly these definitions
 * under a `MemoryRouter` with its own query client, which is the only way an
 * assertion about the shell's identity across a nav click is about the app.
 *
 * The three routes outside the layout are outside it on purpose: `/login` is the
 * door, `/` only redirects (opening a stream for a path nobody stays on would be
 * the remount bug in miniature), and `*` is a sentence, not a screen.
 */
export const routes = (): JSX.Element => (
  <>
    <Route path="/login" component={LoginRoute} />
    {/* Two entries read `/` and they do not compete: this one is a LEAF, so it
        matches `/` and nothing else, while the layout below carries children and
        is therefore matched partially — it contributes no branch of its own and
        only ever prefixes the six paths inside it. */}
    <Route path="/" component={() => <Navigate href="/cases" />} />
    <Route path="/" component={Authenticated}>
      <Route path="/alerts" component={AlertsRoute} />
      <Route path="/alerts/:id" component={AlertDetailRoute} />
      {/* The primary list: one row per firing episode, which is the unit a human
          acknowledges. `/cases/:id` is one of those episodes. */}
      <Route path="/cases" component={CasesRoute} />
      <Route path="/cases/:id" component={CaseDetailRoute} />
      {/* ⛔ `/groups` IS A REAL DESTINATION AND MUST NOT BE A REDIRECT. An
          AlertGroup is Alertmanager's notification grouping — the object that
          owns one Slack thread — and it is NOT a case: a case is one firing of
          ONE alert. Every card and webhook payload oto has ever sent carries
          `/groups/<id>`, and `view.go` still mints exactly that, so this is where
          a year of chat history points. It is deliberately absent from the rail
          (see `AppShell.tsx`): reachable from a card or from a case, never a
          place the product offers to take you. */}
      <Route path="/groups" component={GroupsRoute} />
      <Route path="/groups/:id" component={GroupDetailRoute} />
      {/* The card's Timeline button is `links.group + "/timeline"`, so a deep
          link one segment past a group has to land ON the group rather than on
          the not-found sentence. See `RedirectToGroup`. */}
      <Route path="/groups/:id/*" component={RedirectToGroup} />
      {/* Routing rules and the record of what they did (ADR 0034). `/settings`
          still answers `/settings/policies` and redirects it here, so links
          minted while policies lived there keep resolving.

          ⛔ THE SECTION IS OPTIONAL, AND THE BARE PATH IS WHAT THE RAIL LINKS TO.
          Not a tidiness preference: `<A>` sets `aria-current="page"` itself on an
          EXACT href match, and `mergeProps` puts its own binding last, so a
          destination whose href was `/notifications/policies` announced itself as
          the current page while the section row beneath it announced the same
          thing — two destinations for one location, and two accent rails stacked.
          Pointing the rail at `/notifications` makes the parent's href something
          the location never exactly equals, which hands the decision back to
          `AppShell`, where the one-accent-mark rule is actually written down.
          Each route redirects the bare path to its own first section. */}
      <Route path="/notifications/:section?" component={NotificationsRoute} />
      <Route path="/settings/:section?" component={SettingsRoute} />
    </Route>
    {/* Deliberately outside the authenticated layout: a static, mock-data-only
        visual reference with no real API calls, so it needs neither a session
        nor oto's own AppShell chrome around it.

        And deliberately dev-only: design scaffolding reachable with no session
        has no business resolving in a production build, so both prototype
        routes below are gated on `import.meta.env.DEV` and vanish from the
        route tree entirely once that is `false` — `import.meta.env.DEV` is
        statically known at build time, so a production bundle never even
        contains the branch that would render them. */}
    {import.meta.env.DEV && <Route path="/proto/linear-issues" component={LinearIssuesRoute} />}
    {/* Same rationale: overlay chrome (modals, dialogs) and the settings forms
        drawn against fixtures, so both can be reviewed without a session. */}
    {import.meta.env.DEV && <Route path="/proto/ui-preview" component={ProtoUiPreviewRoute} />}
    {import.meta.env.DEV && (
      <Route path="/proto/settings-preview" component={ProtoSettingsPreviewRoute} />
    )}
    {/* Outside the layout route for the same reason: the redesigned alert
        screens drawn against fixtures, so the visual design can be reviewed
        with no session and nothing listening on :8080. */}
    {import.meta.env.DEV && (
      <Route path="/proto/alerts-preview" component={ProtoAlertsPreviewRoute} />
    )}
    <Route path="*" component={NotFound} />
  </>
);

const App: Component = () => (
  <QueryClientProvider client={queryClient}>
    <Router root={Root}>{routes()}</Router>
  </QueryClientProvider>
);

export default App;
