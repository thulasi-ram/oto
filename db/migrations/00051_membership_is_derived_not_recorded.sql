-- Group membership stops being a RECORDED EVENT and becomes a PROPERTY of the
-- episode: `alert_group_members` is dropped and `alert_occurrences.group_id` —
-- declared in 00007, FK'd in 00008, indexed by `occ_group_idx` — is the whole
-- answer to "what is in this generation".
--
-- ⭐⭐ WHY IT IS CORRECT NOW AND WAS NOT BEFORE. 00050 took `receiver` and
-- Alertmanager's `groupLabels` out of the group key and derived it from the
-- alert's own labels instead. That removed the only way an episode could
-- legitimately belong to two generations at once — `continue: true` fanning one
-- alert into two receivers — and with it the reason for a join table. Episode →
-- Group is now MANY-TO-ONE, exactly like Episode → Alert, and a many-to-one
-- relationship recorded in a second table is a second answer to a question that
-- already has one.
--
-- ⛔⛔ AND BECAUSE THE JOIN TABLE WAS NEVER CLOSED. `left_at` had no production
-- writer: the repository, the service and the port all implemented `Leave`, the
-- service appended `group.member_left`, and NOTHING CALLED ANY OF IT. So every
-- `left_at IS NULL` predicate — six of them — matched every row that had ever
-- been inserted. `gm_current_idx` (00044) was PARTIAL on that predicate and
-- therefore narrowed nothing. The point-in-time replay `joined_at <= $3 AND
-- (left_at IS NULL OR left_at > $3)` could only ever show a generation GROWING.
-- And because the primary key was `(group_id, occurrence_id)`, an alert that
-- resolved and re-fired outside `refire_grace` but inside one generation got a
-- SECOND row, also open, so the card listed the same alert twice — once resolved,
-- once firing. Both tuning knobs default to 1200s, so that is a twenty-minute
-- window, not a pathological one.
--
-- The defect went unnoticed because `group.close` reads `last_activity_at`, a
-- timestamp, and never asked the membership anything.
--
-- ⭐ WHAT REPLACES EACH PREDICATE. `occ_terminal_ended` already states
-- `(state IN ('resolved','expired')) = (ended_at IS NOT NULL)`, so
--
--     m.left_at IS NULL                     ->  o.ended_at IS NULL
--     m.joined_at                           ->  o.started_at
--     m.left_at                             ->  o.ended_at
--     PK (group_id, occurrence_id)          ->  alert_occurrences PK (id)
--
-- and the replay predicate becomes `o.started_at <= $3 AND (o.ended_at IS NULL OR
-- o.ended_at > $3)` — the same two clauses, now over a column something actually
-- writes. Replay is non-monotonic for the first time.
--
-- ⭐ THE ROLLUP DID NOT CHANGE AND MUST NOT. A generation's counts (firing,
-- suppressed, resolved, expired, total, acked) are over EVERY episode the
-- generation has held, not over the live ones — a card reading "12 alerts, 3
-- firing, 9 resolved" is the point of the card. That aggregate was already
-- computing the right thing, by accident, because `left_at IS NULL` matched
-- everything. It now computes it on purpose, as `o.group_id = $2` with no
-- liveness clause at all.
--
-- ⭐ STORM COUNTING IS UNAFFECTED. §B.6 counts DISTINCT alerts JOINING inside a
-- window; an episode joins when it opens, so `count(DISTINCT alert_id) WHERE
-- started_at >= $3` is the same number `alert_group_members` was giving, read off
-- the row that defines the join.
--
-- ⛔ `group.close` IS DELIBERATELY UNCHANGED, and this migration is not the place
-- to change it. It stays driven by `last_activity_at` plus the rollup's refusal to
-- close over a live member. What DOES change is that the refusal is now true: the
-- firing/suppressed counts it consults are derived from `ended_at`, which the
-- state machine writes on every T5/T6.
--
-- ⭐ THE TWO MEMBER EVENT TYPES ARE RETIRED, NOT DELETED. `group.member_joined`
-- and `group.member_left` are no longer appended by anything — `alerts/service`
-- refuses them at the single writer of `alert_events` — but they stay in the
-- closed EventType enum and in `AlertEventType` on the public contract, because
-- `alert_events` is retained THIRTEEN MONTHS and rows carrying those values
-- already exist. Removing a value from the enum a persisted row can contain would
-- make `NewEventType` reject history on read and make oto answer timelines its own
-- generated client refuses to parse. They leave the vocabulary when the last
-- partition holding them is dropped, and not before.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is a CONTRACT step and it is not
-- reversible in the ordinary sense, so it must ship AFTER the release that stopped
-- reading `alert_group_members`. A release-N binary still holding the old SQL
-- would fail on every group read the moment this lands. The Down below rebuilds
-- the table and REPOPULATES it from `alert_occurrences`, which is exact rather
-- than approximate: every column the table carried is derivable from the episode
-- row, which is the argument for dropping it.
--
-- ⚠️ NOT `CONCURRENTLY` for the new index, for the reason 00042 and 00044 both set
-- out: no migration in this tree runs outside goose's transaction, and taking one
-- out of it trades a bounded write pause for a migration that can fail halfway and
-- leave an INVALID index no `goose down` removes.

-- +goose Up

-- The current-member page, in its new home. Same shape as gm_current_idx (00044)
-- and the same argument: both equalities, the partial predicate and the WHOLE
-- sort key, so `listCurrentMembersSQL`'s LIMIT stops the scan and nothing is
-- sorted. `occ_group_idx` (org_id, group_id, started_at DESC) cannot serve it —
-- it carries no tiebreak for the keyset's `(started_at, id)` row comparison and is
-- the size of the generation's whole history rather than of its live membership.
CREATE INDEX occ_group_live_idx ON alert_occurrences (org_id, group_id, started_at DESC, id DESC)  -- vocab:allow -- SEQUENCE ORDER, not history: 00051 runs BEFORE 00052, so `alert_occurrences` is the table's actual name here and `alert_cases` would not resolve. The rename is deliberately its own migration (00052's Up is the rename and nothing else), so every migration numbered below it must spell the old name. 00052 renames this index to case_group_live_idx.
  WHERE ended_at IS NULL;

COMMENT ON INDEX occ_group_live_idx IS
  'Serves listCurrentMembersSQL -- the keyset page behind GET /alert-groups/{id}/alerts, the twenty-row top_alerts preview behind GET /alert-groups/{id} which the ack, snooze and unsnooze replies also render, the fan-out candidate read and the current-member count. It carries both equalities, the partial predicate and the whole sort key, so the LIMIT stops the scan and no Sort node appears. PARTIAL on ended_at IS NULL so it stays the size of the LIVE membership rather than of the generation history: the replay reads (allMembersSQL, membersAtSQL) and the rollup want the ended episodes back and are meant to ride occ_group_idx instead. It replaces gm_current_idx, which was partial on a left_at nothing ever wrote and therefore narrowed nothing at all.';

COMMENT ON COLUMN alert_occurrences.group_id IS  -- vocab:allow -- SEQUENCE ORDER, not history: 00051 runs BEFORE 00052, so `alert_occurrences` is the table's actual name here and `alert_cases` would not resolve. The rename is deliberately its own migration (00052's Up is the rename and nothing else), so every migration numbered below it must spell the old name.
  'THE MEMBERSHIP. Which AlertGroup generation this episode belongs to, and the only record of it since alert_group_members was dropped. Written once, when the episode opens, and never moved: the group key is derived from the alert own labels (ADR 0038) and alert identity IS its label set, so an episode split key is fixed for its whole life. NULL means the group key could not be computed and the signal was recorded groupless, which is the deliberate degradation in the ingest orchestrator. started_at is when it joined and ended_at is when it left; membership is still history, it is just the episode own history now.';

DROP INDEX IF EXISTS gm_current_idx;
DROP TABLE IF EXISTS alert_group_members;

-- +goose Down

CREATE TABLE alert_group_members (
  group_id      UUID        NOT NULL REFERENCES alert_groups(id) ON DELETE CASCADE,
  occurrence_id UUID        NOT NULL REFERENCES alert_occurrences(id) ON DELETE CASCADE,
  org_id        UUID        NOT NULL,
  alert_id      UUID        NOT NULL,
  joined_at     TIMESTAMPTZ NOT NULL,
  left_at       TIMESTAMPTZ,
  PRIMARY KEY (group_id, occurrence_id),
  CONSTRAINT gm_order_ck CHECK (left_at IS NULL OR left_at >= joined_at)
);

-- Exact, not approximate: every column the table carried is a column of the
-- episode. `left_at` comes back populated rather than NULL, which is MORE than
-- the Up ever had — the release being rolled back to will read it as a membership
-- that closes, which is what it always meant to record.
INSERT INTO alert_group_members (group_id, occurrence_id, org_id, alert_id, joined_at, left_at)
SELECT o.group_id, o.id, o.org_id, o.alert_id, o.started_at, o.ended_at
  FROM alert_occurrences o
 WHERE o.group_id IS NOT NULL;

CREATE INDEX gm_alert_idx ON alert_group_members (org_id, alert_id, joined_at DESC);
CREATE INDEX gm_occ_idx   ON alert_group_members (occurrence_id);
CREATE INDEX gm_current_idx ON alert_group_members (org_id, group_id, joined_at DESC, occurrence_id DESC)
  WHERE left_at IS NULL;

COMMENT ON TABLE  alert_group_members IS
  'Membership of one AlertOccurrence in one AlertGroup generation, with join/leave times so the group card can be replayed at any past instant.';
COMMENT ON COLUMN alert_group_members.org_id IS 'Denormalised so gm_alert_idx can lead with org_id (CONTEXT.md §5 rule 7).';
COMMENT ON COLUMN alert_group_members.alert_id IS 'Denormalised from the occurrence to answer "groups for this alert" without a join.';
COMMENT ON COLUMN alert_group_members.left_at IS 'NULL while still a member. Membership is history, not a boolean.';
COMMENT ON INDEX gm_current_idx IS
  'Serves listCurrentMembersSQL, the only read of the CURRENT members of a generation.';

DROP INDEX IF EXISTS occ_group_live_idx;
COMMENT ON COLUMN alert_occurrences.group_id IS NULL;
