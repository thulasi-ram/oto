---
title: Notification content customisation
---
**Status:** Proposed · 2026-08-18 · design only, nothing implemented
**Answers:** "templating and override templating for Slack and other channels"
**Companion ADR:** [0037](/adr/0037-typed-card-knobs-over-a-template-language/) (Proposed)

## The short answer

The question was how to design templating, and override templating, for notification content across
channels. The answer this design arrives at is that **oto should not ship a template language, and the
thing customers actually want is not one.**

That is not a refusal to do the work. It is the result of measuring what a customer can already change
without oto's help, and finding that the remainder is a *selection-and-ordering problem over a closed
set of oto's own facts* — which is served by named typed knobs, each with a golden file, and is made
strictly worse by a language.

Two independent lines of evidence converge on this, and one architectural consequence makes it decisive.

## Why not a template language

### It re-imports oto's own best argument for existing

`docs/design/red-team-memo.md:273`:

> The Slack receiver's templating is `text/template` in YAML — painful to maintain and impossible to
> make interactive. … **This is your best "why not just."**

`docs/design/domain-research.md:220`, on Alertmanager's `payload:` escape hatch:

> `webhook_config` supports a `payload:` map of Go templates that **replaces** the entire default body.
> The docs carry an explicit "USE AT YOUR OWN RISK … THE ALERTMANAGER TEAM WILL NOT PROVIDE ANY
> SUPPORT" warning.

A product whose sharpest differentiator is "we are not Alertmanager's templating" cannot ship
Alertmanager's templating and keep the differentiator.

### A customer error would become an oto page

`internal/notification/domain/notification.go:84-86` defines `ClassConfigInvalid` as *"dead. oto sent
something the provider refused; this is an oto bug and raises a banner on the destination."* The matching
health status is blunter — `internal/notification/domain/channel.go:53-55`: *"HealthConfigInvalid means
oto sent a payload the provider refused. This is an oto bug and must be alerted on."* SPEC.md:3574 routes
`invalid_blocks`, `msg_too_long` and `too_many_attachments` to that class and annotates it **"This is an
oto bug."** Tracing it (audited): `slack.Error.ChannelError()` → `ClassConfigInvalid` →
`dispatch.go:589 s.fail(...)` → `ErrorClass.Terminal()` true → `MarkDead`. A user-authored template
makes a customer's typo indistinguishable from an oto defect, sets
`channels.health_status = 'config_invalid'`, and raises a banner blaming oto.

### Build-time validation is the guarantee, and a language voids it

SPEC.md:3218:

> Implemented in `internal/channels/render/slack`. **Every renderer is a pure function with a
> checked-in `testdata/*.golden.json`.** A Block Kit structural validator runs in CI. We never discover
> a broken layout in production.

SPEC.md:4717 makes the mechanism explicit — CI runs `Validate` over every golden file, "so a limit
violation is caught at build time, not in production." A customer-authored template is unreachable by
build-time validation. The guarantee is not weakened; it stops describing what anyone sees.

The hazard is not theoretical, and the codebase has already met it once: `validate.go` documents that
image blocks are whitelisted but never emitted, and that V10's required-field checks were simply
absent — *the first person to render a Grafana panel would have hit a dead delivery.* Golden tests
cover only the paths oto walks.

### The house precedent is a typed enum, and a content knob has already failed here

ADR 0020 met a genuine "who should we mention?" question and answered it with a four-value enum, a
capped opaque-id list and a severity floor — not a mention template. More pointedly, ADR 0020:445-450
records that `channels.config.mention_on_reminder` — a per-channel content knob, schema-validated,
rendered into the settings form, documented — **"was never read… exactly the trap this amendment
exists to avoid, already shipped."** It was deleted.

A content knob on `channels.config` has already been shipped in this codebase, found dead, and removed.
Any new one carries the burden of proof.

## What a customer can already change

Verified: `internal/channels/render/slack/renderer.go:163` — `annotation(v, keys ...string)` reads
`v.Focus.Annotations`, then `v.Alerts[0].Annotations`, then `v.Rule.Annotations`. It has four call sites
across three surfaces: `root.go:82` (`summary` → the title's italic subtitle), `root.go:92`
(`description`, `message` → the body section) with `root.go:96` de-duplicating body against summary, and
`root.go:757` (→ the top-level `text`, i.e. the push notification).

So the rule author **already owns** the title subtitle, the body prose and the push-notification
sentence, and can already template them with Alertmanager's own `{{ $labels }}` / `{{ $value }}`.
Concretely, editing a rule annotation already satisfies: per-alert wording and tone; the affected
resource named in prose; metric value and threshold; impact and probable-cause copy; the runbook URL;
severity phrasing; team and service naming; per-environment differences; and localisation.

Every one of those is fixed in the customer's existing GitOps repo, with zero oto surface, zero new
bound, and zero new failure mode — and it fails loudly and locally in the customer's own CI rather
than as a dead delivery that §H.9 attributes to oto.

**This is oto's actual answer to "we want to customise the message," and it should be written down so
the answer is given consistently.** `domain-research.md:1576` already recommends shipping Datadog's
five-part body as the default annotation template oto suggests to users.

## The residue

What a Prometheus rule annotation provably cannot express, because Prometheus does not know it:

| # | Fact | On the root card? |
|---|---|---|
| 1 | Firing duration over the group, from upstream `startsAt` | Yes (`durationValue` `root.go:674-690`) |
| 2 | Ack state and actor | Yes (`root.go:181`, `:552`) |
| 3 | Occurrence/reopen counts, 24h/7d/30d frequency, flap score | **Partly** — flap transitions render (`root.go:148-150`); the `alert.history` payload renders nowhere |
| 4 | The state trail | Yes (`trailBlock` `root.go:309`) |
| 5 | Rule-change diffs | **Partly** — the root card carries only a link (`root.go:285-287`); the diff itself is a thread reply (`reply.go:240-261`) |
| 6 | Cross-alert group state — member counts, storm count, `N of M` | Yes |
| 7 | Deep links into oto, and the Alertmanager silence link | Yes (`root.go:434`, `:632`) |
| 8 | Snooze state and expiry | **No** — not in the read model at all |
| 9 | `expired` — "oto stopped hearing about this" | Yes |

**Roughly six and a half of nine.** An earlier draft of this document claimed eight of nine; that was
wrong, and it was wrong in the direction that flattered the conclusion. The corrections matter:

- **(8) snooze is a read-model hole, not a knob.** `NotificationView` carries no snooze field;
  `AlertFacts.SnoozedUntil` and `Snapshot.SnoozedAlerts` exist (`notification/domain/snapshot.go:193`,
  `:345`) and `notification/service/view.go` never copies them. Fixing this is a view change, and no
  amount of card configuration substitutes for it.
- **(3) the `alert.history` payload reaches no surface at all.** `alert.history` is never named anywhere
  under `internal/channels/render/slack/`; `enrichmentSummary` (`reply.go:295-308`) renders a count and a
  label derived by `enricherLabel` (`reply.go:312-318`), never the payload. The `verbosity = all` gate is
  real (`ReasonEnriched` is absent from every `replySets` entry, `verbosity.go:49-68`).
- **(5) applies the same standard as (3) and must get the same verdict.** Counting rule-change diffs as
  "served" because a link exists on the root card, while counting `alert.history` as unserved because it
  only appears in a reply, was two opposite standards two rows apart.

So there are **two** genuine gaps, not one, and one of them is a read-model change rather than a
configuration surface.

The conclusion survives the correction, because the residue is still not *content authorship*. It is
which of oto's own facts appear and where — a closed, finite selection over `NotificationView`'s named
fields, into a closed, finite layout: SPEC.md:3342, **"Block budget: 8 base blocks** (title, body,
fields, members, trail, rule, actions, footer). Ceiling is 50." A language buys expressiveness with
nothing to express, because the unbounded space — prose about the alert — is covered upstream.

What the correction does change is the honest ordering of work: **fixing the two gaps is worth more than
any knob**, and should not be bundled with a configuration surface as though it were the same feature.

## The design

Four items. Nothing here evaluates a user-supplied string.

1. **Close the two residue gaps first.** Copy snooze state and expiry into `NotificationView` and render
   them; render the `alert.history` payload. Both are renderer/view changes with golden files, both are
   unambiguously wanted, and neither needs a configuration surface to be worth doing. This is the part
   that improves the product on Monday.
2. **A state-scoped `card` object in §L.5.1's slack schema**, preserving `additionalProperties: false`,
   holding named booleans over facts oto already computes: `show_rule`, `show_members`, `show_history`,
   `show_footer_receiver`. No strings, no expressions.
   **The booleans must be state-scoped, not flat.** SPEC.md:3382 binds the rule snapshot, the members
   summary, the trail and the overflow links onto `resolved`/`expired` cards, so a flat `show_rule: false`
   would silently violate a binding rule. A knob may suppress a Stanza on a *firing* card only; the
   terminal receipt is not configurable. `show_trail` is therefore dropped from the set entirely — the
   trail exists only because `chat.update` is silent and destructive, and it is the last thing a customer
   should be able to remove.
3. **Every knob is read by exactly one code path with a golden file proving it.** ADR 0020:445-450 is the
   standing proof that an unread `channels.config` knob is worse than no knob.
4. **The `runbook` enricher's org-level `alertname → url` pattern** (SPEC.md:2450) is finished as
   specified — but it must be named as what it is: **a user-authored string that oto interpolates.** It
   is the one exception to "no user strings", and it is defensible only because it is an *Enricher* input,
   not a renderer input, and because its product is a URL constrained by `safeURL`. It lands in the same
   top-level-text path as the historic `<!channel>` injection (`root.go:787-795`), so it inherits that
   mitigation and must be tested against it. A design that refuses user strings and then ships one without
   saying so is not being honest about its own boundary.

### What is *not* proposed, and why it was cut

An earlier draft proposed an ordered `field_order` enum pinned to §H.7's list. **That is unsound, and it
is cut.** §H.7 (SPEC.md:3516) is a row of the character-and-item-limits table whose column is *"Renderer
behaviour on overflow"* — it is a **drop order**, not a declared render order. In `root.go` these are the
same dial: `add()` (`root.go:109-117`) silently returns once `len(fields) >= maxFields` (10, `text.go:33`),
so display order *is* shed priority. Three consequences, any one of which is disqualifying:

- The enum cannot name the real field set. A terminal card calls `add()` up to twelve times, and four of
  its labels — `Resolved`/`Last seen`, `Instances affected`, `Notifications`, `Acknowledged`
  (`root.go:139-146`) — appear nowhere in §H.7's eight. A CI assertion pinning the enum to §H.7 would
  pin a list the renderer already exceeds.
- `Firing-for` is not a stable field name: `durationLabel` (`root.go:650-663`) returns *Duration*,
  *Last seen*, *Silenced for* or *Firing for* by state, so one enum token names four fields.
- Fatally: a user order that demotes the SPEC.md:3382-bound terminal fields sheds them, and **which ones
  shed depends on how many earlier fields had non-empty values — i.e. on the alert data.** That makes
  `field_order` a data-dependent knob wearing a structural knob's clothes, and its failure is a silent
  violation of a binding rule.

Field ordering can be revisited, but only after §H.7 is amended to declare a render order distinct from
its shed order, and after the terminal-card field set is enumerated. It is not a v1 knob.

### Where this lands in the existing seams

Typed knobs ride the existing config pipeline for free: `internal/channels/configschema/schema.go` is a
single source of truth whose doc comment reads *"they validate every create/update on the server, they are
served verbatim by GET /api/v1/channel-types, and they render the settings form in the UI. There is no
second copy of these rules anywhere."* A template body cannot ride it — Block Kit's schema is recursive
and union-typed, so an overlay either breaks the schema-renders-the-form invariant or ships a raw JSON
textarea.

Note honestly that `Renderer` being a registry-selected port is **not** an available extension point here:
this design also refuses any new `channels.renderer` enum member (SPEC.md:1451 `channels_rend_ck`). A seam
nobody is permitted to enter is not a seam. It is named only to say it stays closed.

One caution for whoever edits §L.5.1: **that section is already drifted from the code.** SPEC.md:4633-4638
still declares `mention_on_reminder` and SPEC.md:3192 still cites it, while
`internal/channels/providers/slack/config.go:55` says *"⛔ THERE IS NO `mention_on_reminder` HERE, AND ITS
REMOVAL IS A BUG FIX"*. The SPEC has not absorbed ADR 0020's deletion. Fix that in the same commit or the
schema edit compounds the drift.

## Override: where a setting applies

The request had two nouns — *templating* and *override templating* — and they are separable questions.
Everything above answers the first. This section answers the second, because refusing user-authored bytes
does not dispose of "whose default, overridden by whom." A flat object on `channels.config` cannot express
the ordinary ask: *the platform team sets the org's card shape, and the payments team's critical alerts
also show the rule.*

**Two layers, and no new predicate language.**

1. **The Channel's `card` is the default**, per destination, as above.
2. **An optional sparse `card` patch on `notification_policies`.** Policies already carry `Matchers`,
   `Reasons`, `ChannelIDs` and `Priority` (`notification/domain/policy.go:210-225`), indexed for evaluation
   at `policies_eval_idx ON (org_id, priority)` (`db/migrations/00011_notification.sql:134`).

**There is no cascade to resolve, and that is the point.** `policy.go:214-217` states the existing
invariant: *"Priority orders evaluation, **LOWER FIRST. The first policy that matches wins and no other
policy is consulted** — which is why `notifications` carries a single `policy_id` rather than a join
table."* Exactly one policy ever wins a Notification. So the override is a **single overlay, not a chain**:
the winning policy's patch, if it has one, overlays the Channel's `card`. No merge order to specify, no
tie-break rule, no N-layer precedence table — the ambiguity that makes override cascades painful is already
excluded by a decision the codebase made for routing.

The remaining rules are small:

- The patch is **sparse and tri-state per field** — `true` / `false` / absent. "Absent" means inherit, and
  it must stay distinguishable from "explicitly false", or an operator cannot tell a default from a
  decision.
- **Resolution happens where routing happens, not in the renderer.** The renderer stays a pure function of
  `(NotificationView, RenderOptions)`; the resolved card arrives inside `RenderOptions`.
- **Provenance is already half-built.** `notifications.policy_id` records which policy won, so "which rule
  did this?" is answerable today. What must be added is the *resolved* card on the delivery, so "why did my
  card look like that?" needs one row rather than a replay of policy evaluation against config that may
  since have changed. `dispatch.go:583-587` already persists the rendered payload and fallback for exactly
  this reason, citing §L.6 — *"so a dead delivery can be debugged from the row."* The resolved card sits
  beside it.

This reuses ADR 0017's matcher vocabulary rather than inventing a second predicate language, which is the
main reason to prefer it over any bespoke scoping scheme. It adds a column to `notification_policies`,
which is a bound under §N step 4 — DTO validation, domain constructor and DDL `CHECK` move in one commit.

### Two things this layer does not yet answer

These are open, and they are the reason this section ships as a flagged decision rather than a settled
design.

**Where does the patch get validated?** `card` is defined inside §L.5.1's *slack provider* schema, and the
whole "refused at write time with a 422 and a JSON pointer" claim rests on that per-provider pipeline. But
the patch is proposed as a column on `notification_policies`, a table with no provider context, whose
`channel_ids` may name up to 16 channels of mixed type (`policies_chan_ck`) — while the webhook is
explicitly denied `card` knobs. So: which schema validates a policy patch, what does the JSON pointer point
at, and what happens when one policy fans out to a Slack channel and a webhook? Three candidate answers,
none yet chosen: validate the patch against the union of provider schemas for the channels it names (breaks
when channels are added later); validate against the slack schema and ignore the patch for non-slack
channels (silent no-op — the ADR 0020 trap); or refuse a `card` patch on any policy naming a non-slack
channel (loud, restrictive, and probably right).

**A card override costs a routing fork.** Because the first matching policy wins and no other is consulted,
an operator cannot override only the card — they must create a *higher-priority policy* that also
re-declares `reasons`, `channel_ids` and `throttle`, duplicating the routing rule they wanted to leave
alone. The motivating example above therefore costs more than it appears to. This is the strongest argument
that presentation should *not* ride the policy table, and it should be weighed before the column is added.

Whoever does add it will also have to argue past the binding block guarding this table
(`policy.go` header, `db/migrations/00019_unacked_reminder.sql:15-19`).

**The honest weakness underneath both:** nothing in the codebase layers per-channel settings today.
`Channel.EffectiveVerbosity()` (`notification/domain/channel.go:100`) is schema-default normalisation, not
a cascade. Riding the single-winning-policy invariant is what keeps this from being an invention — but it
couples presentation to routing, and the two questions above are where that coupling bites.

## The failure model, and why it is a separate decision

The hardest question in this space is what happens when a customer setting produces an invalid card.
Under a language it splits in two, by authorship of the *failing byte* — not of the config:

- **Structural** (V0 shape, V1 attachment count, V4 block whitelist, V9 `plain_text`, V12 `action_id`
  namespace, V15 unfurl, V16 duplicate `block_id`) — decidable with zero alert data. Refuse at write
  time: 422 with a JSON pointer, through layer 4's existing `jsonschema/v6` pipeline.
- **Data-dependent** (V18 payload bytes, V5/V6/V14 length and emptiness, V10, V17) — not knowable at
  save time, because the data's length is unknown then. These route through the *existing*
  `truncateSection`/`truncateField`/`truncateAt` helpers, which already mark the cut with
  "… see full detail in oto", making every length check unreachable by construction.

**Under typed knobs the second class is very nearly empty** — every boolean is structural and therefore
decidable at write time, so a customer setting cannot produce a `dead` delivery. An earlier draft claimed
the class was *exactly* empty and that this made the scope decision and the safety decision "the same
decision." Both claims were too strong. The `field_order` proposal was itself data-dependent, which is why
it is cut above; and the two decisions are **separable** — if someone built the missing disclosure
surfaces, the safety objection to a language would weaken while every other objection
(the Alertmanager differentiator, the build-time guarantee, ADR 0020's dead knob) would stand entirely
unmoved. The anti-language case does not need the safety leg. It is a reason to prefer refusal-at-write
over silent fallback, not a reason to refuse a language.

Stated at its correct strength: the asymmetry is one of cost. *Refusal at write time* needs an HTTP 422.
*Silent fallback* needs disclosure surfaces that do not exist — and the gap is worse than a missing metric.

Audited, and this is a finding in its own right: **a render-failure death is currently silent end to end.**
There is no `oto_render_invalid_total`, and the code says so in its own voice
(`providers/slack/errors.go:107`, `render/slack/validate.go:61`, SPEC.md:4713 — *"There is **no**
`oto_render_invalid_total{check}` counter"*). Worse, the substitute the SPEC names does not fire either:
`DispatchService.fail` (`dispatch.go:867`) takes the `class.Exhausted` branch, calls `MarkDead` and
returns `(outcome{}, nil)`, and the render branch at `dispatch.go:589` returns a nil error, so
`Workers.DeliverDispatch` (`notification/worker/worker.go:159-162`) returns `nil`. The job never reaches
the dead-letter, so neither `oto_jobs_dead_total` (`platform/jobs/metrics.go:52`) nor the dead-letter
ERROR log (`platform/jobs/deadletter.go:82`) fires — even though SPEC.md:3574 and SPEC.md:4711 assert
that metric *is* the alert for this case. And `retryDelivery` exists server-side (`api/router.go:108`,
`requeueDeliverySQL` at `repository/config.go:600`) with **no hand-written caller in the UI**.

What *does* exist is one surface worth crediting, in the same function: `dispatch.go:583-587` persists the
rendered payload and fallback alongside the failure, commented against §L.6 *"so a dead delivery can be
debugged from the row."* A dead delivery is therefore diagnosable if you already know to look — and
invisible if you do not.

## What is refused, and stays refused

- No template evaluation of user-supplied strings anywhere in the render path — no `text/template`, no
  `{{ }}`, no mini-language, no CEL (ADR 0017 is the standing refusal of an expression language).
- No user authorship of the top-level `text` or the attachment `fallback`. SPEC.md:3228 (S5): it is
  *"the push notification, the sidebar preview, the search snippet, **and the only thing screen readers
  read**."* Independently: `root.go:787-795` documents that this is the one place a raw upstream
  annotation reached the top-level text **unescaped**, so `runbook_url: "<!channel>"` put a channel-wide
  ping in every person's push notification. The mitigation is `safeURL`, not `escape` — and title,
  summary, severity and team interpolated into `rootText` are *not* escaped, relying on block-level
  escaping instead. This surface is not offered to users.
- No override of `color` or the strikethrough transition (R10 at SPEC.md:43, §H.2, SPEC.md:3364). Colour
  encodes state, exclusively. (Per-attribute emoji is a separate matter: R10 and §H.2 govern *colour* and
  do not forbid emoji. The nearest governing rule is the existing per-channel `ShowFieldEmoji`
  (`ports.go:243`), and any emoji knob belongs there rather than being justified by R10.)
- No removal of the rule snapshot, members summary, trail or overflow links from a `resolved`/`expired`
  card. SPEC.md:3382 binds items 1, 2, 4, 5 — learned from a live run where *"the card became least
  informative at exactly the moment it became the only remaining record."* This is why the `card` booleans
  above are state-scoped and why `show_trail` does not exist.
- No `card` knobs on the webhook schema and no templating of its body. Its 8 non-`omitempty` envelope
  fields are a stable contract, so omitting a declared field is a schema break rather than a rendering.
  Note the limit of the usual citation: SPEC.md:3600 — *"The webhook provider MUST NOT be given
  Slack-specific affordances. If it needs one, the abstraction is wrong and the SPEC changes first"* —
  forbids leaking *Slack* concepts into the webhook. It does **not** settle whether the webhook, or a
  future channel, may have customisation of its own kind. That question is open, and §I.1's silence means
  it must be asked rather than assumed (SPEC.md:5).
- No new member of the `channels.renderer` enum (SPEC.md:1451 `channels_rend_ck`), no per-channel custom
  renderer, no BYO Block Kit, no raw mrkdwn passthrough, no per-org branding.

## Vocabulary

One new term. It names something that **already exists** and currently has no collective noun:
SPEC.md:3342 enumerates the eight base blocks but never names the category.

> **Stanza** — One named, ordered unit of a rendered message: `title`, `body`, `fields`, `members`,
> `trail`, `rule`, `actions`, `footer`. The renderer always has one. Configuration may suppress a
> Stanza or reorder what is inside it; it never authors one and never invents one.
>
> *Most confused with:* **Block**. A `block` is Slack Block Kit's own type name, so it cannot name the
> same unit on the webhook renderer, where a Stanza is one JSON string with no visual identity.

Three candidate terms were considered and killed:

- **Slot** → Stanza. "Slot" is the templating world's word for an outside-filled hole (Vue, Web
  Components, Jinja). A vocabulary cannot refuse "Template" and then adopt Template's own structural
  noun; and it frames the always-present Go body as an unfilled error path.
- **Wording** (a typed, matcher-guarded content claim) → deleted. With no user-authored strings it has
  nothing left to name.
- **Stance** (`Density`, `Prominence`, `Facets`, `MinConfidence`) → deleted, four times over.
  `RenderOptions` already *is* the channel-agnostic posture carrier, so this was a rename, not a term.
  `Facets` collides with an existing meaning — a damping dimension of a bucket
  (`internal/alerts/domain/contracts.go:527`, `repository/alert.go:155` and `:825`,
  `web/src/api/generated/schema.d.ts:141`).
  `Prominence` is inert or illegal, because R10/§H.1 C7 reserve saturated colour exclusively for state.
  `MinConfidence` contradicts SPEC.md:1347, which binds that `match_confidence='ambiguous'` **"MUST be
  surfaced in the UI and in Slack, never hidden."** And "stance" reads as an organisational opinion,
  which FR-1 puts permanently out of scope for a product that records rather than narrates.

Also clarified, no change in behaviour: **Verbosity** is the per-Channel gate on *which* Deliveries
exist at all. It never governs how much one Delivery says. There is no `Density`; "how much this
message says" is expressed by which Stanzas a Channel shows.

## Findings outside this feature's scope

Surfaced while auditing. None is caused by this design; all are real.

1. **`RenderOptions.Verbosity` is dead.** Declared `ports.go:242`, populated `dispatch.go:574`, and no
   non-test file under `internal/channels/render/` reads it — the only three readers are test fixtures.
   Verbosity is enforced upstream in `PlanFor`. Deleting the field is the cleanest proof that "which
   deliveries exist" and "how much each says" are different axes.
2. **The AC-49 vocabulary gate is currently RED on `main`.** `go run ./tools/lintvocab` exits non-zero with
   **56 violations: 54 `assignee`, all in `web/src/features/linear-proto/`** (a Linear-clone prototype,
   which naturally models assignees), **and 2 `on-call`** outside it —
   `web/src/features/alerts/previewFixtures.ts:464` and
   `web/src/features/notifications/PoliciesSection.test.tsx:293`. This is exactly the "a word arrives, then
   the concept, then the column, then the rota" drift the gate's own header says it exists to stop. Either
   the prototype is excluded by path or its identifiers are renamed; a gate that is red is a gate that is
   off. The two `on-call` hits are the more interesting ones, being in real product code.
3. **`tools/lintvocab` omits a stem AC-49 explicitly requires.** AC-49 (`SCOPE-BOUNDARY.md:229`) specifies
   `grep -riE '(assign|assignee|on.?call|rota|escalation policy|postmortem|incident)'`. `lintvocab`'s
   `banned` list (12 stems, ending at MTTR) has no rule for bare `incident` — only the `incident_id`
   column. Conversely, CONTEXT.md overstates coverage in the other direction: it asserts at :100-101 that
   "AC-49 greps for them in CI" over a ban list spanning :103-105, but `schedule` (:103), `severity
   override`, `close`, `merge`, `dismiss` and `watcher`/`subscriber` (:105) were never in AC-49 and are
   enforced nowhere. Both halves should be reconciled — the prose or the linter.
4. **`EnrichmentView` carries no enricher version.** It is
   `{Enricher, Status, Payload, Warnings, Error, ComputedAt}` (`view.go:137-144`), but CONTEXT.md:93
   defines Enrichment as a result from a *"named, **versioned** Enricher."* The glossary claims a
   provenance field the read model does not carry.
5. **The `alert.history` payload reaches no rendered surface.** `alert.history` is never named under
   `internal/channels/render/slack/`; the `enriched` reply renders a count and a derived label only
   (`reply.go:295-308`, `:312-318`). Residue item (3), and the highest-value deliverable here.
6. **Snooze is computed and then dropped.** `AlertFacts.SnoozedUntil` and `Snapshot.SnoozedAlerts` exist
   (`notification/domain/snapshot.go:193`, `:345`); `notification/service/view.go` never copies them into
   `NotificationView`, so no renderer can show snooze state. Residue item (8).
7. **A render-failure death is silent end to end.** No `oto_render_invalid_total` (by the code's own
   admission), and `oto_jobs_dead_total` does not fire either because the render branch returns a nil
   error and the job never reaches the dead-letter — despite SPEC.md:3574 and :4711 asserting that metric
   is the alert. See the failure-model section for the trace.
8. **The UI has no dead-delivery retry.** `POST /api/v1/deliveries/{id}/retry` exists server-side;
   `retryDelivery` appears only in generated `schema.d.ts` with no hand-written caller, and the dead-letter
   screen is an index comment (`del_dead_idx`) with no route.
9. **§L.5.1 is drifted from the code.** SPEC.md:4633-4638 still declares `mention_on_reminder` and
   SPEC.md:3192 still cites it, while `providers/slack/config.go:55` says *"⛔ THERE IS NO
   `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX"*. ADR 0020's deletion never reached the SPEC.
10. **`validate.go` has 19 identifiers, not 18.** V0–V18; its own doc comment (`validate.go:91`) says "the
    eighteen outbound checks of §L.6" because V0 is an extra JSON-decode guard outside the numbered set.
    V14, V17 and V18 are not Block Kit checks at all — they are top-level text, metadata size and payload
    size.
