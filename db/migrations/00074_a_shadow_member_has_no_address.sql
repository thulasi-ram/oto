-- git-bug a74d6b2. A Slack press by an UNLINKED member takes no idempotency claim, so a
-- redelivered press writes the timeline events twice. This migration lands the half of the
-- fix that lives in the schema: `users.email` becomes NULLABLE, so an oto user can exist
-- for a Slack member who has never given oto an address.
--
-- ------------------------------------------------------------------ the defect
--
-- `idempotency_claims`' primary key is (org_id, principal_id, operation, idempotency_key)
-- and `principal_id` is NOT NULL; `idempotency.Claim.validate` refuses `uuid.Nil` as a
-- wiring bug. So a claim needs a PRINCIPAL UUID. A Slack member who has never linked an
-- oto account has no such uuid -- `channels/service.actor` records them as
-- `actor_kind = 'slack'` with the Slack member id, a string like `U024BE7LH` that does not
-- parse -- so `app.slackIdempotency` returned an UNKEYED intent, `alerts/service.Snooze`
-- skipped `idempotency.Resolve` entirely (`if idem.Keyed`), and Slack's redelivery of one
-- human press executed the snooze a second time: the incumbent closed as `superseded`, a
-- second `alert_snoozes` row inserted, and the Case timeline carrying two
-- `alert.unsnoozed(superseded)` + `alert.snoozed` pairs for one gesture.
--
-- ⛔ AN UNLINKED MEMBER IS A FIRST-CLASS STATE AND NOT A BROKEN ONE, which is why the fix
-- could not be "refuse the press". `identity/domain.SlackIdentity` states it in as many
-- words: *"An UNLINKED identity (UserID zero) is a first-class state, not a broken one: it
-- still acks... Requiring a link before an ack could be recorded would mean the product
-- silently loses acknowledgements from anybody who has not onboarded."* So this is a NORMAL
-- path, and the audit record oto sells was doubling on it.
--
-- --------------------------------------------------------------- the owner's ruling
--
-- A SHADOW OTO USER IS CREATED ON FIRST PRESS, AND IT CARRIES NO EMAIL. The alternative
-- that was rejected is a SYNTHETIC address -- `U024BE7LH@slack.invalid` or similar -- and
-- the reason it was rejected is worth keeping, because it will be proposed again by
-- somebody who notices that it needs no migration at all:
--
--   * A synthetic address is INDISTINGUISHABLE FROM A REAL ONE at every reader. Every
--     surface that renders `users.email` would render it, every operator reading the
--     members list would see a plausible-looking mailbox, and the one question that
--     matters -- "has this person given us an address?" -- would have no answer left in
--     the schema. NULL answers it exactly, once, for every reader at once.
--   * It is a value oto INVENTS and then has to keep telling itself is not real. A domain
--     tag in a mailbox is a convention, not a constraint: nothing stops a future writer
--     from mailing it, matching on it, or de-duplicating against it.
--   * `users_email_uniq` would then have to be defended against a COLLISION oto minted
--     itself, which NULL cannot have (see below).
--
-- `password_hash` stays NULL on a shadow row. That column's own comment already carries the
-- consequence -- *"NULL means password login is disabled for this user (SSO or Slack-only)"*
-- -- so a shadow row cannot log in by password, and with a NULL email it cannot be FOUND by
-- a login attempt either: `resolveByEmailSQL` compares `u.email = $1`, and in SQL
-- `NULL = 'anything'` is NULL, which is not TRUE, so the row is never a candidate. Two
-- independent refusals, neither of which relies on the other.
--
-- ------------------------------------------------- users_email_uniq needs no change
--
-- ⭐⭐ `users_email_uniq UNIQUE (org_id, email)` IS LEFT EXACTLY AS 00003 WROTE IT, and that
-- is a decision rather than an oversight. Postgres treats NULLs as DISTINCT in a unique
-- constraint (the SQL-standard default, and the only behaviour available before
-- PostgreSQL 15's opt-in `NULLS NOT DISTINCT`): every row with a NULL `email` is unique
-- against every other one, so an org may hold as many shadow members as it has Slack
-- members who press buttons. Widening the constraint -- to a partial unique INDEX over
-- `WHERE email IS NOT NULL`, which is the change a reader expects to find here -- would
-- change nothing about which rows are admitted and would cost the constraint its NAME,
-- which §L.9 publishes as an API error `code` on a 23505 and §D as a
-- `oto_check_violation_total{constraint}` label. A rename is an API change. There is
-- nothing to buy with it.
--
-- ⚠️ THE PROPERTY THIS RELIES ON IS THE DEFAULT AND NOT A SETTING, so there is no
-- deployment in which it is off: `NULLS NOT DISTINCT` must be spelled on the constraint
-- itself, and this one does not spell it. A future migration that added it would silently
-- cap every org at ONE shadow member and turn the second Slack presser's first press into a
-- 23505 -- so if one is ever proposed, this paragraph is the reason to refuse it.
--
-- ------------------------------------------------------ users_email_ck, and why at all
--
-- ⚠️ THE CHECK DID NOT ACTUALLY NEED WIDENING TO ADMIT A NULL, AND IT IS WIDENED ANYWAY.
-- A CHECK constraint is satisfied when its predicate is TRUE **or NULL**, and with
-- `email` NULL both `email ~ '...'` and `length(email) <= 254` are NULL, so the conjunction
-- is NULL and 00003's predicate already lets the row through. Dropping `NOT NULL` alone
-- would have worked.
--
-- It is widened because the predicate is READ BY PEOPLE, and by one drift test. As 00003
-- wrote it, `users_email_ck` says "an email matches this shape and is at most 254
-- characters" -- a sentence that is now FALSE about the column, and false in the direction
-- that costs a reviewer an hour: the next person to add a NULL-email writer looks at this
-- constraint, believes it forbids what they are about to do, and either abandons a correct
-- change or ships a second column to avoid it. `email IS NULL OR (...)` states the
-- three-valued truth explicitly, so the constraint means what it appears to mean without
-- anybody having to know Postgres's CHECK semantics. `identity/domain.PatternUserEmail`
-- keeps mirroring the regex half byte-for-byte, which is what `TestValidatorMatchesDDL`
-- compares; the `IS NULL OR` wrapper is around it, not in it.
--
-- ⛔ THE CONSTRAINT KEEPS ITS NAME, as `notifications_suppmap_ck` has across 00018, 00059
-- and 00073. §L.9 maps a 23514 to `oto_check_violation_total{constraint}` and a 23505 to an
-- API error `code` carrying the constraint name; renaming one silently changes a metric
-- label and an API response.
--
-- ------------------------------------------------------------------- the members list
--
-- ⭐ A SHADOW MEMBER IS VISIBLE AND IS NOT `disabled_at`-HIDDEN. `users_org_idx ON users
-- (org_id) WHERE disabled_at IS NULL` serves, in 00003's own words, *"the members list and
-- every 'who acked this' lookup, both of which only ever want live users"* -- and those two
-- readers are the argument. A shadow row IS the answer to "who acked this" for every
-- Slack-only presser: `cases.acked_by`, `alert_snoozes.snoozed_by` and
-- `alert_snoozes.ended_by` now point AT it. Hiding it behind `disabled_at` would make the
-- one lookup that index exists for return nothing for the very rows it was added to
-- attribute, which is the audit hole the timeline exists to close -- and it would do so
-- silently, because a missing member renders as a blank actor rather than as an error.
--
-- So the members list gains a row per Slack member who has pressed a button, distinguished
-- by its NULL email and named by its Slack handle. That is operator-visible and it is
-- intended: those people ARE members of the org in the only sense oto has ever claimed to
-- know -- they act on its alerts. `disabled_at` remains what 00003 says it is, a SOFT
-- DISABLE of a real account, and is not repurposed into a visibility flag.

-- +goose Up

-- --------------------------------------------------------------------- users.email

-- EXPAND/CONTRACT (CONTEXT.md §6), and this is the EXPAND direction that cannot fail:
-- dropping NOT NULL admits strictly more rows than before, so no row on disk can violate
-- the result and there is no backfill. Release N, which never writes a NULL, is unaffected.
-- The reverse direction is the one that can fail, and the Down below is where that is
-- handled rather than discovered.
ALTER TABLE users ALTER COLUMN email DROP NOT NULL;

ALTER TABLE users DROP CONSTRAINT users_email_ck;
ALTER TABLE users ADD  CONSTRAINT users_email_ck CHECK (email IS NULL OR (
  email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' AND length(email) <= 254));

-- +goose StatementBegin
COMMENT ON COLUMN users.email IS
  'The login identity, and NULLABLE since 00074 (git-bug a74d6b2). NULL means THIS PERSON HAS NEVER GIVEN OTO AN ADDRESS: the row is a SHADOW MEMBER, minted on the first Slack button press by a workspace member who has not linked an oto account, so that the press has a principal uuid to take an idempotency claim under and a redelivered press is refused instead of executed twice. A shadow row cannot authenticate, by two independent refusals: password_hash is NULL, which this table already documents as password login disabled, and identity/repository.resolveByEmailSQL compares u.email = $1, which is never TRUE for a NULL. It is NOT hidden from the members list -- users_org_idx serves "who acked this" and a shadow row is that answer for every Slack-only presser -- and it is NOT given a synthetic address, because an invented mailbox is indistinguishable from a real one at every reader while NULL answers "has this person given us an address" exactly once for all of them. ⚠️ users_email_uniq (org_id, email) is UNCHANGED and needs no change: Postgres treats NULLs as distinct, so an org may hold many shadow members. Adding NULLS NOT DISTINCT to it would cap an org at one and is the one edit to this column that must be refused.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT users_email_ck ON users IS
  'An address is well-shaped and at most 254 characters, OR ABSENT. The IS NULL disjunct is redundant to Postgres -- a CHECK passes when its predicate is NULL, so 00003''s conjunction already admitted a NULL email -- and it is written anyway because this predicate is read by people: as 00003 spelled it, the constraint appeared to forbid the NULL-email shadow member 00074 introduces, which is the kind of false prohibition that costs a reviewer a correct change. The regex half stays byte-identical to identity/domain.PatternUserEmail, which TestValidatorMatchesDDL compares as a STRING.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK: NO ROW MAY STILL HAVE A NULL EMAIL. Re-adding NOT NULL is the
-- CONTRACT direction and it is the one that can fail. A bare `SET NOT NULL` would already
-- refuse the rollback -- with a 23502 naming a column and no explanation of what the rows
-- are or why an operator should care -- so this block is the same refusal with the count,
-- the reason and the query to inspect them in it.
--
-- ⚠️ NOTHING IS REWRITTEN AND NOTHING IS DELETED, which is 00018's rule as restated by
-- 00059, 00067 and 00073. There is no honest address to invent for a shadow member: they
-- never gave oto one, that absence is the fact the row records, and filling it in would
-- both defeat the reason a synthetic address was rejected in the first place and leave a
-- row that a login attempt could then COMPARE AGAINST. Deleting them is worse: every
-- `cases.acked_by`, `alert_snoozes.snoozed_by` and `alert_snoozes.ended_by` pointing at one
-- is `ON DELETE SET NULL`, so the delete succeeds quietly and takes the attribution off the
-- timeline -- the audit record, destroyed to make a rollback run.
--
-- What an operator does instead is decided deliberately, above the database. Note that the
-- release this rolls back to still executes an unlinked press TWICE, so the presses these
-- rows were minted for are about to start doubling again; the rows themselves are the only
-- record of who made them.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad FROM users WHERE email IS NULL;

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00074 down: % user row(s) have a NULL email, which the NOT NULL this rollback restores does not admit. Each one is a SHADOW MEMBER -- a Slack workspace member who pressed a button in oto without ever linking an account, minted so that the press has a principal to take an idempotency claim under. Inspect them with:  SELECT u.id, u.org_id, u.display_name, u.created_at, si.team_id, si.slack_user_id, si.slack_handle FROM users u LEFT JOIN slack_identities si ON si.user_id = u.id WHERE u.email IS NULL ORDER BY u.created_at;  There is no address to invent for them: the absence IS the fact the row records. Deleting them is not the cheap option either -- cases.acked_by, alert_snoozes.snoozed_by and alert_snoozes.ended_by are all ON DELETE SET NULL, so a DELETE here succeeds silently and strips the acknowledgement off the timeline. Decide what happens to these people deliberately rather than to make the migration run.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- Byte-identical to what 00003 shipped, including the absence of an `IS NULL` disjunct: a
-- rolled-back database is in the state its own migration history describes.
ALTER TABLE users DROP CONSTRAINT users_email_ck;
ALTER TABLE users ADD  CONSTRAINT users_email_ck CHECK (
  email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' AND length(email) <= 254);

ALTER TABLE users ALTER COLUMN email SET NOT NULL;

-- 00003 put no COMMENT on this column and no comment on this constraint, so the rollback
-- takes both back off rather than leaving 00074's text describing a column that is NOT NULL
-- again. A comment naming a state the schema no longer has is the defect 00062 was written
-- to fix.
-- +goose StatementBegin
COMMENT ON COLUMN users.email IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT users_email_ck ON users IS NULL;
-- +goose StatementEnd
