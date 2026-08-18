-- SPEC §L.4.2 (SPEC §P-4) -- alerts_sev_ck, and the §D.4.0 column ban restated.
--
-- alerts.severity stores the RAW upstream label value and is bounded only by
-- length. Users filter on their OWN vocabulary -- sev1, P1, page, ticket -- and
-- normalising at write time would destroy it. Normalisation happens at RENDER
-- time, in the domain, via SeverityFromLabel, which is why §H.2 "anything else /
-- absent -> :white_circle:" row is correct and stays.
--
-- ⛔ DO NOT ADD AN ENUM CHECK ON alerts.severity. There is none, and adding one
-- would be a bug (SPEC §L.4.2). alerts_sev_ck is a LENGTH bound and nothing more.
-- Severity is strict in the domain (NewSeverity, for API input and config) and
-- lenient at the boundary (SeverityFromLabel, total, for upstream labels). That
-- asymmetry is the whole ruling; this constraint is only the backstop that keeps
-- a 4 KB label value out of a promoted column.

-- +goose Up

-- ⛔ D.4.0 THE COLUMN BAN (BINDING, PERMANENT) -- SPEC §D.4.0, §P-9.
--
-- `alerts`, `alert_occurrences` and `alert_groups` MUST NEVER gain `assigned_to`,
-- `assignee_id`, `owner_id`, `owner_team_id`, `watchers`, `subscriber_ids`,
-- `incident_id`, `ticket_id`, `status_page_id`, human-set `priority`,
-- `sla_due_at`, or ANY nullable person-reference with a present-tense meaning.
--
-- `acked_by` is past-tense attribution and is the ONLY person reference permitted
-- on a signal row.
--
-- This single clause is worth more than the rest of the scope doctrine combined,
-- because it is the door that every slippery-slope feature must walk through.
-- `occurrence.acked_by = alice` is a fact about the occurrence -- IT WAS
-- ACKNOWLEDGED, BY WHOM. `occurrence.assigned_to = alice` is a fact about Alice --
-- SHE OWES WORK. Identical columns; opposite products (SCOPE-BOUNDARY §1, FR-1).
--
-- The temptation is precise and will recur: a person is already stored on this
-- row, so adding "assign" looks like one nullable column. What it drags in is an
-- assignee lifecycle -> notification TO THE ASSIGNEE -> a "my alerts" view ->
-- workload balancing -> a rota to pick the default assignee. That is Opsgenie,
-- arrived at in five reasonable pull requests (SCOPE-BOUNDARY §6 SS-1).
--
-- The sanctioned answer to "who is looking at this?" is EPHEMERAL PRESENCE --
-- derived live from open SSE connections, never persisted, gone when the tab
-- closes. It is a `streaming` feature.
--
-- alerts.snoozed_until (00017) is NOT an exception: it is a bare timestamp
-- projection of alert_snoozes, and the attribution lives on that side table
-- precisely so this ban stays absolute (SPEC §D.8b).
--
-- ⭐ AMENDMENT -- READ THIS BEFORE APPLYING THE CLAUSE ABOVE. The clause is
-- BINDING AND UNCHANGED; two of the names it uses are not current, and the text
-- above is left verbatim because it is what §D.4.0 said at THIS migration's
-- version, not because it is a description of today's schema.
--
--   * `alert_occurrences` IS `alert_cases` (00052, ADR 0036). The ban travelled
--     with the table and did not narrow: read every `alert_occurrences` and
--     `occurrence.` above as `alert_cases` and `case.`. SCOPE-BOUNDARY §5.6 and
--     §5.1 carry the current spelling, and `test/scope/forbidden_columns_test.go`
--     enforces the ban against the LIVE schema, so the doctrine is mechanical
--     rather than dependent on this comment being re-typed.
--   * `alerts.snoozed_until` NO LONGER EXISTS (00048). Suppression reads
--     `alert_snoozes` directly. The paragraph above survives because its RULING
--     survives -- a bare-timestamp projection of a side table would not have been
--     an exception to the ban -- but the column it exempts is gone, and
--     `test/arch/snoozecolumn_test.go` now refuses its reintroduction.
--
-- Nothing here weakens the ban. `assigned_to`, `owner_id`, `watchers`,
-- `incident_id`, `sla_due_at` and every sibling are still forbidden on `alerts`,
-- `alert_cases` and `alert_groups`, permanently.

ALTER TABLE alerts ADD CONSTRAINT alerts_sev_ck
  CHECK (severity IS NULL OR length(severity) BETWEEN 1 AND 256);

COMMENT ON COLUMN alerts.severity IS
  'Promoted label, stored RAW and never normalised (SPEC §L.4.2). Drives the Slack EMOJI, never the Slack colour (SPEC §H.1). Bounded by alerts_sev_ck to 1..256 characters; a LENGTH check, deliberately NOT an enum: users filter on their own vocabulary (sev1, P1, page) and normalising at write time would destroy it. Snooze NEVER alters severity: a snoozed critical is still critical (SPEC §B.8.1).';

-- +goose Down

ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_sev_ck;

COMMENT ON COLUMN alerts.severity IS 'Promoted label. Drives the Slack EMOJI, never the Slack colour (SPEC §H.1).';
