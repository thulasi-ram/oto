-- SPEC §D.8b, §B.8 (SPEC §P-1) -- Snooze: oto quiet button.
--
-- Snooze suppresses OTO OWN NOTIFICATIONS for one alert_key until a fixed time T.
-- It changes nothing in the cluster, nothing upstream, and nothing about the
-- signal state. It is the MANUAL sibling of storm collapse and flap damping.
--
--   Snooze IS      a fact about oto notification behaviour for one signal;
--                  stored only here; auto-expiring, always; attributed and
--                  visible; nearer to channels.verbosity than to a silence.
--   Snooze IS NOT  a fact about a person; a write into Alertmanager; an
--                  indefinite mute; a silent suppression.
--
-- ⛔ BINDING (SPEC §B.8.2): `snoozed` is NOT a `suppression_reason` and NOT a
-- `state`. alert_occurrences.suppression_reason mirrors Alertmanager FOUR
-- suppression reasons (silence, inhibition, mute_time_interval,
-- active_time_interval) and NOTHING ELSE, and alert_occurrences.state MUST NEVER
-- take the value `snoozed`. Conflating them would make oto report "Alertmanager
-- is suppressing this" when the truth is "a human asked oto to be quiet" -- a lie
-- about the world, in the one table whose job is to mirror the world.
-- Snooze records itself as notifications.suppressed_reason = 'snoozed' (00018),
-- oto OWN notification-suppression enum, alongside throttled, flapping, storm
-- and verbosity, which is exactly the company it keeps.
--
-- A SEPARATE TABLE, DELIBERATELY. Putting snoozed_by / snooze_note on `alerts`
-- would place a second person reference on a signal row and weaken §D.4.0, the
-- strongest clause in the spec. A side table keeps the column ban absolute AND
-- gives snooze history for free. alerts.snoozed_until is a bare timestamp
-- projection and is not a person reference.

-- ⛔ BINDING (SPEC §D.8b): `alert_snoozes` MUST NEVER gain `assigned_to`, a
-- recurrence rule, `days_of_week`, `time_of_day`, or a NULL `snoozed_until`.
-- A recurring snooze is a maintenance calendar; an unexpiring snooze is a mute.
-- Both are how a channel dies. Maintenance windows, if ever built, are a separate
-- feature with their own scope review (SCOPE-BOUNDARY §4.40).

-- +goose Up

-- --------------------------------------------------------------- alert_snoozes

CREATE TABLE alert_snoozes (
  id               UUID        PRIMARY KEY,
  org_id           UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  alert_id         UUID        NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  alert_key        TEXT        NOT NULL,           -- denormalised; survives for audit
  snoozed_at       TIMESTAMPTZ NOT NULL,
  snoozed_until    TIMESTAMPTZ NOT NULL,           -- NOT NULL: there is no indefinite snooze
  snoozed_by       UUID        REFERENCES users(id) ON DELETE SET NULL,
  snoozed_by_label TEXT        NOT NULL,           -- denormalised, immutable attribution
  note             TEXT,
  ended_at         TIMESTAMPTZ,
  ended_reason     TEXT,                           -- expired | manual | superseded
  ended_by         UUID        REFERENCES users(id) ON DELETE SET NULL,
  ended_by_label   TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT alert_snoozes_key_ck      CHECK (alert_key ~ '^ak_[0-9a-v]{26}$'),
  CONSTRAINT alert_snoozes_order_ck    CHECK (snoozed_until > snoozed_at),
  CONSTRAINT alert_snoozes_min_ck      CHECK (snoozed_until >= snoozed_at + interval '5 minutes'),
  CONSTRAINT alert_snoozes_max_ck      CHECK (snoozed_until <= snoozed_at + interval '30 days'),
  CONSTRAINT alert_snoozes_label_ck    CHECK (length(btrim(snoozed_by_label)) BETWEEN 1 AND 120),
  CONSTRAINT alert_snoozes_note_ck     CHECK (note IS NULL OR length(note) <= 2000),
  CONSTRAINT alert_snoozes_end_ck      CHECK ((ended_at IS NULL) = (ended_reason IS NULL)),
  CONSTRAINT alert_snoozes_endmap_ck   CHECK (ended_reason IS NULL OR
                                              ended_reason IN ('expired','manual','superseded')),
  CONSTRAINT alert_snoozes_endorder_ck CHECK (ended_at IS NULL OR ended_at >= snoozed_at),
  CONSTRAINT alert_snoozes_endby_ck    CHECK ((ended_by IS NULL) OR (ended_by_label IS NOT NULL))
);

-- INVARIANT: at most one ACTIVE snooze per alert. Enforced in the DB, not in Go.
-- snooze() closes any active snooze as 'superseded' in the SAME transaction as
-- the insert, so a 23505 on this index is a concurrency bug, never a race the
-- application is expected to lose.
CREATE UNIQUE INDEX alert_snoozes_active_idx ON alert_snoozes (alert_id) WHERE ended_at IS NULL;
-- Serves: the snooze.expire job every 60s -- the active snoozes whose clock has
-- run out, oldest first. Partial so the ended history never enters the index.
CREATE INDEX alert_snoozes_expiry_idx ON alert_snoozes (snoozed_until) WHERE ended_at IS NULL;
-- Serves: the snooze history for one alert, newest first, and the org-wide
-- active-snooze banner that makes the feature safe (SPEC §B.8.6).
CREATE INDEX alert_snoozes_org_idx    ON alert_snoozes (org_id, alert_id, snoozed_at DESC);

COMMENT ON TABLE  alert_snoozes IS
  'Snooze suppresses OTO OWN NOTIFICATIONS for one alert_key until a fixed time T (SPEC §B.8). It writes nothing into Alertmanager and changes nothing about the signal: a snoozed alert is still firing and is still rendered as firing. snoozed is NEVER a state and NEVER a suppression_reason; it records itself as notifications.suppressed_reason = snoozed.';
COMMENT ON COLUMN alert_snoozes.alert_id IS 'The Alert whose notifications are quiet. Snooze is scoped to an alert_key: an occurrence is too narrow (a re-fire restores the noise) and a group is too broad (it mutes alerts nobody has seen).';
COMMENT ON COLUMN alert_snoozes.alert_key IS 'Denormalised alerts.alert_key so the audit trail survives the Alert row. Same shape as alerts_key_ck.';
COMMENT ON COLUMN alert_snoozes.snoozed_at IS 'When the snooze began, on the oto clock.';
COMMENT ON COLUMN alert_snoozes.snoozed_until IS 'NOT NULL: THERE IS NO INDEFINITE SNOOZE. Minimum 5 minutes, maximum 30 days (alert_snoozes_min_ck, alert_snoozes_max_ck). An unexpiring snooze is a mute, and mutes are how channels die.';
COMMENT ON COLUMN alert_snoozes.snoozed_by IS 'The oto user who asked for quiet. ACTOR, NEVER SUBJECT; attribution on a snooze row, never a present-tense person column on a signal row (§D.4.0).';
COMMENT ON COLUMN alert_snoozes.snoozed_by_label IS 'Denormalised, immutable display name, so a renamed or deleted user never rewrites history.';
COMMENT ON COLUMN alert_snoozes.note IS 'Free-text justification, at most 2000 characters. Shown in the UI banner and in the Slack Notifications field.';
COMMENT ON COLUMN alert_snoozes.ended_at IS 'When the snooze stopped. NULL means active; which is exactly what alert_snoozes_active_idx and alert_snoozes_expiry_idx are partial on.';
COMMENT ON COLUMN alert_snoozes.ended_reason IS 'expired (the snooze.expire job) | manual (a human unsnoozed) | superseded (a new snooze replaced it). Present exactly when ended_at is.';

-- ------------------------------------------------------ alerts.snoozed_until

ALTER TABLE alerts ADD COLUMN snoozed_until TIMESTAMPTZ;

-- Serves: the snoozed facet of the alert list and the org-wide active-snooze
-- banner. Partial because the overwhelming majority of alerts are never snoozed.
CREATE INDEX alerts_snooze_idx ON alerts (org_id, snoozed_until) WHERE snoozed_until IS NOT NULL;

COMMENT ON COLUMN alerts.snoozed_until IS
  'Projection of the ACTIVE alert_snoozes row, written in the same transaction (SPEC §B.8.3). A BARE TIMESTAMP and therefore not a person reference; the attribution lives on alert_snoozes. Snooze is the THIRD ORTHOGONAL AXIS (§B.1): it is not a state, it never touches severity, and a snoozed alert is still rendered as firing. Snoozed alerts are NOT hidden from the default list (§B.8.6).';

-- +goose Down

DROP INDEX IF EXISTS alerts_snooze_idx;
ALTER TABLE alerts DROP COLUMN IF EXISTS snoozed_until;
DROP TABLE IF EXISTS alert_snoozes;
