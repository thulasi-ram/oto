# oto_jobs_unknown_version_total

|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue` |
| Registered in | `internal/platform/jobs/metrics.go`; gated in `internal/platform/jobs/worker.go` |
| Alertable | **YES — page on it.** The help string says `ALERT ON THIS` |
| Rule | `OtoJobUnknownPayloadVersion` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Jobs parked because their payload version is **newer than this worker understands** (SPEC §G.3).
The check runs before the handler and before any timing, so the job is never half-processed. It is
cancelled with `dead_reason="unknown_payload_version"` and also counted on
[`oto_jobs_dead_total`](oto_jobs_dead_total.md) with `class="permanent"`.

Parking rather than guessing is deliberate: a worker that interpreted a payload shape it does not
know would do the wrong thing durably.

## What a non-zero value means

Two binaries are disagreeing about a payload, which in practice means exactly one thing:

**a newer pod enqueued the job and an older pod picked it up.** Either a rolling deploy is in
flight (transient, and it should stop when the last old pod goes), or a **rollback** left old
binaries holding jobs the new one wrote (not transient — those jobs are stranded and nobody is
coming for them).

## What to check

1. Are the versions mixed? `GET /api/v1/version` on each pod — `version`, `commit`, and
   `schema_version`.
2. Is the counter still climbing after the rollout finished? Then it is a rollback, not a rollout.
3. `river_job` for that `kind` in `cancelled`, and the parked payloads in the log:
   ```
   msg="jobs: parking job with unknown payload version" payload_version= supported_version=
   ```

## What to do

- **Rolling deploy in flight**: wait. Confirm it stops.
- **Rollback**: roll *forward* to the build that understands the payload and let it drain the
  parked jobs, or replay the payloads from the dead-letter log against that build. Nothing else
  will run them.
- **Neither**: you have a worker on an old image that nothing is watching. Find it — that pod is
  eating work.
- Prevention: bump `PayloadVersion` and ship the reader before the writer. The gate makes the
  wrong order loud instead of silent, but it cannot make it safe.
