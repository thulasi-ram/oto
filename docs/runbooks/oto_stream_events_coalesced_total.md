# oto_stream_events_coalesced_total

|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **no — informational, and non-zero is the feature working.** |

## What it counts

Frames superseded within the 250 ms coalescing window: latest wins. When an alert group changes
three times in a quarter of a second, the browser is sent the final state once instead of three
intermediate states it would immediately overwrite.

## Why there is no response

Nothing is lost. A coalesced frame is a frame whose content was replaced by a newer version of the
same thing before it was sent, and the client ends up with the newer version — which is what it
would have rendered anyway.

Worth reading, never worth paging:

- A high coalesce ratio during a storm is the window doing its job and is the reason a storm does
  not melt the browser.
- A coalesce rate that is high *outside* a storm suggests something is rewriting group state in a
  tight loop. That is worth a look in the ingest path, not in streaming.

Do not confuse this with [`oto_stream_events_dropped_total`](oto_stream_events_dropped_total.md).
Coalescing replaces; dropping discards and forces a resync.
