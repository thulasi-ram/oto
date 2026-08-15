/**
 * `/settings/:section` — sources, clusters, channels and policies.
 *
 * The section is in the path rather than in component state so a settings screen
 * is linkable like everything else. "Look at the channel config" should be a URL.
 */
import { Match, Switch, type Component } from "solid-js";
import { A, Navigate, useParams } from "@solidjs/router";

import { cx } from "~/components/ui/primitives";
import { ChannelsSection } from "~/features/settings/ChannelsSection";
import { PoliciesSection } from "~/features/settings/PoliciesSection";
import { SourcesSection } from "~/features/settings/SourcesSection";
import { TuningSection } from "~/features/settings/TuningSection";

const SECTIONS = [
  { id: "sources", label: "Sources and clusters" },
  { id: "channels", label: "Channels" },
  { id: "policies", label: "Notification policies" },
  { id: "tuning", label: "Tuning" },
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
        <div class="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-4 overflow-auto p-4">
          <nav
            aria-label="Settings sections"
            class="flex shrink-0 items-center gap-1 border-b border-line"
          >
            {SECTIONS.map((section) => (
              <A
                href={`/settings/${section.id}`}
                aria-current={params.section === section.id ? "page" : undefined}
                class={cx(
                  "-mb-px border-b-2 px-2.5 py-1.5 text-item transition-colors duration-100",
                  params.section === section.id
                    ? "border-accent font-medium text-ink"
                    : "border-transparent text-ink-muted hover:text-ink",
                )}
              >
                {section.label}
              </A>
            ))}
          </nav>

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
          </Switch>
        </div>
      </Match>
    </Switch>
  );
};

export default SettingsRoute;
