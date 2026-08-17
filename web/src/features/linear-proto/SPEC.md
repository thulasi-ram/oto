# linear-proto — Design Spec (LOCKED)

Target: a Linear.app-grade dense issue list, rendered in a Bauhaus visual language.

**This file is authoritative.** Where this spec and an existing component disagree,
the component is wrong. Where this spec and your taste disagree, the spec wins.

---

## 0. The governing decision

Two instructions were given, in order: (1) clone Linear's exact palette, density and
layout mechanics; (2) "the UI should be Bauhaus styles and minimalistic where needed".

They are resolved as:

> **Linear owns the measurements. Bauhaus owns the shapes.**

- Every hex and every spacing number from the Linear brief ships **verbatim**. They are
  literal spec, and they already form a red / amber / blue triad.
- Bauhaus expresses itself as **geometry, subtraction and weight** — never as poster
  colour. Saturated primaries (`#f00`, `#00f`, `#ff0`) vibrate against a `#0f0f11`
  canvas and fail contrast; they appear **nowhere** except the empty-state composition.
- Radius is `0` everywhere. This is the one place the two instructions collide (the
  brief says keycaps are `rounded`), and Bauhaus + the repo's own `--oto-radius-*: 0px`
  convention both win. Keycaps are `rounded-none`.

### Isolation contract

This prototype is **sealed off** from the oto design system:

- All **colour** comes from `--lp-*` variables scoped under `.linear-proto` in
  `linear-proto.css`. Never read or write `--oto-*` *colour* tokens here.
- The **type and radius scales are shared with oto**, not re-invented. oto's ladder is
  10 / 11 / 12 / 13 / 14 / 18px and every radius step is `0px` — that is already a
  dense-tool scale identical to Linear's, so binding to it costs no fidelity whatsoever.
  Colour is what makes this prototype foreign to oto; size is not.
- Never import from `~/components/ui/**`, `~/design/**`, or `~/features/alerts/**`.
- Never modify anything outside `web/src/features/linear-proto/**` and
  `web/src/routes/linear-issues.tsx`.
- **The oto design tests stay armed.** `scales.test.ts` and `index.css.test.ts` scan this
  directory like any other. They ban Tailwind's native `text-xs` / `text-sm` /
  `text-[10px]` ladder and the bare `rounded` ladder in markup, **and** they ban any raw
  `font-size:` / `border-radius:` declaration in a stylesheet that does not point at
  `var(--oto-type-*)` / `var(--oto-radius-*)`.

  We do **not** exempt ourselves, and we do **not** invent a parallel scale. Two earlier
  attempts were rejected: excluding this directory from the tests (it would protect the
  *directory*, so anything later dropped here inherits the blind spot, and promoting a
  component would smuggle banned utilities past a blinded test), and defining scoped
  `lp-t-*` classes (blocked by the stylesheet rule, and it duplicated a scale that
  already existed). Use the oto type utilities — see §4.

---

## 1. Colour

Declared once, in `linear-proto.css`, scoped to `.linear-proto`.

| Var | Value | Use |
|---|---|---|
| `--lp-canvas` | `#0f0f11` | app background, side panel |
| `--lp-surface` | `#151619` | list surface, group header strip |
| `--lp-overlay` | `#1c1d21` | popovers, dropdowns, modals, command palette |
| `--lp-border` | `rgba(255,255,255,0.08)` | default 1px hairline |
| `--lp-border-strong` | `#23252a` | panel separators |
| `--lp-text` | `#f7f8f8` | primary text (issue titles) |
| `--lp-text-2` | `#8a8f98` | secondary / metadata |
| `--lp-text-3` | `#62666d` | muted / shortcuts / issue keys |
| `--lp-accent` | `#5e6ad2` | interactive, focus, Done |
| `--lp-red` | `#f26522` | Urgent — and nothing else |
| `--lp-amber` | `#f5a623` | High, In Progress — and nothing else |
| `--lp-neutral` | `#62666d` | Medium, Low, None, Backlog |
| `--lp-neutral-2` | `#8a8f98` | Todo |

**The three-hue rule.** Red, amber and blue are a semantic alphabet: hue *is* meaning.
No decorative colour anywhere. The only exemptions are label dots and avatar
backgrounds, which are data-driven identity colours (see `AVATAR_COLORS`,
`LABEL_COLORS` in `types.ts`).

State colours:

- Row hover — `bg-white/[0.04]`, `transition-colors duration-150`
- Row selected/focused — `border-l-2 border-l-[--lp-accent]` + `bg-[#5e6ad2]/10`
- Every row carries `border-l-2 border-l-transparent` at rest so the focus rail
  **cannot** cause a 2px horizontal shift.

---

## 2. Geometry — the Bauhaus alphabet

Three primitives, three jobs. All glyphs are `w-3.5 h-3.5` (14px), 1.5px stroke, no
gradient, no rounded joins, no drop shadow.

### Status = CIRCLE, encoded by monotonic fill

Status is *ordered*, so the visual variable must be ordered too. Fill fraction is.

| Status | Glyph | Colour |
|---|---|---|
| Backlog | dotted-stroke ring, empty | `--lp-neutral` |
| Todo | solid-stroke ring, empty | `--lp-neutral-2` |
| In Progress | ring + filled half-disc | `--lp-amber` |
| Done | filled disc + inset check | `--lp-accent` |
| Canceled | ring + diagonal slash | `--lp-neutral` |

### Priority = RECTANGLE bars, encoded by count; TRIANGLE for urgent

Priority is also ordered, but must be *distinguishable from status at 14px in the
adjacent column* — so it uses a different primitive family.

| Priority | Glyph | Colour |
|---|---|---|
| Urgent | filled triangle, apex up, with a bar+dot cut out | `--lp-red` |
| High | three bars, all filled, ascending height | `--lp-amber` |
| Medium | three bars, two filled | `--lp-neutral` |
| Low | three bars, one filled | `--lp-neutral` |
| None | three faint unfilled bars | `--lp-text-3` at 50% |

Unfilled bars render at ~30% opacity so the glyph keeps a constant bounding box and the
column never appears to jitter between rows.

### Everything else stays quiet

Avatars are circles (`rounded-full`) — a Bauhaus primitive, not a rounded rectangle.
Label dots are 6px circles. Nothing else is a primitive. If every element shouts
geometry, the alphabet stops meaning anything.

---

## 3. Density and column mechanics

Import all geometry from `layout.ts`. Never hand-write a column width — the header and
the body cells must reference the same constant or they will drift.

- Row: `h-9`, `px-4`, `gap-2`, `flex items-center`. Fixed height; content truncates.
- View header: `h-11`, `px-4`, `border-b`.
- Group header: `h-9`, `px-4`, surface background.
- Side panel: `w-60`.

Row column order, left to right — **every one `shrink-0` except the title**:

1. `COL.select` `w-5` — checkbox; visible on row hover, on focus, or when any row is selected
2. `COL.priority` `w-5`
3. `COL.status` `w-5`
4. `COL.key` `w-16` — monospace, `tabular-nums`, `--lp-text-3`
5. `COL.title` `flex-1 min-w-0 truncate` — the only elastic column
6. `COL.labels` `max-w-[130px]` — clipped, never wrapped
7. `COL.project` `max-w-[130px] truncate`
8. `COL.avatar` `w-5 h-5`
9. `COL.trailing` `w-24 relative` — see below

### The action-bar swap (no layout shift)

`COL.trailing` is a fixed `w-24 h-5 relative` box. Two children are **stacked
absolutely** inside it, both `inset-0`:

- the target date — `opacity-100`, fades to `opacity-0` on row hover
- the quick-action bar (assign / priority / more) — `opacity-0 pointer-events-none`,
  becomes `opacity-100 pointer-events-auto` on row hover

Because both are absolutely positioned in a fixed-width box, nothing reflows. Use
`transition-opacity duration-150`.

**Required detail:** when a quick-action menu is open, the row must stay in its hover
state even though the pointer has moved into the popover. Track open menus with a
signal and force the action bar visible while any is open — otherwise the menu closes
itself the moment it opens.

---

## 4. Typography

**Never use Tailwind's native size ladder** (`text-xs`, `text-sm`, `text-[10px]`) and
never hand-write a `font-size:` in CSS — see §0. Type comes from oto's own utilities,
which map onto Linear's ladder exactly:

| Role | Classes | px |
|---|---|---|
| Issue title | `text-item font-medium` | 13 / 500 |
| Body, menu items, control rows | `text-item` | 13 / 400 |
| Metadata cells | `text-body` | 12 / 400 |
| Dense secondary text | `text-meta` | 11 / 400 |
| Section labels | `text-micro font-medium uppercase tracking-[0.08em]` | 10 / 500 |
| Issue keys, dates, keycaps | `text-meta font-medium font-mono tabular-nums` | 11 / 500 |

Only three tiers are visible at a glance. Hierarchy comes from **weight and colour**,
never from size inflation — 13px/500/`--lp-text` against 12px/400/`--lp-text-2`
separates more cleanly than two sizes two points apart.

Anything numeric that changes between rows (keys, dates, counts) must be
`tabular-nums` so digits never jitter.

---

## 5. Micro-interactions

- All transitions `duration-150`. Add `motion-reduce:transition-none` everywhere.
- Hover: `bg-white/[0.04]`.
- Keyboard focus: the 2px accent left rail + `bg-[#5e6ad2]/10`. No glow ring;
  `outline-none` on the row, with the rail as the sole indicator.
- Keycap: `bg-white/[0.08] text-[--lp-text-2] border border-white/[0.1] rounded-none px-1.5 py-0.5 text-meta font-medium font-mono tabular-nums leading-none`.
- Overlays (`Popover`, `DropdownMenu`, `Select`): `bg-[--lp-overlay]`, 1px
  `--lp-border`, `rounded-none`, no shadow larger than `shadow-lg`, 4px internal padding,
  menu items `h-7 px-2 text-item`, with a right-aligned keycap where a shortcut exists.

---

## 6. Keyboard model

Roving `tabindex` over rows (exactly one row is tabbable at a time).

| Key | Action |
|---|---|
| `↑` / `↓` | move focus |
| `x` | toggle selection on focused row |
| `Esc` | clear selection, or close the palette |
| `p` | open priority menu on focused row |
| `s` | open status menu on focused row |
| `a` | open assignee menu on focused row |
| `⌘K` | command palette |

Shortcuts must not fire while focus is inside a text input or an open overlay.

---

## 7. Side panel (replaces the top toolbar)

All filter / sort / group control lives in a **left side panel**. The top strip is
reduced to breadcrumb + count only — it holds no controls.

`w-60`, `bg-[--lp-canvas]`, `border-r border-[--lp-border]`, sections separated by
1px rules. Each section opens with a `text-micro font-medium uppercase tracking-[0.08em]` label in
`--lp-text-3`, `h-7`, `px-3`.

1. **Views** — All issues / Active / Backlog. Rows `h-7 px-3 text-item`; active row gets the
   accent left rail, same as list rows.
2. **Filter** — four labelled sub-groups: **Status, Priority, Assignee, Label**. Each is a
   `h-8` disclosure row opening a Kobalte popover of multi-select checkboxes. Applied
   filters render underneath as removable chips (the `text-micro` caps step, `rounded-none`, `×` on
   hover). A "Clear all" appears only when at least one filter is active.
3. **Group by** — single-select radio list: Status / Priority / Assignee / Project / None.
4. **Sort by** — single-select radio list: Manual / Priority / Last updated / Created /
   Target date, plus an asc/desc direction toggle.
5. **Display** — density toggle (Comfortable `h-9` / Compact `h-8`) and a
   "show empty groups" switch.

The panel is collapsible to `w-0` via `[`, and every section is independently
collapsible. Selected state in radio lists is shown by a filled 6px square — not a
checkmark.

**The accent is spent exactly once at rest.** Only the active Views row carries it.
Radio squares, segmented controls and switches encode their state with neutrals —
filled-vs-outlined, a raised `bg-white/[0.08]` surface, and a `--lp-text-3` → `--lp-text`
lift — so each reads correctly even in greyscale. An earlier revision put the accent on
all of them, which left a 240px column carrying seven accent marks against the entire
issue list's one focus rail: the chrome out-shouted the content it exists to filter.
Quiet structure is the requirement; the accent marks *where you are*, nothing else.

---

## 8. Empty state

The single place full-saturation Bauhaus is permitted, because density is zero and
nothing is being scanned: a composition of circle / square / triangle in red, blue and
yellow on the canvas, with a one-line caption in `--lp-text-2` and a keycap hint.

---

## 9. Engineering rules

- SolidJS: `createSignal`, `createMemo`, `<For>`, `<Show>`, `<Switch>`. Never `.map()` in JSX.
- Never destructure props (it breaks reactivity). Access as `props.x` at the use site.
- All overlays are Kobalte (`@kobalte/core`) via the local `primitives/` wrappers.
  Do not import Kobalte directly into feature components.
- Class merging via `cn()` from `~/lib/cn`. Variants via `cva` where a component has
  three or more visual states.
- Named exports for components; `export default` only for route modules.
- No new npm dependencies. No network calls — `mockData.ts` is the only data source.
- Strict TypeScript: no `any`, no non-null `!` assertions.
- Do not run tests, do not start the dev server, do not run git commands.
