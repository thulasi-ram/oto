# 0049 — A Wording is selected by its own clause, not by a route

**Status:** Proposed · 2026-08-22 · amends [0037](0037-wordings-are-liquid-and-structure-stays-otos.md)'s
open question, closes it
**Relates to:** [0017](0017-matchers-over-cel.md) (matchers, not an expression language),
[0037](0037-wordings-are-liquid-and-structure-stays-otos.md) (a Wording is Liquid and structure stays oto's),
[0048](0048-a-wording-is-spelled-by-its-channel.md) (a Wording carries no provider's punctuation)
**Design note:** [notification-content-customisation.md](../design/notification-content-customisation.md)

## Context

ADR 0037 shipped the policy-scoped override **as an open question rather than a decision**, and named the
two things that were unanswered:

> Two things are unanswered: which schema validates a `card`/Wording patch on `notification_policies`, a
> table with no provider context whose `channel_ids` may name up to 16 channels of mixed type; and whether
> it is acceptable that, under first-match-wins (`policy.go:214-217`), an override costs a duplicated
> routing rule. The second is the stronger argument that presentation should not ride the policy table at
> all.

This ADR answers both. The answer to the second is: it is not acceptable, and presentation does not ride
the policy table.

## Decision

**A Wording carries its own `when` clause, and that clause selects it. Routing is not consulted.**

ADR 0037 already gave a Wording a predicate — `When { Matchers []Matcher; Reasons []Reason }` and a
`Priority` — reusing ADR 0017's vocabulary verbatim. Once it has one, it does not need to borrow a
policy's, and the entire difficulty of the open question was the borrowing.

**Two scopes, most-specific first, and no cascade to resolve.**

1. **Org-scoped Wordings** are the house voice: every card this tenant sends.
2. **Channel-scoped Wordings** are the exception, per destination.

A channel-scoped Wording beats an org-scoped one. Within a scope, the `when` clause filters and `Priority`
orders — **LOWER FIRST, first match wins**, the same convention `policy.go:214-217` already states for
routing, because two orderings that read the same way and behave differently is how an operator learns to
distrust both. Resolution is per Stanza, so exactly one Wording wins per Stanza and there is nothing to
merge.

**Most-specific-wins, rather than the design note's layering.** The note put the Channel's Wordings at the
bottom as "the default, per destination" and the override above them. That is upside down once the override
is no longer a routing rule: a rule naming one destination is more specific than one naming a whole tenant,
and a reader who has configured `#security` to speak differently will not expect an org-wide setting to
overrule it.

### Open question 1 — which schema validates a Wording

**None, because there is nothing provider-specific left to validate.** ADR 0048 made a Wording's output
provider-neutral: filters emit neutral marks that each Dialect spells, timestamps are a fact rather than
Slack's `<!date^…>` spelling, and audience refusal is per-provider at the sink. What remains to validate is
the same for every provider present and future:

- the Stanza is one of the four that take prose;
- the template parses, and its delimiters are balanced;
- every field it names exists in `StanzaInput`, checked against a maximal view so that a field a *digest*
  lacks is not mistaken for a typo;
- every filter it names is in the curated set — checked by RENDERING against the fixture corpus, because an
  unknown filter is a render-time error in Liquid and a parse would miss it;
- it renders something on an ordinary card;
- the source fits the one-line-of-prose ceiling.

So the question "which of the up-to-sixteen channels' schemas applies" has no referent. The three candidate
answers ADR 0037 listed were all attempts to pick a provider; the fix was to stop needing one. **A Wording
bound to a Channel is still validated identically** — the binding decides where it applies, never what it
may say.

### Open question 2 — the routing fork

**Dissolved, not accepted.** Under the design note's sketch, an operator who wanted `#security` to read
differently had to create a higher-priority *policy* and re-declare `reasons`, `channel_ids` and `throttle`
— duplicating the routing rule they wanted to leave alone, and creating a second thing that can drift. With
selection moved onto the Wording's own clause, changing how something reads costs a Wording. Routing is
untouched, and the duplicate that would have drifted never exists.

## Consequences

**`notification_policies` gains no column, and its binding block is not argued past.** `policy.go`'s header
and `db/migrations/00019_unacked_reminder.sql:15-19` both bind that table to routing a FACT to a
DESTINATION. A presentation column would have been the first member of a second category on a table whose
guard rail exists precisely to keep it single-purpose. Nothing here goes near it.

**`notifications.policy_id` keeps meaning exactly what it meant.** It records which policy routed the
notification. It never had to also mean "which policy decided how this reads", and now it never will.

**The resolved Wording set is persisted on the delivery row**, beside the rendered payload
`dispatch.go` already stores under §L.6. "Why did my card read like that" is answerable from one row rather
than from a replay against configuration that may since have changed — which is the same argument that put
the rendered payload there, applied to the thing that produced it.

**Resolution happens where the facts are, and the renderer stays pure.** The `when` clause is matched in
`notification/service`, which is where `Matcher` and the closed `Reason` enum live; only the winning
template per Stanza crosses into `channels/domain` on `RenderOptions`. The renderer remains a pure function
of `(NotificationView, RenderOptions)` (SPEC §F.1), so golden-file testability survives intact.

**Two scopes is a ceiling, and it is deliberate.** There is no per-source, per-severity or per-team layer,
and no inheritance. A third scope would reintroduce the cascade this decision exists to avoid, and the
`when` clause already expresses "only for critical checkout alerts" without one.

**What would falsify this:** an operator who wants one wording for a policy's traffic and a different
wording for the same channel's other traffic, and who cannot express the difference with matchers and
reasons. That is the case the policy binding would have served and this does not. Dogfooding against a real
Prometheus is what would surface it; if it recurs, the answer is a `policy_id` term in the `when` clause —
which is a predicate, not a column on the routing table, and would preserve everything above.
