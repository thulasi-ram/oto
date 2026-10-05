/**
 * `/account` — what belongs to the signed-in person rather than to the org.
 *
 * It exists for one section today: linking a Slack account to yourself (git-bug
 * a556a5c). That is not org settings — it is about whose approval YOUR Slack
 * clicks count as — so it lives behind the user menu, not under `/settings`.
 */
import type { Component } from "solid-js";

import { SlackLinkSection } from "~/features/account/SlackLinkSection";

const AccountRoute: Component = () => (
  <div class="min-h-0 w-full flex-1 overflow-auto">
    <div class="flex w-full max-w-3xl flex-col gap-xl p-lg">
      <h1 class="text-title font-semibold text-ink">Account</h1>
      <SlackLinkSection />
    </div>
  </div>
);

export default AccountRoute;
