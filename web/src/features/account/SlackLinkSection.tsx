/**
 * Link a Slack account to yourself (git-bug a556a5c; the ruling of 2026-10-05:
 * SELF-SERVICE IN THE UI).
 *
 * ⭐⭐ A LINK DECIDES WHOSE APPROVAL A SLACK CLICK COUNTS AS. A Remedy approved
 * from Slack counts as the oto user its Slack member is linked to, and double
 * approval counts DIFFERENT users (ADR 0054 §4) — so a wrong link lets one
 * person's clicks count as another's approval.
 *
 * The flow starts in Slack, because only Slack can prove a Slack identity: an
 * unlinked member who presses a Remedy's Approve or Decline is shown a code only
 * they can see. Here, signed in, they enter it — and see WHICH Slack account it
 * would link before anything is written. Two steps, never one: the preview does
 * not use the code up, the confirm does.
 *
 * ⛔ THE CODE IS A CREDENTIAL, AND THE SCREEN SAYS SO. Whoever enters a code in
 * their own session makes that Slack member's clicks count as them; the
 * confirmation naming the member and workspace is the check against entering
 * somebody else's.
 *
 * ⛔ NOTHING HERE NAMES A USER. Every call links, lists or unlinks the signed-in
 * person, because the API has no way to say anybody else.
 */
import { For, Match, Show, Switch, createSignal, type Component } from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";

import {
  linkSlackIdentity,
  listMySlackIdentities,
  previewSlackLink,
  unlinkSlackIdentity,
} from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { SlackIdentity, SlackLinkPreview } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { Button } from "~/components/ui/Button";
import { Panel, PanelHeader, PanelTitle } from "~/components/ui/surfaces";
import {
  TextField,
  TextFieldDescription,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { EmptyState, ErrorBanner, ErrorState, LoadingLine } from "~/components/ui/states";
import { cn } from "~/lib/cn";

import { FIELD, FORM, HELP, PANEL_BODY, PANEL_HEADER, ROW, SECTION } from "../settings/rhythm";

/** The contract's bound on a typed code; the real one is eleven characters. */
const CODE_MAX = 32;

/** `@handle (U…)`, or the member id alone when Slack never sent a handle. */
function memberLabel(m: { readonly handle?: string | null; readonly slack_user_id: string }): string {
  return m.handle ? `@${m.handle} (${m.slack_user_id})` : m.slack_user_id;
}

export const SlackLinkSection: Component = () => {
  const linked = useQuery(() => ({
    queryKey: qk.settings.slackIdentities(),
    queryFn: ({ signal }) => listMySlackIdentities({ signal }),
  }));

  return (
    <div class={SECTION}>
      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Slack</PanelTitle>
        </PanelHeader>
        <div class={cn(PANEL_BODY, FORM)}>
          <p class={HELP}>
            A Slack account linked to you can approve or decline a Remedy from its Slack card, and those
            clicks count as yours. To link one, press a Remedy's <strong class="font-semibold text-ink">Approve</strong>{" "}
            or <strong class="font-semibold text-ink">Decline</strong> in Slack: oto answers with a code only you
            can see. Enter it here within ten minutes.
          </p>
          <LinkForm />
        </div>
      </Panel>

      <Panel>
        <PanelHeader class={PANEL_HEADER}>
          <PanelTitle>Linked Slack accounts</PanelTitle>
        </PanelHeader>
        <Switch>
          <Match when={linked.isPending}>
            <LoadingLine />
          </Match>
          <Match when={linked.isError}>
            <ErrorState error={linked.error} />
          </Match>
          <Match when={(linked.data?.data.length ?? 0) === 0}>
            <EmptyState title="No Slack account is linked to you." />
          </Match>
          <Match when={linked.data}>
            {(page) => (
              <ul>
                <For each={page().data}>{(si) => <LinkedRow identity={si} />}</For>
              </ul>
            )}
          </Match>
        </Switch>
      </Panel>
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const LinkForm: Component = () => {
  const client = useQueryClient();
  const [code, setCode] = createSignal("");
  const [preview, setPreview] = createSignal<SlackLinkPreview | null>(null);
  const [done, setDone] = createSignal<SlackIdentity | null>(null);

  const check = useMutation(() => ({
    mutationFn: () => previewSlackLink({ code: code().trim() }),
    onSuccess: (p: SlackLinkPreview) => {
      setDone(null);
      setPreview(p);
    },
  }));

  const confirm = useMutation(() => ({
    mutationFn: () => linkSlackIdentity({ code: code().trim() }),
    onSuccess: (si: SlackIdentity) => {
      setPreview(null);
      setCode("");
      setDone(si);
      void client.invalidateQueries({ queryKey: qk.settings.slackIdentities() });
    },
  }));

  const cancel = (): void => {
    setPreview(null);
    setCode("");
    check.reset();
    confirm.reset();
  };

  return (
    <div class={FORM}>
      <Show when={check.error !== null}>
        <ErrorBanner error={check.error} />
      </Show>
      <Show when={confirm.error !== null}>
        <ErrorBanner error={confirm.error} />
      </Show>
      <Show when={done()}>
        {(si) => (
          <p role="status" class="text-item text-ink">
            Linked. Clicks from {memberLabel(si())} in workspace {si().team_id} now count as you.
          </p>
        )}
      </Show>

      <Show
        when={preview()}
        fallback={
          <>
            <TextField class={FIELD} value={code()} onChange={setCode}>
              <TextFieldLabel>Link code</TextFieldLabel>
              <TextFieldInput
                id="slack-link-code"
                maxLength={CODE_MAX}
                autocomplete="off"
                spellcheck={false}
                class="font-mono uppercase"
                placeholder="ABCDE-FGHJK"
              />
              <TextFieldDescription class={HELP}>
                ⚠️ A link code is a credential. Whoever enters it in their own oto session makes that Slack
                account's clicks count as them — enter only a code Slack showed you, and never share yours.
              </TextFieldDescription>
            </TextField>
            <div>
              <Button
                size="sm"
                variant="default"
                busy={check.isPending}
                disabled={code().trim() === ""}
                onClick={() => check.mutate()}
              >
                Check code
              </Button>
            </div>
          </>
        }
      >
        {(p) => (
          <div class="flex flex-col gap-sm rounded-control border border-line-strong bg-raised px-md py-sm">
            <Show
              when={!p().already_yours}
              fallback={
                <p class="text-item text-ink">
                  Slack account {memberLabel(p())} in workspace {p().team_id} is already linked to you.
                </p>
              }
            >
              <p class="text-item text-ink" data-confirm-copy>
                Link Slack account <strong class="font-semibold">{memberLabel(p())}</strong> in workspace{" "}
                <strong class="font-semibold">{p().team_id}</strong> to you?{" "}
                <strong class="font-semibold">Clicks from this Slack account will count as you.</strong>
              </p>
              <p class={HELP}>
                If you do not recognise this Slack account, cancel: somebody else's code would make their clicks
                count as yours.
              </p>
            </Show>
            <div class="flex gap-sm">
              <Show when={!p().already_yours}>
                <Button size="sm" variant="default" busy={confirm.isPending} onClick={() => confirm.mutate()}>
                  Link to me
                </Button>
              </Show>
              <Button size="sm" variant="secondary" onClick={cancel}>
                Cancel
              </Button>
            </div>
          </div>
        )}
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const LinkedRow: Component<{ readonly identity: SlackIdentity }> = (props) => {
  const client = useQueryClient();
  const [confirming, setConfirming] = createSignal(false);
  const unlink = useMutation(() => ({
    mutationFn: () => unlinkSlackIdentity(props.identity.id),
    onSuccess: () => {
      setConfirming(false);
      void client.invalidateQueries({ queryKey: qk.settings.slackIdentities() });
    },
  }));

  return (
    <li class={cn(ROW, "flex min-h-12 flex-wrap items-center gap-sm")}>
      <span class="text-item font-medium text-ink">{memberLabel(props.identity)}</span>
      <span class="text-body text-ink-muted">workspace {props.identity.team_id}</span>
      <span class="text-body text-ink-muted">
        linked <RelativeTime value={props.identity.linked_at} />
      </span>
      <span class="ml-auto flex items-center gap-sm">
        <Show when={unlink.error !== null}>
          <ErrorBanner error={unlink.error} />
        </Show>
        <Show
          when={confirming()}
          fallback={
            <Button size="sm" variant="secondary" onClick={() => setConfirming(true)}>
              Unlink
            </Button>
          }
        >
          <span class="text-body text-ink">Its Slack clicks will count as nobody.</span>
          <Button size="sm" variant="destructive" busy={unlink.isPending} onClick={() => unlink.mutate()}>
            Unlink
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            Keep
          </Button>
        </Show>
      </span>
    </li>
  );
};
