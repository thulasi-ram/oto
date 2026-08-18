# Burst load results

These are the numbers from `test/load`, the package that pushes a real
Alertmanager burst through the real router, the real Postgres, the real River
workers and a conforming fake Slack.

> ⛔ **EVERY NUMBER BELOW WAS MEASURED BEFORE ADR 0042 AND IS NOT REPRODUCIBLE.**
> Storm damping is removed. The four columns and rows that name it — *storm
> notices in channel*, `storm` notifications, *groups in storm mode*, *channels
> carrying a `storm_notice_at` latch* — describe a feature that no longer exists,
> and the code that produced them is gone: `alert_groups.storm_mode`,
> `alert_groups.storm_since` and `channels.storm_notice_at` are dropped columns
> and `storm` is not an admissible `notifications.reason`. They are **left in
> place as the historical record of a real run**, because transcribing invented
> numbers over a measurement is worse than dating it. The tests were renamed
> `TestStorm*` → `TestBurst*` and repointed; the next run replaces this file, and
> the structural numbers it exists for — Slack calls per alert, rollups per group,
> nothing lost, nothing wedged — were never bought by damping and should land in
> the same place.

> ⛔ **NOTHING IN THIS FILE IS A CONTRACT.** Every duration here is a property of
> the machine it was taken on, not of oto. The hard assertions live in
> `assertBurstInvariants` and **not one of them reads a clock**. This file exists
> so that a *structural* regression — a rollup that went back to being
> O(alerts), a reply that started broadcasting into the channel, a delivery that
> went missing — shows up as a diff in a review rather than as a feeling in an
> incident.

## How to reproduce

The cases are behind the `load` build tag, so `go test ./...` does not even
compile them in. `harness.New` additionally skips under `-short`. Both belts are
fastened deliberately: `just test` runs `go test -race -count=1 ./...` with no
`-short`, so a `testing.Short()` guard **alone** would have made these
minutes-long cases CI-blocking, which is how an expensive test gets deleted
rather than fixed.

```sh
DOCKER_HOST="unix://$HOME/.colima/default/docker.sock" \
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
OTO_LOAD_RESULTS=/tmp/oto-load.json \
  go test -tags load ./test/load/ -run TestBurst -v -timeout 45m
```

`OTO_LOAD_RESULTS` is optional; without it the numbers are only logged.
`OTO_LOAD_LOG=warn` turns the container's own logger back on.

⚠️ The JSON file is **appended to**, not overwritten, so point it at a scratch
path rather than at a checked-in file. **This document is the checked-in
artefact**; every number below was transcribed from one run of that command.

## The machine these numbers came from

⚠️ **Read this before comparing anything.**

| | |
|---|---|
| Host | Apple silicon (`darwin/arm64`) |
| Container runtime | colima VM: **2 CPUs, 4 GiB RAM**, `aarch64` |
| Postgres | `postgres:17-alpine`, in that same 2-CPU VM |
| Go | 1.26.5 |
| Concurrency | **the VM was under other load during the run** |
| Date | 2026-08-11 |

Two CPUs is the whole budget for Postgres, the River worker pool, the HTTP
server and the test process *together*. Latencies on a real deployment should be
better than these; the point of stating the constraint is that these numbers are
a **floor with a known cause**, not a target.

The sustained case is deliberately modest for the same reason. A load case that
cannot finish on the machine a contributor actually has is a load case nobody
runs.

## Summary

| Case | Alerts | Batches | Groups | Status codes | Slack calls | Slack/alert | Rollups per group | Storm notices in channel | Lost | Duplicated | Wedged |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `single_batch_500` | 500 | 1 | 1 | 1×202 | 4 | 0.8 % | **2** | 0 (folded into root card) | 0 | 0 | no |
| `chunked_batch_2200` | 2 200 | 1 | 1 | 1×202 | 8 | 0.4 % | **6** | 0 (folded into root card) | 0 | 0 | no |
| `sustained` | 3 120 | 36 | 6 | 36×202, **3×503** | 66 | 2.1 % | **7** | **1** (of 6 storming groups) | 0 | 0 | no |
| `shedding_burst` | 1 920 | 48 | 1 | 48×202, **251×503**, **zero 4xx** | 51 | 2.7 % | **49** | 0 (folded into root card) | 0 | 0 | no |

**The headline: oto's own chatter is 0.4 %–2.7 % of the alert volume.** A
receiver that posted per alert would sit at 100 %.

**7 740 alerts were pushed across the four cases. None was lost, none was
delivered twice, and nothing wedged.**

---

## `single_batch_500` — the case the issue names

500 alerts in ONE Alertmanager notification. Exactly B17's `ChunkSize`, so it is
one processing transaction — the shape a real node failure produces. (One
transaction because 500 is the chunk size, not because it is under
`ChunkThreshold`: chunking is unconditional and the threshold governs only
`partial` marking.)

| Measure | Value |
|---|---|
| Ingest latency (single attempt) | **23.9 ms** |
| Status codes | `202` × 1 |
| Push / drain | 0.02 s / **23.2 s** |
| `alerts` rows | 500 (+ 0 rejections) |
| `alert_groups` | 1, `alert_group_members` 500 |
| **`group.upserted` per group** | **2** — for 500 alerts (**0.4 %**) |
| Notifications | `fired` 2, `storm` 1 |
| Deliveries | 4, all `sent` |
| **Slack calls** | **4** (1 `chat.postMessage`, 3 `chat.update`) |
| Slack roots / threads | 1 / 1 |
| Broadcast replies | 0 |
| Groups in storm mode | 1 |
| Threads with head at end | 1 of 1 |
| Jobs | `enrich.run` 500, `notify.evaluate` 1 001, `deliver.dispatch` 4 |

⭐ **This is the number the issue was really about.** Five hundred alerts produced
**two** rollups and **four** Slack calls. Under the O(n) rollup defect that
`grouping/service.JoinMany` fixed, `rollup_publishes_per_group` here would read
~500 — 500 full aggregates and 500 compare-and-set writes serialising on one
`alert_groups` row, at exactly the moment Alertmanager's retry budget is running
out. The load case reproduces the unit test at
`internal/grouping/service/joinmany_test.go` against a real database.

⚠️ **Why 0 storm notices is correct here.** When 500 alerts arrive in one batch,
the storm transition and the group's first notification are minted in the *same*
ingest transaction, so the storm evaluation runs before any root card exists.
§H.6 then answers `post_root` and drops the reply as `fresh_root` — the channel
learns about the storm from the root card itself, which already says
"Storm — N alerts in this group". Requiring a broadcast here would be asserting a
race, which is why only the sustained case demands one.

---

## `chunked_batch_2200` — B17 chunking, a path that had never been run

2 200 alerts in one notification: over B17's 2 000 threshold, so `applyChunks`
splits it into ceil(2200/500) = **5 transactions** and marks the batch `partial`
until the last commits. A batch that ends its life at `partial` is an accepted
202 whose alerts were never applied.

| Measure | Value |
|---|---|
| Ingest latency (single attempt) | **52.0 ms** |
| Status codes | `202` × 1 |
| Push / drain | 0.05 s / **91.5 s** |
| Final batch status | **`processed`** (not stuck at `partial`) |
| `alerts` rows | 2 200 (+ 0 rejections) |
| **`group.upserted` per group** | **6** — 5 chunks + 1 opening (**0.27 %**) |
| Notifications | `fired` 6, `storm` 1 |
| Deliveries | 8, all `sent` |
| **Slack calls** | **8** (1 `chat.postMessage`, 7 `chat.update`) |
| Slack roots / threads | 1 / 1 |
| Threads with head at end | 1 of 1 |
| Jobs | `enrich.run` 2 200, `notify.evaluate` 4 401, `deliver.dispatch` 8 |

One rollup per chunk is the **correct** shape: each chunk is its own transaction
and its own `JoinMany`. That is O(chunks), still not O(alerts).

---

## `sustained` — several thousand alerts across several minutes

⭐ **The case that makes "exactly one storm notice per channel" falsifiable.**
3 120 alerts into **one Slack channel** across **6 groups**: one quiet opening
wave of 20 per group (below the §D.1 storm threshold of 25) that is allowed to
settle so every root card lands first, then 5 storm waves of 100 per group, 12 s
apart. Only when every group's root card has *already* landed does §H.6 answer
the `storm` transition with a reply — and only then are six broadcasts genuinely
on the table for the latch to suppress.

| Measure | Value |
|---|---|
| Ingest latency | **p50 14.1 ms · p90 33.2 ms · p99 34.8 ms · max 35.1 ms** |
| Status codes | `202` × 36, **`503` × 3** |
| Push / drain | 59.7 s / 69.9 s |
| **`Retry-After` oto asked for** | **10 s**, three times |
| Sheds recovered by retry | **3 of 3** — `permanently_refused: []` |
| Batches | 36, all `processed` |
| `alerts` rows | 3 120 (+ 0 rejections), 3 120 members, 6 groups |
| **`group.upserted` per group** | **7 each** for ~520 alerts each (**1.35 %**) |
| Notifications | `fired` 12, `new_alerts` 36, **`storm` 6** |
| Deliveries | **66, all `sent`** — `update_root` 54, `post_root` 6, `thread_reply` 5, `broadcast_reply` **1** |
| **Slack calls** | **66** (12 `chat.postMessage`, 54 `chat.update`) — **2.1 % of 3 120** |
| Slack roots / threads | **6 / 6** |
| **Broadcast replies (storm notices in channel)** | **1** |
| Groups in storm mode | **6** |
| Channels carrying a `storm_notice_at` latch | **1** |
| Threads with head at end | **6 of 6** — nothing wedged |
| `delivery.skipped` events | 0 |
| Jobs | `enrich.run` 3 120, `notify.evaluate` 6 246, `deliver.dispatch` 66 |

### What this case proves

- ⭐ **Exactly one storm notice per channel.** Six generations entered storm mode
  and six `storm` notifications were recorded — one on each group's own thread, so
  the record is complete per §B.6 — but **exactly one** surfaced in-channel as a
  `broadcast_reply`, and exactly one channel carries the `storm_notice_at` latch.
  Six broadcasts into one channel would have been the flood the damper exists to
  prevent.
- ⭐ **No delivery lost or duplicated.** 66 accepted Slack writes against 66
  `sent` rows — compared directly, in both directions. A message in the channel
  with no row behind it, or a row claiming a message never sent, would both show
  up here.
- ⭐ **The ordering gate did not wedge.** All 6 threads finished with
  `last_sent_seq = next_seq - 1`: the head walked to the end of every queue with
  66 deliveries behind it.
- ⭐ **The O(groups) property holds end to end.** 7 rollups per group against ~520
  members each. Under the old code this would read ~520.

### ⚠️ The shedder fires on this machine, with DEFAULT configuration

This case passes **no config tweak** — default pool, default 500 ms
`ingest.acquire_timeout` — and oto still answered **3 × 503**. The shed path is
not theoretical at modest scale, which is exactly what this issue suspected
nobody knew. Every one was `202` on retry and no alert was lost.

### ⚠️ The finding that was a bug in the *driver*, not in oto

oto answered each of those three sheds with **`Retry-After: 10`**. The first run
of this package used a driver whose *entire* retry budget was six attempts 250 ms
apart — **1.5 s against a 10 s ask** — and it duly reported "2 batches
permanently refused, alerts lost". That was never oto shedding into oblivion; it
was an upstream ignoring the deadline it had been handed.

That shape is worth keeping, because it is **Alertmanager's ~5-minute retry
budget in miniature** (ADR 0007). The driver now honours `Retry-After`, records
what oto asked for (`retry_after_seconds_asked_by_oto`), counts where it capped
the wait (`retry_after_capped_by_driver`: 3 — it waits at most 2 s and retries
again rather than sleeping the full 10 s), and **asserts** that its own budget
(38 s) exceeds the largest ask (10 s). If that assertion ever fails, any reported
loss is the driver's fault and the message says so.

---

## `shedding_burst` — the shedder, reached on purpose

48 concurrent batches of 40 alerts against a **4-connection ingest pool** with a
**1 ms acquisition budget**. The gate is reached by *configuration*, not by
volume: `ingest.acquire_timeout` (§G.10) is the documented knob an operator would
lower during an incident. The claim proved is not "oto sheds at N requests per
second" — that is a property of the machine — but **"when oto sheds it answers
503 with a `Retry-After`, never a 4xx, and an Alertmanager-shaped retry recovers
every alert"**.

| Measure | Value |
|---|---|
| Ingest latency | **p50 3.7 ms · p90 21.8 ms · p99 102.4 ms · max 108.7 ms** |
| Status codes | `202` × 48, **`503` × 251** |
| **4xx of any kind** | **0** |
| **429 specifically** | **0** |
| **`Retry-After` oto asked for** | **10 s**, 251 times |
| Sheds recovered by retry | **251 of 251** — `permanently_refused: []` |
| Driver capped the wait | 251 times (2 s instead of 10 s), 502 s of sleep across 48 goroutines |
| Push / drain | 22.2 s / 58.2 s |
| Batches | 48, all `processed` |
| `alerts` rows | 1 920 (+ 0 rejections), 1 920 members, 1 group |
| **`group.upserted` per group** | **49** — 48 batches + 1 opening (**2.55 %**) |
| Notifications | `fired` 1, `new_alerts` 49, `storm` 1 |
| Deliveries | 51, all `sent` |
| **Slack calls** | **51** (1 `chat.postMessage`, 50 `chat.update`) |
| Slack roots / threads | 1 / 1 |
| Threads with head at end | 1 of 1 |
| Jobs | `enrich.run` 1 920, `notify.evaluate` 3 841, `deliver.dispatch` 51 |

### What this case proves

- ⛔ **Not one 4xx, under 251 sheds.** This is the assertion the whole endpoint is
  built around. Alertmanager retries 5xx and **only** 5xx; a 4xx makes it delete
  the notification permanently and silently, during exactly the window when the
  customer's cluster is on fire (ADR 0007). A 429 would be a 4xx. There were none.
- ⭐ **A 503 is a promise, and the promise was kept 251 times.** Every shed batch
  came back and was accepted; 1 920 of 1 920 alerts landed.
- **49 rollups for 1 920 alerts.** One per batch plus the opening — O(batches ×
  groups), not O(alerts). 48 batches into one group is the worst case for the
  defect this issue is adjacent to, and it still sits at 2.55 %.

⚠️ The audit that raised this issue observed "40 requests, 40 × 202, no 429" and
could not tell a working shedder from an absent one, **because it never reached
the gate**. It is reached here.

---

## Things worth watching that are NOT yet asserted

These are recorded, not enforced. They are noted so a future reader has the same
information the run produced.

- **The job queue is still O(alerts) even though the rollup is not.**
  `enrich.run` is exactly one job per alert (500 / 2 200 / 3 120 / 1 920) and
  `notify.evaluate` runs at roughly **2n** (1 001 / 4 401 / 6 246 / 3 841). The
  fix in `grouping/service.JoinMany` made the *rollup and storm evaluation*
  O(groups); it did not, and was not meant to, make enrichment or notification
  evaluation per-batch. On the 2-CPU VM this is what dominates drain time — 2 200
  alerts took 91 s to settle almost entirely in these two kinds. If ingest
  latency is ever reported as good while bursts still feel slow, this is where to
  look first.
- **Drain time scales with alert count, not with Slack traffic.** 23 s for 500,
  91 s for 2 200. Slack calls stayed at 4 and 8.
- **`update_root` dominates every case** (2 / 6 / 54 / 50). Amend-in-place is what
  keeps the Slack call count flat; each new wave edits the existing card rather
  than posting. ⭐ This was previously credited to "storm collapse plus
  amend-in-place". It was never storm collapse: a notification is minted per
  triggering change per group, so the count follows batch arrivals and not alert
  volume, which is why ADR 0042 removed damping without moving these numbers.
- **`notification_deliveries.mode` is the mode decided at fan-out, and the
  dispatcher re-derives it at claim time without writing it back.** A row saying
  `post_root` can legitimately have been sent with `chat.update` because the root
  landed in between — visible in `single_batch_500`, where 2 `post_root` rows
  produced 1 `chat.postMessage` and the rest `chat.update`. This is why the
  lost/duplicated comparison is deliberately **verb-agnostic**; splitting it by
  verb reports a phantom off-by-one.

## What is asserted (and never varies)

From `assertBurstInvariants` — **not one of these reads a clock**:

1. **Nothing silently lost.** No batch permanently refused; every offered alert
   accepted; `alerts` rows + recorded per-alert rejections = alerts pushed; no
   batch stuck at `pending`/`partial`/`failed`.
2. **The driver is not the thing losing alerts.** Its retry budget must exceed the
   largest `Retry-After` oto asked for.
3. **One group per §C.4 identity**, and every alert a member of one.
4. **The O(groups) property**, twice over: an absolute bound on `group.upserted`
   per group, and a ratio (< 20 % of member count once a group exceeds 200).
5. **Nothing broadcast out of a thread, at any volume** — `slack_broadcast_replies`
   must be **0**. This replaced "exactly one storm notice per channel" at ADR 0042
   and is strictly stronger: the storm notice was the only thing a burst ever
   surfaced in-channel, and with damping gone there is nothing to announce.
6. **No delivery lost or duplicated** — accepted Slack writes compared directly
   against `sent` rows, in both directions; no dead-lettered deliveries; nothing
   left `pending`/`sending`; and the conforming fake refused nothing.
7. **One root per thread.**
8. **oto's chatter below 10 % of alert volume.**
9. **The ordering gate made progress** — `last_sent_seq = next_seq - 1` on every
   thread, no dead threads.
