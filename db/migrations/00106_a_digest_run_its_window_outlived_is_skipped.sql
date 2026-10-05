-- A DIGEST WINDOW'S RUN THAT WAS STILL QUEUED WHEN ITS WINDOW CLOSED ENDS `skipped` WITH
-- REASON `window_closed` (owner ruling O4, 2026-10-05; ADR 0053 §6). A digest never waits
-- on an Investigation: when the window closes the digest goes out with whatever Finding was
-- ready, or with the built-in body. A run still `queued` at that moment — behind the org's
-- concurrency or the Investigator's minimum interval — would spend tokens on a Finding no
-- digest will ever read. It now ends without calling a model, and the row says why:
-- recorded, never silent, and never spent for nothing.
--
-- ⭐ ONE REASON ON ONE STATUS. `window_closed` joins `disabled` and `budget` under
-- `skipped`, because the run never started; it is not `failed` (nothing went wrong) and not
-- `subject_gone` (the policy is still there). The Go constant is
-- `investigator/domain.ReasonWindowClosed`; the CHECK below is what lets its write land.
--
-- The table comment is restated with the new reason, and now says what 00102 made true and
-- never wrote down: a digest window's Finding is NOT an Enrichment — it stays on its run and
-- is copied onto the digest it was ready for.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). A widened CHECK and a comment. Release N-1 never writes
-- the new reason, so it runs against this unchanged.

-- +goose Up

ALTER TABLE investigations DROP CONSTRAINT investigations_reason_ck;
ALTER TABLE investigations ADD CONSTRAINT investigations_reason_ck CHECK (
     (status IN ('queued','running','completed') AND reason IS NULL)
  OR (status = 'exhausted' AND reason IN ('step_budget','token_budget','wall_time_budget'))
  OR (status = 'failed'    AND reason IN ('usage_missing','model_error','model_changed','subject_gone','interrupted','internal'))
  OR (status = 'skipped'   AND reason IN ('disabled','budget','window_closed'))
);

-- +goose StatementBegin
COMMENT ON TABLE investigations IS
  'One run of one Investigator version against one subject (ADR 0053 §1), frozen once it ends: queued, running, then completed, exhausted (a per-run budget was hit; whatever Finding it reached is kept and is partial), failed (usage missing, model error, ...) or skipped (the org or Investigator was disabled, the org''s daily token budget was spent, or a digest window''s run was still queued when its window closed; nothing ran). The reason is recorded, never silent. A queued run waits for the org''s concurrency and, when an interval deferred it, for not_before. The budgets it ran under and the tokens it spent are its own columns. A Case''s or Incident''s Finding is also published as the Enrichment investigator.<name>; a digest window''s stays on its run and is copied onto the digest it was ready for. It is NEVER an input to whether a notification is sent (ADR 0053 §2).';
-- +goose StatementEnd

-- +goose Down

-- ⛔ A `skipped`/`window_closed` ROW HAS NO HOME BELOW THIS MIGRATION, AND IT IS DELETED
-- RATHER THAN RELABELLED, for 00098's reason: calling it `disabled` or `budget` would put a
-- reason on the record that is not why it did not run. It never started, so it has no Steps
-- and no Finding; the delete takes only the record that a window outlived its run.
-- (`investigations_frozen` guards UPDATE, not DELETE.)
DELETE FROM investigations WHERE status = 'skipped' AND reason = 'window_closed';

ALTER TABLE investigations DROP CONSTRAINT investigations_reason_ck;
ALTER TABLE investigations ADD CONSTRAINT investigations_reason_ck CHECK (
     (status IN ('queued','running','completed') AND reason IS NULL)
  OR (status = 'exhausted' AND reason IN ('step_budget','token_budget','wall_time_budget'))
  OR (status = 'failed'    AND reason IN ('usage_missing','model_error','model_changed','subject_gone','interrupted','internal'))
  OR (status = 'skipped'   AND reason IN ('disabled','budget'))
);

-- +goose StatementBegin
COMMENT ON TABLE investigations IS
  'One run of one Investigator version against one subject (ADR 0053 §1), frozen once it ends: queued, running, then completed, exhausted (a per-run budget was hit; whatever Finding it reached is kept and is partial), failed (usage missing, model error, ...) or skipped (the org or Investigator was disabled, or the org''s daily token budget was spent; nothing ran). The reason is recorded, never silent. A queued run waits for the org''s concurrency and, when an interval deferred it, for not_before. The budgets it ran under and the tokens it spent are its own columns. A Finding is also published as the Enrichment investigator.<name>; it is NEVER an input to whether a notification is sent (ADR 0053 §2).';
-- +goose StatementEnd
