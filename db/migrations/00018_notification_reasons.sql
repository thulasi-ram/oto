-- SPEC §D.8, §B.8.2, §H.6 (SPEC §P-2) -- notifications reason and suppressed_reason.
--
-- Two vocabulary changes, both forced by SPEC §A.1 and §B.8:
--
--   reason:            drop 'escalation', add 'unacked_reminder', 'snoozed',
--                      'unsnoozed'. `escalation` is a SCOPE-BANNED word: oto has
--                      ONE reminder stage, forever, and it reminds a CHANNEL, not
--                      a person (§G.9.1). Say unacked reminder, not escalation.
--   suppressed_reason: add 'snoozed'. This is oto OWN notification-suppression
--                      enum -- the company snooze keeps is throttled, flapping,
--                      storm and verbosity, NOT Alertmanager four suppression
--                      reasons (see the ⛔ block in 00017_snooze.sql).
--
-- EXPAND/CONTRACT (CONTEXT.md §6). The reason change is a NARROWING, so it runs
-- in three phases inside one transaction, and release N keeps working throughout:
--   1. expand   -- add notifications_reason_expand_ck, the UNION of the old and
--                  new vocabularies. Every existing row satisfies it, and a
--                  release-N writer emitting 'escalation' still satisfies it.
--   2. backfill -- rewrite reason='escalation' to 'unacked_reminder'.
--   3. contract -- swap notifications_reason_ck to the final §D.8 vocabulary and
--                  drop the transitional constraint.
--
-- The suppressed_reason change is a pure WIDENING: no row can violate the new
-- predicate, so the named constraint is simply replaced.
--
-- Constraint names are a RUNTIME CONTRACT (SPEC §L.9, CONTEXT.md §6): they come
-- back as errs.Error.Code on 23514/23505. notifications_reason_ck and
-- notifications_suppmap_ck keep their names on the far side of this migration.

-- +goose Up

-- 1. expand ------------------------------------------------------------------
ALTER TABLE notifications ADD CONSTRAINT notifications_reason_expand_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   -- vocab:allow — the expand/backfill/contract rename itself must name the value it is retiring, or it cannot retire it.
   'comment','unacked_reminder','storm','escalation'));

-- 2. backfill ----------------------------------------------------------------
-- vocab:allow — the expand/backfill/contract rename itself must name the value it is retiring, or it cannot retire it.
UPDATE notifications SET reason = 'unacked_reminder', updated_at = now() WHERE reason = 'escalation';

-- 3. contract ----------------------------------------------------------------
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm'));
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_expand_ck;

-- suppressed_reason: pure widening, 'snoozed' joins oto own damper vocabulary.
ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('no_policy','throttled','storm','flapping','snoozed','verbosity',
                        'channel_disabled','duplicate_render'));

-- vocab:allow — the column comment names the retired value once, to say it is retired.
COMMENT ON COLUMN notifications.reason IS
  'The §H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply. unacked_reminder is oto ONE reminder stage (§G.9.1); never escalation, which is a scope-banned word (§A.1). snoozed and unsnoozed announce a snooze beginning and ending, and are the ONLY two reasons a snooze does not suppress (§B.8.4): a snooze that cannot announce itself is the silent suppression §B.6 forbids.';
COMMENT ON COLUMN notifications.suppressed_reason IS
  'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6). When several apply the FIRST MATCH in this fixed order wins (SPEC §B.8.2): channel_disabled, no_policy, snoozed, storm, flapping, throttled, verbosity, duplicate_render. Snooze outranks the automatic dampers because it is a deliberate human act and therefore the most actionable explanation.';

-- +goose Down

-- Reverse of the widening.
ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('no_policy','throttled','storm','flapping','verbosity',
                        'channel_disabled','duplicate_render'));

-- Reverse of the narrowing. There is NO downlevel value for reason='snoozed' or
-- 'unsnoozed', so this deliberately fails on a database that has recorded one
-- rather than rewriting it into a lie (CONTEXT.md §6: never claim resolved when
-- we mean expired -- the same rule applies to every enum in this schema).
UPDATE notifications SET reason = 'escalation', updated_at = now() WHERE reason = 'unacked_reminder';

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','enriched','rule_changed','comment','escalation','storm'));

COMMENT ON COLUMN notifications.reason IS 'The §H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply.';
COMMENT ON COLUMN notifications.suppressed_reason IS 'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6).';
