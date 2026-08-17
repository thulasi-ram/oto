/**
 * `/settings/:section` — sources, clusters, channels and policies.
 *
 * The section is in the path rather than in component state so a settings screen
 * is linkable like everything else. "Look at the channel config" should be a URL.
 */
import { For, Match, Switch, type Component } from "solid-js";
import { A, Navigate, useParams } from "@solidjs/router";

import { cn } from "~/lib/cn";
import { SidebarPanel } from "~/components/SidebarSlot";
import { ChannelsSection } from "~/features/settings/ChannelsSection";
import { PoliciesSection } from "~/features/settings/PoliciesSection";
import { SourcesSection } from "~/features/settings/SourcesSection";
import { TokensSection } from "~/features/settings/TokensSection";
import { TuningSection } from "~/features/settings/TuningSection";

/*
 * Tokens sits last because it is the least visited and the only one that is not
 * about the alert pipeline — but it belongs in settings rather than under the
 * user menu, which was the other candidate. A PAT is org data with a lifecycle,
 * an audit trail and a revoke button, listed the same way sources and channels
 * are; a menu is for acts, not for inventories.
 */
const SECTIONS = [
  { id: "sources", label: "Sources and clusters" },
  { id: "channels", label: "Channels" },
  { id: "policies", label: "Notification policies" },
  { id: "tuning", label: "Tuning" },
  { id: "tokens", label: "Access tokens" },
] as const;

type SectionId = (typeof SECTIONS)[number]["id"];

function isSection(value: string): value is SectionId {
  return SECTIONS.some((s) => s.id === value);
}

const SettingsRoute: Component = () => {
  const params = useParams<{ section: string }>();

  return (
    <Switch>
      <Match when={!isSection(params.section)}>
        <Navigate href="/settings/sources" />
      </Match>
      <Match when={true}>
        <>
          {/*
            This used to be this route's OWN `w-64` rail, drawn beside its
            content. The shell now owns the one left rail on screen (§5 of the
            porting spec) — a second rail next to it is exactly what that
            change removes — so the section list is handed to the shell's
            contextual zone instead. `SidebarPanel` renders nothing where it
            sits; the shell places it, and withdraws it automatically when this
            route unmounts.

            The links themselves are unchanged: real `<A>`s with the section in
            the path, so deep links, ⌘-click and the back button all keep
            working exactly as they did. The 2px accent rail is drawn at rest
            too, in `border-transparent`, so selecting a section cannot shift
            the row's text by two pixels.
          */}
          <SidebarPanel>
            <nav aria-label="Settings sections" class="flex flex-col gap-2xs">
              <For each={SECTIONS}>
                {(section) => (
                  <A
                    href={`/settings/${section.id}`}
                    aria-current={params.section === section.id ? "page" : undefined}
                    class={cn(
                      "flex h-9 shrink-0 items-center border-l-2 px-md text-item",
                      "transition-colors duration-100",
                      params.section === section.id
                        ? "border-accent bg-raised font-medium text-ink"
                        : "border-transparent text-ink-muted hover:bg-raised hover:text-ink",
                    )}
                  >
                    {section.label}
                  </A>
                )}
              </For>
            </nav>
          </SidebarPanel>

          {/*
            Just the content column now — the rail that used to sit beside it
            was this route's own. The measure is re-judged rather than carried
            over: `max-w-3xl` (48rem), down from the recent `max-w-4xl` (56rem).
            That cap was sized to sit beside a `w-64` (256px) rail this route
            drew itself; with the rail gone the column now starts 256px further
            left on screen, and holding 56rem here would let a form that is a
            couple of fields and a save button run well past a comfortable
            reading measure on a wide display. 48rem is still comfortably enough
            for tuning's `17rem` label column plus a workable control beside it
            — that pairing, not the rail that used to be next to it, is what
            actually sizes this cap.
          */}
          <div class="min-h-0 w-full flex-1 overflow-auto">
            <div class="flex w-full max-w-3xl flex-col gap-xl p-lg">
              <Switch>
                <Match when={params.section === "sources"}>
                  <SourcesSection />
                </Match>
                <Match when={params.section === "channels"}>
                  <ChannelsSection />
                </Match>
                <Match when={params.section === "policies"}>
                  <PoliciesSection />
                </Match>
                <Match when={params.section === "tuning"}>
                  <TuningSection />
                </Match>
                <Match when={params.section === "tokens"}>
                  <TokensSection />
                </Match>
              </Switch>
            </div>
          </div>
        </>
      </Match>
    </Switch>
  );
};

export default SettingsRoute;
