-- The AlertGroup stops mirroring Alertmanager's grouping and starts being
-- DERIVED from the alert's own labels (ADR 0038, SPEC §C.4).
--
--   before   group_key = H(org, source_id, receiver, AM's groupLabels)
--   after    group_key = H(org, cluster_key, alertname, namespace-or-∅)
--
-- ⭐⭐ WHY. Under ADR 0005 the group OWNS a chat thread, so the group key IS the
-- identity of oto's most visible output — and it was computed from a grouping oto
-- neither chooses nor can reproduce. Editing `group_by` in `alertmanager.yml`
-- shifted the whole key space and orphaned every in-flight thread; `continue:
-- true` put one alert in two groups at once, which `alert_group_members`'
-- PRIMARY KEY (group_id, occurrence_id) permits by design and which is itself the
-- proof that membership was never an identity claim. Meanwhile `GET
-- /api/v2/alerts` returns no grouping at all, so reconciler-sourced groups got an
-- empty receiver and no group labels (see 00008 line 81, citing SPEC §C.4) — two
-- ingest paths, two different answers to "which thread does this belong to".
--
-- The axes are the ones `internal/alerts/domain/labels.go` already promotes,
-- minus the two that must not split: `severity`, because an escalation is the
-- same problem getting worse and a group's severity is an aggregate that only
-- means something if both live in one group, and `pod`/`instance`, which is the
-- thing being grouped. `service` is omitted until evidence says otherwise.
--
-- ⚠️⚠️ THE AXES ARE AS-YET UNVALIDATED AGAINST PRODUCTION PAYLOADS. The key is
-- computable from `ingest_batches.payload`, which is retained 30 days;
-- `tools/groupreplay` computes it over stored bodies and reports the resulting
-- group-size distribution against the thread count Alertmanager's own grouping
-- produced. It has only been run against synthetic fixtures.
--
-- ⛔ THERE IS NO BACKFILL, AND THAT IS THE WHOLE MIGRATION'S HARDEST DECISION.
-- Every existing `group_key` was computed under the old rule and is now
-- unreachable: the next observation for one of those alerts computes a different
-- key, finds no open generation under it, and opens generation 1 of a NEW group
-- with a NEW thread. The old generation keeps its thread, keeps its history, and
-- closes on its own through the ordinary `group.close` sweep once no member is
-- live and `group_close_delay_s` has elapsed.
--
-- Re-keying in place was considered and refused twice over:
--
--   * IT IS NOT COMPUTABLE HERE. The new key needs a member alert's `alertname`
--     and `namespace`, which live on `alerts`, not on `alert_groups`. An UPDATE
--     could join through `alert_group_members` — but a group whose members
--     disagree about `alertname` (the common case under `group_by: [cluster]`)
--     has no single new key, so the join would have to CHOOSE one and silently
--     merge the rest.
--   * IT WOULD RE-PARENT A LIVE SLACK THREAD, which Slack cannot do. A generation
--     that changed its key would keep `channel_threads` pointing at a
--     conversation that is now about something else.
--
-- The cost is one transitional day on which some incidents own two threads: the
-- old generation, frozen at whatever it last said, and the new one that carries
-- it forward. That is visible, bounded and self-healing. A silent merge is not.
--
-- ⭐ NOTHING IS INDEXED THAT WAS NOT INDEXED BEFORE, and the reason is worth
-- stating because the obvious alternative is a trap.
--
-- The axes are hashed IN Go and the result is stored as `alert_groups.group_key`,
-- a NOT NULL TEXT column already covered by `grp_open_idx (org_id, group_key)
-- WHERE state = 'open'` — a plain btree over a non-null column, so the ingest hot
-- path's lookup is a real index range. The axes themselves are stored in
-- `group_labels`, a NOT NULL JSONB where ∅ is the ABSENCE of the `namespace` key.
--
-- ⛔ The tempting alternative — promoting the axes to columns and indexing them —
-- would have re-imported a defect this schema already has. `alerts.namespace` is
-- NULLABLE (00007 line 30), so every read that buckets on it must write
-- `COALESCE(namespace, '')` (internal/alerts/repository/alert.go), and a plain
-- btree on the BARE column cannot produce an index range over the wrapped
-- expression — it supplies grouped ordering and nothing more. Indexing the axes
-- would therefore have required indexing the EXPRESSION, or a generated column,
-- or a NOT NULL DEFAULT '' that makes "" and ∅ indistinguishable in SQL while the
-- Go key keeps them distinct. Hashing in Go and storing one opaque non-null key
-- avoids the whole question: there is no nullable axis column to range over.
--
-- ⭐ `receiver` AND `source_group_key` SURVIVE AS PROVENANCE. They record what
-- Alertmanager was doing when the generation opened, they are rendered in a
-- card's footer, and they are no part of any identity. Dropping the COLUMNS is a
-- separate change: they are on the public `AlertGroup` schema, on the outbound
-- webhook envelope and in the `listAlertGroups` `receiver=` filter, so removing
-- them is an API contract edit and not a grouping one.

-- ⛔⛔ THERE IS DELIBERATELY NO `groups_axes_ck`, AND THE FIRST DRAFT OF THIS FILE
-- HAD ONE. It read
--
--     ALTER TABLE alert_groups ADD CONSTRAINT groups_axes_ck
--       CHECK (group_labels ->> 'alertname' IS NOT NULL) NOT VALID;
--
-- on the reasoning that the axes are an invariant of the WRITER and `NOT VALID`
-- excuses the rows already there. THE SECOND HALF IS FALSE. `NOT VALID` skips the
-- one-time VALIDATION SCAN; it does not exempt those rows from the constraint
-- afterwards. PostgreSQL re-checks every CHECK against the NEW ROW VERSION on
-- every UPDATE, so a legacy row that violates the predicate cannot be updated at
-- all — not even by an UPDATE that never mentions `group_labels`. Verified on the
-- deployment's own Postgres 17, inside a transaction that was rolled back:
--
--     UPDATE t SET state = 'closed' WHERE id = 1;      -- group_labels = {}
--     ERROR:  new row for relation "t" violates check constraint "t_ck"
--     DETAIL:  Failing row contains (1, {}, closed).
--     UPDATE t SET n = n + 1 WHERE id = 2;             -- {"cluster":"c1"}
--     ERROR:  new row for relation "t" violates check constraint "t_ck"
--
-- ⛔⛔ AND THE STATEMENT IT REFUSES IS THIS MIGRATION'S OWN HEALING MECHANISM.
-- The paragraph above promises that an old generation "closes on its own through
-- the ordinary `group.close` sweep". That sweep is `updateRollupSQL` and then
-- `closeGroupSQL` (internal/grouping/repository/group.go) — two UPDATEs of exactly
-- the rows the constraint refuses — and `CloseIdle`
-- (internal/grouping/service/service.go) catches the failure and logs a warning,
-- so nothing crashes and nothing recovers. Every pre-00050 generation would stay
-- open FOREVER, its Slack thread never freezing, one warning per legacy group per
-- tick until an operator dropped the constraint by hand. "Visible, bounded and
-- self-healing" would have become invisible, unbounded and permanent, and the
-- migration would have been the thing that broke its own only escape route.
--
-- ⭐ SO THE INVARIANT IS THE WRITER'S, AND IT IS STATED HERE RATHER THAN ENFORCED
-- IN SQL. `group_labels` has exactly one writer: `GroupRepository.OpenGeneration`,
-- reached only from `grouping/service.ResolveGroup`, which passes
-- `kernel.SplitLabels(labels).Map()`. `SplitLabels` is TOTAL — it starts from a
-- `LabelSet`, whose own invariant is a non-empty `alertname`, so there is no label
-- set it can project without one. A CHECK would have bought a backstop against a
-- SECOND writer that bypasses `SplitLabels`; it would have cost every legacy
-- generation its ability to close.
--
-- The two shapes that keep the backstop were weighed and refused for the reason the
-- indexing paragraph above refuses promoting the axes to columns: both leave a
-- PERMANENT mechanism behind to solve a TRANSITIONAL problem. A discriminator
-- column (`NULL` on pre-00050 rows, `true` on new ones, CHECK gated on it) is a
-- column on the entity's table that means nothing to the domain and everything to
-- one migration. This schema's first row-level trigger — `BEFORE INSERT`, which
-- does enforce exactly "rows created at or after 00050" and was confirmed to leave
-- legacy UPDATEs alone — adds a plpgsql exception path to the one write that opens
-- a thread, and still cannot cover an UPDATE. Neither is worth a discriminator that
-- becomes meaningless the day the last pre-00050 generation closes.
--
-- What remains enforced is `groups_labels_ck` from 00008: `group_labels` is a JSONB
-- OBJECT. `{}` is a legal object, and from here on it means "a generation opened
-- before the derived key", which is exactly what it is.

-- +goose Up

COMMENT ON TABLE alert_groups IS
  'ONE GENERATION of one MACHINE-DERIVED grouping of alerts, keyed by (org, cluster, alertname, namespace-or-empty) — the alert own labels, never Alertmanager grouping (ADR 0038). OWNS EXACTLY ONE Slack thread. A closed group that re-opens gets a new generation and therefore a new thread. Never means a UI grouping -- that is a view (SPEC §A.1).';

COMMENT ON COLUMN alert_groups.group_key IS
  'gk_ + 26 base32hex chars over sha256(org_id, cluster_key, canon(alertname, namespace-or-empty)) (SPEC §C.4, ADR 0038). FIXED, NOT CONFIGURABLE: a tunable split key reinvents group_by inside oto and re-inherits the problem it was built to escape. Computed identically on the webhook path and the reconciler path, which is what makes the two agree about which thread an alert belongs to.';

COMMENT ON COLUMN alert_groups.group_labels IS
  'oto OWN split axes for this generation: alertname, plus namespace when the alert has one. An ABSENT namespace is its own partition and is recorded as the ABSENCE of the key, never as an empty string. THE PRESENCE OF alertname IS AN INVARIANT OF THE WRITER AND IS NOT ENFORCED IN SQL: kernel.SplitLabels is total over a LabelSet that already refuses an empty alertname, and a CHECK cannot be added over pre-00050 rows without making them permanently un-UPDATE-able -- see this migration header. A row with no alertname is a generation opened before ADR 0038. EVERY NOTIFICATION MATCHER IS FED THIS MAP, which is why it stopped being Alertmanager groupLabels: a policy matching namespace used to match nothing unless the operator had put namespace in group_by, and it failed quietly as a no_policy suppression rather than as an error.';

COMMENT ON COLUMN alert_groups.receiver IS
  'PROVENANCE ONLY since ADR 0038: the Alertmanager receiver that first delivered into this generation, empty for a reconciler-sourced one. It is NOT part of group_key. While it was, one alert routed to two receivers by continue:true occupied two groups and two threads at once. Removing it MERGES routes that deliberately separated the same alerts; cluster_key is what must distinguish them, which it should be anyway since alert identity is already (org, cluster).';

-- ⛔ Wrapped for the reason the Down's last statement is: the ` -- ` inside the
-- string hides the terminating `;` from goose's line scanner
-- (`endsWithSemicolon` stops at the first `--`-prefixed word), and this is the
-- last statement in the block, so nothing later flushes it by accident. Without
-- the delimiters goose refuses the file with `unexpected unfinished SQL query`.
-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.source_group_key IS
  'Alertmanager raw groupKey, kept verbatim for observability. OPAQUE -- MUST NOT BE PARSED: it is unescaped and unbounded (SPEC §C.4). Since ADR 0038 it is an input to nothing at all; the full envelope it came from is in ingest_batches.payload.';
-- +goose StatementEnd

-- +goose Down

-- Only the prose comes back. There was never a constraint to drop (the header
-- says why), and the keys do not come back either: a Down cannot undo a re-key —
-- the generations opened under the derived rule exist, own threads and have
-- history, and computing their old key would need a receiver and a groupLabels
-- set that were never recorded on them. Rolling back the CODE makes those
-- generations unreachable in the same self-healing way the Up made the old ones
-- unreachable, which is the honest and only available behaviour.

COMMENT ON TABLE alert_groups IS
  'ONE GENERATION of one Alertmanager notification group, from (source, receiver, groupLabels). OWNS EXACTLY ONE Slack thread. A closed group that re-opens gets a new generation and therefore a new thread. Never means a UI grouping -- that is a view (SPEC §A.1).';
COMMENT ON COLUMN alert_groups.group_key IS
  'gk_ + 26 base32hex chars over sha256(org_id, source_id, receiver, canon(groupLabels)) (SPEC §C.4). STABLE ACROSS alertmanager.yml ROUTE EDITS, which is exactly what Alertmanager own groupKey is not.';
COMMENT ON COLUMN alert_groups.group_labels IS NULL;
COMMENT ON COLUMN alert_groups.receiver IS
  'Alertmanager receiver name. Empty string for reconciler-sourced groups with no groupLabels (SPEC §C.4).';
-- ⛔ Wrapped because the ` -- ` inside the string hides the terminating `;` from
-- goose's line scanner, and this is the last statement in the block, so nothing
-- later closes it by accident. Without the delimiters goose refuses the file.
-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.source_group_key IS
  'Alertmanager raw groupKey, kept verbatim for observability. OPAQUE -- MUST NOT BE PARSED: it is unescaped and unbounded (SPEC §C.4).';
-- +goose StatementEnd
