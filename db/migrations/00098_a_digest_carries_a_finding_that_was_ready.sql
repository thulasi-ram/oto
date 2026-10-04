-- ADR 0053 §2, §4: A DIGEST CARRIES A FINDING THAT WAS READY WHEN ITS WINDOW CLOSED, AND
-- NEVER WAITS FOR ONE THAT WAS NOT (git-bug 3e96f5a). "Subjects are case | incident | digest |
-- policy"; a digest's Finding becomes its body for the windows a policy asked for. Five
-- changes:
--
--   1. `investigations_subjkind_ck` admits `digest`, and a digest run names its WINDOW:
--      `subject_id` is the policy (the POLICY half of the pair, exactly as on the digest's
--      own `notifications` row, 00058) and `digest_window_start` / `digest_window_end` are
--      the window half. Two UUIDs hashed into one would resolve against no table — 00058
--      refused that for the notification and this refuses it for the run.
--      `investigations_digest_window_uniq` makes it ONE run per policy per window: the
--      arming tick is idempotent by state, and a window whose run was `skipped` (a kill
--      switch, the day's budget) is not re-armed — the skip is the record.
--   2. `notification_policies.digest_investigator_id`: the policy ASKS. Which Investigator
--      summarises its windows is the operator's word on the POLICY — "for windows a policy
--      asked for" — and NULL, the default, is every policy that did not ask. It requires a
--      digest window (`policies_digest_investigator_ck`): an Investigator for a summary that
--      is never sent is a knob nothing reads. The foreign key is COMPOSITE on
--      (org_id, digest_investigator_id), over the new `investigators_org_id_uniq`, so a
--      policy can name only its own org's Investigator, and deleting the Investigator clears
--      the column alone (`ON DELETE SET NULL (digest_investigator_id)`, PG 15+): the policy
--      goes back to the built-in body, it is not deleted.
--   3. `notifications.digest_finding`: the Finding, COPIED onto the digest at the moment it
--      was sent. ⭐ A DIGEST READS NOTHING AT CLAIM TIME (notification/service.ViewService.
--      Build: everything a digest asserts is on its row), and the Finding joins that rule
--      rather than breaking it — so a retried delivery renders the same body, a Finding that
--      ends after the send can never reach the message, and deleting the Investigator does
--      not rewrite what was posted. NULL is the built-in body: no run, or one that was
--      queued, running, failed or skipped when the window closed.
--
-- ⛔⛔ THE DIGEST NEVER WAITS. The tick that sends a closed window reads whatever usable
-- Finding exists for it at that moment — `completed`, or `exhausted` with the partial
-- Finding it reached (ADR 0053 §6 keeps a partial Finding, and it is marked partial on the
-- card) — and otherwise sends the built-in body. Nothing here holds a digest, retries it, or
-- amends it later: a Finding that arrives after the send is kept on its run and posted
-- nowhere. A Finding is never an input to WHETHER a digest is sent (ADR 0053 §2).
--
-- ⛔ `enrichments_subjkind_ck` IS NOT WIDENED, AND THE TICKET ASKED FOR IT. An Enrichment is
-- keyed by (subject_kind, subject_id, enricher) and REPLACED by the next run (00092's
-- header), so a digest Enrichment would be keyed by the policy alone and every window would
-- overwrite the last — it cannot name the window the digest needs. And nothing reads it: a
-- digest has no card, no API and no SSE stream that shows Enrichments. The Finding lives on
-- its run (`investigations.finding`) and on the digest that carried it (3. above).
--
-- ⭐ THE RUN STARTS AHEAD OF THE CLOSE, BY A LEAD THE INVESTIGATOR'S OWN BUDGET SETS. The
-- `investigations.digest` tick (once a minute, per tenant) arms a window's run once
-- `now >= window_end - lead`, where lead = the Investigator's wall-time budget plus two
-- minutes (one tick of arming latency, one of start latency), capped at half the window —
-- a run that started earlier would summarise less than half of what it is about.
-- (investigator/domain.DigestLead.) A run that is slower than that, or waits on the org's
-- concurrency past the close, simply is not used.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6): both window instants are the
-- application's arithmetic, never the database's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One widened CHECK, three nullable columns release N-1
-- neither reads nor writes, two new CHECKs every existing row satisfies (all three columns
-- NULL), one unique index over a set no existing row is in, and one unique index over an
-- existing primary key. A release-N-1 writer produces none of the new values.

-- +goose Up

ALTER TABLE investigations DROP CONSTRAINT investigations_subjkind_ck;
ALTER TABLE investigations ADD  CONSTRAINT investigations_subjkind_ck
  CHECK (subject_kind IN ('case','incident','digest'));

ALTER TABLE investigations
  ADD COLUMN digest_window_start TIMESTAMPTZ,
  ADD COLUMN digest_window_end   TIMESTAMPTZ;

ALTER TABLE investigations ADD CONSTRAINT investigations_digest_window_ck CHECK (
     (subject_kind =  'digest' AND digest_window_start IS NOT NULL AND digest_window_end > digest_window_start)
  OR (subject_kind <> 'digest' AND digest_window_start IS NULL AND digest_window_end IS NULL)
);

CREATE UNIQUE INDEX investigations_digest_window_uniq
  ON investigations (org_id, subject_id, digest_window_start, digest_window_end)
  WHERE subject_kind = 'digest';

-- +goose StatementBegin
COMMENT ON COLUMN investigations.digest_window_start IS
  'For a digest subject (00098), the INCLUSIVE start of the window it summarises; subject_id is the notification policy. NULL for every other subject. One run per policy per window (investigations_digest_window_uniq). Its Finding is NOT published as an Enrichment: an Enrichment is keyed by subject alone and would be overwritten every window. The digest sent when the window closed copies the Finding onto its own row (notifications.digest_finding) if, and only if, the run had ended with one by then.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN investigations.digest_window_end IS
  'For a digest subject (00098), the EXCLUSIVE end of the window it summarises — the instant the digest is sent at. The run is armed ahead of it by investigator/domain.DigestLead; the digest never waits past it for the run.';
-- +goose StatementEnd

CREATE UNIQUE INDEX investigators_org_id_uniq ON investigators (org_id, id);

ALTER TABLE notification_policies ADD COLUMN digest_investigator_id UUID;

ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_investigator_fk
  FOREIGN KEY (org_id, digest_investigator_id) REFERENCES investigators (org_id, id)
  ON DELETE SET NULL (digest_investigator_id);

ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_investigator_ck
  CHECK (digest_investigator_id IS NULL OR digest_window_s IS NOT NULL);

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.digest_investigator_id IS
  'ADR 0053 §4 (00098): the Investigator that summarises this policy''s digest windows. Its run is armed ahead of each window''s close; if it has ended with a Finding when the window closes, the digest carries it as its body, and otherwise the digest is sent on time with the built-in body. NULL, the default, is a policy that did not ask. It never decides WHETHER a digest is sent (ADR 0053 §2). Requires digest_window_s; the same org''s Investigator only (composite FK); cleared when the Investigator goes.';
-- +goose StatementEnd

ALTER TABLE notifications ADD COLUMN digest_finding JSONB;

ALTER TABLE notifications ADD CONSTRAINT notifications_digest_finding_ck CHECK (
  digest_finding IS NULL OR (subject_kind = 'digest' AND jsonb_typeof(digest_finding) = 'object')
);

-- +goose StatementBegin
COMMENT ON COLUMN notifications.digest_finding IS
  'ADR 0053 §4 (00098): the Investigator''s Finding this digest carried as its body, COPIED when the digest was sent — investigation id, Investigator name and version, the Finding, its class, whether it was partial, and when it was concluded. NULL is the built-in body: the window''s run had not ended with a Finding when the window closed, or the policy asked for none. A digest never waits for a Finding and is never amended with a later one.';
-- +goose StatementEnd

-- +goose Down

-- The copies go with the column. Every digest that carried one is still a valid digest
-- without it — the built-in body is what the release below renders.
ALTER TABLE notifications DROP CONSTRAINT notifications_digest_finding_ck;
ALTER TABLE notifications DROP COLUMN digest_finding;

ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_investigator_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_investigator_fk;
ALTER TABLE notification_policies DROP COLUMN digest_investigator_id;

DROP INDEX investigators_org_id_uniq;

-- ⛔ THE DIGEST RUNS GO WITH THE SUBJECT THE RELEASE BELOW CANNOT NAME, for 00095's reason:
-- `investigations_frozen` guards UPDATE, not DELETE, and their Steps and Suggestions go by
-- the cascade — the record going with its owner.
DELETE FROM investigations WHERE subject_kind = 'digest';

DROP INDEX investigations_digest_window_uniq;
ALTER TABLE investigations DROP CONSTRAINT investigations_digest_window_ck;
ALTER TABLE investigations DROP COLUMN digest_window_end;
ALTER TABLE investigations DROP COLUMN digest_window_start;

-- Byte-identical to 00095's Up.
ALTER TABLE investigations DROP CONSTRAINT investigations_subjkind_ck;
ALTER TABLE investigations ADD  CONSTRAINT investigations_subjkind_ck
  CHECK (subject_kind IN ('case','incident'));
