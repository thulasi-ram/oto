import {
  createContext,
  createMemo,
  createSignal,
  useContext,
  type Accessor,
  type JSX,
} from "solid-js";
import {
  PRIORITY_LABEL,
  PRIORITY_ORDER,
  STATUS_LABEL,
  STATUS_ORDER,
  type Assignee,
  type Issue,
  type Priority,
  type Status,
} from "./types";

/**
 * Shared view state for the issue list.
 *
 * Filtering, sorting and grouping all live here so the side panel and the list
 * read from one source of truth instead of each deriving their own.
 */

export type GroupBy = "status" | "priority" | "assignee" | "project" | "none";
export type SortBy = "manual" | "priority" | "updated" | "created" | "target";
export type SortDir = "asc" | "desc";
export type Density = "comfortable" | "compact";

export interface Filters {
  status: Status[];
  priority: Priority[];
  /** Assignee ids. The literal "unassigned" matches issues with no assignee. */
  assignee: string[];
  /** Label ids. */
  label: string[];
}

export type FilterKey = keyof Filters;

export interface IssueGroup {
  /** Stable key for <For>. */
  key: string;
  label: string;
  /**
   * Reactive: backed by a signal, so a group can hand out a new row list without
   * the group object itself changing identity. Read it inside a tracking scope
   * (`<For each={group.issues}>` does) or updates will be missed.
   */
  readonly issues: Issue[];
}

export const GROUP_BY_OPTIONS: readonly { value: GroupBy; label: string }[] = [
  { value: "status", label: "Status" },
  { value: "priority", label: "Priority" },
  { value: "assignee", label: "Assignee" },
  { value: "project", label: "Project" },
  { value: "none", label: "None" },
] as const;

export const SORT_BY_OPTIONS: readonly { value: SortBy; label: string }[] = [
  { value: "manual", label: "Manual" },
  { value: "priority", label: "Priority" },
  { value: "updated", label: "Last updated" },
  { value: "created", label: "Created" },
  { value: "target", label: "Target date" },
] as const;

const EMPTY_FILTERS: Filters = {
  status: [],
  priority: [],
  assignee: [],
  label: [],
};

function toggleIn<T>(list: readonly T[], value: T): T[] {
  return list.includes(value)
    ? list.filter((entry) => entry !== value)
    : [...list, value];
}

/** Ascending compare that always sorts null/empty values last. */
function compareNullable(a: string | null, b: string | null): number {
  if (a === b) return 0;
  if (a === null || a === "") return 1;
  if (b === null || b === "") return -1;
  return a < b ? -1 : 1;
}

/** Mutable shape the grouping pass builds before it is reconciled onto cached groups. */
interface GroupDraft {
  key: string;
  label: string;
  issues: Issue[];
}

/** Row lists are equal when they hold the same issue objects in the same order. */
function sameRows(a: readonly Issue[], b: readonly Issue[]): boolean {
  return a.length === b.length && a.every((issue, index) => issue === b[index]);
}

interface CachedGroup {
  group: IssueGroup;
  label: string;
  setIssues: (rows: Issue[]) => void;
}

function createCachedGroup(draft: GroupDraft): CachedGroup {
  const [rows, setRows] = createSignal<Issue[]>(draft.issues, { equals: sameRows });
  const group: IssueGroup = {
    key: draft.key,
    label: draft.label,
    get issues(): Issue[] {
      return rows();
    },
  };
  return { group, label: draft.label, setIssues: (next) => setRows(() => next) };
}

export function createIssueViewStore(source: Accessor<Issue[]>) {
  const [filters, setFilters] = createSignal<Filters>(EMPTY_FILTERS);
  const [groupBy, setGroupBy] = createSignal<GroupBy>("status");
  const [sortBy, setSortBy] = createSignal<SortBy>("manual");
  const [sortDir, setSortDir] = createSignal<SortDir>("asc");
  const [density, setDensity] = createSignal<Density>("comfortable");
  const [showEmptyGroups, setShowEmptyGroups] = createSignal(false);
  const [selected, setSelected] = createSignal<ReadonlySet<string>>(new Set());
  const [focusedIndex, setFocusedIndex] = createSignal(0);
  const [panelCollapsed, setPanelCollapsed] = createSignal(false);
  const [paletteOpen, setPaletteOpen] = createSignal(false);
  /** Mutations are held locally; the prototype has no backend. */
  const [overrides, setOverrides] = createSignal<Record<string, Partial<Issue>>>({});

  /**
   * Patched issues are memoised on (source object, patch object) identity so a
   * mutation to *one* issue does not hand every previously-patched issue a fresh
   * object. Combined with the group cache below, this keeps `<For>` rebuilding
   * exactly the row that changed — which is what keeps DOM focus alive.
   */
  const patchCache = new Map<string, { source: Issue; patch: Partial<Issue>; result: Issue }>();

  const issues = createMemo<Issue[]>(() => {
    const patches = overrides();
    return source().map((issue) => {
      const patch = patches[issue.id];
      if (!patch) return issue;
      const cached = patchCache.get(issue.id);
      if (cached && cached.source === issue && cached.patch === patch) {
        return cached.result;
      }
      const result: Issue = { ...issue, ...patch };
      patchCache.set(issue.id, { source: issue, patch, result });
      return result;
    });
  });

  /**
   * Every assignee present in the data, deduped by id and sorted by name.
   * Unassigned is not a person and is never included.
   *
   * Derived from `source()` rather than `issues()`: assignments can only ever be
   * made from this roster, so patches never widen it, and reading the raw source
   * keeps the array reference stable across mutations.
   */
  const assigneeRoster = createMemo<Assignee[]>(() => {
    const seen = new Map<string, Assignee>();
    for (const issue of source()) {
      const assignee = issue.assignee;
      if (assignee && !seen.has(assignee.id)) seen.set(assignee.id, assignee);
    }
    return [...seen.values()].sort((a, b) => a.name.localeCompare(b.name));
  });

  const activeFilterCount = createMemo(() => {
    const current = filters();
    return (
      current.status.length +
      current.priority.length +
      current.assignee.length +
      current.label.length
    );
  });

  const filtered = createMemo(() => {
    const current = filters();
    return issues().filter((issue) => {
      if (current.status.length && !current.status.includes(issue.status)) {
        return false;
      }
      if (current.priority.length && !current.priority.includes(issue.priority)) {
        return false;
      }
      if (current.assignee.length) {
        const id = issue.assignee?.id ?? "unassigned";
        if (!current.assignee.includes(id)) return false;
      }
      if (current.label.length) {
        const hit = issue.labels.some((label) => current.label.includes(label.id));
        if (!hit) return false;
      }
      return true;
    });
  });

  const sorted = createMemo(() => {
    const mode = sortBy();
    const rows = [...filtered()];
    if (mode === "manual") return rows;

    const dir = sortDir() === "asc" ? 1 : -1;
    rows.sort((a, b) => {
      // Every comparator is written a-vs-b, i.e. ascending, so `dir` means what
      // the side panel's label says. "Priority + Ascending" therefore reads
      // lowest-priority-first, which is the honest reading of the control.
      let delta = 0;
      switch (mode) {
        case "priority":
          delta =
            PRIORITY_ORDER.indexOf(a.priority) - PRIORITY_ORDER.indexOf(b.priority);
          break;
        case "updated":
          delta = compareNullable(a.updatedAt, b.updatedAt);
          break;
        case "created":
          delta = compareNullable(a.createdAt, b.createdAt);
          break;
        case "target":
          delta = compareNullable(a.targetDate, b.targetDate);
          break;
      }
      // The key tie-break is inside the `* dir` too, or equal rows would ascend
      // by key even in Descending.
      return (delta === 0 ? a.key.localeCompare(b.key) : delta) * dir;
    });
    return rows;
  });

  /**
   * Group identity cache, keyed by group key.
   *
   * `<For each={view.groups()}>` is reference-keyed, so a brand-new IssueGroup
   * object disposes and rebuilds that group's entire subtree — including the row
   * the user is standing on, which drops DOM focus to <body> and kills the
   * keyboard model. Reusing the previous object whenever key *and* label are
   * unchanged means only the group's row list moves, and the inner reference-keyed
   * `<For each={group.issues}>` then rebuilds just the row that actually changed.
   */
  const groupCache = new Map<string, CachedGroup>();

  const reconcileGroups = (drafts: GroupDraft[]): IssueGroup[] => {
    const next: IssueGroup[] = [];
    const live = new Set<string>();
    for (const draft of drafts) {
      live.add(draft.key);
      const cached = groupCache.get(draft.key);
      if (cached && cached.label === draft.label) {
        // Same group: keep the object, swap only its rows. `sameRows` equality
        // means an unchanged list is not even a notification.
        cached.setIssues(draft.issues);
        next.push(cached.group);
        continue;
      }
      const created = createCachedGroup(draft);
      groupCache.set(draft.key, created);
      next.push(created.group);
    }
    for (const key of [...groupCache.keys()]) {
      if (!live.has(key)) groupCache.delete(key);
    }
    return next;
  };

  const groups = createMemo<IssueGroup[]>(() => {
    const mode = groupBy();
    const rows = sorted();

    if (mode === "none") {
      return reconcileGroups([{ key: "all", label: "All issues", issues: rows }]);
    }

    const buckets = new Map<string, GroupDraft>();
    const ensure = (key: string, label: string) => {
      let bucket = buckets.get(key);
      if (!bucket) {
        bucket = { key, label, issues: [] };
        buckets.set(key, bucket);
      }
      return bucket;
    };

    if (mode === "status" && showEmptyGroups()) {
      STATUS_ORDER.forEach((status) => ensure(status, STATUS_LABEL[status]));
    }
    if (mode === "priority" && showEmptyGroups()) {
      [...PRIORITY_ORDER]
        .reverse()
        .forEach((priority) => ensure(priority, PRIORITY_LABEL[priority]));
    }

    for (const issue of rows) {
      switch (mode) {
        case "status":
          ensure(issue.status, STATUS_LABEL[issue.status]).issues.push(issue);
          break;
        case "priority":
          ensure(issue.priority, PRIORITY_LABEL[issue.priority]).issues.push(issue);
          break;
        case "assignee":
          ensure(
            issue.assignee?.id ?? "unassigned",
            issue.assignee?.name ?? "No assignee",
          ).issues.push(issue);
          break;
        case "project":
          ensure(issue.project ?? "no-project", issue.project ?? "No project").issues.push(
            issue,
          );
          break;
      }
    }

    const ordered = [...buckets.values()];
    if (mode === "status") {
      ordered.sort(
        (a, b) =>
          STATUS_ORDER.indexOf(a.key as Status) - STATUS_ORDER.indexOf(b.key as Status),
      );
    } else if (mode === "priority") {
      ordered.sort(
        (a, b) =>
          PRIORITY_ORDER.indexOf(b.key as Priority) -
          PRIORITY_ORDER.indexOf(a.key as Priority),
      );
    } else {
      // Alphabetical, with the "none" bucket pinned last.
      ordered.sort((a, b) => {
        const aNone = a.key === "unassigned" || a.key === "no-project";
        const bNone = b.key === "unassigned" || b.key === "no-project";
        if (aNone !== bNone) return aNone ? 1 : -1;
        return a.label.localeCompare(b.label);
      });
    }
    return reconcileGroups(ordered);
  });

  /** Flattened row order, so ↑/↓ can cross group boundaries seamlessly. */
  const flatIssues = createMemo(() => groups().flatMap((group) => group.issues));

  return {
    // state
    issues,
    filters,
    groupBy,
    sortBy,
    sortDir,
    density,
    showEmptyGroups,
    selected,
    focusedIndex,
    panelCollapsed,
    paletteOpen,

    // derived
    groups,
    flatIssues,
    assigneeRoster,
    activeFilterCount,
    totalCount: () => issues().length,
    visibleCount: () => flatIssues().length,

    // filter actions
    toggleFilter(key: FilterKey, value: string) {
      setFilters((prev) => ({
        ...prev,
        [key]: toggleIn(prev[key] as string[], value),
      }));
    },
    clearFilter(key: FilterKey) {
      setFilters((prev) => ({ ...prev, [key]: [] }));
    },
    clearAllFilters() {
      setFilters(EMPTY_FILTERS);
    },

    // view actions
    setGroupBy,
    setSortBy,
    setSortDir,
    toggleSortDir: () => setSortDir((dir) => (dir === "asc" ? "desc" : "asc")),
    setDensity,
    setShowEmptyGroups,
    setPanelCollapsed,
    togglePanel: () => setPanelCollapsed((value) => !value),
    setPaletteOpen,

    // selection
    setFocusedIndex,
    toggleSelected(id: string) {
      setSelected((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        return next;
      });
    },
    selectAll() {
      setSelected(new Set(flatIssues().map((issue) => issue.id)));
    },
    clearSelection() {
      setSelected(new Set<string>());
    },

    // mutation (local only)
    patchIssue(id: string, patch: Partial<Issue>) {
      setOverrides((prev) => ({ ...prev, [id]: { ...prev[id], ...patch } }));
    },
  };
}

export type IssueViewStore = ReturnType<typeof createIssueViewStore>;

const IssueViewContext = createContext<IssueViewStore>();

export function IssueViewProvider(props: {
  store: IssueViewStore;
  children: JSX.Element;
}): JSX.Element {
  return (
    <IssueViewContext.Provider value={props.store}>
      {props.children}
    </IssueViewContext.Provider>
  );
}

export function useIssueView(): IssueViewStore {
  const store = useContext(IssueViewContext);
  if (!store) {
    throw new Error("useIssueView must be called inside <IssueViewProvider>");
  }
  return store;
}
