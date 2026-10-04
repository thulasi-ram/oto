-- ADR 0054 §1, §2, §4, §5, §6: AN INVESTIGATOR PROPOSES A REMEDY, AND TWO DIFFERENT APPROVERS
-- ON ITS TOOLSERVER MUST SAY YES (git-bug 4148256). "A change to a cluster that an Investigator
-- proposes and oto executes only after a human approves it: proposed → approved → executed |
-- failed, or declined, or expired, each by a named actor." Six changes:
--
--   1. `remedies` — one row per Remedy a run's Finding proposed. It names the write Tool that
--      would carry it out (`tool_server_id`, `tool_server_name`, `tool_name`) with the EXACT
--      arguments it would be sent, or names none — "no configured Tool can carry this out" —
--      and then it can never be approved: `remedies_no_tool_ck` keeps a Tool-less row out of
--      every state but proposed, declined and expired.
--   2. `remedy_approvals` — one row per approving human, UNIQUE per (Remedy, user): the same
--      person approving twice counts once, in the schema and not only in Go.
--   3. `remedy_transitions` — one row per transition, by a named actor (the Investigator, a
--      user, or oto itself), with the Incident the transition was declared to as a fact, or
--      NULL when the Remedy's subject was in no Incident (recorded, never invented).
--   4. `notifications_reason_ck` gains the six Remedy facts and `notifications.remedy` carries
--      the snapshot each one declares; `policies_reasons_ck`'s ceiling follows the enum to 27.
--   5. `orgs.settings` gains `remedy_approval_window_s`, said in the column's comment.
--
-- ⭐⭐ THE APPROVED ARGUMENTS ARE THE EXECUTED ARGUMENTS, AND THE SCHEMA HOLDS IT. `arguments`
-- is the compact JSON object the Investigator proposed, byte for byte, and
-- `remedies_arguments_hash_ck` pins `arguments_sha256` to the SHA-256 of exactly those bytes.
-- `remedies_frozen` refuses any UPDATE that touches either — or the Tool, the target, the
-- description, the required approvals or who proposed it — so what an approver read is what
-- the executor sends, and an approval row carries the hash its approver saw.
--
-- ⭐⭐ EVERY TRANSITION IS ONE OF A CLOSED SET, AND A TERMINAL STATE IS FROZEN. `remedies_frozen`
-- refuses an UPDATE of an executed, failed, declined or expired row, an UPDATE that does not
-- move the state, and any move outside:
--
--     proposed  → approved | declined | expired
--     approved  → executing | declined | expired | failed
--     executing → executed | failed
--
-- `failed` from `approved` is the executor refusing to send — the Tool is gone, the
-- arguments no longer hash, an approver's grant was withdrawn — and is never a retry: a
-- failed Remedy stays failed, and a retry is a new Remedy and a new approval (§6).
-- `executing` is the executor's claim, committed BEFORE the call, so a worker that dies
-- mid-call leaves the row `executing` — never `approved` — and nothing can send it twice.
--
-- ⭐ `required_approvals` IS SET AT PROPOSAL AND IS 2. Until operator-written risk rules exist
-- (git-bug eb4f21b) every Remedy needs two approvals from two different grant holders. It is a
-- column, not a constant, so the rules can lower it per Remedy; the CHECK admits 1 and 2.
--
-- ⭐ EXPIRY IS RECORDED, NEVER SILENT. `expires_at` is stamped at proposal and again at
-- approval (the org's `remedy_approval_window_s` after each). A row past it reads as expired
-- at once, and the `remedies.sweep` job records the transition — by `system`, declared like
-- every other — so the record and the fact both say so.
--
-- ⚠️ NO FOREIGN KEY ONTO THE TOOLSERVER OR THE INCIDENT. A Remedy must outlive the ToolServer
-- it named: one removed after the proposal makes the Remedy unapprovable and unexecutable,
-- which is answered when someone tries, not by losing the record. `declared_incident_id`
-- names an Incident the way `investigations.subject_id` does, without one.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6): every time here is the app's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three new tables release N-1 never reads; one widened
-- CHECK and one raised ceiling no existing row is near; one nullable column whose CHECK every
-- existing row satisfies (none carries a Remedy reason); one comment.

-- +goose Up

CREATE TABLE remedies (
  id                 UUID        PRIMARY KEY,
  org_id             UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  investigation_id   UUID        NOT NULL REFERENCES investigations(id) ON DELETE CASCADE,
  -- The Investigation's subject, copied: a Remedy is about a Case or an Incident.
  subject_kind       TEXT        NOT NULL,
  subject_id         UUID        NOT NULL,
  -- Who proposed it: always an Investigator, named with its version.
  proposed_by_label  TEXT        NOT NULL,
  -- The write Tool that would carry it out, or all three NULL for none.
  tool_server_id     UUID,
  tool_server_name   TEXT,
  tool_name          TEXT,
  -- The exact arguments, as compact JSON, and the SHA-256 of those bytes. NULL with no Tool.
  arguments          TEXT,
  arguments_sha256   TEXT,
  target             TEXT        NOT NULL,
  description        TEXT        NOT NULL,
  required_approvals INT         NOT NULL,
  state              TEXT        NOT NULL,
  proposed_at        TIMESTAMPTZ NOT NULL,
  expires_at         TIMESTAMPTZ NOT NULL,
  approved_at        TIMESTAMPTZ,
  executing_at       TIMESTAMPTZ,
  ended_at           TIMESTAMPTZ,
  failure_reason     TEXT,
  detail             TEXT,
  -- What the write Tool answered, redacted and capped. Only a Remedy that was sent has one.
  result             TEXT,

  CONSTRAINT remedies_subjkind_ck   CHECK (subject_kind IN ('case','incident')),
  CONSTRAINT remedies_state_ck      CHECK (state IN ('proposed','approved','executing','executed','failed','declined','expired')),
  CONSTRAINT remedies_tool_ck       CHECK (
       (tool_server_id IS NULL AND tool_server_name IS NULL AND tool_name IS NULL AND arguments IS NULL AND arguments_sha256 IS NULL)
    OR (tool_server_id IS NOT NULL AND tool_server_name IS NOT NULL AND tool_name IS NOT NULL
        AND arguments IS NOT NULL AND arguments_sha256 IS NOT NULL)
  ),
  -- ⭐ A Remedy with no Tool can never be approved, so it can never be anything a Tool did.
  CONSTRAINT remedies_no_tool_ck    CHECK (tool_server_id IS NOT NULL OR state IN ('proposed','declined','expired')),
  CONSTRAINT remedies_tool_name_ck  CHECK (
    (tool_server_name IS NULL OR length(tool_server_name) BETWEEN 1 AND 24)
    AND (tool_name IS NULL OR char_length(tool_name) BETWEEN 1 AND 128)
  ),
  CONSTRAINT remedies_arguments_ck  CHECK (
    arguments IS NULL OR (char_length(arguments) BETWEEN 2 AND 16384 AND jsonb_typeof(arguments::jsonb) = 'object')
  ),
  -- ⭐⭐ The hash IS the hash of these bytes: the executor compares, and so does this.
  CONSTRAINT remedies_arguments_hash_ck CHECK (
    arguments IS NULL OR arguments_sha256 = encode(sha256(convert_to(arguments, 'UTF8')), 'hex')
  ),
  CONSTRAINT remedies_target_ck     CHECK (char_length(btrim(target)) BETWEEN 1 AND 500),
  CONSTRAINT remedies_description_ck CHECK (char_length(btrim(description)) BETWEEN 1 AND 2000),
  CONSTRAINT remedies_label_ck      CHECK (length(btrim(proposed_by_label)) BETWEEN 1 AND 200),
  CONSTRAINT remedies_approvals_ck  CHECK (required_approvals BETWEEN 1 AND 2),
  CONSTRAINT remedies_expiry_ck     CHECK (expires_at > proposed_at),
  -- Each instant is set exactly when its state has been reached, and time runs forward.
  -- A declined or expired Remedy may or may not have been approved first; every other
  -- state says which.
  CONSTRAINT remedies_approved_ck   CHECK (
    (   (state = 'proposed' AND approved_at IS NULL)
     OR (state IN ('approved','executing','executed','failed') AND approved_at IS NOT NULL)
     OR state IN ('declined','expired'))
    AND (approved_at IS NULL OR approved_at >= proposed_at)
  ),
  CONSTRAINT remedies_executing_ck  CHECK (
    (state IN ('executing','executed') AND executing_at IS NOT NULL)
    OR (state IN ('proposed','approved','declined','expired') AND executing_at IS NULL)
    OR state = 'failed'
  ),
  CONSTRAINT remedies_executing_time_ck CHECK (
    executing_at IS NULL OR (approved_at IS NOT NULL AND executing_at >= approved_at)
  ),
  CONSTRAINT remedies_ended_ck      CHECK (
    (ended_at IS NULL) = (state IN ('proposed','approved','executing'))
    AND (ended_at IS NULL OR ended_at >= proposed_at)
  ),
  CONSTRAINT remedies_failure_ck    CHECK (
    (state = 'failed') = (failure_reason IS NOT NULL)
    AND (failure_reason IS NULL OR failure_reason IN
      ('tool_error','outcome_unknown','tool_unavailable','arguments_changed','approvals_withdrawn'))
  ),
  CONSTRAINT remedies_detail_ck     CHECK (detail IS NULL OR char_length(detail) BETWEEN 1 AND 2000),
  -- Only a Remedy that was sent has an answer to keep.
  CONSTRAINT remedies_result_ck     CHECK (
    result IS NULL OR (executing_at IS NOT NULL AND char_length(result) <= 16384)
  )
);

-- Serves: one Investigation's Remedies, in the order they were proposed.
CREATE INDEX remedies_run_idx ON remedies (org_id, investigation_id, proposed_at, id);

-- Serves: the sweep — Remedies still waiting on a human or the executor, by deadline.
CREATE INDEX remedies_open_idx ON remedies (org_id, expires_at)
  WHERE state IN ('proposed','approved','executing');

-- +goose StatementBegin
COMMENT ON TABLE remedies IS
  'ADR 0054 (00100, git-bug 4148256): a change to a cluster an Investigator proposed with its Finding, which oto executes through the named write Tool only after the required approvals from DIFFERENT holders of the grant on that Tool''s ToolServer (remedy_approver_grants). proposed -> approved -> executing -> executed | failed, or declined, or expired, each transition recorded in remedy_transitions by a named actor and declared to the Incident as a fact. A Remedy with no Tool says no configured Tool can carry it out and can never be approved. The proposal (Tool, arguments and their SHA-256, target, description, required approvals) is frozen by remedies_frozen; terminal states are frozen; a failed Remedy is never retried.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedies.arguments IS
  'The exact arguments the write Tool would be sent, as the compact JSON object the Investigator proposed. What an approver is shown first and what the executor sends, byte for byte: arguments_sha256 is the SHA-256 of these bytes (remedies_arguments_hash_ck) and the executor refuses to send when they differ. NULL for a Remedy no configured Tool can carry out.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedies.required_approvals IS
  'How many DIFFERENT grant holders must approve before it runs, set at proposal. 2 for every Remedy until operator-written risk rules exist (git-bug eb4f21b), which may lower it per Remedy; a model may only ever raise it (ADR 0054 §3).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedies.expires_at IS
  'When a proposed Remedy, or an approved one not yet sent, expires: remedy_approval_window_s after its proposal, and again after its approval. A Remedy past it reads as expired at once and is recorded expired by remedies.sweep, as a transition by system, declared like any other.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION remedies_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'proposed' THEN
      RAISE EXCEPTION 'remedy % is inserted %: a Remedy begins proposed, and every later state is a transition', NEW.id, NEW.state;
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.state IN ('executed','failed','declined','expired') THEN
    RAISE EXCEPTION 'remedy % is % and is frozen: a Remedy that ended is never rewritten, and a failed one is never retried', OLD.id, OLD.state;
  END IF;
  IF (NEW.id, NEW.org_id, NEW.investigation_id, NEW.subject_kind, NEW.subject_id, NEW.proposed_by_label,
      NEW.tool_server_id, NEW.tool_server_name, NEW.tool_name, NEW.arguments, NEW.arguments_sha256,
      NEW.target, NEW.description, NEW.required_approvals, NEW.proposed_at)
     IS DISTINCT FROM
     (OLD.id, OLD.org_id, OLD.investigation_id, OLD.subject_kind, OLD.subject_id, OLD.proposed_by_label,
      OLD.tool_server_id, OLD.tool_server_name, OLD.tool_name, OLD.arguments, OLD.arguments_sha256,
      OLD.target, OLD.description, OLD.required_approvals, OLD.proposed_at) THEN
    RAISE EXCEPTION 'remedy % is a proposal and is never rewritten: what was approved is what is executed', OLD.id;
  END IF;
  IF NOT ((OLD.state = 'proposed'  AND NEW.state IN ('approved','declined','expired'))
       OR (OLD.state = 'approved'  AND NEW.state IN ('executing','declined','expired','failed'))
       OR (OLD.state = 'executing' AND NEW.state IN ('executed','failed'))) THEN
    RAISE EXCEPTION 'remedy % cannot move from % to %: every change to a Remedy is one of its transitions', OLD.id, OLD.state, NEW.state;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER remedies_frozen
  BEFORE INSERT OR UPDATE ON remedies
  FOR EACH ROW EXECUTE FUNCTION remedies_refuse_rewrite();

-- ------------------------------------------------------------ remedy_approvals

CREATE TABLE remedy_approvals (
  id               UUID        PRIMARY KEY,
  org_id           UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  remedy_id        UUID        NOT NULL REFERENCES remedies(id) ON DELETE CASCADE,
  -- ACTOR metadata in the acked_by mould: nulled when the user goes, the label frozen.
  user_id          UUID        REFERENCES users(id) ON DELETE SET NULL,
  user_label       TEXT        NOT NULL,
  -- The hash of the arguments this approver was shown and approved.
  arguments_sha256 TEXT        NOT NULL,
  approved_at      TIMESTAMPTZ NOT NULL,
  CONSTRAINT remedy_approvals_label_ck CHECK (length(btrim(user_label)) BETWEEN 1 AND 200),
  CONSTRAINT remedy_approvals_hash_ck  CHECK (arguments_sha256 ~ '^[0-9a-f]{64}$')
);

-- ⭐⭐ ONE APPROVAL PER PERSON PER REMEDY: the same user approving twice — in the UI and again
-- in Slack — counts once (ADR 0054 §4).
CREATE UNIQUE INDEX remedy_approvals_user_uniq ON remedy_approvals (remedy_id, user_id);

-- Serves: a Remedy's approvals, in order; and the user side of the foreign key.
CREATE INDEX remedy_approvals_remedy_idx ON remedy_approvals (org_id, remedy_id, approved_at, id);
CREATE INDEX remedy_approvals_user_idx ON remedy_approvals (user_id) WHERE user_id IS NOT NULL;

-- +goose StatementBegin
COMMENT ON TABLE remedy_approvals IS
  'One human''s approval of one Remedy (ADR 0054 §4), made only while they held the grant on its ToolServer, with the SHA-256 of the arguments they were shown. UNIQUE per (remedy, user): one person approving twice counts once. APPEND-ONLY: remedy_approvals_append_only refuses every change but the cascade of its Remedy, org or user going.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION remedy_record_refuse_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- ⭐ A CASCADE IS THE RECORD GOING WITH ITS OWNER, and a user's SET NULL is the actor
  -- leaving with the label frozen: both arrive here nested inside the foreign key's own
  -- trigger. A statement issued against the table directly does not.
  IF pg_trigger_depth() > 1 THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'a Remedy''s approvals and transitions are its record: % on % is refused', TG_OP, TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER remedy_approvals_append_only
  BEFORE UPDATE OR DELETE ON remedy_approvals
  FOR EACH ROW EXECUTE FUNCTION remedy_record_refuse_change();

-- ---------------------------------------------------------- remedy_transitions

CREATE TABLE remedy_transitions (
  id                   UUID        PRIMARY KEY,
  org_id               UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  remedy_id            UUID        NOT NULL REFERENCES remedies(id) ON DELETE CASCADE,
  -- NULL for the proposal itself.
  from_state           TEXT,
  to_state             TEXT        NOT NULL,
  actor_kind           TEXT        NOT NULL,
  actor_id             UUID        REFERENCES users(id) ON DELETE SET NULL,
  actor_label          TEXT        NOT NULL,
  at                   TIMESTAMPTZ NOT NULL,
  failure_reason       TEXT,
  detail               TEXT,
  -- The Incident this transition was declared to as a fact; NULL when the Remedy's subject
  -- was in no Incident at the time — recorded, never invented.
  declared_incident_id UUID,
  CONSTRAINT remedy_transitions_move_ck  CHECK (
       (from_state IS NULL        AND to_state = 'proposed')
    OR (from_state = 'proposed'   AND to_state IN ('approved','declined','expired'))
    OR (from_state = 'approved'   AND to_state IN ('executing','declined','expired','failed'))
    OR (from_state = 'executing'  AND to_state IN ('executed','failed'))
  ),
  -- Who moves it: the Investigator proposes; a human approves or declines; oto expires,
  -- claims, and records what the Tool answered.
  CONSTRAINT remedy_transitions_actor_ck CHECK (
       (actor_kind = 'investigator' AND to_state = 'proposed'               AND actor_id IS NULL)
    OR (actor_kind = 'user'         AND to_state IN ('approved','declined'))
    OR (actor_kind = 'system'       AND to_state IN ('expired','executing','executed','failed') AND actor_id IS NULL)
  ),
  CONSTRAINT remedy_transitions_label_ck CHECK (length(btrim(actor_label)) BETWEEN 1 AND 200),
  CONSTRAINT remedy_transitions_failure_ck CHECK (
    (to_state = 'failed') = (failure_reason IS NOT NULL)
    AND (failure_reason IS NULL OR failure_reason IN
      ('tool_error','outcome_unknown','tool_unavailable','arguments_changed','approvals_withdrawn'))
  ),
  CONSTRAINT remedy_transitions_detail_ck CHECK (detail IS NULL OR char_length(detail) BETWEEN 1 AND 2000)
);

-- Serves: a Remedy's history, in order. One proposal per Remedy.
CREATE INDEX remedy_transitions_remedy_idx ON remedy_transitions (org_id, remedy_id, at, id);
CREATE UNIQUE INDEX remedy_transitions_proposal_uniq ON remedy_transitions (remedy_id) WHERE from_state IS NULL;
CREATE INDEX remedy_transitions_actor_idx ON remedy_transitions (actor_id) WHERE actor_id IS NOT NULL;

-- +goose StatementBegin
COMMENT ON TABLE remedy_transitions IS
  'Every transition of every Remedy (ADR 0054 §1), by a named actor: investigator (proposed), user (approved, declined) or system (expired, executing, executed, failed). Each is declared to the Incident as a fact (ADR 0052 §5), and declared_incident_id records which; NULL means the subject was in no Incident and the fact had no outbound target. APPEND-ONLY: remedy_transitions_append_only refuses every change but the cascade of its Remedy, org or actor going.';
-- +goose StatementEnd

CREATE TRIGGER remedy_transitions_append_only
  BEFORE UPDATE OR DELETE ON remedy_transitions
  FOR EACH ROW EXECUTE FUNCTION remedy_record_refuse_change();

-- ------------------------------------------------------------ the six facts

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest',
   'drawn','case_added','case_removed','quiet','active_again','finding',
   'remedy_proposed','remedy_approved','remedy_declined','remedy_expired','remedy_executed','remedy_failed'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, twenty-seven values. Fifteen are facts about one signal or one window; the last twelve are facts about an Incident -- drawn, case_added, case_removed, quiet, active_again (00084), finding (00095: an Investigation of the Incident reached a new Finding) and the six Remedy transitions remedy_proposed, remedy_approved, remedy_declined, remedy_expired, remedy_executed and remedy_failed (00100, ADR 0054 §2) -- and none of them is a resolve, a close or a status: oto declares facts to an incident tool and never commands it (ADR 0052 §5). `quiet` is every member Case closed, never "the incident is over". A Finding never decides whether anyone is told (ADR 0053 §2); `finding` declares it to wherever the org routes Incident facts.';
-- +goose StatementEnd

ALTER TABLE notifications ADD COLUMN remedy JSONB;

ALTER TABLE notifications ADD CONSTRAINT notifications_remedy_ck CHECK (
  (reason IN ('remedy_proposed','remedy_approved','remedy_declined','remedy_expired','remedy_executed','remedy_failed'))
  = (remedy IS NOT NULL)
  AND (remedy IS NULL OR (subject_kind = 'incident' AND jsonb_typeof(remedy) = 'object'))
);

-- +goose StatementBegin
COMMENT ON COLUMN notifications.remedy IS
  'ADR 0054 §2 (00100): the Remedy transition this Incident fact declares, COPIED in the transaction that made it -- the Remedy, its Tool and exact arguments or "no configured Tool can carry this out", its target and description, its approvals so far, the transition, its actor and instant, and why it failed. Set exactly on the six remedy_* reasons. A snapshot, never re-read: a retried delivery renders the same fact.';
-- +goose StatementEnd

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 27
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..27 SPEC H.6 Reason values. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved, 00084 added the five Incident facts, 00095 added finding, 00100 added the six Remedy transitions. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: every narrowing of the Reason vocabulary must strip the value from this column by hand.';
-- +goose StatementEnd

-- ------------------------------------------------------------ orgs.settings

-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine, retention, Investigations and Remedies, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The eleven keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months, default_verbosity, investigations_enabled (ADR 0053 §6, boolean, default true: the org''s Investigation kill switch; absent means enabled), investigation_daily_tokens (ADR 0053 §6, integer 1000..1000000000, default 2000000: input + output tokens per UTC day; past it a new Investigation is recorded skipped with reason budget), investigation_concurrency (ADR 0053 §6, integer 1..32, default 2: the most Investigations running at once; one past it waits queued) and remedy_approval_window_s (ADR 0054 §2, integer 60..86400, default 3600: how long a proposed Remedy waits for its approvals, and an approved one for its execution, before it is recorded expired). ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to what 00094 shipped. A row that wrote `remedy_approval_window_s` keeps it
-- in the document, which the release below reads past: an unknown JSONB key is not a column
-- it can fail on.
-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine, retention and Investigations, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The ten keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months, default_verbosity, investigations_enabled (ADR 0053 §6, boolean, default true: the org''s Investigation kill switch; absent means enabled), investigation_daily_tokens (ADR 0053 §6, integer 1000..1000000000, default 2000000: input + output tokens per UTC day; past it a new Investigation is recorded skipped with reason budget) and investigation_concurrency (ADR 0053 §6, integer 1..32, default 2: the most Investigations running at once; one past it waits queued). ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- ⛔ THE SIX FACTS GO WITH THE VOCABULARY THE RELEASE BELOW CANNOT NAME, for 00095's reason:
-- their notifications are deleted (their deliveries by the cascade), and the six values are
-- stripped from every policy by hand — a policy that named nothing else is deleted, since an
-- empty reason set is not a policy.
DELETE FROM notifications
 WHERE reason IN ('remedy_proposed','remedy_approved','remedy_declined','remedy_expired','remedy_executed','remedy_failed');
DELETE FROM notification_policies
 WHERE reasons <@ ARRAY['remedy_proposed','remedy_approved','remedy_declined','remedy_expired','remedy_executed','remedy_failed']::text[];
UPDATE notification_policies
   SET reasons = array_remove(array_remove(array_remove(array_remove(array_remove(array_remove(reasons,
                 'remedy_proposed'), 'remedy_approved'), 'remedy_declined'), 'remedy_expired'), 'remedy_executed'), 'remedy_failed')
 WHERE reasons && ARRAY['remedy_proposed','remedy_approved','remedy_declined','remedy_expired','remedy_executed','remedy_failed']::text[];

-- Byte-identical to 00095's Up.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 21
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..21 SPEC H.6 Reason values. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved, 00084 added the five Incident facts, 00095 added finding. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: every narrowing of the Reason vocabulary must strip the value from this column by hand.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_remedy_ck;
ALTER TABLE notifications DROP COLUMN remedy;

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest',
   'drawn','case_added','case_removed','quiet','active_again','finding'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, twenty-one values. Fifteen are facts about one signal or one window; the last six are facts about an Incident -- drawn, case_added, case_removed, quiet, active_again (00084) and finding (00095: an Investigation of the Incident reached a new Finding) -- and none of them is a resolve, a close or a status: oto declares facts to an incident tool and never commands it (ADR 0052 §5). `quiet` is every member Case closed, never "the incident is over". A Finding never decides whether anyone is told (ADR 0053 §2); `finding` declares it to wherever the org routes Incident facts.';
-- +goose StatementEnd

-- ⛔ THE REMEDIES GO WITH THEIR TABLES. The release below has no Remedy; one that was executed
-- changed the cluster through the operator's ToolServer, and that change stays where it was
-- made — only oto's record of it goes. The triggers go with their tables, the shared function
-- after both.
DROP TABLE remedy_transitions;
DROP TABLE remedy_approvals;
DROP FUNCTION remedy_record_refuse_change();
DROP TABLE remedies;
DROP FUNCTION remedies_refuse_rewrite();
