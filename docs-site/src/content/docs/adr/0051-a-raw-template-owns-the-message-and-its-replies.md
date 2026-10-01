---
title: 0051 — A `raw` template owns the whole message, and a template may carry its replies
---
**Status:** Accepted · 2026-10-02
**Amends:** [0050](/oto/adr/0050-a-notification-template-is-one-whole-message/) — its rule that the colour
is never the author's, for `raw` only, and its "Owed" items on thread replies and on `raw` validation
**Relates to:** [0008](/oto/adr/0008-slack-update-in-place-primary/) (why `block_id`s and metadata stay oto's),
[0012](/oto/adr/0012-pastel-chrome-saturated-state/) (the state palette oto's own card keeps)

## Context

An operator rebuilt the resolved card as a `raw` template and found most of the message was not
theirs to write. The bar stayed green on a resolved `digest` alert because oto paints state. The
`[RESOLVED] … on cluster` line above the card was oto's. `{{ links.group }}` came out as a
private-use handle and `{{ case.started_at }}` as a marked timestamp, because only the `card` parser
and the `text` speller resolved those. `{{ actions }}` meant nothing in JSON. A hand-written button
failed outbound validation, and that failure **killed the delivery** — the one outcome ADR 0050 calls
the most important property of the feature. A summary with a `"` in it broke the JSON, and the
error told the author to use a `| json` filter that has never existed.

The owner's ruling: oto should let an author change every element of the message, and `raw` is
what that is for. The author also wanted the thread replies, which no template reached.

## Decision

### `raw` owns the message

A `raw` body may be a bare array of blocks, `{"blocks": [...]}`, or the message itself:

```json
{ "text": "…the push notification…", "color": "#a30200", "blocks": [ … ] }
```

| Element | `card` / `text` | `raw` |
|---|---|---|
| Blocks | the author's | the author's |
| Bar colour | oto's (state) | the author's; one Slack cannot parse is oto's |
| Top-level `text` | the document's plain words | the author's; absent is oto's |
| Where the buttons go | `{{ actions }}` | a `{"type": "oto_actions"}` block |
| Links, times, emphasis | resolved | **resolved** — spelled per string, `mrkdwn` text objects get Slack's |
| `block_id`, metadata, button `action_id`s and values | oto's | oto's |

The last row is the line, and it is drawn at what nobody sees and everything depends on. A
`block_id` is read back off an interaction; message metadata traces a click to its delivery; an
`action_id` an author could rewrite is an alert nobody can acknowledge from Slack.

**The colour rule ADR 0050 stated — "a template that could paint a firing card green could lie" —
is withdrawn for `raw`, deliberately.** It is still true. It is also the author's lie to tell in a
format whose whole promise is that they wrote the message, and the operator who asked for it wanted
the bar to mean *severity*, which oto's palette cannot say. `card` and `text` keep oto's colour.

### Values are escaped for Slack and for JSON at binding, and the author's text is not

A raw template is Block Kit typed by hand, so nothing downstream can tell the author's
`<https://grafana|Grafana>` from a label that says `<!channel>`. The value is defused where it is
still a value: `& < >` become Slack's entities, then the string is escaped as JSON-string content.
A quote or a newline in an annotation no longer breaks the document, and no `| json` filter is
needed. `| upper` and `| lower` step around both kinds of escape, so changing a value's case cannot
change what it means.

A stray `}}` is ordinary JSON and is no longer refused in `raw`. Every other delimiter mistake still is.

### A template that Slack would refuse costs the template, not the alert

`render/slack` runs `Validate` on a templated payload **before** choosing it. A failure falls back to
oto's own message, as every other template failure already did. This closes 0050's "Owed" item on
`raw` validation and restores its unconditional promise.

### A template may carry its thread replies

`notification_templates.reply_source` (migration 00082) is a second body, in the template's own
format, rendered for **thread replies** with `reason` bound. NULL is oto's own replies — every
template written before it. A body that renders nothing for a reason keeps oto's reply for that
reason, so restyling one reply is one `{% if reason == 'all_resolved' %}` around the body.

It lives **on the template, not beside it**, and the policy still names one template. A card and its
replies are one voice, edited in one dialog (two tabs), versioned together — `version` bumps when
`reply_source` changes — and attributed to one revision on the delivery row. The alternative, a
second picker on the policy, would let a card and its replies drift apart and was declined.

Save-time validation renders a reply body against one fixture per reply reason
(`template.ReplyFixtures`) as well as the ordinary corpus. Rendering nothing is not reported, and the
missing-`{{ actions }}` warning does not apply. The preview returns `reply_renderings`, one row per
reason, where a spelling with neither text nor error reads as "oto's own reply".

## Consequences

- oto's own card, replies and both golden corpora are byte-for-byte unchanged. Everything here is
  reachable only through a template.
- A `raw` author can now make a resolved card red. That is the author's call, made in the one format
  that says it is.
- Digests still take no template. The webhook renderer still ignores `reply_source`, because it
  renders no thread replies from a template today.
- An author-written button that isn't `oto_actions` must still pass Slack's rules and oto's
  `action_id` pattern; one that doesn't sends oto's own message rather than a broken one.

## Owed

- **Digests take no template.**
- **A Dialect registry** (carried over from 0050).
