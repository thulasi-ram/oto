-- `alert_cases` gains `number`: the per-ORG name of a case, allocated at INSERT
-- from a counter that belongs to the tenant and to nothing else.
--
-- ⭐⭐ WHAT WAS WRONG, AND IT WAS ON SCREEN RATHER THAN IN THE DATA. `seq` is the
-- firing ordinal WITHIN one Alert -- 1, 2, 3 as an identity re-fires -- and it is
-- correct, load-bearing (`case_seq_uniq`, T1-vs-T7 in `OpenNewCase`) and going
-- nowhere. What it is NOT is a name for a case. A queue of forty cases belonging
-- to forty alerts that have each fired once renders forty rows reading `#1`, and
-- an operator reading that column top to bottom concludes the counter is broken.
-- The column was answering a question nobody asked in a list, in the position
-- where every reader expects the row's identifier.
--
-- ⭐ SO THE TWO FACTS BECOME TWO COLUMNS. `number` names the case -- monotonic
-- within the org, 1-based, the thing a human quotes -- and `seq` goes on saying
-- which firing of its alert it is, which is the only question it was ever good at
-- and the only one the fold on `/cases` asks.
--
-- ⛔ IT IS PER-ORG AND NOT GLOBAL, AND THAT IS A TENANCY RULE RATHER THAN
-- TIDINESS. A single sequence shared by every tenant leaks volume across the
-- boundary: org A reads its own numbers, sees them jump by 900 overnight, and has
-- learnt how busy org B was. That is the same class of thing as answering 403 on
-- a cross-tenant id -- an oracle assembled from a value that looked harmless --
-- and this schema refuses those. Every org counts from 1.
--
-- ⛔ THE COUNTER IS ITS OWN TABLE AND NOT A COLUMN ON `orgs`, FOR ONE OPERATIONAL
-- REASON. Allocation takes a row lock held to the end of the ingest transaction.
-- On `orgs` that lock is shared with `orgs.settings`, so an operator saving the
-- tuning page would block behind an Alertmanager batch, and a batch would block
-- behind the settings save. The counter is hot and the tenant root is not; they do
-- not belong in the same row.
--
-- ⚠️ ALLOCATION SERIALISES CONCURRENT CASE-OPENS WITHIN ONE ORG, and it must:
-- gapless-by-construction and concurrent are not both available. Two ingest
-- transactions opening cases for the same org queue on this row for as long as
-- the first holds its transaction open. That is the same bargain
-- `notification_threads.next_seq` already makes for thread ordering, at the same
-- scale -- one row, one increment -- and it is bounded by the batch, not by the
-- work the batch does afterwards.
--
-- ⚠️ THE SEQUENCE MAY STILL GAP, AND NOTHING PROMISES OTHERWISE. A transaction
-- that allocates and then rolls back has spent its number. `number` is a NAME --
-- unique, ordered, human-quotable -- and is not a count of anything; a reader
-- who subtracts two of them has computed a number with no meaning.

-- +goose Up

-- --------------------------------------------------------- the counter

-- ⛔ NO `updated_at`, AND ITS ABSENCE IS A DECISION. Every other table here
-- carries one, and every other table here is read by something that wants to know
-- when it last changed. This row is bumped on every case-open and read by nothing
-- -- the allocated value comes back on the allocating statement -- so a timestamp
-- would be a second write to the hottest row in the schema in exchange for a fact
-- with no reader. The `alert_cases` row the allocation produced carries the time
-- this counter moved, which is the same instant and is already indexed.
CREATE TABLE org_case_numbers (
  org_id      UUID   PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  next_number BIGINT NOT NULL DEFAULT 1,
  CONSTRAINT org_case_numbers_next_ck CHECK (next_number >= 1)
);

-- +goose StatementBegin
COMMENT ON TABLE org_case_numbers IS
  'One row per org holding the next alert_cases.number to hand out. Written by the case-open path as a single UPSERT-and-increment inside the ingest transaction, and read by nothing: the allocated value comes back on the same statement. A missing row means the org has opened no case yet and allocation creates it at 2, having handed out 1.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN org_case_numbers.next_number IS
  'The number the NEXT case in this org will take. Never decreases. A rolled-back transaction leaves its allocation spent, so alert_cases.number is unique and ordered but not gapless -- it is a name, not a count.';
-- +goose StatementEnd

-- --------------------------------------------------- alert_cases: number

-- Added nullable, backfilled, then made NOT NULL. There is no DEFAULT that could
-- be written here: the value depends on the org of the row being inserted, and a
-- schema with no functions and no triggers has nowhere to express that. The
-- INSERT supplies it, and the constraints below are what make "the INSERT
-- supplies it" enforceable rather than a convention.
ALTER TABLE alert_cases ADD COLUMN number BIGINT;

-- The backfill orders by the same key `/cases` reads in: the oto clock, with the
-- uuidv7 as tiebreak. That is time-ordered too, so cases opened inside one
-- transaction keep the order they were opened in rather than an arbitrary one.
-- +goose StatementBegin
WITH numbered AS (
  SELECT id,
         row_number() OVER (PARTITION BY org_id ORDER BY started_at, id) AS n
    FROM alert_cases
)
UPDATE alert_cases c
   SET number = numbered.n
  FROM numbered
 WHERE c.id = numbered.id;
-- +goose StatementEnd

-- Seeded from what the backfill actually wrote rather than from a count, so an
-- org whose rows were partially deleted still gets a counter above its highest
-- surviving number instead of one that collides with it.
-- +goose StatementBegin
INSERT INTO org_case_numbers (org_id, next_number)
SELECT org_id, max(number) + 1
  FROM alert_cases
 GROUP BY org_id
ON CONFLICT (org_id) DO NOTHING;
-- +goose StatementEnd

ALTER TABLE alert_cases ALTER COLUMN number SET NOT NULL;

-- The uniqueness that makes the number a name. Scoped by org for the reason the
-- header states: two tenants both counting from 1 is the point, not a collision.
ALTER TABLE alert_cases ADD CONSTRAINT case_number_uniq UNIQUE (org_id, number);

ALTER TABLE alert_cases ADD CONSTRAINT case_number_ck CHECK (number >= 1);

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.number IS
  'The case name within its org: 1-based, monotonic, allocated from org_case_numbers at INSERT. This is what a human quotes and what /cases leads each row with. It is NOT alert_cases.seq, which is the firing ordinal within one Alert and answers a different question -- forty alerts that have each fired once carry forty different numbers and forty seq=1.';
-- +goose StatementEnd

-- +goose Down

ALTER TABLE alert_cases DROP CONSTRAINT case_number_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_number_uniq;
ALTER TABLE alert_cases DROP COLUMN number;
DROP TABLE org_case_numbers;
