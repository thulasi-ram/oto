-- `channels` gives up its own clock: created_at and updated_at lose DEFAULT now().
--
-- ⭐⭐ WHY. `channels_time_ck` is `CHECK (updated_at >= created_at)`. 00011 gave
-- `created_at` a `DEFAULT now()`, which is the DATABASE's clock, while every
-- writer of `updated_at` — ChannelRepository.Update, SoftDelete and the two
-- SetHealth statements — stamps the GO PROCESS's clock. Two machines, two clocks.
-- An app server running even a few milliseconds behind its database turns the
-- FIRST health write on a freshly created channel into a 23514, which surfaces as
-- `internal_error/channels_time_ck` — a 500 on the first delivery, with nothing
-- actually wrong. It reproduced on a Colima VM at roughly one run in two.
--
-- ⭐ THE DECISION IS THAT THE APPLICATION OWNS TIME. oto already has an
-- injectable clock (`internal/platform/clock`) precisely so that time is
-- controllable and testable, and every other timestamp on this table already
-- comes through it. The `DEFAULT now()` was the outlier, not the rule. The two
-- alternatives were considered and rejected: loosening the CHECK throws away the
-- one invariant that makes "when did this destination last change" answerable,
-- and moving `updated_at` onto the database clock would make the health write
-- untestable without a real clock and would put ordering back where two pods
-- cannot reason about it.
--
-- ⛔ WHY DROPPING RATHER THAN LEAVING A DEFAULT NOBODY USES. Because the default
-- is not inert — it is a TRAP with a delayed fuse. Every production INSERT
-- already names both columns (`internal/channels/repository/channels.go` is the
-- only one). A default that is never exercised does nothing for the paths that
-- exist; what it does is let a FUTURE writer omit the columns, succeed, and plant
-- a row whose `created_at` came from the wrong clock. That row does not fail at
-- the INSERT, where the mistake is; it fails later, at the first delivery, as a
-- 500 whose stated cause is a CHECK constraint. Without the default the same
-- omission fails immediately and unmistakably, as a not-null violation, in the
-- first test that runs the statement. The safety net catches only writers that
-- are already wrong, and it converts their error into a worse one.
--
-- EXPAND/CONTRACT (CONTEXT.md §6), with N and N+1 running simultaneously.
-- Dropping a DEFAULT is not destructive: no data changes, the columns stay NOT
-- NULL, and every reader is untouched. It is safe in BOTH directions because
-- release N already supplies both values on every INSERT it makes — the sole
-- writer has passed them explicitly since the composition root landed — so an N
-- pod talking to the N+1 schema inserts exactly as it did before. There is
-- nothing to backfill and no contract phase to follow.

-- +goose Up

ALTER TABLE channels ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE channels ALTER COLUMN updated_at DROP DEFAULT;

COMMENT ON COLUMN channels.created_at IS
  'When the application says this destination was configured. NO DEFAULT, ON PURPOSE: it is stamped from the injected Go clock by the inserting repository, never by the database, so that it and updated_at are ordered by ONE clock and channels_time_ck cannot be tripped by a millisecond of skew between an app server and Postgres.';
COMMENT ON COLUMN channels.updated_at IS
  'When the application last changed this row. Stamped from the same injected Go clock as created_at, and advanced monotonically by the writers (GREATEST(updated_at, $n)) so that a pod whose clock lags the pod that wrote the row cannot push it backwards past created_at.';

-- +goose Down

ALTER TABLE channels ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE channels ALTER COLUMN updated_at SET DEFAULT now();
