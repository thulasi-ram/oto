# 0039 — Group close stays activity-driven

**Status:** Accepted · 2026-08-18

**Amends:** [0038](0038-the-group-key-is-derived-from-the-alerts-own-labels.md) — records what the
derived key deliberately did *not* change. ADR 0005's thread-ownership claim survives unamended.

## Context

git-bug `fe73f9a` collapsed `alert_group_members` into `alert_cases.group_id`. Its **Done when**
went one clause further: group close should become *"no live case with this split key"* rather than
`last_activity_at` plus a delay. That clause was not implemented, and an omission with no written
reason is indistinguishable from an oversight. This ADR is the reason.

The clause is attractive because, after `00051`, membership is derived rather than recorded — so
"are there live cases?" is now a cheap, always-true question rather than a join against a table
that could drift. The temptation is to make close ask exactly that question and nothing else.

## Decision

**`CloseIdle` stays driven by `last_activity_at`.** The rollup's refusal to close over a live member
stays as the safety property. Three grounds:

1. **Taken literally, the clause deletes `group_close_delay`.** A generation whose cases have all
   ended would close on the next sweep. An alert that resolves and re-fires inside the delay — the
   ordinary flapping case the delay exists for — would then find its generation gone and open a new
   one, and a new generation is a **new Slack thread that cannot be re-parented**. The clause trades
   a correctness property nobody reported broken for a visible regression in the product's most
   visible output.

2. **The safety goal the clause states is already held.** `CanCloseAt` refuses while
   `counts.Live() > 0`, over a rollup re-derived *inside* `CloseIdle`'s own transaction. What
   `00051` changed is that this refusal became **true rather than nominal**: the firing/suppressed
   counts now derive from `alert_cases.ended_at`, which the state machine writes on every T5/T6.
   Before `00051` the same check consulted a recorded membership row that could lag; after it, the
   check cannot be stale. The ticket's motivation was satisfied by the part that shipped.

3. **The two rules differ only when the delay is doing its job.** "No live case" and
   "no live case *and* idle for `group_close_delay`" agree on every generation except one that has
   gone quiet within the window — which is precisely the generation the delay exists to hold open.

## Consequences

- `group_close_delay` keeps its meaning, and a generation may sit open with zero live cases for the
  length of the window. This is the re-fire grace, not a leak.
- The safety property is now **asserted, not assumed**: a test pins that a generation holding a live
  case is refused closure (`Held == 1`) rather than merely that the sweep ran.
- `fe73f9a` is closed as **partially implemented, deliberately**. The unbuilt clause is recorded
  here rather than left as a silent gap in the ticket's Done-when.
- If the delay is ever shown to hold generations open long enough to matter, the fix is to tune the
  delay — an org-wide knob that already exists — not to remove it.

## Alternatives rejected

| considered | rejected because |
|---|---|
| Implement the clause verbatim | deletes `group_close_delay`; every short resolve/re-fire strands a Slack thread |
| Close on "no live case" but keep the delay as a second condition | that is the behaviour already shipped, written twice |
| Close immediately and re-parent the thread on re-fire | Slack has no re-parent; a thread's identity is its first message |
| Leave the omission undocumented in the ticket | an unexplained gap reads as an oversight and invites the next agent to "fix" it |
