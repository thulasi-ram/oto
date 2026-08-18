---
title: 0037 — The card's shape is oto's, and the knobs over it are typed
---
**Status:** Proposed · 2026-08-18 · awaiting ratification; amends SPEC §L.5.1, §H.1, §H.3, §I.1
**Relates to:** [0008](/adr/0008-slack-update-in-place-primary/) (the card's structure and why a broken
layout is a dead delivery), [0017](/adr/0017-matchers-over-cel/) (the standing refusal of an expression
language), [0020](/adr/0020-broadcast-the-transitions-that-must-be-seen/) (the typed-knob precedent, and
the unread-knob trap)
**Design note:** [notification-content-customisation.md](/design/notification-content-customisation/)

## Context

Customers ask to customise notification content. The obvious shape is a template surface — a body
customers author, per channel, with an override cascade. Grafana OnCall, incident.io and Alertmanager
itself all ship one.

oto is in an unusual position, because the thing being asked for is the thing oto's own design memo
names as its best argument for existing. `red-team-memo.md:273`: the Slack receiver's templating is
"`text/template` in YAML — painful to maintain and impossible to make interactive. **This is your best
'why not just.'**" Alertmanager's own `payload:` escape hatch carries a "USE AT YOUR OWN RISK … THE
ALERTMANAGER TEAM WILL NOT PROVIDE ANY SUPPORT" warning (`domain-research.md:220`).

There is a real argument on the other side and it should not be dismissed. Customers do ask, the ask is
legitimate, and "no" is a support tax of its own. A template surface is also the only answer that is
obviously sufficient — it cannot be under-expressive, because it is a language.

The question was therefore not "is a language nice" but **"what does a customer still need that they
cannot already get?"** Measuring that changed the answer.

A rule author already owns three of the card's prose surfaces. `annotation(v, keys...)`
(`render/slack/renderer.go:163`) reads the alert's own annotations into the title subtitle
(`root.go:82`), the body (`root.go:92`) and the top-level text (`root.go:757`) — and those annotations
are already templatable with Alertmanager's `{{ $labels }}` / `{{ $value }}`, in the customer's GitOps
repo, failing in the customer's own CI. Per-alert wording, tone, affected resource, metric value,
impact copy, runbook URL, severity phrasing, team naming, per-environment variants and localisation are
all already reachable that way.

What remains is what Prometheus does not know: firing duration, ack state, occurrence and flap counts,
the trail, rule-change diffs, cross-group state, oto deep links, snooze state, and `expired`. **Roughly
six and a half of those nine are already rendered**, with two genuine gaps: the `alert.history` payload
reaches no surface at all (it is never named under `render/slack/`; the `enriched` reply shows a count and
a derived label, `reply.go:295-308`), and **snooze is not in the read model** — `AlertFacts.SnoozedUntil`
and `Snapshot.SnoozedAlerts` exist (`notification/domain/snapshot.go:193`, `:345`) and
`notification/service/view.go` never copies them.

So the residue is which of oto's own facts appear, and where — a closed, finite selection over
`NotificationView`'s named fields into a closed, finite layout (SPEC.md:3342: "**Block budget: 8 base
blocks** … Ceiling is 50"). A language buys expressiveness with nothing left to express, because the
unbounded space is already covered upstream. Note what the corrected count implies for sequencing:
**closing the two gaps is worth more than any knob**, and they are separate work.

Three costs make the language actively harmful rather than merely unnecessary:

**A customer error becomes an oto page.** `ClassConfigInvalid` is *"dead. oto sent something the provider
refused; this is an oto bug and raises a banner on the destination"*
(`internal/notification/domain/notification.go:84-86`); the matching `HealthConfigInvalid` adds *"and must
be alerted on"* (`internal/notification/domain/channel.go:53-55`). SPEC.md:3574 routes `invalid_blocks`
there and annotates it "This is an oto bug." A user template makes a typo indistinguishable from a defect
and sets `channels.health_status='config_invalid'`.

**Build-time validation stops describing reality.** SPEC.md:3218 — "**Every renderer is a pure function
with a checked-in `testdata/*.golden.json`.** … We never discover a broken layout in production" — and
SPEC.md:4717 runs `Validate` over every golden file in CI. Customer-authored bytes are unreachable by
that. The hazard is proven, not hypothetical: `validate.go` records that image blocks are whitelisted
but never emitted and V10's required-field checks were absent, so *the first person to render a Grafana
panel would have hit a dead delivery.* Golden tests cover only the paths oto walks.

**This codebase has already shipped a content knob and had to delete it.** ADR 0020:445-450 —
`channels.config.mention_on_reminder`, schema-validated, rendered into the settings form, documented,
"**was never read** … exactly the trap this amendment exists to avoid, already shipped."

## Decision

**oto does not evaluate user-supplied strings in the render path.** No `text/template`, no `{{ }}`, no
mini-language, no CEL. The card's structure, palette and top-level sentence are oto's, and they are not
user-authored.

**The two residue gaps are closed first, as renderer and view work.** Snooze state and expiry are copied
into `NotificationView` and rendered; the `alert.history` payload is rendered. Neither needs a
configuration surface to be worth doing, and both are worth more than any knob.

**Customisation is expressed as named typed knobs over facts oto already computes**, carried by the
existing per-provider JSON Schema — which already validates writes and renders the settings form from the
same bytes (`configschema/schema.go`: "There is no second copy of these rules anywhere"). The v1 surface is
a `card` object in §L.5.1's slack schema (`additionalProperties: false` preserved) holding `show_rule`,
`show_members`, `show_history`, `show_footer_receiver`.

**The `card` booleans are state-scoped, not flat.** A knob may suppress a Stanza on a *firing* card only.
SPEC.md:3382 binds the rule snapshot, members, trail and overflow links onto `resolved`/`expired` cards, so
a flat boolean would silently violate a binding rule. `show_trail` is not offered at all.

**Field ordering is NOT in scope**, and the earlier proposal to pin an enum to §H.7 is withdrawn as
unsound. SPEC.md:3516 is a row of the character-and-item-limits table whose column is "Renderer behaviour
on overflow" — a *drop order*, not a render order, and in `root.go:109-117` they are one dial, because
`add()` returns silently once ten fields are reached. A user reordering therefore decides what sheds, which
depends on which values are non-empty — i.e. on the alert data — and can silently drop SPEC.md:3382's bound
terminal fields. It also cannot name the real field set: terminal cards add twelve fields, four of whose
labels are absent from §H.7's eight, and `Firing-for` names four different fields by state
(`durationLabel`, `root.go:650-663`). Revisit only after §H.7 declares a render order distinct from its
shed order.

**Override is answered as a single overlay, not a cascade, reusing matchers.** A Channel's `card` is the
default; an optional **sparse, tri-state `card` patch on `notification_policies`** overlays it. There is no
precedence table to invent, because `policy.go:214-217` already binds that *"the first policy that matches
wins and no other policy is consulted — which is why `notifications` carries a single `policy_id` rather
than a join table."* Exactly one policy wins, so exactly one patch applies. Absent ≠ false, so inherit stays
distinguishable from decision. `notifications.policy_id` already records which policy won; what is added is
**the resolved card on the delivery row**, so "why did my card look like that" needs one row rather than a
replay against config that may since have changed — the same reasoning that already makes
`dispatch.go:583-587` persist the rendered payload, citing §L.6. Resolution happens where routing happens;
the renderer stays a pure function. This adds a column to `notification_policies`, a bound under §N step 4.

**Every knob is read by exactly one code path with a golden file proving it.** ADR 0020 is the standing
proof that an unread knob is worse than no knob.

**A customer setting cannot mark a delivery `dead`,** because every knob is structural and decidable at
write time, refused there with a 422 and a JSON pointer. This is a reason to prefer refusal over silent
fallback — but it is *not* a reason the language must be refused, and this ADR does not rest on it. The two
decisions are separable: build the missing disclosure surfaces and the safety objection weakens, while the
Alertmanager differentiator, the build-time guarantee and ADR 0020's dead knob stand unmoved.

The disclosure gap is nonetheless real and worse than a missing metric. There is no
`oto_render_invalid_total` and the code says so (`providers/slack/errors.go:107`,
`render/slack/validate.go:61`, SPEC.md:4713). The substitute the SPEC names does not fire either:
`fail` (`dispatch.go:867`) returns `(outcome{}, nil)` and the render branch (`dispatch.go:589`) returns a
nil error, so `Workers.DeliverDispatch` returns `nil`, the job never reaches the dead-letter, and neither
`oto_jobs_dead_total` (`platform/jobs/metrics.go:52`) nor the dead-letter ERROR log fires — despite
SPEC.md:3574 and :4711 asserting that metric is the alert. `retryDelivery` has no hand-written UI caller.
A refusal needs an HTTP 422; a silent fallback would need all of that built first.

**"Edit your rule annotation" is the documented answer to "we want different words",** and it is written
into the setup docs so the answer is given consistently. `domain-research.md:1576` already recommends
shipping Datadog's five-part body as oto's suggested default annotation template.

**One new term: Stanza** — one named, ordered unit of a rendered message (`title`, `body`, `fields`,
`members`, `trail`, `rule`, `actions`, `footer`). It names a category SPEC.md:3342 already enumerates
without naming. Not "Slot": that is the templating world's word for an outside-filled hole, and a
vocabulary cannot refuse "Template" while adopting Template's own structural noun.

## Refused explicitly, so it is not re-litigated

- User authorship of the top-level `text` or attachment `fallback`. SPEC.md:3228 (S5): it is the push
  notification, the sidebar preview, the search snippet "**and the only thing screen readers read**."
  Independently, `root.go:787-795` records that this is the one place a raw upstream annotation reached
  the top-level text unescaped — `runbook_url: "<!channel>"` put a channel-wide ping in every person's
  push notification.
- Override of `color` or the strikethrough transition (R10 at SPEC.md:43, §H.2, SPEC.md:3364). Per-attribute
  emoji is *not* forbidden by those rules — the nearest governing rule is the existing `ShowFieldEmoji`
  (`ports.go:243`), and any emoji knob belongs there rather than being justified by R10.
- Removal of the rule snapshot, members, trail or overflow links from a `resolved`/`expired` card —
  SPEC.md:3382 binds these, learned from a live run where "the card became least informative at exactly
  the moment it became the only remaining record."
- Any `card` knob on the webhook schema, or templating of its body — its 8 non-`omitempty` envelope fields
  are a contract. The usual citation has a limit worth stating: SPEC.md:3600 ("**The webhook provider MUST
  NOT be given Slack-specific affordances**") forbids leaking *Slack* concepts into the webhook; it does
  not settle whether the webhook or a future channel may have customisation of its own kind. That question
  is open and, §I.1 being silent, must be asked (SPEC.md:5).
- A new `channels.renderer` enum member (SPEC.md:1451), a per-channel custom renderer, BYO Block Kit, raw
  mrkdwn passthrough, or per-org branding. Note this means `Renderer` being a registry-selected port is not
  an extension point here — a seam nobody may enter is named only to say it stays closed.
- The one admitted exception to "no user strings": the `runbook` enricher's org-level `alertname → url`
  pattern (SPEC.md:2450) is a user-authored string oto interpolates. It is defensible as an *Enricher*
  input rather than a renderer input, and because its product is a URL constrained by `safeURL` — but it
  lands in the same top-level-text path as the historic `<!channel>` injection (`root.go:787-795`) and must
  be tested against it.

## Consequences

A customer who wants a Block Kit layout oto does not emit — "add our team's image block" — cannot have
it, and will be told to use the `webhook` provider and render downstream. That is a real loss, and it is
the price of the build-time guarantee.

The bet is that closing the two residue gaps, plus four state-scoped suppression knobs, covers v1 demand.
**What would falsify it:** take the real rule corpus behind ADR 0026 and count customisation demands that
require a block type oto does not emit, or a field order oto does not offer. If that count is materially
non-zero as corpus rows rather than anecdotes, the abstraction has collapsed and a Slack-specific overlay
must be reconsidered — as an ADR, not as a design choice, because SPEC.md:5 binds ("where it is silent,
ask — do not invent") and R5 says if the webhook needs a Slack affordance "the abstraction is wrong and the
SPEC changes first."

**The policy patch ships as an open decision, not a settled one.** Two questions are unanswered: which
schema validates a `card` patch on a table that has no provider context and whose `channel_ids` may name up
to 16 channels of mixed type while the webhook is denied `card` knobs; and whether it is acceptable that,
under first-match-wins, a card override costs a whole duplicated routing rule. The second is the stronger
argument that presentation should not ride the policy table at all.

The `notification_policies` patch layer remains the weakest part of this decision, though less weak than it
first appeared: it follows the single-winning-policy invariant rather than inventing a precedence rule.
The residual cost is real and should be checked against demand before the column is added — coupling
presentation to routing means a customer wanting one card shape across two policies must set it twice.
Nothing in the codebase layers per-channel settings today; `Channel.EffectiveVerbosity()`
(`notification/domain/channel.go:100`) is schema-default normalisation, not a cascade.

Because §I.1 is currently silent on message customisation in both the DEFERRED-POST-V1 table and
§I.1.1 PERMANENTLY OUT, this ADR adds the row, so the next reader does not have to ask.

Whoever edits §L.5.1 should know it is already drifted from the code: SPEC.md:4633-4638 still declares
`mention_on_reminder` and SPEC.md:3192 still cites it, while `providers/slack/config.go:55` states "⛔ THERE
IS NO `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX". ADR 0020's deletion never reached the SPEC.
Fix that in the same commit or the schema edit compounds the drift.
