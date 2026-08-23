# 0048 — A Wording is spelled by its channel, and the marks in between are oto's

**Status:** **WITHDRAWN** · 2026-08-22 · superseded by
[0050](0050-a-notification-template-is-one-whole-message.md)
**Relates to:** [0037](0037-wordings-are-liquid-and-structure-stays-otos.md) (the ADR this one
extended, itself superseded by 0050), [0049](0049-a-wording-is-selected-by-its-own-clause-not-by-a-route.md)
(withdrawn alongside this one)

> **Tombstone.** This file exists so the ADR numbering has no silent hole. The decision it recorded
> was withdrawn before it was implemented, and the full text is in git history at commit `705aa50^`.

## What it said

A Wording's curated filters emit a **neutral mark**, not a provider's punctuation, and a per-provider
`Dialect` spells that mark on the way out — `strike` becoming `~x~` on Slack, `~~x~~` on Discord, and
nothing at all on the webhook. The argument was that `*x*` is bold in Slack and italic in Discord, so
a body carrying literal Slack markers does not degrade on another provider, it renders the wrong
emphasis silently.

## Why it is withdrawn

Not because the dialect argument was wrong — it was not — but because the thing it spelled stopped
existing. [ADR 0050](0050-a-notification-template-is-one-whole-message.md) replaces per-Stanza
Wordings with one whole-message NotificationTemplate, so there is no per-Stanza body for a Dialect to
spell. 0050 is the decision in force; read it instead of this.
