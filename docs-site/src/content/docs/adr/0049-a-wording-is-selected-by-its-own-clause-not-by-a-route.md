---
title: 0049 — A Wording is selected by its own clause, not by a route
---
**Status:** **WITHDRAWN** · 2026-08-22 · superseded by
[0050](/oto/adr/0050-a-notification-template-is-one-whole-message/)
**Relates to:** [0017](/oto/adr/0017-matchers-over-cel/) (the matcher vocabulary it reused),
[0037](/oto/adr/0037-wordings-are-liquid-and-structure-stays-otos/) (the ADR this one extended, itself
superseded by 0050), [0048](/oto/adr/0048-a-wording-is-spelled-by-its-channel/) (withdrawn alongside this one)

> **Tombstone.** This file exists so the ADR numbering has no silent hole. The decision it recorded
> was withdrawn before it was implemented, and the full text is in git history at commit `705aa50^`.

## What it said

A Wording carries its own `when` clause — matchers and reasons, in ADR 0017's vocabulary — and that
clause selects it. Routing is not consulted. This closed the open question ADR 0037 shipped with:
presentation should not ride the `notification_policies` table, because under first-match-wins an
override there costs a duplicated routing rule.

## Why it is withdrawn

The conclusion survived; its subject did not.
[ADR 0050](/oto/adr/0050-a-notification-template-is-one-whole-message/) keeps the principle that
presentation is chosen by its own predicate rather than by a route, but applies it to a whole-message
NotificationTemplate instead of a per-Stanza Wording. 0050 is the decision in force; read it instead
of this.
