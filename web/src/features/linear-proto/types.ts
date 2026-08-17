/**
 * Canonical data contract for the linear-proto issue view.
 *
 * This file is the shared contract every prototype component compiles against.
 * Treat it as authoritative: adapt components to it, not it to components.
 */

export type Priority = "urgent" | "high" | "medium" | "low" | "none";

export type Status = "backlog" | "todo" | "in_progress" | "done" | "canceled";

export interface Assignee {
  id: string;
  name: string;
  /** 1-2 character initials, precomputed so avatars never derive at render time. */
  initials: string;
  /** Index into AVATAR_COLORS. */
  colorIndex: number;
}

export interface Label {
  id: string;
  name: string;
  /** Hex colour for the label's leading dot. Data-driven; exempt from the 3-hue rule. */
  color: string;
}

export interface Issue {
  id: string;
  /** Human key, e.g. "OTO-142". Rendered monospace + tabular-nums. */
  key: string;
  title: string;
  priority: Priority;
  status: Status;
  assignee: Assignee | null;
  labels: Label[];
  project: string | null;
  /** ISO-8601 date (yyyy-mm-dd) or null when no target date is set. */
  targetDate: string | null;
  /** ISO-8601 timestamps. */
  createdAt: string;
  updatedAt: string;
}

/** Ascending severity. Index doubles as the sort weight. */
export const PRIORITY_ORDER: readonly Priority[] = [
  "none",
  "low",
  "medium",
  "high",
  "urgent",
] as const;

/** Workflow order. Index doubles as the sort weight and the group order. */
export const STATUS_ORDER: readonly Status[] = [
  "backlog",
  "todo",
  "in_progress",
  "done",
  "canceled",
] as const;

export const PRIORITY_LABEL: Record<Priority, string> = {
  urgent: "Urgent",
  high: "High",
  medium: "Medium",
  low: "Low",
  none: "No priority",
};

export const STATUS_LABEL: Record<Status, string> = {
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In Progress",
  done: "Done",
  canceled: "Canceled",
};

/**
 * Avatar background hues. Deliberately desaturated to sit on the #0f0f11 canvas
 * without competing with the semantic alphabet (red / amber / blue). None of
 * these is the accent #5e6ad2, the urgent #f26522 or the high/in-progress
 * #f5a623 — an identity colour must never read as a priority signal.
 */
export const AVATAR_COLORS: readonly string[] = [
  "#4a6fa5",
  "#4a8fb8",
  "#6b8f5e",
  "#a8734a",
  "#8f5e8f",
  "#5e8f8f",
] as const;

/**
 * Label dot hues. Desaturated, and clear of the two reserved semantic hues:
 * `bug` is a muted orange and `perf` an ochre rather than #f26522 / #f5a623, so
 * a label dot in the labels column cannot be misread as an Urgent or High
 * marker two columns to its left.
 */
export const LABEL_COLORS: Record<string, string> = {
  bug: "#c2603a",
  feature: "#5e6ad2",
  improvement: "#4a8fb8",
  docs: "#6b8f5e",
  infra: "#8a8f98",
  design: "#8f5e8f",
  perf: "#b8913a",
};
