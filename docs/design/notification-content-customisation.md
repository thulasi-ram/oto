# Notification content customisation

**Status:** Proposed · 2026-08-18 · **revised 2026-08-22** — see the revision block below
**Answers:** "templating and override templating for Slack and other channels"
**Companion ADRs:** [0037](../adr/0037-wordings-are-liquid-and-structure-stays-otos.md) (Proposed) ·
[0048](../adr/0048-a-wording-is-spelled-by-its-channel.md) (Accepted — corrects 0037 on markers,
filter naming and audience refusal)

---

## ⚠️ REVISION BLOCK — 2026-08-22: what this document got wrong

**This note was written before the code was read line by line, and the verification pass that preceded
step 1 refuted nine of its claims.** They are corrected here rather than silently edited into the body,
because a design note whose errors are invisible teaches the next reader to trust the rest of it more
than it deserves. Every line below was checked against the tree at commit `0ea5aa1`.

**Facts that changed under it, or were never true:**

1. **⛔ Residue item 8 / finding 6 — "snooze is a read-model hole" — is FIXED and this note should stop
   citing it.** Commit `77886a8` landed it. `NotificationView.SnoozedUntil` exists at
   `internal/channels/domain/view.go:90`, is populated by `internal/notification/service/view.go:259`,
   and renders at `internal/channels/render/slack/root.go:146-147` as §B.8.6's single added
   `*Notifications*` field. **Strike the residue row and finding 6.** The gate "close both before
   shipping Wordings" is half satisfied by work already merged.
2. **Residue item 3 — the `alert.history` payload reaching no surface — was still TRUE when written**,
   and is now closed by the wording package, which exposes enrichment payload scalars **by name** in
   `StanzaInput` (`internal/channels/render/wording/input.go`; `TestEnrichmentPayloadIsReachable`). A
   Wording can put an enricher's own number into a sentence, which is the thing nobody could do.
3. **The file path given for `EnrichmentView` is wrong.** It is
   `internal/channels/domain/view.go:233-240`, **not** `notification/service/view.go:137-144`. Finding
   4's substance — that it carries no enricher version, while CONTEXT.md defines an Enrichment as a
   result from a *"named, **versioned** Enricher"* — survives the corrected citation.
4. **`root.go:100` is a COMMENT, not the sink call.** Both this note and ADR 0037 quote it as
   *"literally `truncateSection(escape(body), v.Links.Group)`"*. That statement is at **`root.go:96`**;
   line 100 reads `// lowest-priority fields are dropped first when the ten-item budget runs out.` The
   argument is unharmed — the pair is real and does carry upstream annotation text today — but the
   citation would have sent a reader to the wrong thing.
5. **`annotation()` is at `renderer.go:178-197`**, not `renderer.go:163` and not `text.go:163`.
6. **The per-Stanza budget is in BYTES, not runes.** This note says "a declared per-Stanza rune budget".
   `truncateAt` cuts so the result *including its suffix* is never longer than `hard` **bytes**
   (`render/slack/text.go`), and `maxSectionText`/`maxFieldText` are byte counts. The distinction is not
   pedantic: a Stanza of CJK or emoji prose hits the sink at roughly a third of the character count an
   author counted. Only the `truncate_runes` **filter** counts runes, deliberately, because an author
   asking for forty characters means forty characters — **both ceilings apply, and they are different
   units.**
7. **The spike's transitive-dependency list is wrong.** It says `golang.org/x/sync`,
   `golang.org/x/tools`, `gopkg.in/yaml.v2`. Actually, against the real module: **added**
   `github.com/osteele/tuesday v1.1.1` (go.mod:35) and `github.com/dlclark/regexp2 v1.11.5` (go.sum,
   pulled transitively); and `go get` **upgraded two existing dependencies** —
   `github.com/go-viper/mapstructure/v2` v2.4.0 → v2.5.0 and `gopkg.in/yaml.v2` v2.2.3 → v2.4.0. This is
   recorded because a dependency upgrade is a fence question, not a free consequence of adding a
   library, and a spike that reports the wrong list makes the fence unanswerable.
8. **⛔ THE "STRICT AT AUTHORING" STORY DOES NOT WORK, AND IT IS THE LOAD-BEARING ONE.** This note (and
   ADR 0037) pair two rules that are incompatible in this engine: *validate under `StrictVariables()` so
   a typo is refused*, and *every field reference carries a `default` so a Stanza can never render
   empty*. Verified: under `StrictVariables`, `{{ alert.nmae | default: "-" }}` fails with
   **"undefined variable"** — the strict check fires **before** the filter, so `default` never runs and
   cannot rescue anything. Strict alone therefore cannot tell a misspelling from a field that is
   legitimately absent on a digest, a resolved card or a signal with no rule snapshot, and it would
   refuse every honest wording that mentions one.

   **What replaced it: a read-set probe** (`internal/channels/render/wording/readset.go`). The typo
   check renders the template repeatedly against a **maximal** input — one where every field oto can
   ever produce is present — planting each undefined path as it is reported and probing again, bounded
   at 48 references because a Stanza is one line of prose. Absence from the maximal view means the name
   **does not exist**, rather than that this particular card lacks it. Three roots are exempt because
   their keys are the customer's data rather than oto's vocabulary — `labels`, `annotations`,
   `enrichment` — where a missing key is a fact about the signal, not a typo. Delivery stays lax and
   `default` does the totality job it was given.
9. **`actions` is a stanza that takes no Wording** — eight names, **seven wordable** (ADR 0048,
   `wording/stanza.go`). An actions block resolves to a list of `Action` structs whose visible label is
   bound to a stable `action_id` an interaction handler matches on, so re-wording a button is an
   interaction change wearing a wording's clothes (ADR 0043; git-bug `85da108`, `ccad583`). It stays in
   the enum so the vocabulary does not fork from SPEC §H.7, and is refused at save time with its own
   sentence, because a stanza silently absent from a menu teaches nobody why.

**Decisions taken since, recorded elsewhere:**

- **Markers are neutral and a `Dialect` spells them per provider; `slack_date` is renamed `datetime`;
  audience refusal is per-provider and had to be BUILT** — [ADR 0048](../adr/0048-a-wording-is-spelled-by-its-channel.md),
  which also documents the Discord seam and states plainly that no Discord provider exists. The
  "Refused" section below still says *"the sink strips … Mention audience stays with `MentionPolicy`"*;
  **there is no `MentionPolicy` and there never was a mention stripper** — read 0048 §3 instead.
- **Open question 1 (which schema validates a patch) is DISSOLVED, not answered.** A provider-neutral
  Wording needs no provider context to validate. See 0048's "Refused".
- **Open question 2 (the routing fork) is answered by NOT adding the column.** Presentation does not
  ride `notification_policies`; a Wording already carries its own `when` and `priority`, resolved
  per Stanza, first match wins, with the resolved set persisted on the delivery row beside `policy_id`.
- **Finding 10 (nineteen checks, not eighteen) is FIXED in the SPEC.** §L.6 now names `V0` in its table
  and states the count. `validate.go:91`'s own doc comment still says "eighteen" and is owed a fix.
- **Finding 7's `oto_render_invalid_total` is still absent.** No collector constructs it; SPEC §L.6 and
  AC-34's struck-metrics table both say so correctly, so there is no false promise left to delete —
  only a counter left to build.
- **Sequencing step 1 is DONE.** SPEC §H.7 now declares a render order distinct from its shed order and
  enumerates the twelve terminal fields; §L.6 counts nineteen; `Stanza` and `Wording` are in CONTEXT.md.

---

## The short answer

Customers get **Wordings**: a Liquid template that produces the text of one **Stanza** of a notification.
Structure stays oto's — Go builds every block, owns the attachment, colour and emoji, and validates the
result. A Wording chooses words; it cannot choose structure, colour, mentions, links, or destinations.

The engine is `github.com/osteele/liquid` v1.9.2 (MIT) on `NewBasicEngine()`, which ships **no tags and no
filters** — so there is no branching, no iteration, and only the filters oto registers by name.

## The gap this closes

Three of the card's prose surfaces already come from the alert's own annotations. `annotation(v, keys...)`
(`render/slack/renderer.go:178-197` — this note said `:163`) has four call sites across three surfaces: the title subtitle
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
| 8 | Snooze state and expiry | ~~**No** — not in the read model at all~~ **Yes** — `SnoozedUntil` (`channels/domain/view.go:90`), rendered at `root.go:146-147`. Closed by `77886a8`; see the revision block |
| 9 | `expired` — "oto stopped hearing about this" | Yes |

Two genuine gaps, and both are plumbing rather than authoring:

- ~~**(8) snooze is a read-model hole.**~~ **CLOSED (`77886a8`).** `notification/service/view.go:259`
  copies it and `channels/domain/view.go:90` carries it. The paragraph that was here described a gap
  that no longer exists.
- **(3) the `alert.history` payload reaches no surface.** `alert.history` is never named under
  `internal/channels/render/slack/`; `enrichmentSummary` (`reply.go:295-308`) renders a count and a label from
  `enricherLabel` (`reply.go:312-318`), never the payload.

~~**Close both before shipping Wordings.**~~ **Both are now closed** — (8) by `77886a8`, (3) by `StanzaInput` exposing enrichment payload scalars by name. A formatting surface over absent facts is a worse product than no
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

The curated filter set already exists as `text.go`'s golden-tested helpers — `human_duration`,
~~`slack_date`~~ **`datetime`** ([ADR 0048](../adr/0048-a-wording-is-spelled-by-its-channel.md): a
timestamp is a fact and `<!date^…>` is one product's spelling), `code`, `strike`, `truncate_runes` —
plus `default`, which is load-bearing for totality and which `NewBasicEngine()` does not ship. ⚠️ **No
filter emits a provider's syntax**: `strike` writes a neutral mark that a `Dialect` spells, never a
tilde. The shipped set is `wording/filters.go`'s `FilterNames`.

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

⛔ **`StrictVariables()` IS NOT THE TYPO CHECK, AND CANNOT BE.** Strict fires *before* the filters, so
`{{ alert.nmae | default: "-" }}` fails with "undefined variable" and `default` never runs — which makes
strict unable to distinguish a misspelling from a field legitimately absent on this card. The read-set
probe in `wording/readset.go` replaced it. See revision-block item 8.

## The safety property

> **A Wording can never mark a delivery `dead`, and can never emit a mention or a link.**

This is provable rather than hoped-for, because `validate.go`'s 19 identifiers (V0–V18) partition cleanly:

- **Structure Go owns** — attachment count, block whitelist, `block_id` uniqueness, `action_id` namespace,
  `plain_text` usage, unfurl flags. Unreachable, because a Wording emits text and Go builds every block.
- **Length and emptiness** — section text, field text, top-level text, metadata size, payload size. Bounded
  by the sink: `escape()` then `truncateSection`/`truncateField` against a declared per-Stanza **byte**
  budget — `truncateAt` counts bytes, and only the `truncate_runes` *filter* counts runes. Both apply.

**The gate artifact is an exhaustiveness test over those 19 identifiers**, each classified user-unreachable or
budget-bounded, failing to compile when a new check appears unclassified. If a check lands in neither bucket,
the feature does not ship.

Totality closes emptiness: every field reference carries a `default`, so a Stanza can never render empty and
no zero-information rule is violated by absence. At delivery, if a Wording errors anyway, that Stanza falls
back to its built-in Go value.

**This is not a new safety system.** `root.go:96` (this note said `:100`, which is a comment) is already `truncateSection(escape(body), v.Links.Group)` —
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
  `channels.renderer` member (SPEC §L.5), no per-channel custom renderer, no per-org branding.
- **No mentions** — ⛔ **as originally written this bullet was false in both halves.** There is no
  `MentionPolicy` (the whole surface was deleted, git-bug `bd0fb1d`) and there was no mention stripper —
  `escape()` defeated the bracketed tokens only as a side effect. Refusal is now per-provider behind
  `Dialect.StripAudience`, because Discord's `@everyone` is unbracketed and escaping would never touch
  it. See [ADR 0048](../adr/0048-a-wording-is-spelled-by-its-channel.md) §3.
- **No user-authored URLs** — `link()` escapes the label but not the url, which is exactly how
  `runbook_url: "<!channel>"` put a channel-wide ping in every push notification (`root.go:787-795`).
- **No Wording on the top-level `text` or attachment `fallback`** — SPEC §H.1 S5 (cited here as
  `SPEC.md:3228`, which is stale by roughly 1 200 lines): the push notification,
  the sidebar preview, the search snippet "and the only thing screen readers read", and the one surface only
  partially escaped by design.
- **No override of `color` or the strikethrough transition** (R10 at SPEC.md:43, §H.2, §H.4). Colour
  encodes state exclusively. Per-attribute emoji is not governed by those rules — `ShowFieldEmoji`
  (`ports.go:243`) is the nearest rule and any emoji knob belongs there.
- **No suppression of the terminal receipt** — SPEC §H.4 binds the rule snapshot, members, trail and
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
> never authors a Stanza's structure and never invents one. **Eight names, seven wordable**: `actions` is
> declared and refused, because a button's label is bound to its `action_id` (ADR 0048, revision-block
> item 9).
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

The Stanza set is not an invention: SPEC §H.3 already enumerates it — **"Block budget: 8 base blocks**
(title, body, fields, members, trail, rule, actions, footer). Ceiling is 50" — and simply had no collective
noun for it. ⚠️ **Every `SPEC.md:NNNN` citation in this note is stale by roughly 1 200–1 500 lines** and
has been converted to a section reference where it mattered; treat any surviving line number as a hint,
not an address.

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

Verified against `github.com/osteele/liquid` v1.9.2 in a scratch module. ⚠️ **The dependency list this
paragraph gave (`golang.org/x/sync`, `golang.org/x/tools`, `gopkg.in/yaml.v2`) is wrong** — see
revision-block item 7 for the real one, which includes two silent UPGRADES of existing dependencies.

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
   `{Enricher, Status, Payload, Warnings, Error, ComputedAt}` (`internal/channels/domain/view.go:233-240`
   — this note cited `notification/service/view.go:137-144`, the wrong module), but CONTEXT.md defines
   Enrichment as a result from a *"named, **versioned** Enricher."*
5. **The `alert.history` payload reaches no rendered surface.** Residue item (3).
6. ~~**Snooze is computed then dropped.**~~ **FIXED 2026-08-21 (`77886a8`).** Residue item (8).
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
9. ~~**§L.5.1 is drifted from the code.** SPEC.md:4633-4638 still declares `mention_on_reminder` and
   SPEC.md:3192 still cites it, while `providers/slack/config.go:55` says *"⛔ THERE IS NO
   `mention_on_reminder` HERE, AND ITS REMOVAL IS A BUG FIX"*. ADR 0020's deletion never reached the SPEC.~~
   **FIXED 2026-08-20 (git-bug `68653ca`).** And it was worse than "drifted": §L.5.1's block is
   labelled *"literal, `internal/channels/providers/slack/schema.json`"* and was not literal — it
   declared `mention_on_reminder` between `max_instances` and `link_names`, where the real file goes
   straight from one to the other. An operator writing channel config from the SPEC would have had
   that key rejected by `DisallowUnknownFields` with no hint why. The block now matches the file and
   records the deletion; §H.6's own removal note was already correct, so the SPEC had also been
   disagreeing with itself.
10. **`validate.go` has 19 identifiers, not 18.** V0–V18; its doc comment (`validate.go:91`) says "the
    eighteen outbound checks of §L.6" because V0 is an extra JSON-decode guard. V14, V17 and V18 are not
    Block Kit checks — they are top-level text, metadata size and payload size.
    **SPEC §L.6 is FIXED (2026-08-22): its table now carries a `V0` row and states the count as
    nineteen. `validate.go:91`'s own comment still says "eighteen" and is owed a one-word fix by
    whoever next touches that file.**

## Sequencing

Pre-release, the division that matters is not risky-vs-safe but **cheap now, expensive forever.**

1. ~~**Doc-only work, free exactly once.**~~ **DONE 2026-08-22.** §H.7 now declares a render order
   distinct from its shed order and enumerates the twelve terminal fields in §H.7.1 (which is what
   unblocks field ordering later, and which turned up a duplicate `*Last seen*` field on expired cards);
   ~~delete `mention_on_reminder` from §L.5.1~~ (git-bug `68653ca`); the metrics table promises nothing
   unbuilt — AC-34's struck list and §L.6 both correctly record that `oto_render_invalid_total` does not
   exist, so the work left there is to BUILD it, not to stop promising it; `Stanza` and `Wording` are in
   CONTEXT.md. §L.6 also gained `V0`, making the documented count nineteen.
2. **Make the pipeline honest, and close the read-model gaps.** Finding 7's render counter and death log,
   finding 8's retry wiring, snooze into `NotificationView`, and the `alert.history` payload rendered.
3. **The safety floor.** `StanzaInput`, the curated filter registry, the strict/lax engine pair, the sink
   wiring, and the 19-check exhaustiveness test. Nothing user-facing ships in this step.
4. **Wordings across all eight Stanzas**, plus the preview endpoint replaying against a stored
   `NotificationView`. Stanza-by-stanza rollout is a released-product technique and is not needed here.
5. **The policy-scoped override last**, once its two open questions are answered.
