-- git-bug 7287b28, the whole of it. `refire_grace_s` and `group_close_delay_s`
-- were org-facing settings that decided nothing, and they leave with
-- `alert_groups`.
--
-- ⛔ TWO KNOBS NOTHING READ, WHICH IS THE DEFECT AND NOT A LEFTOVER. Both were
-- bounds-validated (600..86400 and 60..86400), defaulted from ADR 0026's measured
-- rule corpus, patchable through `PATCH /api/v1/org/settings`, origin-reporting,
-- settable declaratively from a Helm values file, and rendered on the tuning
-- screen with `guide` copy that did arithmetic against the operator's own
-- Alertmanager. Neither had a single reader outside its own CRUD:
--
--   * `refire_grace_s` selected T8 -- the transition that ran a re-fire on the
--     CLOSED case. ADR 0040 retired T8 and made a Case strictly terminal, so
--     `routingCommand` passes `ResolveGrace` now while its own doc comment still
--     described a grace resolving T7 against T8. The mechanism went; the setting
--     stayed.
--   * `group_close_delay_s` timed the close of an `alert_groups` GENERATION. 00069
--     dropped the entity, and its one consumer -- `app/adapters.go`'s
--     `LifecyclePolicy{CloseDelay}` -- went with it.
--
-- ⭐ THE DROP CHANGES NO OBSERVABLE SLACK BEHAVIOUR, AND THAT IS THE STRONGEST
-- ARGUMENT THAT NO REPLACEMENT IS OWED. `platform/tuning/defaults.go` had already
-- written it down before anybody noticed: *"every re-fire opened a new episode, a
-- new generation and a new Slack root card, with a setting on the screen that
-- looked like it should have stopped it."* A card per re-fire is today's shipped
-- behaviour. The thing a rollover rule would have had to preserve was never
-- working.
--
-- ⛔ AND THE "PIN" BETWEEN THEM WAS NEVER ENFORCED. The two defaults were two
-- independent `1200 * time.Second` literals with a comment insisting the equality
-- was *"the whole point rather than a coincidence"*, and the only thing checking
-- it compared the two DEFAULTS -- so two operator-written SETTINGS could
-- contradict each other freely. Deleting BOTH is what makes that moot. Deleting
-- one would have left the trap armed with its tripwire removed.
--
-- DELETED, NOT RETIRED, on the same standing ruling that removed the reminder's
-- five keys (00068) and storm's three (00059): oto is UNRELEASED, `git tag` lists
-- nothing, and a knob that clamps, validates and reports an origin while changing
-- no outcome is a vocabulary entry the next person has to rule out. No ghosts at
-- launch.
--
-- ⚠️ CONFIG, NOT HISTORY -- the division 00067 and 00068 drew. Nothing in
-- `alert_events`, `alert_cases` or `notifications` records which grace was in force
-- when a fact was decided, so there is no history here to keep readable. What goes
-- is configuration that asked for future decisions nothing was going to make.
--
-- ⭐ THE Go SIDE LANDS FIRST AND THAT ORDER IS THE SAFE ONE. `settingsJSON` in
-- `identity/repository` stops naming both keys in the same change as this file, so
-- `encoding/json` already drops them on read before this migration deletes them.
-- There is no deploy window in which Go expects a key the database no longer has.
--
-- ⚠️ NO CHECK CONSTRAINT AND NO COLUMN IS TOUCHED, unlike 00068's half of the same
-- shape. `orgs.settings` is one JSONB document and neither key was ever mirrored
-- into DDL, so R9's three-copies rule had only two copies for these two: this
-- table and `openapi.yaml`. Both are updated in the same change.

-- +goose Up

-- `-` on a jsonb object is a no-op for a key that is absent, which is what makes
-- this safe on an org that never opened the settings screen. The `?|` guard keeps
-- `updated_at` still for every row that had neither key -- `orgs_time_ck` is
-- `updated_at >= created_at` and this statement is the only writer here, so a
-- blanket UPDATE would restamp every tenant to say something changed when nothing
-- did.
UPDATE orgs
   SET settings = settings
         - 'refire_grace_s'
         - 'group_close_delay_s',
       updated_at = now()
 WHERE settings ?| ARRAY['refire_grace_s', 'group_close_delay_s'];

-- ⭐ AND THE COLUMN COMMENT IS THE THIRD PLACE THE KEY SET IS WRITTEN DOWN, WHICH
-- IS WHY IT MOVES HERE RATHER THAN LATER. 00003 enumerated eleven keys in this
-- comment and it has been wrong since 00059 deleted the three storm keys: it still
-- names them, and it never gained `default_verbosity`. A schema comment that lists
-- a closed set has to be maintained by the migration that narrows the set, or
-- `psql \d+ orgs` becomes the most authoritative-looking wrong answer in the
-- system. Restated here as the SEVEN keys `settingsJSON` actually reads.

-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine and retention, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The seven keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months and default_verbosity. ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- +goose Down

-- ⚠️ THE SHAPE COMES BACK AND THE VALUES DO NOT, and this Down restores no data for
-- that reason rather than by omission. There was never a column or a constraint to
-- restore: the two keys were `omitempty` fields on one JSONB document, so the
-- "shape" is entirely in Go and comes back with the code. The Up deleted the keys
-- and recorded nowhere what it deleted, so a mirroring restore would have to
-- INVENT a grace for every org that had tuned one.
--
-- ⛔ AN `UPDATE ... SET settings = settings || '{"refire_grace_s": 1200}'` WOULD BE
-- THE WRONG FIX AND IS THE OBVIOUS ONE. It would hand every org an override it
-- never wrote -- reporting origin `org` for a number oto chose -- and destroy
-- exactly the distinction `SettingsPatch`'s pointers exist to keep: "this org
-- chose 1200" is a different fact from "this org is running the shipped default".
-- Restoring a value nobody set is worse than restoring nothing, because it looks
-- like a rollback.
--
-- An operator finishing this downgrade sets both keys again by hand, on the orgs
-- that had them, and saying so beats a statement that would look like a recovery.
--
-- What DOES come back is the column comment, restored to 00003's text verbatim --
-- storm keys, missing `default_verbosity` and all. A Down that "improved" the
-- comment on the way past would leave the schema in a state no Up ever produced,
-- and the point of a reversible migration is that down-then-up is a round trip.

-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine and retention: refire_grace_s, resolve_grace_s, group_close_delay_s, flap_threshold, flap_window_s, flap_digest_interval_s, storm_threshold, storm_window_s, storm_cooldown_s, raw_retention_days, event_retention_months.';
-- +goose StatementEnd
