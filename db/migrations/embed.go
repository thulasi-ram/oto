// Package migrations embeds the goose SQL migrations so a single oto binary can
// migrate its own database. Expand/contract only: assume release N and N+1 run
// simultaneously.
package migrations

import "embed"

// FS holds every .sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
