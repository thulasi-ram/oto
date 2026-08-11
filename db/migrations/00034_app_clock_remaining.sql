-- The rest of the schema gives up its own clock. Twenty timestamp columns across
-- thirteen tables lose `DEFAULT now()`; six tables KEEP theirs, deliberately, and
-- the last section of this comment says which and why.
--
-- 00032 (`channels`) and 00033 (`orgs`, `channel_credentials`) each made the same
-- argument about one table it had just been bitten by. This migration finishes
-- the sweep, so that the rule is the schema's rather than three tables' — but it
-- finishes it by ENUMERATION, not by pattern: every column below was checked
-- against every writer in `internal/*/repository` first, and the ones whose
-- writers actually rely on the default are still defaulted.
--
-- ⭐⭐ WHY. CONTEXT.md §6 says the application owns time: `internal/platform/clock`
-- exists so that time is injectable, and every repository listed below already
-- stamps these columns from it. A `DEFAULT now()` on a column the application
-- always supplies is not a safety net, it is a TRAP WITH A DELAYED FUSE. It does
-- nothing for the paths that exist. What it does is let a FUTURE writer omit the
-- column, succeed, and plant a row whose timestamp came from the DATABASE's clock
-- while the rest of the row came from the app's. That row does not fail at the
-- INSERT, where the mistake is. On the six tables below that carry a
-- `<table>_time_ck` (`updated_at >= created_at`) it fails much later, on somebody
-- else's UPDATE, as a 500 blaming a CHECK constraint two files away — which is
-- exactly how `channels` cost half a day in 00032. Without the default the same
-- omission fails at the offending statement, as a 23502 not-null violation, in
-- the first test that runs it.
--
-- ⚠️ THE FUSE IS SHORTER ON SOME OF THESE THAN OTHERS, and that is worth saying
-- rather than implying. `clusters`, `alert_sources`, `notification_policies`,
-- `notifications`, `notification_deliveries` and `channel_threads` all carry a
-- CHECK across the pair, so a mixed-clock row on them is a live 500 waiting for
-- an UPDATE. `users` carries `users_time_ck` too, but it has exactly one writer
-- (`internal/app/bootstrap.go`) and no UPDATE at all. `api_tokens.created_at`,
-- `sessions.created_at`, `slack_identities.created_at`, `source_health.updated_at`,
-- `ingest_dedup.seen_at` and `silences.mirrored_at` are single columns in no
-- CHECK: the worst a mixed clock does there is make a timestamp wrong — a
-- retention horizon, a staleness indicator, a "last used" — rather than raise a
-- constraint. They are included because a column that is wrong and never
-- complains is not obviously the better failure, and because leaving three of the
-- twenty defaulted would leave a reader guessing which rule applies where.
--
-- ⛔ SIX TABLES KEEP THEIR DEFAULTS, AND MUST. On each of these a live writer
-- omits the column today, so dropping the default would break a production path
-- immediately rather than protect a future one:
--
--   * `alerts` and `alert_occurrences` and `alert_groups` take BOTH created_at
--     and updated_at from the database: their INSERTs never name them and their
--     UPDATEs write `now()`. They are internally CONSISTENT — one clock, the
--     database's — which is the property that matters; they are simply the
--     opposite convention to this file. becd24b already warned about this.
--     Moving them is a separate change that has to move the INSERTs and the
--     UPDATEs together, and it is not this one.
--   * `alert_event_keys.created_at` has TWO writers that disagree.
--     `notification/repository/events.go` names it; `alerts/repository/event.go`
--     omits it in a set-returning batch insert. The column is in no CHECK and is
--     read only by the pruner, so the disagreement is currently harmless — but a
--     DROP DEFAULT here would 23502 the alerts module's event append, which is on
--     the ingest path, where §B's rule is that nothing transient may fail.
--   * `ui_events.at` is the PARTITION KEY and `streaming/repository/events.go`
--     deliberately omits it, letting the database place the row in the partition
--     that matches the database's own clock. An app-stamped partition key from a
--     pod running ahead of Postgres would route rows into a partition that may
--     not exist yet.
--   * `alert_snoozes.created_at` is omitted by `alerts/repository/snooze.go`,
--     which names `snoozed_at` from the injected clock instead. The row's
--     meaningful instants are app-owned; `created_at` is bookkeeping the writer
--     never mentions.
--
-- Test seeds that relied on the dropped defaults are fixed in the same change to
-- stamp the harness clock (`test/harness/builders.go` and five test files). A
-- seed that needs the database's clock is a seed writing rows the product cannot
-- write.
--
-- EXPAND/CONTRACT (CONTEXT.md §6), with N and N+1 running simultaneously.
-- Dropping a DEFAULT is not destructive: no data changes, the columns stay NOT
-- NULL, and every reader is untouched. It is safe in BOTH directions because
-- release N already supplies every one of these values on every INSERT it makes,
-- so an N pod talking to the N+1 schema inserts exactly as it did before. There
-- is nothing to backfill and no contract phase to follow. The Down restores the
-- defaults, which is what keeps a rolled-back release able to write these tables
-- at all.

-- +goose Up

-- identity (00003). Written by internal/app/bootstrap.go and
-- internal/identity/repository/{tokens,sessions,slack_identities}.go.
ALTER TABLE users            ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE users            ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE api_tokens       ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE sessions         ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE slack_identities ALTER COLUMN created_at DROP DEFAULT;

-- sources (00004). Written by internal/sources/repository/{clusters,sources,health}.go.
ALTER TABLE clusters      ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE clusters      ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE alert_sources ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE alert_sources ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE source_health ALTER COLUMN updated_at DROP DEFAULT;

-- ingestion (00006). Written by internal/ingestion/repository/dedup.go.
ALTER TABLE ingest_dedup ALTER COLUMN seen_at DROP DEFAULT;

-- notification (00011). Written by internal/notification/repository/
-- {config,notifications,deliveries,threads}.go.
ALTER TABLE notification_policies   ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE notification_policies   ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE notifications           ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE notifications           ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE notification_deliveries ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE notification_deliveries ALTER COLUMN updated_at DROP DEFAULT;
ALTER TABLE channel_threads         ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE channel_threads         ALTER COLUMN updated_at DROP DEFAULT;

-- silences (00012). Written by internal/silences/repository/sync.go.
ALTER TABLE silences ALTER COLUMN mirrored_at DROP DEFAULT;

COMMENT ON COLUMN users.created_at IS
  'When the application says this member was created. NO DEFAULT, ON PURPOSE: internal/app.Bootstrap stamps it from the injected Go clock, never the database, so that it and updated_at are ordered by ONE clock and users_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN users.updated_at IS
  'When the application last changed this row. Stamped from the same injected Go clock as created_at. There is no UPDATE statement on this table yet; the one that is written next must advance it monotonically, GREATEST(updated_at, $n), or a pod lagging the one that bootstrapped the deployment will trip users_time_ck.';
COMMENT ON COLUMN api_tokens.created_at IS
  'When the application minted this token. NO DEFAULT, ON PURPOSE: APITokenRepository.Insert stamps it from the injected Go clock, so it is ordered by the same clock as expires_at and last_used_at rather than by whichever machine happened to write it.';
COMMENT ON COLUMN sessions.created_at IS
  'When the application opened this session. NO DEFAULT, ON PURPOSE: SessionRepository.Insert stamps it from the injected Go clock, the same one that computes expires_at — a created_at from the database clock would make the session''s age and its expiry disagree.';
COMMENT ON COLUMN slack_identities.created_at IS
  'When the application first saw this Slack member. NO DEFAULT, ON PURPOSE: SlackIdentityRepository.Upsert stamps it from the injected Go clock, the same one linked_at comes from.';
COMMENT ON COLUMN clusters.created_at IS
  'When the application says this cluster was registered. NO DEFAULT, ON PURPOSE: ClusterRepository.Create stamps it from the injected Go clock, never the database, so that it and updated_at are ordered by ONE clock and clusters_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN clusters.updated_at IS
  'When the application last changed this row. Stamped from the same injected Go clock as created_at, and advanced monotonically by UpdateDisplayName (GREATEST(updated_at, $n)) so that a pod whose clock lags the pod that registered the cluster cannot push it backwards past created_at.';
COMMENT ON COLUMN alert_sources.created_at IS
  'When the application says this upstream was registered. NO DEFAULT, ON PURPOSE: SourceRepository.Create stamps it from the injected Go clock, never the database, so that it and updated_at are ordered by ONE clock and alert_sources_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN alert_sources.updated_at IS
  'When the application last changed this row. Stamped from the same injected Go clock as created_at, and advanced monotonically by Update and SoftDelete (GREATEST(updated_at, $n)) so that a pod whose clock lags the pod that registered the source cannot push it backwards past created_at.';
COMMENT ON COLUMN source_health.updated_at IS
  'When the application last refreshed this liveness projection. NO DEFAULT, ON PURPOSE: both upserts in sources/repository/health.go name it from the injected Go clock, which is also the clock the reaper guard and the staleness display reason about.';
COMMENT ON COLUMN ingest_dedup.seen_at IS
  'Prune horizon. 10 minutes comfortably exceeds n_peers x cluster.peer-timeout (45s for a 3-node cluster). NO DEFAULT, ON PURPOSE: the ingest path stamps it from the injected Go clock, which is the clock the pruner''s own cutoff is computed from — a row written on the database clock would be pruned early or late by exactly the skew between them.';
COMMENT ON COLUMN notification_policies.created_at IS
  'When the application says this routing rule was created. NO DEFAULT, ON PURPOSE: ConfigRepository.CreatePolicy stamps it from the injected Go clock, never the database, so that it and updated_at are ordered by ONE clock and policies_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN notification_policies.updated_at IS
  'When the application last changed this rule. Stamped from the same injected Go clock as created_at, and advanced monotonically by UpdatePolicy and SoftDeletePolicy (GREATEST(updated_at, $n)) so that a pod whose clock lags the pod that created the policy cannot push it backwards past created_at.';
COMMENT ON COLUMN notifications.created_at IS
  'When the application recorded this notification INTENT. NO DEFAULT, ON PURPOSE: NotificationRepository.Insert names it from the caller''s clock, never the database, so that it and updated_at are ordered by ONE clock and notifications_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN notifications.updated_at IS
  'When the application last folded this notification''s aggregate status. Advanced monotonically by SetStatus (GREATEST(updated_at, $n)): the fold is done by a dispatch worker, never by the pod that recorded the intent, and a lagging worker must not trip notifications_time_ck on a message that has already been sent.';
COMMENT ON COLUMN notification_deliveries.created_at IS
  'When the application fanned this delivery out. NO DEFAULT, ON PURPOSE: DeliveryRepository.Create names it from the caller''s clock, never the database, so that it and updated_at are ordered by ONE clock and deliveries_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN notification_deliveries.updated_at IS
  'When a worker last touched this delivery, and the CLAIM LEASE the dispatcher reads. Advanced monotonically by every writer (GREATEST(updated_at, $n)) for two reasons: a lagging pod must not trip deliveries_time_ck on the write that follows a send, and it must not stamp its own fresh claim as already expired, which would let a second worker reclaim a delivery mid-flight and send it twice.';
COMMENT ON COLUMN channel_threads.created_at IS
  'When the application opened this thread. NO DEFAULT, ON PURPOSE: ThreadRepository.Ensure names it from the caller''s clock, never the database, so that it and updated_at are ordered by ONE clock and threads_time_ck cannot be tripped by clock skew between an app server and Postgres.';
COMMENT ON COLUMN channel_threads.updated_at IS
  'When the application last changed oto''s memory of this destination. Advanced monotonically by every writer including OrderingStore.Advance (GREATEST(updated_at, $n)): a thread is opened by whichever pod evaluated the policy and then written by whichever pod the dispatcher ran on, and a lagging one must not fail threads_time_ck on the statement that records where a message landed.';
COMMENT ON COLUMN silences.mirrored_at IS
  'oto clock at the last successful sync of this row. Staleness indicator for the UI. NO DEFAULT, ON PURPOSE: silences/repository/sync.go names it from the injected Go clock, so "how stale is this mirror" is answered by one clock rather than by the difference between two.';

-- +goose Down

ALTER TABLE users            ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE users            ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE api_tokens       ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE sessions         ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE slack_identities ALTER COLUMN created_at SET DEFAULT now();

ALTER TABLE clusters      ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE clusters      ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE alert_sources ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE alert_sources ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE source_health ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE ingest_dedup ALTER COLUMN seen_at SET DEFAULT now();

ALTER TABLE notification_policies   ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE notification_policies   ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE notifications           ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE notifications           ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE notification_deliveries ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE notification_deliveries ALTER COLUMN updated_at SET DEFAULT now();
ALTER TABLE channel_threads         ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE channel_threads         ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE silences ALTER COLUMN mirrored_at SET DEFAULT now();

-- Eighteen of the twenty columns carried no comment before this migration; two
-- did — `ingest_dedup.seen_at` from 00006 and `silences.mirrored_at` from 00012.
-- The Down puts each back exactly as it was, so a rolled-back schema does not
-- keep documenting a rule it no longer enforces.
COMMENT ON COLUMN users.created_at IS NULL;
COMMENT ON COLUMN users.updated_at IS NULL;
COMMENT ON COLUMN api_tokens.created_at IS NULL;
COMMENT ON COLUMN sessions.created_at IS NULL;
COMMENT ON COLUMN slack_identities.created_at IS NULL;
COMMENT ON COLUMN clusters.created_at IS NULL;
COMMENT ON COLUMN clusters.updated_at IS NULL;
COMMENT ON COLUMN alert_sources.created_at IS NULL;
COMMENT ON COLUMN alert_sources.updated_at IS NULL;
COMMENT ON COLUMN source_health.updated_at IS NULL;
COMMENT ON COLUMN notification_policies.created_at IS NULL;
COMMENT ON COLUMN notification_policies.updated_at IS NULL;
COMMENT ON COLUMN notifications.created_at IS NULL;
COMMENT ON COLUMN notifications.updated_at IS NULL;
COMMENT ON COLUMN notification_deliveries.created_at IS NULL;
COMMENT ON COLUMN notification_deliveries.updated_at IS NULL;
COMMENT ON COLUMN channel_threads.created_at IS NULL;
COMMENT ON COLUMN channel_threads.updated_at IS NULL;

COMMENT ON COLUMN ingest_dedup.seen_at IS
  'Prune horizon. 10 minutes comfortably exceeds n_peers x cluster.peer-timeout (45s for a 3-node cluster).';
COMMENT ON COLUMN silences.mirrored_at IS
  'oto clock at the last successful sync of this row. Staleness indicator for the UI.';
