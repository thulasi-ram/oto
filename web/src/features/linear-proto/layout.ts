/**
 * Column geometry for the issue list.
 *
 * The row is a single flex line. Every column is a fixed structural anchor
 * (shrink-0) except the title, which absorbs all remaining space and truncates.
 * Header cells and body cells import the SAME constant so they can never drift
 * out of alignment.
 */
export const COL = {
  /** Drag handle / selection checkbox. */
  select: "w-5 shrink-0 flex items-center justify-center",
  /** Priority glyph. */
  priority: "w-5 shrink-0 flex items-center justify-center",
  /** Status glyph. */
  status: "w-5 shrink-0 flex items-center justify-center",
  /**
   * Issue key, e.g. OTO-142. Geometry only — apply
   * `text-meta font-medium font-mono tabular-nums` at the use site for the
   * monospace/tabular type (see SPEC.md §4; Tailwind's size ladder is banned here).
   */
  key: "w-16 shrink-0",
  /** The one elastic column. min-w-0 is required for truncate to work in flex. */
  title: "flex-1 min-w-0 truncate",
  /** Label chips; clipped rather than wrapped. */
  labels: "max-w-[130px] shrink-0 flex items-center gap-1 overflow-hidden",
  /** Project chip. */
  project: "max-w-[130px] shrink-0 truncate",
  /** Assignee avatar. */
  avatar: "w-5 h-5 shrink-0",
  /**
   * Trailing slot. Holds the target date AND the hover action bar stacked in the
   * same box so swapping between them cannot shift layout horizontally.
   */
  trailing: "w-24 h-5 shrink-0 relative",
} as const;

/**
 * Fixed vertical rhythm. Rows never grow; content truncates instead.
 *
 * Both densities live here because the hit-area cushions in `IssueRow` and
 * `Checkbox` are sized against the SHORTER of the two — a cushion tuned to
 * `h-9` spills 2px into its neighbours once the list is switched to compact.
 * SPEC.md §7: Comfortable `h-9` / Compact `h-8`.
 */
export const ROW_H = "h-9";
export const COMPACT_ROW_H = "h-8";
export const HEADER_H = "h-11";
export const GROUP_HEADER_H = "h-9";

/** Outer horizontal gutter, identical on header, group headers and rows. */
export const GUTTER = "px-4";

/** Inter-element gap inside a row. */
export const ROW_GAP = "gap-2";

export const SIDE_PANEL_W = "w-60";
