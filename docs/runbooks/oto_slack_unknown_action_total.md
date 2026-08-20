# oto_slack_unknown_action_total

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

The known ids are the four that write — `oto.ack`, `oto.unack`, `oto.snooze`, `oto.unsnooze` — plus
`oto.more`, plus anything under the `oto.noop.` prefix (the URL buttons, which Slack delivers an
interaction for and oto must acknowledge with nothing to do).

> ⭐ **`oto.more` was in NO set at all and every press of it counted here.** It is the links overflow —
> the `:package: More` menu holding Timeline, Prometheus, Alertmanager, Rule history and Show all
> labels — and the renderer has always emitted it (`render/slack/root.go`, pinned by five goldens and
> named in SPEC §H.8). Matching neither the writing ids nor `oto.noop.`, it fell to the router's
> `default` arm and incremented this counter on the one element of the card that was working exactly
> as designed. **It is now admitted to the closed set, deliberately rather than renamed to
> `oto.noop.more`**: cards already sitting in Slack carry the literal `oto.more`, so a rename would
> turn every one of those menus into a silent no-op — the defect relocated, not fixed — and the
> overflow is not link-only anyway (see the gap below). A press of it enqueues nothing, because
> pressing a container of places to look is not a verb. **If you are reading this runbook because of a
> historical spike, `oto.more` is the first thing to rule out.**
>
> ⚠️ **KNOWN GAP — `Show all labels` is routed but inert, and that is NOT what this counter is for.**
> The overflow's URL options are complete: Slack navigates, oto acks, done. The `Show all labels`
> option instead carries a `value` (`labels|<case id>`) that SPEC §H.8 designates for a `views.open`
> modal, **and no modal is built**, so pressing that one option does nothing and says nothing. A press
> of it is **known-and-routed, not unroutable**: it does not and must not increment
> `oto_slack_unknown_action_total`. Conflating the two would put a permanent floor under a series whose
> whole meaning is "oto is broken", and would hide the real renderer/router mismatches this metric
> exists to catch. The inert option is a gap to close in the renderer's own right; it is not an
> unknown action, and an operator seeing zero here while a user reports that `Show all labels` did
> nothing is looking at exactly this gap.

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
   (`ActionAcknowledge`, `ActionUnacknowledge`, `ActionSnooze`, `ActionUnsnooze`, `ActionOverflow`,
   `ActionNoopPrefix`) and with what `internal/channels/render/slack/` actually emits. **The block is
   the closed set; anything the renderer emits that is not in it is this metric's whole subject.**
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
