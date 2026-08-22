# oto_render_invalid_total

|  |  |
|---|---|
| Type | counter |
| Labels | `provider` — `slack`, `webhook` · `renderer` — the renderer id from the channel row · `mode` — `post_root`, `update_root`, `thread_reply`, `broadcast_reply` |
| Registered in | `internal/notification/service/metrics.go`; incremented in `internal/notification/service/dispatch.go` |
| Alertable | **YES — page on it.** The code says: *this counter is an alert, not a statistic, and it is the only one this failure has* |

## What it counts

Deliveries that died because oto could **not build a legal payload** for them: the renderer returned
an error, or the payload its own §L.6 outbound checks (V0–V18) refused.

## Why every single increment matters

**Every one is an oto bug, never the destination's.** Nobody was told, and no provider was ever
called — the message did not fail to send, it failed to exist. The delivery is marked `dead` with
`error_class='config_invalid'` in the same transaction, and the offending bytes are kept in
`notification_deliveries.rendered` so the card can be reconstructed exactly.

## ⚠️ Why this counter exists at all, when `oto_jobs_dead_total` looks like it should cover it

It does not cover it, and for a long time both this repository's SPEC and three of its own code
comments said it did. The chain was checked and it is broken in two places:

1. **The job succeeds.** `dispatch.go` marks the delivery dead in the same transaction and then
   reports SUCCESS, correctly — the job was asked to resolve a delivery and it did. So the River
   dead-letter is never reached and `oto_jobs_dead_total` stays flat while cards die.
2. **Propagating the error would not fix it.** A render error has no case in `jobs.Classify` and
   falls through to *retryable*, so River would wake a job whose delivery is already resolved; the
   second pass exits at `Status.Resolved()` and succeeds. One extra wake-up, no dead-letter, no
   information.

And even where `oto_jobs_dead_total` does fire, it cannot tell **"oto built an illegal card"** from
**"the destination's token was revoked"**. One is fixed by shipping a new oto and one by a customer
editing their channel config; they want different people woken, so they need different counters.

## ⛔ There is no `check` label

V0–V18 is a bounded set and would make a legal label, and it is still refused. The check that
rejected a payload is a Slack concept, `dispatch.go`'s own header forbids provider-specific code
there, and `render/slack/validate.go` refuses the same label from the other side. The check name
reaches an operator through the log line and through the dead delivery's stored error, sitting next
to the bytes that failed — which is more useful than a series, because you can read the card.

## What to check

1. The ERROR log record, which carries the check name inside the error string:
   ```
   slack render invalid (V14): top-level text is 412 chars, limit 300
   delivery_id= channel_id= thread_id= provider= renderer= mode=
   ```
2. `notification_deliveries` for `delivery_id`: `status='dead'`, `error_class='config_invalid'`, and
   `rendered` holding the payload that was refused. `GET /api/v1/deliveries/{id}` serves it.
3. Which `mode` is failing. A failure only on `update_root` and not on `post_root` points at the
   §H.4 strikethrough path or at a card that has grown past a budget as its trail lengthened.
4. Whether it correlates with one `renderer`. Both renderers validate their own output, and only one
   of them is likely to be at fault.

## What to do

- **Look at the stored payload first.** It is the whole bug, kept deliberately for this reason, and
  the check name tells you which rule it broke.
- A **customer NotificationTemplate is the FIRST thing to check** (ADR 0050), and this is a reversal
  from the retired Wording design. A Wording emitted text into a block Go built, so structural
  validation was unreachable by construction. A template owns the blocks: a `card` renders through
  oto's own IR compiler and a `raw` template supplies Block Kit directly. Both are caught here rather
  than by Slack, and both fall back to oto's built-in card — so an increment means an alert went out
  in the DEFAULT voice, not that one was lost. Read `notification_deliveries.template_id` on the
  affected rows first. If an increment coincides with a new template, the bug is in the
  sink or in a budget, not in the template — and that is worth knowing precisely because it looks
  like the template.
- A sudden onset after a deploy is a rendering regression: the golden files
  (`internal/channels/render/*/testdata/*.golden.json`) run `Validate` over every fixture in CI, so
  a failure here is a shape that no fixture covers. Add the fixture.
- The delivery is terminal and will not retry itself. Once the bug is fixed, `POST
  /api/v1/deliveries/{id}/retry` is what re-sends it.
