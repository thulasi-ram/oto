-- SPEC §B.5, §B.6 -- `case_policy_config` carries a CASE RETENTION WINDOW W per
-- (namespace, alertname), and `alert_cases` gains the two columns that record a
-- resolve whose close is still pending.
--
-- ⭐⭐ WHAT WAS WRONG. A Case opens when an alert starts firing and closes when it
-- resolves, and since 00054 it is STRICTLY TERMINAL: a re-fire opens the next
-- `seq`. An alert that resolves and re-fires six times in ten minutes therefore
-- produces SIX cases, six root cards, six thread replies and six pings, and
-- nothing merges them because nothing may. The only damper left was at delivery --
-- `notifications.suppressed_reason = 'flapping'` plus a coalesced digest -- which
-- HIDES the noise instead of not making it, and a withheld notification is
-- indistinguishable from a signal that never fired. That is the one thing an
-- alerting product cannot afford (§B.6).
--
-- ⭐ WHAT W IS. A case whose alert has resolved stays OPEN for W and closes only
-- once the alert has stayed resolved for W. A re-fire inside W finds the case
-- STILL OPEN, so it is an ordinary repeat observation (§B.3 T2) and not a new
-- episode. One case across the flap, one notification, one thread reply.
--
-- ⛔⛔ IT IS A DELAYED CLOSE AND NEVER A REOPEN. `case.reopened` and T8 were
-- retired by ADR 0040 and migration 00054 and are not coming back: an episode
-- closes ONCE, `ended_at` is written once, and `case_terminal_ended` still refuses
-- the combination that would resurrect a closed row. The whole mechanism is that
-- the close happens LATER, not that a closed thing is un-closed. `case_one_open_idx`
-- therefore keeps its meaning unchanged -- at most one open episode per Alert --
-- because the retained episode is the open one and no second one is minted.
--
-- ⭐⭐ W DEFAULTS TO 0 AND 0 IS TODAY'S BEHAVIOUR, NOT AN APPROXIMATION OF IT.
-- `retention_window_s` is `NOT NULL DEFAULT 0`, the table starts EMPTY, and a
-- missing row means 0. `internal/alerts/domain/lifecycle.go`'s T5 arm takes the
-- deferral branch only under `cmd.CaseRetention > 0`; at zero it runs the same
-- statements in the same order it ran before this migration existed, and the two
-- columns below stay NULL for every row. No deployment changes behaviour until an
-- operator writes a row here. That is what makes this file safe to ship.
--
-- ⭐ WHY (namespace, alertname) AND NOT SOMETHING FINER. They are ADR 0038's OWN
-- AXES -- `SplitLabels(ls) = {alertname} ∪ {namespace}` -- so an operator learns
-- one set of dimensions for grouping and for retention rather than two. An ABSENT
-- namespace is its own partition there; here it is spelled `''`, because a NULL
-- would defeat `case_policy_axes_uniq` (two NULLs are not equal in a UNIQUE index,
-- so an org could hold two contradictory windows for the same alertname). ADR 0038
-- already folds EMPTY onto ABSENT -- Prometheus treats them as equivalent and
-- `alerts.namespace` stores NULL for both -- so the sentinel loses nothing, and
-- every reader must look the value up as `COALESCE(alerts.namespace, '')`.
--
-- ⚠️ IT IS COARSER THAN THE GROUP KEY BY EXACTLY ONE AXIS, AND THAT IS DELIBERATE.
-- `group_key = H(org, cluster_key, alertname, namespace-or-∅)` (00050) also keys on
-- CLUSTER. W does not: one window governs the same alertname in every cluster of an
-- org. A per-cluster window is the SPLITTING direction, which ADR 0038 records as
-- the safe one to take later; starting split would make an operator configure the
-- same number once per cluster to get the obvious behaviour.
--
-- ⭐ WHY THE PENDING RESOLVE NEEDS TWO COLUMNS AND NOT ONE.
--   * `resolve_pending_at` is OTO'S clock: when the close becomes DUE. It is what
--     the sweep scans, so it is the indexed one, and it moves forward every time a
--     fresh resolve lands inside W -- "stayed resolved for W", not "resolved W ago".
--   * `resolve_pending_end_at` is the `ended_at` THE CLOSE WILL STAMP: the upstream
--     claim from the resolve observation, already clamped to >= `started_at` by
--     §B.3.2. Without it the firing duration would silently include W, and every
--     reader of `ended_at` -- the case list's duration column, the daily rollup, the
--     firing-duration statistic (R8) -- would report an episode W longer than the
--     signal actually burned. The window is oto's own damper and must not be
--     charged to the signal.
--
-- ⛔ THE RECEIPT IS WHAT KEEPS "NEVER FABRICATE A RESOLUTION" TRUE. 00007 calls
-- resolved-versus-expired the distinction oto must never blur, and `sweep.go` states
-- it as "no code path here can produce resolved". A close driven by the sweep
-- still cannot INVENT one: it may only complete a resolve whose receipt is already
-- on the row, and these two columns ARE that receipt -- written by the T5 arm, from
-- an explicit upstream `status="resolved"`. A row with `resolve_pending_at IS NULL`
-- is untouchable by that path, which is the same shape as the reaper's own guard.
--
-- ⚠️ WHAT THIS FILE DOES NOT DO, AND WHY. It does NOT narrow
-- `notifications_suppmap_ck` to remove `flapping`. 00018 widened that CHECK to
-- EIGHT values, four of which are live, `notifications` rows have no reaper
-- (SPEC.md:1346) and 00018:71-77 establishes this repo's rule that an enum narrowing
-- with no downlevel mapping must FAIL rather than rewrite history. `flapping` is
-- retired AT THE WRITER instead -- `internal/notification/domain/suppression.go`
-- refuses to record it, and it stays decodable on read -- exactly as
-- `retiredEventTypes` retires `case.reopened` in `alerts/domain/event.go`. The
-- contraction is a separate file for whoever drops the last partition.
--
-- ⚠️ `alerts.flap_score` AND `alerts.is_flapping` ARE NOT MADE DEAD BY W AND ARE
-- NOT RETIRED HERE. They are the VISIBLE state -- "a VISIBLE UI state, never
-- silent suppression" (00007:50) -- that tells an operator to go fix a Prometheus
-- rule missing a `for:`, and the API filter and the rollup both read them. What W
-- does is make the flap score BLIND: `flap.score` is an EWMA over
-- `case.opened/.resolved/.expired/.suppressed/.unsuppressed`
-- (`alerts/repository/event.go` stateChangeCountsSQL), and a flap damped into one
-- case appends none of them. Feeding the deferred resolve into that score needs a
-- new `alert_events.type`, which is an API-contract change and deliberately not
-- bundled here.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is an EXPAND step, entirely additive: one
-- new table, two new NULLABLE columns, four CHECKs that every release-N row
-- satisfies vacuously (release N writes NULL into both columns because it does not
-- know they exist), one partial index, and no column dropped or narrowed. It is
-- safe to deploy under release N, and release N keeps closing cases immediately
-- because it never reads `case_policy_config`.
--
-- ⭐ THE DOWN IS LOSSLESS AND IT WORKS, BECAUSE IT SPENDS THE RECEIPT RATHER THAN
-- DISCARDING IT. Every case with a pending resolve is CLOSED as
-- `upstream`/`resolve_pending_end_at` -- which is exactly what W=0 would have done
-- at the instant the resolve arrived -- and only then are the columns dropped.
-- Dropping them first would forget that an upstream resolve had been received, and
-- the rolled-back release would eventually end those episodes through the reaper as
-- `expired`/`timeout`, i.e. oto claiming it stopped hearing about an alert whose
-- resolution it had in hand. Completing the close is the truthful direction and it
-- is the one this Down takes.

-- +goose Up

-- ------------------------------------------------------------ case_policy_config

CREATE TABLE case_policy_config (
  id                 UUID        PRIMARY KEY,
  org_id             UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,

  -- ADR 0038's axes. `namespace` uses '' for the absent-namespace partition; see
  -- the header for why a NULL would defeat the unique index.
  namespace          TEXT        NOT NULL DEFAULT '',
  alertname          TEXT        NOT NULL,

  retention_window_s INT         NOT NULL DEFAULT 0,

  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT case_policy_axes_uniq  UNIQUE (org_id, namespace, alertname),
  -- Mirrors alerts_name_ck. An alertname is mandatory because it is mandatory on
  -- every Alert (§C.2) and on every group key (ADR 0038): a row with no alertname
  -- would be an org-wide default, which this table deliberately does not offer --
  -- see the COMMENT ON TABLE.
  CONSTRAINT case_policy_name_ck    CHECK (length(alertname) BETWEEN 1 AND 1024),
  CONSTRAINT case_policy_ns_ck      CHECK (length(namespace) <= 1024),
  -- 0 is today's behaviour and the default. The ceiling is one day: a window
  -- longer than that keeps an episode open across a whole shift's worth of
  -- unrelated firings, which stops being noise reduction and starts being one
  -- case that means nothing.
  CONSTRAINT case_policy_window_ck  CHECK (retention_window_s BETWEEN 0 AND 86400)
);

-- +goose StatementBegin
COMMENT ON TABLE case_policy_config IS
  'Per (namespace, alertname) shaping of the CASE itself, keyed on ADR 0038 axes so an operator learns one set of dimensions and not two. Today it carries exactly one knob, the retention window W. There is deliberately no org-wide row and no wildcard: a default lives in code (0) where it cannot be half-configured, and an absent row IS the default. Coarser than group_key by one axis -- it does not key on cluster -- because splitting later is the safe direction (ADR 0038).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN case_policy_config.namespace IS
  'ADR 0038 axis. The empty string is the ABSENT-namespace partition: alerts.namespace is NULL for both absent and empty because Prometheus treats them as equivalent, so every reader looks this up as COALESCE(alerts.namespace, ''''). A NULL here would let one org hold two contradictory windows for one alertname, because two NULLs are not equal under a UNIQUE index.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN case_policy_config.retention_window_s IS
  'W, the CASE RETENTION WINDOW in seconds. A case whose alert resolved stays OPEN for W and closes only once the alert has stayed resolved for W, so a re-fire inside W lands in the still-open case instead of opening the next seq. It is a DELAYED CLOSE, never a reopen: the episode still closes exactly once (ADR 0040). 0 is the default and is byte-for-byte the pre-00057 behaviour -- the domain takes no deferral branch at all.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT case_policy_window_ck ON case_policy_config IS
  'W is 0..86400. 0 means the case closes on the resolve, which is what oto did before this table existed; one day is the ceiling because a longer window merges a shift of unrelated firings into one case that means nothing.';
-- +goose StatementEnd

CREATE INDEX case_policy_org_idx ON case_policy_config (org_id, alertname, namespace);

-- +goose StatementBegin
COMMENT ON INDEX case_policy_org_idx IS
  'The settings-page listing, ordered the way an operator reads it: by alertname, then by namespace. The lookup on the ingest path rides case_policy_axes_uniq instead, because it supplies all three columns.';
-- +goose StatementEnd

-- ------------------------------------------------- alert_cases: pending resolves

ALTER TABLE alert_cases ADD COLUMN resolve_pending_at     TIMESTAMPTZ;
ALTER TABLE alert_cases ADD COLUMN resolve_pending_end_at TIMESTAMPTZ;

-- Both or neither. A due time with no `ended_at` to stamp would close the episode
-- at the sweep's clock and charge W to the signal's firing duration; an `ended_at`
-- with no due time is a close that nothing will ever perform.
ALTER TABLE alert_cases ADD CONSTRAINT case_pending_pair_ck
  CHECK ((resolve_pending_at IS NULL) = (resolve_pending_end_at IS NULL));

-- A pending close belongs only to an episode that has not closed. Together with
-- `case_terminal_ended` this is what makes the delayed close single-shot: the
-- close clears these two columns in the same UPDATE that writes `ended_at`, so a
-- closed row can never carry a second one.
ALTER TABLE alert_cases ADD CONSTRAINT case_pending_open_ck
  CHECK (resolve_pending_at IS NULL OR state = 'open');

-- The same floor `case_order_ck` puts on `ended_at`, applied to the value that is
-- going to BECOME `ended_at`. §B.3.2 clamps it in Go; this is the belt-and-braces
-- half, and it means a clock-skewed resolve cannot arrive here and abort the close
-- later, when there is no observation left to attribute the skew to.
ALTER TABLE alert_cases ADD CONSTRAINT case_pending_order_ck
  CHECK (resolve_pending_end_at IS NULL OR resolve_pending_end_at >= started_at);

-- An upstream resolve is POSITIVE PROOF OF NON-SUPPRESSION -- Alertmanager would
-- not have delivered it otherwise, which is the same argument §B.3.1 uses to let
-- ingest drive T4 -- so the deferral clears `suppression_reason` exactly as an
-- immediate T5 does. This states that: nothing may say "silenced by <id>" about an
-- episode whose alert upstream has already called resolved.
ALTER TABLE alert_cases ADD CONSTRAINT case_pending_supp_ck
  CHECK (resolve_pending_at IS NULL OR suppression_reason IS NULL);

-- The sweep's whole scan. Partial, because the population is tiny -- only episodes
-- inside their retention window -- and 0 is the default W, so on most deployments
-- the index is empty and costs one entry per pending close on the rest.
CREATE INDEX case_close_due_idx ON alert_cases (org_id, resolve_pending_at)
  WHERE resolve_pending_at IS NOT NULL;

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_pending_at IS
  'OTO clock: when the DELAYED CLOSE becomes due, i.e. the last upstream resolve plus W (case_policy_config.retention_window_s). NULL means no close is pending, which is every row on a deployment that has set no W. It moves FORWARD on each fresh resolve inside the window, because the rule is "stayed resolved for W" and not "resolved W ago", and a re-fire clears it -- the alert is firing again and there is nothing left to close.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_pending_end_at IS
  'The ended_at the pending close will stamp: the UPSTREAM claim from the resolve observation, already clamped to >= started_at (SPEC B.3.2). It exists so W is never charged to the signal firing duration (R8) -- closing at the sweep clock instead would make every reader of ended_at report an episode W longer than the signal actually burned.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT case_pending_open_ck ON alert_cases IS
  'A pending close belongs to an OPEN episode only. With case_terminal_ended this is what keeps the delayed close single-shot: the close writes ended_at and clears both pending columns in one UPDATE, so a closed row cannot carry another. A Case is still strictly terminal (ADR 0040) -- W moves WHEN it closes, never how many times.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX case_close_due_idx IS
  'The case.reap sweeps due-close scan: open episodes whose retention window has elapsed. Empty whenever every W is 0, which is the default.';
-- +goose StatementEnd

-- +goose Down

-- ⭐ SPEND THE RECEIPT BEFORE DROPPING IT. Every pending close is COMPLETED here,
-- exactly as W=0 would have completed it at the instant the resolve arrived:
-- `upstream` because an explicit upstream resolve is what wrote these columns, and
-- `resolve_pending_end_at` because that is the upstream claim the close was always
-- going to stamp. Dropping the columns first would forget the resolve, and the
-- rolled-back release would end those episodes through the reaper as
-- `expired`/`timeout` -- oto claiming it stopped hearing about an alert whose
-- resolution it was holding. `state_version` is bumped because this is a state
-- write and every compare-and-set in flight must lose against it.
-- `suppression_reason` is deliberately not touched: `case_pending_supp_ck` has
-- already guaranteed it is NULL on every row this statement matches, so clearing it
-- would be a second spelling of an invariant the schema states once. `suppressed_by`
-- is left exactly as an immediate T5 leaves it -- the accessor masks a witness set
-- with no reason beside it, and 00007 makes the column NOT NULL, so there is no
-- NULL to write into it.
UPDATE alert_cases
   SET state          = 'closed',
       resolve_reason = 'upstream',
       ended_at       = resolve_pending_end_at,
       state_version  = state_version + 1,
       updated_at     = now()
 WHERE resolve_pending_at IS NOT NULL;

DROP INDEX IF EXISTS case_close_due_idx;

ALTER TABLE alert_cases DROP CONSTRAINT case_pending_supp_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_pending_order_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_pending_open_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_pending_pair_ck;

ALTER TABLE alert_cases DROP COLUMN resolve_pending_end_at;
ALTER TABLE alert_cases DROP COLUMN resolve_pending_at;

DROP INDEX IF EXISTS case_policy_org_idx;
DROP TABLE case_policy_config;
