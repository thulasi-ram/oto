-- ⭐ ONE INDEX COMMENT LOSES A CLAUSE THAT NAMES A JOB THAT NO LONGER EXISTS.
--
-- `case_ack_idx` was widened by 00053, and the comment 00053 shipped with it
-- listed two readers: the default view of `GET /api/v1/cases`, and the reminder
-- sweep that used to run beside it. That sweep was withdrawn (git-bug bd0fb1d) --
-- oto sends nothing unprompted, so there is no ladder and no stage that ends at a
-- person. The index is unchanged and still correct; the sentence describing it
-- names a caller that cannot call.
--
-- ⛔ 00053 IS NOT EDITED, AND THAT IS THE RULE RATHER THAN A PREFERENCE. A
-- migration that has run somewhere is history: rewriting it makes the file
-- disagree with the database it already produced, and every deployment past that
-- point silently diverges. 00078/00079 retired the `wordings` design the same way
-- -- new migrations, not a rewrite of 00076/00077 -- and this follows them.
--
-- ⛔ THE COMMENT IS NOT DROPPED, ONLY CORRECTED. `COMMENT ON INDEX` is not a
-- source comment; it is a row in `pg_description` that every operator reading the
-- schema sees. The index's own justification -- why it is partial, and why it
-- carries the full keyset sort key -- is the part worth keeping, and it survives
-- here verbatim.

-- +goose Up

-- +goose StatementBegin
COMMENT ON INDEX case_ack_idx IS
  'Serves the unacked-and-still-open queue -- the default view of GET /api/v1/cases. Partial on ended_at IS NULL so it stays the size of the LIVE case set rather than of every episode ever opened. Carries the whole keyset sort key (started_at DESC, id DESC) so the LIMIT stops the scan and no Sort node appears; before 00053 it stopped at started_at and the id tiebreak cost an Incremental Sort bounded by one Alertmanager batch.';
-- +goose StatementEnd

-- +goose Down

-- Restores 00053's wording verbatim, stale clause included. A Down that
-- "improves" what it rolls back to is not a rollback.
-- +goose StatementBegin
COMMENT ON INDEX case_ack_idx IS
  'Serves the unacked-and-still-open queue -- the default view of GET /api/v1/cases -- and escalation.check (SPEC G.9). Partial on ended_at IS NULL so it stays the size of the LIVE case set rather than of every episode ever opened. Carries the whole keyset sort key (started_at DESC, id DESC) so the LIMIT stops the scan and no Sort node appears; before 00053 it stopped at started_at and the id tiebreak cost an Incremental Sort bounded by one Alertmanager batch.';
-- +goose StatementEnd
