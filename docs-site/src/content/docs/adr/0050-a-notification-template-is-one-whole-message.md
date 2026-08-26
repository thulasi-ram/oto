---
title: 0050 — A NotificationTemplate is one whole message, and its policy says which one
---
**Status:** Accepted · 2026-08-22
**Supersedes:** [0037](/oto/adr/0037-wordings-are-liquid-and-structure-stays-otos/) (Wordings, per-Stanza
overrides), and the two ADRs that extended it — 0048 (a Wording is spelled by its channel) and 0049
(a Wording is selected by its own clause), both withdrawn with this one.
**Relates to:** [0017](/oto/adr/0017-matchers-over-cel/) (the `when` clause this
stops duplicating), [0008](/oto/adr/0008-slack-update-in-place-primary/) (why a rendered payload is hashed),
[0034](/oto/adr/0034-notifications-as-a-top-level-destination/) (where a policy is written)

## Context

ADR 0037 shipped **Wordings**: one Liquid template producing the *text* of one named block, four of
SPEC §H.7's eight, with structure staying oto's. The safety argument was sound and is worth restating,
because most of it survives: a Wording's output type was a `string`, never a block, so Go still built
every block, assigned every `block_id`, and owned the attachment, colour and emoji. Nothing a customer
typed could reach the parts of the payload oto's own control plane reads back.

It was safe, it was defensible, and it was the wrong product. Reviewers' first question was
consistently *"so where is my template?"*, and the honest answer was that there wasn't one — there
were four holes in somebody else's card. A template a person can read top to bottom is one they can
predict. Four independent overrides, each with its own matcher clause and its own priority, cannot be
read that way and cannot be predicted without holding two resolution rules in your head at once: the
routing one that chose the channel, and the presentation one that chose the words.

Every comparable product does whole-message: Alertmanager, Grafana, Datadog, Opsgenie. That is not
fashion. It is that the unit somebody edits should be the unit that gets sent.

## Decision

A **NotificationTemplate** is one whole message, in one of three formats.

| format | the author writes | portable | oto owns |
|---|---|---|---|
| `card` (default) | Markdown plus two extensions | **yes** — compiled per provider | the action row's identity |
| `text` | one flat string | yes | the sink |
| `raw` | literal Slack Block Kit JSON | **no** — Slack only | validation, `block_id`s |

It carries **no `when` clause**. `notification_policies.template_id` names it. A policy already has
matchers, already has reasons and already chose the destinations; it now also says what they read
like. One routing decision, one place to read it.

### What makes a `card` portable: a document IR

A `card` template renders to Markdown, which is parsed into oto's own small document IR — seven block
kinds, five span kinds — and each provider compiles *that*. Adding Discord is writing one compiler
over those twelve node kinds. It is not touching a template, a filter, or the parser.

The IR is deliberately smaller than Markdown. Every node in it has a defensible spelling in Block Kit,
in a Discord embed, and in plain text. Tables, nested lists and images are **refused at parse time
with a sentence naming the alternative**, rather than silently degraded — a table that renders as
mangled prose at 03:00 is worse than one that never saved.

Markdown was chosen over HTML because it is already what a Slack or Discord user types *in the very
product this message is going to*. HTML's surface is enormous and we would support perhaps ten tags;
every other tag becomes a support ticket. If email ever becomes a first-class channel, HTML becomes
the native target and this decision is worth revisiting.

### The IR is also what fixed the escaping

The predecessor escaped runs of a finished string while walking past markup it had itself emitted.
Two live bugs came out of that shape: oto's own `<!date^…>` token arrived as `&lt;!date^…&gt;`, and an
oto-issued link arrived defused inside a code span because `defuseLinks` could not tell it from an
address a label had smuggled in.

A `SpanText` is prose **by construction**. Escaping and audience-stripping and link-defusing happen
once, per span, and oto's own markup is never in scope for any of them.

### Links and timestamps are unforgeable handles, not URLs

Once Liquid has flattened a template into one string, `https://oto.example/case/1` and
`https://evil.example/phish` are the same kind of thing. So oto does not put URLs into the binding at
all. It puts **private-use handles**, minted *after* `sanitise()` has stripped the entire private-use
area (U+E000–U+F8FF) from the template source and from every interpolated value — so the only handles
that can reach the parser are ones oto put there.

`[text]({{ links.group }})` therefore works, and `[click](https://evil.example)` is refused with a
sentence naming the fix. A literal address typed by an author is prose, and meets `DefuseLink` like
any other bare URL.

### Every value is escaped, unconditionally, with no opt-out

In `card` format every interpolated value is markdown-escaped. There is no `{{{ }}}`, no `| raw`, and
no taint tracking — because none is needed. An alert label is attacker-influenced: anyone who can fire
a metric can write one. A value that cannot produce syntax cannot produce structure, a link, a
mention, or a forged handle. An author who genuinely needs raw markup uses `format: raw`, which is
gated separately.

### Control flow is oto's own, on a basic engine

Whole-message authoring needs iteration — nobody can render a label list without it. `NewBasicEngine`
ships zero tags and zero filters, and **it can never be given the library's**: `Engine.cfg` is
unexported, so `tags.AddStandardTags` is unreachable from outside the package. The alternative was
`NewEngine()`, which brings thirteen tags — `include` and `render` among them, both of which read from
a template store — plus forty filters, and there is no `UnregisterFilter`.

So oto implements `for` and `if`/`unless` itself via `RegisterBlock`. Two consequences, both good:

- The **iteration budget is a real counter** — 250 per loop, 2000 per render, charged inside the loop
  — rather than a source scan for `{% for %}` nesting depth reasoning about a worst case.
- There is no `{% else %}`, because `RegisterBlock` cannot declare a clause. `{% unless %}` covers the
  case it usually serves. That is a real cost and it is smaller than thirteen unaudited tags.

The one rule that survives ADR 0037 intact: **ordered slices, never maps.** Go randomises map
iteration, oto hashes the rendered payload to suppress no-op `chat.update` calls, and a wobbling hash
re-sends the same card forever. `asList` refuses a bare map at the tag, and `BuildInput` offers a
slice for everything worth iterating — so `members` and `trail`, two of the four stanzas the slot
design *had* to refuse, are finally readable.

### The action row is the operator's to place, or to omit

`{{ actions }}` positions the button row. Leaving it out ships a card with no Acknowledge button.

That is permitted, deliberately, and it is a **degraded card and not a lost alert**: `router.go:188`
registers `POST /api/v1/cases/{id}/ack`, reaching the same service method the Slack button does. So
oto warns loudly — at save, and against the example that demonstrates it in the preview — and never
blocks. The save button stays enabled through the warning.

What is *not* the operator's is the row's identity. `action_id` is the dispatch key `interactions.go`
switches on and `validate.go` pins to `^oto\.[a-z0-9._]+$`. A label an author can rewrite is a label;
an `action_id` they can rewrite is an alert nobody can acknowledge.

### A mismatched provider is the operator's headache, never a dropped alert

A policy fans out to as many as sixteen channels and they need not share a provider. A template's
`provider` field is **declared intent**: it drives the editor mode, the preview default and a
save-time warning. It gates nothing. `card` and `text` render anywhere; `raw` sent to a webhook falls
back to oto's built-in card.

Every failure path — parse error, unknown filter, missing field, empty render, unsupported Markdown,
JSON that interpolation broke, wrong provider entirely — returns "no template" and Go builds its own
card. **A template can never mark a delivery dead.** That is the single most important property in
the feature and it is pinned by a test that walks six distinct ways to be wrong.

## Consequences

- ADR 0037's per-Stanza vocabulary is retired. `Stanza` as a *noun* survives only inside the Slack
  renderer, describing blocks Go builds; it is no longer a customer-facing concept.
- ADR 0037's V0–V18 exhaustiveness gate is retired with it. Its argument — "output is a string, so
  every structural check is unreachable" — stops being true the moment a template owns structure. The
  replacement is narrower and more honest: a template-rendered payload goes through **the same
  `Validate` every built-in card passes**, asserted end to end.
- A delivery records `(template_id, template_version)` and not the source. A delivery row is written
  on every send and a template runs to 16 KiB. What makes the pointer sufficient is that the rendered
  payload is already on the same row: the bytes are never in doubt, only the attribution is.
- `version` bumps only when `source` or `format` changes. A rename produced no different bytes on any
  card, and bumping then would claim two revisions that rendered identically.
- Deleting a template is soft, and the policies naming it fall back to oto's built-in card at the next
  delivery. The policy keeps pointing at the row; resolution stops returning it.

## Owed

- **A Dialect registry.** `previewDialects` is a literal slice in two places. A provider shipped
  without a Dialect should refuse to construct; today it is simply a column the preview does not show.
- **Digests and thread replies take no template.** They are built by their own renderers and emit
  block names §H.7 does not have. The digest view stays in the fixture corpus as a robustness case.
- **A `raw` template is validated for JSON shape, not for Block Kit validity**, until `render/slack`
  runs `Validate` on the result. Slack's own rejection is caught and falls back; it is not predicted.
