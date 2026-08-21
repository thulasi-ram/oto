# 0048 — A Wording is spelled by its channel, and the marks in between are oto's

**Status:** Accepted · 2026-08-22 · **corrects** [0037](0037-wordings-are-liquid-and-structure-stays-otos.md)
in three places, one of which is a safety claim that was false when it was written
**Relates to:** [0037](0037-wordings-are-liquid-and-structure-stays-otos.md) (the ADR this amends —
its ruling that a Wording is Liquid and structure stays oto's is untouched),
[0013](0013-actor-never-subject.md) (why oto may not name a human),
[0020](0020-broadcast-the-transitions-that-must-be-seen.md) (the unread-knob trap),
[0043](0043-the-slack-action-row-renders-five-elements.md) (why a button's label is structure)
**Amends:** SPEC §H.7 (the Stanza set and the two orders), §L.6 (nineteen checks, not eighteen)
**Design note:** [notification-content-customisation.md](../design/notification-content-customisation.md)

## Context

ADR 0037 ends on a sentence that is doing more work than it looks like it is doing:

> **A Wording is text, so it crosses channels.** Slack receives it as the mrkdwn emphasis subset after
> escaping; the webhook receives it as a string in a `rendered` map keyed by Stanza and strips the
> markers, because the webhook is not a degraded Slack. … **text is portable, layout is not.**

That is a binary — Slack's punctuation, or none — and it is true only while exactly two providers exist
and one of them is a program. The brief that produced this feature asked for "Slack and other
channels" and named Discord. Taking a third provider seriously breaks the sentence, and it breaks it
on a detail that is not a nuance:

| meaning | Slack mrkdwn | Discord markdown |
|---|---|---|
| bold | `*x*` | `**x**` |
| italic | `_x_` | `*x*` or `_x_` |
| strike | `~x~` | `~~x~~` |
| code | `` `x` `` | `` `x` `` |

**`*x*` is BOLD in Slack and ITALIC in Discord.** A Wording body carrying literal Slack markers does
not *degrade* on Discord — it renders the wrong emphasis, silently, in a message about a production
signal. Nobody gets an error, and the author never learns. So "text is portable" is not a fact about
text; it is a fact about text that carries no provider's punctuation, and 0037 asserted the first while
describing the second.

The same crack runs through two more of 0037's sentences, and one of them is load-bearing for safety.
Both are corrected below.

## Decision

### 1. A filter emits a NEUTRAL MARK. A Dialect spells it.

**The curated filters do not write a provider's syntax.** `strike` does not emit `~x~`; it emits a
semantic mark that the Slack sink spells `~x~`, a Discord sink would spell `~~x~~`, and the webhook
sink drops while keeping the words. Same for `code`, `bold` and `italic`.

The implementation is committed: `internal/channels/render/wording/dialect.go`. Its shape is the whole
decision, so it is worth stating rather than pointing at.

- **`Dialect` is an interface with five methods** — `Name`, `Emphasis(MarkKind) (open, close string)`,
  `Timestamp(at, fallback)`, `StripAudience(s)` and `EscapeText(s)`. A provider is added by writing one
  of these. Not by touching a template, not by touching a filter, not by touching the sink.
- **The marks are private-use codepoints, and that is the mechanism rather than a style.** A Wording
  must not be able to forge one, because forging a mark is how a customer would get raw markup — and
  eventually an audience ping — past the sink. So `sanitise` strips the whole private-use area, plus
  other-format codepoints (where the bidi overrides live) and control characters, from **every**
  interpolated value and **every** literal *before any filter runs*. The only codepoints in that range
  that can reach a Dialect are ones oto's own filters put there.
- **An unrecognised mark is dropped and the words are kept.** An empty `Emphasis` pair means "this
  provider cannot show it"; the emphasis goes and the text it wrapped stays. A private-use codepoint
  reaching a chat client renders as a replacement glyph on some and nothing on others — invisible to
  the author, visible to exactly one reader — so `Spell` never lets one out.
- **`EscapeText` is on the interface, and the ORDER is why.** Escaping the finished string would
  destroy oto's own output: Slack's `<!date^…>` token would become `&lt;!date^…&gt;` and render as
  literal garbage. Escaping before rendering is worse — the marks are inserted *by filters, during the
  render*, so there is no "before" that sees them. `Spell` therefore walks the string, escapes each run
  of words as it goes, and writes markup through untouched. And escaping is per-provider because it is
  a quirk like any other: handing a webhook consumer `&amp;` where the alert said `&` is not safety, it
  is corruption of a value that program is about to process.

Two Dialects ship: `SlackDialect` (backtick, `~`, `*`, `_` — deliberately identical to what
`render/slack/text.go` already emits by hand, so a Wording and a built-in string are typographically
indistinguishable on the card) and `PlainDialect` (drops every mark, keeps every word).

**This is cheap now and expensive forever**, which is the sequencing rule the design note opens with. No
Discord provider is built. The filter set still must not hardcode Slack, because the cost of unpicking
it later is every stored Wording in every customer's database.

### 2. `slack_date` is renamed `datetime`. A timestamp is a fact; `<!date^…>` is one product's spelling.

The design note's curated filter list literally named `slack_date`. Slack renders a viewer-local time
with `<!date^unix^{fmt}|fallback>`; Discord uses `<t:unix:R>`; a webhook consumer wants an ISO-8601
string it can parse. Registering the filter as `slack_date` bakes one provider's spelling into the
user-facing language of all of them — into autocomplete, into documentation, into every template a
customer ever writes.

**The filter is `datetime`** (`wording/filters.go`, `FilterNames`). It passes a time mark straight
through: `BuildInput` has already stamped both the epoch and oto's own UTC rendering into the mark, and
the Dialect decides which of the two the provider can use. `SlackDialect.Timestamp` emits the `<!date>`
token with oto's UTC string as the fallback (S13); `PlainDialect.Timestamp` returns the fallback and
nothing else. Applied to something that is not a mark, `datetime` is the identity, so
`{{ "n/a" | datetime }}` says `n/a` rather than erroring.

`human_duration` and `truncate_runes` were already neutral and keep their names.

### 3. Refusing an audience is per-provider, and it had to be BUILT rather than reused.

**⛔ ADR 0037 IS WRONG ABOUT MENTIONS, IN BOTH HALVES OF ONE SENTENCE.** It refuses them like this:

> **No mentions.** The sink strips `<!channel>`, `<!here>`, `<@U…>` and `<!subteam^…>` from
> interpolated values *and* from literals. Mention audience stays with the existing `MentionPolicy` —
> one mechanism, one code path.

Neither half was true when it was written.

- **There was no mention stripping in the sink.** `escape()`
  (`internal/channels/render/slack/text.go:138-141`) replaces `&`, `<` and `>` generically, to defend
  the mrkdwn parser. It neutralises those four tokens' brackets as a **side effect**, not as a rule
  about audiences. A future refactor could narrow it without knowing that anything was resting on it.
- **There is no `MentionPolicy`, and there is no mention surface at all.** The whole thing was deleted
  (git-bug `bd0fb1d`). `RenderOptions.Mentions` is gone and `internal/channels/domain/ports.go:350-358`
  is its tombstone; `internal/channels/render/slack/renderer.go:27-46` carries the matching *"⛔ THE
  RENDERER HOLDS NO MENTION AUDIENCE, AND IT USED TO"*, and records that it was deleted **twice** for
  two different reasons — first as a control wired to nothing, then as a mechanism withdrawn on product
  grounds with the unacked reminder it served.

So 0037's claim that this is "an existing system pointed at an input of the same trust class" is
exactly backwards for this one property. **There is no existing mechanism to keep using.** Generic
escaping is what currently holds the line, and it holds it only for **bracketed** dialects.

And that is the safety consequence, not a matter of taste. Every token in 0037's list is Slack-shaped
and bracketed. **Discord's broadcast pings are `@everyone` and `@here` — bare literals with no
brackets.** Escaping `<` would never touch them. A single Slack-shaped stripper passes `@everyone`
through untouched, so the property "a Wording can never address an audience" is provider-relative, and
one stripper cannot hold it for all providers.

**DECISION: the neutral rule is stated once — *no Wording output may address a group of humans* — and
each Dialect implements it for its own spellings, behind `StripAudience`.** It runs on the finished
string, after marks are spelled.

`SlackDialect.StripAudience` removes `<!channel>`, `<!here>`, `<!everyone>`, their HTML-escaped forms,
the bare `@channel`/`@here`/`@everyone`, and the id-carrying `<@…>` and `<!subteam^…>` spans in both
raw and escaped bracketing. **It runs even though `escape()` already defeats the bracketed forms, and
the redundancy is the point**: ADR 0037 promises a Wording "can never emit a mention", and a promise
that rests on a side effect of an unrelated function is not a promise. `PlainDialect.StripAudience`
removes `@channel`, `@here`, `@everyone` and `@room`, so a webhook consumer that forwards oto's text
into a chat product cannot be used as a laundering step for a ping the Wording was not allowed to send.

`TestNoWordingCanEmitAnAudience` (`wording_test.go:107`) is what holds it: it renders a set of hostile
sources — including a literal `<!channel> @everyone @here <@U024BE7LH> <!subteam^SAZ94GDB8>` — through
**every** Dialect against **every** fixture, and fails on any banned token in the output. Its `banned`
map is keyed by `Dialect.Name()`, which is the shape a third provider slots into.

⚠️ **AND THE TEST IS NOT YET EXHAUSTIVE OVER PROVIDERS.** It iterates a hand-written slice
`[]Dialect{SlackDialect{}, PlainDialect{}}`, so a Dialect added and forgotten is a Dialect untested,
and a provider shipped with no Dialect at all is not caught anywhere. That is the same failure shape
§L.6's check enumeration exists to prevent, and it fails in the direction that pings a whole channel at
03:00. **The requirement is that the test iterate a registry rather than a literal, and a provider
without a Dialect refuse to construct.** Recorded as owed work rather than claimed as done.

## The Discord seam — designed, and deliberately not built

**No Discord provider exists in this tree, and this ADR does not add one.** What it adds is the answer
to "what would a `DiscordDialect` have to say", written down while the interface it must satisfy is
being decided, because a seam is only cheap before the first provider ships. This section is a
specification for work that has not started, not a description of code.

A `DiscordDialect` must answer five questions, which are exactly the five methods.

| Method | What Discord's answer is |
|---|---|
| `Name()` | `"discord"` |
| `Emphasis(MarkBold)` | `**` / `**` — **two** asterisks. One is italic. This one line is the reason marks exist |
| `Emphasis(MarkItalic)` | `*` / `*` or `_` / `_`; `_` is the safer pick, since `*` collides with the bold spelling under nesting |
| `Emphasis(MarkStrike)` | `~~` / `~~` — **two** tildes. Slack's one tilde is literal text on Discord |
| `Emphasis(MarkCode)` | `` ` `` / `` ` `` — the one spelling the two products agree on |
| `Timestamp(at, fallback)` | `<t:unix:R>` for a relative rendering, `<t:unix:f>` for an absolute one. Note the shape difference: Discord's token carries **no fallback string**, so the `fallback` argument is discarded rather than embedded, unlike Slack's `<!date^…\|fallback>` |
| `StripAudience(s)` | `@everyone` and `@here` (**bare, unbracketed** — this is the case a Slack-shaped stripper misses), plus `<@id>` for a user and `<@&roleid>` for a role. The bracketed pair are `stripBracketed` calls; the bare pair are `replaceFold` calls, and it is the bare pair that makes this method per-provider rather than shared |
| `EscapeText(s)` | Discord has no `&`/`<`/`>` entity escaping. Its markup characters are `*`, `_`, `~`, `` ` ``, `\|` and `\`, neutralised by backslash-prefixing. Reusing Slack's escaper here would emit literal `&amp;` into a Discord message |

**The limits are different in kind, not only in number**, and a Discord provider's renderer — not its
Dialect — is where that lands:

| | Slack (verified, `render/slack/text.go`) | Discord (published limits, re-verify before building) |
|---|---|---|
| body text | `section.text` 3 000 | embed `description` 4 096 |
| field label | — | field `name` 256 |
| field value | `section.fields` item 2 000 | field `value` 1 024 |
| fields per unit | 10 | 25 |
| footer | (a `context` block, 10 elements) | 2 048 |
| whole unit | 50 blocks per message | 6 000 characters per embed |
| units per message | 1 attachment (S3) | 10 embeds |
| top-level text | oto's own 3 000 (V14) | `content` 2 000 |

The two that would actually bite are **field `value` at 1 024** — half of Slack's 2 000, so the
per-Stanza budget a Wording is validated against must be the **minimum across providers**, never
Slack's — and **field `name` at 256**, which Slack does not bound separately at all because a label and
its value share one 2 000-char item.

**And one genuinely structural difference, which turns out to cost nothing.** A Discord embed's `color`
is a **single integer**, rendered as a left-border stripe on the embed. That maps cleanly onto oto's
attachment colour: one unit, one colour, one stripe down the side. So **R10 — colour encodes state,
exclusively (SPEC §H.1 S4, §H.2)** — survives a Discord port unchanged. `#a30200` becomes `10682368`
and means the same thing. This is worth recording because it is the rule most likely to be assumed
broken by a port, and it is not.

**What would have to happen to build one**, in order: a `providers/discord` package with a connection
holding a bot token or a webhook URL (ADR 0047's connection/channel split already has the shape); a
`render/discord` package that builds embeds the way `render/slack` builds blocks, with its own golden
files and its own `Validate` enumerating its own checks the way §L.6 does; a `DiscordDialect` in
`render/wording`; and the update-in-place story, which is the genuinely hard part and the one this
section does **not** answer — ADR 0008 makes `chat.update` on a stored `(channel_id, ts)` the primary
mechanism, and whether Discord's message-edit endpoint holds the same properties under the same failure
modes is an unasked question. **A Dialect is an afternoon. A provider is not, and nothing here claims
otherwise.**

## Refused

- **No per-provider Wording.** A Wording is one row and one text, and it renders on every destination a
  policy names. The alternative — a Wording per provider — reintroduces exactly the fork this ADR
  exists to prevent, and it would make "which Wording won" a two-dimensional question.
- **No provider context at validation time** (and this is what makes the above affordable). Because a
  Wording is provider-neutral by construction — neutral marks, neutral filters, no literal audience
  spelling in any dialect, and a length budget taken as the minimum across providers — validating one
  needs no knowledge of where it will be sent. The design note's open question "which schema validates
  a Wording patch on a policy naming sixteen channels of mixed type" is **dissolved rather than
  answered**: nothing being validated is provider-specific.
- **No raw markup passthrough, in any dialect.** `sanitise` runs before the filters and the Dialect runs
  after them, so there is no seam where a customer's literal `**` becomes Discord's bold. If they type
  two asterisks, two asterisks is what a reader sees. `bold` is the filter, and it is the only way in.

## Consequences

Adding a provider's *typography* is now one file and one interface implementation, and the compiler
names what is missing. Adding a provider's *transport, structure and update semantics* is unchanged and
is still the expensive part — this ADR narrows the seam, it does not shrink the port.

The cost accepted is one more indirection between what an author types and what a reader sees. A
Wording that says `{{ service | code }}` produces neither a backtick nor a visible mark; it produces a
private-use codepoint that only `Spell` resolves. That is invisible in a debugger and it is why
`dialect.go` opens with two ⛔ paragraphs rather than one, and why the preview endpoint must render
through a real Dialect rather than showing the intermediate string.

**What would falsify this:** build the Discord provider and count how many of the eight rows in the
seam table above turn out to be wrong. If `Dialect` needs a sixth method to accommodate it, the
interface was drawn at the wrong altitude and the right correction is a smaller one — probably
splitting `EscapeText` from `Emphasis`, since escaping is the method whose contract is least alike
across the two products. If it needs no new method, the seam was worth designing before it was needed.
