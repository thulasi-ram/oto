package app

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"time"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/migrate"
)

// The bodies of the two unversioned ops probes and of GET /api/v1/version.
//
// They are typed structs rather than `map[string]any` on purpose: these three
// shapes are in the published contract (HealthDTO, ReadyDTO, VersionDTO), a
// contract test asserts the running server matches it, and a map is a shape
// nothing can check.

// readyPingTimeout bounds the database check. A readiness probe that blocks is a
// pod that never comes back, so this is deliberately shorter than any sane probe
// interval.
const readyPingTimeout = 2 * time.Second

// versionDTO is the contract's VersionDTO: which build is this, and which schema
// does it expect.
type versionDTO struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// BuiltAt is omitted rather than sent as an empty string, because the contract
	// types it as a Timestamp and "" is not one.
	BuiltAt       string `json:"built_at,omitempty"`
	GoVersion     string `json:"go_version"`
	SchemaVersion string `json:"schema_version"`
}

// readyDTO is the contract's ReadyDTO.
//
// `status` is `ready`/`not_ready` and NOT `ok`. That is not pedantry: `ok` is
// what /healthz says, the two probes mean different things, and an operator
// grepping a probe body for the wrong word gets a false answer during a
// database outage.
type readyDTO struct {
	Status        string  `json:"status"`
	Database      string  `json:"database"`
	Migrations    string  `json:"migrations"`
	SchemaVersion *string `json:"schema_version"`
	Detail        *string `json:"detail,omitempty"`
	// Pools is oto's own operational detail, not part of ReadyDTO. The contract
	// does not forbid it and it is the only place pool saturation is visible
	// without scraping /metrics.
	Pools *db.Stats `json:"pools,omitempty"`
}

const (
	readyStatusReady    = "ready"
	readyStatusNotReady = "not_ready"

	databaseOK          = "ok"
	databaseUnreachable = "unreachable"

	migrationsApplied = "applied"
	migrationsPending = "pending"
	migrationsUnknown = "unknown"
)

// versionDTO answers "which build is this deployment running".
func (c *Container) versionDTO() versionDTO {
	out := versionDTO{
		Version:   c.Config.Version,
		Commit:    c.Config.Commit,
		BuiltAt:   c.Config.BuildDate,
		GoVersion: runtime.Version(),
	}
	if out.Commit == "" {
		out.Commit = "unknown"
	}
	// The schema this BINARY expects. It is the honest answer to "which version
	// is this code written against", and it needs no database — which matters,
	// because /version is the endpoint you call when nothing else works.
	if latest, err := migrate.Latest(); err == nil {
		out.SchemaVersion = migrate.FormatVersion(latest)
	} else {
		out.SchemaVersion = "unknown"
	}
	return out
}

// readiness answers "may this pod take traffic", and says WHY when the answer is
// no.
//
// Two dependencies are reported, not one. A database that answers but is two
// migrations behind the binary is the shape of a half-finished rolling deploy,
// and a pod in that state serving queries against columns that do not exist yet
// is how a deploy becomes an incident.
func (c *Container) readiness(ctx context.Context) (readyDTO, int) {
	out := readyDTO{
		Status:     readyStatusNotReady,
		Database:   databaseUnreachable,
		Migrations: migrationsUnknown,
	}

	if c.Pools == nil {
		out.Detail = strptr("the database pools are not initialised")
		return out, http.StatusServiceUnavailable
	}
	if err := c.Pools.Ping(ctx, readyPingTimeout); err != nil {
		out.Detail = strptr(err.Error())
		return out, http.StatusServiceUnavailable
	}

	out.Database = databaseOK
	stats := c.Pools.Stats()
	out.Pools = &stats

	applied, err := c.appliedSchemaVersion(ctx)
	switch {
	case err != nil:
		// The database is reachable but goose's bookkeeping table could not be
		// read. That is genuinely unknown, and unknown is not ready.
		out.Detail = strptr(err.Error())
		return out, http.StatusServiceUnavailable
	case applied > 0:
		out.SchemaVersion = strptr(migrate.FormatVersion(applied))
	}

	expected, err := migrate.Latest()
	if err != nil {
		out.Detail = strptr(err.Error())
		return out, http.StatusServiceUnavailable
	}
	if applied < expected {
		out.Migrations = migrationsPending
		out.Detail = strptr("the database is at " + migrate.FormatVersion(applied) +
			"; this build expects " + migrate.FormatVersion(expected))
		return out, http.StatusServiceUnavailable
	}

	// A database AHEAD of this binary is the normal state during a rolling deploy
	// under expand/contract, and it is ready: release N is required to run
	// against release N+1's schema.
	out.Migrations = migrationsApplied
	out.Status = readyStatusReady
	return out, http.StatusOK
}

// appliedSchemaVersion reads goose's bookkeeping table on the EXISTING pool.
//
// Never `migrate.Statuses`: that opens its own connection, and a readiness probe
// scraped every few seconds by every pod must not be a source of connections.
func (c *Container) appliedSchemaVersion(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, readyPingTimeout)
	defer cancel()

	var version int64
	err := c.Pools.General.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`,
	).Scan(&version)
	if err != nil {
		return 0, errors.New("the migration history could not be read: " + err.Error())
	}
	return version, nil
}

func strptr(s string) *string { return &s }
