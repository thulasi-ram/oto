# oto_stream_notify_malformed_total

|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes — any increment.** |
| Rule | `OtoStreamNotifyMalformed` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

NOTIFY payloads on the `oto_events` channel that did not parse as `<org_id>:<seq>`.

## What a non-zero value means

Only two things can produce this, and both need a human:

1. **A bug in oto** — the trigger or the code that emits the NOTIFY is writing a shape the bridge
   does not read. Since both sides ship together, this normally appears immediately after a deploy
   or a migration.
2. **A foreign writer on the channel** — something other than oto is issuing
   `NOTIFY oto_events, '…'` against the same database. A stray script, a second application sharing
   the database, or a hand-run `NOTIFY` during debugging.

The consequence is bounded: a malformed doorbell is ignored, and the reconciling poll picks the rows
up anyway, so the UI is late rather than wrong. But it means the doorbell contract is broken, and
the next thing that breaks it may not be so harmless.

## What to check

1. The log line around the increment, which carries the payload that failed to parse.
2. Did it start at a deploy or a migration? `GET /api/v1/version` for the commit and
   `schema_version`; `just status` for migration state.
3. `pg_stat_activity` and your own tooling for anything else connected to this database that might
   be issuing NOTIFY.
4. [`oto_stream_poll_recovered_total`](oto_stream_poll_recovered_total.md) — it should be absorbing
   the missed doorbells. If it is not, events are also not being written.

## What to do

- Deploy-shaped: roll back or roll forward to a build where the emitter and the bridge agree, then
  file it. The payload format is a contract between a database trigger and Go code, and nothing
  mechanically enforces it.
- Foreign writer: stop it. `oto_events` belongs to oto.
