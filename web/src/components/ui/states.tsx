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
import { For, Show, type Component, type JSX, type ParentComponent } from "solid-js";

import { ApiError } from "~/api/client";
import { Button, cx } from "./primitives";

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
    class={cx("flex flex-col items-center justify-center gap-2 px-6 py-14 text-center", props.class)}
  >
    {/* A quiet chime mark — the product's own glyph rather than a generic
        "no data" illustration, and the only decorative art in the app. */}
    <svg viewBox="0 0 40 40" class="size-8 text-line-strong" aria-hidden="true">
      <path
        d="M20 6c-5 0-9 4-9 9v7l-3 5h24l-3-5v-7c0-5-4-9-9-9Z"
        fill="none"
        stroke="currentColor"
        stroke-width="1.6"
        stroke-linejoin="round"
      />
      <path d="M17 31a3 3 0 0 0 6 0" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
    </svg>
    <p class="text-[14px] font-medium text-ink">{props.title}</p>
    <Show when={props.body}>
      <p class="max-w-sm text-[12px] leading-relaxed text-ink-muted">{props.body}</p>
    </Show>
    <Show when={props.action}>
      <div class="mt-1">{props.action}</div>
    </Show>
  </div>
);

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
    class={cx("block rounded-[3px] bg-sunken motion-safe:animate-pulse", props.class)}
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
          class="flex items-center gap-4 border-b border-line px-3"
          style={{ height: "var(--oto-row-h)" }}
        >
          <For each={Array.from({ length: props.cols ?? 5 })}>
            {(_, i) => (
              <Skeleton class={cx("h-2.5", i() === 0 ? "w-40" : i() === 1 ? "w-24" : "w-16")} />
            )}
          </For>
        </div>
      )}
    </For>
  </div>
);

/** An inline "working…" line for panels that are small enough not to need rows. */
export const LoadingLine: Component<{ readonly label?: string }> = (props) => (
  <p class="px-3 py-6 text-center text-[12px] text-ink-subtle" aria-live="polite">
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
      class={cx("flex flex-col items-center gap-2 px-6 py-12 text-center", props.class)}
      role="alert"
    >
      <p class="text-[14px] font-semibold text-ink">{title()}</p>
      <p class="max-w-md text-[12px] leading-relaxed text-ink-muted">{body()}</p>
      <Show when={api()?.violations.length}>
        <ul class="mt-1 max-w-md space-y-0.5 text-left text-[12px] text-ink-muted">
          <For each={api()?.violations}>
            {(v) => (
              <li>
                <code class="font-mono text-[11px] text-ink">{v.field}</code> — {v.message}
              </li>
            )}
          </For>
        </ul>
      </Show>
      <Show when={props.onRetry}>
        {(retry) => (
          <Button class="mt-2" size="sm" onClick={() => retry()()}>
            Try again
          </Button>
        )}
      </Show>
      <Show when={api()?.requestId}>
        {(id) => (
          <p class="mt-1 font-mono text-[10px] text-ink-subtle" title="Quote this when reporting it">
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
    class={cx(
      "flex items-start gap-2 rounded-[4px] border border-line-strong bg-raised px-2.5 py-2",
      "text-[12px] leading-snug text-ink",
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
