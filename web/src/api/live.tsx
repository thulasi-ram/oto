/**
 * Wiring the SSE stream into the query cache.
 *
 * The policy is deliberately conservative: a frame is a **change notice, not a
 * resource** (§E.4), so oto invalidates and lets solid-query refetch rather
 * than patching cached rows from a partial payload. Patching would be faster
 * and would drift, and a list that drifts is a list that lies.
 *
 * The one place the payload is used directly is the timeline, where
 * `event.appended` carries everything a row needs — and even there the query is
 * still invalidated, so the optimistic row is replaced by the authoritative one.
 */
import { useQueryClient } from "@tanstack/solid-query";
import {
  createContext,
  createSignal,
  onCleanup,
  onMount,
  useContext,
  type Accessor,
  type JSX,
} from "solid-js";

import { AlertStream, type ConnectionState, type StreamDetail, type StreamInterest } from "./stream";
import { qk } from "./keys";
import type { StreamFrame } from "./types";

interface LiveContextValue {
  readonly state: Accessor<ConnectionState>;
  readonly detail: Accessor<StreamDetail>;
  readonly reconnect: () => void;
  readonly acknowledgeResync: () => void;
  readonly onFrame: (handler: (frame: StreamFrame) => void) => () => void;
}

const LiveContext = createContext<LiveContextValue>();

export interface LiveProviderProps {
  readonly interest?: StreamInterest;
  readonly children: JSX.Element;
}

export function LiveProvider(props: LiveProviderProps): JSX.Element {
  const queryClient = useQueryClient();
  const [state, setState] = createSignal<ConnectionState>("idle");
  const [detail, setDetail] = createSignal<StreamDetail>({
    lastSeq: null,
    retryAt: null,
    attempt: 0,
    resyncReason: null,
    lastMessageAt: null,
  });

  const stream = new AlertStream(props.interest ?? {});

  const invalidate = (frame: StreamFrame): void => {
    switch (frame.kind) {
      case "alert.upserted":
        void queryClient.invalidateQueries({ queryKey: qk.alerts.all() });
        // An alert that moved is an alert oto may have just formed an intent
        // about — including a SUPPRESSED one, which leaves no delivery behind
        // and would therefore be announced by nothing else. The activity log
        // absorbs the rate itself (`notificationActivityQuery`); reaching it is
        // this file's half of the bargain.
        void queryClient.invalidateQueries({ queryKey: qk.notifications.all() });
        break;
      case "case.upserted":
        // The frame the case list exists for: an episode opening, being
        // acknowledged, or ending is exactly what moves a row in and out of the
        // queue on `/cases`.
        void queryClient.invalidateQueries({ queryKey: qk.cases.all() });
        void queryClient.invalidateQueries({ queryKey: qk.alerts.all() });
        void queryClient.invalidateQueries({ queryKey: qk.groups.all() });
        break;
      case "group.upserted":
        void queryClient.invalidateQueries({ queryKey: qk.groups.all() });
        break;
      case "event.appended":
        // The timeline is the differentiator; it must never lag its own stream.
        void queryClient.invalidateQueries({ queryKey: qk.cases.all() });
        void queryClient.invalidateQueries({ queryKey: qk.alerts.all() });
        void queryClient.invalidateQueries({ queryKey: qk.groups.all() });
        void queryClient.invalidateQueries({ queryKey: qk.notifications.all() });
        break;
      case "delivery.updated":
        // `["alerts"]` because that is where a delivery is read per alert: the
        // notification list on the alert detail screen is
        // `qk.alerts.notifications(id)`, and `DeliveryPanel` renders what that
        // query already holds. There was a `["deliveries"]` invalidation here
        // and no query anywhere under that prefix — a line that named a
        // resource oto does not cache and updated nothing.
        //
        // `["notifications"]` because a delivery moving is exactly what changes
        // an intent's status from `pending` to `delivered`, `partial` or
        // `failed`, and that status is the activity log's headline.
        void queryClient.invalidateQueries({ queryKey: qk.alerts.all() });
        void queryClient.invalidateQueries({ queryKey: qk.notifications.all() });
        break;
      case "source.health":
        void queryClient.invalidateQueries({ queryKey: qk.settings.sources() });
        break;
      case "resync":
        // "Your incremental state is not trustworthy." Refetch everything on
        // screen; do not attempt to reconcile.
        void queryClient.invalidateQueries();
        break;
      default:
        // An unknown kind is forward-compatible, not an error. Do nothing
        // rather than guess — the polling refetch will pick the change up.
        break;
    }
  };

  onMount(() => {
    const offState = stream.onState((next, d) => {
      setState(next);
      setDetail(d);
    });
    const offFrame = stream.onFrame(invalidate);
    stream.start();

    onCleanup(() => {
      offState();
      offFrame();
      stream.close();
    });
  });

  const value: LiveContextValue = {
    state,
    detail,
    reconnect: () => stream.reconnectNow(),
    acknowledgeResync: () => stream.clearResync(),
    onFrame: (handler) => stream.onFrame(handler),
  };

  return <LiveContext.Provider value={value}>{props.children}</LiveContext.Provider>;
}

export function useLive(): LiveContextValue {
  const ctx = useContext(LiveContext);
  if (!ctx) throw new Error("oto: useLive() must be called inside <LiveProvider>");
  return ctx;
}

/**
 * Human-readable connection state.
 *
 * The copy never overstates: "live" is only ever said when frames are actually
 * arriving, and every other state names what is wrong and what happens next.
 */
export function describeConnection(state: ConnectionState, detail: StreamDetail): string {
  switch (state) {
    case "live":
      return "Live — updates arrive as they happen";
    case "connecting":
      return "Connecting to the live stream…";
    case "reconnecting": {
      if (detail.retryAt === null) return "Reconnecting…";
      const secs = Math.max(0, Math.ceil((detail.retryAt - Date.now()) / 1000));
      return `Disconnected — retrying in ${secs}s. What you see may be out of date.`;
    }
    case "offline":
      return "Offline — this page is a snapshot, not a live view.";
    case "idle":
      return "Not connected.";
    default:
      return "Unknown connection state.";
  }
}
