-- ADR 0052 §2 and §4: a CORRELATOR — matchers over Cases, optionally a count over
-- a window, written by an operator — is the machine author of Incidents, and when
-- several match one Case the first in the operator's order draws it (git-bug
-- 61eeddf). Until this migration every Incident was hand-drawn, and the storm ADR
-- 0045 accepted (N alerts, N threads) had no machine answer.
--
-- ⭐⭐ IT IS A NOTIFICATION POLICY'S FIRST HALF, AND EVERY COLUMN THAT CAN BE ONE IS
-- ONE. `matchers` is `notification_policies.matchers` exactly — a JSONB array of
-- `{name,op,value}` in ADR 0017's grammar, at most 32, evaluated by the one Go
-- matcher both modules now share (`alerts/domain/matcher.go`). `count_min` and
-- `count_window_s` are 00072's count condition exactly — 2..10000 over a sliding
-- 60..86400-second lookback, both halves or neither — and `priority` is the policy's
-- evaluation order exactly: 0..10000, LOWER FIRST, first match wins, ties broken by
-- age and then id. 00076 said it for templates and it is said again here: two
-- orderings that read the same way and behave differently is how an operator
-- learns to distrust both. The ticket calls this column the operator-set
-- "order"; it is spelled `priority` because that is how oto spells it twice
-- already.
--
-- ⛔ A CORRELATOR IS NOT A "RULE", AND NOTHING IN THIS FILE CALLS IT ONE. In oto a
-- rule is the Prometheus alerting rule (CONTEXT.md §3); the word is taken.
--
-- ⭐ FIRST MATCH WINS, AND "MATCH" MEANS THE MATCHERS — NOT THE COUNT. This is the
-- policy's semantics carried over unchanged: a policy whose matchers hold claims
-- the fact even when its own count condition then holds it back
-- (`below_threshold`), and no later policy is consulted. So a Case the first
-- Correlator's matchers accept is THAT Correlator's, recorded in
-- `correlator_matches`, whether or not it clears the count yet; the second
-- Correlator never sees it. Falling through to the next Correlator on a count miss
-- would make which story a Case belongs to depend on how many OTHER Cases happened
-- to arrive first, which nobody could read back from the configuration.
--
-- ⭐ `correlator_matches` IS WHY THE COUNT IS ANSWERABLE IN ONE INDEXED READ. "≥5
-- Cases matching X within 600 s" needs the four Cases before the fifth, and the
-- matchers are evaluated in Go (an anchored RE2 regex is not a SQL predicate), so
-- re-deriving them would mean re-reading every Case opened in the window and
-- re-running every Correlator over each — O(N²) across a storm, at exactly the
-- moment a storm is the input. Recording each first match as it is decided makes
-- the count `count(*)` over `(correlator_id, case_started_at)`. It is keyed by the
-- CASE, so a Case is matched by at most one Correlator, which IS the first-wins
-- rule stated where a retried job cannot break it.
--
-- ⛔ SOFT DELETE, BECAUSE THE INCIDENTS OUTLIVE THEIR AUTHOR. `incidents` and
-- `incident_members` name the Correlator that drew or added them (00083), and
-- "why is this an Incident?" must still have an answer someone can read back after
-- the Correlator is retired (§2). A DELETE would have to null those columns —
-- which `incidents_drawn_by_ck` refuses, correctly — or cascade the Incidents away.
-- So the FKs this migration adds are plain RESTRICT, and deleting is
-- `deleted_at`, `notification_policies`' shape for the same audit reason.
--
-- ⭐ AND THE FOREIGN KEYS 00083 PROMISED ARRIVE HERE. 00083 declared
-- `drawn_by_correlator_id` and `added_by_correlator_id` without one because the
-- table did not exist, and said the Correlator ticket would add them in the same
-- migration that creates it. Nothing has written either column yet — no Correlator
-- existed to write it — so both constraints validate against all-NULL columns.
--
-- ⛔ EVALUATION IS NEVER ON THE INGEST TRANSACTION (CONTEXT.md commitment 2), and
-- nothing here is read by it. A Case opening enqueues `incidents.correlate` in the
-- transaction that opened it — a queue insert, the same cost as `enrich.run` —
-- and the job runs on the `lifecycle` queue, never on `notify`, so a slow or
-- failing Correlator can neither block nor delay the Case's own notification.
-- Nothing is held back to wait for a Correlator (ADR 0052 §6).
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Two new tables, two foreign keys on columns no
-- release has written, one partial index on `incidents`. A release-N reader never
-- names a Correlator and is unaffected.

-- +goose Up

-- -------------------------------------------------------------- correlators

CREATE TABLE correlators (
  id             UUID        PRIMARY KEY,
  org_id         UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name           CITEXT      NOT NULL,
  priority       INT         NOT NULL DEFAULT 100,
  enabled        BOOLEAN     NOT NULL DEFAULT true,
  matchers       JSONB       NOT NULL DEFAULT '[]'::jsonb,
  count_min      INT,
  count_window_s INT,
  created_at     TIMESTAMPTZ NOT NULL,
  updated_at     TIMESTAMPTZ NOT NULL,
  deleted_at     TIMESTAMPTZ,

  CONSTRAINT correlators_name_ck     CHECK (length(btrim(name::text)) BETWEEN 1 AND 120),
  CONSTRAINT correlators_prio_ck     CHECK (priority BETWEEN 0 AND 10000),
  -- `policies_matchers_ck` verbatim. AN EMPTY LIST MATCHES EVERY CASE, as an empty
  -- policy matches every fact: "any Case" is a Correlator an operator may mean —
  -- with a count it is "≥N Cases of anything inside W", which is the storm.
  CONSTRAINT correlators_matchers_ck CHECK (jsonb_typeof(matchers) = 'array' AND jsonb_array_length(matchers) <= 32),
  -- 00072's three count constraints, with 00072's arguments: two because the Case
  -- being evaluated is itself inside the window, the throttle's window bound, and
  -- symmetric because neither half means anything alone. There is no unit rule:
  -- a Correlator counts Cases and nothing else.
  CONSTRAINT correlators_count_min_ck    CHECK (count_min IS NULL OR count_min BETWEEN 2 AND 10000),
  CONSTRAINT correlators_count_window_ck CHECK (count_window_s IS NULL OR count_window_s BETWEEN 60 AND 86400),
  CONSTRAINT correlators_count_pair_ck   CHECK ((count_min IS NULL) = (count_window_s IS NULL)),
  CONSTRAINT correlators_time_ck         CHECK (updated_at >= created_at)
);

-- Unique among the LIVE ones: a retired Correlator keeps its name on the Incidents
-- it drew, and must not stop an operator writing a new one under the same name.
CREATE UNIQUE INDEX correlators_name_uniq ON correlators (org_id, name) WHERE deleted_at IS NULL;

-- Serves: the evaluator's walk — live Correlators in the operator's order, first
-- match wins — once per Case open. `policies_eval_idx`'s shape, with the tie-break
-- columns so the walk needs no Sort node.
CREATE INDEX correlators_eval_idx ON correlators (org_id, priority, created_at, id)
  WHERE enabled AND deleted_at IS NULL;

-- +goose StatementBegin
COMMENT ON TABLE correlators IS
  'An operator-written definition that draws Incidents (ADR 0052 §2): matchers over Cases in the notification-policy grammar (ADR 0017), optionally a count over a sliding window (00072''s shape). Walked in priority order, LOWER FIRST, on every Case open by the incidents.correlate job -- never on the ingest transaction -- and the first whose matchers hold claims the Case (correlator_matches). A Case already in an Incident, or one a human ever removed from one, is skipped by every Correlator. NOT a rule: in oto that word is the Prometheus alerting rule. Soft-deleted, because the Incidents it drew name it.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN correlators.priority IS
  'Evaluation order, 0..10000, LOWER IS FIRST, ties by created_at then id -- notification_policies.priority''s sentence, on purpose. The first Correlator whose matchers hold claims the Case; no later one is consulted, even when the first''s count condition is not yet met.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN correlators.count_min IS
  'Draw only once at least this many Cases this Correlator claimed opened inside count_window_s, the Case being evaluated included. 2..10000 or NULL for "every matching Case draws or joins". The Incident is drawn over every such Case at once, on the one that clears the threshold. 00072''s count condition, read here as a floor on DRAWING and never on joining: a Case that matches while this Correlator''s Incident is active joins it whatever the count.';
-- +goose StatementEnd

-- ------------------------------------------------------- correlator_matches

CREATE TABLE correlator_matches (
  -- ⭐ THE PRIMARY KEY IS THE CASE, and that is first-wins as a constraint: one
  -- Case, at most one Correlator, whatever a retried or duplicated job does.
  case_id         UUID        PRIMARY KEY REFERENCES alert_cases(id) ON DELETE CASCADE,
  org_id          UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  correlator_id   UUID        NOT NULL REFERENCES correlators(id),
  -- A COPY of alert_cases.started_at, taken once at match time, so the window
  -- read is one index range and never a join. A Case's start never moves.
  case_started_at TIMESTAMPTZ NOT NULL,
  matched_at      TIMESTAMPTZ NOT NULL
);

-- Serves: "how many Cases has this Correlator claimed inside [t - W, t]", and the
-- Cases themselves when the count clears.
CREATE INDEX correlator_matches_window_idx ON correlator_matches (correlator_id, case_started_at);

-- +goose StatementBegin
COMMENT ON TABLE correlator_matches IS
  'Which Correlator claimed which Case: the first, in priority order, whose matchers held when the Case opened. Keyed by the Case, so a Case is claimed at most once. It is the numerator of a count condition and the record of why a Correlator-drawn membership exists. It says nothing about Incident membership, which is incident_members alone: a claimed Case may be below its Correlator''s threshold and in no Incident.';
-- +goose StatementEnd

-- ------------------------------------------------- the foreign keys 00083 owed

ALTER TABLE incidents ADD CONSTRAINT incidents_correlator_fk
  FOREIGN KEY (drawn_by_correlator_id) REFERENCES correlators(id);

ALTER TABLE incident_members ADD CONSTRAINT incident_members_correlator_fk
  FOREIGN KEY (added_by_correlator_id) REFERENCES correlators(id);

-- Serves: "this Correlator's latest Incident" — the one a matching Case may join.
-- Partial, because a human-drawn Incident is never a Correlator's to grow (§4).
CREATE INDEX incidents_correlator_idx ON incidents (drawn_by_correlator_id, number DESC)
  WHERE drawn_by_correlator_id IS NOT NULL;

-- +goose StatementBegin
COMMENT ON COLUMN incidents.drawn_by_correlator_id IS
  'The operator-written Correlator that drew this Incident, when one did (ADR 0052 §2). References correlators since 00085, which soft-deletes so this stays answerable. Exactly one of this and drawn_by_label is set. A Correlator only ever grows the Incidents it drew; one a human drew never gains a Case by itself (§4).';
-- +goose StatementEnd

-- +goose Down

-- ⚠️ THE DOWN LOSES CONFIGURATION AND SAYS SO. Every Correlator an operator wrote
-- goes with the table, and after it nothing draws an Incident by itself. The
-- Incidents the Correlators drew STAY, with `drawn_by_correlator_id` still naming
-- an id that no longer resolves — 00083's state exactly, which its comment
-- described as "no foreign key yet". They are not deleted, because they are what
-- happened.
DROP INDEX IF EXISTS incidents_correlator_idx;
ALTER TABLE incident_members DROP CONSTRAINT IF EXISTS incident_members_correlator_fk;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_correlator_fk;

-- +goose StatementBegin
COMMENT ON COLUMN incidents.drawn_by_correlator_id IS
  'The operator-written Correlator that drew this Incident, when one did (ADR 0052 §2). No foreign key yet: the correlators table arrives with the Correlator and adds it. Exactly one of this and drawn_by_label is set.';
-- +goose StatementEnd

DROP TABLE correlator_matches;
DROP TABLE correlators;
