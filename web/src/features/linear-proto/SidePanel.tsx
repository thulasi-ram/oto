import { createMemo, createSignal, For, Show, type JSX } from "solid-js";

import { cn } from "~/lib/cn";

import { SIDE_PANEL_W } from "./layout";
import { MOCK_ASSIGNEES, MOCK_LABELS } from "./mockData";
import { Avatar } from "./primitives/Avatar";
import { Checkbox } from "./primitives/Checkbox";
import { Keycap } from "./primitives/Keycap";
import { Popover } from "./primitives/Popover";
import { PriorityIcon } from "./primitives/PriorityIcon";
import { StatusIcon } from "./primitives/StatusIcon";
import {
  GROUP_BY_OPTIONS,
  SORT_BY_OPTIONS,
  useIssueView,
  type Density,
  type FilterKey,
  type SortDir,
} from "./store";
import {
  PRIORITY_LABEL,
  PRIORITY_ORDER,
  STATUS_LABEL,
  STATUS_ORDER,
  type Status,
} from "./types";

/**
 * The left control panel — SPEC §7.
 *
 * Every filter / group / sort / display control in the prototype lives here, in
 * five labelled sections separated by hairlines. Nothing here owns state: the
 * panel is a pure view over `useIssueView()`.
 *
 * Vertical rhythm is fixed and deliberately narrow: `h-7` for every control row
 * and every section label, `h-8` only for the four filter disclosures. Exactly
 * one control type size (`text-item`) throughout, plus the `text-micro` caps step
 * for section labels, chips and every count — a count is one role and wears one
 * treatment everywhere, matching `GroupHeader`. Depth is borders-only: no shadow
 * in this file.
 *
 * Radius is 0 everywhere, and the accent hue appears exactly ONCE at rest: on the
 * active Views row. Every other selected state in this panel — radio squares,
 * segmented controls, the switch — is expressed in `--lp-text`, a raised neutral
 * surface and position, never in hue. A 240px column carrying seven accent marks
 * out-shouts the entire issue list it exists to filter; one mark tells you where
 * you are and lets the list be the thing you look at.
 */

/* ------------------------------------------------------------------ shared */

/**
 * Every row carries a 2px transparent left rail so that selecting one cannot
 * shift its text sideways (SPEC §1). Section labels carry it too, which is what
 * keeps label text and row text on the same 14px optical margin.
 *
 * `active:scale-[0.97]` is the press acknowledgement. The transition names its
 * properties — never `transition-all`, which would sweep up layout properties
 * nobody asked to animate — and puts the transform on the same 150ms
 * `--lp-ease` curve as the colour change.
 */
const RAIL = "border-l-2 border-l-transparent";

const ROW =
  "w-full flex items-center gap-2 pl-3 pr-2 text-left outline-none " +
  "transition-[color,background-color,border-color,transform] duration-150 " +
  "ease-[var(--lp-ease)] active:scale-[0.97] motion-reduce:transition-none " +
  RAIL;

const ROW_QUIET =
  "text-[var(--lp-text-2)] hover:bg-white/[0.04] focus-visible:bg-white/[0.06]";

/**
 * "A count" is ONE role across the prototype, so it gets one treatment: the
 * `text-micro` caps step, `tabular-nums` because the digits change under you.
 * `GroupHeader` already renders its count this way; the panel and the view
 * header follow it rather than inventing a second, larger count.
 */
const COUNT =
  "text-micro font-medium uppercase tracking-[0.08em] tabular-nums shrink-0";

/**
 * Filter-popover entrance. The MOTION itself belongs to the `Popover` primitive,
 * which animates its content with a keyframe; a duplicate `@starting-style`
 * transition here would be dead code, because a CSS animation outranks a
 * transition on the same properties.
 *
 * What this class still owns is the ORIGIN: Kobalte resolves the popover's
 * placement at open time and publishes it as a variable, so the content grows
 * out of the disclosure that opened it rather than out of its own centre.
 */
const POPOVER_MOTION = "origin-[var(--kb-popover-content-transform-origin)]";

interface Option {
  value: string;
  label: string;
  /** A thunk, not a node: the same option renders in both the list and a chip. */
  glyph: () => JSX.Element;
}

/* ----------------------------------------------------------- filter options */

const STATUS_OPTIONS: readonly Option[] = STATUS_ORDER.map((status) => ({
  value: status,
  label: STATUS_LABEL[status],
  glyph: () => <StatusIcon status={status} />,
}));

const PRIORITY_OPTIONS: readonly Option[] = [...PRIORITY_ORDER]
  .reverse()
  .map((priority) => ({
    value: priority,
    label: PRIORITY_LABEL[priority],
    glyph: () => <PriorityIcon priority={priority} />,
  }));

const ASSIGNEE_OPTIONS: readonly Option[] = [
  ...MOCK_ASSIGNEES.map((assignee) => ({
    value: assignee.id,
    label: assignee.name,
    glyph: () => <Avatar assignee={assignee} class="w-3.5 h-3.5" />,
  })),
  {
    value: "unassigned",
    label: "Unassigned",
    glyph: () => <Avatar assignee={null} class="w-3.5 h-3.5" />,
  },
];

const LABEL_OPTIONS: readonly Option[] = MOCK_LABELS.map((label) => ({
  value: label.id,
  label: label.name,
  glyph: () => <LabelDot color={label.color} />,
}));

const FILTER_GROUPS: readonly { key: FilterKey; label: string; options: readonly Option[] }[] =
  [
    { key: "status", label: "Status", options: STATUS_OPTIONS },
    { key: "priority", label: "Priority", options: PRIORITY_OPTIONS },
    { key: "assignee", label: "Assignee", options: ASSIGNEE_OPTIONS },
    { key: "label", label: "Label", options: LABEL_OPTIONS },
  ];

/* ------------------------------------------------------------ view presets */

/**
 * The saved views are expressed purely as status filters, so "which view am I
 * in" is derived from the filter state rather than stored twice. Touching the
 * Status filter directly simply drops you into no named view.
 */
const VIEW_PRESETS: readonly { id: string; label: string; status: readonly Status[] }[] = [
  { id: "all", label: "All issues", status: [] },
  { id: "active", label: "Active", status: ["todo", "in_progress"] },
  { id: "backlog", label: "Backlog", status: ["backlog"] },
];

const DENSITY_OPTIONS: readonly { value: Density; label: string }[] = [
  { value: "comfortable", label: "Comfortable" },
  { value: "compact", label: "Compact" },
];

const SORT_DIR_OPTIONS: readonly { value: SortDir; label: string }[] = [
  { value: "asc", label: "Ascending" },
  { value: "desc", label: "Descending" },
];

/* ------------------------------------------------------------------- panel */

export function SidePanel() {
  const view = useIssueView();

  const statusFilter = () => view.filters().status;

  const activePreset = createMemo(() => {
    const current = statusFilter();
    const match = VIEW_PRESETS.find(
      (preset) =>
        preset.status.length === current.length &&
        preset.status.every((status) => current.includes(status)),
    );
    return match ? match.id : null;
  });

  const applyPreset = (preset: (typeof VIEW_PRESETS)[number]): void => {
    view.clearFilter("status");
    for (const status of preset.status) view.toggleFilter("status", status);
  };

  const chips = createMemo(() => {
    const current = view.filters();
    const out: { key: FilterKey; value: string; label: string; glyph: () => JSX.Element }[] =
      [];
    for (const group of FILTER_GROUPS) {
      for (const value of current[group.key]) {
        const option = group.options.find((candidate) => candidate.value === value);
        if (!option) continue;
        out.push({ key: group.key, value, label: option.label, glyph: option.glyph });
      }
    }
    return out;
  });

  /** Manual order has no direction, and only status/priority groups can be empty. */
  const dirDisabled = () => view.sortBy() === "manual";
  const emptyGroupsDisabled = () =>
    view.groupBy() !== "status" && view.groupBy() !== "priority";

  return (
    <aside
      aria-label="View controls"
      class={cn(
        "h-full shrink-0 overflow-hidden bg-[var(--lp-canvas)]",
        "transition-[width] duration-150 motion-reduce:transition-none",
        view.panelCollapsed()
          ? "w-0"
          : cn(SIDE_PANEL_W, "border-r border-[var(--lp-border)]"),
      )}
    >
      {/*
        Fixed inner width so nothing reflows while the panel animates shut, and
        `invisible` (not `hidden`) so the collapsed panel leaves both the tab
        order and the accessibility tree without unmounting.
      */}
      <div
        class={cn(SIDE_PANEL_W, "h-full flex flex-col", view.panelCollapsed() && "invisible")}
      >
        <div class="flex-1 overflow-y-auto">
          {/* ---------------------------------------------------------- Views */}
          <Section label="Views">
            <For each={VIEW_PRESETS}>
              {(preset) => (
                <button
                  type="button"
                  aria-current={activePreset() === preset.id ? "page" : undefined}
                  onClick={() => applyPreset(preset)}
                  class={cn(
                    ROW,
                    "text-item h-7",
                    activePreset() === preset.id
                      ? "border-l-[var(--lp-accent)] bg-[#5e6ad2]/10 text-[var(--lp-text)]"
                      : ROW_QUIET,
                  )}
                >
                  <span class="flex-1 truncate">{preset.label}</span>
                </button>
              )}
            </For>
          </Section>

          {/* --------------------------------------------------------- Filter */}
          <Section label="Filter">
            <For each={FILTER_GROUPS}>
              {(group) => <FilterDisclosure group={group} />}
            </For>

            <Show when={chips().length > 0}>
              <div class="mt-2 pt-2 border-t border-[var(--lp-border)]">
                <div class="px-3 flex flex-wrap gap-1">
                  <For each={chips()}>
                    {(chip) => (
                      <FilterChip
                        label={chip.label}
                        glyph={chip.glyph}
                        onRemove={() => view.toggleFilter(chip.key, chip.value)}
                      />
                    )}
                  </For>
                </div>

                <button
                  type="button"
                  onClick={() => view.clearAllFilters()}
                  class={cn(
                    ROW,
                    "text-item h-7 mt-1 text-[var(--lp-text-3)]",
                    "hover:bg-white/[0.04] hover:text-[var(--lp-text)]",
                    "focus-visible:bg-white/[0.06] focus-visible:text-[var(--lp-text)]",
                  )}
                >
                  <span class="flex-1">Clear all</span>
                  <span class={COUNT}>{view.activeFilterCount()}</span>
                </button>
              </div>
            </Show>
          </Section>

          {/* ------------------------------------------------------- Group by */}
          <Section label="Group by">
            <div role="radiogroup" aria-label="Group by">
              <For each={GROUP_BY_OPTIONS}>
                {(option) => (
                  <RadioRow
                    label={option.label}
                    selected={view.groupBy() === option.value}
                    onSelect={() => view.setGroupBy(option.value)}
                  />
                )}
              </For>
            </div>
          </Section>

          {/* -------------------------------------------------------- Sort by */}
          <Section label="Sort by">
            <div role="radiogroup" aria-label="Sort by">
              <For each={SORT_BY_OPTIONS}>
                {(option) => (
                  <RadioRow
                    label={option.label}
                    selected={view.sortBy() === option.value}
                    onSelect={() => view.setSortBy(option.value)}
                  />
                )}
              </For>
            </div>
            <Segmented
              ariaLabel="Sort direction"
              options={SORT_DIR_OPTIONS}
              value={view.sortDir()}
              disabled={dirDisabled()}
              onSelect={(value) => {
                if (value !== view.sortDir()) view.toggleSortDir();
              }}
            />
          </Section>

          {/* -------------------------------------------------------- Display */}
          <Section label="Display">
            <Segmented
              ariaLabel="Row density"
              options={DENSITY_OPTIONS}
              value={view.density()}
              onSelect={(value) => view.setDensity(value)}
            />
            <button
              type="button"
              role="switch"
              aria-checked={view.showEmptyGroups()}
              aria-disabled={emptyGroupsDisabled() ? true : undefined}
              onClick={() => {
                if (emptyGroupsDisabled()) return;
                view.setShowEmptyGroups(!view.showEmptyGroups());
              }}
              class={cn(
                ROW,
                "text-item h-7 mt-1.5",
                ROW_QUIET,
                emptyGroupsDisabled() && "opacity-40 hover:bg-transparent",
              )}
            >
              <span class="flex-1 truncate">Show empty groups</span>
              <ToggleSwitch on={view.showEmptyGroups()} />
            </button>
          </Section>
        </div>

        <footer class="shrink-0 border-t border-[var(--lp-border)]">
          {/*
            The collapse control is `h-8` — under the 40px minimum — so a
            `::before` extends it to 40 without moving a pixel of paint. It grows
            into its own top border and the last section's 6px bottom padding, so
            no other extended hit area and no other control is overlapped.
          */}
          <button
            type="button"
            onClick={() => view.togglePanel()}
            class={cn(
              ROW,
              "text-item h-8 text-[var(--lp-text-3)]",
              "relative before:absolute before:content-[''] before:inset-x-0 before:-inset-y-1",
              "hover:bg-white/[0.04] hover:text-[var(--lp-text-2)]",
              "focus-visible:bg-white/[0.06] focus-visible:text-[var(--lp-text-2)]",
            )}
          >
            <span class="flex-1">Collapse panel</span>
            <Keycap>[</Keycap>
          </button>
        </footer>
      </div>
    </aside>
  );
}

/* ---------------------------------------------------------------- sections */

/**
 * A labelled, independently collapsible section closed by a 1px rule.
 *
 * The caret row is `h-7`, so a `::before` extends the hit area upward to 34px —
 * upward only, into the hairline and the previous section's 6px of padding,
 * because extending downward would steal the top of the first control row
 * underneath.
 *
 * Collapse animates opacity and transform only, never `height`: an animated
 * height forces layout on every frame and, in a panel of stacked sections,
 * drags every section below it along for the ride. The enter transition rides
 * `@starting-style` (Tailwind's `starting:` variant) so it needs no JS and
 * simply does not run on browsers without it.
 */
function Section(props: { label: string; children: JSX.Element }) {
  const [open, setOpen] = createSignal(true);

  return (
    // The 6px of bottom padding belongs to the section, not to its body, so it
    // survives collapse — which is what guarantees the next header's extended
    // hit area always lands in dead space instead of on the row above it.
    <section class="pb-1.5 border-b border-[var(--lp-border)] last:border-b-0">
      <button
        type="button"
        aria-expanded={open()}
        onClick={() => setOpen((value) => !value)}
        class={cn(
          ROW,
          "text-micro font-medium uppercase tracking-[0.08em] h-7 text-[var(--lp-text-3)]",
          "relative before:absolute before:content-[''] before:inset-x-0",
          "before:-top-1.5 before:bottom-0",
          "hover:text-[var(--lp-text-2)] focus-visible:text-[var(--lp-text-2)]",
        )}
      >
        <span class="flex-1">{props.label}</span>
        <Caret open={open()} />
      </button>

      <Show when={open()}>
        <div
          class={cn(
            "pb-0.5 origin-top transition-[opacity,transform] duration-150",
            "ease-[var(--lp-ease)] starting:opacity-0 starting:-translate-y-0.5",
            "motion-reduce:transition-none",
          )}
        >
          {props.children}
        </div>
      </Show>
    </section>
  );
}

/**
 * The prototype's ONE disclosure caret: the same stroked chevron `GroupHeader`
 * draws — identical viewBox, path, 1.5 stroke and square cap — simply rendered
 * smaller, so the two never read as two different caret systems.
 *
 * It must not be a filled triangle. A filled triangle is the Urgent priority
 * glyph (SPEC §2), and inside the Priority filter popover that caret would sit
 * inches from the real thing and claim a meaning it does not have.
 *
 * Open points down at what it owns; closed rotates to point right.
 */
function Caret(props: { open: boolean }) {
  return (
    <svg
      viewBox="0 0 14 14"
      aria-hidden="true"
      fill="none"
      class={cn(
        "w-2 h-2 shrink-0 transition-transform duration-150 motion-reduce:transition-none",
        !props.open && "-rotate-90",
      )}
    >
      <path
        d="M3.5 5.5 L7 9 L10.5 5.5"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="square"
      />
    </svg>
  );
}

/* ------------------------------------------------------------------ filter */

function FilterDisclosure(props: {
  group: { key: FilterKey; label: string; options: readonly Option[] };
}) {
  const view = useIssueView();
  const [open, setOpen] = createSignal(false);

  const applied = (): readonly string[] => view.filters()[props.group.key];
  const isChecked = (value: string) => applied().includes(value);

  return (
    // The Popover primitive renders its own <button> around `trigger`; `flex
    // *:w-full` stretches that button to the panel width and drops the inline
    // baseline gap it would otherwise leave beneath itself.
    <div class="flex *:w-full">
      <Popover
        class={cn("w-56", POPOVER_MOTION)}
        open={open()}
        onOpenChange={setOpen}
        trigger={
          <span
            class={cn(
              ROW,
              "text-item h-8 cursor-default",
              applied().length > 0 ? "text-[var(--lp-text)]" : "text-[var(--lp-text-2)]",
              "hover:bg-white/[0.04]",
            )}
          >
            <span class="flex-1 truncate">{props.group.label}</span>
            <Show
              when={applied().length > 0}
              fallback={<span class={cn(COUNT, "text-[var(--lp-text-3)]")}>Any</span>}
            >
              {/*
                Deliberately NOT accent: a count is not a selection, and four
                accent pips down the panel pull the eye off the issue list.
              */}
              <span class={cn(COUNT, "text-[var(--lp-text)]")}>{applied().length}</span>
            </Show>
            <Caret open={open()} />
          </span>
        }
      >
        <div class="py-1">
          <For each={props.group.options}>
            {(option) => (
              <OptionRow
                option={option}
                checked={isChecked(option.value)}
                onToggle={() => view.toggleFilter(props.group.key, option.value)}
              />
            )}
          </For>

          <Show when={applied().length > 0}>
            <div class="mt-1 pt-1 border-t border-[var(--lp-border)]">
              <button
                type="button"
                onClick={() => view.clearFilter(props.group.key)}
                class={cn(
                  "text-item w-full h-7 px-2 flex items-center text-left outline-none",
                  "text-[var(--lp-text-3)] active:scale-[0.97]",
                  "transition-[color,background-color,transform] duration-150",
                  "ease-[var(--lp-ease)] motion-reduce:transition-none",
                  "hover:bg-white/[0.04] hover:text-[var(--lp-text)]",
                  "focus-visible:bg-white/[0.06] focus-visible:text-[var(--lp-text)]",
                )}
              >
                Clear {props.group.label.toLowerCase()}
              </button>
            </div>
          </Show>
        </div>
      </Popover>
    </div>
  );
}

/**
 * A multi-select row inside a filter popover.
 *
 * The Checkbox owns the whole hit area and the accessible name (its own label),
 * so there is exactly one focus stop and one toggle per row — no wrapper button
 * shadowing the control. The matching glyph sits in a fixed trailing column,
 * which also gives the popover a clean right rail of status / priority / avatar
 * marks to scan down.
 */
function OptionRow(props: { option: Option; checked: boolean; onToggle: () => void }) {
  return (
    <div
      class={cn(
        "text-item h-7 px-2 flex items-center gap-2",
        "transition-colors duration-150 motion-reduce:transition-none",
        "hover:bg-white/[0.04]",
      )}
    >
      <Checkbox
        checked={props.checked}
        onChange={() => props.onToggle()}
        label={props.option.label}
        class="flex-1 min-w-0"
      />
      <span class="w-3.5 h-3.5 shrink-0 flex items-center justify-center">
        {props.option.glyph()}
      </span>
    </div>
  );
}

/**
 * An applied filter. The `×` reveals on hover/focus but always occupies width.
 *
 * That `×` is a 16×20 target, so a `::before` grows it outward — but only into
 * dead space. Horizontally the inset is HALF the 4px `gap-1` the chips wrap on,
 * so two side-by-side chips' targets meet exactly at the middle of the gap and
 * neither reaches across onto its neighbour's `×`. Vertically it takes the whole
 * 4px, which stops at the next chip row's edge without crossing it.
 */
function FilterChip(props: {
  label: string;
  glyph: () => JSX.Element;
  onRemove: () => void;
}) {
  return (
    <span
      class={cn(
        "text-micro font-medium uppercase tracking-[0.08em] group/chip h-5 max-w-full inline-flex items-center gap-1 pl-1",
        "border border-[var(--lp-border)] bg-white/[0.03] text-[var(--lp-text-2)]",
      )}
    >
      <span class="shrink-0 flex items-center">{props.glyph()}</span>
      <span class="truncate">{props.label}</span>
      <button
        type="button"
        aria-label={`Remove filter ${props.label}`}
        onClick={() => props.onRemove()}
        class={cn(
          "w-4 h-full shrink-0 flex items-center justify-center leading-none outline-none",
          "relative before:absolute before:content-[''] before:-inset-x-0.5 before:-inset-y-1",
          "text-[var(--lp-text-3)] opacity-0 active:scale-[0.97]",
          "transition-[opacity,color,transform] duration-150 ease-[var(--lp-ease)]",
          "motion-reduce:transition-none",
          "group-hover/chip:opacity-100 focus-visible:opacity-100",
          "hover:text-[var(--lp-text)]",
        )}
      >
        ×
      </button>
    </span>
  );
}

function LabelDot(props: { color: string }) {
  return (
    <span
      aria-hidden="true"
      class="w-1.5 h-1.5 shrink-0 rounded-full"
      style={{ "background-color": props.color }}
    />
  );
}

/* ------------------------------------------------------------- selection UI */

/**
 * Single-select row. Selection is a filled 6px square, never a tick.
 *
 * The square is `--lp-text`, not the accent: ten of these squares live in the
 * panel, and hue spent on all of them would out-shout the one accent mark that
 * says which view you are in. Filled-vs-outlined is already an unambiguous
 * binary, and it is reinforced by the label lifting from `--lp-text-2` to
 * `--lp-text` — the same weight/colour hierarchy §4 uses everywhere else.
 */
function RadioRow(props: { label: string; selected: boolean; onSelect: () => void }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={props.selected}
      onClick={() => props.onSelect()}
      class={cn(
        ROW,
        "text-item h-7 hover:bg-white/[0.04] focus-visible:bg-white/[0.06]",
        props.selected ? "text-[var(--lp-text)]" : "text-[var(--lp-text-2)]",
      )}
    >
      <span class="w-3 h-3 shrink-0 flex items-center justify-center">
        <span
          class={cn(
            "w-1.5 h-1.5",
            props.selected
              ? "bg-[var(--lp-text)]"
              : "border border-[var(--lp-border-strong)]",
          )}
        />
      </span>
      <span class="flex-1 truncate">{props.label}</span>
    </button>
  );
}

/**
 * Two-up square toggle. Used for sort direction and row density.
 *
 * The pressed half is a RAISED NEUTRAL surface (`bg-white/[0.08]`, one step
 * above the row-hover wash) with `--lp-text` on it, not an accent tint. Two
 * segmented controls sit within 100px of each other in this panel; tinting both
 * would put four coloured marks in the eye's path to the list.
 */
function Segmented<T extends string>(props: {
  ariaLabel: string;
  options: readonly { value: T; label: string }[];
  value: T;
  onSelect: (value: T) => void;
  disabled?: boolean;
}) {
  return (
    <div class="pl-3.5 pr-3 pt-1.5">
      <div
        role="group"
        aria-label={props.ariaLabel}
        class={cn(
          "h-7 flex border border-[var(--lp-border-strong)]",
          props.disabled && "opacity-40 pointer-events-none",
        )}
      >
        <For each={props.options}>
          {(option, index) => (
            <>
              <Show when={index() > 0}>
                <span aria-hidden="true" class="w-px bg-[var(--lp-border-strong)]" />
              </Show>
              <button
                type="button"
                aria-pressed={props.value === option.value}
                tabindex={props.disabled ? -1 : 0}
                onClick={() => props.onSelect(option.value)}
                class={cn(
                  "text-item flex-1 h-full outline-none active:scale-[0.97]",
                  "transition-[color,background-color,transform] duration-150",
                  "ease-[var(--lp-ease)] motion-reduce:transition-none",
                  props.value === option.value
                    ? "bg-white/[0.08] text-[var(--lp-text)]"
                    : "text-[var(--lp-text-2)] hover:bg-white/[0.04] focus-visible:bg-white/[0.06]",
                )}
              >
                {option.label}
              </button>
            </>
          )}
        </For>
      </div>
    </div>
  );
}

/**
 * Square switch — zero radius, constant footprint, no accent.
 *
 * "On" is carried by three neutral signals at once: the knob sits at the far
 * end of the track, the track fills with a raised neutral wash, and the knob
 * lifts from `--lp-text-3` to full `--lp-text`. Position alone already reads at
 * a glance; the value change makes it survive a greyscale glance too. Nothing
 * here needs hue, and the panel keeps its single accent mark.
 */
/**
 * The switch track + knob.
 *
 * NOT named `Switch`: the Solid JSX transform treats `<Switch>` as a built-in
 * control-flow component by NAME and auto-imports solid-js's version, silently
 * shadowing any local declaration. A local `Switch` therefore never renders —
 * Solid's does, receives no `<Match>` children, and throws at runtime. The same
 * trap applies to Show / For / Index / Match / Dynamic / Portal / Suspense /
 * ErrorBoundary, so never name a component after one of them.
 */
function ToggleSwitch(props: { on: boolean }) {
  return (
    <span
      aria-hidden="true"
      class={cn(
        "w-7 h-3.5 shrink-0 flex items-center p-px border border-[var(--lp-border-strong)]",
        "transition-colors duration-150 motion-reduce:transition-none",
        props.on ? "justify-end bg-white/[0.08]" : "justify-start",
      )}
    >
      <span
        class={cn("w-3 h-3", props.on ? "bg-[var(--lp-text)]" : "bg-[var(--lp-text-3)]")}
      />
    </span>
  );
}
