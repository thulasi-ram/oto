//go:build tools

// This file is never compiled into the binary. It exists so that `go mod tidy`
// keeps the executables and libraries oto depends on pinned in go.mod/go.sum,
// even before the code that uses them has been written.
//
// Run `make generate` to install the executables into ./bin.

package tools

import (
	// Command-line tools.
	_ "github.com/pressly/goose/v3/cmd/goose"

	// Job queue (SPEC §G): river plus its pgx driver.
	_ "github.com/riverqueue/river"
	_ "github.com/riverqueue/river/riverdriver/riverpgxv5"

	// Alert-list filter builder only (C19); every other query is hand-written SQL.
	_ "github.com/Masterminds/squirrel"

	// Block Kit / webhook payload structural validation in CI.
	_ "github.com/santhosh-tekuri/jsonschema/v6"

	// Slack SDK, pinned exactly (SPEC §I.3).
	_ "github.com/slack-go/slack"

	// HTTP server and client instrumentation.
	_ "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	// Testing: real Postgres, never a mocked DB.
	_ "github.com/stretchr/testify/require"
	_ "github.com/testcontainers/testcontainers-go"
	_ "github.com/testcontainers/testcontainers-go/modules/postgres"
)
