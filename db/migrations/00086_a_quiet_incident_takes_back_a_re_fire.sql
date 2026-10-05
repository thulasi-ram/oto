-- ADR 0052 §4: a later Case matching a Correlator joins its Incident while the
-- Incident is active AND within the Correlator's operator-written `quiet_grace`
-- after it went quiet; past that, it draws a new Incident (git-bug 34a27c5).
-- 00085 shipped the first half only, so a disk alert firing every twenty minutes
-- drew one Incident per firing overnight — the "ten Incidents" ADR 0052 names, and,
-- because every Incident is declared outbound (§5), ten external incidents.
--
-- ⭐⭐ A GRACE WINDOW RETURNS, AND IT IS NOT THE ONE THAT LEFT. `group_close_delay_s`
-- and `refire_grace_s` were deleted with `AlertGroup` (00069, 00071), and the
-- reason recorded there was never "a grace window is wrong". It was that their
-- ENTITY was wrong: a grouping oto derived on its own, which no operator wrote and
-- no operator could read back, timed by two org-wide knobs that 00071 found
-- decided nothing. ADR 0039 kept its reasoning because "the shape recurs", and
-- this is the recurrence it meant — the same question (how long after it fell
-- silent does a story still own the next firing?) asked of the right entity:
--
--   * IT BELONGS TO AN INCIDENT, which is a grouping an operator OR a human drew,
--     not one oto derived. ADR 0042 §3 and 0044's authority test is "did somebody
--     write this?", and a Correlator is somebody's written decision.
--   * IT IS A NUMBER THE OPERATOR WROTE ON THAT CORRELATOR, not an org-wide tuning
--     default with an origin and a measured corpus behind it. NULL — the default —
--     is "join only while active", which is 00085's behaviour exactly, so nothing
--     changes until an operator asks for it by name.
--   * AND SOMETHING READS IT FROM THE FIRST ROW: the evaluator's join rule
--     (`domain.Correlator.Joins`), on every Case a Correlator claims. That is the
--     test 00071 failed `refire_grace_s` on — a knob that validates, clamps and
--     reports while deciding nothing — and this one fails it only if that function
--     stops being called.
--
-- ⭐ "WENT QUIET" IS READ, NEVER STORED, as the Incident's state is (ADR 0052 §3).
-- An Incident has no `quiet_at` column and gains none here: the instant is the
-- latest `ended_at` among its current member Cases — the close that left it with
-- no open member — or the latest removal, if a human's removal is what quieted it.
-- A column would need a writer, and the only honest writer is the Case lifecycle,
-- which must not know about Incidents.
--
-- ⛔ IT IS NOT A SCHEDULE (SCOPE-BOUNDARY §4.8). It says how long after a fact a
-- later fact still belongs to the same story; it carries no time of day.
--
-- The bound is the count window's, 60..86400: under a minute is "only while
-- active" spelled with extra steps, and over a day is a story nobody would
-- recognise as one.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One nullable column, one CHECK every existing
-- row (NULL) satisfies. A release-N pod writes NULL and gets 00085's behaviour.

-- +goose Up

ALTER TABLE correlators ADD COLUMN quiet_grace_s INT;

ALTER TABLE correlators ADD CONSTRAINT correlators_quiet_grace_ck
  CHECK (quiet_grace_s IS NULL OR quiet_grace_s BETWEEN 60 AND 86400);

-- +goose StatementBegin
COMMENT ON COLUMN correlators.quiet_grace_s IS
  'How long after this Correlator''s latest Incident went QUIET a matching Case still joins it (ADR 0052 §4), 60..86400 seconds; NULL joins only while it is active. Past the grace, a matching Case draws a new Incident, so a flapping alert is one story rather than one per firing. "Went quiet" is read, not stored: the latest ended_at among the current member Cases, or the latest removal. Joining a quiet Incident declares case_added and active_again. A human-drawn Incident never grows by itself, grace or not. A grace window on an entity an operator wrote -- not group_close_delay_s / refire_grace_s, which left with AlertGroup (00069, 00071) because their entity was one oto derived.';
-- +goose StatementEnd

-- +goose Down

-- ⚠️ THE DOWN DROPS CONFIGURATION: every Correlator falls back to joining only
-- while active, so the next overnight flap draws an Incident per firing again —
-- and each is declared outbound. The values are recoverable from nowhere else.
ALTER TABLE correlators DROP CONSTRAINT IF EXISTS correlators_quiet_grace_ck;
ALTER TABLE correlators DROP COLUMN IF EXISTS quiet_grace_s;
