package db

import "context"

// TrigramAvailable reports whether the `pg_trgm` extension is enabled on the
// connected Postgres.
//
// ⭐ THIS IS A DETECTOR, NEVER AN INSTALLER. oto's own migrations
// (`internal/platform/migrate`) must never attempt `CREATE EXTENSION pg_trgm`
// or build a `gin_trgm_ops` index — either would hard-fail startup on any
// managed Postgres that does not permit extensions, which is exactly the
// failure mode ADR 0014 rejected TimescaleDB over. `pg_trgm` is far more
// widely permitted than TimescaleDB, but the same reasoning still applies: a
// migration oto controls must never bet the boot sequence on a privilege the
// operator may not have granted.
//
// The query it runs is privilege-free by construction: `pg_extension` is
// readable by any connected role, so this needs no elevated grant and can
// never itself be the thing that fails. See docs/runbooks for the one-time,
// operator-run SQL that turns this on.
//
// Call it ONCE, at process startup, and cache the bool for the process
// lifetime (see internal/app's Container). It answers a fact about the
// deployment, not about any one query, and re-checking it on every alert
// search would spend a round trip re-discovering something that cannot change
// without a restart.
func TrigramAvailable(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`,
	).Scan(&ok)
	if err != nil {
		return false, err
	}
	return ok, nil
}
