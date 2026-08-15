-- source_health learns WHICH ROUTES REACH OTO, not just what the root route says.
--
-- ⭐⭐ WHY, AND WHY 00028 WAS NOT ENOUGH. 00028 added the three Alertmanager
-- timings every oto knob is derived from, read off the TOP-LEVEL route, plus a
-- count of how many descendant routes state a timing of their own. That count was
-- honest about the limitation and useless for fixing it: it told an operator "the
-- numbers above may not govern your alerts" without ever saying which numbers do.
-- On any Alertmanager that overrides `group_interval` on the route oto's receiver
-- hangs off — which is most of them — every tuning verdict oto gave was arithmetic
-- about a cluster that was not the operator's, delivered with a caveat instead of
-- a correction.
--
-- 00028's own comment gave the reason to stop: resolving the timings for a
-- PARTICULAR ALERT would mean re-implementing Alertmanager's matcher tree against
-- that alert's labels, and being wrong invisibly. That reason still stands and
-- nothing here does it. What this column stores is a STRUCTURAL resolution, true
-- for every alert and needing no label set:
--
--   * inheritance -- `receiver`, `group_by` and the three timings flow down from
--     the nearest ancestor that states them, exactly as `dispatch.NewRoute`
--     copies its parent's options and overrides only what the child states;
--   * delivery -- `dispatch.Route.Match` answers with a node only when NO child
--     matched, so a route with a matcher-less child never delivers itself;
--   * `continue: true` -- evaluation carries on to later siblings, which is why
--     SEVERAL routes can deliver to one receiver WITH DIFFERENT TIMINGS. The
--     answer for a receiver is a SET, and this column stores a set.
--
-- ⛔ THE ANSWER IS A SET AND MUST NOT BE FLATTENED INTO THREE MORE COLUMNS. Two
-- routes can reach oto's receiver under different matchers with different
-- `group_interval`s; there is no single number, and inventing one -- first match,
-- slowest, an average -- would repeat the exact failure of the hand-typed form
-- this whole feature replaced. The reader is told "these two routes reach oto and
-- disagree". That is a list, so it is jsonb.
--
-- ⛔ NULL MEANS NOT OBSERVED, and it is read against `am_route_timings_at` in
-- 00028, not against `updated_at`. Same rule, same reason: `updated_at` moves on
-- every probe including failed ones, and a stale route list shown as fresh would
-- be a lie about the shape of somebody's cluster.
--
-- ⚠️ OTO CANNOT READ ITS OWN WEBHOOK URL, so `receiver_basis` inside the document
-- is load-bearing. The ingest path is `/api/v1/ingest/alertmanager/{source_id}`,
-- so an operator's webhook URL literally contains the id of the source oto is
-- probing and would identify oto's receiver exactly -- but `webhook_config.url` is
-- a `SecretURL` and `config.original` is the MARSHALLED config, so it arrives as
-- the literal string `<secret>`. Verified, not assumed: the checked-in
-- `internal/sources/client/alertmanager/testdata/compose_v0.28.1.yaml` is a real
-- capture and reads `url: <secret>`. So identification is an inference -- exactly
-- one webhook receiver means that is oto's; several means ambiguous and every
-- candidate is shown -- and the basis travels with the answer so a screen can
-- never render an inference as a reading.
--
-- SHAPE, kept deliberately loose. The document is
--   {"receiver": "oto"|null, "basis": "sole_webhook"|"ambiguous"|"no_webhook",
--    "webhook_receivers": ["..."], "dropped": 0,
--    "routes": [{"receiver": "...", "path": [{"matchers": ["..."],
--                "deprecated": false, "continue": false}],
--                "group_wait_ms": 30000|null, "group_wait_from": 0|null, ...,
--                "group_by": ["..."], "group_by_all": false,
--                "shadowed": false}]}
-- and it is a PROJECTION of somebody else's config file, re-derived in full on
-- every successful probe. There is no CHECK on its interior for that reason: a
-- constraint here would fail a reconcile pass over a display detail, and the
-- shape is owned by the repository mapper that writes it, which is the only
-- reader. `routes` is capped at 64 by the parser with `dropped` counting the rest,
-- so the column cannot grow without bound.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One NULLable jsonb column on a projection
-- table is a pure widening: nothing to backfill, no constraint a release-N writer
-- can violate, and a release-N writer simply leaves it NULL, which reads as "not
-- yet observed" -- the correct answer for a source nobody has probed since the
-- upgrade. The Down drops it and loses only an observation the next reconcile
-- pass re-derives within its interval.

-- +goose Up

ALTER TABLE source_health
  ADD COLUMN am_routes JSONB;

COMMENT ON COLUMN source_health.am_routes IS
  'The source''s route tree, resolved: every DELIVERING route with its inherited receiver, its inherited group_wait/group_interval/repeat_interval, the depth each of those was stated at, its matcher path from the root, its group_by, and whether an earlier matcher-less sibling makes it unreachable. Plus which receiver oto believes is its own and the BASIS for that belief — Alertmanager redacts webhook_config.url as `<secret>`, so identification is an inference (sole webhook receiver) and is labelled as one. NULL = NOT OBSERVED; freshness is am_route_timings_at, never updated_at. The answer for a receiver is a SET, because `continue: true` lets several routes deliver to one receiver with different timings, and it must never be flattened to one triple.';

-- +goose Down

ALTER TABLE source_health DROP COLUMN am_routes;
