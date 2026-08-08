-- ADR 0020 (SPEC §H.5, §H.6, §D.8) -- notifications.reason gains 'severity_raised'.
--
-- WHY. ADR 0008 made chat.update the primary verb, and chat.update is COMPLETELY
-- SILENT: no notification, no unread badge, nothing rises in the channel. A card
-- can go from warning to critical and every person in the channel can miss it.
-- ADR 0020 makes that transition a broadcast -- but a broadcast is a property of
-- a Reason, and notifications_reason_ck enumerated EIGHTEEN values, none of which
-- was a severity change. A severity bump arrived as part of 'new_alerts' or as a
-- bare root update, so the clearest case for broadcasting was the one case oto
-- could not express. This migration is the fix, and ADR 0020 records it as the
-- largest single cost of that decision.
--
-- 'severity_raised' is a RISE ONLY. A decrease is good news arriving quietly,
-- which is exactly what update-in-place is for, so it gets no Reason and no row.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Adding a value is a pure WIDENING, so the
-- expand phase is the whole of the Up and there is nothing to backfill and
-- nothing to contract:
--
--   1. expand   -- swap notifications_reason_ck for the union of the old
--                  vocabulary and 'severity_raised'. Every existing row satisfies
--                  it, and a release-N writer that has never heard of
--                  'severity_raised' satisfies it too, so release N and release
--                  N+1 run side by side through the whole deploy.
--   2. backfill -- NONE. No existing row is wrong; nothing is being renamed.
--   3. contract -- NONE ON THE WAY UP. The contract is the Down, below, and it is
--                  the phase that must be able to FAIL.
--
-- The ORDER of the two releases is the load-bearing part: this migration must be
-- applied BEFORE the code that emits the value, or the first severity rise after
-- deploy is a 23514. That is the ordinary expand-first discipline and it is why
-- the Up is a widening rather than a swap.
--
-- Constraint names are a RUNTIME CONTRACT (SPEC §L.9, CONTEXT.md §6): they come
-- back as errs.Error.Code on 23514/23505. notifications_reason_ck keeps its name
-- on the far side of this migration, in both directions.

-- +goose Up

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm','severity_raised'));

-- vocab:allow — the column comment names the retired word once, to say it is retired. It carries forward verbatim from 00018, which is the migration that retired it.
COMMENT ON COLUMN notifications.reason IS
  'The §H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply, and (since ADR 0020) whether the reply is BROADCAST into the channel. unacked_reminder is oto ONE reminder stage (§G.9.1); never escalation, which is a scope-banned word (§A.1). snoozed and unsnoozed announce a snooze beginning and ending, and are the ONLY two reasons a snooze does not suppress (§B.8.4): a snooze that cannot announce itself is the silent suppression §B.6 forbids. severity_raised is an Alert severity CLASS increasing (warning -> critical) and exists because chat.update is silent: without it the card goes from amber to red and the channel is told nothing. A severity DECREASE has no Reason: good news is allowed to arrive quietly.';

-- +goose Down

-- The contract phase, and it is DELIBERATELY ABLE TO FAIL.
--
-- There is NO downlevel value for reason='severity_raised'. Rewriting one into
-- 'new_alerts' would be recording that instances were added when what happened is
-- that an existing alert got worse -- the same class of lie as claiming resolved
-- when we mean expired, which CONTEXT.md §6 forbids for every enum in this schema.
-- So this narrowing simply re-adds the old predicate: on a database that has
-- recorded a severity rise it raises 23514 and STOPS, naming
-- notifications_reason_ck. That is the correct outcome. A down migration that
-- silently rewrote history to make itself succeed would be worse than one that
-- refuses to run.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm'));

COMMENT ON COLUMN notifications.reason IS
  'The §H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply. unacked_reminder is oto ONE reminder stage (§G.9.1); never escalation, which is a scope-banned word (§A.1). snoozed and unsnoozed announce a snooze beginning and ending, and are the ONLY two reasons a snooze does not suppress (§B.8.4): a snooze that cannot announce itself is the silent suppression §B.6 forbids.';
