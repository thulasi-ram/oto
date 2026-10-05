-- ADR 0056 §2–§4: A CASE THE UPSTREAM STOPPED SPEAKING ABOUT EXPIRES, AND SAYS WHY
-- (owner ruling, 2026-10-05). A Case ends only by an upstream resolve or by the
-- reaper, and the reaper missed two common shapes, so their Cases stayed open forever:
--
--   1. NO UPSTREAM END TIME. Alertmanager zeroes `endsAt` on a firing webhook, so a
--      Case known only from webhooks never carries `source_ends_at` and is never a
--      `timeout` candidate. It now expires as `silent` once its HEALTHY source has said
--      nothing about it for longer than that source's `max_silence_s`.
--   2. A DELETED SOURCE. `caseSourcesSQL` finds no live source for the Case and the
--      §B.4 guard holds it, forever. It now expires as `source_removed` once no live
--      source feeds the Case's cluster.
--
-- ⭐⭐ BOTH ARE `expired`, NEVER `resolved` (ADR 0056 §4). The four alert states are
-- unchanged: `upstream` is resolved and `timeout`, `silent` and `source_removed` are
-- expired. `case_resreason_ck` widens from two values to four and nothing else about
-- the closed half moves -- `case_resolve_ck` still makes the reason present exactly
-- when closed, so the derivation in SPEC §B.2 stays total.
--
-- ⛔ NO PERSON ENDS A CASE. Nothing here is a "manual" reason, and there is no column
-- a human writes that closes anything: `max_silence_s` is configuration, read only by
-- the reaper, and deleting a source is an act on configuration (CONTEXT.md's door "No
-- human writes a signal's `state`"; SCOPE-BOUNDARY verdict #34).
--
-- ⭐ `max_silence_s` DEFAULTS TO A DAY, AND EVERY EXISTING SOURCE TAKES THE DEFAULT.
-- Alertmanager repeats a firing notification every `repeat_interval` (4h unless set),
-- so a day of silence under a healthy source means upstream stopped speaking about the
-- alert. That includes the stale Cases this ADR exists for: on the first reaper tick
-- after this migration, open Cases that have been silent for a day expire, at most
-- `DefaultSweepLimit` per tick. NULL turns the expiry off for one source. The bounds
-- are an hour (below Alertmanager's own default repeat, so a mistyped minute count is
-- refused rather than expiring every live Case) and thirty days.
--
-- ⚠️ AN ORG WHOSE `repeat_interval` EXCEEDS ITS `max_silence_s` MUST RAISE IT, or its
-- long-firing Cases expire while still firing. The source settings say so next to the
-- field (ADR 0056 Consequences).
--
-- EXPAND/CONTRACT (CONTEXT.md §6). A defaulted, nullable column with a CHECK every row
-- satisfies, a widened CHECK, and an index. A release-N pod never names the column and
-- never writes either new reason, so it runs unchanged against this schema; it would
-- read a `silent` row's reason as an unknown enum value, which is why the Down below
-- rewrites them rather than leaving them for that release to meet.

-- +goose Up

ALTER TABLE alert_cases DROP CONSTRAINT case_resreason_ck;

ALTER TABLE alert_cases ADD CONSTRAINT case_resreason_ck
  CHECK (resolve_reason IS NULL
         OR resolve_reason IN ('upstream', 'timeout', 'silent', 'source_removed'));

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_reason IS
  'Why the episode closed. upstream: an explicit status=resolved arrived, and it is the ONLY resolution. timeout: source_ends_at + resolve_grace passed under a healthy source. silent: a healthy source said nothing about it for longer than its max_silence_s (ADR 0056 section 3). source_removed: no live source feeds its cluster any more (ADR 0056 section 2). The last three are all expired. Since ADR 0040 this is the SOLE record of resolved-versus-expired on a Case, and case_resolve_ck guarantees a closed episode says why.';
-- +goose StatementEnd

ALTER TABLE alert_sources ADD COLUMN max_silence_s INT DEFAULT 86400;

ALTER TABLE alert_sources ADD CONSTRAINT alert_sources_silence_ck
  CHECK (max_silence_s IS NULL OR max_silence_s BETWEEN 3600 AND 2592000);

-- +goose StatementBegin
COMMENT ON COLUMN alert_sources.max_silence_s IS
  'How long, in seconds, this source may say nothing about an open Case before the reaper expires it as silent (ADR 0056 section 3). Default one day; NULL turns it off. Asked only while the source is healthy: under an unhealthy one oto cannot tell silence from an outage, so the Case is held (SPEC B.4). Must exceed the source Alertmanager repeat_interval, or long-firing Cases expire while still firing.';
-- +goose StatementEnd

-- The reaper's two new scans walk OPEN episodes oldest-heard-first: `silent` stops
-- reading where `last_observed_at` stops being old enough, and `source_removed` reads
-- the same population for the anti-join against live sources. Partial, so the index
-- holds only open episodes, and it excludes a pending close because neither scan may
-- end an episode holding an upstream resolve (00057).
CREATE INDEX case_silence_idx ON alert_cases (org_id, last_observed_at)
  WHERE ended_at IS NULL AND resolve_pending_at IS NULL;

-- +goose StatementBegin
COMMENT ON INDEX case_silence_idx IS
  'The case.reap scans for ADR 0056: open episodes without a pending close, in the order they were last heard about. Read by the silent scan (last_observed_at against the source max_silence_s) and the source_removed scan (open episodes whose cluster has no live source).';
-- +goose StatementEnd

-- +goose Down

-- ⚠️ THE DOWN NARROWS AN ENUM, SO IT REWRITES THE ROWS THE WIDER ONE ADMITTED. A
-- `silent` or `source_removed` Case is EXPIRED, and the release below spells expired
-- exactly one way, so they become `timeout`: the state survives and only the specific
-- reason is lost. The event that closed each one keeps it in its payload, which this
-- Down does not touch. Deleting the rows, or leaving them to fail the CHECK, are the
-- two alternatives, and the first erases history while the second aborts the rollback.
UPDATE alert_cases SET resolve_reason = 'timeout'
 WHERE resolve_reason IN ('silent', 'source_removed');

DROP INDEX IF EXISTS case_silence_idx;

ALTER TABLE alert_sources DROP CONSTRAINT IF EXISTS alert_sources_silence_ck;
ALTER TABLE alert_sources DROP COLUMN IF EXISTS max_silence_s;

ALTER TABLE alert_cases DROP CONSTRAINT case_resreason_ck;

ALTER TABLE alert_cases ADD CONSTRAINT case_resreason_ck
  CHECK (resolve_reason IS NULL OR resolve_reason IN ('upstream', 'timeout'));

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_reason IS
  'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). Since ADR 0040 this is the SOLE record of resolved-versus-expired on a Case: state says only that the episode closed, and case_resolve_ck guarantees a closed episode says why. oto can still never claim resolved when it means expired, because it can no longer claim resolved at all without this column saying upstream.';
-- +goose StatementEnd
