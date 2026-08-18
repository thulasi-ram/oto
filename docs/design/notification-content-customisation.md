# Notification content customisation

**Status:** Proposed · 2026-08-18 · design only, nothing implemented
**Answers:** "templating and override templating for Slack and other channels"
**Companion ADR:** [0037](../adr/0037-wordings-are-liquid-and-structure-stays-otos.md) (Proposed)

## The short answer

Customers get **Wordings**: a Liquid template that produces the text of one **Stanza** of a notification.
Structure stays oto's — Go builds every block, owns the attachment, colour and emoji, and validates the
result. A Wording chooses words; it cannot choose structure, colour, mentions, links, or destinations.

The engine is `github.com/osteele/liquid` v1.9.2 (MIT) on `NewBasicEngine()`, which ships **no tags and no
filters** — so there is no branching, no iteration, and only the filters oto registers by name.

## The gap this closes

Three of the card's prose surfaces already come from the alert's own annotations. `annotation(v, keys...)`
(`render/slack/renderer.go:163`) has four call sites across three surfaces: the title subtitle
(`root.go:82`), the body (`root.go:92`, de-duplicated at `:96`) and the top-level text (`root.go:757`). Those
annotations are already templatable with Alertmanager's own `text/template`, in the customer's GitOps repo,
failing in the customer's CI. Per-alert prose, tone, metric values, impact copy, runbook URL, team naming,
per-environment variants and localisation are all already reachable that way, and that remains the right
answer for all of them.

What nobody can do is put **oto's own facts into a sentence.**

> "Firing 20 minutes, 4th time this week, still unacked."

Prometheus does not know any of that, so the rule author cannot write it. And oto has no authoring surface,
so the operator cannot either. Today only Go can write that sentence. That is the gap, and it is the whole
justification for this feature.

An earlier revision of this document refused all user-authored strings and offered show/hide booleans
instead. That was wrong twice over. It measured the residue as *"is this fact on the card?"* when the real
question is *"can anyone put this fact into a sentence?"*; and a boolean chooses whether a fact appears, never
how it reads. The residue argument holds for **layout**. It was over-applied to **wording**, which is a
different thing.

## The residue

What a Prometheus rule annotation cannot express, because Prometheus does not know it:

| # | Fact | On the root card? |
|---|---|---|
| 1 | Firing duration over the group, from upstream `startsAt` | Yes (`durationValue` `root.go:674-690`) |
| 2 | Ack state and actor | Yes (`root.go:181`, `:552`) |
| 3 | Occurrence/reopen counts, frequency, flap score | **Partly** — flap transitions render (`root.go:148-150`); the `alert.history` payload renders nowhere |
| 4 | The state trail | Yes (`trailBlock` `root.go:309`) |
| 5 | Rule-change diffs | **Partly** — the root card carries only a link (`root.go:285-287`); the diff is a thread reply (`reply.go:240-261`) |
| 6 | Cross-alert group state — member counts, storm count, `N of M` | Yes |
| 7 | Deep links into oto, and the Alertmanager silence link | Yes (`root.go:434`, `:632`) |
| 8 | Snooze state and expiry | **No** — not in the read model at all |
| 9 | `expired` — "oto stopped hearing about this" | Yes |

Two genuine gaps, and both are plumbing rather than authoring:

- **(8) snooze is a read-model hole.** `NotificationView` carries no snooze field; `AlertFacts.SnoozedUntil`
  and `Snapshot.SnoozedAlerts` exist (`notification/domain/snapshot.go:193`, `:345`) and
  `notification/service/view.go` never copies them. No Wording can reference a field that is not there.
- **(3) the `alert.history` payload reaches no surface.** `alert.history` is never named under
  `internal/channels/render/slack/`; `enrichmentSummary` (`reply.go:295-308`) renders a count and a label from
  `enricherLabel` (`reply.go:312-318`), never the payload.

**Close both before shipping Wordings.** A formatting surface over absent facts is a worse product than no
formatting surface, and these are the facts customers most want to talk about.

## The design

A **Wording** is a row: a `stanza`, a `when` clause, and a Liquid `template`.

```
Wording {
  Stanza    StanzaID          // title | body | fields | members | trail | rule | actions | footer
  When      { Matchers []Matcher; Reasons []Reason }   // ADR 0017 vocabulary, verbatim
  Template  string            // Liquid, one line of prose
  Priority  int
}
```

`when` reuses `Matcher{Name,Op,Value}` and the closed `Reason` enum unchanged, so no second predicate
language is introduced. **Conditionality is two Wordings with different matchers, never a branch inside one**
— not for safety, but because a matcher lets the UI show *which Wording won* and a buried `{% if %}` cannot.

### The engine, configured

```go
// Delivery-time: lax, so a missing field degrades one Stanza, never a delivery.
render := liquid.NewBasicEngine()          // no tags, no filters, no control flow
registerFilters(render)                     // exactly oto's curated set, by name

// Save-time: strict, so a typo is refused while a human is present to be told.
validate := liquid.NewBasicEngine()
registerFilters(validate)
validate.StrictVariables()
```

The curated filter set already exists as `text.go`'s golden-tested helpers — `human_duration`, `slack_date`,
`code`, `strike`, `truncate_runes` — plus `default`, which is load-bearing for totality and which
`NewBasicEngine()` does not ship.

### Bindings

A Wording is given a **flat `map[string]any` of scalars and pre-formatted strings**: a purpose-built
`StanzaInput` projection, never `*NotificationView` and never a domain struct. This is not a precaution —
Liquid reflects into Go structs, and a spike passing one into bindings printed its field via `{{ s.Token }}`.
Making bindings a flat map turns "what can a Wording reach" into a struct definition rather than a
reflection question.

### Validation

Saving a Wording **parses strictly and then renders** against a fixture corpus, including the hostile cases
(empty labels, oversized annotation, nil enrichment, terminal card). Rendering is required, not optional: an
unknown filter is a render-time error in Liquid, not a parse-time one. A failure is an HTTP 422 quoting the
offending expression.

## The safety property

> **A Wording can never mark a delivery `dead`, and can never emit a mention or a link.**

This is provable rather than hoped-for, because `validate.go`'s 19 identifiers (V0–V18) partition cleanly:

- **Structure Go owns** — attachment count, block whitelist, `block_id` uniqueness, `action_id` namespace,
  `plain_text` usage, unfurl flags. Unreachable, because a Wording emits text and Go builds every block.
- **Length and emptiness** — section text, field text, top-level text, metadata size, payload size. Bounded
  by the sink: `escape()` then `truncateSection`/`truncateField` against a declared per-Stanza rune budget.

**The gate artifact is an exhaustiveness test over those 19 identifiers**, each classified user-unreachable or
budget-bounded, failing to compile when a new check appears unclassified. If a check lands in neither bucket,
the feature does not ship.

Totality closes emptiness: every field reference carries a `default`, so a Stanza can never render empty and
no zero-information rule is violated by absence. At delivery, if a Wording errors anyway, that Stanza falls
back to its built-in Go value.

**This is not a new safety system.** `root.go:100` is already `truncateSection(escape(body), v.Links.Group)` —
upstream annotation text flows through this exact pair today, and a Wording is the same trust class. The
helper's own comment is the doctrine:

> *"A card that was silently truncated tells an operator a smaller truth than the one that exists, and they
> have no way to know. A card that says '…' and offers a link tells them exactly what happened."*

## Across channels

A Wording is text, and **text is portable where layout is not.** Slack receives it as the mrkdwn emphasis
subset after escaping; the webhook receives it as a string in a `rendered` map keyed by Stanza, with markers
stripped, because the webhook is not a degraded Slack. A future email channel receives plain text. This is the
part of the cross-channel problem that a posture-or-layout abstraction could not solve.

## Refused

- **No structure** — no block authoring, no BYO Block Kit, no raw mrkdwn passthrough, no new
  `channels.renderer` member (SPEC.md:1451), no per-channel custom renderer, no per-org branding.
- **No mentions** — the sink strips `<!channel>`, `<!here>`, `<@U…>`, `<!subteam^…>` from interpolated values
  *and* from literals. Mention audience stays with `MentionPolicy`.
- **No user-authored URLs** — `link()` escapes the label but not the url, which is exactly how
  `runbook_url: "<!channel>"` put a channel-wide ping in every push notification (`root.go:787-795`).
- **No Wording on the top-level `text` or attachment `fallback`** — SPEC.md:3228 (S5): the push notification,
  the sidebar preview, the search snippet "and the only thing screen readers read", and the one surface only
  partially escaped by design.
- **No override of `color` or the strikethrough transition** (R10 at SPEC.md:43, §H.2, SPEC.md:3364). Colour
  encodes state exclusively. Per-attribute emoji is not governed by those rules — `ShowFieldEmoji`
  (`ports.go:243`) is the nearest rule and any emoji knob belongs there.
- **No suppression of the terminal receipt** — SPEC.md:3382 binds the rule snapshot, members, trail and
  overflow links onto `resolved`/`expired` cards, learned from a live run where "the card became least
  informative at exactly the moment it became the only remaining record."
- **No enumeration** — without `{% for %}` a Wording cannot iterate members. "N of M instances" stays Go's.
  A real ceiling, accepted deliberately.

Above the ceiling the answer is an **Enricher**: the customer owns the computation, it is provenanced,
testable and rate-limited, and the Wording only places its result. For a Block Kit layout oto does not emit,
the answer is the `webhook` provider and render downstream.

## Override: where a setting applies

The request had two nouns, and they are separable. Everything above answers *templating*. This answers
*override templating*, because refusing structure does not dispose of "whose default, overridden by whom."

**Two layers, and no cascade to resolve.**

1. A Channel's Wordings are the default, per destination.
2. An optional Wording set on `notification_policies`, which already carries `Matchers`, `Reasons`,
   `ChannelIDs` and `Priority` (`notification/domain/policy.go:210-225`), indexed at
   `policies_eval_idx ON (org_id, priority)` (`db/migrations/00011_notification.sql:134`).

There is no precedence table to invent, because `policy.go:214-217` already binds it: *"Priority orders
evaluation, **LOWER FIRST. The first policy that matches wins and no other policy is consulted** — which is
why `notifications` carries a single `policy_id` rather than a join table."* Exactly one policy wins, so
exactly one override applies. The ambiguity that makes override cascades painful is already excluded by a
decision made for routing.

Resolution happens where routing happens; the renderer stays a pure function of
`(NotificationView, RenderOptions)`. `notifications.policy_id` already records which policy won, so what must
be added is the **resolved Wording set on the delivery row** — "why did my card read like that" should need
one row, not a replay against config that may since have changed. `dispatch.go:583-587` already persists the
rendered payload for exactly this reason, citing §L.6.

### Two things this layer does not answer

**Where does the patch get validated?** A Wording set is provider-shaped (the mrkdwn subset is Slack's), but
`notification_policies` has no provider context and its `channel_ids` may name up to 16 channels of mixed
type. Three candidate answers, none chosen: validate against the union of provider schemas for the channels
named (breaks when channels are added later); validate against Slack and ignore for others (a silent no-op —
the ADR 0020 trap); or refuse a Wording patch on any policy naming a non-Slack channel (loud, restrictive,
probably right).

**An override costs a routing fork.** Because the first matching policy wins, an operator cannot override
only the wording — they must create a higher-priority policy that also re-declares `reasons`, `channel_ids`
and `throttle`, duplicating the routing rule they wanted to leave alone. This is the strongest argument that
presentation should not ride the policy table at all, and it should be weighed before the column is added.
Whoever adds it must also argue past the binding block guarding this table (`policy.go` header,
`db/migrations/00019_unacked_reminder.sql:15-19`).

## Vocabulary

Two new terms.

> **Stanza** — One named, ordered unit of a rendered message: `title`, `body`, `fields`, `members`, `trail`,
> `rule`, `actions`, `footer`. The renderer always has one. Configuration may substitute a Stanza's text; it
> never authors a Stanza's structure and never invents one.
>
> *Most confused with:* **Block**. A `block` is Slack Block Kit's own type name, so it cannot name the same
> unit on the webhook renderer, where a Stanza is one JSON string with no visual identity.

> **Wording** — One typed, matcher-guarded Liquid template that produces the text of one Stanza. It chooses
> words; it cannot choose structure, order, colour, mentions, links, or destination.
>
> *Most confused with:* **Template**. A Wording *is* a template, but the noun names the bounded product
> rather than the mechanism — one line of prose for one Stanza, with no branching and no iteration. Calling
> it a "template" invites the document-shaped expectations (partials, inheritance, layout) that this design
> refuses.

The Stanza set is not an invention: SPEC.md:3342 already enumerates it — **"Block budget: 8 base blocks**
(title, body, fields, members, trail, rule, actions, footer). Ceiling is 50" — and simply has no collective
noun for it.

Three candidates were considered and killed. **Slot** → Stanza: "slot" is the templating world's word for an
outside-filled hole, and the Go renderer always has a body. **Stance** (`Density`, `Prominence`, `Facets`,
`MinConfidence`) → nothing: `RenderOptions` already *is* the channel-agnostic posture carrier; `Facets`
collides with an existing meaning (a damping dimension of a bucket — `alerts/domain/contracts.go:527`,
`repository/alert.go:155` and `:825`, `web/src/api/generated/schema.d.ts:141`); `Prominence` is inert or
illegal because R10 reserves colour exclusively for state; and `MinConfidence` contradicts SPEC.md:1347,
which binds that `match_confidence='ambiguous'` **"MUST be surfaced in the UI and in Slack, never hidden."**
**Density** → the existing `Verbosity`, which is the per-Channel gate on *which* Deliveries exist and never
governs how much one says.

## Spike evidence

Verified against `github.com/osteele/liquid` v1.9.2 in a scratch module. Transitive deps: `golang.org/x/sync`,
`golang.org/x/tools`, `gopkg.in/yaml.v2`.

| Question | Result |
|---|---|
| Licence | MIT — compatible |
| `NewBasicEngine()` tag surface | No `if`/`for`/`assign`/`case`/`unless`/`include`/`capture` |
| `NewBasicEngine()` filter surface | **Zero filters.** All 25 probed absent — oto registers its own by name |
| `UnregisterTag` for control flow | **Does not work** on block tags; no `UnregisterBlock`. `NewBasicEngine()` is the mechanism |
| Map iteration determinism | **Non-deterministic** — two orders on consecutive renders. Unreachable without `{% for %}` |
| `StrictVariables()` on a typo | `undefined variable in {{ srvice }}` |
| Default engine on a typo | Renders empty — never kills the card |
| Unknown filter | **Render-time** error, not parse-time → save-time validation must execute |
| Go struct in bindings | `{{ s.Token }}` renders the field — bindings must be a flat scalar map |
| Alertmanager idioms | `{{ $labels.job }}`, `{{ .Service }}`, `{{ humanize $value }}` are all **syntax errors** |
| Error message quality | Quotes the offending expression; **no line or column** |
| `"" \| default:` | Empty string is falsy — matches the totality rule |
| AST access | `Template.GetRoot()` exposes it — the declared read set is buildable |

## Findings outside this feature's scope

Surfaced while auditing. None is caused by this design; all are real.

1. **`RenderOptions.Verbosity` is dead.** Declared `ports.go:242`, populated `dispatch.go:574`, and no
   non-test file under `internal/channels/render/` reads it — the three readers are test fixtures. Verbosity
   is enforced upstream in `PlanFor`.
2. **The AC-49 vocabulary gate is RED on `main`.** `go run ./tools/lintvocab` exits non-zero with 56
   violations: 54 `assignee`, all in `web/src/features/linear-proto/` (a routed design study that deliberately
   models Linear's domain), and 2 `on-call` in real product code —
   `web/src/features/alerts/previewFixtures.ts:464` and
   `web/src/features/notifications/PoliciesSection.test.tsx:293`. Prototype paths should be excluded by path
   or marker; a gate that is red is a gate that is off.
3. **`tools/lintvocab` omits a stem AC-49 requires.** AC-49 (`SCOPE-BOUNDARY.md:229`) specifies
   `grep -riE '(assign|assignee|on.?call|rota|escalation policy|postmortem|incident)'`, and there is no rule
   for bare `incident` — only the `incident_id` column. CONTEXT.md overstates coverage the other way: it
   asserts at :100-101 that "AC-49 greps for them in CI" over a list spanning :103-105, but `schedule` (:103),
   `severity override`, `close`, `merge`, `dismiss` and `watcher`/`subscriber` (:105) are enforced nowhere.
4. **`EnrichmentView` carries no enricher version.** It is
   `{Enricher, Status, Payload, Warnings, Error, ComputedAt}` (`view.go:137-144`), but CONTEXT.md:93 defines
   Enrichment as a result from a *"named, **versioned** Enricher."*
5. **The `alert.history` payload reaches no rendered surface.** Residue item (3).
6. **Snooze is computed then dropped.** Residue item (8).
7. **A render-failure death is silent end to end.** No `oto_render_invalid_total`, by the code's own
   admission (`providers/slack/errors.go:107`, `render/slack/validate.go:61`, SPEC.md:4713). The substitute the
   SPEC names does not fire either: `fail` (`dispatch.go:867`) returns `(outcome{}, nil)` and the render branch
   (`dispatch.go:589`) returns a nil error, so `Workers.DeliverDispatch` (`worker.go:159-162`) returns `nil`,
   the job never reaches the dead-letter, and neither `oto_jobs_dead_total` (`platform/jobs/metrics.go:52`)
   nor `deadletter.go:82` fires — despite SPEC.md:3574 and :4711 asserting that metric is the alert. **Fix
   this before shipping Wordings**, not because a Wording can cause a death (it cannot) but because an oto
   render bug is currently invisible in production.
8. **The UI has no dead-delivery retry.** `POST /api/v1/deliveries/{id}/retry` exists server-side;
   `retryDelivery` appears only in generated `schema.d.ts` with no hand-written caller, and the dead-letter
   screen is an index comment (`del_dead_idx`) with no route.
9. **§L.5.1 is drifted from the code.** SPEC.md:4633-4638 still declares `mention_on_reminder` and
   SPEC.md:3192 still cites it, while `providers/slack/config.go:55` says *"⛔ THERE IS NO
   `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX"*. ADR 0020's deletion never reached the SPEC.
10. **`validate.go` has 19 identifiers, not 18.** V0–V18; its doc comment (`validate.go:91`) says "the
    eighteen outbound checks of §L.6" because V0 is an extra JSON-decode guard. V14, V17 and V18 are not
    Block Kit checks — they are top-level text, metadata size and payload size.

## Sequencing

Pre-release, the division that matters is not risky-vs-safe but **cheap now, expensive forever.**

1. **Doc-only work, free exactly once.** Amend §H.7 to declare a render order distinct from its shed order and
   enumerate the twelve terminal fields (this is what unblocks field ordering later); delete
   `mention_on_reminder` from §L.5.1; make the metrics table stop promising counters that do not exist; write
   `Stanza` and `Wording` into CONTEXT.md before they appear in schema property names.
2. **Make the pipeline honest, and close the read-model gaps.** Finding 7's render counter and death log,
   finding 8's retry wiring, snooze into `NotificationView`, and the `alert.history` payload rendered.
3. **The safety floor.** `StanzaInput`, the curated filter registry, the strict/lax engine pair, the sink
   wiring, and the 19-check exhaustiveness test. Nothing user-facing ships in this step.
4. **Wordings across all eight Stanzas**, plus the preview endpoint replaying against a stored
   `NotificationView`. Stanza-by-stanza rollout is a released-product technique and is not needed here.
5. **The policy-scoped override last**, once its two open questions are answered.
