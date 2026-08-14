/**
 * The door.
 *
 * The server side of this has been complete and tested the whole time — `POST
 * /api/v1/auth/login` works, sets a hardened cookie and answers with the
 * principal. What was missing was any way to reach it from the product, so a
 * fresh install rendered skeleton rows forever and read as broken.
 *
 * # What this screen refuses to do
 *
 * ⛔ IT DOES NOT SAY WHETHER THE ACCOUNT EXISTS. The contract is explicit that a
 * failed login returns "an unspecific 401 that does not reveal whether the
 * account exists", and a form that helpfully distinguished "no such user" from
 * "wrong password" would hand that back to an enumerator. One sentence covers
 * both, and it is the server's own.
 *
 * # The 429 is real
 *
 * Repeated failures are rate-limited server-side, and that limiter is tested. A
 * form that rendered a 429 as a generic failure would teach the operator to keep
 * hammering a door that is now counting against them, so it gets its own
 * sentence and its own retry-after.
 */
import { useNavigate } from "@solidjs/router";
import { createSignal, Show, type Component } from "solid-js";

import { ApiError } from "~/api/client";
import { useSession } from "~/api/session";
import { Chime } from "~/components/ui/Chime";
import { Button, Field, Input } from "~/components/ui/primitives";

/** What the operator is told, per failure. Never more specific than the server. */
function messageFor(err: unknown): string {
  if (!(err instanceof ApiError)) return "Something went wrong. Try again.";
  if (err.status === 0) return "Cannot reach oto. Check that the server is running.";
  if (err.status === 401) return "Those details were not accepted. Check them and try again.";
  if (err.status === 429) {
    const wait = err.retryAfterSeconds;
    return wait === null
      ? "Too many attempts. Wait a moment before trying again."
      : `Too many attempts. Wait ${wait}s before trying again.`;
  }
  if (err.status === 422) return "Fill in both fields.";
  return err.headline;
}

const LoginRoute: Component = () => {
  const session = useSession();
  const navigate = useNavigate();

  const [email, setEmail] = createSignal("");
  const [password, setPassword] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (e: Event): Promise<void> => {
    e.preventDefault();
    if (busy()) return;
    setBusy(true);
    setError(null);
    try {
      await session.signIn({ email: email(), password: password() });
      // `replace` so Back does not return to a login form the user is now past.
      navigate("/alerts", { replace: true });
    } catch (err) {
      setError(messageFor(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="flex flex-1 flex-col items-center justify-center gap-10 p-8">
      {/* The one screen with no incident on it.
          Everywhere else the fūrin would compete with something an operator is
          trying to read, so it stays at 16px in the chrome and 32px in an empty
          panel. Here there is nothing to compete with, and the negative space
          around it — ma — is doing as much work as the glyph: `gap-10` against
          the form's own `gap-4` is what makes it read as a threshold rather than
          as a decoration bolted to a field. Still, quiet, and no larger than the
          empty state's glyph. */}
      <Chime size="glyph" class="text-line-strong" />

      <form
        class="flex w-full max-w-[320px] flex-col gap-4"
        onSubmit={(e) => void submit(e)}
        aria-labelledby="login-heading"
      >
        <div class="flex flex-col gap-1">
          <h1 id="login-heading" class="text-title font-semibold text-ink">
            Sign in to oto
          </h1>
          <p class="text-body text-ink-muted">Use the account created at setup.</p>
        </div>

        <Field id="login-email" label="Email" required>
          {(a) => (
            <Input
              {...a}
              type="email"
              name="email"
              autocomplete="username"
              required
              value={email()}
              onInput={(e) => setEmail(e.currentTarget.value)}
            />
          )}
        </Field>

        <Field id="login-password" label="Password" required>
          {(a) => (
            <Input
              {...a}
              type="password"
              name="password"
              autocomplete="current-password"
              required
              value={password()}
              onInput={(e) => setPassword(e.currentTarget.value)}
            />
          )}
        </Field>

        {/* role=alert so the failure is announced, not just painted. */}
        <Show when={error()}>
          {(msg) => (
            <p class="text-body font-medium leading-snug text-ink" role="alert">
              <span
                aria-hidden="true"
                class="mr-1 inline-block size-1.5 rounded-full bg-accent align-middle"
              />
              {msg()}
            </p>
          )}
        </Show>

        <Button type="submit" variant="primary" busy={busy()}>
          Sign in
        </Button>
      </form>
    </div>
  );
};

export default LoginRoute;
