-- source_health learns the three Alertmanager numbers every oto knob depends on.
--
-- ⭐⭐ WHY. docs/setup/tuning.md derives every tuning knob oto has from
-- `group_wait`, `group_interval` and `repeat_interval`: a `refire_grace` below
-- `group_interval` is unreachable and every re-fire opens a new Slack thread; a
-- `storm_window` below `group_wait` cannot see a burst; a `flap_threshold` above
-- the observable ceiling is dead code that looks correctly configured. Until now
-- the tuning screen ASKED an operator to type these three in and kept the answer
-- in one browser's localStorage. That is unshared (the person beside you sees
-- nothing), unvalidated (nothing checked it against the source), and silently
-- wrong the moment somebody edited alertmanager.yml -- so two operators could
-- open the same page and be given contradictory guidance about the same cluster.
--
-- oto does not need to ask. GET /api/v2/status returns `config.original`, oto
-- already parses that YAML (it is where send_resolved comes from), and the route
-- block is in it. These columns are that reading, per source, refreshed on every
-- reconcile.
--
-- ⛔ NULL MEANS UNKNOWN AND MUST NEVER BE BACKFILLED WITH ALERTMANAGER'S
-- DOCUMENTED DEFAULTS. Alertmanager marshals all three as omitempty pointers, so
-- a stock alertmanager.yml -- which sets none of them -- reports none of them,
-- and the 30s/5m/4h defaults are applied later, in dispatch.NewRoute, where the
-- status endpoint cannot see them. There is no DEFAULT on these columns for
-- exactly that reason. The whole value of this data is telling an operator when
-- one of their knobs can never fire, and a confident wrong number destroys that
-- while an honest gap does not.
--
-- ⚠️ THE THREE ARE PER-ROUTE AND INHERITED, so the values governing a PARTICULAR
-- alert are the ones on the route that matched it. oto records the TOP-LEVEL
-- route -- what governs everything matching no more specific route, and exactly
-- what the tuning guide tells an operator to read -- and records beside it how
-- many descendant routes state a timing of their own, so a reader is TOLD when
-- the top-level number is not the whole story. Resolving per alert would mean
-- re-implementing Alertmanager's matcher tree, including `continue: true` and
-- regex matchers, and being wrong invisibly.
--
-- MILLISECONDS, not seconds: `group_wait: 500ms` is legal and a seconds column
-- would silently round it to zero, which reads as "notify immediately".
--
-- ⚠️ A SEPARATE OBSERVED-AT FROM updated_at. `updated_at` moves on every probe,
-- including ones that could not reach the source at all; this one moves only when
-- the numbers beside it were genuinely read. A screen showing `updated_at` beside
-- a stale set of timings would be claiming they are fresh.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Five NULLable columns on a projection table is
-- a pure widening: nothing to backfill, no constraint a release-N writer can
-- violate, and a release-N writer simply leaves them NULL, which reads as
-- "not yet observed" -- the correct answer for a source nobody has probed since
-- the upgrade. The Down drops them and loses only an observation that the next
-- reconcile pass re-derives within its interval.

-- +goose Up

ALTER TABLE source_health
  ADD COLUMN am_group_wait_ms      BIGINT,
  ADD COLUMN am_group_interval_ms  BIGINT,
  ADD COLUMN am_repeat_interval_ms BIGINT,
  ADD COLUMN am_child_routes       INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN am_child_routes_with_timings INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN am_route_timings_at   TIMESTAMPTZ;

ALTER TABLE source_health
  ADD CONSTRAINT source_health_am_timings_ck CHECK (
    (am_group_wait_ms      IS NULL OR am_group_wait_ms      >= 0) AND
    (am_group_interval_ms  IS NULL OR am_group_interval_ms  >= 0) AND
    (am_repeat_interval_ms IS NULL OR am_repeat_interval_ms >= 0) AND
    am_child_routes >= 0 AND
    am_child_routes_with_timings >= 0 AND
    am_child_routes_with_timings <= am_child_routes
  );

COMMENT ON COLUMN source_health.am_group_wait_ms IS
  'The TOP-LEVEL route group_wait this Alertmanager reports in config.original, in milliseconds. NULL = NOT OBSERVED, and it must never be backfilled with Alertmanager''s documented 30s default: Alertmanager omits an unset value, applies the default later in dispatch.NewRoute, and the status endpoint cannot see it. Read, never typed in by a human (docs/setup/tuning.md).';
COMMENT ON COLUMN source_health.am_group_interval_ms IS
  'The TOP-LEVEL route group_interval, in milliseconds. It is the clock rate of oto''s whole view of the world: oto cannot learn of a change to an existing group faster than this, so every oto duration is a multiple of it. NULL = NOT OBSERVED, never the documented 5m default.';
COMMENT ON COLUMN source_health.am_repeat_interval_ms IS
  'The TOP-LEVEL route repeat_interval, in milliseconds. It is what produces notification_reason "repeat interval elapsed", which oto maps to an update-only delivery. NULL = NOT OBSERVED, never the documented 4h default.';
COMMENT ON COLUMN source_health.am_child_routes IS
  'How many descendant routes sit below the top-level route, at any depth. Context for the column beside it: "2 of 14" and "2 of 2" are very different pictures of how much of the traffic the top-level timings actually govern.';
COMMENT ON COLUMN source_health.am_child_routes_with_timings IS
  'How many descendant routes state a group_wait, group_interval or repeat_interval of their own. The three columns above are the TOP-LEVEL route''s, which govern every alert matching no more specific route; a non-zero count here says out loud that some alerts are batched differently, because resolving the per-alert value would mean re-implementing Alertmanager''s matcher tree.';
COMMENT ON COLUMN source_health.am_route_timings_at IS
  'When the three timings beside it were last read off this source. Separate from updated_at, which moves on every probe including failed ones: a stale reading shown against updated_at would be claiming to be fresh. NULL until the first successful config parse.';

-- +goose Down

ALTER TABLE source_health DROP CONSTRAINT IF EXISTS source_health_am_timings_ck;

ALTER TABLE source_health
  DROP COLUMN am_group_wait_ms,
  DROP COLUMN am_group_interval_ms,
  DROP COLUMN am_repeat_interval_ms,
  DROP COLUMN am_child_routes,
  DROP COLUMN am_child_routes_with_timings,
  DROP COLUMN am_route_timings_at;
