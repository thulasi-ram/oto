/**
 * `/notifications/:section` — the routing rules, and the record of what they did.
 *
 * ⭐ WHY THIS IS A DESTINATION AND NOT A SETTINGS BAND (ADR 0034). Policies used
 * to be the third of five sections under `/settings`, filed between the channel
 * list and the access tokens. Two things were wrong with that. The question a
 * policy answers — *"who hears about this, and why did nobody hear about that"* —
 * is asked while looking at alerts, not while configuring the product; and the
 * other half of that answer, the activity log, is not configuration at all. It is
 * an operational read, the same kind of object as the alert list, and putting it
 * behind a gear icon would have hidden the one screen that proves oto's silence
 * was deliberate.
 *
 * The two sections are genuine destinations — each is linkable, each is a place
 * you go and stay — so they use the shell's contextual zone exactly as settings
 * does, and for the reason `AppShell` states: the zone is for where you can go,
 * never for what you are filtering.
 */
import { For, Match, Switch, type Component } from "solid-js";
import { Navigate, useParams } from "@solidjs/router";

import { cn } from "~/lib/cn";
import { SidebarPanel, SubNavLink } from "~/components/SidebarSlot";
import { ActivitySection } from "~/features/notifications/ActivitySection";
import { PoliciesSection } from "~/features/notifications/PoliciesSection";

/*
 * Policies first because it is the one that is *written*: an operator arriving
 * here for the first time has no policies, and every row the activity log could
 * show them would say `no_policy`. Activity second, and it is the one they come
 * back for.
 */
const SECTIONS = [
  { id: "policies", label: "Policies" },
  { id: "activity", label: "Activity log" },
] as const;

type SectionId = (typeof SECTIONS)[number]["id"];

/** `undefined` is the bare `/notifications`, which the rail links to. */
function isSection(value: string | undefined): value is SectionId {
  return SECTIONS.some((s) => s.id === value);
}

const NotificationsRoute: Component = () => {
  const params = useParams<{ section?: string }>();

  return (
    <Switch>
      {/* Covers both the bare path and a typo'd one, and lands on the first
          section — which is Policies for the reason `SECTIONS` states. */}
      <Match when={!isSection(params.section)}>
        <Navigate href={`/notifications/${SECTIONS[0].id}`} />
      </Match>
      <Match when={true}>
        <>
          {/* The section list is handed to the shell, which draws it INDENTED
              UNDER the "Notifications" entry of the primary nav and withdraws it
              when this route unmounts. No `<nav>` of its own: it lands inside the
              shell's own navigation landmark, and a second one nested there
              would announce these links twice.

              The links are real `<A>`s with the section in the path, so deep
              links, ⌘-click and the back button all keep working. `SubNavLink`
              owns the appearance, shared with `routes/settings.tsx` — the two
              lists occupy the same pixels on different paths and a second copy
              of the class string is how they would drift apart. */}
          <SidebarPanel>
            <For each={SECTIONS}>
              {(section) => (
                <SubNavLink
                  href={`/notifications/${section.id}`}
                  current={params.section === section.id}
                >
                  {section.label}
                </SubNavLink>
              )}
            </For>
          </SidebarPanel>

          {/*
            Two different measures, because these are two different kinds of
            screen. The policy editor is a form and reads at `max-w-3xl`, the cap
            settings uses and for the same reason. The activity log is a table of
            rows whose right-hand edge carries a timestamp; capping it at a
            reading measure would strand that column in the middle of a wide
            display, so it takes the width it is given.
          */}
          <div class="min-h-0 w-full flex-1 overflow-auto">
            <div
              class={cn(
                "flex w-full flex-col gap-xl p-lg",
                params.section === "policies" ? "max-w-3xl" : "",
              )}
            >
              <Switch>
                <Match when={params.section === "policies"}>
                  <PoliciesSection />
                </Match>
                <Match when={params.section === "activity"}>
                  <ActivitySection />
                </Match>
              </Switch>
            </div>
          </div>
        </>
      </Match>
    </Switch>
  );
};

export default NotificationsRoute;
