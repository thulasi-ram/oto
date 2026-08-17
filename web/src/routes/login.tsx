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
import { Logo } from "~/components/Logo";
import { Button } from "~/components/ui/Button";
import { Ink, clearColumn } from "~/components/ui/Ink";
import { TextField, TextFieldInput, TextFieldLabel } from "~/components/ui/TextField";

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
    // `min-h-full` because `flex-1` has never done anything here: the shell
    // mounts this route inside a plain `h-full` div rather than a flex column,
    // so the wrapper has always been content-height (435 px of an 827 px
    // viewport) and the composition has always sat at the top of the screen.
    // Nothing noticed while the canvas was empty. With ink on it, `overflow-
    // hidden` cuts the bleed off at a hard horizontal line two thirds up the
    // page — a rectangle, which is the one thing a brush must never look like.
    <div class="relative flex min-h-full flex-1 flex-col items-center justify-center gap-10 overflow-hidden p-8">
      {/* ---- atmosphere (§M.9) -------------------------------------------
          The ensō, twice, oversized and cropped by two opposite corners, at
          `--oto-wash`. This is the ONE screen §M.2 permits it on: every other
          surface has an alert on it, and decoration there costs a firing row
          its scarcity. The form below stays bare on the canvas — putting it on
          an opaque `bg-surface` card is the trivially safe way to guarantee
          contrast and it turns a threshold into a card, which is the opposite
          of what the comment under this one is asking for.

          ⭐ THE CONTRAST GUARANTEE IS GEOMETRY, NOT A CAREFULLY CHOSEN OPACITY.
          Both spans stretch to `inset-0` of this wrapper — not to the size of
          the art — so `clearColumn`'s `50%` is the wrapper's middle, which is
          where the form is centred. The 400 px it clears is the form's 320 px
          plus 40 px of air on each side, at every viewport width and with no
          media query. Measured, this is what the alternative costs:
          `--oto-text-subtle` is 4.90:1 on `--oto-bg` in light and 4.37:1 under
          a flat 6% wash, which fails AA — and nothing in CI would catch it,
          because `contrast.test.ts` measures token pairs and not composites.

          Nothing here animates. U9's decorative one-shot budget is a document's
          worth and the fūrin already spends it (ADR 0028); a fading wash would
          be a second one. */}
      <Ink
        motif="enso"
        size="28rem 28rem, 100% 100%"
        position="-6rem -8rem, center"
        carve={clearColumn("400px")}
        class="absolute inset-0"
      />
      <Ink
        motif="enso"
        size="28rem 28rem, 100% 100%"
        position="right -6rem bottom -8rem, center"
        carve={clearColumn("400px")}
        class="absolute inset-0"
      />

      {/* `relative` on the two content elements, rather than a negative z-index
          on the ink: neither this wrapper nor anything above it opens a stacking
          context, so `-z-10` would send the wash behind the page background and
          out of sight entirely. Two positioned siblings at `z-index: auto` paint
          in DOM order, and the ink is written first. */}

      {/* The one screen with no incident on it, and therefore the only one that
          gets the mark itself rather than a piece of it.
          Everywhere else the fūrin would compete with something an operator is
          trying to read, so it stays at 16px in the chrome (beside the
          signature) and 32px in an empty panel. Here there is nothing to
          compete with: the ensō, the bell inside it and the brush "oto" get to
          be the whole composition they were drawn as. The negative space around
          them — ma — is doing as much work as the ink: `gap-10` against the
          form's own `gap-4` is what makes this read as a threshold rather than
          as a decoration bolted to a field.
          `text-line-strong` rather than `text-ink`, for the reason the glyph
          was: this is a greeting, not a heading, and the sentence under it is
          what the person came here to act on. */}
      <Logo class="relative size-28 text-line-strong" />

      <form
        class="relative flex w-full max-w-[320px] flex-col gap-4"
        onSubmit={(e) => void submit(e)}
        aria-labelledby="login-heading"
      >
        <div class="flex flex-col gap-1">
          <h1 id="login-heading" class="text-title font-semibold text-ink">
            Sign in to oto
          </h1>
          <p class="text-body text-ink-muted">Use the account created at setup.</p>
        </div>

        <TextField value={email()} onChange={setEmail} name="email" required>
          <TextFieldLabel>Email</TextFieldLabel>
          <TextFieldInput id="login-email" type="email" autocomplete="username" />
        </TextField>

        <TextField value={password()} onChange={setPassword} name="password" required>
          <TextFieldLabel>Password</TextFieldLabel>
          <TextFieldInput id="login-password" type="password" autocomplete="current-password" />
        </TextField>

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

        <Button type="submit" busy={busy()}>
          Sign in
        </Button>
      </form>
    </div>
  );
};

export default LoginRoute;
