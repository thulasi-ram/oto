---
title: 0005 — A durable group key, and the AlertGroup generation owns the Slack thread
---
**Status:** Accepted · 2026-08-07

## Context
Alertmanager's `groupKey` is `routeKey:labelSet` — it embeds the **route tree path**. Any edit to
`alertmanager.yml` that changes a matched route's matchers, or inserts an ancestor route, changes
`groupKey` for otherwise identical alerts. Config reloads are routine. It is also unescaped and
unbounded, so parsing it is wrong.

Separately: an earlier draft attached the Slack thread to an *occurrence*. Alertmanager notifies
per **group**. Posting one card per alert instance means a 300-alert storm becomes 300 Slack
messages at ~1 msg/s/channel — a 5-minute backlog of pure noise.

## Decision
```
group_key = "gk_" || base32hexLower(sha256(org_id || source_id || receiver || canon(groupLabels))[0:16])
```
Stable across route-config edits. Alertmanager's raw `groupKey` is stored as `source_group_key`
for observability and **MUST NOT be parsed**.

`(org_id, group_key, generation)` is UNIQUE. A generation is one open→closed cycle of the group.

**The Slack root message belongs to one AlertGroup generation.** `channel_threads` binds
`(channel_id, 'alert_group', group_id)` to `(provider_conversation_id, provider_thread_id)`.
Per-occurrence lifecycle facts become thread replies or root updates, never new roots.

A configurable oto grouping-rule engine is **cut**. UI grouping by alertname / namespace /
fingerprint is a **query** concern, entirely separate from persisted notification grouping.

## Consequences
- Editing `alertmanager.yml` no longer orphans every open thread. This is acceptance criterion 7.
- The Slack channel maps 1:1 to how Alertmanager already thinks, which is how every good
  reference tool (OnCall, incident.io, PagerDuty) behaves.
- A new root message is posted only when a **new generation** opens. Everything else is a
  `chat.update`.
- oto cannot express "group these differently from Alertmanager" in v1. That is a deliberate
  reduction in the correctness surface, and the UI's arbitrary grouping covers the human need.

## Alternatives rejected
- **Use AM's `groupKey` directly:** orphans threads on every config reload.
- **Thread per occurrence:** catastrophic in a storm; contradicts every reference implementation.
- **Ship a configurable grouping-rule engine in v1:** a second grouping semantics to keep
  consistent with Alertmanager's, for no user value that the UI's view-grouping does not provide.
