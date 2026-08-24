/**
 * `/proto/ui-preview` — oto's overlay chrome, drawn against fixtures, with no
 * session and no backend.
 *
 * ⛔ IT IS OUTSIDE THE AUTHENTICATED LAYOUT ROUTE, exactly as
 * `/proto/alerts-preview` is, and for the same reason: it must open on a laptop
 * with nothing running on :8080. `RequireSession` would hold the whole tree
 * behind a `/me` probe that can only 401, and `LiveProvider` would open an SSE
 * connection nobody can serve.
 *
 * ⭐ IT RENDERS THE SHIPPING COMPONENTS, NEVER COPIES OF THEM. `Modal` and
 * `SnoozeDialog` are imported and fed props, so what a reviewer sees here is
 * what an operator will see. Nothing in either component was changed to
 * accommodate this page.
 *
 * The only stub is a second `QueryClient`. `SnoozeDialog` runs its submission
 * through TanStack's `useMutation`, which needs a provider in scope; this one
 * never reaches a network because the dialog's `onSubmit` is a local promise
 * and no `queryFn` here talks to anything. No `qk` key is written by hand —
 * nothing on this page reads a query at all.
 *
 * Never imports from a sandbox feature and never reads private CSS variables
 * belonging to one. This screen is oto's own tokens or nothing.
 */
import { For, createSignal, type Component, type ParentComponent } from "solid-js";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";

import { Button } from "~/components/ui/Button";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import { PageEmptyState } from "~/components/ui/states";
import { PageHeading } from "~/components/ui/surfaces";
import { setTheme, theme } from "~/design/theme";
import { SnoozeDialog } from "~/features/alerts/SnoozeDialog";

/* -------------------------------------------------------------------------- */
/* The stub: a client that answers nothing, because nothing here asks          */
/* -------------------------------------------------------------------------- */

const previewClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: Infinity,
      gcTime: Infinity,
      retry: false,
      refetchOnMount: false,
      refetchOnWindowFocus: false,
      refetchOnReconnect: false,
    },
    mutations: { retry: 0 },
  },
});

/* -------------------------------------------------------------------------- */
/* Page chrome                                                                */
/* -------------------------------------------------------------------------- */

/** The label that stops a screenshot of this page being mistaken for production. */
const PreviewHeader: Component = () => (
  <header class="sticky top-0 z-30 flex shrink-0 items-center gap-md border-b border-line-strong bg-raised px-lg py-sm">
    <span class="rounded-chip border border-accent-border bg-accent-fill px-2xs py-0.5 text-micro font-semibold uppercase tracking-widest text-accent">
      Design preview
    </span>
    <span class="text-meta text-ink-muted">
      Fixture data — no session, no backend, nothing here is real.
    </span>
    <div class="flex-1" />
    <span class="text-micro uppercase tracking-widest text-ink-subtle">{theme()}</span>
    <Button
      variant="secondary"
      size="sm"
      onClick={() => setTheme(theme() === "dark" ? "light" : "dark")}
    >
      Switch to {theme() === "dark" ? "light" : "dark"}
    </Button>
  </header>
);

/** One reviewable band: a title, a sentence about what it is, and the trigger. */
const Band: ParentComponent<{ readonly title: string; readonly note: string }> = (props) => (
  <section class="flex flex-col gap-sm border-b border-line px-lg py-md">
    <h2 class="text-title font-semibold tracking-tight text-ink">{props.title}</h2>
    <p class="text-meta text-ink-muted">{props.note}</p>
    <div class="flex flex-wrap items-center gap-sm pt-2xs">{props.children}</div>
  </section>
);

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

const LONG_PARAGRAPHS: readonly string[] = [
  "A rule change is not a state change. When a rule's expression, its `for` window or its labels move underneath an alert, oto keeps rendering the alert exactly as it was firing, and records the drift beside it rather than folding the two together.",
  "The snapshot below is the rule as oto last saw it evaluated, not the rule as it is written in your repository now. Those two disagree more often than anyone expects, and the disagreement is the interesting part.",
  "Every field is shown even when it is unchanged, because a diff that hides the unchanged fields cannot tell you that a field you were relying on has quietly stopped existing.",
  "Annotations travel with the alert, not with the rule, so an annotation that was correct at fire time stays visible after the rule that produced it has been rewritten.",
  "Attribution is per-gesture. Whoever acknowledged, snoozed or commented is named on the row, and the name survives the alert resolving.",
  "Delivery is separate again: an alert can be firing, unacknowledged and undelivered all at once, and conflating any two of those three is how a channel goes quiet without anybody noticing.",
  "Retries are bounded and announced. A notification that will not be retried says so, and says why, rather than sitting in a queue that looks busy.",
  "Nothing here polls. Frames arrive over the live connection or the shell says out loud that they are not arriving.",
  "A resync replaces the whole visible page rather than patching it, because a partially-patched list is a list that is lying about its own completeness.",
  "None of the above is configurable. The honesty machinery is the product.",
];

/* -------------------------------------------------------------------------- */
/* The bands                                                                  */
/* -------------------------------------------------------------------------- */

const StandardModalBand: Component = () => {
  const [open, setOpen] = createSignal(false);
  return (
    <Band
      title="Modal — the usual composition"
      note="Title, description, a body of prose, and a footer of actions. This is the shape every alerts-area overlay is built from."
    >
      <Button variant="default" size="sm" onClick={() => setOpen(true)}>
        Open standard modal
      </Button>
      <Modal open={open()} onOpenChange={setOpen}>
        <ModalContent>
          <ModalHeader>
            <ModalTitle>Acknowledge this alert</ModalTitle>
            <ModalDescription>
              An acknowledgement says a human has seen this and is dealing with it. It does not
              resolve the alert, does not stop it firing, and does not suppress a single
              notification.
            </ModalDescription>
          </ModalHeader>

          <div class="flex flex-col gap-sm text-item leading-relaxed text-ink">
            <p>
              The alert stays at its own severity and stays in the default list. What changes is
              that everyone else can see it is claimed, and by whom.
            </p>
            <p class="text-meta text-ink-subtle">
              Attributed to you, kept in the history afterwards, and reversible.
            </p>
          </div>

          <ModalFooter>
            <Button size="sm" variant="secondary" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button size="sm" variant="default" onClick={() => setOpen(false)}>
              Acknowledge
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </Band>
  );
};

const LongModalBand: Component = () => {
  const [open, setOpen] = createSignal(false);
  return (
    <Band
      title="Modal — long content that must scroll"
      note="More body than fits the viewport. The panel must stay inside the screen, the header and footer must stay reachable, and the scroll must happen inside the panel rather than behind it."
    >
      <Button variant="default" size="sm" onClick={() => setOpen(true)}>
        Open long modal
      </Button>
      <Modal open={open()} onOpenChange={setOpen}>
        <ModalContent>
          <ModalHeader>
            <ModalTitle>What oto records about a rule change</ModalTitle>
            <ModalDescription>
              Ten paragraphs, deliberately taller than any laptop viewport.
            </ModalDescription>
          </ModalHeader>

          <div class="flex flex-col gap-sm text-item leading-relaxed text-ink">
            <For each={LONG_PARAGRAPHS}>{(para) => <p>{para}</p>}</For>
          </div>

          <ModalFooter>
            <Button size="sm" variant="secondary" onClick={() => setOpen(false)}>
              Close
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </Band>
  );
};

const ConfirmModalBand: Component = () => {
  const [open, setOpen] = createSignal(false);
  return (
    <Band
      title="Modal — small confirm"
      note="One sentence and two buttons. The narrow end of the range: the panel must not stretch to its maximum width just because it can."
    >
      <Button variant="secondary" size="sm" onClick={() => setOpen(true)}>
        Open confirm modal
      </Button>
      <Modal open={open()} onOpenChange={setOpen}>
        {/* `max-w-96` (24rem), not `max-w-sm`: see the ⛔ block in `Modal.tsx` —
            the named spacing steps shadow the t-shirt width ladder, so
            `max-w-sm` compiles to 8px. A spacing multiple cannot be shadowed. */}
        <ModalContent class="max-w-96">
          <ModalHeader>
            <ModalTitle>Discard this comment?</ModalTitle>
            <ModalDescription>What you have typed is not kept anywhere.</ModalDescription>
          </ModalHeader>
          <ModalFooter>
            <Button size="sm" variant="secondary" onClick={() => setOpen(false)}>
              Keep editing
            </Button>
            <Button size="sm" variant="destructive" onClick={() => setOpen(false)}>
              Discard
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </Band>
  );
};

const SnoozeBand: Component = () => {
  const [openAlert, setOpenAlert] = createSignal(false);
  return (
    <Band
      title="SnoozeDialog"
      note="The shipping dialog. A snooze is always a hold on ONE alert — there is no second subject. Submitting resolves locally against nothing — no request leaves the browser."
    >
      <Button variant="secondary" size="sm" onClick={() => setOpenAlert(true)}>
        Snooze an alert
      </Button>
      <SnoozeDialog
        open={openAlert()}
        onClose={() => setOpenAlert(false)}
        onSubmit={() => Promise.resolve(null)}
        onSuccess={() => undefined}
      />
    </Band>
  );
};

/**
 * §M.9's decorative ink, in both themes, on one page.
 *
 * ⭐ IT IS HERE BECAUSE INK IS THE ONE THING A UNIT TEST CANNOT SEE. `ink.test.ts`
 * proves the composites, the derivation and the `forced-colors` guard;
 * `Ink.test.tsx` proves the DOM contract. Neither can tell you that a mask
 * letterboxed itself into invisibility, that a brush is sitting a step too high,
 * or that the two heading hues collapse into one another in dark — which they
 * nearly do, and which this band is the fastest way to see (ADR 0032).
 *
 * Every shape and both hues appear, including the `accent` variant no shipping
 * heading spends, because "legal but unspent" is exactly the thing a reviewer
 * needs to look at before spending it.
 */
const InkBand: Component = () => (
  <Band
    title="Decorative ink (§M.9)"
    note="Tier A, carries no fact, static. A brush goes behind a page heading and nowhere else; a motif goes in the corner of a full-page empty state. Toggle the theme above — all of it is one declaration per tint."
  >
    <div class="flex w-full flex-col gap-lg">
      <div class="flex flex-col gap-md bg-surface p-lg">
        <PageHeading brush="swipe">HighMemoryUtilisation</PageHeading>
        <PageHeading brush="rule">payments · critical · eu-west-1</PageHeading>
        <PageHeading brush="swipe" hue="accent">
          HighMemoryUtilisation
        </PageHeading>
        <PageHeading brush="rule" hue="accent">
          payments · critical · eu-west-1
        </PageHeading>
      </div>

      <div class="grid grid-cols-2 gap-md">
        <div class="h-64 bg-surface">
          <PageEmptyState
            motif="kumo"
            title="No alerts match these filters."
            body="The filters are doing something — that is not the same as there being nothing here."
          />
        </div>
        <div class="h-64 bg-surface">
          <PageEmptyState
            motif="sakura"
            title="Nothing has gone quiet."
            body="`expired` means oto stopped hearing about an alert — never that it resolved."
          />
        </div>
      </div>
    </div>
  </Band>
);

/* -------------------------------------------------------------------------- */
/* The route                                                                  */
/* -------------------------------------------------------------------------- */

const ProtoUiPreviewRoute: Component = () => (
  <QueryClientProvider client={previewClient}>
    <div class="flex min-h-screen flex-col bg-bg text-ink">
      <PreviewHeader />
      <div class="flex flex-col">
        <StandardModalBand />
        <LongModalBand />
        <ConfirmModalBand />
        <SnoozeBand />
        <InkBand />
      </div>
    </div>
  </QueryClientProvider>
);

export default ProtoUiPreviewRoute;
