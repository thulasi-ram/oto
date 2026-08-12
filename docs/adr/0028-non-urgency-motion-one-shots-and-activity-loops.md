# 0028 — Non-urgency motion: one-shot events, honest activity loops, and nothing else

**Status:** Accepted · 2026-08-12
**Decided WITHOUT the owner.** See *How to overturn this*, below. One part of it was not open:
the ticket records the owner's instruction to **add a U9 rather than amend U4**, and that ordering
is honoured here rather than relitigated.
**Relates to:** [0012](0012-pastel-chrome-saturated-state.md) (the two-tier palette, and U1/U4 —
the rules this sits beside without touching), [0010](0010-sse-with-durable-resume.md) (the durable
resume point whose presence is the signal that was mistaken for a page load)
**Amends:** SPEC §M.3 (new rule **U9**), §M.7 (the motion gate, which knew of exactly one
animation), §J AC-47 (which asserted the same)
**Resolves:** git-bug `7b57c22` — *"The chime cannot swing: `connecting → live` is a
'sessionStorage is empty' signal, not a page-load signal, and SPEC §N forbids choosing a motion
rule without an ADR"*

## Context

The SPEC legislates exactly one animation. U4 says *"No flashing, no blinking, ever. The only
motion tied to urgency is a slow 2 s opacity pulse on the unacked-critical dot"*; AC-47 repeats
*"the only urgency motion is the unacked-critical pulse"*; the §M.7 gate asserts that pulse is
absent under `prefers-reduced-motion` and knows of nothing else to check.

Read carefully, U4 is a rule about **urgency** motion. It says nothing at all about motion that
carries no urgency — which is not the same as forbidding it. And four such animations already
ship:

| Where | What | Kind |
|---|---|---|
| `AppShell.tsx:60` | `motion-safe:animate-pulse` on the connection dot while `connecting` | 2 s opacity loop |
| `states.tsx:62` | `motion-safe:animate-pulse` on skeleton rows | 2 s opacity loop |
| `primitives.tsx:87` | `motion-safe:animate-spin` on the button spinner | 1 s uniform rotation |
| `Dialog.tsx:109` | `open:oto-enter` — 140 ms opacity + 2 px translate | one-shot |

So the true state of the product was: one animation legislated, four shipping under no rule, and
a fifth proposed — a single damped swing on the header fūrin when oto opens. §N is explicit about
what that fifth one costs: *"Implementers who find an ambiguity MUST raise it rather than choose.
An undocumented choice made by one agent is a compile error or a silent behavioural divergence for
the next four."* The gap is not that the swing is unbuilt; it is that nothing in the SPEC could
have told anyone whether it was allowed, and nothing could have told the authors of the four
existing animations either.

**The trigger was wrong in a way worth writing down, because it reads as right.** The obvious
signal for "oto has opened and is listening" is the connection reaching `live` for the first time
— `connecting → live`. It is not that. `stream.ts:309` chooses between the two opening states with
`this.#lastSeq === null ? "connecting" : "reconnecting"`, and `#lastSeq` is seeded in the
constructor from `sessionStorage` (`readResumePoint`, `stream.ts:429`). `connecting` therefore
means **"this tab holds no resume point"**, and that diverges from "this page just loaded" in both
directions:

- **A reload of a tab that has ever received a sequenced frame** keeps its resume point, opens as
  `reconnecting`, and never passes through `connecting` at all. The bell would be silent for the
  returning operator — precisely the person the mark is for. A cold start whose first fetch fails
  loses it too: `#scheduleRetry` runs on `!res.ok` and the state leaves `connecting` before
  anything is live.
- **A second tab, or a visitor whose site data was cleared,** has no resume point while the same
  install has been running for hours. `connecting` fires with no page load in sight — which is
  correct for what the field means and wrong for what the bell wanted.
- **A quiet install never gets a resume point at all.** `#lastSeq` is written only on a sequenced
  frame (`stream.ts:377`); heartbeats carry no sequence. So every reconnect re-enters `connecting`
  — on `online` (`:274`), on tab visibility while not live (`:287`), on the 50 s stall check that
  runs every 10 s (`:290`), on a clean end-of-stream (`:353`), and on every deploy. The bell would
  swing on tab focus, all day, on the install with the least going on.

There is a second and independent objection. Motion attached to a connection transition is
**state** motion: a silent visual channel saying "we went live", beside a labelled `aria-live`
badge that already says it. U1 requires at least two *labelled* channels, and connection health is
deliberately Tier A (`AppShell.tsx`, ADR 0012). A bell is not a label.

## Decision

### 1. A new rule, U9, in §M.3. U4 is untouched.

> **U9 — Non-urgency motion is legislated, and comes in exactly two kinds.**
>
> **(a) One-shot.** Triggered by a discrete event (a mount, an open, a commit), ≤ 200 ms,
> `opacity` and `transform` only, never looping. A one-shot that is *purely decorative* — brand
> motion, carrying no fact — fires **at most once per document**.
>
> **(b) Indeterminate-activity.** A loop is permitted only while something is genuinely pending
> (loading, connecting, in flight), must stop when the pending thing does, and is limited to an
> opacity cycle of period ≥ 1 s or a uniform rotation. It is never the sole carrier of the fact
> that something is pending — a text label carries it too.
>
> Both kinds: no luminance oscillation beyond what U4 already forbids, never the sole carrier of a
> state fact, and absent under `prefers-reduced-motion: reduce`. **No animation announces a
> connection-state change** — a (b) loop may run *while* the connection is pending, beside the
> label that says so, but nothing may mark the transition itself. Connection health is a labelled
> Tier-A fact, and a silent channel for it violates U1.
>
> **Amended by §5 below**, which adds the closing clause placing colour transitions outside both
> kinds and bounding them separately. **§M.3 carries the complete rule**; this block is the two
> kinds alone, and copying it anywhere as if it were the whole of U9 reproduces the defect §5
> exists to fix.

U4 keeps its unqualified sentence. The missing rule was about a whole class of motion, not an
exception to "no flashing, no blinking, ever", and carving an exception into an absolute is how
absolutes stop being read.

### 2. The four shipping animations are accounted for, and all four are legal under U9.

The connection dot, the skeleton rows and the button spinner are form **(b)**: each loops only
while its subject is genuinely pending, each stops when it resolves, each sits beside a text label
saying the same thing (`Connecting…`, the row's own shape, the button's disabled label), and each
is already written `motion-safe:`. The dialog's `oto-enter` is form **(a)**: 140 ms, opacity and
transform, once per open. Nothing has to be deleted, and nothing was blessed retroactively without
being looked at.

Note what this cost the drafting: the ticket's draft U9 was a single clause — *one-shot, ≤ 200 ms,
opacity and transform only* — and it would have outlawed three of the four. A rule that quietly
makes the shipping product illegal is not a rule, it is a future argument. Hence two kinds.

### 3. The fūrin swings for 180 ms, on its own mount, once per document.

- **Trigger: the header mark's first mount in a document.** That is the only page-load fact a
  component can observe without asking anything else, and it is owned by `Chime.tsx` rather than
  by `stream.ts`. A reload rings (new document); a soft navigation that remounts the shell does
  not (same document); no connection state can reach it, because none is wired to it.
- **The latch is a `data-` attribute on `<html>`**, not a module variable and emphatically not
  `sessionStorage`. "Once per document" is what the rule says, and the document is the thing that
  dies when the page does. Storage outlives a reload — putting the latch there would reproduce the
  original defect one level down, silencing the bell for every visit after the first and making
  "did it ring" depend on the user's storage settings.
- **180 ms, rotation only**, `0° → 8° → −3.2° → 0°` on a `cubic-bezier(.32,.72,.35,1)`, pivoting
  at `transform-origin: 50% 15%` — the crown of the dome, where the cord would be. At the 8° peak
  the mouth travels ≈ 1.4 px at 16 px, a little over one hairline: legible on a two-stroke
  dome, which is all the mark has (a tanzaku or a clapper is sub-pixel mush at that size, and the
  ticket's blocker 2 records the arithmetic).
- **The rotation is applied to the `<svg>` element, never to an inner `<g>`.** The SVG viewport
  clips at `overflow: hidden`, so an inner group swings its shoulders out of the box, and
  `overflow: visible` is no escape because the wordmark sits 6 px away. Rotating the element
  rotates its clip with it; the furthest corner travels 0.25 px past its own edge, a quarter of
  the gap it has.
- **Suppression is doubled and neither half is a list of names.** The call site writes
  `motion-safe:oto-chime-swing`, so under `reduce` no rule is generated at all; and the
  `prefers-reduced-motion` sweep in `@layer base` (`index.css`) neutralises `animation-duration`
  on `*` regardless. Commit `d271be1` is why the second half works for variants: the utility is
  declared with `@utility`, so `motion-safe:` composes and the sweep still reaches the generated
  `.motion-safe\:oto-chime-swing` selector, which a hand-maintained class list would have missed.

### 4. The §M.7 gate and AC-47 stop asserting that there is exactly one animation.

Both now say what is actually true: one *urgency* animation, some number of U9 animations, and
**all** of them absent under `prefers-reduced-motion`. The enforcement of that last clause is
structural — `index.css.test.ts` asserts the guard sweeps `*` rather than naming classes, so it
cannot fall behind the next animation someone adds.

### 5. A colour transition is not motion, and U9 says so in bounds rather than by silence.

U9 as drafted in §1 above says *"`opacity` and `transform` only"* in clause (a) without saying
what that list was scoped to. Read as written, it governs every CSS transition in the product — and on
the day it was written it outlawed five shipping call sites, none of which is an animation and
none of which §2 classified:

| Where | What |
|---|---|
| `primitives.tsx:42` | `transition-colors duration-100` on the button base, for `hover:`/`active:` variant colours |
| `primitives.tsx:315` | the filter chip, settling between selected and unselected |
| `AppShell.tsx:218` | primary nav, settling on `aria-current="page"` |
| `AppShell.tsx:278` | the sign-out button, on hover |
| `settings.tsx:48` | the settings section tabs, on `aria-current="page"` |

This is the §2 mistake a second time, and it is worth naming as such: the draft rule was written
against the animations, the animations were counted, and the transitions were not looked at
because nobody thinks of `transition-colors` as motion. That instinct is right, and the fix is to
make the rule say it. A control settling from one Tier-A token to another displaces nothing,
repeats nothing, and cannot pull the eye — the property that makes U4 and U9 (b) worth
legislating is absent. It is the hover and focus feedback U7 and Tier A already require, written
in CSS rather than in two class names.

So U9 gains a closing clause: a colour transition is **neither kind**, and is bound instead by
three limits, all of which the five sites already satisfy —

- **colour properties alone** — `transition-colors`, never `all`, never a length, never a layout
  property. `all` is how a colour transition silently becomes a layout animation the next time
  someone adds a `padding` to the variant.
- **≤ 150 ms.** The five sites are `duration-100`. The ceiling is below U9 (a)'s 200 ms because a
  transition that can be re-triggered by every pointer movement across a nav bar has a lower
  tolerance than a once-per-open entrance.
- **between Tier-A tokens only** — because U5 already reserves saturated hue for state, and a
  control that *animates* into a state hue would be a silent state channel (U1) as well as a Tier-B
  leak. The five sites move between `accent`, `ink`, `raised`, `surface` and `line`; none names an
  `--oto-state-*` token.

The reduced-motion sweep in `index.css` flattens these regardless — it neutralises
`transition-duration` on `*`, not just `animation-duration` — so the "absent under `reduce`"
half of U9 already covers them without a word being added.

## Consequences

- Every new animation now has a rule to be judged against, and a reviewer has something to point
  at. The cost is that "just add a little transition" is now a question with an answer that can be
  *no*.
- The researched 1400 ms damped swing does not ship. What ships is a 180 ms tick: it reads as
  *the bell moved*, not as *the bell rang out*. That is a real loss of character, accepted
  deliberately — see the alternatives.
- The `<html>` element carries one `data-oto-chime-rung` attribute after the shell mounts. It is
  inert, and it is visible in devtools, which is the point: the latch can be inspected.
- `Chime.tsx` now touches `document`. It is still pure of connection state, colour and severity,
  and the one global it reads is the one the rule is defined in terms of.
- Anyone who wants the bell to mean "we are connected" must now amend U9 through §N rather than
  wire a signal, and the ADR says why they will fail.

## How to overturn this

Cheap, in this order:

1. **The duration.** Raise the U9 one-shot ceiling in §M.3, change `180ms` in the
   `oto-chime-swing` utility, and re-record the keyframes. Nothing else moves. If a designer looks
   at the tick and wants the researched swing, this is a one-line rule change and a one-line CSS
   change — but it must be that order, and the ADR must say what the new ceiling protects.
2. **The trigger.** It is one function, `swingClass` in `Chime.tsx`. Any replacement must survive
   the two cases in `Chime.test.tsx`: a reload holding a resume point (which never sees
   `connecting`) still rings, and a quiet install's endless reconnects never do.
3. **The two-kind shape of U9.** If the loops in form (b) are later deleted or replaced, U9
   collapses back to the ticket's single clause and this ADR should be amended to say so.

## Alternatives rejected

- **Amend U4 instead of adding U9.** The owner's instruction, and correct independently: U4's
  *"No flashing, no blinking, ever"* is an absolute, and the missing rule governs a class of
  motion U4 never claimed. Qualifying an absolute to make room for a wind-bell teaches the next
  reader that the absolutes are negotiable.
- **Write U9 first and let it bless the bell.** Rejected as the ordering trap the ticket names: a
  rule written to fit a feature is not a rule. The rule was written against the four animations
  already shipping, and then the bell was cut down to fit it — which is why the 1400 ms swing is
  not here.
- **The researched 1400 ms damped swing.** Fails the ≤ 200 ms ceiling this same ADR writes.
  Keeping it would have meant either a ceiling chosen to accommodate it (the trap above) or an
  argued exemption for the product's own logo, which is the least defensible thing to grant an
  exemption to. A long swing is also the most attention-costly motion in an ops tool, competing
  with the one animation that is *supposed* to pull the eye — the unacked-critical pulse.
- **`connecting → live` as the trigger.** The ticket's central finding, restated in Context: it
  means "no resume point in this tab", it misses every reload that has one, and it fires on tab
  focus forever on a quiet install. Rejected twice over, because even a reliable connection
  trigger would make motion a silent carrier of a state fact (U1).
- **A "first live of this session" flag owned by `LiveProvider` or `AlertStream`.** Fixes the
  reliability half and not the U1 half, and it pushes a brand concern into the transport layer,
  where the next reader has to discover that the stream now has a presentation responsibility.
- **`sessionStorage` (or `localStorage`) as the latch — "ring once per browser session".** This is
  the original defect wearing a different hat. Storage survives a reload, so the returning
  operator is greeted exactly once, ever, and whether the bell works depends on a storage
  permission. The document is the only thing whose lifetime *is* the page's.
- **No latch: ring on every mount of the mark.** Simpler, and wrong the day the shell remounts —
  sign-in, an error boundary, a layout-replacing route. A greeting that repeats within one page
  visit is a tic. It also makes the U9 (a) clause unenforceable for decorative motion generally.
- **A `swing` prop on `Chime`, set by `AppShell`.** Moves the decision to the call site, where the
  next call site will get it wrong, and makes "how many times has this rung" a question no single
  file can answer. The component owns its own trigger precisely so the answer is in one place.
- **Rotate an inner `<g>`, or set `overflow: visible` on the SVG.** The first clips the dome's
  shoulders against the viewport; the second escapes the clip and lands the bell in the wordmark
  6 px away. Rotating the element itself has neither problem.
- **Name `.oto-chime-swing` in the reduced-motion block.** A no-op. The block already forces
  `animation-duration: .01ms` and `animation-iteration-count: 1` on `*`, and a named rule would
  not even match the generated `.motion-safe\:oto-chime-swing` selector — a different selector, as
  `index.css.test.ts` explains at length.
- **Keep U9's bare *"opacity and transform only"* and strip the five `transition-colors` sites.**
  The rule was drafted without looking at them, so treating it as a finding about the code inverts
  which of the two was examined. It would also delete hover and focus feedback that U7 and Tier A
  require, to satisfy a clause whose author was thinking about the spinner — the §2 error exactly,
  and §2 already recorded that a rule which quietly makes the shipping product illegal is a future
  argument rather than a rule.
- **Say nothing about colour transitions and let "not motion" be understood.** What produced this
  defect. U9's whole purpose is that a reviewer has something to point at; a class of CSS that the
  rule appears to forbid and is understood to permit is worse than no rule, because the next
  reader resolves the ambiguity alone (§N).
- **Legislate nothing and ship the swing anyway.** What §N exists to prevent, and what produced
  four unlegislated animations already.
- **Delete the four existing animations so "exactly one animation" stays true.** Would remove
  honest pending-state feedback (spinner, skeletons, connecting dot) to preserve a sentence. The
  sentence was the thing that was wrong.
