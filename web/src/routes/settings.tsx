/**
 * `/settings/:section` — sources, clusters, channels, tuning and tokens.
 *
 * The section is in the path rather than in component state so a settings screen
 * is linkable like everything else. "Look at the channel config" should be a URL.
 *
 * ⛔ NOTIFICATION POLICIES ARE NOT HERE ANY MORE (ADR 0034). They are a
 * destination of their own at `/notifications/policies`, beside the activity log
 * that records what they did. `/settings/policies` still resolves — see
 * `MOVED` — because that URL was linkable for the whole life of the screen and a
 * bookmark that lands on "That page does not exist." teaches an operator that
 * oto loses things.
 */
import { For, Match, Switch, type Component } from "solid-js";
import { Navigate, useParams } from "@solidjs/router";

import { SidebarPanel, SubNavLink } from "~/components/SidebarSlot";
import { ChannelsSection } from "~/features/settings/ChannelsSection";
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
  { id: "tuning", label: "Tuning" },
  { id: "tokens", label: "Access tokens" },
] as const;

type SectionId = (typeof SECTIONS)[number]["id"];

/** `undefined` is the bare `/settings`, which the rail links to. */
function isSection(value: string | undefined): value is SectionId {
  return SECTIONS.some((s) => s.id === value);
}

/**
 * Sections that used to be here, and where they went.
 *
 * A redirect rather than a removal, and a table rather than an `if`: the next
 * section that moves adds a line instead of a branch, and the reader can see at
 * a glance which of yesterday's URLs still resolve.
 */
const MOVED: Readonly<Record<string, string>> = {
  policies: "/notifications/policies",
};

const SettingsRoute: Component = () => {
  const params = useParams<{ section?: string }>();
  const moved = (): string | undefined =>
    params.section === undefined ? undefined : MOVED[params.section];

  return (
    <Switch>
      <Match when={moved()}>{(href) => <Navigate href={href()} />}</Match>
      {/* Covers both the bare `/settings` the rail links to and a typo'd one. */}
      <Match when={!isSection(params.section)}>
        <Navigate href={`/settings/${SECTIONS[0].id}`} />
      </Match>
      <Match when={true}>
        <>
          {/*
            This used to be this route's OWN `w-64` rail, drawn beside its
            content. The shell now owns the one left rail on screen (§5 of the
            porting spec) — a second rail next to it is exactly what that
            change removes — so the section list is handed to the shell instead,
            which draws it INDENTED UNDER the "Settings" entry of the primary
            nav. `SidebarPanel` renders nothing where it sits; the shell places
            it, and withdraws it automatically when this route unmounts.

            No `<nav>` of its own: it lands inside the shell's own navigation
            landmark, and a second one nested there would announce these links
            twice. The links themselves are unchanged — real `<A>`s with the
            section in the path, so deep links, ⌘-click and the back button all
            keep working exactly as they did — and `SubNavLink` owns their
            appearance, shared with `routes/notifications.tsx` so the two lists
            that occupy these same pixels cannot drift apart.
          */}
          <SidebarPanel>
            <For each={SECTIONS}>
              {(section) => (
                <SubNavLink
                  href={`/settings/${section.id}`}
                  current={params.section === section.id}
                >
                  {section.label}
                </SubNavLink>
              )}
            </For>
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
