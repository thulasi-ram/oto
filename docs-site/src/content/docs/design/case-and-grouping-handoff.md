---
title: Handoff — the Case, the derived group, and what is not yet built
---

**Date:** 2026-08-18 · **Branch:** `main`
**State:** design agreed and recorded. **Nothing is implemented.**

---

## Where the work already lives — read these first, don't re-derive

| artifact | path / id |
|---|---|
| The design decisions, in full | `docs-site/src/content/docs/design/case-and-grouping.md` |
| The commit that added it | `22fab74` (pushed to `origin/main`) |
| The six tickets | `git bug bug --status open` — ids below |

```
dc4d731  high · contract      AlertOccurrence → AlertCase (+ the ADR)
8e54b18  medium · contract    remove alerts.ack_state
9318a6e  high · notification  notification reads alert_snoozes; the Quiet tab
bc691fa  high · contract      derived group key
fe73f9a  medium · ui          Leave is dead code; delete alert_group_members
33665d1  low · docs           stale planner note above Rollup
```

Dependencies: `fe73f9a` depends on `bc691fa`. `8e54b18` and `9318a6e` are independent of
everything. `33665d1` is a drive-by, unrelated.

The design doc carries the reasoning, the rejected alternatives and the open items. **This
handoff only carries what is not in it.**

---

## What the user actually decided, and how firmly

Firm, argued, not to be reopened without new information:

- `AlertCase` is the name. `Episode` was offered and rejected as "too big"; `AlertRollup`
  collides with `DeliveryRollup`; `AmAlert`/`OtoAlert` were withdrawn by the user once it was
  clear `alerts.Observation` already exists in code (`ingestion/service/process.go:213`).
- Ack is Case-only — column, button, filter, counter all move.
- Snooze stays Alert-scoped, and the main alert tab shows **only unsnoozed** alerts.
- Grouping tuning stays org-wide.

Held more loosely, worth a check-in before building:

- The split-key axes `(cluster, alertname, namespace-or-∅)`. The user agreed, but the doc's
  own open item says validate against replayed `ingest_batches.payload` first. Do that before
  the migration, not after.
- Whether the Case list keeps an acked-count grouped by namespace (needs a join to `alerts`
  for the promoted label).

---

## Things learned the hard way this session — don't rediscover them

- **`git bug bug new -F` takes the file's first line as the title and deletes it from the
  body.** `-t` is ignored. Put the title on line 1, blank line, then the body. Six tickets were
  created, removed and recreated over this.
- **Ticket house style is six sections**, in order: `**What is wrong.**` ·
  `**The demonstration.**` · `**Why it matters.**` · `**Where.**` · `**Scope.** small|medium|large` ·
  `**Done when.**`. Titles are declarative statements of the defect, never imperatives.
  ⚠️ Older tickets (`85da108`, `0ac132b`) use only four sections — calibrate on a recent one.
- **`git bug push` is separate from `git push`.** Bugs live in `refs/bugs/*`.
- **The shell is zsh; arrays are 1-indexed.** Collecting ticket ids into an array and labelling
  with `${IDS[0]}` shifted every label onto the wrong ticket, silently. Label with literal ids.
- **RTK mangles grep and test output** — line numbers survive, content does not. Use
  `rtk proxy <cmd>` whenever the content matters, and `Read` to verify any line you intend to
  cite in a ticket.
- **Gortex is inactive for this directory** (not a tracked repo). The `gortex-*` skills and MCP
  tools will not work; use Read/Grep.

---

## Corrections I made mid-session that are already folded into the doc

Listed so a fresh agent doesn't resurrect the earlier, wrong versions:

1. I claimed the group's per-member join in the notification snapshot would be expensive.
   It isn't — `alert_snoozes_active_idx` is `UNIQUE (alert_id) WHERE ended_at IS NULL`, so the
   build side is only *active* snoozes. This is why `9318a6e` is viable at all.
2. I claimed a left anti-join can't stop early under keyset pagination. With a tiny build side
   it streams. This is why the Quiet tab works.
3. I defended `alerts.ack_state` on the strength of one roll-up counter. The user pushed back
   correctly: within that very query, `acked` is the only counter that isn't a property of the
   alert.
4. I first recommended *wiring up* `Leave`. Under a derived group key the correct move is to
   *delete* it. `fe73f9a` reflects the second answer.

---

## Suggested first moves

1. **Start with `9318a6e` or `8e54b18`.** Both are self-contained, neither depends on the
   rename, and both remove a cross-module SQL read before anything else touches those tables.
   Sequencing note inside both tickets: redirect `notification/repository/snapshot.go` *before*
   dropping any column — a cross-module SQL read has no compiler error to break.
2. **`dc4d731` needs its ADR written before any code moves.** §A.1 requires the FR-1-by-name
   argument for `case` in writing. The argument and the Case/correlation line are already
   drafted in §3 of the design doc — lift them.
3. **`bc691fa` wants the replay validation before the migration.** The key is computable from
   `ingest_batches.payload` (30-day retention).

---

## Suggested skills

- **`grill-with-docs`** — highest value here. It stress-tests a plan against CONTEXT.md and the
  ADRs and updates documentation inline. This repo's doctrine is dense (SPEC §A.1 bans,
  SCOPE-BOUNDARY, `tools/lintvocab`) and `dc4d731` in particular is a vocabulary change that has
  to survive that doctrine by name.
- **`tdd`** — the "Done when" clause of every ticket is already written as assertions. `fe73f9a`
  and `8e54b18` in particular name the exact test.
- **`code-review`** — before each commit; the repo's bar for cited line numbers and measured
  claims is high.
- **`run`** — for `9318a6e`, whose deliverable is a visible UI tab with a severity-carrying
  badge. Worth seeing rather than asserting.

Skip `to-issues` and `to-prd` — the issues exist. Skip the `gortex-*` family — inactive here.

---

## Open items, verbatim from the doc's §11

- The Case-list acked count grouped by namespace — needs a join; decide if it's worth it.
- Ack surviving a re-fire inside `refire_grace` — agreed in discussion, needs a test to assert it.
- `service` as a fourth split axis — omitted for now, add only on evidence.
- Validating the split key against replayed payloads before committing to the axes.
