-- ADR 0053 §1, §3, §6: AN INVESTIGATION RUNS AGAINST A CASE (git-bug 180a525). An
-- INVESTIGATOR is a named, versioned configuration — which model, which prompt, which
-- Tools it may call, and its budgets; an INVESTIGATION is one run of one Investigator
-- version against one subject, frozen once it ends; a STEP is one immutable entry in
-- its transcript; the FINDING is what it concluded, published as an Enrichment
-- (`investigator.<name>`) so cards, API and SSE need nothing new.
--
-- ⭐⭐ WHAT IS VERSIONED IS EXACTLY WHAT ADR 0053 §6 NAMES, AND NOTHING ELSE. "Changing an
-- Investigator's model, prompt or allowlist makes a new version; a Finding names the
-- version that produced it." So `investigator_versions` holds the model (the endpoint row
-- it dials AND the (base_url, model) identity pinned from it), the prompt and the Tool
-- allowlist, and its rows are never updated. `enabled` and the three per-run budgets live
-- on `investigators` and change in place: they decide whether and how long a run may go,
-- not what produced a Finding, and every Investigation copies the budgets it ran under
-- onto its own row, so "what was it allowed to spend?" is still a fact about that run.
--
-- ⭐ THE PIN IS (base_url, model), COPIED, NOT ONLY THE ENDPOINT ROW'S ID (00091's header).
-- A run re-reads the endpoint row and refuses to start when its identity no longer equals
-- the version's pin (`model_changed`), so a Finding can never name a model it was not
-- produced by.
--
-- ⭐⭐ A STEP IS APPEND-ONLY, AND THE DATABASE SAYS SO. CONTEXT.md: "If you would ever
-- UPDATE it, it is not a Step." `investigation_steps_append_only` refuses every UPDATE,
-- and every DELETE that is not the cascade of its org or its Investigation going — a
-- record going with its owner is not a transcript being rewritten. An Investigation row
-- is written while it is `queued` or `running` and FROZEN once it ends:
-- `investigations_frozen` refuses an UPDATE of a row whose status is terminal. That is
-- what makes "why did it say that?" answerable a year later: nothing that answered it
-- can have moved.
--
-- ⭐ `subject_kind` IS A CLOSED SET OF ONE, SHAPED FOR FOUR. ADR 0053 §4 names `case |
-- incident | digest | policy`; only `case` has a run path in this ticket, so the CHECK
-- admits only `case`, and a later ticket widens it the way 00052 and 00084 widened theirs.
-- `subject_id` carries no foreign key for the reason `enrichments.subject_id` carries
-- none: it is polymorphic, and `subject_kind` names the table it points into.
--
-- ⭐ `alert_key` IS A COPY, TAKEN AT REQUEST TIME, FOR ONE READ. The built-in Tool "prior
-- Findings on the same alert_key" (ADR 0053 §3: memory is oto's own history, not a new
-- store) is one index range over this table, never a join into `alerts` — a module's own
-- table answering its own question. An Alert's key never changes.
--
-- ⛔ THE KILL SWITCH IS RECORDED, NOT SILENT (§6: "Enabled — Nothing starts"; "hitting
-- one is recorded, never silent"). A request while the org or the Investigator is
-- disabled, and a queued run whose switch was turned off before it began, are rows with
-- status `skipped` and reason `disabled` — nothing called a model and nothing was spent,
-- and the timeline of a Case still says somebody asked.
--
-- ⛔ NO API KEY CAN REACH A STEP. The key lives sealed in `channel_credentials` (00091);
-- the run loop holds a ModelProvider that already carries it and never sees the string,
-- so no column below is a place it could be written from.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP. The application stamps every one from the injected
-- clock (CONTEXT.md §6, migrations 00032-00034).
--
-- ⛔ NOTHING HERE IS READ BY THE NOTIFICATION PATH. A Finding never decides whether anyone
-- is told (ADR 0053 §2); the run is a River job on a queue of its own (`investigate`), so
-- it cannot hold a worker an enrichment or a notification is waiting for.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Four new tables, two trigger functions, and a
-- comment on `orgs.settings` naming the one new key, which release N never writes.

-- +goose Up

-- ------------------------------------------------------------- investigators

CREATE TABLE investigators (
  id              UUID        PRIMARY KEY,
  org_id          UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  -- ⭐ THE ALPHABET IS THE ENRICHER NAME'S. A Finding is published as the Enrichment
  -- `investigator.<name>`, and `enrichments_name_ck` admits `^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`
  -- — so a name with a hyphen, an underscore or a capital is one whose Finding could not
  -- be stored. Refusing it here is the same rule said where an operator can still act.
  name            TEXT        NOT NULL,
  enabled         BOOLEAN     NOT NULL,
  max_steps       INT         NOT NULL,
  max_tokens      BIGINT      NOT NULL,
  max_wall_s      INT         NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL,
  CONSTRAINT investigators_name_ck   CHECK (name ~ '^[a-z][a-z0-9]{0,62}$'),
  CONSTRAINT investigators_steps_ck  CHECK (max_steps BETWEEN 1 AND 100),
  CONSTRAINT investigators_tokens_ck CHECK (max_tokens BETWEEN 1000 AND 2000000),
  CONSTRAINT investigators_wall_ck   CHECK (max_wall_s BETWEEN 10 AND 1800),
  CONSTRAINT investigators_time_ck   CHECK (updated_at >= created_at)
);

-- Serves: the settings list (every Investigator in one org, by name) and the uniqueness
-- of a name within an org — one index, because the name IS the list order and the
-- Enrichment name.
CREATE UNIQUE INDEX investigators_org_name_uniq ON investigators (org_id, name);

-- +goose StatementBegin
COMMENT ON TABLE investigators IS
  'A named configuration of a model-driven investigation (ADR 0053 §1): the mutable half — whether it may start (enabled) and how far one run may go (max_steps Tool calls, max_tokens input+output, max_wall_s). What produced a Finding — model, prompt, Tool allowlist — is investigator_versions, which is never updated. The name is the Enrichment name investigator.<name>, so it takes that alphabet and is never renamed.';
-- +goose StatementEnd

-- ----------------------------------------------------- investigator_versions

CREATE TABLE investigator_versions (
  id                UUID        PRIMARY KEY,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  investigator_id   UUID        NOT NULL REFERENCES investigators(id) ON DELETE CASCADE,
  version           INT         NOT NULL,
  -- NO ACTION: an endpoint a version dials cannot be deleted out from under the
  -- Findings that name it. There is no endpoint delete path yet; this is the rule
  -- the one that arrives must answer. Not RESTRICT, which is checked mid-statement:
  -- an org going takes its endpoints and its versions in one cascade, and only an
  -- end-of-statement check sees that the versions went too.
  model_provider_id UUID        NOT NULL REFERENCES model_providers(id),
  model_endpoint    TEXT        NOT NULL,
  model_name        TEXT        NOT NULL,
  prompt            TEXT        NOT NULL,
  tool_allowlist    TEXT[]      NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL,
  CONSTRAINT investigator_versions_version_ck  CHECK (version >= 1),
  CONSTRAINT investigator_versions_endpoint_ck CHECK (model_endpoint ~ '^https?://' AND length(model_endpoint) <= 2048),
  CONSTRAINT investigator_versions_model_ck    CHECK (char_length(model_name) BETWEEN 1 AND 200),
  CONSTRAINT investigator_versions_prompt_ck   CHECK (char_length(prompt) BETWEEN 1 AND 32768),
  -- ⛔ EXACT NAMES, NO WILDCARDS (§6). The protocol's function-name alphabet has no `*`,
  -- `?` or `.`, so a pattern cannot be spelled, and an element outside it is refused
  -- rather than read as one.
  CONSTRAINT investigator_versions_tools_ck    CHECK (
    cardinality(tool_allowlist) <= 64
    AND array_position(tool_allowlist, NULL) IS NULL
    AND array_to_string(tool_allowlist, ',') !~ '[^A-Za-z0-9_,-]'
  )
);

-- Serves: "this Investigator's versions, newest first", and one version per number.
CREATE UNIQUE INDEX investigator_versions_number_uniq ON investigator_versions (org_id, investigator_id, version);

-- +goose StatementBegin
COMMENT ON TABLE investigator_versions IS
  'One immutable version of an Investigator (ADR 0053 §6): the model endpoint it dials and the (base_url, model) identity pinned from it, the prompt, and the exact Tool allowlist. Changing any of them writes version N+1; nothing updates a row here, so a Finding naming a version names what produced it.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN investigator_versions.model_endpoint IS
  'model_providers.base_url as it stood when this version was written, pinned with model_name. A run whose endpoint row no longer has this identity is refused as model_changed rather than producing a Finding that names the wrong model.';
-- +goose StatementEnd

-- ------------------------------------------------------------ investigations

CREATE TABLE investigations (
  id                      UUID        PRIMARY KEY,
  org_id                  UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  subject_kind            TEXT        NOT NULL,
  subject_id              UUID        NOT NULL,
  alert_key               TEXT,
  investigator_id         UUID        NOT NULL REFERENCES investigators(id) ON DELETE CASCADE,
  investigator_version_id UUID        NOT NULL REFERENCES investigator_versions(id) ON DELETE CASCADE,
  status                  TEXT        NOT NULL,
  reason                  TEXT,
  reason_detail           TEXT,
  -- The budgets in force when it was requested, copied: the Investigator's own row
  -- changes in place, and "what was this run allowed?" must not.
  max_steps               INT         NOT NULL,
  max_tokens              BIGINT      NOT NULL,
  max_wall_s              INT         NOT NULL,
  -- What it spent (§6: "every Investigation records the tokens it spent").
  tokens_in               BIGINT      NOT NULL,
  tokens_out              BIGINT      NOT NULL,
  tool_calls              INT         NOT NULL,
  finding                 TEXT,
  requested_by            UUID        REFERENCES users(id) ON DELETE SET NULL,
  requested_by_label      TEXT        NOT NULL,
  requested_at            TIMESTAMPTZ NOT NULL,
  started_at              TIMESTAMPTZ,
  ended_at                TIMESTAMPTZ,

  -- ⭐ SHAPED FOR FOUR, ADMITTING ONE (ADR 0053 §4). Widen it with the subject's run path.
  CONSTRAINT investigations_subjkind_ck CHECK (subject_kind IN ('case')),
  CONSTRAINT investigations_status_ck   CHECK (status IN ('queued','running','completed','exhausted','failed','skipped')),
  -- A run that ended any way but `completed` says why, from a closed set per status,
  -- and one that has not ended (or completed) carries no reason.
  CONSTRAINT investigations_reason_ck   CHECK (
       (status IN ('queued','running','completed') AND reason IS NULL)
    OR (status = 'exhausted' AND reason IN ('step_budget','token_budget','wall_time_budget'))
    OR (status = 'failed'    AND reason IN ('usage_missing','model_error','model_changed','subject_gone','interrupted','internal'))
    OR (status = 'skipped'   AND reason IN ('disabled'))
  ),
  CONSTRAINT investigations_detail_ck   CHECK (reason_detail IS NULL OR char_length(reason_detail) BETWEEN 1 AND 2000),
  CONSTRAINT investigations_steps_ck    CHECK (max_steps BETWEEN 1 AND 100),
  CONSTRAINT investigations_tokens_ck   CHECK (max_tokens BETWEEN 1000 AND 2000000),
  CONSTRAINT investigations_wall_ck     CHECK (max_wall_s BETWEEN 10 AND 1800),
  CONSTRAINT investigations_spend_ck    CHECK (tokens_in >= 0 AND tokens_out >= 0 AND tool_calls >= 0),
  CONSTRAINT investigations_finding_ck  CHECK (finding IS NULL OR char_length(finding) BETWEEN 1 AND 16384),
  CONSTRAINT investigations_label_ck    CHECK (length(btrim(requested_by_label)) BETWEEN 1 AND 200),
  CONSTRAINT investigations_key_ck      CHECK (alert_key IS NULL OR length(alert_key) BETWEEN 1 AND 512),
  -- A `queued` or `skipped` run never started; every other one did. Only a terminal
  -- run has ended. Time runs forward.
  CONSTRAINT investigations_started_ck  CHECK ((started_at IS NULL) = (status IN ('queued','skipped'))),
  CONSTRAINT investigations_ended_ck    CHECK ((ended_at IS NULL) = (status IN ('queued','running'))),
  CONSTRAINT investigations_time_ck     CHECK (
    (started_at IS NULL OR started_at >= requested_at)
    AND (ended_at IS NULL OR ended_at >= coalesce(started_at, requested_at))
  )
);

-- Serves: "this Case's Investigations, latest first" — the API's list and the Finding a
-- subject shows (§4: "the latest is shown").
CREATE INDEX investigations_subject_idx ON investigations (org_id, subject_kind, subject_id, requested_at DESC, id DESC);

-- Serves: the built-in Tool "prior Findings on the same alert_key" — ended runs that
-- reached a Finding, newest first. Partial, so the queue of runs in flight is not in it.
CREATE INDEX investigations_alert_key_idx ON investigations (org_id, alert_key, ended_at DESC, id DESC)
  WHERE finding IS NOT NULL;

-- +goose StatementBegin
COMMENT ON TABLE investigations IS
  'One run of one Investigator version against one subject (ADR 0053 §1), frozen once it ends: queued, running, then completed, exhausted (a per-run budget was hit; whatever Finding it reached is kept and is partial), failed (usage missing, model error, ...) or skipped (the org or Investigator was disabled; nothing ran). The reason is recorded, never silent. The budgets it ran under and the tokens it spent are its own columns. A Finding is also published as the Enrichment investigator.<name>; it is NEVER an input to whether a notification is sent (ADR 0053 §2).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN investigations.alert_key IS
  'For a case subject, the Alert''s key copied at request time, so "prior Findings on the same alert_key" is one index range over this table. NULL for a subject with no Alert.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN investigations.requested_by IS
  'The human who asked, when a human did (ADR 0053 §4: "a human asks"). ACTOR metadata in the acked_by mould: nulled when the user goes, with requested_by_label frozen beside it. NO per-person metric is derived from it (SPEC R8).';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION investigations_refuse_change_once_ended() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('completed','exhausted','failed','skipped') THEN
    RAISE EXCEPTION 'investigation % has ended (%) and is frozen: an Investigation is never rewritten', OLD.id, OLD.status;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER investigations_frozen
  BEFORE UPDATE ON investigations
  FOR EACH ROW EXECUTE FUNCTION investigations_refuse_change_once_ended();

-- ------------------------------------------------------- investigation_steps

CREATE TABLE investigation_steps (
  id               UUID        PRIMARY KEY,
  org_id           UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  investigation_id UUID        NOT NULL REFERENCES investigations(id) ON DELETE CASCADE,
  seq              INT         NOT NULL,
  kind             TEXT        NOT NULL,
  -- model_turn: what the model said, the Tool calls it asked for, why it stopped, and
  -- what the turn cost.
  text             TEXT,
  tool_calls       JSONB,
  finish_reason    TEXT,
  tokens_in        BIGINT,
  tokens_out       BIGINT,
  -- tool_call: which call, which Tool, the raw arguments the model wrote, what came of
  -- it, and what the model was answered with.
  call_id          TEXT,
  tool_name        TEXT,
  arguments        TEXT,
  outcome          TEXT,
  result           TEXT,
  duration_ms      INT         NOT NULL,
  recorded_at      TIMESTAMPTZ NOT NULL,
  CONSTRAINT investigation_steps_seq_ck  CHECK (seq >= 1),
  CONSTRAINT investigation_steps_kind_ck CHECK (kind IN ('model_turn','tool_call')),
  CONSTRAINT investigation_steps_turn_ck CHECK (kind <> 'model_turn' OR (
    tool_calls IS NOT NULL AND jsonb_typeof(tool_calls) = 'array'
    AND finish_reason IS NOT NULL
    AND tokens_in IS NOT NULL AND tokens_in >= 0 AND tokens_out IS NOT NULL AND tokens_out >= 0
    AND call_id IS NULL AND tool_name IS NULL AND outcome IS NULL)),
  CONSTRAINT investigation_steps_call_ck CHECK (kind <> 'tool_call' OR (
    call_id IS NOT NULL AND tool_name IS NOT NULL AND arguments IS NOT NULL AND result IS NOT NULL
    AND outcome IN ('ok','refused','timeout','truncated','failed')
    AND tool_calls IS NULL AND tokens_in IS NULL AND tokens_out IS NULL)),
  CONSTRAINT investigation_steps_size_ck CHECK (
    coalesce(char_length(text), 0) <= 65536 AND coalesce(char_length(arguments), 0) <= 65536
    AND coalesce(char_length(result), 0) <= 65536),
  CONSTRAINT investigation_steps_duration_ck CHECK (duration_ms >= 0)
);

-- Serves: the transcript, in order, and one entry per position.
CREATE UNIQUE INDEX investigation_steps_seq_uniq ON investigation_steps (org_id, investigation_id, seq);

-- +goose StatementBegin
COMMENT ON TABLE investigation_steps IS
  'One immutable entry in an Investigation''s transcript (ADR 0053 §1): a model turn (text, the Tool calls it asked for, finish reason, tokens) or one Tool call with its outcome (ok, refused — outside the allowlist or past the step budget —, timeout, truncated, failed) and the result the model was answered with. APPEND-ONLY: investigation_steps_append_only refuses every UPDATE, and every DELETE but the cascade of its org or its Investigation going. If you would ever UPDATE it, it is not a Step.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION investigation_steps_refuse_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- ⭐ A CASCADE IS THE RECORD GOING WITH ITS OWNER. A foreign key's ON DELETE CASCADE
  -- runs inside its own trigger, so a delete reached from `orgs` or `investigations`
  -- arrives here nested; a DELETE issued against this table directly does not.
  IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1 THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'a Step is an immutable transcript entry: % on investigation_steps is refused', TG_OP;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER investigation_steps_append_only
  BEFORE UPDATE OR DELETE ON investigation_steps
  FOR EACH ROW EXECUTE FUNCTION investigation_steps_refuse_change();

-- ------------------------------------------------------------ orgs.settings

-- `investigations_enabled` joins the document (ADR 0053 §6's org "Enabled"). JSONB, so
-- no column moves; the comment is the schema's statement of the key set, and it says
-- eight now.
-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine, retention and Investigations, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The eight keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months, default_verbosity and investigations_enabled (ADR 0053 §6, boolean, default true: the org''s Investigation kill switch; absent means enabled). ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to what 00071 shipped. A row that wrote `investigations_enabled` keeps
-- it in the document, which the release below reads past: an unknown JSONB key is not a
-- column it can fail on.
-- +goose StatementBegin
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine and retention, as a partial document: an ABSENT key means "this org never wrote it" and is a different fact from a written value that happens to equal the shipped default -- which is what makes the settings screen able to report an origin. The seven keys are resolve_grace_s, flap_threshold, flap_window_s, flap_digest_interval_s, raw_retention_days, event_retention_months and default_verbosity. ⛔ NARROWING THIS SET IS THIS COMMENT''S JOB TOO: refire_grace_s and group_close_delay_s left in 00071, the four unacked_reminder_* keys in 00068, broadcast_on_resolved in 00069, and the three storm_* keys in 00059.';
-- +goose StatementEnd

DROP TABLE investigation_steps;
DROP FUNCTION investigation_steps_refuse_change();
DROP TABLE investigations;
DROP FUNCTION investigations_refuse_change_once_ended();
DROP TABLE investigator_versions;
DROP TABLE investigators;
