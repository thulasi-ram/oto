-- `orgs` and `channel_credentials` give up their own clock, the way `channels`
-- did in 00032. `orgs.created_at`, `orgs.updated_at` and
-- `channel_credentials.created_at` lose DEFAULT now().
--
-- ⭐⭐ WHY, FOR `orgs`. `orgs_time_ck` is `CHECK (updated_at >= created_at)`.
-- 00003 gave both columns a `DEFAULT now()`, which is the DATABASE's clock, but
-- the row is not created by the database: `internal/app/bootstrap.go` inserts it
-- and names `created_at` from the GO clock. The one statement that ever moved
-- `updated_at` afterwards — `identity/repository/orgs.go` UpdateSettings — wrote
-- `now()`. Two machines, two clocks, one CHECK across them.
--
-- ⛔ THIS IS THE MIRROR OF 00032 AND IT WAS LIVE. `channels` needed the app
-- server running BEHIND its database; `orgs` needs it running AHEAD. A Go clock
-- a few milliseconds ahead of Postgres turns a settings write made shortly after
-- bootstrap into a 23514, surfacing as `internal_error/orgs_time_ck` — a 500.
-- The window is "shortly after the org row was created", and the person in that
-- window is a brand-new operator opening the settings screen right after first
-- setup, which is precisely what a new user does. Nothing is wrong with the
-- deployment; the two writers simply do not agree about what time it is.
--
-- ⭐ WHY, FOR `channel_credentials`. `channel_credentials_rot_ck` is
-- `CHECK (rotated_at IS NULL OR rotated_at >= created_at)` and has the same
-- shape as `channels`: a DB-defaultable `created_at` against a `rotated_at` that
-- `channels/repository/credentials.go` Rotate stamps from the Go clock. Its
-- severity today is lower only by accident of who writes it — production Create
-- already names `created_at`, so only non-repository inserts (test seeds) could
-- reach the default. That is the same "unreachable" the `channels` default had,
-- and the `channels` default is the one that eventually cost half a day.
--
-- ⭐ THE DECISION IS THAT THE APPLICATION OWNS TIME, restated once for both
-- tables. oto has an injectable clock (`internal/platform/clock`) precisely so
-- that time is controllable and testable, and it is the clock every other
-- timestamp on these rows already comes from. The alternatives were considered
-- and rejected for the same reasons as in 00032: loosening a CHECK throws away
-- the one invariant that makes "when did this change" answerable, and moving
-- more columns onto the database clock makes the writes untestable without a
-- real clock and puts ordering back where N pods cannot reason about it.
--
-- ⛔ WHY DROPPING RATHER THAN LEAVING A DEFAULT NOBODY USES. Because the default
-- is not inert, it is a TRAP with a delayed fuse, and the argument 00032 made is
-- if anything stronger here. Every production INSERT already names the columns:
-- `bootstrap.go` for `orgs`, `credentials.go` Create for `channel_credentials`.
-- A default that is never exercised does nothing for the paths that exist; what
-- it does is let a FUTURE writer omit the column, succeed, and plant a row whose
-- `created_at` came from the wrong clock. That row does not fail at the INSERT,
-- where the mistake is. It fails much later, in `orgs`' case at somebody's first
-- settings change, as a 500 whose stated cause is a CHECK constraint two files
-- away. Without the default the same omission fails at the offending statement,
-- as a not-null violation, in the first test that runs it.
--
-- Three test seeds DID rely on the defaults (`test/harness/builders.go`,
-- `test/integration/am_route_timings_test.go`,
-- `test/integration/slack_acknowledge_test.go` for `orgs`;
-- `slack_acknowledge_test.go` and `delivery_summary_test.go` for the credential)
-- and they are fixed in the same change to stamp the harness clock. A seed that
-- needs the database's clock is a seed writing rows the product cannot write.
--
-- EXPAND/CONTRACT (CONTEXT.md §6), with N and N+1 running simultaneously.
-- Dropping a DEFAULT is not destructive: no data changes, the columns stay NOT
-- NULL, and every reader is untouched. It is safe in BOTH directions because
-- release N already supplies these values on every INSERT it makes — the sole
-- writer of each table has passed them explicitly since those files were written
-- — so an N pod talking to the N+1 schema inserts exactly as it did before.
-- There is nothing to backfill and no contract phase to follow. The Down
-- restores the defaults, which is what keeps a rolled-back release able to write
-- these tables at all.

-- +goose Up

ALTER TABLE orgs ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE orgs ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE channel_credentials ALTER COLUMN created_at DROP DEFAULT;

COMMENT ON COLUMN orgs.created_at IS
  'When the application says this tenant was created. NO DEFAULT, ON PURPOSE: internal/app.Bootstrap stamps it from the injected Go clock, never the database, so that it and updated_at are ordered by ONE clock and orgs_time_ck cannot be tripped by a millisecond of skew between an app server and Postgres.';
COMMENT ON COLUMN orgs.updated_at IS
  'When the application last changed this row. Stamped from the same injected Go clock as created_at, and advanced monotonically by UpdateSettings (GREATEST(updated_at, $n)) so that a pod whose clock lags the pod that bootstrapped the deployment cannot push it backwards past created_at.';
COMMENT ON COLUMN channel_credentials.created_at IS
  'When the application sealed this secret. NO DEFAULT, ON PURPOSE: CredentialRepository.Create stamps it from the injected Go clock, so it and rotated_at are ordered by ONE clock and channel_credentials_rot_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN channel_credentials.rotated_at IS
  'When the secret was last re-sealed. NULL means never rotated. Stamped from the same injected Go clock as created_at and advanced monotonically, GREATEST(created_at, rotated_at, $n), so that a rotating pod running behind the sealing pod can neither trip channel_credentials_rot_ck nor report the secret as older than it is.';

-- +goose Down

ALTER TABLE orgs ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE orgs ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE channel_credentials ALTER COLUMN created_at SET DEFAULT now();

-- The three columns above carried no comment before this migration; the fourth
-- carried 00011's. The Down puts each back exactly as it was, so a rolled-back
-- schema does not keep documenting a rule it no longer enforces.
COMMENT ON COLUMN orgs.created_at IS NULL;
COMMENT ON COLUMN orgs.updated_at IS NULL;
COMMENT ON COLUMN channel_credentials.created_at IS NULL;
COMMENT ON COLUMN channel_credentials.rotated_at IS
  'When the secret was last re-sealed. NULL means never rotated.';
