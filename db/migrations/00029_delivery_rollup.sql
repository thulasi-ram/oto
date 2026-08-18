-- The index behind `delivery_summary`, plus two column comments that had gone
-- half-true.
--
-- ⭐⭐ WHY. `delivery_summary` is declared on four response schemas -- the alert
-- detail, the occurrence detail, the group detail and the notification detail --
-- and until now it was emitted by NONE of them. It is optional in the contract,
-- so every schema validator passed and the gap was invisible: the field simply
-- never appeared, and nothing anywhere said so.
--
-- That field is not decoration. oto's product claim is that its SILENCE can be
-- trusted, and silence is only trustworthy when it can be told apart from
-- failure. Somebody who sees no Slack message has to be able to learn whether
-- nothing fired or whether four deliveries died on an expired token, and from
-- outside this database those two look identical.
--
-- WHAT THIS MIGRATION ADDS is one index. The roll-up reads
-- notification_deliveries joined to notifications, filtered by the subject:
--
--   alert       -- notifications naming the alert, plus every notification about
--                  a group generation the alert has been a member of. Rides
--                  notif_alert_idx and gm_alert_idx.
--   occurrence  -- notifications naming the occurrence, plus the generations that
--                  occurrence joined. Rides THIS index and gm_occ_idx.
--   alert_group -- notifications about the generation. Rides notif_subject_idx.
--
-- The occurrence arm was the only one with no index to ride: notifications has
-- (org_id, alert_id) and (org_id, subject_kind, subject_id) and nothing on
-- occurrence_id, so a detail page would have paid a sequential scan of every
-- notification in the org for one card.
--
-- PARTIAL, because occurrence_id is NULL on every group-scoped intent -- which is
-- most of them -- and indexing those rows would double the index for entries that
-- can never be probed.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). An index is a pure widening: nothing to
-- backfill, no constraint a release-N writer can violate, and the Down drops it
-- and loses only speed.
--
-- The two COMMENT rewrites at the foot are a correction, not a change of
-- behaviour. 00028 said the timing columns must never be backfilled with
-- Alertmanager's documented defaults, which is still exactly right about
-- STORAGE; it read as though the defaults may never be shown at all, and that is
-- what left oto rendering "unknown" for every stock Alertmanager. The defaults
-- are now applied at READ time, labelled as derived rather than observed, and the
-- columns still store only what was seen.

-- +goose Up

CREATE INDEX notif_occurrence_idx ON notifications (org_id, occurrence_id)  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
  WHERE occurrence_id IS NOT NULL;  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.

COMMENT ON INDEX notif_occurrence_idx IS  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
  'Serves the delivery roll-up of one firing episode: was anybody told about THIS outage, and did it land. Partial because occurrence_id is NULL on every group-scoped intent, which is most of them.';  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.

COMMENT ON COLUMN source_health.am_group_wait_ms IS
  'The TOP-LEVEL route group_wait this Alertmanager reports in config.original, in milliseconds. NULL means THE CONFIGURATION STATED NOTHING, and it is never backfilled: Alertmanager omits an unset value, applies its documented 30s later in dispatch.NewRoute, and the status endpoint cannot see it. What that NULL MEANS is decided on read -- with am_route_timings_at set it is reported as the documented default, labelled as derived rather than observed; with am_route_timings_at NULL it is reported as unknown. Read from the source, never typed in by a human (docs/setup/tuning.md).';
COMMENT ON COLUMN source_health.am_group_interval_ms IS
  'The TOP-LEVEL route group_interval, in milliseconds. It is the clock rate of oto''s whole view of the world: oto cannot learn of a change to an existing group faster than this, so every oto duration is a multiple of it. NULL means the configuration stated nothing, never zero, and the documented 5m is supplied on read rather than stored here.';
COMMENT ON COLUMN source_health.am_repeat_interval_ms IS
  'The TOP-LEVEL route repeat_interval, in milliseconds. It is what produces notification_reason "repeat interval elapsed", which oto maps to an update-only delivery. NULL means the configuration stated nothing; the documented 4h is supplied on read rather than stored here.';

-- +goose Down

DROP INDEX IF EXISTS notif_occurrence_idx;

COMMENT ON COLUMN source_health.am_group_wait_ms IS
  'The TOP-LEVEL route group_wait this Alertmanager reports in config.original, in milliseconds. NULL = NOT OBSERVED, and it must never be backfilled with Alertmanager''s documented 30s default: Alertmanager omits an unset value, applies the default later in dispatch.NewRoute, and the status endpoint cannot see it. Read, never typed in by a human (docs/setup/tuning.md).';
COMMENT ON COLUMN source_health.am_group_interval_ms IS
  'The TOP-LEVEL route group_interval, in milliseconds. It is the clock rate of oto''s whole view of the world: oto cannot learn of a change to an existing group faster than this, so every oto duration is a multiple of it. NULL = NOT OBSERVED, never the documented 5m default.';
COMMENT ON COLUMN source_health.am_repeat_interval_ms IS
  'The TOP-LEVEL route repeat_interval, in milliseconds. It is what produces notification_reason "repeat interval elapsed", which oto maps to an update-only delivery. NULL = NOT OBSERVED, never the documented 4h default.';
