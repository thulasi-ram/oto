/**
 * Empty, loading and error states.
 *
 * These are the states an operational tool spends most of its life in, and the
 * standard oto holds them to is honesty:
 *
 *   - An empty list says **why** it is empty, and distinguishes "nothing
 *     matched your filters" from "there is nothing here at all". Those are
 *     different facts and conflating them is how a filter typo becomes a quiet
 *     lie about the world.
 *   - A loading state occupies **the same box** the content will occupy, so
 *     nothing jumps when the data lands.
 *   - An error shows the request id, because that is the string that makes a
 *     support conversation short.
 */
import { For, Show, splitProps, type Component, type JSX, type ParentComponent } from "solid-js";

import { ApiError } from "~/api/client";
import { Chime } from "./Chime";
import { Button } from "./Button";
import { Ink, clearColumn } from "./Ink";
import { cn } from "~/lib/cn";

/* -------------------------------------------------------------------------- */
/* Empty                                                                      */
/* -------------------------------------------------------------------------- */

export interface EmptyStateProps {
  readonly title: string;
  readonly body?: string;
  readonly action?: JSX.Element;
  readonly class?: string;
}

export const EmptyState: Component<EmptyStateProps> = (props) => (
  <div
    class={cn("flex flex-col items-center justify-center gap-2 px-6 py-14 text-center", props.class)}
  >
    {/* A quiet chime mark — the product's own glyph rather than a generic
        "no data" illustration.

        It used to say "and the only decorative art in the app", which stopped
        being true with §M.9: a full-page empty state may also carry an ambient
        motif. What is still true, and is the reason this component did not grow
        a prop for it, is that THIS glyph is the only art the *shared* empty
        state has — see `PageEmptyState` for the six sub-panels that would
        otherwise render six washes on one quiet alert. */}
    <Chime size="glyph" class="text-line-strong" />
    <p class="text-title font-medium text-ink">{props.title}</p>
    <Show when={props.body}>
      {/* `max-w-96` (24rem) is the measure this sentence has always wanted; the
          named `max-w-sm` resolves against the spacing namespace and compiles
          to 8px — see the ⛔ block in `Modal.tsx`. */}
      <p class="max-w-96 text-body leading-relaxed text-ink-muted">{props.body}</p>
    </Show>
    <Show when={props.action}>
      <div class="mt-1">{props.action}</div>
    </Show>
  </div>
);

/**
 * The two ambient motifs a FULL-PAGE empty state may carry — SPEC §M.9,
 * ADR 0035. Chosen because their traditional meaning already matches the state,
 * not because they are decorative.
 *
 *   kumo   — cloud, stillness. Nothing is wrong, and that is the point.
 *   sakura — *mono no aware*, transience. `expired`, and nowhere else.
 *
 * ⛔ ONE MOTIF PER STATE, NEVER BOTH. The moment a panel carries clouds *and*
 * petals the distinction they exist to draw is gone — which is why this is a
 * single required prop rather than a set of optional ones.
 *
 * ⛔ SAKURA IS GATED, AND THE GATE IS THE POINT. A petal that appears on every
 * empty panel stops meaning transience within a day. `expired` is the one state
 * whose meaning *is* transience — §M.1 is explicit that it reads "oto stopped
 * hearing about this", never "resolved" — and the whole reason this component
 * exists is that on screen it was indistinguishable from a filter that matched
 * nothing.
 */
export type EmptyMotif = "kumo" | "sakura";

/**
 * Where each motif sits, and how big. Both preserve their asset's aspect (200×80
 * and 72×72), because a stretched cloud reads as smoke and a stretched petal
 * reads as a leaf — the assets carry `preserveAspectRatio="none"` so that a box
 * of the wrong shape distorts visibly rather than letterboxing the mask into
 * nothing, and these two numbers are what keeps it from having to.
 */
const MOTIF: Readonly<Record<EmptyMotif, { readonly size: string; readonly position: string }>> = {
  // Mist drifts in from the edge and trails off, the way suyari-gumo does.
  kumo: { size: "20rem 8rem, 100% 100%", position: "left -3rem bottom 2rem, center" },
  // One petal, already fallen, on the other side. Square: the art is rotated
  // inside its own box, so a non-square box would clip the corners it turns into.
  sakura: { size: "5rem 5rem, 100% 100%", position: "right 3rem bottom 2rem, center" },
};

/**
 * A full-page empty state: the sentence, plus one corner-anchored motif.
 *
 * ⛔ THIS IS A SEPARATE COMPONENT AND NOT A PROP ON `EmptyState`, FOR ONE
 * CONCRETE REASON. `EmptyState` has eighteen call sites and six of them are
 * sub-panels on a single alert-detail page — delivery, enrichment, cases,
 * timeline, rule drift, snoozes. A wash on the shared component renders six of
 * them on one quiet alert, and at six a gesture becomes a texture. The shared
 * component is therefore untouched, and this one is reachable only from the
 * handful of screens that fill a page on their own.
 *
 * ⭐ THE MOTIF CANNOT REACH THE TEXT, AND NOT BECAUSE AN OPACITY WAS CHOSEN
 * CAREFULLY. `clearColumn` intersects a transparent band across the middle
 * 28rem with the art, and the sentence is capped at `max-w-96` (24rem) inside
 * it — so the ink is geometrically excluded at every viewport width, and on a
 * narrow one it simply disappears rather than sliding under the words.
 *
 * ⛔ AND IT IS NEVER THE ONLY CHANNEL (U1). The copy already says the whole
 * fact; the ink is a second reading of it. Every state below must still be
 * correct with the ink removed. Nothing here moves, either — U9's decorative
 * one-shot budget is spent by the fūrin's greeting (ADR 0028), so the obvious
 * next thought (drifting clouds, falling petals) is forbidden rather than
 * merely unimplemented.
 */
export const PageEmptyState: Component<EmptyStateProps & { readonly motif: EmptyMotif }> = (
  props,
) => {
  // `splitProps` rather than reading `props.title`/`props.body` across: it keeps
  // the getters, so the sentence stays reactive, and it forwards an ABSENT
  // optional as absent — which `exactOptionalPropertyTypes` requires and an
  // explicit `body={props.body}` does not do.
  const [mine, rest] = splitProps(props, ["motif", "class"]);

  return (
    <div class="relative flex min-h-0 flex-1 flex-col justify-center overflow-hidden">
      <Ink
        motif={mine.motif}
        size={MOTIF[mine.motif].size}
        position={MOTIF[mine.motif].position}
        carve={clearColumn("28rem")}
        class="absolute inset-0"
      />
      <EmptyState {...rest} class={cn("relative", mine.class)} />
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Loading                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * A skeleton block. It animates only when the platform allows motion; under
 * `prefers-reduced-motion` it is a flat tint, which is still a perfectly good
 * "something is coming" signal.
 */
export const Skeleton: Component<{ readonly class?: string }> = (props) => (
  <span
    aria-hidden="true"
    class={cn("block rounded-chip bg-sunken motion-safe:animate-pulse", props.class)}
  />
);

/**
 * Skeleton rows at exactly `--oto-row-h`, so the table does not resize when the
 * real rows arrive. No layout shift is a correctness property here, not polish:
 * a row that moves under a cursor at 3am gets the wrong alert acknowledged.
 */
export const TableSkeleton: Component<{ readonly rows?: number; readonly cols?: number }> = (
  props,
) => (
  <div role="presentation">
    <For each={Array.from({ length: props.rows ?? 12 })}>
      {() => (
        <div
          class="flex items-center gap-md border-b border-line px-3"
          style={{ height: "var(--oto-row-h)" }}
        >
          <For each={Array.from({ length: props.cols ?? 5 })}>
            {(_, i) => (
              <Skeleton class={cn("h-2.5", i() === 0 ? "w-40" : i() === 1 ? "w-24" : "w-16")} />
            )}
          </For>
        </div>
      )}
    </For>
  </div>
);

/** An inline "working…" line for panels that are small enough not to need rows. */
export const LoadingLine: Component<{ readonly label?: string }> = (props) => (
  <p class="px-3 py-6 text-center text-body text-ink-subtle" aria-live="polite">
    {props.label ?? "Loading…"}
  </p>
);

/* -------------------------------------------------------------------------- */
/* Error                                                                      */
/* -------------------------------------------------------------------------- */

export interface ErrorStateProps {
  readonly error: unknown;
  readonly onRetry?: () => void;
  readonly class?: string;
}

/**
 * The full-panel failure. It says what failed, whether retrying is worth it,
 * and carries the request id — the server promises every message here is safe
 * to render and contains no secret and no raw payload.
 */
export const ErrorState: Component<ErrorStateProps> = (props) => {
  const api = (): ApiError | null => (props.error instanceof ApiError ? props.error : null);

  const title = (): string => api()?.headline ?? "Something went wrong.";

  const body = (): string => {
    const e = api();
    if (!e) return props.error instanceof Error ? props.error.message : String(props.error);
    if (e.status === 0) {
      return "The browser could not reach oto. It may be down, or this connection may be blocked.";
    }
    if (e.status === 401) return "This session is not signed in.";
    if (e.status === 403) return "This session is not allowed to see that.";
    if (e.status === 400 && e.code === "unknown_parameter") {
      return "oto rejected a filter it does not recognise. That is deliberate — an unknown parameter is refused rather than ignored, so a typo can never quietly return an unfiltered list.";
    }
    if (e.status === 400 && e.code === "cursor_filter_mismatch") {
      return "The page cursor was minted under different filters. Pagination has been reset.";
    }
    if (e.status >= 500) {
      return "oto failed to answer. This is a fault on oto's side, not in what you asked for — retrying is reasonable, and the request id below is what makes it findable in the logs.";
    }
    // The detail is the server's own sentence; repeating the title would just
    // say the same thing twice.
    return e.problem?.detail ?? e.message;
  };

  return (
    <div
      class={cn("flex flex-col items-center gap-2 px-6 py-12 text-center", props.class)}
      role="alert"
    >
      <p class="text-title font-semibold text-ink">{title()}</p>
      {/* `max-w-112` (28rem), not `max-w-md`: a named width key resolves against
          the spacing namespace first, so `max-w-md` compiles to 12px. */}
      <p class="max-w-112 text-body leading-relaxed text-ink-muted">{body()}</p>
      <Show when={api()?.violations.length}>
        <ul class="mt-1 max-w-112 space-y-0.5 text-left text-body text-ink-muted">
          <For each={api()?.violations}>
            {(v) => (
              <li>
                <code class="font-mono text-meta text-ink">{v.field}</code> — {v.message}
              </li>
            )}
          </For>
        </ul>
      </Show>
      <Show when={props.onRetry}>
        {(retry) => (
          <Button class="mt-2" variant="secondary" size="sm" onClick={() => retry()()}>
            Try again
          </Button>
        )}
      </Show>
      <Show when={api()?.requestId}>
        {(id) => (
          <p class="mt-1 font-mono text-micro text-ink-subtle" title="Quote this when reporting it">
            request {id()}
          </p>
        )}
      </Show>
    </div>
  );
};

/** A compact inline banner for a failure that does not empty the whole screen. */
export const ErrorBanner: ParentComponent<{
  readonly error?: unknown;
  readonly class?: string;
}> = (props) => (
  <div
    role="alert"
    class={cn(
      "flex items-start gap-2 rounded-control border border-line-strong bg-raised px-2.5 py-2",
      "text-body leading-snug text-ink",
      props.class,
    )}
  >
    <span aria-hidden="true" class="mt-1 size-1.5 shrink-0 rounded-full bg-accent" />
    <div class="min-w-0">
      {props.children ??
        (props.error instanceof ApiError ? props.error.message : String(props.error ?? ""))}
    </div>
  </div>
);
