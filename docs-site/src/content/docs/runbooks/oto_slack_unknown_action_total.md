---
title: oto_slack_unknown_action_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | **none, deliberately** (see below) |
| Registered in | `internal/channels/service/metrics.go` |
| Alertable | **yes — any increment.** Both causes are oto's fault |
| Rule | `OtoSlackUnknownAction` in `deploy/prometheus/oto-rules.yaml` |

> SPEC §H.8 spells this metric `slack_unknown_action_total`, without the prefix. The code adds the
> `oto_` prefix on purpose — the SPEC line is an omission, not a second rule, and an unprefixed
> metric would be the only one in oto's namespace a scrape config could not select with
> `{__name__=~"oto_.*"}`.

## What it counts

Verified Slack interactions naming an `action_id` oto has no branch for. The signature was valid,
the payload was genuine, a human really did press the button — and oto had nowhere to route it.

The known ids are `oto.ack`, `oto.unack`, and anything under the `oto.noop.` prefix (the URL
buttons, which Slack delivers an interaction for and oto must acknowledge with nothing to do).

## Why it exists, and why it is a counter rather than a log line

An unknown action is answered **200**, and must be: Slack disables an app's event subscriptions when
more than 95 % of deliveries fail inside a 60-minute window. So the endpoint deliberately reports
success for a press it could not serve, and **this counter is the only evidence anywhere that it
happened**. Without it the path is back to its original defect — an authentic press, a tick shown to
the user, and no record at all.

It carries **no labels on purpose**. The obvious label is `action_id`, whose values come from a
payload oto does not author; an unknown id is by definition outside oto's closed set, so labelling
by it hands an unbounded key space to Prometheus. The id is on the WARN log beside the increment,
which is where high-cardinality detail belongs.

## What a non-zero value means

Exactly one of two things, and both are bugs in oto:

1. **A card rendered an `action_id` the consumer does not switch on** — the renderer and the router
   disagree.
2. **An `action_id` was renamed**, which retroactively breaks every card already sitting in Slack.
   Cards are durable; a rename is a wire-contract break for every message already posted, forever.

The user-visible effect is worse than an error: the person sees the interaction accepted and
believes they acknowledged the alert. They did not.

## What to check

1. The WARN log beside the increment — it carries `action_id`, `slack_team_id`, `slack_channel_id`.
   The `action_id` string is the whole diagnosis.
2. Compare it with the constant block in `internal/channels/service/interactions.go`
   (`ActionAcknowledge`, `ActionUnacknowledge`, `ActionNoopPrefix`) and with what
   `internal/channels/render/slack/` actually emits.
3. Did it start at a deploy? `GET /api/v1/version`. A rename lands with a release.
4. Whether the increment came from the HTTP path or the `slack.interaction` worker — both record
   it, because from an operator's chair it is the same fact.

## What to do

- **Renamed id**: restore the old id and keep serving it. Cards already in Slack cannot be
  re-rendered, so the router must accept both the old and the new id indefinitely — the rename is
  not complete when the code ships, it is complete when the last card carrying the old id has
  aged out.
- **Renderer/router mismatch**: add the branch, or stop rendering the button. A card carrying an
  Acknowledge nobody consumes is worse than a card with no button at all.
- Tell the affected people their acknowledgement did not land — oto answered 200 and they have no
  way to know.
