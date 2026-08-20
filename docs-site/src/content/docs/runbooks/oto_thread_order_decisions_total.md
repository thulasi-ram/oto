---
title: oto_thread_order_decisions_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `action`, `reason` |
| Registered in | `internal/platform/jobs/ordering/gate.go` |
| Alertable | **no — diagnostic.** Alert on the two metrics it explains |

## What it counts

Every verdict the ordering gate reaches, which is the complete decision table for "may this delivery
send now". It exists to explain the other two ordering metrics, not to be paged on.

| `action` | `reason` | Meaning |
|---|---|---|
| `proceed` | `in_order` | the head is at this slot; send |
| `wait_for_root` | `awaiting_root` | the thread's root message has not landed yet |
| `wait_for_predecessor` | `awaiting_predecessor` | an earlier slot is still in flight |
| `recover_thread` | `thread_dead` | the thread is terminal and must be recovered |
| `recover_thread` | `root_never_landed` | the root was allocated but never sent |
| `recover_thread` | `head_of_line_stalled` | a slot has been in flight past the §G.5 lease (120 s) |
| `abandon` | `unsequenced` | the item carries no sequence number |
| `abandon` | `unknown_state` | the thread's state is not one the gate recognises; nothing is sent and nothing is acted on |
| `out_of_order` | `already_resolved` | this slot is already finished; the head merely had not caught up |

## How to read it

- `proceed` should dominate overwhelmingly.
- A rise in `wait_for_*` is queueing, and shows up as latency on
  [`oto_thread_head_wait_seconds`](/runbooks/oto_thread_head_wait_seconds/) and as `awaiting_*` snoozes on
  [`oto_jobs_snoozed_total`](/runbooks/oto_jobs_snoozed_total/).
- A rise in `recover_thread` is the gate deciding a thread is stuck; the recovery it performs is
  what increments [`oto_thread_gap_recovered_total`](/runbooks/oto_thread_gap_recovered_total/). That is
  the one to page on.
- `abandon` with `unsequenced` should be zero. Non-zero means a delivery reached the gate without a
  sequence number, which is a bug.

## What to do

Nothing directly. Use it to answer "why" once one of the other two has fired.
