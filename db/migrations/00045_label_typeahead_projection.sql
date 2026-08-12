-- The label typeahead stops scanning the tenant, and the note that said it had
-- to finally becomes false.
--
-- ⭐⭐ WHY. `GET /api/v1/labels` and `GET /api/v1/labels/{name}/values` are the
-- alert filter bar's typeahead. They fire PER KEYSTROKE, and until this
-- migration both of them read `alerts` in full, once per keystroke, per
-- operator. `distinctLabelNamesSQL` expanded every non-synthetic alert in the
-- org through a LATERAL jsonb_object_keys before it could GROUP BY:
--
--   SELECT k, count(*) FROM alerts a, LATERAL jsonb_object_keys(a.labels) AS k
--    WHERE a.org_id = $1 AND NOT a.synthetic AND k LIKE $2 || '%'
--    GROUP BY k ORDER BY n DESC, k ASC LIMIT $3
--
-- and `distinctLabelValuesSQL` did the same without the lateral, over
-- `a.labels ->> $2`. `alerts` is on ADR 0024's never-reaped list -- one row per
-- alert identity for the life of the install -- and `MaxLabels` (B3) is 64, so
-- the lateral is one to sixty-four rows per alert and the volume only ever
-- rises. At 200 000 alerts averaging ten labels that is 2M intermediate rows per
-- keystroke to return at most 200.
--
-- ⛔ THE OLD NOTE'S TWO FALSE CLAUSES, because both are the kind that reads as
-- true. It said the query "is bounded by `limit`": LIMIT bounds rows RETURNED,
-- and the `GROUP BY … ORDER BY n DESC` above it must aggregate the whole tenant
-- before it can know which twenty-five rows those are. It said the endpoint "is
-- a discovery endpoint, not a hot path": it is the filter bar of the incident
-- view, which is to say it is the hot path at the only moment anybody cares.
--
-- ⛔ AND ITS PROPOSED REMEDY IS NOT MERELY UNOWNED, IT IS IMPOSSIBLE. The note
-- offered "an expression index over jsonb_object_keys would be a migration this
-- module does not own", which frames the fix as an ownership question. Postgres
-- settles it first:
--
--   CREATE INDEX ON alerts (jsonb_object_keys(labels));
--   ERROR:  set-returning functions are not allowed in index expressions
--
-- There is no index over a set-returning function, in any opclass, GIN included.
-- The value side is shut for a second reason: `a.labels ->> $2` takes the label
-- NAME as a runtime parameter, and an index expression must be fixed at CREATE
-- INDEX time, so there is no expression to index either. `alerts_labels_gin`
-- (00007) is jsonb_path_ops and answers containment only, exactly as its own
-- comment says. NO INDEX ON `alerts` CAN SERVE EITHER QUERY. That is why this
-- migration adds tables rather than an index, and it is the whole argument.
--
-- ⭐ THE SHAPE OF THE MATCH, because it is what picks the opclass. Both filters
-- are `LIKE $n || '%'` -- an anchored, case-SENSITIVE PREFIX, never a substring
-- and never ILIKE (`parseLabelQuery` passes the raw query through; nothing
-- lowercases it). A prefix rides an ordinary btree, so pg_trgm is NOT needed and
-- NO EXTENSION IS ENABLED HERE. `text_pattern_ops` is a built-in btree opclass,
-- not an extension, and it is used rather than the default because LIKE can only
-- be turned into an index range under a collation whose ordering agrees with
-- byte order. docker-compose.yml initdb's the dev database with `--locale=C`,
-- where the default opclass would happen to work; deploy/helm points at an
-- operator's own Postgres and says nothing about its locale. An index whose
-- usability depends on an initdb flag nobody wrote down is an index that is
-- there and not used, so the opclass is stated.
--
-- ⭐ TWO TABLES, BECAUSE THE TWO READS HAVE DIFFERENT BOUNDS.
--
--   alert_labels       one row per (alert, label name), the EXACT expansion of
--                      `alerts.labels` for every non-synthetic alert. It serves
--                      the VALUE typeahead: `org_id` and `label_name` are
--                      equalities and `label_value` is the prefix range, so the
--                      work is bounded by the alerts carrying that one name with
--                      that one prefix. Typing a longer prefix now narrows the
--                      WORK and not merely the result, which is the specific
--                      thing the old query could not do.
--   alert_label_names  one row per (org, label name) with the count. It serves
--                      the NAME typeahead, bounded by the org's LABEL
--                      CARDINALITY -- tens to thousands of rows -- rather than
--                      by its alert count. This table is why there are two: the
--                      name enumeration with an EMPTY prefix (the state the
--                      typeahead OPENS in) has no range to narrow, so reading it
--                      off `alert_labels` would still have been a full pass, and
--                      a fix that only helps once the operator has typed a
--                      character is not a fix for the first keystroke.
--
-- ⭐ WHY A COUNT AND NOT A DISTINCT. The count is the contract's `alert_count`
-- and it is what the ORDER BY uses. `DistinctLabelNames` has always said why: a
-- typeahead that offers a label matching nothing spends the one minute of an
-- incident that matters most.
--
-- ⭐ HOW IT STAYS TRUE, and why the two tables are maintained differently.
-- `upsertAlertsSQL` is the ONLY writer of `alerts.labels` in the tree -- the
-- other three UPDATEs on `alerts` touch flap_score, snoozed_until and the
-- occurrence projection -- so both tables are maintained in that one statement,
-- in that one transaction, while `Service.observe` holds the alert's row lock.
--
--   alert_labels is maintained by FULL REPLACEMENT of one alert's set: delete
--   the names the new label set no longer has, upsert the ones it does. A
--   replacement is race-free where a delta is not, because it writes the whole
--   set rather than a difference, so it does not need to know what was there
--   before and cannot be wrong about it.
--
--   alert_label_names is maintained by a DELTA, because a count is shared
--   between alerts and cannot be replaced by one of them. The delta is not
--   computed from the old labels -- which the upsert's RETURNING cannot see and
--   which a separate read could not lock -- but from the RETURNING of the two
--   statements above it: a name deleted from alert_labels is -1, a name INSERTED
--   there (xmax = 0) is +1, and a name whose VALUE merely changed returns
--   nothing and moves no count. Both of those RETURNINGs are produced under row
--   locks, and READ COMMITTED re-reads a locked row before applying, so the
--   arithmetic is exact under concurrency rather than approximately exact.
--
-- ⛔ THE CHECK IS THE POINT. `alert_label_names_count_ck` refuses a negative
-- count. A count projection that can drift is a count projection that rots
-- silently, and silence is the failure mode 00042 spent a page on. If the
-- arithmetic above is ever wrong the ingest path fails loudly with a 23514
-- rather than quietly mis-ranking a typeahead forever.
--
-- ⚠️ A ZEROED ROW IS KEPT, NOT DELETED. When the last alert carrying a name
-- loses it, the row stays at zero and the read filters `alert_count > 0` (as
-- does alert_label_names_rank_idx, which is partial on the same predicate).
-- Deleting it would need a second statement on the ingest path to save a row per
-- label name ever seen, which is the smallest number in this schema.
--
-- ⛔ THE PARTIAL PREDICATE ON alert_labels_value_idx IS NOT TIDINESS, IT IS THE
-- INGEST PATH. B5 (`MaxLabelValueBytes`) admits a 4096-byte label value and B4 a
-- 1024-byte name. A btree index tuple cannot exceed 2704 bytes, and an INSERT
-- that would produce one does not skip the index, it ERRORS. Since this index is
-- now written by `upsertAlertsSQL`, that error is an INGEST OUTAGE triggered by
-- one incompressible label inside the bounds the domain already accepts. (Only
-- incompressible: a repetitive value compresses into the limit even inside an
-- index tuple, which is exactly what makes this the kind of thing that passes
-- every test and then happens.)
--
-- ⛔ AND THE PREDICATE MUST COUNT BYTES, NOT CHARACTERS, WHICH IS WHERE THE
-- FIRST VERSION OF THIS MIGRATION WAS WRONG. It said `length(label_value) <=
-- 512`. `length()` counts CHARACTERS and the btree cap is BYTES, so a value of
-- 512 ASTRAL code points is 2048 bytes and SATISFIES that predicate — which
-- means the row is not excluded from the index, it is admitted to it. On PG
-- 17.10 (UTF8, --locale=C, the docker-compose.yml database), with a 1024-byte
-- name and exactly that value, both well inside B4 and B5 and both accepted by
-- `NewLabels`:
--
--   ERROR:  index row size 3104 exceeds btree version 4 maximum 2704
--           for index "alert_labels_value_idx"
--
-- ⛔ AND BOUNDING THE VALUE ALONE CAN NEVER BE ENOUGH, however it is measured.
-- The index row is (org_id, label_name, label_value), so `label_name` is IN it:
-- the value alone cannot reach 2704, and what overflows is the SUM. The trigger
-- is `octet_length(name) + octet_length(value)` above roughly 2672, so both
-- columns are bounded or neither is.
--
-- Hence: `octet_length(label_name) <= 1024 AND octet_length(label_value) <= 512`.
-- 1024 is B4 restated in the unit B4 is actually written in (`NewLabels` measures
-- Go `len`, which is bytes), so it excludes nothing the domain admits and closes
-- the name side against a multibyte name that `alert_labels_name_ck` would let
-- through. 512 bytes is the value bound because a typeahead is a picker, and a
-- value nobody can read in a dropdown is not one they can pick. Measured on the
-- above database, the largest index tuple this pair ADMITS is 1568 bytes against
-- the 2704 maximum — 1136 bytes of headroom — with both columns at their bound
-- and both incompressible.
--
-- A row failing a PARTIAL index's predicate is simply not indexed, so nothing is
-- rejected: the value is STORED in full and only its presence in the TYPEAHEAD is
-- bounded. `distinctLabelValuesSQL` carries this predicate VERBATIM, BOTH
-- conjuncts, so the planner can prove the partial index applies — a query
-- carrying only the value half does not imply the name half and would silently
-- stop using the index. The two are changed together or not at all.
--
-- ⚠️ WHAT THIS COSTS ON THE HOT PATH, stated plainly. One ingest batch of N
-- alerts averaging L labels now does N x L index probes against
-- alert_labels_pk, and WRITES only where something actually changed: a
-- re-observation of an alert whose labels are identical -- which is the
-- overwhelming majority of observations -- deletes nothing, updates nothing and
-- leaves no dead tuple, because the upsert's `WHERE label_value IS DISTINCT FROM
-- EXCLUDED.label_value` declines the no-op. The cost is therefore proportional
-- to the BATCH, never to the tenant, which is the property the read side was
-- missing. There is one contention point: two concurrent batches for one org
-- that both CREATE alerts will queue on the same handful of alert_label_names
-- rows. The wait is one row update long, it only happens on alert creation, and
-- it buys a filter bar that answers in a millisecond during the storm that
-- creates them.
--
-- ⭐ NOTHING NEEDS TO REAP THESE TABLES, and that is a consequence rather than
-- an oversight. `alerts` is never deleted (ADR 0024) with exactly one exception,
-- `internal/drill/repository/dispose.go`, which deletes SYNTHETIC alerts -- and
-- a synthetic alert is never projected here in the first place, so its disposal
-- has nothing to undo. Both tables carry org_id REFERENCES orgs ON DELETE
-- CASCADE and alert_labels also cascades from alerts, so a tenant deletion takes
-- them with it.
--
-- ⛔ SYNTHETICS ARE EXCLUDED HERE, WHICH IS WHERE THE EXCLUSION MOVES TO. A
-- drill writes an `oto_drill` label carrying a uuid, and a typeahead that
-- offered it -- with a count of one, forever -- would advertise oto's own
-- plumbing as the customer's estate. The two reads used to say `NOT a.synthetic`
-- themselves; now the rows never enter, and the reads carry no such predicate.
-- These are two of the reads listed on the `alerts.synthetic` column comment in
-- 00039, and that comment still describes them correctly: the exclusion moved
-- upstream, it did not go away.
--
-- EXPAND/CONTRACT (CONTEXT.md §6), AND THE ONE THING IT DOES NOT GIVE FOR FREE.
-- Two new tables and a backfill. A release-N pod that has never heard of them
-- keeps writing `alerts.labels` and keeps reading the old queries, and nothing it
-- writes can violate any CONSTRAINT here — no check, no FK, no unique.
--
-- ⛔ BUT CONSTRAINTS ARE NOT COMPLETENESS, AND AN EARLIER DRAFT OF THIS NOTE SAID
-- "nothing it writes can violate anything here", which is true of the former and
-- false of the latter. N and N+1 RUN SIMULTANEOUSLY. Only N+1 maintains this
-- projection; a release-N pod writes `alerts.labels` and maintains nothing. So an
-- alert CREATED by a release-N pod after this migration ran is absent from both
-- tables, and its names are under-counted in `alert_label_names`, until something
-- re-observes it. It is not corruption and it never becomes negative — it is
-- SILENT INCOMPLETENESS, bounded by the rollout window, and it self-heals for any
-- alert that is observed again (which for an Alertmanager-fed install is every
-- still-firing alert, every group interval). What does NOT self-heal is an alert
-- that fired once during the rollout and was never seen again.
--
-- ⭐ THE FIX IS THE RECONCILE BELOW, RE-RUN ONCE THE ROLLOUT HAS COMPLETED, and
-- it is the same block this migration backfills with — not a copy of it. Because
-- the projection is a PURE FUNCTION of `alerts.labels`, re-running it is
-- idempotent and total: it inserts what is missing, corrects what drifted,
-- deletes what no longer exists and re-derives every count from the rows rather
-- than from a delta. Running it on a converged install is a no-op that writes
-- nothing.
--
-- ⛔ WHY NOT A TRIGGER, since a trigger WOULD close this outright and needs no
-- operator. It was the other candidate and it loses on three counts. (1) It is
-- permanent cost on the hottest write path in the system to fix a defect that
-- exists only during one rollout — CONTEXT.md holds ingest to a p99 of 250 ms.
-- (2) It cannot coexist with `upsertAlertsSQL`'s maintenance without
-- DOUBLE-COUNTING, so adopting it means deleting that maintenance and
-- re-deriving the delta arithmetic — the bump/mint split, the −1/+1 fold, the
-- exactness under batches and concurrent mints — in PL/pgSQL, where it is far
-- harder to test than the Go-side statement that already has that arithmetic
-- right. Trading a verified implementation for an unverified one is a poor price
-- for a transient gap. (3) This schema has NO triggers. It has functions
-- (00005's partition management, 00036's retention defaults), but every one of
-- them is invoked explicitly by an operator or a job; none is implicit
-- write-path logic. Making the ingest path's correctness depend on schema-
-- resident logic is a precedent that belongs in an ADR, not in the migration that
-- happens to need it first.
--
-- The Down drops both tables and the typeahead goes back to scanning, which is
-- the state the release it rolls back to expects.

-- +goose Up

CREATE TABLE alert_labels (
  org_id      UUID NOT NULL REFERENCES orgs(id)   ON DELETE CASCADE,
  alert_id    UUID NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  label_name  TEXT NOT NULL,
  label_value TEXT NOT NULL,
  CONSTRAINT alert_labels_pk       PRIMARY KEY (org_id, alert_id, label_name),
  -- ⚠️ B4 and B5 IN CHARACTERS, WHICH IS DELIBERATELY LOOSER THAN THE DOMAIN.
  -- internal/alerts/domain/labels.go (R9) measures Go `len`, i.e. BYTES, so these
  -- admit a little more than `NewLabels` does — a 1024-CHARACTER multibyte name
  -- is up to 4096 bytes and passes here. That is on purpose and it is not the
  -- byte/character confusion fixed on alert_labels_value_idx: these are
  -- BACKSTOPS, there is no byte limit on a heap column, and a backstop that is
  -- loose can never reject data the domain accepted, while a backstop tightened
  -- to octet_length could abort this migration's backfill on an install holding
  -- one legacy row. The bound that MUST be exact in bytes is the btree one, and
  -- it is stated on the index, where the byte limit actually exists.
  CONSTRAINT alert_labels_name_ck  CHECK (length(label_name) BETWEEN 1 AND 1024),
  CONSTRAINT alert_labels_value_ck CHECK (length(label_value) <= 4096)
);

CREATE TABLE alert_label_names (
  org_id      UUID   NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  label_name  TEXT   NOT NULL,
  alert_count BIGINT NOT NULL,
  CONSTRAINT alert_label_names_pk       PRIMARY KEY (org_id, label_name),
  CONSTRAINT alert_label_names_count_ck CHECK (alert_count >= 0),
  CONSTRAINT alert_label_names_name_ck  CHECK (length(label_name) BETWEEN 1 AND 1024)
);

-- ╔═══════════════════════════════════════════════════════════════════════════╗
-- ║ THE RECONCILE. Re-runnable, idempotent, and total.                        ║
-- ╚═══════════════════════════════════════════════════════════════════════════╝
--
-- ⭐ THIS BLOCK IS BOTH THE BACKFILL AND THE ROLLOUT REPAIR, which is why it is
-- written to be re-run rather than to assume empty tables. Copy these four
-- statements into a transaction and run them once the N→N+1 rollout has
-- completed, to pick up the alerts release-N pods created without maintaining
-- the projection (see EXPAND/CONTRACT above). On a converged install every one
-- of them matches zero rows. There is no second copy of this SQL to drift from.
--
-- ⛔ IT RUNS BEFORE THE INDEXES, AND THAT ORDER IS LOAD-BEARING. If
-- alert_labels_value_idx already existed, an install holding a label that
-- overflows a btree tuple would abort the INSERT, and since this is a goose
-- migration that is not a 500 on one request — it is a MIGRATION THAT WILL NOT
-- APPLY, on exactly the installs that have the most data. The partial predicate
-- is now byte-correct so no such row can reach the index anyway, but building the
-- indexes after the rows removes the question entirely, and bulk-loading first
-- and indexing after is the faster order regardless.
--
-- coalesce because jsonb_each_text renders a JSON null as SQL NULL: the domain's
-- label values are strings, and a label that somehow is not becomes the empty
-- string rather than a 23502 in the middle of a migration.

-- 1. Every (alert, name) the source says should exist, with the value it says.
--
-- ⛔ `octet_length(e.key) <= 1024` IS NOT THE INDEX PREDICATE REPEATED, IT IS THE
-- ONE BOUND A PARTIAL INDEX CANNOT EXPRESS. `alert_labels_pk` is a PRIMARY KEY
-- over (org_id, alert_id, label_name) and a primary key CANNOT BE PARTIAL, so the
-- name in it is subject to the same 2704-byte btree cap with no predicate
-- available to exclude it. Measured: a name of 700 astral code points is 2800
-- bytes and fails `alert_labels_pk` on INSERT, and `alert_labels_name_ck` does
-- not stop it because that check counts CHARACTERS and 700 is under 1024.
--
-- `NewLabels` can never produce such a name — the label-name charset admits only
-- [a-zA-Z_][a-zA-Z0-9_]*, so a name that came through the domain is ASCII and B4
-- already caps it at 1024 bytes. But this statement does not read the domain, it
-- reads `alerts.labels` as it FOUND it, on an install that may predate that
-- enforcement. Without this bound one such legacy row is not a dropped label, it
-- is a MIGRATION THAT WILL NOT APPLY — and unlike the index, no reordering can
-- avoid it, because the primary key is created with the table.
--
-- So B4 is applied here in BYTES, which is the unit B4 is written in: a name
-- beyond it was never a valid label, it is excluded from the projection rather
-- than allowed to block the deploy, and `alerts.labels` still holds it verbatim.
INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
SELECT a.org_id, a.id, e.key, coalesce(e.value, '')
  FROM alerts a, LATERAL jsonb_each_text(a.labels) AS e(key, value)
 WHERE NOT a.synthetic
   AND octet_length(e.key) <= 1024
    ON CONFLICT ON CONSTRAINT alert_labels_pk DO UPDATE
   SET label_value = EXCLUDED.label_value
 WHERE alert_labels.label_value IS DISTINCT FROM EXCLUDED.label_value;

-- 2. Every row the source no longer backs: a name an alert dropped while a
--    release-N pod was the one writing, or an alert that became synthetic.
DELETE FROM alert_labels l
 WHERE NOT EXISTS (
   SELECT 1 FROM alerts a
    WHERE a.id = l.alert_id AND a.org_id = l.org_id
      AND NOT a.synthetic AND a.labels ? l.label_name);

-- 3. The counts, RE-DERIVED FROM THE ROWS rather than nudged by a delta — which
--    is what makes this repair total where the ingest path's exact arithmetic
--    cannot be, having never seen the writes it missed.
INSERT INTO alert_label_names (org_id, label_name, alert_count)
SELECT org_id, label_name, count(*)
  FROM alert_labels
 GROUP BY org_id, label_name
    ON CONFLICT ON CONSTRAINT alert_label_names_pk DO UPDATE
   SET alert_count = EXCLUDED.alert_count
 WHERE alert_label_names.alert_count IS DISTINCT FROM EXCLUDED.alert_count;

-- 4. Names that now back nothing are ZEROED, not deleted — the same rule the
--    ingest path follows, so a reconcile leaves the table in a state the ingest
--    path would also have produced.
UPDATE alert_label_names n
   SET alert_count = 0
 WHERE n.alert_count <> 0
   AND NOT EXISTS (SELECT 1 FROM alert_labels l
                    WHERE l.org_id = n.org_id AND l.label_name = n.label_name);

-- Serves distinctLabelValuesSQL: (org_id, label_name) equality then a
-- label_value prefix RANGE. PARTIAL, in BYTES, on BOTH columns, for the reason
-- argued at length above: this index is written by the ingest path, a btree tuple
-- is capped at 2704 bytes, and `length()` would have measured characters.
-- ⛔ distinctLabelValuesSQL repeats this predicate VERBATIM. Change both or
-- neither; dropping either conjunct there silently costs the index.
CREATE INDEX alert_labels_value_idx
    ON alert_labels (org_id, label_name, label_value text_pattern_ops)
 WHERE octet_length(label_name) <= 1024 AND octet_length(label_value) <= 512;

-- Serves distinctLabelNamesSQL whole: the ORDER BY as well as the filter, so the
-- LIMIT stops the scan instead of a Sort consuming the org first.
CREATE INDEX alert_label_names_rank_idx
    ON alert_label_names (org_id, alert_count DESC, label_name)
 WHERE alert_count > 0;

COMMENT ON TABLE alert_labels IS
  'The exact expansion of alerts.labels, one row per (alert, label name), for NON-SYNTHETIC alerts only. A PROJECTION: alerts.labels is the truth, and this table is a pure function of it, so it can always be rebuilt by the same statement 00045 backfilled it with. It exists because no index on alerts can serve the label typeahead — jsonb_object_keys is set-returning and cannot appear in an index expression at all, and labels ->> $2 takes the label name as a runtime parameter. Maintained inside upsertAlertsSQL by full replacement of one alert set at a time, which is race-free where a delta would not be.';
COMMENT ON COLUMN alert_labels.label_value IS
  'Stored in full up to B5 (4096 bytes). Only rows whose name is at most 1024 BYTES and whose value is at most 512 BYTES are INDEXED, by alert_labels_value_idx; see its comment for why that bound is on the index rather than on the column, why it is measured in bytes, and why it names both columns.';
COMMENT ON INDEX alert_labels_value_idx IS
  'Serves distinctLabelValuesSQL: org_id and label_name as equalities, label_value as a PREFIX RANGE, so a longer prefix narrows the work rather than only the result. text_pattern_ops because LIKE becomes an index range only under byte ordering, and the deployment does not pin a collation. PARTIAL because B5 admits a 4096-byte value, a btree tuple is capped at 2704 bytes, and an over-long entry ERRORS the INSERT rather than skipping the index — which, since this index is written by the ingest path, is an ingest outage caused by one incompressible label. The predicate counts OCTETS, not characters: length() would measure characters, and 512 astral code points are 2048 bytes, so a character-based predicate ADMITS the row that overflows rather than excluding it. It bounds BOTH columns because label_name is part of the index row — the value alone cannot reach 2704 and what overflows is the sum. Largest tuple the predicate admits, measured: 1568 bytes against the 2704 maximum. distinctLabelValuesSQL repeats the predicate verbatim, both conjuncts, or the planner cannot prove the partial index applies.';
COMMENT ON TABLE alert_label_names IS
  'The label-cardinality projection behind GET /api/v1/labels: one row per (org, label name) with the number of non-synthetic alerts carrying it. Bounded by the org LABEL cardinality rather than its ALERT count, which is the point — the name typeahead opens with an empty prefix and so has no range to narrow, and reading it off alert_labels would still have been a full pass. Maintained by an exact delta inside upsertAlertsSQL, taken from the RETURNING of the alert_labels delete and insert rather than from a re-read of the old labels.';
COMMENT ON COLUMN alert_label_names.alert_count IS
  'Alerts carrying this name. Kept at zero rather than deleted when the last one loses it; every reader filters alert_count > 0, as does alert_label_names_rank_idx. alert_label_names_count_ck refuses a negative value on purpose: a count projection that can drift silently is worse than one that fails the ingest transaction that broke it.';
COMMENT ON INDEX alert_label_names_rank_idx IS
  'Serves distinctLabelNamesSQL end to end — the ORDER BY alert_count DESC, label_name as well as the org filter — so the LIMIT terminates the scan instead of a Sort reading every name first. Partial on the same alert_count > 0 the query carries, so zeroed names cost nothing to keep. Proved against a real plan in internal/alerts/repository/labels_plan_test.go, with the index dropped inside a rolled-back transaction as the control.';

-- +goose Down

DROP TABLE IF EXISTS alert_label_names;
DROP TABLE IF EXISTS alert_labels;
