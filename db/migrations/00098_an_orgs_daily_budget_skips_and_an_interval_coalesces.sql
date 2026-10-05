-- ADR 0053 §6: AN ORG'S DAILY BUDGET SKIPS ON THE RECORD, ITS CONCURRENCY WAITS, AND AN
-- INVESTIGATOR'S MINIMUM INTERVAL COALESCES (git-bug bf172fe). 00096 enforced the per-run
-- budgets and the kill switch; this is the rest of §6's table:
--
--   Daily token budget  org             New Investigations are recorded as `skipped` with
--                                       reason `budget`, not queued. Resets at UTC midnight.
--   Concurrency         org             Waits in the job queue; never dropped.
--   Minimum interval    each Investigator  Membership-change triggers inside the interval
--                                       coalesce into one run.
--
-- ⭐ THE TWO ORG CONTROLS ARE SETTINGS, NOT COLUMNS. `investigation_daily_tokens` and
-- `investigation_concurrency` join the `orgs.settings` document beside
-- `investigations_enabled`, with their bounds in `identity/domain` and the contract and an
-- origin like every other key; this migration's only change for them is the comment that
-- states the key set. The minimum interval IS a column, on `investigators` — the mutable
-- half, beside `enabled` and the budgets — because it decides WHEN a run may start, not
-- what produced a Finding, so changing it is not a new version.
--
-- ⭐ THE DAY'S SPEND IS READ FROM THE STEPS, NOT FROM THE RUNS. `investigations.tokens_in`
-- and `tokens_out` are written when a run ends; a model turn's tokens are written on its
-- Step the moment it happens, stamped `recorded_at`. Summing today's model-turn Steps
-- counts the runs still going and puts a run that crosses midnight in the days it spent
-- in. `investigation_steps_spend_idx` serves exactly that read.
--
-- ⭐ CONCURRENCY COUNTS `running` ROWS UNDER AN ADVISORY LOCK. Starting a run takes the
-- org's transaction-scoped advisory lock (`platform/db` LockNamespaceInvestigations),
-- counts the org's `running` rows and moves this one from `queued` to `running` in the
-- same transaction — so two workers cannot both see one free slot and both take it.
-- `investigations_running_idx` serves the count, and is partial so the history is not in it.
--
-- ⭐ `not_before` IS THE INTERVAL ON THE RECORD. A run the interval deferred is `queued`
-- with the earliest time it may start, and its job is scheduled for that moment; every
-- membership change before then finds it queued and coalesces into it. NULL for a run
-- nothing deferred.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6). `min_interval_s` takes a constant
-- DEFAULT so the rows that exist get the shipped ten minutes and release N-1, which does
-- not name the column, can still insert.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One column with a default, one nullable column, a
-- reason CHECK widened by one value release N-1 never writes, two indexes and two comments.

-- +goose Up

ALTER TABLE investigators
  ADD COLUMN min_interval_s INT NOT NULL DEFAULT 600,
  ADD CONSTRAINT investigators_interval_ck CHECK (min_interval_s BETWEEN 0 AND 86400);

-- +goose StatementBegin
COMMENT ON COLUMN investigators.min_interval_s IS
  'ADR 0053 §6 "Minimum interval between runs on one subject": membership-change triggers inside it, measured from when the last run on that subject started, coalesce into one run that starts when the interval is up. 0 runs on every trigger. A human''s request is not held to it. Not versioned: it decides when a run may start, not what produced a Finding.';
-- +goose StatementEnd

ALTER TABLE investigations
  ADD COLUMN not_before TIMESTAMPTZ,
  ADD CONSTRAINT investigations_not_before_ck CHECK (not_before IS NULL OR not_before >= requested_at);

-- +goose StatementBegin
COMMENT ON COLUMN investigations.not_before IS
  'The earliest a queued run may start, when an Investigator''s minimum interval deferred it (ADR 0053 §6); its job is scheduled for the same moment. NULL when nothing deferred it.';
-- +goose StatementEnd

ALTER TABLE investigations DROP CONSTRAINT investigations_reason_ck;
ALTER TABLE investigations ADD CONSTRAINT investigations_reason_ck CHECK (
     (status IN ('queued','running','completed') AND reason IS NULL)
  OR (status = 'exhausted' AND reason IN ('step_budget','token_budget','wall_time_budget'))
  OR (status = 'failed'    AND reason IN ('usage_missing','model_error','model_changed','subject_gone','interrupted','internal'))
  OR (status = 'skipped'   AND reason IN ('disabled','budget'))
);

-- Serves: "the tokens this org has spent since 00:00 UTC" — one index range per request.
CREATE INDEX investigation_steps_spend_idx ON investigation_steps (org_id, recorded_at)
  INCLUDE (tokens_in, tokens_out) WHERE kind = 'model_turn';

-- Serves: "how many runs are running in this org", under the org's advisory lock.
CREATE INDEX investigations_running_idx ON investigations (org_id) WHERE status = 'running';

-- +goose StatementBegin
COMMENT ON TABLE investigations IS
  'One run of one Investigator version against one subject (ADR 0053 §1), frozen once it ends: queued, running, then completed, exhausted (a per-run budget was hit; whatever Finding it reached is kept and is partial), failed (usage missing, model error, ...) or skipped (the org or Investigator was disabled, or the org''s daily token budget was spent; nothing ran). The reason is recorded, never silent. A queued run waits for the org''s concurrency and, when an interval deferred it, for not_before. The budgets it ran under and the tokens it spent are its own columns. A Finding is also published as the Enrichment investigator.<name>; it is NEVER an input to whether a notification is sent (ADR 0053 §2).';
-- +goose StatementEnd

-- `investigation_daily_tokens` and `investigation_concurrency` join the document (ADR 0053
-- §6). JSONB, so no column moves; the comment is the schema's statement of the key set,
-- and it says ten now.
-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine, retention and Investigations, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The ten keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months, default_verbosity, investigations_enabled (ADR 0053 §6, boolean, default true: the org''s Investigation kill switch; absent means enabled), investigation_daily_tokens (ADR 0053 §6, integer 1000..1000000000, default 2000000: input + output tokens per UTC day; past it a new Investigation is recorded skipped with reason budget) and investigation_concurrency (ADR 0053 §6, integer 1..32, default 2: the most Investigations running at once; one past it waits queued). ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to what 00096 shipped. A row that wrote either key keeps it in the
-- document, which the release below reads past: an unknown JSONB key is not a column it
-- can fail on.
-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine, retention and Investigations, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The eight keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months, default_verbosity and investigations_enabled (ADR 0053 §6, boolean, default true: the org''s Investigation kill switch; absent means enabled). ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE investigations IS
  'One run of one Investigator version against one subject (ADR 0053 §1), frozen once it ends: queued, running, then completed, exhausted (a per-run budget was hit; whatever Finding it reached is kept and is partial), failed (usage missing, model error, ...) or skipped (the org or Investigator was disabled; nothing ran). The reason is recorded, never silent. The budgets it ran under and the tokens it spent are its own columns. A Finding is also published as the Enrichment investigator.<name>; it is NEVER an input to whether a notification is sent (ADR 0053 §2).';
-- +goose StatementEnd

DROP INDEX investigations_running_idx;
DROP INDEX investigation_steps_spend_idx;

-- ⛔ A `skipped`/`budget` ROW HAS NO HOME BELOW THIS MIGRATION, AND IT IS DELETED RATHER
-- THAN RELABELLED. Calling it `disabled` would put a reason on the record that is not why
-- it did not run. It never started, so it has no Steps and published no Finding: the
-- delete takes only the record that somebody asked while the day's budget was spent.
-- (`investigations_frozen` guards UPDATE, not DELETE.)
DELETE FROM investigations WHERE status = 'skipped' AND reason = 'budget';

ALTER TABLE investigations DROP CONSTRAINT investigations_reason_ck;
ALTER TABLE investigations ADD CONSTRAINT investigations_reason_ck CHECK (
     (status IN ('queued','running','completed') AND reason IS NULL)
  OR (status = 'exhausted' AND reason IN ('step_budget','token_budget','wall_time_budget'))
  OR (status = 'failed'    AND reason IN ('usage_missing','model_error','model_changed','subject_gone','interrupted','internal'))
  OR (status = 'skipped'   AND reason IN ('disabled'))
);

ALTER TABLE investigations DROP CONSTRAINT investigations_not_before_ck;
ALTER TABLE investigations DROP COLUMN not_before;

ALTER TABLE investigators DROP CONSTRAINT investigators_interval_ck;
ALTER TABLE investigators DROP COLUMN min_interval_s;
