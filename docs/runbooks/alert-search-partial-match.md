# Alert search — optional partial/substring matching on alert names

This is not a metric page like the rest of this directory — it is a one-time, **operator-run, fully
optional** SQL snippet. oto never runs it itself and never requires it.

## What this closes

`?q=` free-text alert search always runs a full-text query (`websearch_to_tsquery`) over `alertname`,
`annotations->>'summary'` and `annotations->>'description'`, backed by the `alerts_text_idx` GIN index
(`db/migrations/00007_alerts.sql`). That gets you negation (`-word`), phrase search (`"exact phrase"`)
and OR (`word1 OR word2`) for free, but it cannot do one specific thing: real alert names are often
PascalCase compound identifiers with no word boundaries — `CheckoutErrorRateHigh` is an actual value
from a test fixture — and Postgres's text-search parser tokenizes that as **one lexeme**. No tsquery
variant can ever substring-match "error" inside it; tsquery matches whole lexemes, never fragments of
one.

[`pg_trgm`](https://www.postgresql.org/docs/current/pgtrgm.html) (trigram similarity, which is what
accelerates `ILIKE '%...%'`) is the tool for exactly that gap. Enabling it lets alert search also match
`alertname ILIKE '%error%'` against `CheckoutErrorRateHigh`.

## Why oto does not do this for you

`internal/platform/migrate` runs oto's own migrations, all-or-nothing: any one step failing fails the
whole `oto migrate` run. Attempting `CREATE EXTENSION pg_trgm` there would hard-fail startup on any
managed Postgres that does not permit extensions — the exact failure mode
[ADR 0014](../adr/0014-postgres-only-no-analytical-store.md) rejected TimescaleDB over. `pg_trgm` is
far more widely available than TimescaleDB (it ships in Postgres's own `contrib`), but the reasoning
still holds: a migration oto controls must never bet the boot sequence on a privilege the operator may
not have granted.

Instead, oto detects the capability with a single privilege-free query —
`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`, readable by any connected role,
no elevated grant needed (`internal/platform/db/capabilities.go`, `TrigramAvailable`). It runs once at
process startup and is cached for the process lifetime. The result is threaded into
`alerts/repository.AlertRepository` (which branches `?q=` on it) and surfaced on `GET /api/v1/me` as
`search.partial_match_enabled`, so the UI can tell whether to advertise substring search at all.

## Enabling it

Run this once, whenever is convenient — it needs no maintenance window:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX CONCURRENTLY IF NOT EXISTS alerts_alertname_trgm_idx ON alerts USING GIN (alertname gin_trgm_ops);
```

- Safe to run on any Postgres that permits extensions at all — a fresh install, an existing one, a
  managed service that allows `pg_trgm` (most do; it is a standard `contrib` module, unlike
  TimescaleDB).
- oto picks it up automatically at the **next process restart** — the check runs once at boot, not on
  a timer.
- If you enable the extension but skip the index (or `CREATE INDEX CONCURRENTLY` is still building),
  search still works correctly — the `ILIKE '%...%'` clause just runs as an unindexed sequential scan
  on `alertname` instead of an index scan, which is slower but not wrong.
- Disabling it later (`DROP EXTENSION pg_trgm`) is equally safe: the next restart's detector reports
  `false` again and search falls back to the tsquery-only clause, exactly as it behaved before you
  opted in.
