-- The reconciler stops being switchable off per source.
--
-- ⭐⭐ WHY. ADR 0006 calls `source.reconcile` MANDATORY, and 00004 gave every row a
-- boolean that turned it off. The two could not both be right. This migration says
-- which, and it is the ADR — not because the ADR is older, but because an audit of
-- what the flag actually DOES found that it does not do the thing its 2026-08-08
-- amendment defended it for.
--
-- ⛔ WHAT THE FLAG ACTUALLY DID. `source_health.status` has exactly two writers:
-- `Probe` (the manual `POST /sources/{id}/test`) and the reconcile pass itself.
-- `TouchPush` deliberately does NOT move it — a webhook arriving proves the source
-- can reach oto and proves nothing about oto reaching the source. So with
-- reconciliation off, the health projection FREEZES at whatever it last said, and
-- `source_health.status = 'healthy'` — the §B.4 guard the reaper consults before
-- ending an episode — keeps answering "yes, oto can see this source" forever, on
-- the strength of one successful probe that may be weeks old. Meanwhile
-- Alertmanager's MuteStage has dropped every silenced alert before the webhook
-- fired, `source_ends_at` stops advancing, `resolve_grace` elapses, and
-- `occurrence.reap` ends the episode as `expired` / `resolve_reason='timeout'` —
-- an ending oto records for an alert that never ended. One PATCH, no warning, and
-- the history stops being a record.
--
-- ⛔ AND WHAT IT DID NOT DO. The amendment kept the flag for the deployment whose
-- Alertmanager API is unreachable outbound while its webhook still posts, arguing
-- that without the flag such a source would be marked `unreachable` and freeze the
-- reaper forever. That is true — and it is EQUALLY true with the flag. A source oto
-- cannot reach never earns a `healthy` row in the first place: 00004's health row
-- is seeded `unknown`, only a successful probe moves it, and a firewalled source
-- has no successful probe to give. `unknown` blocks the reaper exactly as
-- `unreachable` does. So the flag bought that deployment nothing except silence
-- about why, while buying every REACHABLE source a way to make the reaper trust a
-- stale verdict. The knob that genuinely serves a slow, rate-limited or distant
-- Alertmanager is `reconcile_interval_s`, which survives untouched and spans
-- 10 s to 1 h.
--
-- CONTRACTED, not widened (CONTEXT.md §6). A release-N writer sets this column;
-- dropping it would break that writer mid-deploy, so this must ship AFTER the
-- release that stopped writing it — the DTOs, the mapper, the INSERT and the
-- UPDATE in `internal/sources` no longer name the column in the same change.
-- Nothing reads it: `ListDue`'s gate is gone with it, so the fan-out now schedules
-- every live source, which is what ADR 0006 said all along.
--
-- DATA LOSS is deliberate and total: the only information in the column is which
-- sources were being lied about, and that fact is preserved better by the sources
-- themselves starting to reconcile. The Down restores the column with its old
-- default, so every row comes back as `true` — the value the system is now
-- asserting is the only correct one.

-- +goose Up

ALTER TABLE alert_sources DROP COLUMN reconcile_enabled;

-- +goose Down

ALTER TABLE alert_sources
  ADD COLUMN reconcile_enabled BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN alert_sources.reconcile_enabled IS 'Whether source.reconcile polls /api/v2/alerts. The reconciler is the ONLY producer of state=suppressed (SPEC §G.8).';
