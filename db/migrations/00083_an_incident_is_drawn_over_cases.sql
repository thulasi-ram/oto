-- ADR 0052 §1–§4: an Incident is a set of one or more Cases drawn together as one
-- story, and a Case belongs to AT MOST ONE Incident (git-bug b2672a1).
--
-- ⭐⭐ THREE TABLES, AND NOT ONE OF THEM IS A SIGNAL TABLE. `alerts` and
-- `alert_cases` gain NO column here, and that is the whole shape of the decision
-- rather than a tidiness preference. The membership is its own row, keyed by the
-- Case, so the signal tables keep saying what they have always said — what fired,
-- when, and how it ended — and an Incident is a READING over them. SCOPE-BOUNDARY
-- §5.6's door stays shut on the signal rows: no `alert_cases.incident_id`, ever.
--
-- ⛔ AN INCIDENT HAS NO STATE COLUMN, AND THAT IS THE RULING (ADR 0052 §3). It is
-- active while any member Case is open and quiet otherwise, and both words are
-- READ OFF the member Cases at query time. A column would need a writer, and the
-- only writer that could keep it honest is the Case lifecycle — which would then
-- have to know about Incidents, inverting the dependency the signal layer is built
-- on. A column some OTHER writer kept would be a status a hand could set, and
-- "mitigated", a lead and a human-set severity are exactly the response the owner
-- ruled out: they live in the incident tool this Incident is declared to (§5),
-- never here. So there is no `status`, no `severity`, no `lead`, and no
-- `closed_at` for anything to stamp.
--
-- ⭐ WHO DREW IT IS RECORDED, AND IT IS ONE OF EXACTLY TWO ANSWERS (§2). A
-- Correlator — an operator-written rule — or a human. Both columns are on the row
-- and `incidents_drawn_by_ck` admits exactly one, so "why is this an Incident?"
-- always has an answer someone can read back. A model is not a third column: it
-- PROPOSES membership as a Suggestion and never draws (§2).
--
-- ⚠️ `drawn_by_correlator_id` HAS NO FOREIGN KEY YET, AND SAYING SO IS THE POINT.
-- The `correlators` table is the Correlator ticket's, and a column that will
-- reference it is declared here so the XOR is a CHECK from the first row rather
-- than a convention the second migration has to retrofit onto live data. Nothing
-- writes it until that table exists; the ticket that creates the table adds the
-- FOREIGN KEY in the same migration.
--
-- ⭐ THE ACTOR IS METADATA IN THE `acked_by` MOULD — past-tense attribution, a
-- `users` reference nulled when the user goes, and a FROZEN LABEL beside it so the
-- record still reads "drawn by alice" a year after alice left (00039's
-- `started_by`, 00007's `acked_by`). ACTOR, NEVER SUBJECT: no column here says
-- anybody owes the Incident anything, and no per-person metric is derived from
-- any of them (SPEC R8).
--
-- ⭐⭐ AT MOST ONE INCIDENT PER CASE IS THE DATABASE'S RULE, NOT THE SERVICE'S.
-- `incident_members_case_live_uniq` is a PARTIAL unique index on the Case over the
-- memberships that have not been removed. The service reads the incumbent first —
-- only to tell the caller WHICH Incident holds the Case, so the refusal can point
-- at a move — and two concurrent adds of one Case to two Incidents still meet the
-- index, whichever read they made. A check-then-insert in Go is a race; a unique
-- index is not.
--
-- ⭐ REMOVAL IS A TOMBSTONE, NOT A DELETE. A removed Case stays recorded as
-- removed — by whom, when, and, for a move, to which Incident — because "this Case
-- WAS in #4 and alice took it out" is a fact about the story that a delete would
-- erase, and ADR 0052 §4 needs it later: a Case a human removed is never re-added
-- by a Correlator, and the Correlator can only honour that if the removal is still
-- on disk. The partial index ignores tombstones, so a Case may be removed and
-- added again, and each spell is its own row.
--
-- ⛔ NO `updated_at` ANYWHERE. `incidents` is never updated — its number, its
-- drawing instant and its author are settled at INSERT — and a membership row is
-- written once and tombstoned at most once, with `removed_at` as the record of the
-- second write. A timestamp with no reader is a write with no purpose.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three new tables, no column added to an
-- existing one, nothing to backfill. A release-N reader has never heard of these
-- tables and is unaffected by them.

-- +goose Up

-- --------------------------------------------------------- the counter

-- The 00081 shape for the 00081 reasons, which are not repeated here at length:
-- per-ORG so one tenant's volume never leaks into another's numbers, its own
-- table rather than a column on `orgs` so the allocation lock is not shared with
-- the settings row, and allocated by one data-modifying CTE in the INSERT that
-- consumes it. Unique and ordered, NOT gapless: a rolled-back draw has spent its
-- number, and `number` is a name, never a count.
CREATE TABLE org_incident_numbers (
  org_id      UUID   PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  next_number BIGINT NOT NULL DEFAULT 1,
  CONSTRAINT org_incident_numbers_next_ck CHECK (next_number >= 1)
);

-- +goose StatementBegin
COMMENT ON TABLE org_incident_numbers IS
  'One row per org holding the next incidents.number to hand out. Bumped by the single INSERT that draws an Incident and read by nothing else: the allocated value comes back on the same statement. A missing row means the org has drawn no Incident yet.';
-- +goose StatementEnd

-- -------------------------------------------------------------- incidents

CREATE TABLE incidents (
  id                     UUID        PRIMARY KEY,
  org_id                 UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  number                 BIGINT      NOT NULL,
  drawn_at               TIMESTAMPTZ NOT NULL,
  drawn_by               UUID        REFERENCES users(id) ON DELETE SET NULL,
  drawn_by_label         TEXT,
  drawn_by_correlator_id UUID,

  CONSTRAINT incidents_number_uniq UNIQUE (org_id, number),
  CONSTRAINT incidents_number_ck   CHECK (number >= 1),
  -- EXACTLY ONE AUTHOR (ADR 0052 §2). The LABEL is the human half's presence
  -- marker rather than `drawn_by`, because `drawn_by` is nulled when the user is
  -- deleted and the Incident must not thereby stop having been drawn by somebody.
  CONSTRAINT incidents_drawn_by_ck CHECK ((drawn_by_label IS NULL) <> (drawn_by_correlator_id IS NULL)),
  CONSTRAINT incidents_human_ck    CHECK (drawn_by IS NULL OR drawn_by_label IS NOT NULL),
  CONSTRAINT incidents_label_ck    CHECK (drawn_by_label IS NULL OR length(btrim(drawn_by_label)) BETWEEN 1 AND 200)
);

-- +goose StatementBegin
COMMENT ON TABLE incidents IS
  'An Incident: a set of one or more Cases drawn together as one story (ADR 0052). It is a fact about SIGNALS. Its state is NOT stored: it is active while any member Case is open and quiet otherwise, read off alert_cases at query time and never written by a hand. Its RESPONSE (status, lead, severity, comms, write-up) is managed in the external tool it is declared to and is never stored here. Membership is incident_members.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incidents.number IS
  'The Incident name within its org: 1-based, monotonic, allocated from org_incident_numbers at INSERT. What a human quotes and what /incidents/{number} addresses. Unique and ordered but not gapless -- a name, not a count.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incidents.drawn_by IS
  'The human who drew this Incident, when a human did. ACTOR metadata in the acked_by mould: past-tense attribution, nulled when the user goes, with drawn_by_label frozen beside it. NO per-person metric is derived from it (SPEC R8). Exactly one of drawn_by_label and drawn_by_correlator_id is set.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incidents.drawn_by_correlator_id IS
  'The operator-written Correlator that drew this Incident, when one did (ADR 0052 §2). No foreign key yet: the correlators table arrives with the Correlator and adds it. Exactly one of this and drawn_by_label is set.';
-- +goose StatementEnd

-- ------------------------------------------------------ incident_members

CREATE TABLE incident_members (
  id                     UUID        PRIMARY KEY,
  org_id                 UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  incident_id            UUID        NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  -- CASCADE because the only path that deletes a Case is a drill disposing of its
  -- own synthetic signal (ADR 0024 carve-out); signal tables are otherwise never
  -- reaped, so in production this never fires.
  case_id                UUID        NOT NULL REFERENCES alert_cases(id) ON DELETE CASCADE,
  added_at               TIMESTAMPTZ NOT NULL,
  added_by               UUID        REFERENCES users(id) ON DELETE SET NULL,
  added_by_label         TEXT,
  added_by_correlator_id UUID,
  removed_at             TIMESTAMPTZ,
  removed_by             UUID        REFERENCES users(id) ON DELETE SET NULL,
  removed_by_label       TEXT,
  -- Set only when the removal was HALF OF A MOVE, so the tombstone reads "moved to
  -- #7" rather than "removed". SET NULL rather than CASCADE: an Incident is never
  -- deleted by oto, and if one ever were, this row's own removal is still true.
  moved_to_incident_id   UUID        REFERENCES incidents(id) ON DELETE SET NULL,

  -- Who added it: a Correlator or a human, exactly one, as on `incidents`.
  CONSTRAINT incident_members_added_by_ck    CHECK ((added_by_label IS NULL) <> (added_by_correlator_id IS NULL)),
  CONSTRAINT incident_members_added_human_ck CHECK (added_by IS NULL OR added_by_label IS NOT NULL),
  CONSTRAINT incident_members_added_label_ck CHECK (added_by_label IS NULL OR length(btrim(added_by_label)) BETWEEN 1 AND 200),
  -- ⛔ ONLY A HUMAN REMOVES (ADR 0052 §4), so a tombstone always carries a
  -- human label and there is no correlator column on this half.
  CONSTRAINT incident_members_removed_ck       CHECK ((removed_at IS NULL) = (removed_by_label IS NULL)),
  CONSTRAINT incident_members_removed_human_ck CHECK (removed_by IS NULL OR removed_at IS NOT NULL),
  CONSTRAINT incident_members_removed_label_ck CHECK (removed_by_label IS NULL OR length(btrim(removed_by_label)) BETWEEN 1 AND 200),
  CONSTRAINT incident_members_moved_ck         CHECK (moved_to_incident_id IS NULL OR removed_at IS NOT NULL),
  CONSTRAINT incident_members_moved_self_ck    CHECK (moved_to_incident_id IS DISTINCT FROM incident_id),
  CONSTRAINT incident_members_time_ck          CHECK (removed_at IS NULL OR removed_at >= added_at)
);

-- ⭐⭐ THE AT-MOST-ONE RULE (ADR 0052 §4), stated where nothing can race it. Over
-- LIVE memberships only: a tombstone is history and must not stop the Case being
-- added somewhere again.
CREATE UNIQUE INDEX incident_members_case_live_uniq ON incident_members (case_id) WHERE removed_at IS NULL;

-- Serves: an Incident's members, live and removed, in the order they joined — the
-- detail page and the list's per-Incident roll-up both walk this.
CREATE INDEX incident_members_incident_idx ON incident_members (incident_id, added_at, id);

-- +goose StatementBegin
COMMENT ON TABLE incident_members IS
  'One spell of one Case inside one Incident. Written when the Case is drawn or added, tombstoned (removed_at) when a human removes or moves it, never deleted. incident_members_case_live_uniq makes a Case belong to at most one Incident at a time (ADR 0052 §4). This is NOT a column on alert_cases: the signal row says nothing about Incidents.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incident_members.removed_at IS
  'When a human took this Case out of this Incident. NULL while the Case is a member. A removed Case stays recorded as removed, because a Correlator must never re-add a Case a human removed (ADR 0052 §4) and can only honour that while the removal is on disk.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incident_members.moved_to_incident_id IS
  'Set when this removal was half of a move: the Incident the Case went to, in the same transaction that wrote the new membership. NULL for a plain removal.';
-- +goose StatementEnd

-- +goose Down

DROP TABLE incident_members;
DROP TABLE incidents;
DROP TABLE org_incident_numbers;
