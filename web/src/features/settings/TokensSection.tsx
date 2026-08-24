/**
 * Personal access tokens — the credential for everything that is not this app.
 *
 * # Why this screen has to exist here and nowhere else
 *
 * ⛔ MINTING A PAT REQUIRES A SESSION, AND THE BROWSER IS THE ONLY THING THAT
 * HOLDS ONE. All three operations are `security: [sessionCookie]` in the
 * contract, and `internal/identity/api/tokens.go` gives the reason rather than
 * the rule: "a token cannot enumerate its own siblings". The session credential
 * is an `HttpOnly` cookie, so no script can read it and no other client can be
 * handed it — which means this is not a convenience wrapper over an API an
 * operator could reach anyway. It is the only reachable surface there is.
 *
 * Without it the paths were `oto bootstrap`, which mints one token and then
 * refuses to run again for the life of the deployment (`ErrAlreadyBootstrapped`),
 * and a `fetch()` typed into a devtools console. Losing the bootstrap token meant
 * losing API access with no supported way back.
 *
 * # What the list is, and is not
 *
 * It is the signed-in operator's OWN tokens. The narrowing is the service's and
 * it is a privacy rule, not an authorisation one — v1 has no RBAC (SPEC §R2) and
 * every principal has full access to its org, so this is not a permission
 * boundary and must not be dressed as one. The panel says whose list it is,
 * because a list titled "tokens" that quietly showed a subset would misrepresent
 * the org's credential surface to somebody auditing it.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import { maxLengthOf } from "~/api/bounds";
import { violationsByField } from "~/api/client";
import { createApiToken, listApiTokens, revokeApiToken } from "~/api/endpoints";
import { CreateTokenRequestSchema } from "~/api/generated/validators";
import { qk } from "~/api/keys";
import type { ApiToken, ApiTokenCreated } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import { Chip, Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";
import { idempotencyKey } from "~/lib/format";

import { OneTimeSecret } from "./OneTimeSecret";
import { FIELD, FORM, HELP, PANEL_BODY, PANEL_HEADER, ROW, SECTION } from "./rhythm";

/**
 * ⛔ READ OFF THE GENERATED SCHEMA, NOT TYPED HERE. `SourcesSection` shipped a
 * hand-copied URL pattern that had drifted from the contract in two ways, which
 * is why `bounds.ts` exists; a length cap is the same class of claim and gets the
 * same treatment.
 */
const NAME_MAX = maxLengthOf(CreateTokenRequestSchema, "name");

export const TokensSection: Component = () => {
  const [minting, setMinting] = createSignal(false);
  const [secret, setSecret] = createSignal<ApiTokenCreated | null>(null);

  const tokens = useQuery(() => ({
    queryKey: qk.settings.apiTokens(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listApiTokens({ signal }),
  }));

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Your access tokens</PanelTitle>
          <Button size="sm" variant="default" onClick={() => setMinting(true)}>
            Create a token
          </Button>
        </PanelHeader>

        <Switch>
          <Match when={tokens.isPending}>
            <LoadingLine />
          </Match>
          <Match when={tokens.isError}>
            <ErrorState error={tokens.error} onRetry={() => void tokens.refetch()} />
          </Match>
          <Match when={(tokens.data?.data.length ?? 0) === 0}>
            <EmptyState
              title="You have no tokens."
              body="A personal access token is how anything outside this browser authenticates as you — a script, a terminal, CI. It is shown once when created."
            />
          </Match>
          <Match when={true}>
            <ul>
              <For each={tokens.data?.data ?? []}>{(t) => <TokenRow token={t} />}</For>
            </ul>
          </Match>
        </Switch>

        <p class={cn(PANEL_BODY, HELP, "border-t border-line")}>
          These are yours alone — another operator's tokens are not listed here and this is not an
          org-wide inventory. v1 has no roles, so a token carries the same access your session does.
        </p>
      </Panel>

      <MintDialog
        open={minting()}
        onClose={() => setMinting(false)}
        onMinted={(created) => setSecret(created)}
      />

      <SecretDialog created={secret()} onClose={() => setSecret(null)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const TokenRow: Component<{ readonly token: ApiToken }> = (props) => {
  const client = useQueryClient();
  const t = (): ApiToken => props.token;
  const [confirming, setConfirming] = createSignal(false);

  const revoke = useMutation(() => ({
    mutationFn: () => revokeApiToken(t().id),
    onSuccess: () => {
      setConfirming(false);
      void client.invalidateQueries({ queryKey: qk.settings.apiTokens() });
    },
  }));

  const revoked = (): boolean => t().revoked_at !== null && t().revoked_at !== undefined;

  /**
   * An expiry in the past is spent, and it is NOT the same fact as revoked: one
   * ran out, the other was taken away. The row distinguishes them because the
   * next move differs — a spent token is replaced, a revoked one was replaced
   * already and is being audited.
   */
  const expired = (): boolean => {
    const at = t().expires_at;
    return at !== null && at !== undefined && Date.parse(at) < Date.now();
  };

  return (
    <li class={cn(ROW, "flex min-h-12 flex-wrap items-center gap-sm")}>
      <span class="text-item font-medium text-ink">{t().name}</span>
      {/*
        The prefix, not a masked secret. oto stores only a sha256, so there is no
        value to mask — inventing `oto_pat_AbCd••••••••` would render a string
        that does not exist anywhere. Four characters is what the server keeps
        precisely so a token can be told apart without being revealed.
      */}
      <Chip mono title="The first characters of this token, kept so it can be identified without being shown.">
        {t().prefix}
      </Chip>
      <Show when={revoked()}>
        <Chip title="Revoked. It may keep working for up to a minute while the credential cache expires.">
          revoked
        </Chip>
      </Show>
      <Show when={!revoked() && expired()}>
        <Chip title="Past its expiry. It no longer authenticates anything.">expired</Chip>
      </Show>

      <span class="text-meta text-ink-subtle">
        <Show
          when={t().last_used_at}
          fallback={<span title="This token has never authenticated a request.">never used</span>}
        >
          {(at) => (
            <>
              last used <RelativeTime value={at()} label="Last used" /> ago
            </>
          )}
        </Show>
      </span>

      {/*
        ⛔ THE PREPOSITION BELONGS TO THE FORMATTER — the same fault
        `EnrichmentPanel`'s expiry chip carried. `relativeTime` already renders a
        FUTURE instant as `in 3d` and a past one bare, so the trailing " from
        now" here spelled the row "expires in 3d from now": one interval named
        twice. The VERB is still this row's to choose, because `expired()` above
        is the only thing that knows which side of the expiry we are reading
        from, and `expired 1d ago` and `expires in 3d` are two different facts
        about whether this credential still authenticates anything.
      */}
      <Show when={t().expires_at}>
        {(at) => (
          <span class="text-meta text-ink-subtle">
            {expired() ? "expired" : "expires"} <RelativeTime value={at()} label="Expires" />
            {expired() ? " ago" : ""}
          </span>
        )}
      </Show>

      <div class="ml-auto flex items-center gap-sm">
        <Show when={!revoked()}>
          <Button size="sm" variant="destructive" onClick={() => setConfirming(true)}>
            Revoke
          </Button>
        </Show>
      </div>

      <Show when={revoke.error !== null}>
        <ErrorBanner error={revoke.error} />
      </Show>

      <Modal
        open={confirming()}
        onOpenChange={(isOpen) => {
          if (!isOpen) setConfirming(false);
        }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>Revoke {t().name}?</ModalTitle>
            <ModalDescription>
              Anything still authenticating with this token starts failing. It cannot be un-revoked.
            </ModalDescription>
          </ModalHeader>

          <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
            {/*
              ⛔ THE DELAY IS STATED, because the operator most likely to press
              this is responding to a leak and needs to know the door is not shut
              yet. oto caches credentials, so a revoked token keeps working for up
              to a minute. Saying "revoked" and stopping would let somebody
              conclude the incident was over sixty seconds before it was.
            */}
            <p>
              Revocation takes effect within <strong class="font-semibold">one minute</strong>, not
              instantly — oto caches credentials, and this token keeps working until that cache
              expires. If you are responding to a leak, treat the next minute as still exposed.
            </p>
          </div>

          <ModalFooter>
            <Button size="sm" variant="secondary" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button
              size="sm"
              variant="destructive"
              busy={revoke.isPending}
              onClick={() => revoke.mutate()}
            >
              Revoke it
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

const MintDialog: Component<{
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onMinted: (created: ApiTokenCreated) => void;
}> = (props) => {
  const client = useQueryClient();
  const [name, setName] = createSignal("");

  const mint = useMutation(() => ({
    /*
     * A fresh key per submit, and here that matters more than usual. The
     * contract refuses a reused `Idempotency-Key` with a `409` rather than
     * replaying the original answer, because the original answer was a secret
     * oto no longer holds — replaying would mean it had kept one in the clear.
     * So a retry is a new gesture with a new key, never the same one sent twice.
     */
    mutationFn: () => createApiToken({ name: name().trim() }, idempotencyKey()),
    onSuccess: (created) => {
      setName("");
      props.onMinted(created);
      props.onClose();
      void client.invalidateQueries({ queryKey: qk.settings.apiTokens() });
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mint.error);

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) props.onClose();
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Create a personal access token</ModalTitle>
          <ModalDescription>
            It authenticates as you, with the same access your session has. The secret is shown once
            and never again.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mint.error !== null}>
            <ErrorBanner error={mint.error} />
          </Show>

          <TextField
            class={FIELD}
            value={name()}
            required
            validationState={violations().get("name") ? "invalid" : "valid"}
            onChange={setName}
          >
            <TextFieldLabel>Name</TextFieldLabel>
            <TextFieldInput id="token-name" maxLength={NAME_MAX} placeholder="laptop CLI" />
            <TextFieldDescription class={HELP}>
              For your own recall. It is the only thing distinguishing this token from your others in
              the list, since the secret is never shown again.
            </TextFieldDescription>
            <TextFieldErrorMessage id="token-name-error" role="alert">
              {violations().get("name")}
            </TextFieldErrorMessage>
          </TextField>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mint.isPending}
            disabled={name().trim() === ""}
            onClick={() => mint.mutate()}
          >
            Create
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */

/**
 * The secret, once.
 *
 * Same shape as the source screen's ingest-token dialog and for the same reason:
 * `POST /api/v1/api-tokens`'s 201 body and `SourceCreatedDTO` are the only two
 * responses in this API that ever carry a credential, and an operator should not
 * have to learn two different ways of being handed one.
 */
const SecretDialog: Component<{
  readonly created: ApiTokenCreated | null;
  readonly onClose: () => void;
}> = (props) => (
  <Modal
    open={props.created !== null}
    onOpenChange={(isOpen) => {
      if (!isOpen) props.onClose();
    }}
  >
    <ModalContent>
      <ModalHeader>
        <ModalTitle>Your new token</ModalTitle>
        <ModalDescription>
          This is the only time oto will show it. Only a hash is stored, so a lost token is replaced
          rather than recovered.
        </ModalDescription>
      </ModalHeader>

      <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
        <Show when={props.created}>
          {(created) => (
            <OneTimeSecret
              label={created().token.name}
              value={created().secret}
              secret
              help="Send it as `Authorization: Bearer …`. Treat it like a password: it has the same access to this org as your own session."
            />
          )}
        </Show>
      </div>

      <ModalFooter>
        <Button size="sm" variant="default" onClick={props.onClose}>
          I have copied it
        </Button>
      </ModalFooter>
    </ModalContent>
  </Modal>
);
