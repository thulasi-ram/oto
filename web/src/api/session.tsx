/**
 * Who is signed in, and what the app does when the answer is "nobody".
 *
 * # Why this exists at all
 *
 * The session credential is an `oto_session` cookie, and it is `HttpOnly`. No
 * script in this app can read it, write it, or ask the browser whether it is
 * still valid — which is the correct design and also means **the UI cannot
 * answer "am I signed in?" locally.** The only honest answer comes from the
 * server, so this provider asks `GET /api/v1/me` once on boot and treats the
 * reply as the truth:
 *
 *   - `200` → signed in, and the body is the principal the shell renders.
 *   - `401` → not signed in, go to the login screen.
 *
 * # Why the app renders nothing until that resolves
 *
 * ⛔ RENDERING THE APP OPTIMISTICALLY IS THE BUG THIS FILE FIXES. Before this
 * existed, `/` navigated straight to `/alerts`, every request answered 401,
 * nothing consumed the 401, and the visitor watched skeleton rows load forever
 * behind a "Stale, retry in 2s" badge. The product read as broken and silent
 * rather than as "you need to sign in", which is the worst available reading of
 * a working server. A brief neutral pause on a cold load is a much smaller cost
 * than a permanent lie, so the gate resolves before any authenticated screen
 * mounts.
 *
 * # The later-401 path
 *
 * A session can also die mid-visit — it expires, or `logout` is called from
 * another tab. Those 401s surface from arbitrary queries deep in the tree, so
 * `client.ts` publishes them through `onUnauthenticated` and this provider
 * listens. It does NOT navigate on its own: it flips state to signed-out, and
 * the guard reacts. One source of truth for "where do we go", rather than a
 * redirect fired from inside a fetch handler.
 */
import { useNavigate } from "@solidjs/router";
import {
  createContext,
  createResource,
  createSignal,
  onCleanup,
  useContext,
  type Accessor,
  type ParentComponent,
} from "solid-js";

import { onUnauthenticated } from "./client";
import { getCurrentPrincipal, login as postLogin, logout as postLogout } from "./endpoints";
import type { LoginRequest, Me } from "./types";

export interface Session {
  /** The principal, or `null` when signed out. `undefined` while unresolved. */
  readonly me: Accessor<Me | null | undefined>;
  /** True until the boot probe has answered. Nothing authenticated renders yet. */
  readonly resolving: Accessor<boolean>;
  readonly signIn: (body: LoginRequest) => Promise<void>;
  readonly signOut: () => Promise<void>;
}

const SessionContext = createContext<Session>();

/** The session, or a loud failure. A component outside the provider is a bug. */
export function useSession(): Session {
  const s = useContext(SessionContext);
  if (s === undefined) throw new Error("oto: useSession outside <SessionProvider>");
  return s;
}

export const SessionProvider: ParentComponent = (props) => {
  // `undefined` = not yet asked. `null` = asked, and the answer was nobody.
  const [me, setMe] = createSignal<Me | null | undefined>(undefined);

  // The boot probe. A 401 is an ANSWER here, not a failure: it is how the server
  // says "nobody", and treating it as an error would put the app in an error
  // state on the single most ordinary path there is — a first-time visitor.
  const [probe] = createResource(async () => {
    try {
      const principal = await getCurrentPrincipal();
      setMe(principal);
    } catch {
      setMe(null);
    }
    return true;
  });

  // A session that dies mid-visit. Flip state; the guard decides where to go.
  onCleanup(onUnauthenticated(() => setMe(null)));

  const session: Session = {
    me,
    resolving: () => probe.loading,
    signIn: async (body) => {
      // `login` answers with the same MeResponse as `/me`, so a successful sign
      // in needs no second round trip to learn who just signed in.
      setMe(await postLogin(body));
    },
    signOut: async () => {
      try {
        await postLogout();
      } finally {
        // Signed out locally even if the revoke failed. Leaving the shell up
        // after the user asked to leave is worse than a session row that lives
        // until it expires on its own.
        setMe(null);
      }
    },
  };

  return <SessionContext.Provider value={session}>{props.children}</SessionContext.Provider>;
};

/**
 * Gate an authenticated subtree.
 *
 * Three states, and the middle one is the whole point: while the probe is in
 * flight nothing authenticated mounts, so no screen ever fires the request that
 * would 401 and paint a permanent skeleton.
 */
export const RequireSession: ParentComponent = (props) => {
  const session = useSession();
  const navigate = useNavigate();

  return (
    <>
      {session.resolving() ? (
        <div
          class="flex flex-1 items-center justify-center p-8"
          role="status"
          aria-label="Checking your session"
        />
      ) : session.me() ? (
        props.children
      ) : (
        (() => {
          // `replace` so Back does not walk into the screen that just bounced.
          navigate("/login", { replace: true });
          return null;
        })()
      )}
    </>
  );
};
