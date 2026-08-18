# 0037 — A Wording is Liquid, and the structure around it stays oto's

**Status:** Proposed · 2026-08-18 · amends SPEC §F.1, §H.1, §H.3, §I.1, §L.5.1
**Relates to:** [0008](0008-slack-update-in-place-primary.md) (the card's structure; a broken layout is a
dead delivery), [0017](0017-matchers-over-cel.md) (matchers, not an expression language — this ADR keeps
that refusal for predicates and narrows it for prose),
[0020](0020-broadcast-the-transitions-that-must-be-seen.md) (the typed-knob precedent, and the unread-knob
trap)
**Design note:** [notification-content-customisation.md](../design/notification-content-customisation.md)

## Context

Customers will want to change how a notification reads. The question is what surface to give them.

There is a real gap, and it is narrower than "templating" but wider than a set of checkboxes. Three surfaces
of the Slack card already take their prose from the alert's own annotations — `annotation(v, keys...)`
(`render/slack/renderer.go:163`) feeds the title subtitle (`root.go:82`), the body (`root.go:92`) and the
top-level text (`root.go:757`) — and a rule author can already template those with Alertmanager's own
`text/template`, in their GitOps repo, failing in their own CI. So per-alert prose, tone, metric values,
impact copy and localisation are already solved, upstream, by the person who owns the rule.

What nobody can do is put **oto's own facts into a sentence**. "Firing 20 minutes, 4th time this week, still
unacked" is a sentence only oto can write, because Prometheus does not know any of it — and today only Go can
write it. The rule author lacks the facts; the operator lacks the surface. That gap is the entire
justification for this ADR, and a show/hide boolean does not touch it: a boolean chooses whether a fact
appears, never how it reads.

An earlier revision of this ADR refused all user-authored strings and offered typed booleans instead. That
was wrong. It measured the residue as "is this fact on the card?" when the question is "can anyone put this
fact into a sentence?", and it justified a bespoke typed-node grammar on safety grounds that turn out to be
**orthogonal to the choice of grammar** — the safety property is a function of the output type and the sink,
not of the language. Having established that, there was no remaining argument for hand-rolling a parser,
diagnostics, an evaluator, a formatter set and documentation for a notation nobody has ever seen.

## Decision

**A Wording is a Liquid template that produces the text of one Stanza.** Structure stays oto's: Go builds
every block, assigns every `block_id`, and owns the attachment, colour and emoji. A Wording's output type is
a string, never a block — which is what keeps every structural check in `validate.go` unreachable by a
customer.

**The engine is `github.com/osteele/liquid` v1.9.2 (MIT), built on `NewBasicEngine()`.** Verified by spike:

- `NewBasicEngine()` registers **no tags and no filters**. There is no `{% if %}`, `{% for %}`, `{% assign %}`,
  `{% case %}`, `{% include %}` or `{% capture %}`, and no inherited Shopify filter set. oto registers exactly
  the filters it wants, one at a time — curated by construction rather than by subtraction, so there is no
  bulk-import surface to re-audit on upgrade. (`UnregisterTag` is not the mechanism: it removes tags but not
  block tags, and there is no `UnregisterBlock`.)
- **No `{% for %}` makes a correctness bug structurally unreachable.** Liquid's map iteration is
  non-deterministic — a spike over a four-key map produced two different orders on consecutive renders. oto
  hashes the rendered payload (`rendered_hash`) to suppress no-op `chat.update` calls, so a wobbling hash
  would re-send the same card forever. With no loop construct, no Wording can reach it.
- **Strict at authoring, lax at delivery.** `StrictVariables()` turns an unknown variable into
  `undefined variable in {{ srvice }}`; the default engine renders it as empty. So oto validates with a strict
  engine when a human is present to be told, and renders with a lax engine so a missing field degrades one
  Stanza instead of killing a delivery. This is the red-team doctrine — refuse at write time, degrade at
  render time — and it is two constructors rather than custom machinery.
- **Save-time validation must execute, not merely parse.** An unknown filter is a render-time error, not a
  parse-time one. So saving a Wording parses it strictly *and* renders it against a fixture corpus including
  the hostile cases (empty labels, oversized annotation, nil enrichment, terminal card).
- **Bindings are a flat `map[string]any` of scalars — never a domain type.** Liquid reflects into Go structs:
  a spike passing a struct into bindings rendered its unexported-by-intent field via `{{ s.Token }}`. A
  Wording is therefore given a purpose-built `StanzaInput` projection, so "what can a Wording reach" is a
  struct definition rather than a reflection question. `*NotificationView` is never passed.

**Liquid over Go `text/template` and over Jinja**, for three reasons that survived the spike. Its filter
syntax reads in the order a person formats (`{{ service | default: "unknown" | truncate: 40 }}` against Go's
inside-out `{{ truncate 40 (default "unknown" .Service) }}`), and filter chaining is the operation this
feature exists for. It was designed for user-authored templates in a product the author does not control, so
it is logic-limited by intent rather than by our restraint — where adopting Jinja would mean forbidding
inheritance, macros and includes in someone else's engine and re-checking that on every upgrade. And it is
**mechanically incompatible with Alertmanager's idioms**: `{{ $labels.job }}`, `{{ .Service }}` and
`{{ humanize $value }}` are all Liquid *syntax errors*, so a pasted Alertmanager snippet is refused at save
time rather than silently rendering blank. Adopting `text/template` would have given the same delimiters with
different variables, which is a trap rather than a transfer of knowledge.

**The safety property, and it is testable rather than asserted:** *a Wording can never mark a delivery `dead`,
and can never emit a mention or a link.* Every identifier in `validate.go` is either about structure Go owns —
unreachable, because a Wording emits text — or about length and emptiness, which the sink bounds. The gate
artifact is an **exhaustiveness test over `validate.go`'s 19 identifiers (V0–V18)**, each classified
user-unreachable or budget-bounded, failing to compile when a new check appears unclassified. If a check
lands in neither bucket, the feature does not ship.

**Every interpolated value and every literal passes the existing sink** — `escape()` then
`truncateSection`/`truncateField`, which already mark the cut and link to oto, and which already carry
upstream annotation text today (`root.go:100` is literally `truncateSection(escape(body), v.Links.Group)`).
This is not a new safety system; it is an existing one pointed at an input of the same trust class.

**A Wording is text, so it crosses channels.** Slack receives it as the mrkdwn emphasis subset after
escaping; the webhook receives it as a string in a `rendered` map keyed by Stanza and strips the markers,
because the webhook is not a degraded Slack. This is the part of the cross-channel problem that a
posture-and-layout abstraction could not solve: **text is portable, layout is not.**

**Conditionality is two Wordings with different matchers, not a branch inside one.** The `when` clause reuses
`Matcher{Name,Op,Value}` and `Reasons` verbatim, so ADR 0017's refusal of a second predicate language stands.
The reason is legibility, not safety: a matcher lets the UI show *which Wording won*, and a branch buried in a
template body cannot.

## Refused

- **No structure.** No block authoring, no BYO Block Kit, no raw mrkdwn passthrough, no new
  `channels.renderer` member (SPEC.md:1451), no per-channel custom renderer, no per-org branding.
- **No mentions.** The sink strips `<!channel>`, `<!here>`, `<@U…>` and `<!subteam^…>` from interpolated
  values *and* from literals. Mention audience stays with the existing `MentionPolicy` — one mechanism, one
  code path.
- **No user-authored URLs.** `link()` escapes the label but not the url, which is exactly how
  `runbook_url: "<!channel>"` put a channel-wide ping in every push notification (`root.go:787-795`). Links
  come only from the fixed `Links` set.
- **No Wording on the top-level `text` or attachment `fallback`.** SPEC.md:3228 (S5): it is the push
  notification, the sidebar preview, the search snippet "and the only thing screen readers read" — and it is
  the one surface that is only partially escaped by design.
- **No override of `color` or the strikethrough transition** (R10 at SPEC.md:43, §H.2, SPEC.md:3364). Colour
  encodes state, exclusively. Per-attribute emoji is *not* governed by those rules; the nearest rule is the
  existing `ShowFieldEmoji` (`ports.go:243`), and an emoji knob belongs there.
- **No suppression of the terminal receipt.** SPEC.md:3382 binds the rule snapshot, members, trail and
  overflow links onto `resolved`/`expired` cards, learned from a live run where "the card became least
  informative at exactly the moment it became the only remaining record."
- **No enumeration.** Without `{% for %}`, a Wording cannot iterate the member list. "N of M instances" stays
  Go's. This is a real ceiling and it is accepted deliberately.

## Consequences

The expressiveness ceiling is one line of prose per Stanza, built from literals, allowlisted field references
and curated filters, with no branching and no iteration. A customer above that ceiling is pointed at an
**Enricher** — they own the computation, it is provenanced and testable, and the Wording only places its
result. A customer who wants a Block Kit layout oto does not emit is pointed at the `webhook` provider.

Two costs are accepted rather than solved. Liquid's errors quote the offending expression but carry no
line or column — acceptable for a one-line Stanza, and it would not be for a document. And the settings form
can no longer be generated from JSON Schema the way `configschema` generates the channel form, because a
template body is a text field; the replacement is an editor with autocomplete over `StanzaInput`, fed by a
`view-paths` endpoint. That is more work and a higher ceiling. `Template.GetRoot()` exposes the AST, so the
declared read set — "which Wordings break if this Enricher is deleted" — remains buildable.

**What would falsify this:** dogfood oto against a real Prometheus for two weeks and count the times a
wording change is wanted and the ceiling blocks it. If iteration or branching is repeatedly the blocker, the
ceiling is wrong. The pre-release corpus check behind ADR 0026 is a weaker substitute — walk its rules and ask
what sentence each deserves. The earlier revision's falsifier ("count customer customisation demands") was
unrunnable: there are no customers yet.

**Field ordering is not decided here.** It remains blocked on a SPEC defect rather than on this ADR:
SPEC.md:3516 is a drop-order-on-overflow, and `root.go:109-117` makes display order and shed priority one dial,
so a user-settable order silently decides what sheds. Pre-release that defect is a doc edit — amend §H.7 to
declare a render order distinct from its shed order and enumerate the twelve terminal fields — after which
ordering can be revisited.

**The policy-scoped override ships as an open question**, not a decision. Two things are unanswered: which
schema validates a `card`/Wording patch on `notification_policies`, a table with no provider context whose
`channel_ids` may name up to 16 channels of mixed type; and whether it is acceptable that, under
first-match-wins (`policy.go:214-217`), an override costs a duplicated routing rule. The second is the
stronger argument that presentation should not ride the policy table at all.
