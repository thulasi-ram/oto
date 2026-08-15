---
title: 0010 — Server-Sent Events with durable resume, not WebSocket
---
**Status:** Accepted · 2026-08-07

## Context
The alert wall must be live. The traffic is entirely server→client: every client→server action
already has a REST endpoint, and Slack interactivity arrives over webhooks or Socket Mode. The
real requirement is not "push" — it is **correctness after a gap**. An engineer's laptop sleeps
through an incident and must wake into a correct UI, not a stale one.

## Decision
One endpoint: `GET /api/v1/stream`, `text/event-stream`, with `Last-Event-ID` resume backed by a
durable `ui_events` table.

- `ui_events.seq` is a monotonic `BIGSERIAL` and is the SSE event id. On reconnect the browser
  sends `Last-Event-ID: N` **automatically**; the server replays `seq > N` from the last 24 hours,
  then attaches live.
- If the gap exceeds 10 000 rows or falls outside the retention window, the server sends a single
  `resync` frame and the client refetches.
- Fan-out: each API pod holds one `pgx` connection issuing `LISTEN oto_events`. Writers
  `NOTIFY oto_events, '<org_id>:<seq>'` **in the same transaction** that wrote `ui_events`. The
  payload is deliberately tiny — the 8 kB NOTIFY limit is a trap; the hub re-reads the rows.
- Coalescing: at most one frame per connection per 250 ms per `(kind, resource_id)`.
- Backpressure: a bounded ring buffer per connection; on overflow, drop and send `resync`.
  **Never block a writer for a reader.**
- Fallback: every stream-fed list endpoint also accepts `?since_seq=` for pure polling, used when
  a corporate proxy kills SSE.

## Consequences
- SSE is plain HTTP: it inherits the existing auth middleware, proxies, tracing and metrics, and
  `EventSource` is five lines in the browser. There is no second serialisation path and no custom
  reconnect logic.
- **Resume is free and correct.** WebSocket gives none of this without building a replay log
  anyway — at which point the socket buys nothing.
- `ui_events` is an hourly-partitioned table with 24-hour retention, and one more writer
  obligation inside domain transactions.
- Solid's fine-grained reactivity means an SSE-driven store update re-renders exactly one table
  row. That is Solid's superpower and the reason it is in the stack.
- Client→server subscription changes require a reconnect with different query parameters. Acceptable.

## Alternatives rejected
- **WebSocket:** bidirectional capability we do not need, plus connection state, heartbeats,
  custom reconnect and a second serialisation path. It becomes necessary only for collaborative
  editing or high-frequency client-initiated subscriptions — at which point it is an *additive*
  endpoint, not a rewrite.
- **Long polling:** more requests, worse latency, same amount of code.
- **Polling only:** simpler, and visibly worse for a live alert wall. Retained as the fallback, not the default.
