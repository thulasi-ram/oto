/**
 * What a cache entry is: its key, what fetches it, and what keeps it fresh.
 *
 * `api/keys.ts` names entries and `api/endpoints.ts` fetches them, and until
 * this file existed nothing joined the two — every call site re-paired a key
 * with a function by hand, and the third copy of a pair is where they diverge.
 * `["settings","clusters"]` had reached that third copy: the alert filter bar
 * declared it with `staleTime: 5 * 60_000` and the settings screen declared the
 * same key with the same function and no `staleTime` at all, so one cache entry
 * had two freshness policies and which one applied depended on which screen you
 * were looking at. `staleTime` is per-observer over a shared entry, so nothing
 * about that reads as a bug from either call site.
 *
 * So: **one factory per resource that more than one screen reads, and the
 * factory owns the policy.** A screen may add what is local to the screen
 * (`enabled`, `placeholderData`, a `refetchInterval` that is that screen's
 * safety net); it may not restate what the resource is.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 *
 * The second half of this file is `FRESHNESS`, and it is the part that makes an
 * unreachable key impossible rather than merely fixed once. Every path in `qk`
 * must appear there naming ONE reason its data cannot silently rot:
 *
 *   `live`      a stream frame invalidates a prefix of it (`api/live.tsx`).
 *   `mutation`  only a local write changes it, and that write invalidates or
 *               rewrites the entry in the same handler.
 *   `poll`      the reading screen declares a `refetchInterval`; nothing else
 *               will ever tell it.
 *   `bounded`   nothing announces a change, and that is tolerable: it is
 *               refetched next time a screen asks and its `staleTime` has run
 *               out. The number is the promise, so it is stated here.
 *   `immutable` the answer cannot change once given, so nothing has to.
 *
 * `queries.test.tsx` checks each claim against what the app actually does —
 * `live` and `mutation` against the real invalidation call sites, the whole
 * table against `qk` itself. A key added to `qk` with no answer here fails
 * there, which is the only version of this rule that survives the next screen.
 */
import { qk } from "./keys";
import {
  listAlerts,
  listChannelConnections,
  listChannelTypes,
  listChannels,
  listClusters,
  listLabelNames,
  listNotifications,
  listPolicies,
  listRuleSnapshots,
  listSources,
  listNotificationTemplates,
  previewNotificationTemplate,
} from "./endpoints";
import { keepPrevious } from "~/lib/keysetFeed";
import type {
  AlertListQuery,
  NotificationListQuery,
  RuleSnapshotQuery,
  NotificationTemplateFormat,
} from "./types";

/**
 * The floor under everything the live stream owns, and the client's default.
 *
 * `App.tsx` reads it from here rather than restating it, because a default is a
 * freshness policy like any other and this is where those live.
 */
export const DEFAULT_STALE_MS = 15_000;

/**
 * How long a picker's reference list may be trusted.
 *
 * Five minutes, and it is a claim about the data rather than a tuning knob:
 * clusters and label names change when a human creates one, no stream frame
 * announces either, and the mutation that creates one invalidates the entry it
 * changed. So the only staleness this bounds is another operator's write in
 * another session, which no operational decision turns on. The client default
 * of 15 s (`App.tsx`) is for the entities the stream owns — an alert row is
 * wrong the moment it is out of date, and a cluster list is not.
 */
export const REFERENCE_STALE_MS = 5 * 60_000;

/**
 * The channel *types* this build of the server supports — its capability list,
 * not the org's data. It changes on deploy, so half an hour is generous.
 */
export const CAPABILITY_STALE_MS = 30 * 60_000;

/**
 * The recent alerts a policy dry run is offered to run against.
 *
 * ⭐ `include: ["current_case"]` IS LOAD-BEARING, NOT A CONVENIENCE. The preview
 * endpoint takes a `case_id` and nothing else (git-bug 7570090): a Case IS the
 * conversation, so "who would this reach?" is a question about one firing
 * episode. The picker still shows ALERTS, because a case has no name of its own
 * and `KubePodCrashLooping · prod-eu` is what an operator recognises — but what
 * it sends is `current_case.id`, and without this `include` that id is absent
 * and every row is unpickable.
 */
export const RECENT_ALERTS: AlertListQuery = {
  limit: 20,
  sort: "-last_seen_at",
  include: ["current_case"],
};

/**
 * How long the dry-run picker stays quiet, however loud the stream is.
 *
 * ⛔ INVALIDATION AND REFETCHING ARE NOT THE SAME EVENT, and this constant is
 * the difference. `invalidateQueries` marks every matching entry stale AND
 * refetches the active ones, and it does that regardless of `staleTime` — a
 * plain number cannot absorb a storm, it only governs mounts and focus. So the
 * picker states its staleness as a FUNCTION: for a minute after each answer it
 * resolves to `"static"`, which is the one value solid-query's refetch pass
 * honours (`refetchQueries` skips a static query), and after that it is an
 * ordinary short-lived entry again.
 *
 * The result is the policy the screen actually wants: every alert frame still
 * REACHES the picker and marks it stale — that was the whole point of moving it
 * under `["alerts"]` — but a thousand-frame storm costs at most one request a
 * minute, and the first frame after the minute is up pays for it. Nothing on
 * this screen turns on which twenty alerts are the twenty most recent to the
 * second; picking one to dry-run a routing rule against does not.
 */
export const RECENT_ALERTS_QUIET_MS = 60_000;

/* -------------------------------------------------------------------------- */
/* One factory per shared resource                                            */
/* -------------------------------------------------------------------------- */

/** Every cluster in the org — the filter bar's picker and the settings list. */
export function clustersQuery() {
  return {
    queryKey: qk.settings.clusters(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listClusters({ signal }),
    staleTime: REFERENCE_STALE_MS,
  };
}

/**
 * Every source, with its health embedded — read by the settings list, the
 * tuning screen and the shell's reach banner.
 *
 * No `staleTime`: a `source.health` frame invalidates `["settings","sources"]`,
 * so this entry is one the stream owns and the client default is the right
 * floor under it.
 */
export function sourcesQuery() {
  return {
    queryKey: qk.settings.sources(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listSources({ signal }),
  };
}

/** Every channel — the channels screen edits them, the policy editor picks one. */
export function channelsQuery() {
  return {
    queryKey: qk.settings.channels(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listChannels({ signal }),
  };
}

/**
 * Every connection — the org-wide setup a Settings admin owns. The channel
 * dialog on the notification policy screen picks one of these by id.
 */
export function channelConnectionsQuery() {
  return {
    queryKey: qk.settings.channelConnections(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listChannelConnections({ signal }),
  };
}

/** The server's channel-type descriptors, which drive the dynamic form. */
export function channelTypesQuery() {
  return {
    queryKey: qk.settings.channelTypes(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listChannelTypes({ signal }),
    staleTime: CAPABILITY_STALE_MS,
  };
}

/** Every routing policy. */
export function policiesQuery() {
  return {
    queryKey: qk.settings.policies(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listPolicies({ signal }),
  };
}

/** Every NotificationTemplate in the org, newest first. */
export function notificationTemplatesQuery() {
  return {
    queryKey: qk.templates.list(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listNotificationTemplates({ signal }),
  };
}

/**
 * What one candidate template would say, on every fixture, in every Dialect.
 *
 * ⭐ IT IS A QUERY AND NOT A MUTATION, AND THE CACHE IS THE AUTHORING LOOP. The
 * endpoint writes nothing and its answer is a pure function of the two strings in
 * the key, so an author who types a word and deletes it again gets the previous
 * rendering back instantly rather than paying for a second POST of a question
 * already answered. `enabled` is the caller's, because only the caller knows
 * whether there is a template to ask about — the contract's `minLength: 1` makes
 * an empty one a `422` rather than an empty answer.
 *
 * ⛔ A NON-EMPTY `problems` IS A SUCCESS, NOT AN ERROR. The server answers `200`
 * for an invalid template on purpose: the refusal and the output belong on screen
 * together, because the fix is usually only visible when both are.
 */
export function templatePreviewQuery(format: NotificationTemplateFormat, source: string) {
  return {
    queryKey: qk.templates.preview(format, source),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      previewNotificationTemplate({ format, source }, { signal }),
  };
}

/** The label names offered as matcher completions. */
export function labelNamesQuery() {
  return {
    queryKey: qk.labels.names(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listLabelNames({ signal }),
    staleTime: REFERENCE_STALE_MS,
  };
}

/**
 * The short list of recent alerts a policy dry run is run against.
 *
 * ⛔ IT IS UNDER `["alerts"]`, AND THAT IS THE POINT OF THIS FUNCTION. It used
 * to be the literal `["policy-preview","recent-alerts"]`, which is a filtered
 * alert list sitting outside the prefix — so `alert.upserted`, the one frame the
 * screen exists to react to, went straight past it and the operator dry-ran a
 * routing policy against a list that nothing could ever refresh.
 *
 * What it is NOT is a live list. `RECENT_ALERTS_QUIET_MS` says why: the frame
 * reaches it and marks it stale, and the refetch that follows is rate-limited to
 * once a minute rather than once a frame.
 */
export function recentAlertsQuery() {
  return {
    queryKey: qk.alerts.recent(),
    queryFn: ({ signal }: { signal: AbortSignal }) => listAlerts(RECENT_ALERTS, {}, { signal }),
    staleTime: recentAlertsStaleTime,
  };
}

/**
 * `"static"` for a minute after each answer, an ordinary short life after that.
 *
 * `dataUpdatedAt` is 0 before the first answer, so an entry with no data is
 * never static and the first mount always fetches.
 */
function recentAlertsStaleTime(query: {
  readonly state: { readonly dataUpdatedAt: number };
}): number | "static" {
  const age = Date.now() - query.state.dataUpdatedAt;
  return age < RECENT_ALERTS_QUIET_MS ? "static" : DEFAULT_STALE_MS;
}

/**
 * One page of the notification activity log.
 *
 * ⛔ IT IS A HISTORY FEED, AND IT IS PAGED, AND THOSE TWO FACTS SET ITS POLICY
 * TOGETHER. Every frame that changes an alert can also mint a notification
 * intent — a suppressed one leaves no delivery behind, so `delivery.updated`
 * alone would never announce it — which is why `api/live.tsx` invalidates
 * `["notifications"]` from the alert frames as well. That is the right *reach*
 * and the wrong *rate*: a storm would then refetch page one under the reader
 * once per frame, and the rows it is re-fetching are rows that have already
 * happened.
 *
 * So the same shape `recentAlertsQuery` uses: the frame still marks the entry
 * stale, and `"static"` is what solid-query's refetch pass honours, so a storm
 * costs one request per quiet window rather than one per frame. Nothing an
 * operator does on this screen turns on the log being right to the second — it
 * is read to answer "was anybody told, and why not", about intents that are
 * already in the past.
 */
export function notificationActivityQuery(query: NotificationListQuery) {
  return {
    queryKey: qk.notifications.list(query),
    queryFn: ({ signal }: { signal: AbortSignal }) => listNotifications(query, { signal }),
    staleTime: activityStaleTime,
    placeholderData: keepPrevious,
  };
}

/** How long the activity feed stays quiet, however loud the stream is. */
export const ACTIVITY_QUIET_MS = 20_000;

/** `"static"` for a quiet window after each answer; ordinary staleness after. */
function activityStaleTime(query: {
  readonly state: { readonly dataUpdatedAt: number };
}): number | "static" {
  const age = Date.now() - query.state.dataUpdatedAt;
  return age < ACTIVITY_QUIET_MS ? "static" : DEFAULT_STALE_MS;
}

/**
 * One page of a rule's captured versions, for the drift panel's history.
 *
 * The `staleTime` is STATED here rather than inherited from the client default,
 * because `FRESHNESS` below promises a number for this key and a promise whose
 * value lives in `App.tsx` is a promise no test of this key can check. The
 * number is the same one — a version list is not an alert row — and the drift
 * panel adds only what is local to it (`enabled`: the history is fetched when
 * the operator asks for it, not when the panel renders).
 */
export function ruleSnapshotsQuery(query: RuleSnapshotQuery) {
  return {
    queryKey: qk.rules.snapshots(query),
    queryFn: ({ signal }: { signal: AbortSignal }) => listRuleSnapshots(query, { signal }),
    staleTime: DEFAULT_STALE_MS,
  };
}

/* -------------------------------------------------------------------------- */
/* Freshness, declared for every key `qk` can produce                         */
/* -------------------------------------------------------------------------- */

export type Freshness =
  /** A stream frame invalidates a prefix of this key. */
  | { readonly by: "live" }
  /** Only a local write changes it, and that write invalidates or rewrites it. */
  | { readonly by: "mutation" }
  /** The reading screen polls; nothing else will ever tell it. `why` is that screen. */
  | { readonly by: "poll"; readonly why: string }
  /** Nobody announces a change, and the declared `staleTime` is how stale it may get. */
  | { readonly by: "bounded"; readonly ms: number; readonly why: string }
  /** The answer cannot change once given. `why` says what makes that true. */
  | { readonly by: "immutable"; readonly why: string };

/**
 * Every path in `qk`, and the one thing that keeps it honest.
 *
 * Keyed by `"group.name"` — the path, not the key array, so that an entry here
 * cannot drift into describing a key that no longer exists.
 */
export const FRESHNESS: Readonly<Record<string, Freshness>> = {
  "alerts.all": { by: "live" },
  "alerts.list": { by: "live" },
  // Reached by every `["alerts"]` invalidation like any other alert list; what
  // is particular to it is when it acts on one (`RECENT_ALERTS_QUIET_MS`).
  "alerts.recent": { by: "live" },
  "alerts.rollups": { by: "live" },
  "alerts.detail": { by: "live" },
  "alerts.events": { by: "live" },
  "alerts.cases": { by: "live" },
  "alerts.enrichments": { by: "live" },
  "alerts.rule": { by: "live" },
  "alerts.snoozes": { by: "live" },
  // The dead deliveries behind one notification, for the retry affordance. `live`
  // because `delivery.updated` is precisely the frame that ends a retry: the row
  // the button was offered on must stop being offered once it is no longer dead,
  // and that frame already invalidates the `["alerts"]` prefix this key sits under.
  "alerts.deadDeliveries": { by: "live" },
  "alerts.activeSnoozes": { by: "live" },
  "alerts.notifications": { by: "live" },

  // The activity log. Reached by the three frames that can mint an intent —
  // `alert.upserted`, `event.appended`, `delivery.updated` — and rate-limited
  // at the entry rather than at the invalidation (`ACTIVITY_QUIET_MS`).
  "notifications.all": { by: "live" },
  "notifications.list": { by: "live" },

  // The firing episodes. `case.upserted` is the frame that opens, acknowledges
  // and closes one; `event.appended` is what puts a row on an episode's own
  // timeline. Both invalidate `["cases"]` in `api/live.tsx`.
  "cases.all": { by: "live" },
  "cases.list": { by: "live" },
  "cases.detail": { by: "live" },
  "cases.timeline": { by: "live" },

  // Health arrives as `source.health`; the rejection feed and the failed-batch
  // list hang under the same source prefix and ride the same frame.
  "settings.sources": { by: "live" },
  "settings.rejections": { by: "live" },
  "settings.failedBatches": { by: "live" },

  "settings.clusters": { by: "mutation" },
  // Minting and revoking are the only things that change this list, both happen
  // on the screen that reads it, and both invalidate it. `last_used_at` does
  // move without a local write — but nothing announces that, and a screen that
  // polled for it would be asking the server every few seconds to watch a column
  // whose whole purpose is "is this old token still in use somewhere", a
  // question answered by looking rather than by watching.
  "settings.apiTokens": { by: "mutation" },
  "settings.channels": { by: "mutation" },
  "settings.channelConnections": { by: "mutation" },
  "settings.policies": { by: "mutation" },
  // Settings, like the policies beside them: creating, editing and deleting a
  // A template is written, edited and deleted on the screen that reads the list,
  // and all three invalidate it. No stream frame can change one — a template is
  // what an operator wrote, not something an alert did.
  "templates.list": { by: "mutation" },
  // `TuningSection` writes the saved settings back with `setQueryData`, which is
  // the same guarantee by a shorter route: the entry holds the server's answer.
  "settings.org": { by: "mutation" },
  // Started and disposed through mutations that write the drill back with
  // `setQueryData`; `DrillPanel`'s poll only covers the queue hops in between,
  // and stops the moment the verdict is frozen.
  "settings.drill": { by: "mutation" },

  "settings.channelTypes": {
    by: "bounded",
    ms: CAPABILITY_STALE_MS,
    why: "the server's capability list for this build, which changes on deploy and not on any action an operator can take here",
  },
  "labels.names": {
    by: "bounded",
    ms: REFERENCE_STALE_MS,
    why: "the label vocabulary of the estate — it grows as new alerts arrive, and a completion list that is five minutes behind costs a keystroke, not a mistake",
  },
  "rules.snapshots": {
    by: "bounded",
    ms: DEFAULT_STALE_MS,
    why: "each snapshot is immutable, so only the *set* of versions grows; the drift panel is opened deliberately and re-asks when it is",
  },
  "rules.batch": {
    by: "immutable",
    why: "a snapshot is content-addressed, so once an id has been resolved its answer can never change; asking again would be asking a settled question",
  },
  "templates.preview": {
    by: "immutable",
    why: "a preview writes nothing and reads nothing of the org: it is one template rendered against a fixture corpus that ships with the build, so the format and the source in the key are the whole question and the answer to them cannot change while the tab is open",
  },

  "stats.overview": {
    by: "poll",
    why: "ShellBanner's sixty-second safety net — twenty-six columns over five tables is too much to refetch per reconciler heartbeat",
  },
};
