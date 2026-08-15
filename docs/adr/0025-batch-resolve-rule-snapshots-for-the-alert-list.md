# 0025 — The alert list resolves rule snapshots in a batch, not per row

**Status:** Accepted · 2026-08-09
**Relates to:** [0009](0009-rule-snapshot-versioning-at-fire-time.md) (what a snapshot is),
[0002](0002-domain-first-layout-three-model-sets.md) and CONTEXT.md §5.4–§5.5 (the layering rule
this is about), `api/openapi/openapi.yaml` (`batchGetRuleSnapshots`, `RuleSnapshotRefDTO`)

## Context

"For every alert that has ever fired, oto shows … **what the rule said at that moment**" is the
first promise in the README and the whole point of ADR 0009. Until now the alert list — the screen
operators actually spend their time in — could not keep it.

`GET /api/v1/alerts?include=rule` embeds a `RuleSnapshotRefDTO`: an id, and nothing else. That is
deliberate and it is written down in three places (`alerts/api/handlers.go`, `alerts/api/dto.go`,
the contract), for two reasons that have not expired:

1. **`alerts/api` may not name `rules/domain`** (CONTEXT.md §5.4). Rendering a `RuleSnapshotDTO`
   means naming the rules module's types, and only `rules/api` may.
2. **Payload.** `expr` runs to 64 KiB, plus two label maps, per snapshot. Embedding that in every
   row of a 200-alert page is exactly the payload explosion `include=` exists to keep opt-in.

So the ref was correct — and it left the list with a capability gap rather than a conformance
defect. Showing `expr` or `for` on fifty rows meant fifty calls to `GET /alerts/{id}/rule`. The UI
therefore did the only sane thing and showed **no** rule at all, which loses the differentiator on
the busiest screen in the product. The alternative — doing the N+1 — makes the list slowest exactly
when there are most alerts, which is when it matters.

Note what was *not* wrong: the layering rule. `rules/api` already serves `/alerts/{id}/rule` and
`/occurrences/{id}/rule`, reaching `alerts` through an `AlertReader` port it declares itself. The
URL tree and the Go package tree are separate things, and the sanctioned pattern already existed.

## Decision

**`GET /api/v1/rule-snapshots/batch?id=<uuid>,<uuid>,…`**, owned by `rules/api`, returns the
`RuleSnapshotDTO` for a whole page of snapshot ids in one call. The alert list keeps its ref, asks
`include=rule` alongside `include=current_occurrence`, collects the distinct ids on screen, and
resolves them in **one further request**. Two requests render the list; never one per alert.

Shape, and the reason for each part:

- **Keyed by snapshot id, not by alert id.** The list already holds the ids. Keying by alert id
  would make the rules service walk alert → occurrence → snapshot per row, which is the same N+1
  moved to the server.
- **Content addressing is what makes it small.** A rule that has not been edited is *one* row
  however many alerts fired under it (ADR 0009), so a page of fifty alerts routinely asks about a
  handful of snapshots. Duplicate ids are therefore **accepted, not refused** — repetition is the
  signal — and collapsed server-side before the query.
- **An unknown id is absent from the result, never a 404.** `rule_snapshots` is append-only, so the
  only ways to miss are an id from another org or one a caller invented. Failing the request for
  either would blank the rule column of a page that is otherwise entirely answerable. Join by `id`;
  never by position.
- **No `page` object.** The caller enumerated the set; there is nothing to page through and no
  `next_cursor` that could ever exist.
- **Capped at 100 ids.** This is a URL-length bound, not a database one: 200 UUIDs is a 7.4 KB
  request line, which survives a default proxy header buffer only until a session cookie joins it.
  100 is exactly one page of the UI's list; a caller paging at the contract's 200 ceiling makes two
  calls, which is still constant in the size of the page.
- **`include=rule` costs nothing extra.** `alerts/api` resolves `current_occurrence` and `rule` from
  the same read, so a list that already asks for the occurrence pays nothing to ask for the ref too.

The UI renders `expr` in a `Rule` column, truncated, mono, Tier A chrome (§M) — the rule text is
never a state hue. Four different absences are rendered four different ways (no snapshot bound /
still resolving / resolved to nothing / captured as `unavailable`), because rendering all four as an
empty cell is "oto's silence is indistinguishable from no alert" in miniature. The column is not
drawn at all when the caller did not ask for `include=rule`, so a column of dashes can never claim
"oto captured no rule" when the truth is "nobody asked".

## Consequences

- The alert list keeps the product's first promise, at two requests per page instead of N+1.
- **The client joins, and the join is the cost.** The rule column arrives one round trip after the
  rest of the row. The table renders immediately and the column fills in; it never blocks the list.
- A page can render with a rule column that is partially resolved. That is visible and labelled, not
  silent.
- Snapshots are immutable, so the client caches them with `staleTime: Infinity` and keyed by the
  sorted distinct id set: two pages sharing their rules share one entry and one request.
- `rules` gains `SnapshotRepository.GetMany` and `Service.GetMany`. One indexed
  `WHERE org_id = $1 AND id = ANY($2)` — the primary key with the org filter on top.
- **The layering rule is unchanged and is now settled in writing.** `alerts/api` still names nothing
  in `rules`. The ⛔ comment in `alerts/api/handlers.go` now points here, so the next reader who
  wants to widen the ref finds the answer instead of re-litigating §5.4.

## Alternatives rejected

**Embed the full `RuleSnapshotDTO` in the alert list row.** The obvious shape — one request, no
client-side join, no partial states. Rejected on both of the grounds the ref was chosen for, and
neither has weakened:

- It requires either `alerts/api` importing `rules/domain` (a straight §5.4 breach and a new edge in
  the dependency graph `depguard` exists to forbid), or a consumer-declared port in `alerts/api`
  returning a locally-declared struct. The second is legal Go and worse in practice: it puts a
  hand-maintained fifteen-field copy of a `rules`-owned schema under `alerts`' ownership, so "what
  did the rule say" gets two answers that drift apart quietly. The contract already names this
  failure by name.
- It is a payload disaster on the exact page it is meant to serve: `expr` up to 64 KiB plus two
  label maps, × 200 rows, on the request an operator makes most often. `include=` exists to make
  that opt-in, and this would make the opt-in unusable.

**What that costs us.** The embedded shape's real advantage was *atomicity*: one response, one
consistent picture, no loading state in a column, no client-side join to get wrong. We gave that up
knowingly. The mitigation is that the join key is a UUID and the joined rows are immutable, which is
the easiest join there is — and that the four unresolved states are rendered honestly rather than as
blanks.

**A compact `RuleSummaryDTO` (just `expr` and `for`) embedded in the row.** Halves the payload
argument but not the ownership one: it is still a rules-shaped schema declared under `alerts`, and it
is a *third* rendering of a rule snapshot beside `RuleSnapshotDTO` and `RuleSnapshotRefDTO`. Three
renderings of one thing is how a contract starts disagreeing with itself.

**Batch by alert id (`/alerts/rules?alert_id=…`) instead of snapshot id.** Saves nothing — the
client still needs the alert list first, so the round trip is identical — and it costs an
alert → occurrence → snapshot resolution per id inside the service. All of the client cost, plus a
server-side N+1.

**Fold the ids into `GET /rule-snapshots` as an alternative selector.** One operation, two selection
modes, two pagination models: `source_id`+`rule_name` is a keyset list over `(captured_at, id)`,
while an id bag is unordered and unpageable. The codebase already refuses this shape twice for the
same reason — `listAlertRollups` is a separate operation from `listAlerts`, and `ListSnapshots` is
separate from `History` — and a `cursor` parameter that is meaningless half the time is how a
contract teaches clients to ignore it.

**Fix it in the UI by fetching rules only for visible rows.** Bounds the N+1 without removing it, and
couples the network to scroll position: fast scrolling issues and cancels requests for rows nobody
read. A virtualised table would then make the rule column the only part of the row that depends on
having been looked at.
