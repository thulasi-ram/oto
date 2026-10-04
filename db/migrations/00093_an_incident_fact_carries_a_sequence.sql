-- ADR 0052 §5: EVERY INCIDENT FACT CARRIES A PER-INCIDENT SEQUENCE (owner ruling,
-- 2026-10-04). An Incident's facts are separate notifications with separate delivery
-- jobs and separate retry budgets, so a receiver can be handed them out of order: the
-- `quiet` a removal produced can land before the `case_removed` that caused it, because
-- the removal's delivery hit a 503 and retried. Nothing on the envelope let a receiver
-- tell which came first -- `delivered_at` is when oto SENT it, which is exactly the
-- thing a retry reorders. The `sequence` is the order the facts HAPPENED in, and a
-- receiver drops a fact older than the highest sequence it has seen for that Incident.
--
-- ⭐⭐ ALLOCATED IN THE TRANSACTION THAT RECORDS THE FACT, BY THE INCIDENT'S OWN ROW.
-- `incidents.fact_sequence` is bumped by an UPDATE ... RETURNING in the same transaction
-- that enqueues the fact's `notify.incident` job (ADR 0001's outbox), so a fact that
-- rolled back spent nothing anyone saw and a fact that committed has its number for good.
-- The UPDATE takes the Incident's row lock -- the very lock the membership verbs and the
-- Case-ending observer already take before they count (`IncidentRepository.Lock`) -- and
-- holds it to COMMIT, so two transactions declaring facts about one Incident allocate in
-- the order they commit. A sequence allocated from a global SEQUENCE could not promise
-- that: nextval() is handed out at call time, not commit time, and the later number
-- could commit first.
--
-- ⭐ THE NUMBER IS FROZEN ON THE NOTIFICATION ROW, SO A RETRY REUSES IT. The job carries
-- it to `notify.incident`, which writes it to `notifications.incident_sequence` beside
-- the §C.7 key; a redelivered job meets the key and reads the row it already wrote, and
-- every delivery attempt renders from that row. One fact, one number, however many
-- times it is sent.
--
-- ⚠️ IT IS PER INCIDENT, 1-BASED, AND STARTS AT `drawn`, but it is NOT GAPLESS ON THE
-- WIRE: a fact an org routes nowhere still takes its number (it is recorded as
-- `no_policy`), so a receiver sees 1, 2, 4. It is an order, not a count -- the same
-- contract as `incidents.number`.
--
-- ⛔ NO BACKFILL. An Incident drawn before this migration starts at 0 and numbers its
-- next fact 1, which is not its `drawn`. The facts it already declared carried no
-- sequence, so there is nothing a receiver could have ordered against, and inventing
-- a count from the `notifications` rows would miss every fact still in the queue and
-- every row the retention sweep has reaped.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). A defaulted NOT NULL counter and a nullable column,
-- each with a CHECK every existing row satisfies. A release-N pod never names either:
-- its draws start the counter at 0, its notification rows carry NULL, and its
-- envelopes omit `sequence` -- which is what a fact declared before this release does.

-- +goose Up

ALTER TABLE incidents ADD COLUMN fact_sequence BIGINT NOT NULL DEFAULT 0;

ALTER TABLE incidents ADD CONSTRAINT incidents_fact_sequence_ck CHECK (fact_sequence >= 0);

-- +goose StatementBegin
COMMENT ON COLUMN incidents.fact_sequence IS
  'The sequence of this Incident''s LATEST declared fact (ADR 0052 §5): 0 before any, then 1 for drawn, bumped by UPDATE ... RETURNING in the transaction that enqueues each notify.incident job, under the Incident''s row lock, so facts are numbered in commit order. Carried on the oto.notification.v1 envelope as incident.sequence; a receiver drops a fact older than the highest it has seen. Not gapless on the wire (an unrouted fact still takes a number). An Incident drawn before 00093 counts from its first fact after it.';
-- +goose StatementEnd

ALTER TABLE notifications ADD COLUMN incident_sequence BIGINT;

-- Only an Incident fact carries one, and only from 1. NULL on every other row, and on an
-- Incident fact declared before 00093 -- whose job carried none.
ALTER TABLE notifications ADD CONSTRAINT notifications_incident_seq_ck
  CHECK (incident_sequence IS NULL OR (subject_kind = 'incident' AND incident_sequence >= 1));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.incident_sequence IS
  'The per-Incident sequence of the fact this Incident notification declares (00093), frozen at evaluation from the notify.incident job, which was handed it by the transaction that made the fact true. Every delivery attempt renders it as incident.sequence, so a retry is the same fact with the same number. NULL for every non-Incident row, and for an Incident fact declared before 00093.';
-- +goose StatementEnd

-- +goose Down

-- ⚠️ THE DOWN DROPS ORDER, NOT DATA A RELEASE BELOW CAN READ. The release below 00093
-- neither writes nor renders a sequence, so its envelopes simply omit the field again;
-- a receiver that drops facts older than the highest sequence it has seen must treat an
-- absent one as unordered, which docs/setup/webhook.md tells it to.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_incident_seq_ck;
ALTER TABLE notifications DROP COLUMN IF EXISTS incident_sequence;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_fact_sequence_ck;
ALTER TABLE incidents DROP COLUMN IF EXISTS fact_sequence;
